package main

import (
	"os"
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
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Errorf("sync workflow permissions = %#v, want only contents: read", workflow.Permissions)
	}
}
