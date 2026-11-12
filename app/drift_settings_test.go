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
			permissionReads := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.Method == http.MethodGet {
					if tt.collaborator && strings.HasSuffix(req.URL.Path, "/collaborators") {
						if tt.name == "collaborator invited" {
							_, _ = io.WriteString(w, `[]`)
						} else {
							_, _ = io.WriteString(w, `[{"login":"example-user"}]`)
						}
						return
					}
					if tt.collaborator {
						permissionReads++
					}
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
			if tt.name == "collaborator changed" && permissionReads != 2 {
				t.Errorf("permission reads = %d, want 2", permissionReads)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); !strings.Contains(got, "["+tt.wantStatus+"]") || strings.Contains(got, "private detail") || strings.Contains(got, "example-user") {
				t.Errorf("unexpected report: %s", got)
			}
		})
	}
}

func TestCollaboratorMembershipPreventsPublicReadFalseMatch(t *testing.T) {
	for _, tt := range []struct {
		name       string
		listCode   int
		wantWrites int
	}{
		{"absent collaborator is invited with read alias", http.StatusOK, 1},
		{"inaccessible collaborator list is unchecked", http.StatusForbidden, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case http.MethodGet:
					if !strings.HasSuffix(req.URL.Path, "/collaborators") {
						t.Fatalf("unexpected permission lookup: %s", req.URL.Path)
					}
					r.WriteHeader(tt.listCode)
					_, _ = io.WriteString(r, `[]`)
				case http.MethodPut:
					writes++
					body, _ := io.ReadAll(req.Body)
					if !strings.Contains(string(body), `"permission":"pull"`) {
						t.Errorf("read alias not mapped to pull: %s", body)
					}
					r.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(r, `{}`)
				}
				return r.Result(), nil
			})})
			var output strings.Builder
			report := newDriftReport(&output)
			report.beginRepo("example/public", true)
			err := reconcileCollaborators(context.Background(), client, "example", "public", []Collaborator{{Username: "example-user", Permission: "read"}}, report)
			if (err != nil) != (tt.listCode != http.StatusOK) || writes != tt.wantWrites {
				t.Fatalf("error %v, writes %d", err, writes)
			}
		})
	}
}

func TestCollaboratorMembershipRequiresCompleteValidList(t *testing.T) {
	for _, tt := range []struct {
		name       string
		firstBody  string
		secondCode int
		wantError  bool
	}{
		{"member on second page", `[{"login":"other"}]`, http.StatusOK, false},
		{"second page fails", `[{"login":"other"}]`, http.StatusForbidden, true},
		{"null response", `null`, 0, true},
		{"empty response", ``, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				if req.Method != http.MethodGet {
					writes++
					t.Fatal("partial membership must not cause a write")
				}
				if strings.HasSuffix(req.URL.Path, "/permission") {
					_, _ = io.WriteString(r, `{"permission":"write","role_name":"maintain"}`)
				} else if req.URL.Query().Get("page") == "2" {
					r.WriteHeader(tt.secondCode)
					_, _ = io.WriteString(r, `[{"login":"EXAMPLE-USER"}]`)
				} else {
					if tt.secondCode != 0 {
						r.Header().Set("Link", `<https://api.github.com/repos/example/public/collaborators?page=2>; rel="next"`)
					}
					_, _ = io.WriteString(r, tt.firstBody)
				}
				return r.Result(), nil
			})})
			report := newDriftReport(io.Discard)
			report.beginRepo("example/public", true)
			err := reconcileCollaborators(context.Background(), client, "example", "public", []Collaborator{{Username: "example-user", Permission: "maintain"}}, report)
			if (err != nil) != tt.wantError || writes != 0 {
				t.Fatalf("error %v, writes %d", err, writes)
			}
		})
	}
}
