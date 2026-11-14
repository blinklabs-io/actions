package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSyncWorkflowHasReadOnlyDefaultPermissions(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/sync.yaml")
	if err != nil {
		t.Fatalf("read sync workflow: %v", err)
	}
	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse sync workflow: %v", err)
	}
	if len(workflow.Permissions) != 2 || workflow.Permissions["contents"] != "read" || workflow.Permissions["actions"] != "read" {
		t.Errorf("sync workflow permissions = %#v, want contents and actions read", workflow.Permissions)
	}
}

func TestGovernanceSyncRequiresEnvironmentBeforeAppToken(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/sync.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Environment string `yaml:"environment"`
			Steps       []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"push", "schedule", "workflow_dispatch"} {
		if _, ok := workflow.On[trigger]; !ok {
			t.Errorf("missing %s trigger", trigger)
		}
	}
	if _, ok := workflow.On["pull_request"]; ok {
		t.Error("pull requests must not trigger a mutating sync")
	}
	job := workflow.Jobs["orchestrate-governance"]
	if job.Environment != "governance-sync" || !strings.Contains(job.If, "refs/heads/main") {
		t.Fatalf("sync is not restricted to the protected main environment: %+v", job)
	}
	verify, mint := -1, -1
	for i, step := range job.Steps {
		if strings.Contains(step.Run, "verify-governance-approval") {
			verify = i
		}
		if strings.Contains(step.Uses, "actions/create-github-app-token@") {
			mint = i
		}
	}
	if verify < 0 || mint < 0 || verify >= mint {
		t.Fatalf("approval check %d must precede app token %d", verify, mint)
	}
}

func TestReleaseFanoutUsesMainOnlyEnvironment(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/release-fanout.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Environment string `yaml:"environment"`
			Steps       []struct {
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["apply"]
	if job.Environment != "release-fanout" || !strings.Contains(job.If, "refs/heads/main") {
		t.Fatalf("fan-out apply is not restricted to the main environment: %+v", job)
	}
	checked, authenticated := false, false
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "deployment-branch-policies") {
			checked = true
		}
		if strings.Contains(step.With["private-key"], "FANOUT_APP_PRIVATE_KEY") {
			authenticated = true
			if !checked {
				t.Error("fan-out must verify environment protection before minting a token")
			}
		}
	}
	if !checked || !authenticated {
		t.Fatal("fan-out environment check or credentials are missing")
	}
}
