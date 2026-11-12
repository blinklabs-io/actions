package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

func TestBranchProtectionDiff(t *testing.T) {
	current := &github.Protection{
		RequiredPullRequestReviews: &github.PullRequestReviewsEnforcement{RequiredApprovingReviewCount: 1},
		AllowForcePushes:           &github.AllowForcePushes{Enabled: true},
	}
	desired := BranchProtection{RequiredApprovingReviewCount: 2}
	before, after := branchProtectionDiff(current, desired)
	if want := "required_approving_review_count: 1\nallow_force_pushes: true"; before != want {
		t.Errorf("before = %q, want %q", before, want)
	}
	if want := "required_approving_review_count: 2\nallow_force_pushes: false"; after != want {
		t.Errorf("after = %q, want %q", after, want)
	}
}

func TestBranchIsUnprotectedRecognizesGitHubSentinel(t *testing.T) {
	if !branchIsUnprotected(github.ErrBranchNotProtected) {
		t.Fatal("GitHub's unprotected-branch sentinel was not recognized")
	}
}

func TestSyncBranchProtectionDrift(t *testing.T) {
	const (
		matching = `{"required_pull_request_reviews":{"required_approving_review_count":2}}`
		drifted  = `{"required_pull_request_reviews":{"required_approving_review_count":1},"allow_force_pushes":{"enabled":true}}`
	)
	for _, tt := range []struct {
		name, body, status        string
		getCode, putCode, putWant int
		private, wantErr          bool
	}{
		{"matched", matching, "matched", 200, 0, 0, false, false},
		{"read failed", "", "unchecked", 500, 0, 0, false, true},
		{"new protection", `{"message":"Branch not protected"}`, "corrected", 404, 200, 1, false, false},
		{"write failed", drifted, "failed", 200, 500, 1, false, true},
		{"corrected", drifted, "corrected", 200, 200, 1, false, false},
		{"empty dismissal restrictions", `{"required_pull_request_reviews":{"required_approving_review_count":1,"dismissal_restrictions":{"users":[],"teams":[],"apps":[]}},"allow_force_pushes":{"enabled":true}}`, "corrected", 200, 200, 1, false, false},
		{"unmanaged protection", `{"required_pull_request_reviews":{"required_approving_review_count":1,"require_last_push_approval":true}}`, "skipped", 200, 0, 0, false, true},
		{"bound check", `{"required_pull_request_reviews":{"required_approving_review_count":1},"required_status_checks":{"strict":false,"contexts":["ci"],"checks":[{"context":"ci","app_id":123}]}}`, "skipped", 200, 0, 0, false, true},
		{"private write failed", drifted, "failed", 200, 500, 1, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var writes int
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", "application/json")
				if req.URL.Path != "/repos/example/sensitive-repo/branches/sensitive-branch/protection" {
					t.Errorf("unexpected request path %q", req.URL.Path)
				}
				code, body := tt.getCode, tt.body
				switch req.Method {
				case http.MethodGet:
				case http.MethodPut:
					writes++
					var update github.ProtectionRequest
					if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
						t.Errorf("decode protection update: %v", err)
					} else if update.RequiredPullRequestReviews == nil || update.RequiredPullRequestReviews.RequiredApprovingReviewCount != 2 ||
						update.AllowForcePushes == nil || *update.AllowForcePushes {
						t.Errorf("unexpected update: %+v", update)
					}
					code, body = tt.putCode, matching
				default:
					t.Errorf("unexpected request method %q", req.Method)
					code = 500
				}
				if code == 0 || (code >= 400 && body == "") {
					code, body = 500, `{"message":"sensitive-api-detail"}`
				}
				rec.WriteHeader(code)
				_, _ = io.WriteString(rec, body)
				return rec.Result(), nil
			})})

			var output strings.Builder
			report := newDriftReport(&output)
			report.beginRepo("example/sensitive-repo", !tt.private)
			ctx := context.WithValue(context.Background(), driftContextKey{}, report)
			desired := []BranchProtection{{Branch: "sensitive-branch", RequiredApprovingReviewCount: 2}}
			err := syncBranchProtection(ctx, client, "example", "sensitive-repo", desired)
			if (err != nil) != tt.wantErr || (err != nil && strings.Contains(err.Error(), "sensitive-")) {
				t.Errorf("unexpected error: %v", err)
			}
			if writes != tt.putWant {
				t.Errorf("PUT calls = %d, want %d", writes, tt.putWant)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			got := output.String()
			if tt.private {
				if strings.Contains(got, "sensitive-") || strings.Contains(got, "allow_force_pushes") ||
					!strings.Contains(got, "1 private or unknown") || !strings.Contains(got, "- failed: 1") {
					t.Errorf("private report = %q", got)
				}
				return
			}
			if !strings.Contains(got, "["+tt.status+"]") || strings.Contains(got, "sensitive-api-detail") {
				t.Errorf("unexpected report = %q", got)
			}
			if tt.putWant > 0 && tt.name != "new protection" && (!strings.Contains(got, "- allow_force_pushes: true") ||
				!strings.Contains(got, "+ allow_force_pushes: false")) {
				t.Errorf("missing old/new diff: %q", got)
			}
		})
	}
}

func TestBranchProtectionAmbiguousNotFoundAndInvalidStrict(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		desired        BranchProtection
		wantGET        int
	}{
		{"ambiguous not found", `{"message":"Not Found"}`, BranchProtection{Branch: "main"}, 1},
		{"strict without checks", ``, BranchProtection{Branch: "main", RequireUpToDate: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gets, writes := 0, 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				if req.Method == http.MethodGet {
					gets++
					r.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(r, tc.response)
				} else {
					writes++
				}
				return r.Result(), nil
			})})
			var out strings.Builder
			report := newDriftReport(&out)
			report.beginRepo("example/public", true)
			ctx := context.WithValue(context.Background(), driftContextKey{}, report)
			if err := syncBranchProtection(ctx, client, "example", "public", []BranchProtection{tc.desired}); err == nil {
				t.Fatal("expected observation error")
			}
			if writes != 0 || gets != tc.wantGET {
				t.Fatalf("gets=%d writes=%d", gets, writes)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "[unchecked]") {
				t.Fatalf("missing unchecked result: %s", out.String())
			}
		})
	}
}
