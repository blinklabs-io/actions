package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/go-github/v60/github"
)

func reconcileRepoSettings(ctx context.Context, client *github.Client, owner, repo string, settings RepoSettings, report *driftReport) error {
	apply, err := planRepoSettings(ctx, client, owner, repo, settings, report)
	if err != nil || apply == nil {
		return err
	}
	return apply()
}

func planRepoSettings(ctx context.Context, client *github.Client, owner, repo string, settings RepoSettings, report *driftReport) (func() error, error) {
	const field = "delete_branch_on_merge"
	if settings.DeleteBranchOnMerge == nil {
		report.change("setting", field, "", "").status = "unmanaged"
		return nil, nil
	}

	current, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil || current == nil || current.DeleteBranchOnMerge == nil {
		report.change("setting", field, "", "").status = "unchecked"
		return nil, errors.New("could not observe repository settings")
	}
	desired := *settings.DeleteBranchOnMerge
	if current.GetDeleteBranchOnMerge() == desired {
		report.change("setting", field, "", "").status = "matched"
		return nil, nil
	}

	entry := report.change("setting", field, strconv.FormatBool(current.GetDeleteBranchOnMerge()), strconv.FormatBool(desired))
	return func() error {
		if err := report.checkpoint(entry); err != nil {
			entry.status = "skipped"
			return errors.New("could not record planned setting change")
		}
		update := &github.Repository{DeleteBranchOnMerge: github.Bool(desired)}
		if _, _, err := client.Repositories.Edit(ctx, owner, repo, update); err != nil {
			entry.status = "failed"
			return errors.New("could not update repository settings")
		}
		entry.status = "corrected"
		return nil
	}, nil
}

func reconcileCollaborators(ctx context.Context, client *github.Client, owner, repo string, collaborators []Collaborator, report *driftReport) error {
	apply, err := planCollaborators(ctx, client, owner, repo, collaborators, report)
	if apply == nil {
		return err
	}
	return errors.Join(err, apply())
}

func planCollaborators(ctx context.Context, client *github.Client, owner, repo string, collaborators []Collaborator, report *driftReport) (func() error, error) {
	var changes []func() error
	var observeErr error
	for _, c := range collaborators {
		if c.Username == "" || c.Permission == "" {
			report.change("collaborator", "configured user", "", "").status = "unchecked"
			observeErr = errors.New("collaborator configuration is incomplete")
			continue
		}
		current, err := observeCollaboratorRole(ctx, client, owner, repo, c.Username)
		if err != nil {
			report.change("collaborator", "configured user", "", "").status = "unchecked"
			observeErr = errors.New("could not observe collaborator permissions")
			continue
		}
		desired := c.Permission
		switch desired {
		case "pull":
			desired = "read"
		case "push":
			desired = "write"
		}
		if current == desired {
			report.change("collaborator", "configured user", "", "").status = "matched"
			continue
		}
		entry := report.change("collaborator", "configured user", current, desired)
		changes = append(changes, func() error {
			if err := report.checkpoint(entry); err != nil {
				entry.status = "skipped"
				return errors.New("could not record planned collaborator change")
			}
			options := &github.RepositoryAddCollaboratorOptions{Permission: c.Permission}
			_, response, err := client.Repositories.AddCollaborator(ctx, owner, repo, c.Username, options)
			if err != nil {
				entry.status = "failed"
				return errors.New("could not update collaborator permissions")
			}
			if response != nil && response.StatusCode == http.StatusCreated {
				entry.status = "invited"
				return nil
			}
			observed, err := observeCollaboratorRole(ctx, client, owner, repo, c.Username)
			if err != nil || observed != desired {
				entry.status = "failed"
				return errors.New("collaborator update did not produce the configured effective permission")
			}
			entry.status = "corrected"
			return nil
		})
	}
	if len(changes) == 0 {
		return nil, observeErr
	}
	return func() error {
		var result error
		for _, apply := range changes {
			result = errors.Join(result, apply())
		}
		return result
	}, observeErr
}

func observeCollaboratorRole(ctx context.Context, client *github.Client, owner, repo, user string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s/permission", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(user))
	req, err := client.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	// The permission field collapses maintain into write and triage into read.
	// go-github v60 does not expose role_name on this response.
	var role struct {
		Permission string `json:"permission"`
		Name       string `json:"role_name"`
	}
	if _, err := client.Do(ctx, req, &role); err != nil {
		// A 404 can mean the repository or permission resource was inaccessible.
		return "", err
	}
	if role.Name != "" {
		return role.Name, nil
	}
	if role.Permission == "" {
		return "", errors.New("permission response missing role")
	}
	return role.Permission, nil
}
