package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

// workflowSourceChanges compares expanded contracts, including injected fanout.
// Only a change confined to reusable source references can narrow an automatic sync.
func workflowSourceChanges(previous, current []byte) ([]string, bool, error) {
	var before, after Config
	if err := yaml.Unmarshal(previous, &before); err != nil {
		return nil, false, err
	}
	if err := yaml.Unmarshal(current, &after); err != nil {
		return nil, false, err
	}
	if err := expandProfiles(&before); err != nil {
		return nil, false, err
	}
	if err := expandProfiles(&after); err != nil {
		return nil, false, err
	}
	if len(before.Repositories) != len(after.Repositories) {
		return nil, false, nil
	}
	var changed []string
	for i := range after.Repositories {
		old, next := &before.Repositories[i], &after.Repositories[i]
		if old.Name != next.Name || len(old.Workflows) != len(next.Workflows) {
			return nil, false, nil
		}
		different := false
		for j := range next.Workflows {
			different = different || old.Workflows[j].ReusableWorkflow != next.Workflows[j].ReusableWorkflow
			old.Workflows[j].ReusableWorkflow = ""
			next.Workflows[j].ReusableWorkflow = ""
		}
		if different {
			changed = append(changed, next.Name)
		}
	}
	oldJSON, err := json.Marshal(before)
	if err != nil {
		return nil, false, err
	}
	nextJSON, err := json.Marshal(after)
	if err != nil {
		return nil, false, err
	}
	return changed, len(changed) > 0 && bytes.Equal(oldJSON, nextJSON), nil
}

func workflowSyncScope(event, target, previous, revision string, current []byte, readPrevious func(string) ([]byte, error)) ([]string, bool, error) {
	if target != "" {
		if event != "workflow_dispatch" {
			return nil, false, errors.New("targeted workflow sync requires a manual dispatch")
		}
		return []string{target}, true, nil
	}
	if event != "push" {
		return nil, false, nil
	}
	if !commitSHAPattern.MatchString(previous) || previous == strings.Repeat("0", 40) ||
		!commitSHAPattern.MatchString(revision) || readPrevious == nil {
		return nil, false, errors.New("workflow sync requires a valid previous push revision")
	}
	old, err := readPrevious(previous)
	if err != nil {
		return nil, false, errors.New("could not read previous governance configuration")
	}
	return workflowSourceChanges(old, current)
}

// syncSelectedWorkflows does not reconcile settings, discovery or lifecycle state.
// The caller must first verify the committed current-main configuration and state.
func syncSelectedWorkflows(ctx context.Context, client *github.Client, cfg Config, names []string,
	readSource func(string, string) ([]byte, error), checkCurrent func() error, report *driftReport,
) error {
	selected := cfg
	selected.Repositories = nil
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			return errors.New("duplicate targeted repository")
		}
		seen[name] = true
		found := false
		for _, repo := range cfg.Repositories {
			if repo.Name == name {
				selected.Repositories = append(selected.Repositories, repo)
				found = true
				break
			}
		}
		if !found {
			return errors.New("workflow target must be an explicitly configured repository")
		}
	}
	if len(selected.Repositories) == 0 || checkCurrent == nil {
		return errors.New("workflow sync requires a target and current-main verification")
	}
	if err := expandProfiles(&selected); err != nil {
		return err
	}
	if err := validateSharedWorkflowRefs(&selected, readSource); err != nil {
		return err
	}
	if err := resolvePinnedWorkflowRefs(&selected, latestWorkflowChange); err != nil {
		return err
	}
	for _, repo := range selected.Repositories {
		if err := checkCurrent(); err != nil {
			return err
		}
		owner, name := parseRepoString(repo.Name)
		info, _, err := client.Repositories.Get(ctx, owner, name)
		public := err == nil && info != nil && info.Private != nil && !info.GetPrivate() && info.GetVisibility() == "public"
		report.beginRepo(repo.Name, public)
		if err != nil || info == nil || info.Private == nil || info.GetID() <= 0 ||
			!strings.EqualFold(info.GetFullName(), repo.Name) || info.GetArchived() || info.GetDisabled() {
			return errors.New("could not verify existing workflow target")
		}
		repoCtx := context.WithValue(ctx, driftContextKey{}, report)
		if err := syncWorkflows(repoCtx, client, owner, name, repo.Workflows); err != nil {
			return err
		}
	}
	return nil
}
