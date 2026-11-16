package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProtectedConventionalCommitWorkflowsMatchGovernance(t *testing.T) {
	const revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const source = ".github/workflows/reuseable-conventional-commits.yml"
	const workflow = "blinklabs-io/actions/" + source
	t.Setenv("GITHUB_REPOSITORY", "blinklabs-io/actions")
	t.Setenv("GITHUB_REF", "refs/heads/main")

	checkedIn, err := os.ReadFile("../.github/workflows/conventional-commits.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct {
		Jobs map[string]struct {
			Uses string `yaml:"uses"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(checkedIn, &wrapper); err != nil {
		t.Fatal(err)
	}
	path, checkedInRevision, ok := strings.Cut(wrapper.Jobs["orchestrate"].Uses, "@")
	if !ok || path != workflow || !commitSHAPattern.MatchString(checkedInRevision) {
		t.Fatal("checked-in Conventional Commits workflow must use an immutable revision")
	}

	cfg := loadRealConfig(t)
	if err := resolvePinnedWorkflowRefs(&cfg, func(path string) (string, []byte, error) {
		switch path {
		case source, ".github/workflows/reuseable-helm-publish.yml":
			return revision, []byte(callableWorkflowYAML), nil
		default:
			t.Fatalf("unexpected pinned workflow %q", path)
			return "", nil, nil
		}
	}); err != nil {
		t.Fatal(err)
	}
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
			if wf.ReusableWorkflow != workflow+"@"+revision {
				t.Errorf("%s does not resolve the Conventional Commits workflow to its latest revision", repo.Name)
			}
			rendered, err := renderWorkflow(tmpl, wf)
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.ReplaceAll(rendered, []byte(revision), []byte(checkedInRevision))
			if !bytes.Equal(want, checkedIn) {
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
