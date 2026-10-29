package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHelmPublisherMigrationIsAtomic(t *testing.T) {
	cfg := loadRealConfig(t)
	var workflows []WorkflowConfig
	for _, repo := range cfg.Repositories {
		if repo.Name == "blinklabs-io/helm-charts" {
			workflows = repo.Workflows
		}
	}
	var publisher WorkflowConfig
	for _, workflow := range workflows {
		if workflow.DestinationFile == "publish-helm-charts.yml" {
			publisher = workflow
		}
	}
	if len(publisher.Supersedes) != 37 {
		t.Fatalf("migration replaces %d chart publishers, want 37", len(publisher.Supersedes))
	}
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, workflow := range workflows {
		if workflow.DestinationFile != publisher.DestinationFile {
			rendered, err := renderWorkflow(tmpl, workflow)
			if err != nil {
				t.Fatal(err)
			}
			contents[workflow.DestinationFile] = string(rendered)
		}
	}
	for _, name := range publisher.Supersedes {
		contents[name] = "old publisher"
	}
	t.Chdir("..")
	for _, failAt := range []string{"", "tree"} {
		t.Run("failure="+failAt, func(t *testing.T) {
			g := &recordingGitHub{contents: contents, failAt: failAt}
			syncWorkflows(context.Background(), newRecordingClient(t, g), "o", "r", workflows)
			var tree struct {
				Entries []struct {
					Path    string  `json:"path"`
					SHA     *string `json:"sha"`
					Content *string `json:"content"`
				} `json:"tree"`
			}
			if err := json.Unmarshal([]byte(g.tree()), &tree); err != nil {
				t.Fatal(err)
			}
			if len(tree.Entries) != 38 {
				t.Fatalf("staged %d files, want one publisher and 37 deletions", len(tree.Entries))
			}
			deletions := map[string]bool{}
			for _, entry := range tree.Entries {
				if entry.Path == ".github/workflows/"+publisher.DestinationFile {
					if entry.Content == nil || !strings.Contains(*entry.Content, publisher.ReusableWorkflow) {
						t.Fatal("replacement workflow is missing its pinned reusable call")
					}
				} else {
					if entry.SHA != nil || entry.Content != nil {
						t.Fatalf("old publisher %s was not deleted", entry.Path)
					}
					deletions[entry.Path] = true
				}
			}
			for _, name := range publisher.Supersedes {
				if !deletions[".github/workflows/"+name] {
					t.Errorf("old publisher %s was not removed", name)
				}
			}
			wantUpdates := 1
			if failAt != "" {
				wantUpdates = 0
			}
			if got := g.count(http.MethodPatch, "/git/refs/"); got != wantUpdates {
				t.Errorf("branch updates = %d, want %d", got, wantUpdates)
			}
		})
	}
}
