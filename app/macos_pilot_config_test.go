package main

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMacOSPilotWorkflowSettings(t *testing.T) {
	cases := map[string]map[string]struct {
		kind string
		node bool
	}{
		"blinklabs-io/adder": {"test-macos-release.yml": {"pkg", false}},
		"blinklabs-io/bursa": {"test-macos-release.yml": {"pkg", true}, "test-macos-cli-release.yml": {"zip", false}},
	}
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range loadRealConfig(t).Repositories {
		files, ok := cases[repo.Name]
		if !ok {
			continue
		}
		delete(cases, repo.Name)
		for file, want := range files {
			wf := findWorkflow(t, repo.Workflows, file)
			if len(wf.Triggers) != 1 {
				t.Errorf("%s %s must be manual only", repo.Name, file)
			}
			if _, ok := wf.Triggers["workflow_dispatch"]; !ok {
				t.Errorf("%s %s missing manual trigger", repo.Name, file)
			}
			if wf.Permissions["contents"] != "read" || len(wf.Permissions) != 1 {
				t.Errorf("%s %s has unexpected permissions", repo.Name, file)
			}
			if (wf.Secrets["apple-installer-certificate"] != "") != (want.kind == "pkg") {
				t.Errorf("%s %s has unexpected installer credential mapping", repo.Name, file)
			}
			var config struct {
				ArtifactType string `json:"artifact_type"`
				Build        struct {
					Web *struct {
						Reuse bool `json:"reuse"`
					} `json:"web"`
				} `json:"build"`
			}
			if err := json.Unmarshal([]byte(wf.Params["config"]), &config); err != nil {
				t.Fatal(err)
			}
			if config.ArtifactType != want.kind {
				t.Errorf("%s %s unexpected artifact type %q", repo.Name, file, config.ArtifactType)
			}
			if want.node && (config.Build.Web == nil || config.Build.Web.Reuse) {
				t.Errorf("%s %s must build its web bundle from a clean checkout", repo.Name, file)
			}
			rendered, err := renderWorkflow(tmpl, wf)
			if err != nil {
				t.Fatal(err)
			}
			var output struct {
				Jobs map[string]struct {
					With map[string]interface{} `yaml:"with"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal(rendered, &output); err != nil {
				t.Fatal(err)
			}
			if _, ok := output.Jobs["orchestrate"].With["config"].(string); !ok {
				t.Errorf("%s %s config must render as a string", repo.Name, file)
			}
			if want.node {
				if got, ok := output.Jobs["orchestrate"].With["node-version"].(string); !ok || got != "22.x" {
					t.Errorf("node-version must be a string, got %#v", output.Jobs["orchestrate"].With["node-version"])
				}
			}
		}
	}
	if len(cases) != 0 {
		t.Errorf("missing pilot repositories: %v", cases)
	}
}
