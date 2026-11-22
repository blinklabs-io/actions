package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/go-github/v60/github"
)

var teamSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func teamPermission(permission string) string {
	switch permission {
	case "read", "pull":
		return "pull"
	case "write", "push":
		return "push"
	case "triage", "maintain", "admin":
		return permission
	default:
		return ""
	}
}

func validateTeamGrants(teams []TeamGrant) error {
	seen := make(map[string]bool)
	for _, team := range teams {
		if !teamSlugPattern.MatchString(team.Slug) || teamPermission(team.Permission) == "" || seen[team.Slug] {
			return errors.New("teams must have unique organization slugs and a supported repository permission")
		}
		seen[team.Slug] = true
	}
	return nil
}

func observeTeams(ctx context.Context, client *github.Client, owner, repo string) (map[string]string, error) {
	roles := make(map[string]string)
	opts := &github.ListOptions{PerPage: 100}
	for {
		teams, response, err := client.Repositories.ListTeams(ctx, owner, repo, opts)
		if err != nil || response == nil || response.StatusCode != http.StatusOK || teams == nil {
			return nil, errors.New("could not observe repository teams")
		}
		for _, team := range teams {
			if team == nil || team.GetSlug() == "" || team.GetPermission() == "" {
				return nil, errors.New("repository team observation was incomplete")
			}
			slug := strings.ToLower(team.GetSlug())
			if _, exists := roles[slug]; exists {
				return nil, errors.New("repository team observation contained duplicate teams")
			}
			roles[slug] = team.GetPermission()
		}
		if response.NextPage == 0 {
			return roles, nil
		}
		if response.NextPage <= opts.Page {
			return nil, errors.New("repository team pagination did not advance")
		}
		opts.Page = response.NextPage
	}
}

func reconcileTeams(ctx context.Context, client *github.Client, owner, repo string, teams []TeamGrant, report *driftReport) error {
	if len(teams) == 0 {
		return nil
	}
	if err := validateTeamGrants(teams); err != nil {
		report.change("team", "configured team", "", "").status = "unchecked"
		return err
	}
	current, err := observeTeams(ctx, client, owner, repo)
	if err != nil {
		for i := range teams {
			report.change("team", fmt.Sprintf("configured team %d", i+1), "", "").status = "unchecked"
		}
		return err
	}
	var result error
	for i, team := range teams {
		target := fmt.Sprintf("configured team %d", i+1)
		desired := teamPermission(team.Permission)
		observed := current[team.Slug]
		if observed == desired {
			report.change("team", target, "", "").status = "matched"
			continue
		}
		if observed == "" {
			observed = "none"
		}
		entry := report.change("team", target, observed, desired)
		if err := report.checkpoint(entry); err != nil {
			entry.status = "skipped"
			return errors.New("could not record planned team change")
		}
		_, err := client.Teams.AddTeamRepoBySlug(ctx, owner, team.Slug, owner, repo, &github.TeamAddTeamRepoOptions{Permission: desired})
		if err != nil {
			entry.status = "failed"
			result = errors.Join(result, errors.New("could not update repository team permission"))
			continue
		}
		updated, err := observeTeams(ctx, client, owner, repo)
		if err != nil || updated[team.Slug] != desired {
			entry.status = "failed"
			for remaining := i + 1; remaining < len(teams); remaining++ {
				report.change("team", fmt.Sprintf("configured team %d", remaining+1), "", "").status = "skipped"
			}
			return errors.Join(result, errors.New("team permission did not match after update"))
		}
		current = updated
		entry.status = "corrected"
	}
	return result
}
