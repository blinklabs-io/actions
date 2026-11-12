package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSharedLintDefaultsUseReviewedRelease(t *testing.T) {
	var versions []string
	for _, tc := range []struct{ file, input string }{
		{"reuseable-golangci-lint.yml", "version"},
		{"reuseable-go-test.yml", "golangci-version"},
	} {
		data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			On struct {
				WorkflowCall struct {
					Inputs map[string]struct {
						Default string `yaml:"default"`
					} `yaml:"inputs"`
				} `yaml:"workflow_call"`
			} `yaml:"on"`
		}
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		version := wf.On.WorkflowCall.Inputs[tc.input].Default
		if !regexp.MustCompile(`^v[0-9]+[.][0-9]+[.][0-9]+$`).MatchString(version) {
			t.Errorf("%s: default %q is not a complete release version", tc.file, version)
		}
		versions = append(versions, version)
	}
	if versions[0] != versions[1] {
		t.Errorf("shared lint defaults differ: %s and %s", versions[0], versions[1])
	}
}

func TestManagedGoTestPlatformSelectorsMatchMatrix(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/reuseable-go-test.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			WorkflowCall struct {
				Inputs map[string]struct {
					Default string `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_call"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	var defaults []string
	if err := json.Unmarshal([]byte(wf.On.WorkflowCall.Inputs["platforms"].Default), &defaults); err != nil {
		t.Fatalf("invalid default platform matrix: %v", err)
	}
	if len(defaults) != 1 || defaults[0] != "ubuntu-26.04" {
		t.Fatalf("default platform matrix = %v, want Ubuntu 26.04", defaults)
	}
	cfg := loadRealConfig(t)
	checked := 0
	for _, repo := range cfg.Repositories {
		for _, wf := range repo.Workflows {
			if !strings.Contains(wf.ReusableWorkflow, "/reuseable-go-test.yml@") {
				continue
			}
			checked++
			platforms := append([]string(nil), defaults...)
			if raw := wf.Params["platforms"]; raw != "" {
				if err := json.Unmarshal([]byte(raw), &platforms); err != nil {
					t.Fatalf("invalid managed platform matrix: %v", err)
				}
			}
			for _, selector := range []string{"golangci-platform", "nilaway-platform"} {
				selected := wf.Params[selector]
				if selected == "" {
					continue
				}
				found := false
				for _, platform := range platforms {
					found = found || platform == selected
				}
				if !found {
					t.Errorf("managed %s selector %q does not appear in %v", selector, selected, platforms)
				}
			}
			for _, platform := range platforms {
				if platform == "ubuntu-latest" {
					t.Error("managed Go test matrix uses a floating Ubuntu runner")
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no managed Go test callers checked")
	}
}

func TestGoTestRejectsAbsentToolPlatformSelectors(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/reuseable-go-test.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["go-test"].Steps {
		if step.Name != "Validate tool platform selectors" {
			continue
		}
		for _, selector := range []string{"golangci-platform", "nilaway-platform"} {
			if !strings.Contains(step.If, "!contains(fromJSON(inputs.platforms), inputs."+selector+")") {
				t.Errorf("platform validation does not check %s", selector)
			}
		}
		if !strings.Contains(step.Run, "exit 1") {
			t.Error("invalid platform selector does not fail the job")
		}
		return
	}
	t.Fatal("platform selector validation step not found")
}

func TestManagedLintVersionOverridesAreExactReleases(t *testing.T) {
	cfg := loadRealConfig(t)
	exact := regexp.MustCompile(`^v[0-9]+[.][0-9]+[.][0-9]+$`)
	checked := 0
	for _, repo := range cfg.Repositories {
		for _, wf := range repo.Workflows {
			key := ""
			switch {
			case strings.Contains(wf.ReusableWorkflow, "/reuseable-golangci-lint.yml@"):
				key = "version"
			case strings.Contains(wf.ReusableWorkflow, "/reuseable-go-test.yml@"):
				key = "golangci-version"
			default:
				continue
			}
			checked++
			if version, overridden := wf.Params[key]; overridden && !exact.MatchString(version) {
				t.Errorf("managed lint version override %q is not a complete release", version)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no managed lint callers checked")
	}
}
