package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelmPublisherMigrationIsAtomic(t *testing.T) {
	const actionsSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("GITHUB_REPOSITORY", "blinklabs-io/actions")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	legacyPublishers := strings.Fields(`
publish-adder-helm-chart.yml
publish-balius-helm-chart.yml
publish-balius-operator-helm-chart.yml
publish-bind9-helm-chart.yml
publish-bluefin-helm-chart.yml
publish-bursa-helm-chart.yml
publish-butane-oracle-helm-chart.yml
publish-cardano-db-sync-helm-chart.yml
publish-cardano-node-helm-chart.yml
publish-cardano-node-leios-helm-chart.yml
publish-cdnsd-helm-chart.yml
publish-demeter-fabric-helm-chart.yml
publish-dingo-helm-chart.yml
publish-dingo-operator-helm-chart.yml
publish-dolos-helm-chart.yml
publish-ergo-node-helm-chart.yml
publish-ext-blockfrost-operator-helm-chart.yml
publish-ext-cardano-node-operator-helm-chart.yml
publish-ext-kupo-operator-helm-chart.yml
publish-ext-ogmios-operator-helm-chart.yml
publish-ext-proxy-helm-chart.yml
publish-ext-tx-submit-api-operator-helm-chart.yml
publish-ext-utxorpc-operator-helm-chart.yml
publish-handshake-node-helm-chart.yml
publish-hsd-helm-chart.yml
publish-hydrozoa-helm-chart.yml
publish-kupo-helm-chart.yml
publish-ogmios-helm-chart.yml
publish-openvpn-helm-chart.yml
publish-postgres-cluster-helm-chart.yml
publish-rbac-helm-chart.yml
publish-storageclasses-helm-chart.yml
publish-svc-dns-stack-helm-chart.yml
publish-tx-submit-api-helm-chart.yml
publish-tx-submit-api-mirror-helm-chart.yml
publish-vpn-indexer-helm-chart.yml
publish-wireguard-helm-chart.yml
`)
	cfg := loadRealConfig(t)
	if err := resolvePinnedWorkflowRefs(&cfg, func(path string) (string, error) {
		if path != ".github/workflows/reuseable-helm-publish.yml" {
			t.Fatalf("unexpected workflow path %q", path)
		}
		return actionsSHA, nil
	}); err != nil {
		t.Fatal(err)
	}
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
	if len(publisher.Supersedes) != len(legacyPublishers) {
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
	for _, name := range legacyPublishers {
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
			replacementWritten := false
			for _, entry := range tree.Entries {
				if entry.Path == ".github/workflows/"+publisher.DestinationFile {
					replacementWritten = true
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
			for _, name := range legacyPublishers {
				if !deletions[".github/workflows/"+name] {
					t.Errorf("old publisher %s was not removed", name)
				}
			}
			if !replacementWritten {
				t.Fatal("replacement publisher was not staged")
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

func TestHelmPublisherPinsLatestChange(t *testing.T) {
	const actionsSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	t.Setenv("GITHUB_REPOSITORY", "blinklabs-io/actions")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	cfg := loadRealConfig(t)
	if err := resolvePinnedWorkflowRefs(&cfg, func(path string) (string, error) {
		if path != ".github/workflows/reuseable-helm-publish.yml" {
			t.Fatalf("unexpected workflow path %q", path)
		}
		return actionsSHA, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range cfg.Repositories {
		if repo.Name != "blinklabs-io/helm-charts" {
			continue
		}
		for _, wf := range repo.Workflows {
			if wf.DestinationFile != "publish-helm-charts.yml" {
				continue
			}
			want := "blinklabs-io/actions/.github/workflows/reuseable-helm-publish.yml@" + actionsSHA
			if wf.ReusableWorkflow != want {
				t.Fatalf("publisher ref = %q, want %q", wf.ReusableWorkflow, want)
			}
			return
		}
	}
	t.Fatal("Helm publisher workflow not configured")
}

func TestLatestWorkflowChangeIgnoresUnrelatedCommits(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	path := filepath.Join(dir, ".github", "workflows", "reuseable-helm-publish.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("name: publisher\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := func() string {
		git("add", ".")
		git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-s", "-m", "test: update fixture")
		return git("rev-parse", "HEAD")
	}
	want := commit()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit()
	t.Chdir(dir)
	got, err := latestWorkflowChange(".github/workflows/reuseable-helm-publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("publisher revision = %s, want last publisher change %s", got, want)
	}
}

func TestPinnedWorkflowRejectsUnsafeSource(t *testing.T) {
	wf := WorkflowConfig{
		ReusableWorkflow:  "blinklabs-io/actions/.github/workflows/reuseable-helm-publish.yml@main",
		PinToLatestChange: true,
	}
	for _, tc := range []struct {
		name, repository, ref, sha, workflow string
	}{
		{"other repository", "other/actions", "refs/heads/main", strings.Repeat("a", 40), ""},
		{"other branch", "blinklabs-io/actions", "refs/heads/feature", strings.Repeat("a", 40), ""},
		{"short SHA", "blinklabs-io/actions", "refs/heads/main", "abc123", ""},
		{"foreign workflow", "blinklabs-io/actions", "refs/heads/main", strings.Repeat("a", 40), "other/actions/.github/workflows/publish.yml@main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_REPOSITORY", tc.repository)
			t.Setenv("GITHUB_REF", tc.ref)
			testWF := wf
			if tc.workflow != "" {
				testWF.ReusableWorkflow = tc.workflow
			}
			cfg := Config{Repositories: []RepoConfig{{Name: "blinklabs-io/helm-charts", Workflows: []WorkflowConfig{testWF}}}}
			if err := resolvePinnedWorkflowRefs(&cfg, func(string) (string, error) {
				return tc.sha, nil
			}); err == nil {
				t.Fatal("unsafe workflow source was accepted")
			}
		})
	}
}
