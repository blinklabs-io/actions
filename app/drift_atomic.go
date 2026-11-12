package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/go-github/v60/github"
)

const updateRefsMutation = `mutation UpdateManagedRef($input: UpdateRefsInput!) {
	updateRefs(input: $input) {
		__typename
	}
}`

// updateManagedRef publishes a staged commit only if the observed branch head
// is still current. A non-force REST ref update alone accepts a rewind to an
// ancestor of the new commit.
func updateManagedRef(ctx context.Context, client *github.Client, repositoryID, branch, beforeSHA, afterSHA string) error {
	const zeroSHA = "0000000000000000000000000000000000000000"
	if repositoryID == "" || branch == "" || beforeSHA == "" || afterSHA == "" ||
		beforeSHA == zeroSHA || afterSHA == zeroSHA {
		return errors.New("missing repository, branch, or commit for atomic ref update")
	}

	type refUpdate struct {
		Name      string `json:"name"`
		BeforeOID string `json:"beforeOid"`
		AfterOID  string `json:"afterOid"`
		Force     bool   `json:"force"`
	}
	type updateRefsInput struct {
		RepositoryID string      `json:"repositoryId"`
		RefUpdates   []refUpdate `json:"refUpdates"`
	}
	body := struct {
		Query     string `json:"query"`
		Variables struct {
			Input updateRefsInput `json:"input"`
		} `json:"variables"`
	}{Query: updateRefsMutation}
	body.Variables.Input = updateRefsInput{
		RepositoryID: repositoryID,
		RefUpdates: []refUpdate{{
			Name:      "refs/heads/" + branch,
			BeforeOID: beforeSHA,
			AfterOID:  afterSHA,
			Force:     false,
		}},
	}
	req, err := client.NewRequest(http.MethodPost, "graphql", body)
	if err != nil {
		return errors.New("could not build atomic ref update request")
	}
	req.Header.Set("Accept", "application/json")
	var response struct {
		Data struct {
			UpdateRefs struct {
				TypeName string `json:"__typename"`
			} `json:"updateRefs"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if _, err := client.Do(ctx, req, &response); err != nil {
		return errors.New("atomic ref update request failed")
	}
	if len(response.Errors) > 0 {
		return errors.New("GitHub rejected the atomic ref update")
	}
	if response.Data.UpdateRefs.TypeName != "UpdateRefsPayload" {
		return errors.New("GitHub did not confirm the atomic ref update")
	}
	return nil
}
