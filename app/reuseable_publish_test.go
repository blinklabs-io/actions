package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReusablePublishReleaseAssetAuthentication(t *testing.T) {
	workflowData, err := os.ReadFile("../.github/workflows/reuseable-publish.yml")
	if err != nil {
		t.Fatalf("read reusable publish workflow: %v", err)
	}

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowData, &workflow); err != nil {
		t.Fatalf("parse reusable publish workflow: %v", err)
	}

	for _, step := range workflow.Jobs["build-binaries"].Steps {
		if step.Name != "Upload release asset" {
			continue
		}
		if got, want := step.Env["GH_TOKEN"], "${{ github.token }}"; got != want {
			t.Fatalf("Upload release asset GH_TOKEN = %q, want %q", got, want)
		}
		curlCommand := continuedShellCommand(step.Run, "curl")
		if curlCommand == "" {
			t.Fatal("Upload release asset must invoke curl")
		}
		if !strings.Contains(curlCommand, `-H "Authorization: Bearer $GH_TOKEN"`) {
			t.Fatal("Upload release asset must authenticate with GH_TOKEN")
		}
		return
	}

	t.Fatal("build-binaries job has no Upload release asset step")
}

func continuedShellCommand(script, command string) string {
	var commandLines []string
	capturing := false
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if !capturing {
			if !strings.HasPrefix(line, command+" ") {
				continue
			}
			capturing = true
		}
		commandLines = append(commandLines, strings.TrimSuffix(line, `\`))
		if !strings.HasSuffix(line, `\`) {
			break
		}
	}
	return strings.Join(commandLines, " ")
}
