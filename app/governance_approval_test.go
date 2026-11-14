package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

func TestGovernanceApprovalRequiresCoreReviewForCurrentRun(t *testing.T) {
	const prefix = `{"name":"governance-sync","id":5,"can_admins_bypass":false,"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},"protection_rules":`
	const environment = prefix + `[{"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"id":7889900}}]}]}`
	const approved = `[{"state":"approved","environments":[{"name":"governance-sync","id":5}]}]`
	for _, tt := range []struct {
		name, environment, approval, attempt string
		wantErr                              bool
	}{
		{"core approval", environment, approved, "1", false},
		{"no protection", prefix + `[]}`, approved, "1", true},
		{"different team", prefix + `[{"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"id":1}}]}]}`, approved, "1", true},
		{"alternative reviewer", prefix + `[{"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"id":7889900}},{"type":"User","reviewer":{"id":2}}]}]}`, approved, "1", true},
		{"admin bypass enabled", strings.Replace(environment, `"can_admins_bypass":false`, `"can_admins_bypass":true`, 1), approved, "1", true},
		{"unprotected branches allowed", strings.Replace(environment, `"custom_branch_policies":true`, `"custom_branch_policies":false`, 1), approved, "1", true},
		{"unapproved", environment, `[]`, "1", true},
		{"different environment", environment, `[{"state":"approved","environments":[{"name":"other"}]}]`, "1", true},
		{"recreated environment", environment, `[{"state":"approved","environments":[{"name":"governance-sync","id":6}]}]`, "1", true},
		{"rejected", environment, `[{"state":"rejected","environments":[{"name":"governance-sync"}]}]`, "1", true},
		{"rerun cannot reuse approval", environment, approved, "2", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch req.URL.Path {
				case "/repos/org/actions/environments/governance-sync":
					_, _ = io.WriteString(r, tt.environment)
				case "/repos/org/actions/environments/governance-sync/deployment-branch-policies":
					_, _ = io.WriteString(r, `{"total_count":1,"branch_policies":[{"name":"main","type":"branch"}]}`)
				case "/repos/org/actions/actions/runs/7/approvals":
					_, _ = io.WriteString(r, tt.approval)
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
				}
				return r.Result(), nil
			})})
			err := verifyGovernanceApproval(context.Background(), client, "org/actions", "7", tt.attempt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("approval error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestGovernanceApprovalRejectsBroadBranchPolicy(t *testing.T) {
	for _, policy := range []string{`{"name":"*","type":"branch"}`, `{"name":"main","type":"tag"}`} {
		t.Run(policy, func(t *testing.T) {
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch req.URL.Path {
				case "/repos/org/actions/environments/governance-sync":
					_, _ = io.WriteString(r, `{"name":"governance-sync","id":5,"can_admins_bypass":false,"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},"protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"id":7889900}}]}]}`)
				case "/repos/org/actions/environments/governance-sync/deployment-branch-policies":
					_, _ = io.WriteString(r, `{"total_count":1,"branch_policies":[`+policy+`]}`)
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
				}
				return r.Result(), nil
			})})
			if err := verifyGovernanceApproval(context.Background(), client, "org/actions", "7", "1"); err == nil {
				t.Fatal("approval accepted an environment open to non-main branches")
			}
		})
	}
}
