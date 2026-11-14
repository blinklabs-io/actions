package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-github/v60/github"
)

const governanceEnvironment = "governance-sync"
const governanceCoreTeamID = 7889900

func verifyGovernanceApproval(ctx context.Context, client *github.Client, repository, runID, attempt string) error {
	owner, repo, ok := strings.Cut(repository, "/")
	id, err := strconv.ParseInt(runID, 10, 64)
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || err != nil || id < 1 || attempt != "1" {
		return errors.New("governance approval context is invalid; start a new run")
	}
	base := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	req, err := client.NewRequest(http.MethodGet, base+"/environments/"+governanceEnvironment, nil)
	if err != nil {
		return errors.New("could not construct governance environment request")
	}
	var environment struct {
		Name            string `json:"name"`
		ID              int64  `json:"id"`
		CanAdminsBypass *bool  `json:"can_admins_bypass"`
		BranchPolicy    *struct {
			ProtectedBranches    bool `json:"protected_branches"`
			CustomBranchPolicies bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
		Rules []struct {
			Type      string `json:"type"`
			Reviewers []struct {
				Type     string `json:"type"`
				Reviewer struct {
					ID int64 `json:"id"`
				} `json:"reviewer"`
			} `json:"reviewers"`
		} `json:"protection_rules"`
	}
	if _, err := client.Do(ctx, req, &environment); err != nil || environment.Name != governanceEnvironment || environment.ID <= 0 ||
		environment.CanAdminsBypass == nil || *environment.CanAdminsBypass || environment.BranchPolicy == nil ||
		environment.BranchPolicy.ProtectedBranches || !environment.BranchPolicy.CustomBranchPolicies {
		return errors.New("could not verify governance environment")
	}
	protected := false
	reviewerRules := 0
	for _, rule := range environment.Rules {
		if rule.Type == "required_reviewers" {
			reviewerRules++
			if len(rule.Reviewers) == 1 && rule.Reviewers[0].Type == "Team" && rule.Reviewers[0].Reviewer.ID == governanceCoreTeamID {
				protected = true
			}
		}
	}
	if !protected || reviewerRules != 1 {
		return errors.New("governance environment does not require the Core team")
	}
	req, err = client.NewRequest(http.MethodGet, base+"/environments/"+governanceEnvironment+"/deployment-branch-policies", nil)
	if err != nil {
		return errors.New("could not verify governance branch policy")
	}
	var policies struct {
		Total    int `json:"total_count"`
		Branches []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"branch_policies"`
	}
	if _, err := client.Do(ctx, req, &policies); err != nil || policies.Total != 1 || len(policies.Branches) != 1 || policies.Branches[0].Name != "main" || policies.Branches[0].Type != "branch" {
		return errors.New("governance environment is not restricted to main")
	}
	req, err = client.NewRequest(http.MethodGet, fmt.Sprintf("%s/actions/runs/%d/approvals", base, id), nil)
	if err != nil {
		return errors.New("could not construct governance approval request")
	}
	var approvals []struct {
		State        string `json:"state"`
		Environments []struct {
			Name string `json:"name"`
			ID   int64  `json:"id"`
		} `json:"environments"`
	}
	if _, err := client.Do(ctx, req, &approvals); err != nil {
		return errors.New("could not verify governance approval")
	}
	for _, approval := range approvals {
		if approval.State == "approved" {
			for _, approved := range approval.Environments {
				if approved.Name == governanceEnvironment && approved.ID == environment.ID {
					return nil
				}
			}
		}
	}
	return errors.New("the current run has no governance environment approval")
}
