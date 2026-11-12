package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

var errReportUnavailable = errors.New("report unavailable")

func TestManagedFileDriftAndResults(t *testing.T) {
	t.Chdir("..")
	wf := WorkflowConfig{
		DestinationFile:  "conventional-commits.yml",
		WorkflowName:     "Conventional Commits",
		ReusableWorkflow: "example/action.yml@main",
		Triggers:         map[string]interface{}{"pull_request": nil},
	}
	for _, tt := range []struct {
		name, old, failAt, want string
		readError               int
		wantErr                 bool
	}{
		{"corrected", "old content", "", "corrected", 0, false},
		{"update failed", "old content", "tree", "failed", 0, true},
		{"read failed", "", "", "unchecked", 500, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &recordingGitHub{contents: map[string]string{"conventional-commits.yml": tt.old}, failAt: tt.failAt}
			if tt.readError != 0 {
				fake.contentsErrors = map[string]int{"conventional-commits.yml": tt.readError}
			}
			client := newRecordingClient(t, fake)
			var output strings.Builder
			report := newDriftReport(&output)
			report.beginRepo("example/public", true)
			ctx := context.WithValue(context.Background(), driftContextKey{}, report)
			err := syncWorkflows(ctx, client, "o", "r", []WorkflowConfig{wf})
			if (err != nil) != tt.wantErr {
				t.Fatalf("sync error = %v", err)
			}
			if err := report.render(); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); !strings.Contains(got, "["+tt.want+"]") {
				t.Fatalf("missing %s status: %s", tt.want, got)
			}
			if tt.wantErr && fake.saw(http.MethodPost, "/graphql") {
				t.Fatal("ref updated despite failure")
			}
			if tt.want == "corrected" && (!strings.Contains(output.String(), "- sha256:") || !strings.Contains(output.String(), "+ sha256:") || !strings.Contains(output.String(), "commit: newcommit")) {
				t.Fatalf("report omitted hashes or commit: %s", output.String())
			}
		})
	}
}

func TestManagedFileReportFailurePreventsWrite(t *testing.T) {
	t.Chdir("..")
	fake := &recordingGitHub{contents: map[string]string{"conventional-commits.yml": "old"}}
	report := newDriftReport(driftFailWriter{err: errReportUnavailable})
	report.beginRepo("example/public", true)
	ctx := context.WithValue(context.Background(), driftContextKey{}, report)
	wf := WorkflowConfig{DestinationFile: "conventional-commits.yml", WorkflowName: "Conventional Commits", ReusableWorkflow: "example/action.yml@main", Triggers: map[string]interface{}{"pull_request": nil}}
	if err := syncWorkflows(ctx, newRecordingClient(t, fake), "o", "r", []WorkflowConfig{wf}); err == nil {
		t.Fatal("expected report failure")
	}
	if fake.saw(http.MethodPost, "/git/trees") || fake.saw(http.MethodPost, "/graphql") {
		t.Fatal("repository changed despite report failure")
	}
}
