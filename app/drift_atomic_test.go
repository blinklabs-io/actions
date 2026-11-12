package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

func TestUpdateManagedRef(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		wantErr  string
	}{
		{"success", 200, `{"data":{"updateRefs":{"__typename":"UpdateRefsPayload"}}}`, ""},
		{"GraphQL error", 200, `{"data":{"updateRefs":{"__typename":"UpdateRefsPayload"}},"errors":[{"message":"private detail"}]}`, "GitHub rejected the atomic ref update"},
		{"HTTP error", 403, `{"message":"private detail"}`, "atomic ref update request failed"},
		{"malformed JSON", 200, `private detail`, "atomic ref update request failed"},
		{"malformed payload", 200, `{"data":{"updateRefs":{"__typename":"private detail"}}}`, "GitHub did not confirm the atomic ref update"},
		{"missing data", 200, `{}`, "GitHub did not confirm the atomic ref update"},
		{"null update result", 200, `{"data":{"updateRefs":null}}`, "GitHub did not confirm the atomic ref update"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := strings.Repeat("a", 40)
			after := strings.Repeat("b", 40)
			calls := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || req.URL.Path != "/graphql" || req.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("expected an authenticated GraphQL POST, got %s %s", req.Method, req.URL.Path)
				}
				var body struct {
					Query     string `json:"query"`
					Variables struct {
						Input struct {
							RepositoryID string `json:"repositoryId"`
							RefUpdates   []struct {
								Name      string `json:"name"`
								BeforeOID string `json:"beforeOid"`
								AfterOID  string `json:"afterOid"`
								Force     *bool  `json:"force"`
							} `json:"refUpdates"`
						} `json:"input"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				input := body.Variables.Input
				if !strings.Contains(body.Query, "updateRefs(input: $input)") || input.RepositoryID != "repo-id" || len(input.RefUpdates) != 1 {
					t.Fatalf("unexpected mutation: %+v", body)
				}
				update := input.RefUpdates[0]
				if update.Name != "refs/heads/release/v1" || update.BeforeOID != before || update.AfterOID != after ||
					update.Force == nil || *update.Force {
					t.Errorf("unexpected ref update: %+v", update)
				}
				return &http.Response{
					StatusCode: tt.status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(tt.response)),
				}, nil
			})}).WithAuthToken("test-token")
			err := updateManagedRef(context.Background(), client, "repo-id", "release/v1", before, after)
			if calls != 1 {
				t.Fatalf("got %d requests, want 1", calls)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("got error %v, want %q", err, tt.wantErr)
			}
		})
	}
}
