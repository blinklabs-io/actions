package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v60/github"
)

// recordingGitHub is a minimal stand-in for the Contents API: it answers the
// calls syncWorkflows makes and records every request, so a test can assert on
// what the bot did rather than on what it logged.
type recordingGitHub struct {
	mu sync.Mutex
	// writeStatus is returned for the PUT that creates or updates a file.
	writeStatus int
	requests    []string
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

func (g *recordingGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.record(r.Method, r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			// The pipeline file is absent so it gets created, and the wrappers
			// it supersedes are present so removing them is a real DELETE.
			// Without both, a test asserting "nothing was deleted" passes
			// whether or not the guard exists.
			if strings.HasSuffix(r.URL.Path, "/ci.yml") {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"file","name":"wrapper.yml","sha":"deadbeef","content":"","encoding":"base64"}`))
		case r.Method == http.MethodPut:
			if g.writeStatus != 0 && g.writeStatus != http.StatusOK {
				http.Error(w, `{"message":"boom"}`, g.writeStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":{"sha":"abc"}}`))
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
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
// syncWorkflows writes the pipeline and then deletes the wrappers it replaced.
// If the write fails and the delete runs anyway, the repository is left with no
// CI at all: the wrappers are gone and the pipeline that was meant to replace
// them was never created. The wrappers are still correct at that point, so the
// right move is to leave them and retry on the next reconciliation.
func TestSyncWorkflowsKeepsWrappersWhenPipelineWriteFails(t *testing.T) {
	// syncWorkflows loads templates by a path relative to the repository root.
	t.Chdir("..")

	g := &recordingGitHub{writeStatus: http.StatusInternalServerError}
	client := newRecordingClient(t, g)

	syncWorkflows(context.Background(), client, "o", "r", goPipeline())

	if !g.saw(http.MethodPut, "ci.yml") {
		t.Fatalf("expected an attempt to write ci.yml; requests were %v", g.requests)
	}
	if g.saw(http.MethodDelete, "") {
		t.Errorf(
			"deleted a superseded wrapper after the pipeline write failed; "+
				"requests were %v",
			g.requests,
		)
	}
}

// TestSyncWorkflowsRemovesWrappersAfterSuccessfulWrite is the other half: the
// removal has to happen when the write succeeds, or a repository runs the
// pipeline and every wrapper it replaced.
func TestSyncWorkflowsRemovesWrappersAfterSuccessfulWrite(t *testing.T) {
	t.Chdir("..")

	g := &recordingGitHub{writeStatus: http.StatusOK}
	client := newRecordingClient(t, g)

	syncWorkflows(context.Background(), client, "o", "r", goPipeline())

	if !g.saw(http.MethodPut, "ci.yml") {
		t.Fatalf("expected ci.yml to be written; requests were %v", g.requests)
	}
	for _, wrapper := range []string{
		"golangci-lint.yml", "nilaway.yml", "go-test.yml", "ci-docker.yml",
	} {
		if !g.saw(http.MethodDelete, wrapper) {
			t.Errorf(
				"superseded wrapper %s was not deleted; it would keep starting "+
					"its own run alongside the pipeline. Requests were %v",
				wrapper,
				g.requests,
			)
		}
	}
}
