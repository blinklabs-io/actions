package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

func TestReconcilePrivateRepositoryReportsCountsOnly(t *testing.T) {
	t.Chdir("..")
	fake := &recordingGitHub{contents: map[string]string{}}
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/repos/o/private-repo" {
			r := httptest.NewRecorder()
			r.Header().Set("Content-Type", "application/json")
			if req.Method == http.MethodGet {
				_, _ = io.WriteString(r, `{"private":true,"default_branch":"main","node_id":"repo-id","delete_branch_on_merge":false}`)
			} else {
				writes++
				_, _ = io.WriteString(r, `{"private":true,"delete_branch_on_merge":true}`)
			}
			return r.Result(), nil
		}
		r := httptest.NewRecorder()
		fake.handler().ServeHTTP(r, req)
		return r.Result(), nil
	})})
	var output strings.Builder
	report := newDriftReport(&output)
	repos := []RepoConfig{{Name: "o/private-repo", Settings: RepoSettings{DeleteBranchOnMerge: github.Bool(true)}}}
	if err := reconcileRepositories(context.Background(), client, repos, report); err != nil {
		t.Fatal(err)
	}
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("got %d settings writes, want 1", writes)
	}
	got := output.String()
	if strings.Contains(got, "private-repo") || strings.Contains(got, "delete_branch_on_merge") {
		t.Fatalf("private details exposed: %s", got)
	}
	if !strings.Contains(got, "1 private or unknown") || !strings.Contains(got, "1 corrected") {
		t.Fatalf("private counts missing: %s", got)
	}
}

func TestUnknownVisibilityIsNotModified(t *testing.T) {
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		if req.Method != http.MethodGet {
			writes++
		}
		r.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(r, `{"default_branch":"main"}`)
		return r.Result(), nil
	})})
	var output strings.Builder
	report := newDriftReport(&output)
	if err := reconcileRepositories(context.Background(), client, []RepoConfig{{Name: "o/unknown"}}, report); err == nil {
		t.Fatal("missing visibility should fail")
	}
	if writes != 0 {
		t.Fatalf("got %d writes with unknown visibility", writes)
	}
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "o/unknown") || !strings.Contains(output.String(), "1 unchecked") {
		t.Fatalf("unexpected report: %s", output.String())
	}
}

func TestSettingReportPrecedesUpdate(t *testing.T) {
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("example/public", true)
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(r, `{"delete_branch_on_merge":false}`)
		} else {
			writes++
			if got := output.String(); !strings.Contains(got, "- false\n+ true") {
				t.Errorf("update occurred before diff was recorded: %s", got)
			}
			_, _ = io.WriteString(r, `{}`)
		}
		return r.Result(), nil
	})})
	if err := reconcileRepoSettings(context.Background(), client, "example", "public", RepoSettings{DeleteBranchOnMerge: github.Bool(true)}, report); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("got %d writes, want 1", writes)
	}
}

func TestSettingReportFailurePreventsUpdate(t *testing.T) {
	writes := 0
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		r.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(r, `{"delete_branch_on_merge":false}`)
		} else {
			writes++
		}
		return r.Result(), nil
	})})
	report := newDriftReport(driftFailWriter{err: errReportUnavailable})
	report.beginRepo("example/public", true)
	if err := reconcileRepoSettings(context.Background(), client, "example", "public", RepoSettings{DeleteBranchOnMerge: github.Bool(true)}, report); err == nil {
		t.Fatal("expected report failure")
	}
	if writes != 0 {
		t.Fatalf("got %d writes after report failure", writes)
	}
}
