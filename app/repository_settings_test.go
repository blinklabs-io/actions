package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

func TestRepositorySettingsDocumentedRequests(t *testing.T) {
	for _, tt := range []struct {
		name, config, observed string
		want                   map[string]any
	}{
		{"explicit false and empty metadata", "description: ''\nhomepage: ''\nhas_issues: false\nhas_projects: false\nhas_wiki: false\nhas_discussions: false\n", `{"description":"old","homepage":"https://example.com","has_issues":true,"has_projects":true,"has_wiki":true,"has_discussions":true,"allow_auto_merge":true}`, map[string]any{"description": "", "homepage": "", "has_issues": false, "has_projects": false, "has_wiki": false, "has_discussions": false}},
		{"nullable metadata already clear", "description: ''\nhomepage: ''", `{"description":null,"homepage":null}`, nil},
		{"default branch and PR options", "default_branch: main\nallow_auto_merge: false\nallow_update_branch: true\nweb_commit_signoff_required: true", `{"default_branch":"develop","allow_auto_merge":true,"allow_update_branch":false,"web_commit_signoff_required":false}`, map[string]any{"default_branch": "main", "allow_auto_merge": false, "allow_update_branch": true, "web_commit_signoff_required": true}},
		{"squash message includes matching title", "squash_merge_commit_title: PR_TITLE\nsquash_merge_commit_message: BLANK", `{"squash_merge_commit_title":"PR_TITLE","squash_merge_commit_message":"COMMIT_MESSAGES"}`, map[string]any{"squash_merge_commit_title": "PR_TITLE", "squash_merge_commit_message": "BLANK"}},
		{"merge message includes matching title", "merge_commit_title: MERGE_MESSAGE\nmerge_commit_message: PR_BODY", `{"merge_commit_title":"MERGE_MESSAGE","merge_commit_message":"PR_TITLE"}`, map[string]any{"merge_commit_title": "MERGE_MESSAGE", "merge_commit_message": "PR_BODY"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var settings RepoSettings
			if err := yaml.Unmarshal([]byte(tt.config), &settings); err != nil {
				t.Fatal(err)
			}
			var patches []map[string]any
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				if req.URL.Path != "/repos/example/project" {
					t.Error(req.URL.Path)
				}
				switch req.Method {
				case "GET":
					_, _ = io.WriteString(r, tt.observed)
				case "PATCH":
					var body map[string]any
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					patches = append(patches, body)
					_, _ = io.WriteString(r, `{}`)
				default:
					t.Errorf("unexpected method %s", req.Method)
					r.WriteHeader(500)
				}
				return r.Result(), nil
			})})
			report := newDriftReport(io.Discard)
			report.beginRepo("example/project", true)
			if err := reconcileRepoSettings(context.Background(), client, "example", "project", settings, report); err != nil {
				t.Fatal(err)
			}
			if tt.want == nil {
				if len(patches) != 0 {
					t.Fatalf("unexpected write: %#v", patches)
				}
				return
			}
			if len(patches) != 1 || !reflect.DeepEqual(patches[0], tt.want) {
				t.Fatalf("patches=%#v want=%#v", patches, tt.want)
			}
		})
	}
}

func TestRepositorySettingsRejectUnobservedFields(t *testing.T) {
	for _, observed := range []string{
		`{"description":null}`, `{"description":null,"has_issues":null}`,
		`{"description":null,"has_issues":"true"}`, `{"description":{},"has_issues":false}`,
		`{"has_issues":false}`, `null`,
	} {
		writes := 0
		client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
			r := httptest.NewRecorder()
			r.Header().Set("Content-Type", "application/json")
			if req.Method != "GET" {
				writes++
			}
			_, _ = io.WriteString(r, observed)
			return r.Result(), nil
		})})
		report := newDriftReport(io.Discard)
		report.beginRepo("example/project", true)
		err := reconcileRepoSettings(context.Background(), client, "example", "project", RepoSettings{Description: github.String("new"), HasIssues: github.Bool(true)}, report)
		if err == nil || writes != 0 {
			t.Fatalf("response=%s err=%v writes=%d", observed, err, writes)
		}
	}
}

func TestRepositorySettingsConfigurationValidation(t *testing.T) {
	for _, config := range []string{
		"default_branch: ''", "squash_merge_commit_title: invalid", "squash_merge_commit_message: PR_BODY",
		"merge_commit_message: PR_BODY", "merge_commit_title: PR_TITLE\nmerge_commit_message: COMMIT_MESSAGES",
		"allow_merge_commit: false\nallow_squash_merge: false\nallow_rebase_merge: false",
		"private: false", "archived: true", "name: renamed", "unknown_setting: true",
	} {
		var settings RepoSettings
		err := yaml.Unmarshal([]byte(config), &settings)
		if err == nil {
			err = validateRepoSettings(settings)
		}
		if err == nil {
			t.Fatalf("accepted invalid or unsupported config %q", config)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("profiles:\n  p:\n    settings:\n      has_wiki: false\n      homepage: ''\nrepositories:\n  - name: example/project\n    profile: p\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	settings := cfg.Repositories[0].Settings
	if settings.HasWiki == nil || *settings.HasWiki || settings.Homepage == nil || *settings.Homepage != "" {
		t.Fatal("explicit false/empty not inherited")
	}
}

func TestRepositorySettingsPrivateMetadataRedacted(t *testing.T) {
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		if req.Method == "GET" {
			_, _ = io.WriteString(r, `{"description":"confidential-old","homepage":null}`)
		} else {
			_, _ = io.WriteString(r, `{}`)
		}
		return r.Result(), nil
	})})
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("example/private", false)
	if err := reconcileRepoSettings(context.Background(), client, "example", "private", RepoSettings{Description: github.String("confidential-new"), Homepage: github.String("https://internal.example")}, report); err != nil {
		t.Fatal(err)
	}
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"confidential", "internal.example", "example/private", "description", "homepage"} {
		if strings.Contains(output.String(), hidden) {
			t.Fatalf("leaked metadata: %s", output.String())
		}
	}
}

func TestRepositorySettingsPreserveYAMLAnchors(t *testing.T) {
	for i, settings := range []string{
		"    settings:\n      <<: *defaults\n      has_wiki: *disabled\n",
		"    settings: *defaults\n",
		"    settings:\n      <<: [*defaults, *other]\n",
	} {
		data := "disabled: &disabled false\ndefaults: &defaults\n  has_issues: true\n  homepage: ''\nother: &other\n  has_wiki: false\nrepositories:\n  - name: example/project\n" + settings
		var cfg Config
		if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
			t.Fatal(err)
		}
		s := cfg.Repositories[0].Settings
		if s.HasIssues == nil || !*s.HasIssues || s.Homepage == nil || *s.Homepage != "" {
			t.Fatalf("lost alias values: %+v", s)
		}
		if i != 1 && (s.HasWiki == nil || *s.HasWiki) {
			t.Fatal("scalar alias or second merge source lost explicit false")
		}
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("defaults: &defaults\n  typo: true\nrepositories:\n  - name: example/project\n    settings:\n      <<: *defaults\n"), &cfg); err == nil {
		t.Fatal("unknown field in alias accepted")
	}
}

func TestDefaultBranchFailureStopsManagedWrites(t *testing.T) {
	for _, protectionFails := range []bool{true, false} {
		var events []string
		repoReads := 0
		client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
			r := httptest.NewRecorder()
			r.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(req.URL.Path, "/protection"):
				switch req.Method {
				case "GET":
					events = append(events, "observe protection")
					if protectionFails {
						r.WriteHeader(403)
					} else {
						r.WriteHeader(404)
						_, _ = io.WriteString(r, `{"message":"Branch not protected"}`)
					}
				case "PUT":
					events = append(events, "protect branch")
					_, _ = io.WriteString(r, `{}`)
				default:
					t.Error(req.Method)
				}
			case req.Method == "GET" && req.URL.Path == "/repos/example/project":
				repoReads++
				_, _ = io.WriteString(r, `{"id":1,"node_id":"id","full_name":"example/project","private":false,"visibility":"public","default_branch":"develop"}`)
			case req.Method == "PATCH" && req.URL.Path == "/repos/example/project":
				events = append(events, "select branch")
				r.WriteHeader(422)
				_, _ = io.WriteString(r, `{"message":"Validation failed"}`)
			default:
				t.Errorf("managed operation after refused branch selection: %s %s", req.Method, req.URL.Path)
				r.WriteHeader(500)
			}
			return r.Result(), nil
		})})
		report := newDriftReport(io.Discard)
		err := reconcileRepositories(context.Background(), client, []RepoConfig{{Name: "example/project", Settings: RepoSettings{DefaultBranch: github.String("main")}, BranchProtection: []BranchProtection{{Branch: "main", RequiredApprovingReviewCount: 1}}, Workflows: []WorkflowConfig{{DestinationFile: "ci.yml", ReusableWorkflow: "example/ci/.github/workflows/test.yml@main"}}}}, report)
		want := []string{"observe protection", "protect branch", "select branch"}
		wantReads := 2
		if protectionFails {
			want = []string{"observe protection"}
			wantReads = 1
		}
		if err == nil || !reflect.DeepEqual(events, want) || repoReads != wantReads {
			t.Fatalf("err=%v events=%v reads=%d", err, events, repoReads)
		}
	}
}

func TestLinearHistoryEnablesMergeMethodBeforeProtection(t *testing.T) {
	t.Chdir("..")
	for _, method := range []string{"allow_squash_merge", "allow_rebase_merge"} {
		for _, failure := range []string{"", "merge method", "protection", "protection without default branch"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				current := map[string]any{
					"id": 1, "node_id": "id", "full_name": "example/project", "private": false, "visibility": "public",
					"default_branch": "develop", "allow_merge_commit": true, "allow_squash_merge": false, "allow_rebase_merge": false,
				}
				var events []string
				protected := false
				fake := &recordingGitHub{contents: map[string]string{}}
				client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
					r := httptest.NewRecorder()
					r.Header().Set("Content-Type", "application/json")
					switch {
					case req.URL.Path == "/repos/example/project" && req.Method == http.MethodGet:
						_ = json.NewEncoder(r).Encode(current)
					case req.URL.Path == "/repos/example/project" && req.Method == http.MethodPatch:
						var body map[string]any
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body[method] == true {
							events = append(events, "enable merge method")
							if len(body) != 1 {
								t.Errorf("prerequisite patch changes unrelated settings: %v", body)
							}
							if failure == "merge method" {
								r.WriteHeader(http.StatusForbidden)
								return r.Result(), nil
							}
						} else {
							events = append(events, "select default branch")
							if !protected || body["default_branch"] != "main" {
								t.Errorf("default branch selection before successful protection: %v", body)
							}
						}
						for k, v := range body {
							current[k] = v
						}
						_, _ = io.WriteString(r, `{}`)
					case strings.HasSuffix(req.URL.Path, "/protection") && req.Method == http.MethodGet:
						r.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(r, `{"message":"Branch not protected"}`)
					case strings.HasSuffix(req.URL.Path, "/protection") && req.Method == http.MethodPut:
						events = append(events, "protect branch")
						var body map[string]any
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body["required_linear_history"] != true {
							t.Errorf("linear-history requirement was not sent: %v", body)
						}
						if current["allow_squash_merge"] != true && current["allow_rebase_merge"] != true {
							r.WriteHeader(http.StatusUnprocessableEntity)
							return r.Result(), nil
						}
						if strings.HasPrefix(failure, "protection") {
							r.WriteHeader(http.StatusForbidden)
							return r.Result(), nil
						}
						protected = true
						_, _ = io.WriteString(r, `{}`)
					default:
						if failure == "" && protected && current["default_branch"] == "main" && req.Method == http.MethodGet {
							fake.handler().ServeHTTP(r, req)
							return r.Result(), nil
						}
						t.Errorf("unexpected downstream operation: %s %s", req.Method, req.URL.Path)
						r.WriteHeader(http.StatusInternalServerError)
					}
					return r.Result(), nil
				})})
				settings := RepoSettings{DefaultBranch: github.String("main")}
				if method == "allow_squash_merge" {
					settings.AllowSquashMerge = github.Bool(true)
				} else {
					settings.AllowRebaseMerge = github.Bool(true)
				}
				if failure == "protection without default branch" {
					settings.DefaultBranch = nil
				}
				repo := RepoConfig{Name: "example/project", Settings: settings,
					BranchProtection: []BranchProtection{{Branch: "main", RequireLinearHistory: true}}}
				if failure != "" {
					repo.Workflows = []WorkflowConfig{{DestinationFile: "ci.yml", ReusableWorkflow: "example/ci/.github/workflows/test.yml@main"}}
					repo.Teams = []TeamGrant{{Slug: "core", Permission: "push"}}
				}
				err := reconcileRepositories(context.Background(), client, []RepoConfig{repo}, newDriftReport(io.Discard))
				want := []string{"enable merge method", "protect branch", "select default branch"}
				if failure == "merge method" {
					want = want[:1]
				} else if failure != "" {
					want = want[:2]
				}
				if (err != nil) != (failure != "") || !reflect.DeepEqual(events, want) {
					t.Fatalf("err=%v events=%v want=%v", err, events, want)
				}
				if failure != "merge method" && current[method] != true {
					t.Fatal("configured merge method was not retained")
				}
			})
		}
	}
}
