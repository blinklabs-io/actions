package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRegistry struct{ tags map[string][]string }

func (f fakeRegistry) ChartVersions(source string) ([]string, error) {
	tags, ok := f.tags[source]
	if !ok {
		return nil, fmt.Errorf("unknown source: %s", source)
	}
	return tags, nil
}
func (f fakeRegistry) ImageVersions(source string) ([]string, error) {
	return f.ChartVersions(source)
}
func (f fakeRegistry) Digest(string) (string, error) { return "sha256:test", nil }
func (f fakeRegistry) Verify(p pin) error {
	if p.Target == "" {
		return fmt.Errorf("empty target")
	}
	return nil
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "helmfile-app/service"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, helmfilePath), "releases:\n  - name: service\n    chart: oci://ghcr.io/example/charts/service\n    version: 1.2.3\n    values:\n      - service/values.yaml\n")
	write(t, filepath.Join(root, "helmfile-app/service/values.yaml"), "image:\n  repository: ghcr.io/example/service\n  tag: \"1.2.3\"\n")
	write(t, filepath.Join(root, reposPath), "repositories: []\n")
	return root
}
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func mustCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := command(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func testRegistry() fakeRegistry {
	return fakeRegistry{map[string][]string{
		"oci://ghcr.io/example/charts/service": {"1.2.3", "1.2.4", "2.0.0-rc1"},
		"ghcr.io/example/service":              {"1.2.3", "1.2.4"},
	}}
}

func TestNewerRejectsPrereleaseAndPrefixChange(t *testing.T) {
	if got := newer("1.2.3", []string{"2.0.0-rc1", "latest", "v2.0.0"}); got != "" {
		t.Fatalf("unexpected upgrade: %s", got)
	}
	if got := newer("1.2.3", []string{"1.2.4", "1.2.5"}); got != "1.2.5" {
		t.Fatalf("got %s", got)
	}
	if got := newer("v1.2.3", []string{"1.2.4"}); got != "" {
		t.Fatalf("prefix changed: %s", got)
	}
	if got := newer("1.2.3", []string{"999999999999999999999999999999.0.0"}); got != "" {
		t.Fatalf("overflowed version accepted: %s", got)
	}
}

func TestPlanAndApplyChartAndImage(t *testing.T) {
	root := fixture(t)
	planned, skipped, err := plan(root, testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(planned["service"]) != 2 {
		t.Fatalf("planned=%v skipped=%v", planned, skipped)
	}
	changed, err := applyPins(root, "service", planned["service"])
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed=%v", changed)
	}
	if !strings.Contains(read(t, filepath.Join(root, helmfilePath)), "version: 1.2.4") {
		t.Fatal("chart version unchanged")
	}
	if !strings.Contains(read(t, filepath.Join(root, "helmfile-app/service/values.yaml")), "tag: \"1.2.4\"") {
		t.Fatal("image tag unchanged")
	}
	changed, err = applyPins(root, "service", planned["service"])
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("repeated edit: %v", changed)
	}
}

func TestSharedValuesAreReported(t *testing.T) {
	root := fixture(t)
	file := filepath.Join(root, helmfilePath)
	write(t, file, read(t, file)+"  - name: second\n    chart: charts/local\n    values:\n      - service/values.yaml\n")
	planned, skipped, err := plan(root, testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(planned["service"]) != 1 || planned["service"][0].Kind != "chart" {
		t.Fatalf("planned=%v", planned)
	}
	count := 0
	for _, item := range skipped {
		if strings.Contains(item, "shared values file") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("shared notices=%d: %v", count, skipped)
	}
}

func TestDirectImageAndChangedChartSourceAreRejected(t *testing.T) {
	root := fixture(t)
	write(t, filepath.Join(root, helmfilePath), "releases:\n  - name: local\n    chart: charts/local\n")
	chart := filepath.Join(root, "helmfile-app/charts/local")
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(chart, "values.yaml"), "image: python:3.13-slim@sha256:abc\n")
	planned, skipped, err := plan(root, testRegistry())
	if err != nil || len(planned) != 0 {
		t.Fatalf("planned=%v err=%v", planned, err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "direct image reference") {
		t.Fatalf("skipped=%v", skipped)
	}
	stale := pin{Release: "local", Kind: "chart", Path: helmfilePath, Current: "1.2.3", Target: "1.2.4", Source: "oci://ghcr.io/example/charts/service"}
	if _, err := applyPins(root, "local", []pin{stale}); err == nil || !strings.Contains(err.Error(), "chart source changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestRangedReleaseAndDigest(t *testing.T) {
	root := fixture(t)
	write(t, filepath.Join(root, helmfilePath), "releases:\n{{- range $network := list \"preview\" \"preprod\" }}\n  - name: dingonodes-{{ $network }}\n    chart: oci://ghcr.io/example/charts/dingonodes\n    version: 0.1.0\n    values:\n      - dingonodes/defaults.yaml.gotmpl\n      - dingonodes/{{ $network }}/values.{{ $network }}.yaml.gotmpl\n{{- end }}\n  - name: {{ .Values.dynamic }}\n    chart: charts/local\n")
	base := filepath.Join(root, "helmfile-app/dingonodes")
	if err := os.MkdirAll(filepath.Join(base, "preview"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "preprod"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "defaults.yaml.gotmpl"), "nodes: {}\n")
	for _, network := range []string{"preview", "preprod"} {
		write(t, filepath.Join(base, network, "values."+network+".yaml.gotmpl"), "nodes:\n  a:\n    spec:\n      image:\n        repository: ghcr.io/example/dingo\n        tag: \"0.94.2\"\n        digest: \"sha256:old\"\n")
	}
	specs, skipped, err := releases(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "dingonodes-preview" || specs[1].ChartOwner != "dingonodes-preview" || len(skipped) != 1 {
		t.Fatalf("specs=%v skipped=%v", specs, skipped)
	}
	registry := fakeRegistry{map[string][]string{"oci://ghcr.io/example/charts/dingonodes": {"0.1.0", "0.1.1"}, "ghcr.io/example/dingo": {"0.94.2", "0.95.0"}}}
	planned, _, err := plan(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(planned["dingonodes-preview"]) != 2 || len(planned["dingonodes-preprod"]) != 1 {
		t.Fatalf("planned=%v", planned)
	}
	if _, err = applyPins(root, "dingonodes-preprod", planned["dingonodes-preprod"]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(base, "preprod/values.preprod.yaml.gotmpl")), "digest: \"sha256:test\"") {
		t.Fatal("digest unchanged")
	}
	if strings.Contains(read(t, filepath.Join(base, "preview/values.preview.yaml.gotmpl")), "sha256:test") {
		t.Fatal("other network changed")
	}
}

func TestRegistryDigestUsesRawManifestBytes(t *testing.T) {
	root := fixture(t)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "skopeo"), "#!/bin/sh\necho 'registry warning' >&2\nif [ \"$1\" = inspect ]; then printf '{\"schemaVersion\":2}\\n'; else printf '{\"Tags\":[\"1.2.3\",\"1.2.4\"]}'; fi\n")
	if err := os.Chmod(filepath.Join(bin, "skopeo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, err := newRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := r.ChartVersions("oci://ghcr.io/example/charts/service")
	if err != nil || len(tags) != 2 {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
	digest, err := r.Digest("ghcr.io/example/image:1.2.4")
	if err != nil {
		t.Fatal(err)
	}
	if digest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("{\"schemaVersion\":2}\n"))) {
		t.Fatalf("digest=%s", digest)
	}
}

func TestImageNamespaceDoesNotUseMatchingHelmAlias(t *testing.T) {
	root := fixture(t)
	write(t, filepath.Join(root, reposPath), "repositories:\n  - name: grafana\n    url: https://example.invalid/charts\n")
	bin := t.TempDir()
	write(t, filepath.Join(bin, "helm"), "#!/bin/sh\nif [ \"$1 $2\" = \"search repo\" ]; then printf '[{\"name\":\"grafana/pdc-agent\",\"version\":\"1.2.4\"}]'; fi\n")
	write(t, filepath.Join(bin, "skopeo"), "#!/bin/sh\nif [ \"$1\" = list-tags ]; then printf '{\"Tags\":[\"1.2.5\"]}'; fi\n")
	for _, name := range []string{"helm", "skopeo"} {
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, err := newRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	chart, err := r.ChartVersions("grafana/pdc-agent")
	if err != nil || len(chart) != 1 || chart[0] != "1.2.4" {
		t.Fatalf("chart=%v err=%v", chart, err)
	}
	image, err := r.ImageVersions("grafana/pdc-agent")
	if err != nil || len(image) != 1 || image[0] != "1.2.5" {
		t.Fatalf("image=%v err=%v", image, err)
	}
	if r.checked != 2 {
		t.Fatalf("expected independent chart and image queries, got %d", r.checked)
	}
	p := pin{Kind: "image", Source: "grafana/pdc-agent", Current: "1.2.3", Target: "1.2.5"}
	body := description("pdc-agent", []pin{p}, r.URLs, 7)
	if !strings.Contains(body, "https://hub.docker.com/r/grafana/pdc-agent/tags") || strings.Contains(body, "example.invalid") {
		t.Fatalf("image source link used Helm alias: %s", body)
	}
}

func fakeGH(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	path := filepath.Join(bin, "gh")
	write(t, path, "#!/bin/sh\n"+script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func initGitFixture(t *testing.T, root string) {
	t.Helper()
	bare := filepath.Join(root, "remote.git")
	mustCommand(t, root, "git", "init", "--bare", bare)
	mustCommand(t, root, "git", "init", "-b", "main")
	mustCommand(t, root, "git", "config", "user.name", "release-bot[bot]")
	mustCommand(t, root, "git", "config", "user.email", "123+release-bot[bot]@users.noreply.github.com")
	mustCommand(t, root, "git", "remote", "add", "origin", bare)
	mustCommand(t, root, "git", "add", "helmfile-app")
	mustCommand(t, root, "git", "commit", "-s", "-m", "test: initial pins")
	mustCommand(t, root, "git", "push", "origin", "main")
	t.Cleanup(func() { mustCommand(t, root, "git", "worktree", "prune") })
}

func TestPushedBranchIsReusedAfterPRCreationFails(t *testing.T) {
	root := fixture(t)
	initGitFixture(t, root)
	failFile := filepath.Join(t.TempDir(), "fail")
	write(t, failFile, "yes")
	t.Setenv("FAKE_GH_FAIL", failFile)
	fakeGH(t, "if [ \"$1 $2\" = \"pr create\" ]; then if [ -e \"$FAKE_GH_FAIL\" ]; then rm \"$FAKE_GH_FAIL\"; echo transient >&2; exit 1; fi; echo https://github.com/example/repo/pull/2; exit 0; fi\nexit 1\n")
	t.Setenv("RELEASE_PIN_ACTOR", "app/release-bot")
	p := pin{Release: "service", Kind: "chart", Path: helmfilePath, Current: "1.2.3", Target: "1.2.4", Source: "oci://ghcr.io/example/charts/service"}
	issue := &openIssue{Number: 7}
	if _, err := publishOne(root, "example/repo", "service", []pin{p}, nil, nil, issue); err == nil {
		t.Fatal("expected first PR creation to fail")
	}
	if out := mustCommand(t, root, "git", "ls-remote", "--heads", "origin", "refs/heads/chore/update-service-issue-7"); out == "" {
		t.Fatal("branch not pushed")
	}
	activity, err := publishOne(root, "example/repo", "service", []pin{p}, nil, nil, issue)
	if err != nil {
		t.Fatal(err)
	}
	if activity != "service: opened https://github.com/example/repo/pull/2" {
		t.Fatalf("activity=%s", activity)
	}
}

func TestExistingPRGetsSignedCommitAndMainMerge(t *testing.T) {
	root := fixture(t)
	initGitFixture(t, root)
	fakeGH(t, "if [ \"$1 $2\" = \"pr create\" ]; then echo https://github.com/example/repo/pull/1; exit 0; fi\nif [ \"$1 $2\" = \"pr edit\" ]; then exit 0; fi\nexit 1\n")
	t.Setenv("RELEASE_PIN_ACTOR", "app/release-bot")
	first := pin{Release: "service", Kind: "chart", Path: helmfilePath, Current: "1.2.3", Target: "1.2.4", Source: "oci://ghcr.io/example/charts/service"}
	issue := &openIssue{Number: 7}
	if _, err := publishOne(root, "example/repo", "service", []pin{first}, nil, nil, issue); err != nil {
		t.Fatal(err)
	}
	branch := "chore/update-service-issue-7"
	previous := mustCommand(t, root, "git", "rev-parse", "origin/"+branch)
	existing := &openPR{Number: 1, Head: branch, Body: description("service", []pin{first}, nil, 7), URL: "https://github.com/example/repo/pull/1"}
	second := first
	second.Target = "1.2.5"
	activity, err := publishOne(root, "example/repo", "service", []pin{second}, nil, existing, issue)
	if err != nil || !strings.Contains(activity, "updated") {
		t.Fatalf("activity=%s err=%v", activity, err)
	}
	current := mustCommand(t, root, "git", "rev-parse", "origin/"+branch)
	if current == previous || mustCommand(t, root, "git", "rev-parse", current+"^") != previous {
		t.Fatal("existing branch was not advanced")
	}
	if !strings.Contains(mustCommand(t, root, "git", "show", "-s", "--format=%B", current), "Signed-off-by: release-bot[bot]") {
		t.Fatal("new commit lacks signoff")
	}
	existing.Body = description("service", []pin{second}, nil, 7)
	write(t, filepath.Join(root, "README.md"), "main advanced\n")
	mustCommand(t, root, "git", "add", "README.md")
	mustCommand(t, root, "git", "commit", "-s", "-m", "test: advance main")
	mustCommand(t, root, "git", "push", "origin", "main")
	activity, err = publishOne(root, "example/repo", "service", []pin{second}, nil, existing, issue)
	if err != nil || !strings.Contains(activity, "updated") {
		t.Fatalf("activity=%s err=%v", activity, err)
	}
	merged := mustCommand(t, root, "git", "rev-parse", "origin/"+branch)
	if merged == current || mustCommand(t, root, "git", "merge-base", "origin/main", merged) != mustCommand(t, root, "git", "rev-parse", "origin/main") {
		t.Fatal("main merge was not pushed")
	}
	if !strings.Contains(mustCommand(t, root, "git", "show", "-s", "--format=%B", merged), "Signed-off-by: release-bot[bot]") {
		t.Fatal("merge lacks signoff")
	}
}

func TestOtherPRTitleAndIssueBodyBlockDuplicate(t *testing.T) {
	root := fixture(t)
	openPRs := filepath.Join(t.TempDir(), "prs.json")
	issues := filepath.Join(t.TempDir(), "issues.json")
	write(t, openPRs, `[{"number":1,"title":"Upgrade service","body":"","headRefName":"manual","baseRefName":"main","isCrossRepository":false,"author":{"login":"maintainer"},"url":"https://github.com/example/repo/pull/1"}]`)
	write(t, issues, `[{"number":2,"title":"Upgrade chart","body":"The service release needs an update.","author":{"login":"maintainer"}}]`)
	t.Setenv("FAKE_OPEN_PRS", openPRs)
	t.Setenv("FAKE_ISSUES", issues)
	t.Setenv("RELEASE_PIN_ACTOR", "app/release-bot")
	fakeGH(t, "if [ \"$1 $2\" = \"pr list\" ]; then case \"$*\" in *'--state open'*) cat \"$FAKE_OPEN_PRS\";; *) echo '[]';; esac; exit 0; fi\nif [ \"$1 $2\" = \"issue list\" ]; then cat \"$FAKE_ISSUES\"; exit 0; fi\nexit 1\n")
	p := pin{Release: "service", Kind: "chart", Path: helmfilePath, Current: "1.2.3", Target: "1.2.4", Source: "oci://ghcr.io/example/charts/service"}
	activity, failed, err := publish(root, "example/repo", map[string][]pin{"service": {p}}, 10, nil)
	if err != nil || failed || len(activity) != 1 || !strings.Contains(activity[0], "another open PR") {
		t.Fatalf("activity=%v failed=%v err=%v", activity, failed, err)
	}
	write(t, openPRs, `[{"number":1,"title":"Unrelated","body":"","headRefName":"manual","baseRefName":"main","isCrossRepository":false,"author":{"login":"maintainer"},"url":"https://github.com/example/repo/pull/1"}]`)
	activity, failed, err = publish(root, "example/repo", map[string][]pin{"service": {p}}, 10, nil)
	if err != nil || failed || len(activity) != 1 || !strings.Contains(activity[0], "another open issue") {
		t.Fatalf("activity=%v failed=%v err=%v", activity, failed, err)
	}
}

func TestUntrustedIssueMarkerIsRejected(t *testing.T) {
	root := fixture(t)
	issues := filepath.Join(t.TempDir(), "issues.json")
	write(t, issues, `[{"number":7,"title":"service release pins","body":"<!-- release-pin-updater: service -->","author":{"login":"maintainer"}}]`)
	t.Setenv("FAKE_ISSUES", issues)
	t.Setenv("RELEASE_PIN_ACTOR", "app/release-bot")
	fakeGH(t, "if [ \"$1 $2\" = \"pr list\" ]; then echo '[]'; exit 0; fi\nif [ \"$1 $2\" = \"issue list\" ]; then cat \"$FAKE_ISSUES\"; exit 0; fi\nexit 1\n")
	p := pin{Release: "service", Kind: "chart", Path: helmfilePath, Current: "1.2.3", Target: "1.2.4", Source: "oci://ghcr.io/example/charts/service"}
	activity, failed, err := publish(root, "example/repo", map[string][]pin{"service": {p}}, 10, nil)
	if err != nil || !failed || len(activity) != 1 || !strings.Contains(activity[0], "untrusted") {
		t.Fatalf("activity=%v failed=%v err=%v", activity, failed, err)
	}
}
