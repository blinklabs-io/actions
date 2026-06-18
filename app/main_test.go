package main

import (
	"os"
	"os/exec"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// parseRepoString
// ---------------------------------------------------------------------------

func TestParseRepoString_Valid(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
	}{
		{"blinklabs-io/actions", "blinklabs-io", "actions"},
		{"org/repo-name", "org", "repo-name"},
		// SplitN(n=2) keeps any extra slashes in the repo segment
		{"owner/repo/extra", "owner", "repo/extra"},
	}

	for _, tt := range tests {
		owner, repo := parseRepoString(tt.input)
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("parseRepoString(%q) = (%q, %q), want (%q, %q)",
				tt.input, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

// TestParseRepoString_Invalid uses the subprocess pattern to assert that
// invalid repo name values in repos-config.yaml trigger a non-zero exit.
// Each sub-test re-runs the test binary with TEST_INVALID_REPO set so that
// parseRepoString is called with the bad value and os.Exit(1) is expected.
func TestParseRepoString_Invalid(t *testing.T) {
	// When running as the subprocess, call parseRepoString with the provided
	// value and return. TEST_INVALID_REPO_SET=1 is the subprocess trigger so
	// that an empty TEST_INVALID_REPO value is handled correctly.
	if os.Getenv("TEST_INVALID_REPO_SET") == "1" {
		parseRepoString(os.Getenv("TEST_INVALID_REPO"))
		return
	}

	invalid := []string{
		"no-slash",       // missing "/" entirely
		"/missing-owner", // empty owner segment
		"missing-repo/",  // empty repo segment
		"",               // completely empty name
	}

	for _, input := range invalid {
		input := input
		t.Run(input, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestParseRepoString_Invalid")
			cmd.Env = append(os.Environ(),
				"TEST_INVALID_REPO_SET=1",
				"TEST_INVALID_REPO="+input,
			)
			err := cmd.Run()
			if err == nil {
				t.Errorf("parseRepoString(%q): expected non-zero exit, but process succeeded", input)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Config YAML unmarshaling
// ---------------------------------------------------------------------------

func TestConfigUnmarshal_Valid(t *testing.T) {
	raw := `
repositories:
  - name: blinklabs-io/test-repo
    settings:
      delete_branch_on_merge: true
    collaborators:
      - username: alice
        permission: write
    workflows:
      - destination_file: issue-close.yaml
        workflow_name: "Issue Close"
        reusable_workflow: "blinklabs-io/actions/.github/workflows/reuseable-test-issue-on-close.yml@main"
        params:
          issue_number: "${{ github.event.issue.number }}"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unexpected error on valid config: %v", err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("expected 1 repository, got %d", len(cfg.Repositories))
	}
	repo := cfg.Repositories[0]
	if repo.Name != "blinklabs-io/test-repo" {
		t.Errorf("unexpected repo name: %q", repo.Name)
	}
	if !repo.Settings.DeleteBranchOnMerge {
		t.Error("expected delete_branch_on_merge to be true")
	}
	if len(repo.Collaborators) != 1 || repo.Collaborators[0].Username != "alice" {
		t.Error("collaborators not parsed correctly")
	}
	if len(repo.Workflows) != 1 || repo.Workflows[0].DestinationFile != "issue-close.yaml" {
		t.Error("workflows not parsed correctly")
	}
}

func TestConfigUnmarshal_MalformedYAML(t *testing.T) {
	raw := `
repositories:
  - name: [unclosed bracket
    settings:
      delete_branch_on_merge: true
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err == nil {
		t.Error("expected error for malformed YAML, got nil")
	}
}

func TestConfigUnmarshal_EmptyRepositories(t *testing.T) {
	raw := `repositories: []`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repositories) != 0 {
		t.Errorf("expected 0 repositories, got %d", len(cfg.Repositories))
	}
}

func TestConfigUnmarshal_MissingRepositoriesKey(t *testing.T) {
	raw := `{}`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Absent key → zero-value slice; no panic expected downstream.
	if cfg.Repositories != nil {
		t.Errorf("expected nil repositories slice, got %v", cfg.Repositories)
	}
}

// TestConfigUnmarshal_InvalidRepoName verifies that a config entry with an
// invalid name value is accepted by the YAML parser but would be caught by
// parseRepoString at runtime (tested via TestParseRepoString_Invalid).
func TestConfigUnmarshal_InvalidRepoName(t *testing.T) {
	cases := []struct {
		desc string
		raw  string
	}{
		{"empty name", `repositories:\n  - name: ""`},
		{"no slash", `repositories:\n  - name: "no-slash"`},
		{"missing owner", `repositories:\n  - name: "/missing-owner"`},
		{"missing repo", `repositories:\n  - name: "missing-repo/"`},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var cfg Config
			// YAML parsing itself must not fail — the invalid value is a valid string.
			if err := yaml.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Errorf("yaml.Unmarshal should not error on a plain string name, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WorkflowConfig params
// ---------------------------------------------------------------------------

func TestWorkflowConfig_EmptyParams(t *testing.T) {
	raw := `
repositories:
  - name: blinklabs-io/repo
    workflows:
      - destination_file: test.yaml
        workflow_name: "Test"
        reusable_workflow: "blinklabs-io/actions/.github/workflows/reuseable-test-issue-on-close.yml@main"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wf := cfg.Repositories[0].Workflows[0]
	if wf.Params == nil {
		// nil map is safe — template range over nil map produces no output
		return
	}
	if len(wf.Params) != 0 {
		t.Errorf("expected empty params map, got %v", wf.Params)
	}
}
