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

func TestGovernanceSyncChecksContextBeforeAppToken(t *testing.T) {
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
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
				With map[string]string `yaml:"with"`
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
	if job.Environment != "" || !strings.Contains(job.If, "refs/heads/main") {
		t.Fatalf("sync is not restricted to the main branch without a paid environment: %+v", job)
	}
	verify, mint, identity, engine := -1, -1, -1, -1
	for i, step := range job.Steps {
		if strings.Contains(step.Run, "verify-governance-context") {
			verify = i
		}
		if strings.Contains(step.Uses, "actions/create-github-app-token@") {
			mint = i
			if step.With["client-id"] != "${{ secrets.GOVERNANCE_APP_CLIENT_ID }}" ||
				step.With["private-key"] != "${{ secrets.GOVERNANCE_APP_PRIVATE_KEY }}" {
				t.Error("sync does not use the existing governance app credentials")
			}
		}
		if strings.Contains(step.Run, `test "$APP_SLUG" = blinklabs-governance`) {
			identity = i
			if step.Env["APP_SLUG"] != "${{ steps.generate-token.outputs.app-slug }}" {
				t.Error("the identity check does not use the minted token's app slug")
			}
		}
		if strings.TrimSpace(step.Run) == "go run ./app" {
			engine = i
		}
	}
	if verify < 0 || mint < 0 || identity < 0 || engine < 0 || verify >= mint || mint >= identity || identity >= engine {
		t.Fatalf("context, token, identity and execution are out of order: %d %d %d %d", verify, mint, identity, engine)
	}
}

func TestReleaseFanoutUsesCurrentMainWithoutEnvironment(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/release-fanout.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Environment string `yaml:"environment"`
			Steps       []struct {
				ID   string            `yaml:"id"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["apply"]
	if job.Environment != "" || !strings.Contains(job.If, "refs/heads/main") {
		t.Fatalf("fan-out apply is not restricted to the main branch: %+v", job)
	}
	checked, authenticated, identity, target := -1, -1, -1, -1
	for i, step := range job.Steps {
		if strings.Contains(step.Run, "git/ref/heads/main") && strings.Contains(step.Run, `test "$current" = "$GITHUB_SHA"`) {
			checked = i
		}
		if strings.Contains(step.Uses, "actions/create-github-app-token@") {
			authenticated = i
			if step.ID != "app" || step.With["client-id"] != "${{ secrets.GOVERNANCE_APP_CLIENT_ID }}" ||
				step.With["private-key"] != "${{ secrets.GOVERNANCE_APP_PRIVATE_KEY }}" {
				t.Error("fan-out does not use the existing governance app credentials")
			}
			if step.With["repositories"] != "${{ matrix.name }}" || step.With["permission-contents"] != "write" {
				t.Errorf("fan-out token is not restricted to the target repository: %+v", step.With)
			}
			for key := range step.With {
				if strings.HasPrefix(key, "permission-") && key != "permission-contents" {
					t.Errorf("fan-out token requests unexpected permission %s", key)
				}
			}
		}
		if strings.TrimSpace(step.Run) == `test "$APP_SLUG" = blinklabs-governance` &&
			step.Env["APP_SLUG"] == "${{ steps.app.outputs.app-slug }}" {
			identity = i
		}
		if strings.Contains(step.Uses, "actions/checkout@") && step.With["repository"] == "${{ matrix.repo }}" {
			target = i
		}
	}
	if checked < 0 || authenticated < 0 || identity < 0 || target < 0 ||
		checked >= authenticated || authenticated >= identity || identity >= target {
		t.Fatalf("fan-out revision check, token, identity and target checkout are out of order: %d %d %d %d", checked, authenticated, identity, target)
	}
}
