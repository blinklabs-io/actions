package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

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
	if settings == (RepoSettings{}) {
		report.change("setting", "delete_branch_on_merge", "", "").status = "unmanaged"
		return nil, nil
	}
	if err := validateRepoSettings(settings); err != nil {
		report.change("setting", "configuration", "", "").status = "unchecked"
		return nil, err
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, errors.New("could not encode configured repository settings")
	}
	var desired map[string]any
	if err := json.Unmarshal(data, &desired); err != nil {
		return nil, errors.New("could not decode configured repository settings")
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	req, err := client.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, errors.New("could not prepare repository settings observation")
	}
	var current map[string]any
	_, observeErr := client.Do(ctx, req, &current)
	update := make(map[string]any)
	fields := make([]string, 0, len(desired))
	for name := range desired {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	var entries []*driftEntry
	var observationErr error
	for _, name := range fields {
		want := desired[name]
		observed, exists := current[name]
		// GitHub returns null for unset metadata; absence of the key is still an error.
		if exists && observed == nil && (name == "description" || name == "homepage") {
			observed = ""
		}
		valid := false
		switch want.(type) {
		case bool:
			_, valid = observed.(bool)
		case string:
			_, valid = observed.(string)
		}
		if observeErr != nil || !exists || !valid {
			report.change("setting", name, "", "").status = "unchecked"
			observationErr = errors.New("could not observe repository settings")
			continue
		}
		if observed == want {
			report.change("setting", name, "", "").status = "matched"
			continue
		}
		entries = append(entries, report.change("setting", name, fmt.Sprint(observed), fmt.Sprint(want)))
		update[name] = want
	}
	// GitHub requires the title field whenever a commit-message default is sent,
	// even if the configured title already matches.
	for message, title := range map[string]string{
		"squash_merge_commit_message": "squash_merge_commit_title",
		"merge_commit_message":        "merge_commit_title",
	} {
		if _, changed := update[message]; changed {
			update[title] = desired[title]
		}
	}
	if observationErr != nil {
		report.diagnostic("repository settings observation failed")
		for _, entry := range entries {
			entry.status = "skipped"
		}
		return nil, observationErr
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return func() error {
		for _, entry := range entries {
			if err := report.checkpoint(entry); err != nil {
				for _, pending := range entries {
					pending.status = "skipped"
				}
				return errors.New("could not record planned setting change")
			}
		}
		request, err := client.NewRequest(http.MethodPatch, path, update)
		if err == nil {
			_, err = client.Do(ctx, request, nil)
		}
		if err != nil {
			report.diagnostic("repository settings update failed")
			for _, entry := range entries {
				entry.status = "failed"
			}
			return errors.New("could not update repository settings")
		}
		for _, entry := range entries {
			entry.status = "corrected"
		}
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
	var members map[string]bool
	var membersErr error
	if len(collaborators) > 0 {
		members, membersErr = observeCollaboratorMembership(ctx, client, owner, repo)
	}
	for _, c := range collaborators {
		if c.Username == "" || c.Permission == "" {
			report.change("collaborator", "configured user", "", "").status = "unchecked"
			observeErr = errors.New("collaborator configuration is incomplete")
			continue
		}
		if membersErr != nil {
			report.diagnostic("collaborator membership observation failed")
			report.change("collaborator", "configured user", "", "").status = "unchecked"
			observeErr = errors.New("could not observe collaborator membership")
			continue
		}
		current := "none"
		var err error
		if members[strings.ToLower(c.Username)] {
			current, err = observeCollaboratorRole(ctx, client, owner, repo, c.Username)
		}
		if err != nil {
			report.diagnostic("collaborator permission observation failed")
			report.change("collaborator", "configured user", "", "").status = "unchecked"
			observeErr = errors.New("could not observe collaborator permissions")
			continue
		}
		desired, requested := c.Permission, c.Permission
		switch desired {
		case "pull":
			desired = "read"
		case "push":
			desired = "write"
		case "read":
			requested = "pull"
		case "write":
			requested = "push"
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
			options := &github.RepositoryAddCollaboratorOptions{Permission: requested}
			_, response, err := client.Repositories.AddCollaborator(ctx, owner, repo, c.Username, options)
			if err != nil {
				report.diagnostic("collaborator update failed")
				entry.status = "failed"
				return errors.New("could not update collaborator permissions")
			}
			if response != nil && response.StatusCode == http.StatusCreated {
				entry.status = "invited"
				return nil
			}
			observed, err := observeCollaboratorRole(ctx, client, owner, repo, c.Username)
			if err != nil || observed != desired {
				report.diagnostic("effective collaborator permission did not match after update")
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

func observeCollaboratorMembership(ctx context.Context, client *github.Client, owner, repo string) (map[string]bool, error) {
	members := make(map[string]bool)
	opts := &github.ListCollaboratorsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		users, response, err := client.Repositories.ListCollaborators(ctx, owner, repo, opts)
		if err != nil || response == nil || response.StatusCode != http.StatusOK || users == nil {
			return nil, errors.New("could not list collaborators")
		}
		for _, user := range users {
			if user == nil || user.GetLogin() == "" {
				return nil, errors.New("collaborator list contained an invalid user")
			}
			members[strings.ToLower(user.GetLogin())] = true
		}
		if response.NextPage == 0 {
			return members, nil
		}
		if response.NextPage <= opts.Page {
			return nil, errors.New("collaborator list pagination did not advance")
		}
		opts.Page = response.NextPage
	}
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
