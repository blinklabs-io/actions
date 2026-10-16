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

func TestProtocolLibrariesHaveReadOnlyWorkflowPermissions(t *testing.T) {
	cases := []struct {
		repo        string
		destination string
	}{
		{repo: "blinklabs-io/gouroboros", destination: "conventional-commits.yml"},
		{repo: "blinklabs-io/gouroboros", destination: "go-test.yml"},
		{repo: "blinklabs-io/ouroboros-mock", destination: "conventional-commits.yml"},
		{repo: "blinklabs-io/ouroboros-mock", destination: "golangci-lint.yml"},
		{repo: "blinklabs-io/ouroboros-mock", destination: "nilaway.yml"},
	}
	cfg := loadRealConfig(t)
	workflowTemplate, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	pipelineTemplate := newPipelineTemplate(t)

	for _, test := range cases {
		t.Run(test.repo+"/"+test.destination, func(t *testing.T) {
			var target *WorkflowConfig
			var repoWorkflows []WorkflowConfig
			for _, repo := range cfg.Repositories {
				if repo.Name != test.repo {
					continue
				}
				repoWorkflows = repo.Workflows
				for i := range repo.Workflows {
					if repo.Workflows[i].DestinationFile == test.destination {
						target = &repo.Workflows[i]
						break
					}
				}
				break
			}
			if target == nil {
				t.Fatalf("%s workflow %q is missing", test.repo, test.destination)
			}

			var permissions map[string]string
			if target.Pipeline == "" {
				rendered, err := renderWorkflow(workflowTemplate, *target)
				if err != nil {
					t.Fatal(err)
				}
				var parsed struct {
					Permissions map[string]string `yaml:"permissions"`
				}
				if err := yaml.Unmarshal(rendered, &parsed); err != nil {
					t.Fatalf("parse rendered workflow: %v", err)
				}
				permissions = parsed.Permissions
			} else {
				var members []WorkflowConfig
				for _, workflow := range repoWorkflows {
					if workflow.Pipeline == target.Pipeline {
						members = append(members, workflow)
					}
				}
				rendered, err := renderPipeline(pipelineTemplate, members)
				if err != nil {
					t.Fatal(err)
				}
				var parsed parsedWorkflow
				if err := yaml.Unmarshal(rendered, &parsed); err != nil {
					t.Fatalf("parse rendered pipeline: %v", err)
				}
				job, ok := parsed.Jobs[target.jobID()]
				if !ok {
					t.Fatalf("rendered pipeline has no job %q", target.jobID())
				}
				permissions = job.Permissions
			}

			if len(permissions) != 1 || permissions["contents"] != "read" {
				t.Fatalf("rendered permissions = %#v, want only contents: read", permissions)
			}
		})
	}
}
