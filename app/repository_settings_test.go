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
