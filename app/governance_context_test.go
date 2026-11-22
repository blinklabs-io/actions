package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v60/github"
)

func TestGovernanceContext(t *testing.T) {
	for _, tt := range []struct {
		name, repo, revision, ref, event, attempt, observed string
		status                                              int
		wantErr, wantRequest                                bool
	}{
		{"manual", "org/actions", testLifecycleSHA, "refs/heads/main", "workflow_dispatch", "1", testLifecycleSHA, 200, false, true},
		{"push", "org/actions", testLifecycleSHA, "refs/heads/main", "push", "1", testLifecycleSHA, 200, false, true},
		{"schedule", "org/actions", testLifecycleSHA, "refs/heads/main", "schedule", "1", testLifecycleSHA, 200, false, true},
		{"pull request", "org/actions", testLifecycleSHA, "refs/heads/main", "pull_request", "1", testLifecycleSHA, 200, true, false},
		{"privileged pull request", "org/actions", testLifecycleSHA, "refs/heads/main", "pull_request_target", "1", testLifecycleSHA, 200, true, false},
		{"branch", "org/actions", testLifecycleSHA, "refs/heads/feature", "workflow_dispatch", "1", testLifecycleSHA, 200, true, false},
		{"tag", "org/actions", testLifecycleSHA, "refs/tags/main", "workflow_dispatch", "1", testLifecycleSHA, 200, true, false},
		{"rerun", "org/actions", testLifecycleSHA, "refs/heads/main", "workflow_dispatch", "2", testLifecycleSHA, 200, true, false},
		{"invalid repository", "org/../actions", testLifecycleSHA, "refs/heads/main", "push", "1", testLifecycleSHA, 200, true, false},
		{"invalid revision", "org/actions", "main", "refs/heads/main", "push", "1", testLifecycleSHA, 200, true, false},
		{"stale", "org/actions", testLifecycleSHA, "refs/heads/main", "push", "1", testLifecycleStateSHA, 200, true, true},
		{"unobservable", "org/actions", testLifecycleSHA, "refs/heads/main", "push", "1", "", 403, true, true},
		{"empty object", "org/actions", testLifecycleSHA, "refs/heads/main", "push", "1", "", 200, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodGet || req.URL.Path != "/repos/org/actions/git/ref/heads/main" {
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				r.WriteHeader(tt.status)
				if tt.status == 200 {
					_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, tt.observed)
				} else {
					_, _ = io.WriteString(r, `{"message":"Forbidden"}`)
				}
				return r.Result(), nil
			})})
			err := verifyGovernanceContext(context.Background(), client, tt.repo, tt.revision, tt.ref, tt.event, tt.attempt)
			if (err != nil) != tt.wantErr || (requests > 0) != tt.wantRequest {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}
}
