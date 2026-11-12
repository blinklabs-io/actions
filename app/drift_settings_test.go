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

type settingsTestTransport func(*http.Request) (*http.Response, error)

func (f settingsTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReconcileSettingsAndCollaborators(t *testing.T) {
	tests := []struct {
		name, readBody, writeMethod, wantStatus string
		readCode, writeCode                     int
		collaborator, wantErr                   bool
	}{
		{"setting changed", `{"delete_branch_on_merge":false}`, "PATCH", "corrected", 200, 200, false, false},
		{"setting matched", `{"delete_branch_on_merge":true}`, "", "matched", 200, 0, false, false},
		{"setting missing field", `{}`, "", "unchecked", 200, 0, false, true},
		{"setting read failure", `{"message":"private detail"}`, "", "unchecked", 403, 0, false, true},
		{"setting write failure", `{"delete_branch_on_merge":false}`, "PATCH", "failed", 200, 500, false, true},
		{"collaborator changed", `{"permission":"read"}`, "PUT", "corrected", 200, 204, true, false},
		{"collaborator inherited role remains", `{"permission":"admin"}`, "PUT", "failed", 200, 204, true, true},
		{"collaborator invited", `{"permission":"none"}`, "PUT", "invited", 200, 201, true, false},
		{"collaborator matched", `{"permission":"write","role_name":"maintain"}`, "", "matched", 200, 0, true, false},
		{"collaborator ambiguous 404", `{"message":"private detail"}`, "", "unchecked", 404, 0, true, true},
		{"collaborator read failure", `{"message":"private detail"}`, "", "unchecked", 500, 0, true, true},
		{"collaborator write failure", `{"permission":"read"}`, "PUT", "failed", 200, 403, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var writes []string
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.Method == http.MethodGet {
					w.WriteHeader(tt.readCode)
					if tt.name == "collaborator changed" && len(writes) > 0 {
						_, _ = io.WriteString(w, `{"permission":"write","role_name":"maintain"}`)
						return
					}
					_, _ = io.WriteString(w, tt.readBody)
					return
				}
				writes = append(writes, req.Method)
				if tt.writeCode == 0 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				want := `"delete_branch_on_merge":true`
				if tt.collaborator {
					want = `"permission":"maintain"`
				}
				if !strings.Contains(string(body), want) {
					t.Errorf("request body = %q, missing %q", body, want)
				}
				w.WriteHeader(tt.writeCode)
				if tt.writeCode != 204 {
					_, _ = io.WriteString(w, `{"message":"private detail"}`)
				}
			})
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				return recorder.Result(), nil
			})})
			var output strings.Builder
			report := newDriftReport(&output)
			report.beginRepo("example/public", true)
			ctx := context.Background()
			var err error
			if tt.collaborator {
				err = reconcileCollaborators(ctx, client, "example", "public", []Collaborator{{Username: "example-user", Permission: "maintain"}}, report)
			} else {
				err = reconcileRepoSettings(ctx, client, "example", "public", RepoSettings{DeleteBranchOnMerge: github.Bool(true)}, report)
			}
			if (err != nil) != tt.wantErr || (err != nil && strings.Contains(err.Error(), "private detail")) {
				t.Errorf("unexpected error: %v", err)
			}
			if got := strings.Join(writes, ","); got != tt.writeMethod {
				t.Errorf("write methods = %q, want %q", got, tt.writeMethod)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); !strings.Contains(got, "["+tt.wantStatus+"]") || strings.Contains(got, "private detail") {
				t.Errorf("unexpected report: %s", got)
			}
		})
	}
}
