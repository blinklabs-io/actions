package main

import (
	"os"
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
// lag behind a security-relevant fix. This SHA carries the per-architecture
// image scan, the scan-gated finalize-release, and the msys2 v2.32.0 CGO
// toolchain pin.
//
// It MUST be a commit reachable on main (the squash/merge commit from the PR
// that introduced this reusable), NOT a feature-branch commit: this repo
// squash-merges, so a branch commit is unreachable once the branch is deleted
// and GitHub Actions then fails to resolve the reusable ("cannot find
// workflow") before any job is created.
const wantDesktopReusableSHA = "394a5619291089536fd1a22361de7b8ea6703ac8"

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
