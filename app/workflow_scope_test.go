package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

const scopeFixture = `release_fanout_dispatch_ref: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
repositories:
  - name: o/r
    workflows:
      - destination_file: check.yml
        workflow_name: checks
        reusable_workflow: blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main
        triggers:
          pull_request:
        permissions:
          contents: read
        params:
          run-build: "true"
  - name: o/unchanged
    workflows:
      - destination_file: check.yml
        reusable_workflow: other/ci/.github/workflows/check.yml@main
`

func TestWorkflowSourceChangesPreservesFullGovernance(t *testing.T) {
	public := strings.Replace(scopeFixture, "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main", sharedWorkflowRepository+".github/workflows/reuseable-go-test.yml@"+canarySharedRef, 1)
	for _, tc := range []struct {
		name, next string
		scoped     bool
	}{
		{"source only", public, true},
		{"unchanged", scopeFixture, false},
		{"input changed", strings.Replace(public, "run-build: \"true\"", "run-build: \"false\"", 1), false},
		{"permission changed", strings.Replace(public, "contents: read", "contents: write", 1), false},
		{"settings changed", strings.Replace(public, "  - name: o/r", "  - name: o/r\n    settings:\n      delete_branch_on_merge: true", 1), false},
		{"archival changed", strings.Replace(public, "  - name: o/r", "  - name: o/r\n    archive_on_removal: true", 1), false},
		{"repository removed", public[:strings.Index(public, "  - name: o/unchanged")], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, scoped, err := workflowSourceChanges([]byte(scopeFixture), []byte(tc.next))
			if err != nil {
				t.Fatal(err)
			}
			if scoped != tc.scoped {
				t.Fatalf("workflow-only = %v, want %v", scoped, tc.scoped)
			}
			if scoped && !reflect.DeepEqual(names, []string{"o/r"}) {
				t.Fatalf("targets=%v, want only o/r", names)
			}
		})
	}
}

func TestWorkflowSyncScopeUsesTrustedEvent(t *testing.T) {
	for _, tc := range []struct {
		event, target, previous string
		wantErr, scoped         bool
	}{
		{event: "schedule"}, {event: "workflow_dispatch"},
		{event: "workflow_dispatch", target: "o/r", scoped: true},
		{event: "push", target: "o/r", wantErr: true},
		{event: "push", previous: "main", wantErr: true},
		{event: "push", previous: strings.Repeat("0", 40)},
	} {
		t.Run(tc.event+tc.target+tc.previous, func(t *testing.T) {
			_, scoped, err := workflowSyncScope(tc.event, tc.target, tc.previous, canarySharedRef, []byte(scopeFixture), func(string) ([]byte, error) { t.Fatal("unexpected history read"); return nil, nil })
			if (err != nil) != tc.wantErr || scoped != tc.scoped {
				t.Fatalf("scope=%v err=%v", scoped, err)
			}
		})
	}
	_, _, err := workflowSyncScope("push", "", strings.Repeat("c", 40), canarySharedRef, []byte(scopeFixture), func(string) ([]byte, error) { return nil, errors.New("missing history") })
	if err == nil {
		t.Fatal("history lookup failure accepted")
	}
}

func TestSelectedWorkflowSyncOnlyWritesSelectedFiles(t *testing.T) {
	t.Chdir("..")
	g := &recordingGitHub{contents: map[string]string{}}
	handler := g.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r" {
			g.record(r.Method, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "node_id": "repo-id", "full_name": "o/r", "default_branch": "main", "private": false, "visibility": "public"})
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	cfg := Config{Repositories: []RepoConfig{
		{Name: "o/r", Workflows: []WorkflowConfig{{DestinationFile: "check.yml", WorkflowName: "checks", ReusableWorkflow: sharedWorkflowRepository + ".github/workflows/reuseable-go-test.yml@" + canarySharedRef, Triggers: map[string]interface{}{"pull_request": nil}}}},
		{Name: "o/unchanged", Settings: RepoSettings{DeleteBranchOnMerge: github.Bool(true)}},
	}}
	checked := 0
	err := syncSelectedWorkflows(context.Background(), client, cfg, []string{"o/r"}, func(string, string) ([]byte, error) { return []byte(callableWorkflowYAML), nil }, func() error { checked++; return nil }, newDriftReport(io.Discard))
	for _, request := range g.requests {
		if strings.Contains(request, "unchanged") || strings.Contains(request, "governance-applied-state") || strings.HasPrefix(request, "PATCH ") || strings.HasPrefix(request, "PUT ") {
			t.Fatalf("out-of-scope request: %s", request)
		}
	}
	if checked != 2 || !g.saw(http.MethodPost, "/git/trees") || !g.saw(http.MethodPost, "/graphql") {
		t.Fatalf("target was not committed: %v", g.requests)
	}
	if err != nil {
		t.Fatalf("target workflow sync failed: %v; requests=%v", err, g.requests)
	}
	if !strings.Contains(g.tree(), "shared-actions/") || !strings.Contains(g.tree(), ".github/workflows/check.yml") {
		t.Fatal("configured public caller was not generated")
	}
}

func TestSelectedWorkflowSyncRejectsInvalidTargetBeforeWrites(t *testing.T) {
	cfg := Config{Repositories: []RepoConfig{{Name: "o/r"}}}
	for _, names := range [][]string{{"o/missing"}, {"o/r", "o/r"}, nil} {
		err := syncSelectedWorkflows(context.Background(), nil, cfg, names, nil, func() error { t.Fatal("invalid target reached reconciliation"); return nil }, newDriftReport(io.Discard))
		if err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	cfg.Repositories[0].Workflows = []WorkflowConfig{{ReusableWorkflow: sharedWorkflowRepository + ".github/workflows/missing.yml@" + canarySharedRef}}
	err := syncSelectedWorkflows(context.Background(), nil, cfg, []string{"o/r"}, func(string, string) ([]byte, error) { return nil, errors.New("missing public source") }, func() error { t.Fatal("invalid source reached reconciliation"); return nil }, newDriftReport(io.Discard))
	if err == nil {
		t.Fatal("invalid source accepted")
	}
	cfg.Repositories[0].Workflows = nil
	err = syncSelectedWorkflows(context.Background(), nil, cfg, []string{"o/r"}, nil, func() error { return errors.New("controller main moved") }, newDriftReport(io.Discard))
	if err == nil {
		t.Fatal("moved controller main accepted")
	}
}

func TestWorkflowScopeRetainsRenderInputsAndRecovery(t *testing.T) {
	for _, path := range []string{".github/workflows/reuseable-helm-publish.yml", "templates/workflow.tmpl", "templates/pipeline.tmpl"} {
		if !workflowRenderInputsChanged([]string{"repos-config.yaml", path}) {
			t.Fatalf("render input %s would be skipped", path)
		}
	}
	if workflowRenderInputsChanged([]string{"repos-config.yaml", "app/workflow_scope.go"}) {
		t.Fatal("scope implementation changes are not render inputs")
	}
	for _, sentinel := range []error{errWorkflowInputsChanged, errPreviousConfigUnavailable} {
		_, scoped, err := workflowSyncScope("push", "", strings.Repeat("c", 40), canarySharedRef, []byte(scopeFixture), func(string) ([]byte, error) { return nil, sentinel })
		if err != nil || scoped {
			t.Fatalf("normal governance not retained: scoped=%v err=%v", scoped, err)
		}
	}
	_, scoped, err := workflowSyncScope("push", "", strings.Repeat("c", 40), canarySharedRef, []byte(scopeFixture), func(string) ([]byte, error) { return []byte("repositories: ["), nil })
	if err != nil || scoped {
		t.Fatalf("configuration recovery blocked: scoped=%v err=%v", scoped, err)
	}
}

func TestTargetedGovernanceReportWritesSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_STEP_SUMMARY", path)
	report, summary, err := newGovernanceReport(canarySharedRef)
	if err != nil {
		t.Fatal(err)
	}
	report.beginRepo("o/r", true)
	report.change("managed file", "check.yml", "old", "new").status = "corrected"
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	if err := summary.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "o/r") || !strings.Contains(string(data), "check.yml") {
		t.Fatal("targeted drift report missing from job summary")
	}
}

func TestSelectedWorkflowSyncStopsWhenControllerMainMoves(t *testing.T) {
	t.Chdir("..")
	g := &recordingGitHub{contents: map[string]string{}}
	handler := g.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r" {
			g.record(r.Method, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "node_id": "repo-id", "full_name": "o/r", "default_branch": "main", "private": false, "visibility": "public"})
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	cfg := Config{Repositories: []RepoConfig{{Name: "o/r", Workflows: []WorkflowConfig{{DestinationFile: "check.yml", WorkflowName: "checks", ReusableWorkflow: "other/ci/.github/workflows/check.yml@main", Triggers: map[string]interface{}{"pull_request": nil}}}}}}
	checks := 0
	err := syncSelectedWorkflows(context.Background(), client, cfg, []string{"o/r"}, nil, func() error {
		checks++
		if checks == 1 {
			return nil
		}
		if !g.saw(http.MethodPost, "/git/commits") {
			t.Fatal("final controller check ran before the staged commit")
		}
		return errors.New("controller main moved")
	}, newDriftReport(io.Discard))
	if err == nil {
		t.Fatal("stale controller published managed files")
	}
	if checks != 2 {
		t.Fatalf("controller checks=%d, want two", checks)
	}
	if g.saw(http.MethodPost, "/graphql") {
		t.Fatal("target main updated after controller main moved")
	}
}
