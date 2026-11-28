package main

import (
	"bytes"
	"os"
	"testing"
)

func TestConventionalCommitsUseSharedStandard(t *testing.T) {
	const shared = "blinklabs-io/shared-actions/.github/workflows/reuseable-conventional-commits.yml@d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
	const local = "./.github/workflows/reuseable-conventional-commits.yml"
	for _, repo := range loadRealConfig(t).Repositories {
		want := shared
		if repo.Name == "blinklabs-io/shared-actions" {
			want = local
		}
		count := 0
		for _, wf := range repo.Workflows {
			if wf.DestinationFile == "conventional-commits.yml" {
				count++
				if wf.ReusableWorkflow != want || wf.PinToLatestChange {
					t.Errorf("%s: conventional commits caller = %q, want %q", repo.Name, wf.ReusableWorkflow, want)
				}
			}
		}
		if count != 1 {
			t.Errorf("%s: got %d conventional commits callers, want one", repo.Name, count)
		}
	}
}

func TestProtectedConventionalCommitWorkflowsMatchGovernance(t *testing.T) {
	const workflow = "blinklabs-io/shared-actions/.github/workflows/reuseable-conventional-commits.yml@d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
	checkedIn, err := os.ReadFile("../.github/workflows/conventional-commits.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadRealConfig(t)
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{"blinklabs-io/actions": false, "blinklabs-io/docker-template": false}
	for _, repo := range cfg.Repositories {
		if _, ok := targets[repo.Name]; !ok {
			continue
		}
		for _, wf := range repo.Workflows {
			if wf.DestinationFile != "conventional-commits.yml" {
				continue
			}
			targets[repo.Name] = true
			if wf.ReusableWorkflow != workflow || wf.PinToLatestChange {
				t.Errorf("%s must use the reviewed public Conventional Commits revision without dynamic pinning", repo.Name)
			}
			rendered, err := renderWorkflow(tmpl, wf)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rendered, checkedIn) {
				t.Errorf("%s governance output differs from the checked-in wrapper", repo.Name)
			}
		}
	}
	for name, found := range targets {
		if !found {
			t.Errorf("%s has no Conventional Commits workflow", name)
		}
	}
}
