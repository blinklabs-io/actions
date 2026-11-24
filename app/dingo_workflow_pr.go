package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v60/github"
)

const (
	dingoOwner         = "blinklabs-io"
	dingoRepo          = "dingo"
	dingoInitialBranch = "ci/shared-workflow-callers-issue-6712"
	dingoSyncBranch    = "ci/governance-workflows"
	dingoSyncTitle     = "ci: sync managed Dingo workflows"
)

func isDingo(owner, repo string) bool {
	return strings.EqualFold(owner, dingoOwner) && strings.EqualFold(repo, dingoRepo)
}

func dingoWorkflowPath(path string) bool {
	return path == ".github/workflows/conventional-commits.yml" || path == ".github/workflows/release-fanout.yml"
}

func validDingoWorkflowPR(pr *github.PullRequest, branch string) bool {
	return pr.GetNumber() > 0 && pr.GetState() == "open" &&
		pr.GetHead().GetRef() == branch && pr.GetBase().GetRef() == "main" &&
		strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), "blinklabs-io/dingo") &&
		strings.EqualFold(pr.GetBase().GetRepo().GetFullName(), "blinklabs-io/dingo") &&
		(branch == dingoSyncBranch || (branch == dingoInitialBranch && pr.GetNumber() == 6713))
}

func verifyDingoWorkflowPR(ctx context.Context, client *github.Client, number int, branch, head string) error {
	pr, _, err := client.PullRequests.Get(ctx, dingoOwner, dingoRepo, number)
	if err != nil || pr.GetNumber() != number || !validDingoWorkflowPR(pr, branch) || pr.GetHead().GetSHA() != head {
		return errors.New("workflow pull request changed during reconciliation")
	}
	return nil
}

func openDingoWorkflowPR(ctx context.Context, client *github.Client) (*github.PullRequest, error) {
	var found *github.PullRequest
	for _, branch := range []string{dingoInitialBranch, dingoSyncBranch} {
		prs, _, err := client.PullRequests.List(ctx, dingoOwner, dingoRepo, &github.PullRequestListOptions{
			State: "open", Head: dingoOwner + ":" + branch,
		})
		if err != nil {
			return nil, errors.New("could not look up Dingo workflow pull requests")
		}
		for _, pr := range prs {
			if found != nil || !validDingoWorkflowPR(pr, branch) {
				return nil, errors.New("unexpected or competing Dingo workflow pull request")
			}
			found = pr
		}
	}
	return found, nil
}

// GitHub compares a PR against its merge base. A branch behind main can already
// propose precisely the desired changes; rewriting it would dismiss its review.
func dingoBranchMatches(ctx context.Context, client *github.Client, base, head string, ops []fileOp) (bool, error) {
	comparison, _, err := client.Repositories.CompareCommits(ctx, dingoOwner, dingoRepo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil || comparison.GetMergeBaseCommit().GetSHA() == "" {
		return false, errors.New("could not verify Dingo workflow branch diff")
	}
	desired := make(map[string]string, len(ops))
	for _, op := range ops {
		desired[op.path] = string(op.content)
	}
	matches := true
	for _, file := range comparison.Files {
		path := file.GetFilename()
		if !dingoWorkflowPath(path) || (file.GetPreviousFilename() != "" && !dingoWorkflowPath(file.GetPreviousFilename())) {
			return false, errors.New("dingo workflow branch contains unrelated changes")
		}
		want, ok := desired[path]
		delete(desired, path)
		if file.GetStatus() == "removed" || file.GetStatus() == "renamed" {
			matches = false
			continue
		}
		if !ok {
			// A proposed file may have landed independently on main.
			mainBlob, err := dingoFileSHA(ctx, client, base, path)
			if err != nil {
				return false, err
			}
			matches = matches && mainBlob != "" && mainBlob == file.GetSHA()
			continue
		}
		data, _, err := client.Git.GetBlobRaw(ctx, dingoOwner, dingoRepo, file.GetSHA())
		if err != nil {
			return false, errors.New("could not read proposed workflow")
		}
		matches = matches && string(data) == want
	}
	matches = matches && len(desired) == 0
	// The same blob can still conflict if main changed that path since the
	// branch forked. Refresh the PR from main only for such overlapping edits.
	ancestor := comparison.GetMergeBaseCommit().GetSHA()
	if matches && ancestor != base {
		for _, op := range ops {
			oldBlob, err := dingoFileSHA(ctx, client, ancestor, op.path)
			if err != nil {
				return false, err
			}
			mainBlob, err := dingoFileSHA(ctx, client, base, op.path)
			if err != nil {
				return false, err
			}
			if oldBlob != mainBlob {
				return false, nil
			}
		}
	}
	return matches, nil
}

func dingoFileSHA(ctx context.Context, client *github.Client, revision, path string) (string, error) {
	file, _, _, err := client.Repositories.GetContents(ctx, dingoOwner, dingoRepo, path, &github.RepositoryContentGetOptions{Ref: revision})
	if isNotFound(err) {
		return "", nil
	}
	if err != nil || file.GetType() != "file" || file.GetSHA() == "" {
		return "", errors.New("could not compare Dingo workflow bases")
	}
	return file.GetSHA(), nil
}

func syncDingoWorkflowPR(ctx context.Context, client *github.Client, base string, batch *commitBatch) (result error) {
	report := reportFromContext(ctx)
	defer func() {
		if result != nil && report != nil {
			report.diagnostic(result.Error())
		}
	}()
	if base != "main" || batch.repositoryID == "" || batch.baseCommitSHA == "" {
		return errors.New("unexpected Dingo workflow base")
	}
	for _, op := range batch.ops {
		if op.remove || !dingoWorkflowPath(op.path) {
			return errors.New("dingo drift exceeds the two managed workflow files")
		}
	}
	if len(batch.ops) == 0 {
		return nil
	}
	pr, err := openDingoWorkflowPR(ctx, client)
	if err != nil {
		return err
	}
	branch := dingoSyncBranch
	if pr != nil {
		branch = pr.GetHead().GetRef()
	}
	ref, _, err := client.Git.GetRef(ctx, dingoOwner, dingoRepo, "heads/"+branch)
	head := ref.GetObject().GetSHA()
	if err != nil && (pr != nil || !isNotFound(err)) {
		return errors.New("could not read Dingo workflow branch")
	}
	if (err == nil && head == "") || (pr != nil && pr.GetHead().GetSHA() != head) {
		return errors.New("dingo workflow branch changed during observation")
	}
	matches := false
	if head != "" {
		matches, err = dingoBranchMatches(ctx, client, batch.baseCommitSHA, head, batch.ops)
		if err != nil {
			return err
		}
	}

	main, _, err := client.Git.GetRef(ctx, dingoOwner, dingoRepo, "heads/main")
	if err != nil || main.GetObject().GetSHA() != batch.baseCommitSHA {
		return errors.New("dingo main changed during observation")
	}
	if !matches {
		if pr != nil {
			if err := verifyDingoWorkflowPR(ctx, client, pr.GetNumber(), branch, head); err != nil {
				return err
			}
		}
		// Build from observed main. Keep the old PR head as a parent so the
		// update is a fast-forward even after main advances or a squash merge.
		parents := []*github.Commit{{SHA: github.String(batch.baseCommitSHA)}}
		if head != "" && head != batch.baseCommitSHA {
			parents = append([]*github.Commit{{SHA: github.String(head)}}, parents...)
		}
		message := dingoSyncTitle + "\n\nSigned-off-by: blinklabs-governance[bot] <294663193+blinklabs-governance[bot]@users.noreply.github.com>"
		commit, err := createManagedCommit(ctx, client, dingoOwner, dingoRepo, batch.baseCommitSHA, batch, message, parents)
		if err != nil {
			return err
		}
		if head == "" {
			_, _, err = client.Git.CreateRef(ctx, dingoOwner, dingoRepo, &github.Reference{
				Ref: github.String("refs/heads/" + branch), Object: &github.GitObject{SHA: github.String(commit)},
			})
		} else {
			err = updateManagedRef(ctx, client, batch.repositoryID, branch, head, commit)
		}
		if err != nil {
			return errors.New("could not publish Dingo workflow branch")
		}
		head = commit
	}
	if pr == nil {
		pr, _, err = client.PullRequests.Create(ctx, dingoOwner, dingoRepo, &github.NewPullRequest{
			Head: github.String(branch), Base: github.String(base), Title: github.String(dingoSyncTitle),
			Body: github.String("Sync the Conventional Commits and release fanout callers with the governance configuration."),
		})
		if err != nil {
			return errors.New("could not open Dingo workflow pull request")
		}
	}
	if pr.GetNumber() <= 0 {
		return errors.New("missing Dingo workflow pull request number")
	}
	if err := verifyDingoWorkflowPR(ctx, client, pr.GetNumber(), branch, head); err != nil {
		return err
	}
	if report != nil {
		report.diagnostic(fmt.Sprintf("pending workflow pull request: https://github.com/blinklabs-io/dingo/pull/%d", pr.GetNumber()))
	}
	return nil
}
