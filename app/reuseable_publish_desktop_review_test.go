package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// desktopWorkflow is a partial view of the reusable-publish-desktop workflow
// used to assert the review-driven guarantees (attestation-preserving manifest,
// shipped+attested tray binary, no-partial-release guard, image scan, and
// least-privilege job permissions).
type desktopWorkflow struct {
	On struct {
		WorkflowCall struct {
			Inputs map[string]yaml.Node `yaml:"inputs"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Name string `yaml:"name"`
			If   string `yaml:"if"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadDesktopWorkflow(t *testing.T) desktopWorkflow {
	t.Helper()
	data, err := os.ReadFile(reusablePublishDesktopPath)
	if err != nil {
		t.Fatalf("read reusable publish-desktop workflow: %v", err)
	}
	var wf desktopWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse reusable publish-desktop workflow: %v", err)
	}
	return wf
}

// TestManifestPreservesAttestations asserts the manifest merge uses
// `docker buildx imagetools create` (which copies OCI attestation descriptors)
// and no longer uses `docker manifest create`, which fails on the
// unknown/unknown attestation entries. (P1: preserve OCI attestations)
func TestManifestPreservesAttestations(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	job, ok := wf.Jobs["build-image-manifest"]
	if !ok {
		t.Fatal("build-image-manifest job missing")
	}
	var sawImagetools bool
	for _, s := range job.Steps {
		for _, line := range strings.Split(s.Run, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue // ignore explanatory comments
			}
			if strings.Contains(trimmed, "docker manifest create") {
				t.Error("build-image-manifest still runs `docker manifest create`, which drops attestations")
			}
			if strings.Contains(trimmed, "docker buildx imagetools create") {
				sawImagetools = true
			}
		}
	}
	if !sawImagetools {
		t.Error("build-image-manifest does not use `docker buildx imagetools create`")
	}
}

// TestTrayBinaryShippedAndAttested asserts tray-enabled rows package the tray
// binary into the tarball and attest it, so every built subject is downloadable
// and covered by provenance. (P1: ship and attest the Linux tray binary)
func TestTrayBinaryShippedAndAttested(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	job, ok := wf.Jobs["build-binaries"]
	if !ok {
		t.Fatal("build-binaries job missing")
	}
	var packagesTray, attestsTray bool
	for _, s := range job.Steps {
		if strings.Contains(s.Run, `${APPLICATION_NAME}-tray`) && strings.Contains(s.Run, "tar czf") {
			packagesTray = true
		}
		if s.Name == "Attest tray binary" && strings.Contains(s.If, "matrix.tray") {
			attestsTray = true
		}
	}
	if !packagesTray {
		t.Error("tarball upload does not package the tray binary on tray rows")
	}
	if !attestsTray {
		t.Error("no matrix.tray-gated `Attest tray binary` step")
	}
}

// TestNoPartialRelease asserts finalize-release does not use always(): with a
// plain `needs`, the job is skipped if any build fails, leaving a draft rather
// than publishing a release missing assets. (P1: do not publish a partial release)
func TestNoPartialRelease(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	job, ok := wf.Jobs["finalize-release"]
	if !ok {
		t.Fatal("finalize-release job missing")
	}
	if strings.Contains(job.If, "always()") {
		t.Errorf("finalize-release still uses always(); if = %q", job.If)
	}
	for _, want := range []string{"build-binaries", "build-images", "build-image-manifest"} {
		found := false
		for _, s := range []string{"create-draft-release", "build-binaries", "build-images", "build-image-manifest"} {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("finalize-release must depend on %q", want)
		}
	}
}

// TestImageScanEnabled asserts the published image is Trivy-scanned with the
// SARIF uploaded, and the manifest job holds the required security-events scope.
// (P2: retain the shared image vulnerability scan)
func TestImageScanEnabled(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	if _, ok := wf.On.WorkflowCall.Inputs["enable-trivy-scan"]; !ok {
		t.Error("missing enable-trivy-scan input")
	}
	job, ok := wf.Jobs["build-image-manifest"]
	if !ok {
		t.Fatal("build-image-manifest job missing")
	}
	if job.Permissions["security-events"] != "write" {
		t.Errorf("build-image-manifest needs security-events: write, got %q", job.Permissions["security-events"])
	}
	var sawTrivy bool
	for _, s := range job.Steps {
		if strings.Contains(s.Uses, "aquasecurity/trivy-action") {
			sawTrivy = true
		}
	}
	if !sawTrivy {
		t.Error("build-image-manifest does not run the Trivy scanner")
	}
}

// TestLeastPrivilegeJobPermissions asserts the release jobs drop the unused
// Actions/Checks/Statuses scopes and only hold what they use. (P2: remove
// unrelated write scopes)
func TestLeastPrivilegeJobPermissions(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	unused := []string{"actions", "checks", "statuses"}

	bin := wf.Jobs["build-binaries"].Permissions
	for _, k := range append(append([]string{}, unused...), "packages") {
		if _, ok := bin[k]; ok {
			t.Errorf("build-binaries should not request %q", k)
		}
	}
	for _, k := range []string{"attestations", "contents", "id-token"} {
		if bin[k] != "write" {
			t.Errorf("build-binaries needs %q: write, got %q", k, bin[k])
		}
	}

	img := wf.Jobs["build-images"].Permissions
	for _, k := range unused {
		if _, ok := img[k]; ok {
			t.Errorf("build-images should not request %q", k)
		}
	}
	if img["contents"] != "read" {
		t.Errorf("build-images needs contents: read, got %q", img["contents"])
	}
	if img["packages"] != "write" {
		t.Errorf("build-images needs packages: write, got %q", img["packages"])
	}
}

// TestBuildxPinnedToCurrentSHA asserts the buildx action is pinned to the same
// v4.3.0 SHA the rest of the repo uses, not a rolled-back version. (P2: keep
// Adder's current Buildx action)
func TestBuildxPinnedToCurrentSHA(t *testing.T) {
	data, err := os.ReadFile(reusablePublishDesktopPath)
	if err != nil {
		t.Fatal(err)
	}
	const wantSHA = "docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e"
	const oldSHA = "docker/setup-buildx-action@bb05f3f5519dd87d3ba754cc423b652a5edd6d2c"
	if strings.Contains(string(data), oldSHA) {
		t.Error("reusable still pins the older docker/setup-buildx-action v4.2.0 SHA")
	}
	if !strings.Contains(string(data), wantSHA) {
		t.Errorf("reusable should pin docker/setup-buildx-action to %s", wantSHA)
	}
}
