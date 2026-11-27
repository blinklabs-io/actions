package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestManagedConfigsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"wallet", `{"artifact_type":"pkg"}`, true},
		{"../escape", `{}`, false}, {"nested/file", `{}`, false}, {".hidden", `{}`, false},
		{"bad-name", `[]`, false}, {"null", `null`, false}, {"bad-json", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateManagedConfigs(map[string]string{tc.name: tc.raw}); (err == nil) != tc.valid {
				t.Fatalf("validation error %v, want valid=%t", err, tc.valid)
			}
		})
	}
}

func TestManagedConfigsSyncAtomically(t *testing.T) {
	t.Chdir("..")
	configs := map[string]string{"macos-wallet": `{ "artifact_type": "pkg" }`, "macos-cli": `{"artifact_type":"zip"}`}
	for _, tc := range []struct {
		name               string
		fake               *recordingGitHub
		wantErr, wantWrite bool
	}{
		{"create", &recordingGitHub{contents: map[string]string{}}, false, true},
		{"unchanged", &recordingGitHub{contents: map[string]string{"macos-wallet.json": "{\"artifact_type\":\"pkg\"}\n", "macos-cli.json": "{\"artifact_type\":\"zip\"}\n"}}, false, false},
		{"read failure", &recordingGitHub{contents: map[string]string{}, contentsErrors: map[string]int{"macos-wallet.json": 500}}, true, false},
		{"tree failure", &recordingGitHub{contents: map[string]string{}, failAt: "tree"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := syncWorkflowsWithConfigs(context.Background(), newRecordingClient(t, tc.fake), "o", "r", nil, "", configs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("sync error %v", err)
			}
			if tc.fake.saw(http.MethodPost, "/graphql") != tc.wantWrite {
				t.Fatalf("unexpected branch update: %v", tc.fake.requests)
			}
			if tc.wantWrite {
				tree := tc.fake.tree()
				for _, path := range []string{".github/governance/macos-wallet.json", ".github/governance/macos-cli.json"} {
					if !strings.Contains(tree, path) {
						t.Errorf("missing %s in %s", path, tree)
					}
				}
				var body struct {
					Tree []struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					} `json:"tree"`
				}
				if err := json.Unmarshal([]byte(tree), &body); err != nil {
					t.Fatal(err)
				}
				want := map[string]string{".github/governance/macos-wallet.json": "{\"artifact_type\":\"pkg\"}\n", ".github/governance/macos-cli.json": "{\"artifact_type\":\"zip\"}\n"}
				for _, entry := range body.Tree {
					if expected, ok := want[entry.Path]; ok {
						if entry.Content != expected {
							t.Errorf("%s contains %q, want %q", entry.Path, entry.Content, expected)
						}
						delete(want, entry.Path)
					}
				}
				if len(want) != 0 {
					t.Fatalf("missing staged settings: %v", want)
				}
				if tc.fake.count(http.MethodPost, "/git/commits") != 1 {
					t.Fatal("configs must share a single commit")
				}
			}
		})
	}
}

func TestBursaManagedConfigsMatchSignedPilots(t *testing.T) {
	for _, repo := range loadRealConfig(t).Repositories {
		if repo.Name != "blinklabs-io/bursa" {
			continue
		}
		for name, file := range map[string]string{"macos-wallet": "test-macos-release.yml", "macos-cli": "test-macos-cli-release.yml"} {
			pilot := findWorkflow(t, repo.Workflows, file)
			config := repo.ManagedConfigs[name]
			if config == "" || config != pilot.Params["config"] || !json.Valid([]byte(config)) {
				t.Errorf("%s must match its signed pilot", name)
			}
		}
		return
	}
	t.Fatal("Bursa missing")
}
