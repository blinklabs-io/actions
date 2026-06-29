package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// newWorkflowTemplate creates a *template.Template with the same FuncMap and
// delimiters used by syncWorkflows, so template-rendering tests match
// production output exactly.
func newWorkflowTemplate(t *testing.T) (*template.Template, error) {
	t.Helper()
	funcMap := template.FuncMap{
		"quoteForYAML": func(s string) string {
			if strings.HasPrefix(strings.TrimSpace(s), "[") {
				return "'" + s + "'"
			}
			if strings.Contains(s, "\n") {
				trimmed := strings.TrimRight(s, "\n")
				lines := strings.Split(trimmed, "\n")
				result := "|-"
				for _, line := range lines {
					result += "\n        " + line
				}
				return result
			}
			return s
		},
	}
	return template.New("workflow.tmpl").Delims("[[", "]]").Funcs(funcMap).ParseFiles("../templates/workflow.tmpl")
}

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
	raw := strings.Join([]string{
		"repositories:",
		"  - name: blinklabs-io/test-repo",
		"    settings:",
		"      delete_branch_on_merge: true",
		"    collaborators:",
		"      - username: alice",
		"        permission: write",
		"    workflows:",
		"      - destination_file: issue-close.yaml",
		"        workflow_name: \"Issue Close\"",
		"        reusable_workflow: \"blinklabs-io/actions/.github/workflows/reuseable-test-issue-on-close.yml@main\"",
		"        secrets:",
		"          project_pat: ORG_PROJECT_PAT",
		"        params:",
		"          issue_number: \"${{ github.event.issue.number }}\"",
		"",
	}, "\n")
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
	if repo.Workflows[0].Secrets["project_pat"] != "ORG_PROJECT_PAT" {
		t.Errorf("workflow secrets not parsed correctly: %v", repo.Workflows[0].Secrets)
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

func TestWorkflowTemplate_ExplicitSecrets(t *testing.T) {
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatalf("unexpected template parse error: %v", err)
	}

	triggersYAML, err := renderTriggers(nil) // default: issues.closed + workflow_dispatch
	if err != nil {
		t.Fatalf("renderTriggers error: %v", err)
	}

	data := templateData{
		WorkflowName:     "Set Project Closed Date",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-set-project-closed-date.yml@main",
		TriggersYAML:     triggersYAML,
		Params: map[string]string{
			"project_url": "https://github.com/orgs/blinklabs-io/projects/11",
		},
		Secrets: map[string]string{
			"project_pat": "ORG_PROJECT_PAT",
		},
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatalf("unexpected template execution error: %v", err)
	}
	out := rendered.String()
	if !strings.Contains(out, "      project_pat: ${{ secrets.ORG_PROJECT_PAT }}") {
		t.Fatalf("rendered workflow missing explicit secret mapping:\n%s", out)
	}
	if strings.Contains(out, "secrets: inherit") {
		t.Fatalf("rendered workflow should not inherit all secrets:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// renderTriggers
// ---------------------------------------------------------------------------

func TestRenderTriggers_Default(t *testing.T) {
	got, err := renderTriggers(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "  issues:") {
		t.Errorf("default triggers missing issues block:\n%s", got)
	}
	if !strings.Contains(got, "  workflow_dispatch:") {
		t.Errorf("default triggers missing workflow_dispatch:\n%s", got)
	}
	// nil values must not appear as "null"
	if strings.Contains(got, "null") {
		t.Errorf("triggers YAML must not contain 'null':\n%s", got)
	}
}

func TestRenderTriggers_PullRequestPush(t *testing.T) {
	triggers := map[string]interface{}{
		"pull_request": nil,
		"push": map[string]interface{}{
			"branches": []interface{}{"main"},
			"tags":     []interface{}{"v*"},
		},
	}
	got, err := renderTriggers(triggers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "  pull_request:") {
		t.Errorf("missing pull_request trigger:\n%s", got)
	}
	if !strings.Contains(got, "  push:") {
		t.Errorf("missing push trigger:\n%s", got)
	}
	if strings.Contains(got, "null") {
		t.Errorf("triggers YAML must not contain 'null':\n%s", got)
	}
}

func TestWorkflowTemplate_ConfigurableTriggers(t *testing.T) {
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatalf("unexpected template parse error: %v", err)
	}

	triggersYAML, err := renderTriggers(map[string]interface{}{
		"pull_request": nil,
		"push": map[string]interface{}{
			"branches": []interface{}{"main"},
			"tags":     []interface{}{"v*"},
		},
	})
	if err != nil {
		t.Fatalf("renderTriggers error: %v", err)
	}

	data := templateData{
		WorkflowName:     "go-test",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
		TriggersYAML:     triggersYAML,
		Permissions: map[string]string{
			"contents": "read",
		},
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatalf("unexpected template execution error: %v", err)
	}
	out := rendered.String()
	if !strings.Contains(out, "  pull_request:") {
		t.Fatalf("rendered workflow missing pull_request trigger:\n%s", out)
	}
	if !strings.Contains(out, "  push:") {
		t.Fatalf("rendered workflow missing push trigger:\n%s", out)
	}
	// with: block must not appear when Params is nil
	if strings.Contains(out, "    with:") {
		t.Fatalf("rendered workflow should not have 'with:' when params are empty:\n%s", out)
	}
	// permissions block must appear when Permissions is set
	if !strings.Contains(out, "permissions:") {
		t.Fatalf("rendered workflow missing permissions block:\n%s", out)
	}
	if !strings.Contains(out, "  contents: read") {
		t.Fatalf("rendered workflow missing permissions entry:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// docker-openvpn: schedule trigger + enable-trivy-scan
// ---------------------------------------------------------------------------

// TestRenderTriggers_WithSchedule verifies that a schedule cron entry is
// correctly rendered inside the on: block without "null" and with proper
// indentation.
func TestRenderTriggers_WithSchedule(t *testing.T) {
	triggers := map[string]interface{}{
		"push": map[string]interface{}{
			"branches": []interface{}{"main"},
			"tags":     []interface{}{"v*.*.*"},
		},
		"schedule": []interface{}{
			map[string]interface{}{"cron": "0 0 * * 1"},
		},
	}
	got, err := renderTriggers(triggers)
	if err != nil {
		t.Fatalf("renderTriggers error: %v", err)
	}
	if !strings.Contains(got, "  schedule:") {
		t.Errorf("missing schedule key in triggers output:\n%s", got)
	}
	if !strings.Contains(got, "cron:") {
		t.Errorf("missing cron key in triggers output:\n%s", got)
	}
	if !strings.Contains(got, "0 0 * * 1") {
		t.Errorf("missing cron value in triggers output:\n%s", got)
	}
	if strings.Contains(got, "null") {
		t.Errorf("triggers YAML must not contain 'null':\n%s", got)
	}
}

// TestWorkflowTemplate_OpenvpnPublish verifies the full rendered publish
// wrapper for docker-openvpn: schedule trigger, security-events permission,
// enable-trivy-scan param, and docker-image/ghcr-image params.
func TestWorkflowTemplate_OpenvpnPublish(t *testing.T) {
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatalf("unexpected template parse error: %v", err)
	}

	triggersYAML, err := renderTriggers(map[string]interface{}{
		"push": map[string]interface{}{
			"branches": []interface{}{"main"},
			"tags":     []interface{}{"v*.*.*"},
		},
		"schedule": []interface{}{
			map[string]interface{}{"cron": "0 0 * * 1"},
		},
	})
	if err != nil {
		t.Fatalf("renderTriggers error: %v", err)
	}

	data := templateData{
		WorkflowName:     "publish",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-publish-docker.yml@main",
		TriggersYAML:     triggersYAML,
		Permissions: map[string]string{
			"contents":        "write",
			"packages":        "write",
			"security-events": "write",
		},
		Secrets: map[string]string{
			"docker-password": "DOCKER_PASSWORD",
		},
		Params: map[string]string{
			"docker-image":      "blinklabs/openvpn",
			"ghcr-image":        "blinklabs-io/openvpn",
			"description":       "Simple OpenVPN image",
			"enable-trivy-scan": "true",
		},
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatalf("unexpected template execution error: %v", err)
	}
	out := rendered.String()

	checks := []struct {
		desc    string
		contain string
	}{
		{"schedule trigger", "schedule:"},
		{"cron value", "0 0 * * 1"},
		{"push trigger", "push:"},
		{"security-events permission", "security-events: write"},
		{"docker-image param", "docker-image: blinklabs/openvpn"},
		{"ghcr-image param", "ghcr-image: blinklabs-io/openvpn"},
		{"description param", "description: Simple OpenVPN image"},
		{"enable-trivy-scan param", "enable-trivy-scan: true"},
		{"docker-password secret", "docker-password: ${{ secrets.DOCKER_PASSWORD }}"},
		{"governance header", "# Generated automatically by org-governance-bot"},
		{"reuseable ref", "reuseable-publish-docker.yml@main"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.contain) {
			t.Errorf("rendered workflow missing %s (%q):\n%s", c.desc, c.contain, out)
		}
	}
	// enable-trivy-scan must NOT be wrapped in single quotes (it's a plain bool string)
	if strings.Contains(out, "'true'") {
		t.Errorf("enable-trivy-scan should not be single-quoted:\n%s", out)
	}
}
