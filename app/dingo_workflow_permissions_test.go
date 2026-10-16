package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDingoConventionalWorkflowUsesReadOnlyPermissions(t *testing.T) {
	cfg := loadRealConfig(t)
	for _, repo := range cfg.Repositories {
		if repo.Name != "blinklabs-io/dingo" {
			continue
		}
		for _, workflow := range repo.Workflows {
			if workflow.DestinationFile != "conventional-commits.yml" {
				continue
			}
			if got := workflow.Permissions["contents"]; got != "read" {
				t.Fatalf("configured contents permission = %q, want read", got)
			}
			template, err := newWorkflowTemplate(t)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := renderWorkflow(template, workflow)
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Permissions map[string]string `yaml:"permissions"`
			}
			if err := yaml.Unmarshal(rendered, &parsed); err != nil {
				t.Fatal(err)
			}
			if got := parsed.Permissions["contents"]; got != "read" {
				t.Fatalf("rendered contents permission = %q, want read", got)
			}
			return
		}
		t.Fatal("Dingo conventional-commits workflow is missing")
	}
	t.Fatal("Dingo is missing from repos-config.yaml")
}
