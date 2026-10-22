package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type reusableWorkflowFile struct {
	Jobs map[string]struct {
		Needs    []string `yaml:"needs"`
		Strategy struct {
			MaxParallel string `yaml:"max-parallel"`
		} `yaml:"strategy"`
		Steps []struct {
			Name string            `yaml:"name"`
			Run  string            `yaml:"run"`
			Uses string            `yaml:"uses"`
			Env  map[string]string `yaml:"env"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readReusableWorkflow(t *testing.T, path string) reusableWorkflowFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read workflow %s: %v", path, err)
	}
	var workflow reusableWorkflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow %s: %v", path, err)
	}
	return workflow
}

func TestReusableGoTestWorkflowUsesMatrixArchitectureAndEffectiveGuards(t *testing.T) {
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-go-test.yml")
	job := workflow.Jobs["go-test"]
	steps := make(map[string]struct {
		Run string            `yaml:"run"`
		Env map[string]string `yaml:"env"`
	})
	for _, step := range job.Steps {
		steps[step.Name] = struct {
			Run string            `yaml:"run"`
			Env map[string]string `yaml:"env"`
		}{Run: step.Run, Env: step.Env}
	}
	for _, name := range []string{"go-build", "go-test", "go-test-extra", "go-test-race"} {
		if got := steps[name].Env["GOARCH"]; got != "${{ matrix.go-arch }}" {
			t.Errorf("%s GOARCH = %q, want matrix.go-arch", name, got)
		}
	}
	for _, name := range []string{"go-vet", "Test additional Go modules", "Run NilAway", "golangci-lint (additional modules)"} {
		if got := steps[name].Env["GOARCH"]; got != "${{ matrix.go-arch }}" {
			t.Errorf("%s GOARCH = %q, want matrix.go-arch", name, got)
		}
	}
	if _, ok := steps["Install NilAway"].Env["GOARCH"]; ok {
		t.Error("Install NilAway must build a runner-native binary")
	}
	for _, name := range []string{"golangci-lint", "golangci-lint (additional checks)"} {
		if got := steps[name].Env["GOARCH"]; got != "${{ matrix.go-arch }}" {
			t.Errorf("%s GOARCH = %q, want matrix.go-arch", name, got)
		}
	}
	formatRun := steps["Check formatting"].Run
	if !strings.Contains(formatRun, "_diff=\"$(golangci-lint fmt --diff 2>&1)\"") ||
		!strings.Contains(formatRun, "_format_status") || strings.Contains(formatRun, "| grep") {
		t.Errorf("format check does not preserve tool failures and diff output:\n%s", formatRun)
	}
	if run := steps["Buf format check"].Run; !strings.Contains(run, "buf format -d --exit-code") {
		t.Errorf("Buf format check does not fail on formatting differences:\n%s", run)
	}
	if run := steps["Verify generated code is up to date"].Run; !strings.Contains(run, "git diff --exit-code") || !strings.Contains(run, "git ls-files --others --exclude-standard") {
		t.Errorf("generated-code check ignores untracked files:\n%s", run)
	}
	if run := steps["Check go.mod is tidy"].Run; !strings.Contains(run, "git diff --exit-code -- go.mod go.sum") || !strings.Contains(run, "git ls-files --others --exclude-standard -- go.mod go.sum") {
		t.Errorf("module tidy check ignores untracked go.sum files:\n%s", run)
	}

	crossBuild := workflow.Jobs["cross-build"]
	if len(crossBuild.Needs) != 1 || crossBuild.Needs[0] != "go-test" {
		t.Errorf("cross-build needs = %v, want [go-test]", crossBuild.Needs)
	}
	if got := crossBuild.Strategy.MaxParallel; got != "" {
		t.Errorf("cross-build max-parallel = %q, want no matrix: every target builds in one job", got)
	}
	if got, want := job.Strategy.MaxParallel, "${{ fromJSON(inputs.max-parallel) }}"; got != want {
		t.Errorf("go-test max-parallel = %q, want %q", got, want)
	}
	if len(crossBuild.Steps) == 0 {
		t.Fatal("cross-build job has no steps")
	}
	if got := crossBuild.Steps[0].With["submodules"]; got != "${{ inputs.submodules }}" {
		t.Errorf("cross-build checkout submodules = %q, want inputs.submodules", got)
	}
	buildRun := crossBuild.Steps[len(crossBuild.Steps)-1].Run
	if !strings.Contains(buildRun, "GOOS=\"$TARGET_OS\" GOARCH=\"$TARGET_ARCH\" go build \"${_flags[@]}\" $_packages") ||
		!strings.Contains(buildRun, "done <<< \"$_targets\"") ||
		strings.Contains(buildRun, "-o /dev/null") {
		t.Errorf("cross-build does not build all packages for the selected target:\n%s", buildRun)
	}
}

func TestReusableGoTestCoverageUsesWorkingDirectory(t *testing.T) {
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-go-test.yml")
	for _, step := range workflow.Jobs["go-test"].Steps {
		if step.Name != "Upload coverage to Codecov" {
			continue
		}
		if got, want := step.With["files"], "${{ inputs.working-directory }}/${{ inputs.coverage-file }}"; got != want {
			t.Errorf("Codecov files = %q, want %q", got, want)
		}
		return
	}
	t.Fatal("go-test job has no coverage upload step")
}

func TestReusableGoTestRacePassDoesNotOverwriteCoverage(t *testing.T) {
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-go-test.yml")
	var raceRun string
	for _, step := range workflow.Jobs["go-test"].Steps {
		if step.Name == "go-test-race" {
			raceRun = step.Run
			break
		}
	}
	if raceRun == "" {
		t.Fatal("go-test job has no race-test step")
	}

	tmp := t.TempDir()
	fakeBin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(tmp, "go-args")
	fakeGo := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GO_ARGS_FILE\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", "-c", raceRun)
	cmd.Dir = tmp
	cmd.Env = []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"GO_ARGS_FILE=" + argsFile,
		"TEST_FLAGS=-coverprofile=coverage.out -covermode=atomic -count=1",
		"TEST_PACKAGES=./...",
		"RUNNER_OS=Linux",
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run race-test script: %v\n%s", err, output)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read go arguments: %v", err)
	}
	if got, want := string(args), "test\n-race\n-count=1\n./...\n"; got != want {
		t.Errorf("race go arguments = %q, want %q", got, want)
	}
}

func TestReusableGoWorkflowsCacheFromModuleFiles(t *testing.T) {
	for _, path := range []string{
		"../.github/workflows/reuseable-go-test.yml",
		"../.github/workflows/reuseable-golangci-lint.yml",
		"../.github/workflows/reuseable-nilaway.yml",
	} {
		workflow := readReusableWorkflow(t, path)
		found := false
		for _, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if !strings.Contains(step.Uses, "actions/setup-go@") {
					continue
				}
				cachePath := step.With["cache-dependency-path"]
				if !strings.Contains(cachePath, "${{ inputs.working-directory }}/go.mod") ||
					!strings.Contains(cachePath, "${{ inputs.working-directory }}/go.sum") {
					t.Errorf("%s cache dependency path = %q, want go.mod and go.sum", path, cachePath)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no actions/setup-go step", path)
		}
	}
}

func TestReusableGolangciLintFormattingCheckPreservesToolFailures(t *testing.T) {
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-golangci-lint.yml")
	var formatRun string
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step.Name == "Check formatting" {
				formatRun = step.Run
			}
		}
	}
	if !strings.Contains(formatRun, "_diff=\"$(golangci-lint fmt --diff 2>&1)\"") ||
		!strings.Contains(formatRun, "_format_status") || strings.Contains(formatRun, "| grep") {
		t.Errorf("format check does not preserve tool failures and diff output:\n%s", formatRun)
	}
}

func TestReusableGoTestCrossBuildBuildsEveryTargetInOneJob(t *testing.T) {
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-go-test.yml")
	steps := workflow.Jobs["cross-build"].Steps
	if len(steps) == 0 {
		t.Fatal("cross-build job has no steps")
	}
	buildRun := steps[len(steps)-1].Run

	tmp := t.TempDir()
	fakeBin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(tmp, "go-calls")
	// The fake go fails for windows/arm64 so the run must keep going and then
	// report that target.
	fakeGo := "#!/bin/sh\n" +
		"echo \"$GOOS/$GOARCH $*\" >> \"$GO_CALLS_FILE\"\n" +
		"[ \"$GOOS/$GOARCH\" != windows/arm64 ]\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	if err := os.Symlink(jq, filepath.Join(fakeBin, "jq")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", "-c", buildRun)
	cmd.Dir = tmp
	cmd.Env = []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"GO_CALLS_FILE=" + callsFile,
		`CROSS_BUILD_TARGETS=[{"os":"linux","arch":"amd64"},{"os":"windows","arch":"arm64"},{"os":"darwin","arch":"arm64"}]`,
		"BUILD_PACKAGES=./cmd/a ./cmd/b",
		"TEST_TAGS=pkcs11",
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("cross-build succeeded although windows/arm64 failed:\n%s", output)
	}
	if !strings.Contains(string(output), "cross-build failed for: windows/arm64") {
		t.Errorf("cross-build did not name the failed target:\n%s", output)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read go calls: %v", err)
	}
	want := "linux/amd64 build -tags pkcs11 ./cmd/a ./cmd/b\n" +
		"windows/arm64 build -tags pkcs11 ./cmd/a ./cmd/b\n" +
		"darwin/arm64 build -tags pkcs11 ./cmd/a ./cmd/b\n"
	if got := string(calls); got != want {
		t.Errorf("go calls = %q, want %q", got, want)
	}
}
