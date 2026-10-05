package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGoLibraryProxyPullHandlesSkippedExamples(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/reuseable-go-library-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If    string   `yaml:"if"`
			Needs []string `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["pull-go-module"]
	// A status function overrides GitHub's implicit success() check, which
	// otherwise propagates the skipped optional example job to proxy indexing.
	if !strings.Contains(job.If, "!cancelled()") {
		t.Fatal("proxy pull condition must handle skipped example builds without running on cancellation")
	}
	if !strings.Contains(job.If, "needs.publish-release.result == 'success'") {
		t.Fatal("proxy pull must require successful publication")
	}
	if len(job.Needs) != 1 || job.Needs[0] != "publish-release" {
		t.Fatalf("proxy pull dependencies = %v, want publish-release", job.Needs)
	}
}

func TestConfiguredLibraryReleasesPullGoModule(t *testing.T) {
	for _, file := range []string{"reuseable-go-library-release.yml", "reuseable-go-module-release.yml"} {
		data, err := os.ReadFile("../.github/workflows/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			On struct {
				Call struct {
					Inputs map[string]struct {
						Default bool `yaml:"default"`
					} `yaml:"inputs"`
				} `yaml:"workflow_call"`
			} `yaml:"on"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		if !workflow.On.Call.Inputs["pull-go-module"].Default {
			t.Errorf("%s does not index Go modules by default", file)
		}
	}
	count := 0
	for _, repo := range loadRealConfig(t).Repositories {
		for _, workflow := range repo.Workflows {
			if !strings.Contains(workflow.ReusableWorkflow, "/reuseable-go-library-release.yml@") &&
				!strings.Contains(workflow.ReusableWorkflow, "/reuseable-go-module-release.yml@") {
				continue
			}
			count++
			if value, ok := workflow.Params["pull-go-module"]; ok && value != "true" {
				t.Errorf("%s release disables Go module indexing: %q", repo.Name, value)
			}
		}
	}
	if count == 0 {
		t.Fatal("no Go library releases configured")
	}
}
