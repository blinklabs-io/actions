package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDriftReportRedactsNonPublicEntries(t *testing.T) {
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("private-repo", false)
	entry := report.change("hidden-kind", "hidden-path", "hidden-old", "hidden-new")
	entry.status = "failed"
	report.beginRepo("unknown-repo", false)
	report.beginRepo("example/public", true)
	report.change("setting", "enabled", "false", "true")
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, hidden := range []string{"private-repo", "unknown-repo", "hidden-"} {
		if strings.Contains(got, hidden) {
			t.Errorf("report exposed %q", hidden)
		}
	}
	for _, want := range []string{"Checked 3 repository entries; 2 private or unknown.", "- failed: 1", "example/public", "[pending]"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q: %s", want, got)
		}
	}
}

func TestDriftReportFencesPublicDiffAndShowsStatus(t *testing.T) {
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("example/public", true)
	report.change("setting", "first\n~~~", "old\n~~~~", "new\n`more`").status = "corrected"
	report.change("setting", "second", "old", "new").status = "failed"
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"~~~~~diff\n", "~~~~~\n", "first�~~~ [corrected]", "- old\n- ~~~~\n+ new\n+ `more`\n", "second [failed]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q: %s", want, got)
		}
	}
}

type driftFailWriter struct{ err error }

func (w driftFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDriftReportReturnsWriteError(t *testing.T) {
	want := errors.New("report sink unavailable")
	report := newDriftReport(driftFailWriter{err: want})
	if err := report.render(); !errors.Is(err, want) {
		t.Fatalf("render error = %v, want %v", err, want)
	}
}

func TestDriftReportEscapesRunnerCommands(t *testing.T) {
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("example/public", true)
	entry := report.change("setting", "##[warning]", "old", "##[error]unsafe")
	if err := report.checkpoint(entry); err != nil {
		t.Fatal(err)
	}
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "##[") {
		t.Fatalf("report contains a runner command: %s", output.String())
	}
}

func TestDriftReportDiagnosticsArePublicOnly(t *testing.T) {
	var output strings.Builder
	report := newDriftReport(&output)
	report.beginRepo("hidden", false)
	report.change("setting", "target", "", "").status = "failed"
	report.diagnostic("private diagnostic")
	report.beginRepo("example/public", true)
	report.change("setting", "target", "", "").status = "failed"
	report.diagnostic("public diagnostic")
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private diagnostic") || !strings.Contains(output.String(), "public diagnostic") {
		t.Fatalf("unexpected diagnostics: %s", output.String())
	}
}
