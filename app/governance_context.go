package main

import (
	"context"
	"errors"
	"strings"

	"github.com/google/go-github/v60/github"
)

func verifyGovernanceContext(ctx context.Context, client *github.Client, repository, revision, ref, event, attempt string) error {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || !lifecycleRepoName.MatchString(owner) || !lifecycleRepoName.MatchString(repo) ||
		owner == "." || owner == ".." || repo == "." || repo == ".." ||
		!lifecycleRevision.MatchString(revision) || ref != "refs/heads/main" || attempt != "1" {
		return errors.New("governance run context is invalid; start a new run on main")
	}
	switch event {
	case "push", "schedule", "workflow_dispatch":
	default:
		return errors.New("governance run event is not allowed")
	}
	controller := &lifecycleController{owner: owner, repo: repo, revision: revision}
	return controller.requireCurrentMain(ctx, client)
}
