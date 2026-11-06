package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func commitCheckScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../.github/workflows/reuseable-conventional-commits.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, step := range workflow.Jobs["conventional-commits"].Steps {
		if step.Name == "Validate pull request commits" && step.Run != "" {
			return step.Run
		}
	}
	t.Fatal("commit validation step not found")
	return ""
}

func gitFixture(t *testing.T, count, invalidAt int, subjects ...string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	git("commit", "--quiet", "--allow-empty", "-m", "chore: initial")
	base := git("rev-parse", "HEAD")
	git("switch", "--quiet", "-c", "feature")
	for i := 0; i < count; i++ {
		subject := "fix: valid commit " + string(rune('a'+i))
		if i < len(subjects) {
			subject = subjects[i]
		}
		if i == invalidAt {
			subject = "invalid ##[add-mask]injected"
		}
		git("commit", "--quiet", "--allow-empty", "-m", subject)
	}
	git("switch", "--quiet", "main")
	git("commit", "--quiet", "--allow-empty", "-m", "chore: advance base")
	git("merge", "--quiet", "--no-ff", "-m", "Merge feature", "feature")
	return dir, git("rev-parse", "HEAD"), base
}

func runCommitCheck(t *testing.T, dir, sha, event string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-c", commitCheckScript(t))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GITHUB_SHA="+sha, "GITHUB_EVENT_NAME="+event)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestConventionalCommitsChecksEveryCommit(t *testing.T) {
	dir, sha, _ := gitFixture(t, 35, -1)
	out, err := runCommitCheck(t, dir, sha, "pull_request")
	if err != nil || !strings.Contains(out, "Validated 35 pull request commits") {
		t.Fatalf("valid commits: %v: %s", err, out)
	}
	dir, sha, _ = gitFixture(t, 35, 34)
	out, err = runCommitCheck(t, dir, sha, "pull_request")
	if err == nil || !strings.Contains(out, "Invalid commit ") || strings.Contains(out, "##[add-mask]") {
		t.Fatalf("invalid 35th commit was not rejected: %v: %s", err, out)
	}
}

func TestConventionalCommitsRejectsMissingHistory(t *testing.T) {
	dir, sha, base := gitFixture(t, 1, -1)
	for _, tc := range []struct{ name, sha, event string }{
		{"missing SHA", "", "pull_request"},
		{"unknown SHA", strings.Repeat("0", 40), "pull_request"},
		{"non-merge commit", base, "pull_request"},
		{"unsupported event", sha, "push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runCommitCheck(t, dir, tc.sha, tc.event)
			if err == nil || !strings.Contains(out, "Unable to validate") {
				t.Fatalf("expected failure: %v: %s", err, out)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(base+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCommitCheck(t, dir, sha, "pull_request")
	if err == nil || !strings.Contains(out, "full commit history is required") {
		t.Fatalf("shallow history was not rejected: %v: %s", err, out)
	}
}

func TestConventionalCommitsPreservesExistingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, subject string
		valid         bool
	}{
		{"scoped breaking change", "feat(scope)!: add feature", true},
		{"merge exception", "Merge branch 'main'", true},
		{"unsupported type", "unknown: change", false},
		{"legacy colonless type", "feat", true},
		{"scope with JS whitespace", "fix(\ufeffscope): change", false},
		{"scope with non JS whitespace", "fix(\u0085scope): change", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, sha, _ := gitFixture(t, 1, -1, tc.subject)
			out, err := runCommitCheck(t, dir, sha, "pull_request")
			if (err == nil) != tc.valid || (!tc.valid && !strings.Contains(out, "Invalid commit ")) {
				t.Fatalf("valid=%v: %v: %s", tc.valid, err, out)
			}
		})
	}
}
