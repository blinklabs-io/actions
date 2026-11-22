package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

func TestTeamGrantReconciliation(t *testing.T) {
	for _, tt := range []struct {
		name, initial, updated, desired string
		readCode, writeCode             int
		wantWrites                      int
		wantErr                         bool
	}{
		{"add team", `[]`, `[{"slug":"core","permission":"push"}]`, "write", 200, 204, 1, false},
		{"maintain is distinct from write", `[{"slug":"core","permission":"push"}]`, `[{"slug":"core","permission":"maintain"}]`, "maintain", 200, 204, 1, false},
		{"matched", `[{"slug":"core","permission":"push"}]`, "", "push", 200, 204, 0, false},
		{"unlisted teams untouched", `[{"slug":"other","permission":"admin"},{"slug":"core","permission":"push"}]`, "", "push", 200, 204, 0, false},
		{"ambiguous missing repository", `{}`, "", "push", 404, 204, 0, true},
		{"forbidden observation", `{}`, "", "push", 403, 204, 0, true},
		{"incomplete observation", `[{"slug":"core"}]`, "", "push", 200, 204, 0, true},
		{"null observation", `null`, "", "push", 200, 204, 0, true},
		{"update denied", `[]`, "", "push", 200, 403, 1, true},
		{"update not effective", `[]`, `[]`, "push", 200, 204, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case http.MethodGet:
					if req.URL.Path != "/repos/example/project/teams" {
						t.Errorf("unexpected read %s", req.URL.Path)
					}
					r.WriteHeader(tt.readCode)
					body := tt.initial
					if writes > 0 {
						body = tt.updated
					}
					_, _ = io.WriteString(r, body)
				case http.MethodPut:
					writes++
					if req.URL.Path != "/orgs/example/teams/core/repos/example/project" {
						t.Errorf("unexpected write %s", req.URL.Path)
					}
					var body map[string]string
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					want := tt.desired
					if want == "write" {
						want = "push"
					}
					if len(body) != 1 || body["permission"] != want {
						t.Errorf("request = %#v", body)
					}
					r.WriteHeader(tt.writeCode)
				default:
					t.Errorf("unexpected request %s", req.Method)
					r.WriteHeader(500)
				}
				return r.Result(), nil
			})})
			var output strings.Builder
			report := newDriftReport(&output)
			report.beginRepo("example/project", true)
			err := reconcileTeams(context.Background(), client, "example", "project", []TeamGrant{{Slug: "core", Permission: tt.desired}}, report)
			if (err != nil) != tt.wantErr || writes != tt.wantWrites {
				t.Fatalf("err=%v writes=%d", err, writes)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr && strings.Contains(output.String(), "[corrected]") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestTeamObservationPagination(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		reads, writes := 0, 0
		client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
			r := httptest.NewRecorder()
			r.Header().Set("Content-Type", "application/json")
			if req.Method != "GET" {
				writes++
				r.WriteHeader(500)
				return r.Result(), nil
			}
			reads++
			if req.URL.Query().Get("page") == "2" {
				if failSecond {
					r.WriteHeader(403)
				} else {
					_, _ = io.WriteString(r, `[{"slug":"core","permission":"push"}]`)
				}
			} else {
				r.Header().Set("Link", `<https://api.github.com/repos/example/project/teams?page=2>; rel="next"`)
				_, _ = io.WriteString(r, `[{"slug":"other","permission":"admin"}]`)
			}
			return r.Result(), nil
		})})
		report := newDriftReport(io.Discard)
		report.beginRepo("example/project", true)
		err := reconcileTeams(context.Background(), client, "example", "project", []TeamGrant{{Slug: "core", Permission: "push"}}, report)
		if (err != nil) != failSecond || reads != 2 || writes != 0 {
			t.Fatalf("err=%v reads=%d writes=%d", err, reads, writes)
		}
	}
}

func TestTeamConfigurationAndProfiles(t *testing.T) {
	for _, teams := range [][]TeamGrant{
		{{Slug: "core", Permission: ""}}, {{Slug: "../core", Permission: "admin"}},
		{{Slug: "core", Permission: "owner"}}, {{Slug: "core", Permission: "push"}, {Slug: "core", Permission: "admin"}},
	} {
		cfg := Config{Repositories: []RepoConfig{{Name: "example/project", Teams: teams}}}
		if err := expandProfiles(&cfg); err == nil {
			t.Fatalf("accepted invalid teams %#v", teams)
		}
	}
	cfg := Config{Profiles: map[string]Profile{"standard": {Teams: []TeamGrant{{Slug: "core", Permission: "push"}}, Settings: RepoSettings{AllowMergeCommit: github.Bool(false)}}}, Repositories: []RepoConfig{{Name: "example/project", Profile: "standard"}}}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories[0].Teams) != 1 || cfg.Repositories[0].Settings.AllowMergeCommit == nil || *cfg.Repositories[0].Settings.AllowMergeCommit {
		t.Fatal("profile did not inherit teams and explicit false")
	}
	cfg.Repositories = []RepoConfig{{Name: "example/project", Profile: "standard", Teams: []TeamGrant{{Slug: "ops", Permission: "admin"}}}}
	if err := expandProfiles(&cfg); err == nil {
		t.Fatal("accepted explicit team override")
	}
}

func TestSharedActionsConfiguration(t *testing.T) {
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, repo := range cfg.Repositories {
		if repo.Name != "blinklabs-io/shared-actions" {
			continue
		}
		if repo.Private == nil || *repo.Private || len(repo.Workflows) != 0 {
			t.Fatal("bootstrap must be explicitly public without private workflow callers")
		}
		want := map[string]string{"core": "push", "ops": "admin", "pms": "maintain", "docs": "push"}
		if len(repo.Teams) != len(want) {
			t.Fatal(repo.Teams)
		}
		for _, team := range repo.Teams {
			if want[team.Slug] != team.Permission {
				t.Fatal(team)
			}
		}
		if repo.Settings.AllowSquashMerge == nil || !*repo.Settings.AllowSquashMerge || repo.Settings.AllowMergeCommit == nil || *repo.Settings.AllowMergeCommit || repo.Settings.AllowRebaseMerge == nil || *repo.Settings.AllowRebaseMerge {
			t.Fatal("expected squash-only merging")
		}
		if len(repo.BranchProtection) != 1 || repo.BranchProtection[0].Branch != "main" || repo.BranchProtection[0].RequiredApprovingReviewCount != 1 {
			t.Fatal("expected main review protection")
		}
		return
	}
	t.Fatal("missing shared-actions configuration")
}

func TestTeamVerificationFailureStopsLaterGrants(t *testing.T) {
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		if req.Method == "PUT" {
			writes++
			r.WriteHeader(204)
		} else if writes > 0 {
			r.WriteHeader(403)
		} else {
			_, _ = io.WriteString(r, `[]`)
		}
		return r.Result(), nil
	})})
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("example/private", false)
	err := reconcileTeams(context.Background(), client, "example", "private", []TeamGrant{{Slug: "core", Permission: "push"}, {Slug: "ops", Permission: "admin"}}, report)
	if err == nil || writes != 1 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"example/private", "core", "ops", "push", "admin"} {
		if strings.Contains(output.String(), s) {
			t.Fatalf("private metadata leaked: %s", output.String())
		}
	}
}

func TestMergeMethodSettingsPatch(t *testing.T) {
	for _, missing := range []bool{false, true} {
		writes := 0
		client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
			r := httptest.NewRecorder()
			r.Header().Set("Content-Type", "application/json")
			if req.Method == "GET" {
				body := `{"allow_merge_commit":true,"allow_rebase_merge":true,"allow_squash_merge":true}`
				if missing {
					body = `{"allow_merge_commit":true,"allow_squash_merge":true}`
				}
				_, _ = io.WriteString(r, body)
			} else {
				writes++
				if req.Method != "PATCH" {
					t.Error(req.Method)
				}
				var body map[string]bool
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body) != 2 {
					t.Errorf("changed unconfigured or matched setting: %#v", body)
				}
				for _, key := range []string{"allow_merge_commit", "allow_rebase_merge"} {
					v, ok := body[key]
					if !ok || v {
						t.Error(body)
					}
				}
				_, _ = io.WriteString(r, `{}`)
			}
			return r.Result(), nil
		})})
		report := newDriftReport(io.Discard)
		report.beginRepo("example/project", true)
		err := reconcileRepoSettings(context.Background(), client, "example", "project", RepoSettings{AllowMergeCommit: github.Bool(false), AllowRebaseMerge: github.Bool(false), AllowSquashMerge: github.Bool(true)}, report)
		if (err != nil) != missing || (writes == 0) != missing {
			t.Fatalf("err=%v writes=%d", err, writes)
		}
	}
}

func TestBranchProtectionFailurePreventsTeamGrants(t *testing.T) {
	teamCalls := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "teams"):
			teamCalls++
			r.WriteHeader(500)
		case req.URL.Path == "/repos/example/project":
			_, _ = io.WriteString(r, `{"id":1,"full_name":"example/project","private":false,"visibility":"public"}`)
		default:
			r.WriteHeader(403)
		}
		return r.Result(), nil
	})})
	report := newDriftReport(io.Discard)
	err := reconcileRepositories(context.Background(), client, []RepoConfig{{Name: "example/project", Teams: []TeamGrant{{Slug: "core", Permission: "push"}}, BranchProtection: []BranchProtection{{Branch: "main", RequiredApprovingReviewCount: 1}}}}, report)
	if err == nil || teamCalls != 0 {
		t.Fatalf("err=%v team calls=%d", err, teamCalls)
	}
}

func TestTeamGrantRequiresRecordedPlan(t *testing.T) {
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		if req.Method != "GET" {
			writes++
			r.WriteHeader(204)
		} else {
			_, _ = io.WriteString(r, `[]`)
		}
		return r.Result(), nil
	})})
	report := newDriftReport(driftFailWriter{err: errReportUnavailable})
	report.beginRepo("example/project", true)
	if err := reconcileTeams(context.Background(), client, "example", "project", []TeamGrant{{Slug: "core", Permission: "push"}}, report); err == nil || writes != 0 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
}
