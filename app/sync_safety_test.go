package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v60/github"
)

// recordingGitHub is a minimal stand-in for the two APIs a sync touches: the
// Contents API it reads current file state from, and the Git Data API it writes
// the aggregated commit with. It records every request and captures the tree
// body, so a test can assert on what the bot committed rather than what it
// logged.
type recordingGitHub struct {
	mu sync.Mutex
	// failAt names the Git Data step that returns 500: "tree", "commit", or
	// "ref". Empty means the whole write succeeds.
	failAt   string
	requests []string
	// treeBody is the raw JSON sent to POST /git/trees, so a test can assert the
	// aggregated commit both writes the pipeline and deletes the wrappers.
	treeBody string
}

func (g *recordingGitHub) record(method, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, method+" "+path)
}

func (g *recordingGitHub) saw(method, substr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.requests {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func (g *recordingGitHub) count(method, substr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func (g *recordingGitHub) tree() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.treeBody
}

func (g *recordingGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.record(r.Method, r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			// The pipeline file is absent so it gets created, and the wrappers
			// it supersedes are present so removing them is a real deletion.
			// Without both, a test asserting the commit does both would pass
			// whether or not the code does.
			if strings.HasSuffix(r.URL.Path, "/ci.yml") {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"file","name":"wrapper.yml","sha":"deadbeef","content":"","encoding":"base64"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"type":"commit","sha":"basecommit"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/commits/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"basecommit","tree":{"sha":"basetree"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/trees"):
			body, _ := io.ReadAll(r.Body)
			g.mu.Lock()
			g.treeBody = string(body)
			g.mu.Unlock()
			if g.failAt == "tree" {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"newtree"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/commits"):
			if g.failAt == "commit" {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"newcommit"}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/git/refs/"):
			if g.failAt == "ref" {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"newcommit"}}`))
		default:
			http.Error(w, `{"message":"unexpected"}`, http.StatusInternalServerError)
		}
	})
}

func newRecordingClient(t *testing.T, g *recordingGitHub) *github.Client {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := github.NewClient(nil)
	client.BaseURL = base
	return client
}

// TestSyncWorkflowsKeepsWrappersWhenPipelineWriteFails is the ordering
// invariant that matters most in this change.
//
// The pipeline write and the removal of the wrappers it replaces go in one
// commit. If that commit cannot be created, the branch must not move: otherwise
// the wrappers would be gone while the pipeline meant to replace them was never
// written, leaving the repository with no CI at all. Failing the tree creation
// stands in for any write failure; the ref update must never run.
func TestSyncWorkflowsKeepsWrappersWhenPipelineWriteFails(t *testing.T) {
	// syncWorkflows loads templates by a path relative to the repository root.
	t.Chdir("..")

	g := &recordingGitHub{failAt: "tree"}
	client := newRecordingClient(t, g)

	syncWorkflows(context.Background(), client, "o", "r", goPipeline())

	if !g.saw(http.MethodPost, "/git/trees") {
		t.Fatalf("expected an attempt to build the commit tree; requests were %v", g.requests)
	}
	// The tree the bot tried to write both creates the pipeline and deletes the
	// wrappers, so the two can never be separated across commits.
	if !strings.Contains(g.tree(), "ci.yml") {
		t.Errorf("staged tree did not write the pipeline ci.yml; tree was %s", g.tree())
	}
	// The write failed, so the branch must not have advanced.
	if g.saw(http.MethodPatch, "/git/refs/") {
		t.Errorf(
			"advanced the branch after the commit write failed; the wrappers "+
				"would be deleted with no pipeline to replace them. Requests were %v",
			g.requests,
		)
	}
}

// TestSyncWorkflowsRemovesWrappersAfterSuccessfulWrite is the other half: a
// successful run writes the pipeline and deletes every wrapper it replaced, and
// does so in a single commit so it starts one CI run rather than one per file.
func TestSyncWorkflowsRemovesWrappersAfterSuccessfulWrite(t *testing.T) {
	t.Chdir("..")

	g := &recordingGitHub{}
	client := newRecordingClient(t, g)

	syncWorkflows(context.Background(), client, "o", "r", goPipeline())

	// Aggregation: many file changes, exactly one commit and one branch update.
	if got := g.count(http.MethodPost, "/git/commits"); got != 1 {
		t.Fatalf("expected exactly one commit for all changes, got %d; requests were %v", got, g.requests)
	}
	if got := g.count(http.MethodPatch, "/git/refs/"); got != 1 {
		t.Fatalf("expected exactly one branch update, got %d; requests were %v", got, g.requests)
	}

	tree := g.tree()
	if !strings.Contains(tree, "ci.yml") {
		t.Errorf("commit tree did not write the pipeline ci.yml; tree was %s", tree)
	}
	// A deletion is a tree entry with "sha":null. Its presence proves the
	// wrappers left with the same commit that wrote the pipeline.
	if !strings.Contains(tree, `"sha":null`) {
		t.Errorf("commit tree deleted no wrappers; tree was %s", tree)
	}
	for _, wrapper := range []string{
		"golangci-lint.yml", "nilaway.yml", "go-test.yml", "ci-docker.yml",
	} {
		if !strings.Contains(tree, wrapper) {
			t.Errorf(
				"superseded wrapper %s was not deleted in the commit tree; it "+
					"would keep starting its own run alongside the pipeline. Tree was %s",
				wrapper,
				tree,
			)
		}
	}
}
