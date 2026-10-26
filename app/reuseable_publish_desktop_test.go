package main

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// reusablePublishDesktopPath is the reusable workflow that drives adder's
// multi-platform release pipeline (macOS .pkg, Windows .msi, tarballs, images).
const reusablePublishDesktopPath = "../.github/workflows/reuseable-publish-desktop.yml"

// loadPublishDesktopWorkflow parses the reusable-publish-desktop workflow's
// workflow_call inputs/secrets and job names. The `on` key is decoded via a
// struct field tag so yaml.v3 does not fold the bare `on:` key to a boolean.
func loadPublishDesktopWorkflow(t *testing.T) (inputs, secrets, jobs map[string]struct{}) {
	t.Helper()
	data, err := os.ReadFile(reusablePublishDesktopPath)
	if err != nil {
		t.Fatalf("read reusable publish-desktop workflow: %v", err)
	}
	var wf struct {
		On struct {
			WorkflowCall struct {
				Inputs  map[string]yaml.Node `yaml:"inputs"`
				Secrets map[string]yaml.Node `yaml:"secrets"`
			} `yaml:"workflow_call"`
		} `yaml:"on"`
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse reusable publish-desktop workflow: %v", err)
	}
	toSet := func(m map[string]yaml.Node) map[string]struct{} {
		out := make(map[string]struct{}, len(m))
		for k := range m {
			out[k] = struct{}{}
		}
		return out
	}
	inputs = toSet(wf.On.WorkflowCall.Inputs)
	secrets = toSet(wf.On.WorkflowCall.Secrets)
	jobs = toSet(wf.Jobs)
	if len(inputs) == 0 {
		t.Fatal("reusable publish-desktop workflow declares no workflow_call inputs")
	}
	if len(jobs) == 0 {
		t.Fatal("reusable publish-desktop workflow declares no jobs")
	}
	return inputs, secrets, jobs
}

// TestReusablePublishDesktopJobs verifies the reusable workflow keeps the full
// release pipeline: draft release, the multi-platform binary/installer matrix,
// native image builds, the manifest merge, and the finalize step.
func TestReusablePublishDesktopJobs(t *testing.T) {
	_, _, jobs := loadPublishDesktopWorkflow(t)
	for _, want := range []string{
		"create-draft-release",
		"build-binaries",
		"build-images",
		"build-image-manifest",
		"finalize-release",
	} {
		if _, ok := jobs[want]; !ok {
			t.Errorf("reusable publish-desktop workflow missing job %q", want)
		}
	}
}

// TestReusablePublishDesktopRequiredInputs verifies the caller-facing contract:
// application-name (required) plus the identifiers adder configures.
func TestReusablePublishDesktopRequiredInputs(t *testing.T) {
	inputs, _, _ := loadPublishDesktopWorkflow(t)
	for _, want := range []string{
		"application-name",
		"go-version",
		"docker-image",
		"description",
	} {
		if _, ok := inputs[want]; !ok {
			t.Errorf("reusable publish-desktop workflow missing input %q", want)
		}
	}
}

// adderPublishWorkflow returns the fully-expanded publish.yml workflow entry for
// blinklabs-io/adder from repos-config.yaml.
func adderPublishWorkflow(t *testing.T) WorkflowConfig {
	t.Helper()
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatalf("read repos-config.yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse repos-config.yaml: %v", err)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatalf("expand profiles: %v", err)
	}
	for _, repo := range cfg.Repositories {
		if repo.Name != "blinklabs-io/adder" {
			continue
		}
		for _, wf := range repo.Workflows {
			if wf.DestinationFile == "publish.yml" {
				return wf
			}
		}
		t.Fatal("blinklabs-io/adder has no publish.yml workflow")
	}
	t.Fatal("blinklabs-io/adder not found in repos-config.yaml")
	return WorkflowConfig{}
}

// TestAdderPublishUsesDesktopReusable pins adder's publish.yml to the shared
// reusable and asserts required params are present.
func TestAdderPublishUsesDesktopReusable(t *testing.T) {
	wf := adderPublishWorkflow(t)
	path, _, _ := strings.Cut(wf.ReusableWorkflow, "@")
	if !strings.HasSuffix(path, "/reuseable-publish-desktop.yml") {
		t.Errorf("adder publish.yml reusable_workflow = %q, want the reuseable-publish-desktop.yml reference", wf.ReusableWorkflow)
	}
	if got := wf.Params["application-name"]; got != "adder" {
		t.Errorf("adder publish.yml application-name = %q, want \"adder\"", got)
	}
	if got := wf.Params["docker-image"]; got != "blinklabs/adder" {
		t.Errorf("adder publish.yml docker-image = %q, want \"blinklabs/adder\"", got)
	}
}

// commitSHARef matches a full 40-hex-character git commit SHA.
var commitSHARef = regexp.MustCompile(`^[0-9a-f]{40}$`)

// wantDesktopReusableSHA is the reviewed commit adder's publish call must pin
// to. It is advanced deliberately (in the same commit that bumps the caller)
// whenever the reusable's release behavior changes, so the pin cannot silently
// lag behind a security-relevant fix. This SHA also resolves the latest Go
// patch release instead of accepting an older cached toolchain.
//
// It MUST be a commit reachable on main (the squash/merge commit from the PR
// that introduced this reusable), NOT a feature-branch commit: this repo
// squash-merges, so a branch commit is unreachable once the branch is deleted
// and GitHub Actions then fails to resolve the reusable ("cannot find
// workflow") before any job is created.
const wantDesktopReusableSHA = "e3dee08bef03cc5e245e18069f48644a6084a4f3"

// TestAdderPublishPinnedToImmutableRef enforces the repository policy that a
// secret-bearing release call must reference a frozen commit SHA, not a mutable
// branch/tag ref like @main. This is the caller that receives Apple, Google
// Cloud, and Docker credentials, so a mutable ref would let an unrelated Actions
// change alter Adder signing/publishing without an Adder review.
func TestAdderPublishPinnedToImmutableRef(t *testing.T) {
	wf := adderPublishWorkflow(t)
	_, ref, ok := strings.Cut(wf.ReusableWorkflow, "@")
	if !ok {
		t.Fatalf("adder publish.yml reusable_workflow %q has no @ref", wf.ReusableWorkflow)
	}
	if !commitSHARef.MatchString(ref) {
		t.Errorf("adder publish.yml must pin the secret-bearing reusable to a 40-char commit SHA, got mutable ref %q", ref)
	}
	// Also pin to the specific reviewed commit that carries the current
	// release behavior (per-arch scan + scan-gated finalize), so the caller
	// cannot regress to an earlier commit lacking those fixes.
	if ref != wantDesktopReusableSHA {
		t.Errorf("adder publish.yml pins reuseable-publish-desktop.yml@%s, want the reviewed SHA %s", ref, wantDesktopReusableSHA)
	}
}

// TestAdderPublishSecretsAndParamsAreDeclared guards against caller/reusable
// drift: every secret and param key adder maps must be a declared input/secret
// of the reusable, otherwise the workflow call fails validation at runtime.
func TestAdderPublishSecretsAndParamsAreDeclared(t *testing.T) {
	inputs, secrets, _ := loadPublishDesktopWorkflow(t)
	wf := adderPublishWorkflow(t)

	for key := range wf.Secrets {
		if _, ok := secrets[key]; !ok {
			t.Errorf("adder publish.yml maps secret %q which the reusable does not declare", key)
		}
	}
	for key := range wf.Params {
		if _, ok := inputs[key]; !ok {
			t.Errorf("adder publish.yml passes param %q which the reusable does not declare as an input", key)
		}
	}
}

// desktopTargetRow is one build-binaries row of binary-target-matrix.
type desktopTargetRow struct {
	Runner string `json:"runner"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Tray   bool   `json:"tray"`
}

// TestDesktopBinaryTargetMatrixDefault pins the default build-binaries rows to
// the eight targets the matrix built before it became an input, in the same
// order, so a caller that does not pass binary-target-matrix builds exactly
// what it built before. The order matters: the two darwin rows sit at opposite
// ends so the default max-parallel of 2 never holds two macOS slots at once.
func TestDesktopBinaryTargetMatrixDefault(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	job, ok := wf.Jobs["build-binaries"]
	if !ok {
		t.Fatal("build-binaries job missing")
	}
	if got, want := job.Strategy.Matrix.Include.Value, "${{ fromJSON(inputs.binary-target-matrix) }}"; got != want {
		t.Fatalf("build-binaries matrix include = %q, want %q", got, want)
	}

	input, ok := wf.On.WorkflowCall.Inputs["binary-target-matrix"]
	if !ok {
		t.Fatal("missing binary-target-matrix input")
	}
	var spec struct {
		Required bool   `yaml:"required"`
		Type     string `yaml:"type"`
		Default  string `yaml:"default"`
	}
	if err := input.Decode(&spec); err != nil {
		t.Fatalf("decode binary-target-matrix input: %v", err)
	}
	if spec.Required || spec.Type != "string" {
		t.Errorf("binary-target-matrix must be an optional string input, got required=%v type=%q", spec.Required, spec.Type)
	}
	var rows []desktopTargetRow
	dec := json.NewDecoder(strings.NewReader(spec.Default))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("binary-target-matrix default is not a JSON row array: %v", err)
	}
	want := []desktopTargetRow{
		{"macos-15", "darwin", "arm64", true},
		{"ubuntu-latest", "freebsd", "amd64", false},
		{"ubuntu-latest", "freebsd", "arm64", false},
		{"ubuntu-latest", "linux", "amd64", true},
		{"ubuntu-24.04-arm", "linux", "arm64", true},
		{"windows-latest", "windows", "amd64", true},
		{"windows-11-arm", "windows", "arm64", true},
		{"macos-15-intel", "darwin", "amd64", true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("binary-target-matrix default rows =\n%+v\nwant\n%+v", rows, want)
	}
}

// TestDesktopBuildBinariesMatrixKeys asserts build-binaries reads no matrix key
// beyond the four every binary-target-matrix row carries. A step that grew a
// dependency on a new key would otherwise see an empty value on every row a
// caller supplies without it.
func TestDesktopBuildBinariesMatrixKeys(t *testing.T) {
	data, err := os.ReadFile(reusablePublishDesktopPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "\n  build-binaries:\n")
	end := strings.Index(text, "\n  build-images:\n")
	if start < 0 || end < start {
		t.Fatal("cannot locate the build-binaries job")
	}
	allowed := map[string]bool{"runner": true, "os": true, "arch": true, "tray": true}
	for _, m := range regexp.MustCompile(`matrix\.([A-Za-z0-9_-]+)`).FindAllStringSubmatch(text[start:end], -1) {
		if !allowed[m[1]] {
			t.Errorf("build-binaries reads matrix.%s, which binary-target-matrix rows do not define", m[1])
		}
	}
}
