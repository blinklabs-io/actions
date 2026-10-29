package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func helmPublishStep(t *testing.T, name string) string {
	t.Helper()
	workflow := readReusableWorkflow(t, "../.github/workflows/reuseable-helm-publish.yml")
	for _, step := range workflow.Jobs["publish"].Steps {
		if step.Name == name {
			return step.Run
		}
	}
	t.Fatalf("missing Helm publisher step %q", name)
	return ""
}

func TestHelmPublishDetectChangedCharts(t *testing.T) {
	script := helmPublishStep(t, "Detect changed charts")
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		git("add", ".")
		git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-s", "-m", "test: update chart fixture")
		return git("rev-parse", "HEAD")
	}
	git("init", "--initial-branch=main")
	for _, chart := range []string{"alpha", "beta", "deleted", "renamed", "unchanged"} {
		write("charts/"+chart+"/Chart.yaml", "apiVersion: v2\nname: "+chart+"\nversion: 1.0.0\n")
	}
	before := commit()
	write("charts/alpha/templates/config.yaml", "first commit\n")
	commit()
	write("charts/beta/values.yaml", "second commit\n")
	write("charts/new/Chart.yaml", "apiVersion: v2\nname: new\nversion: 1.0.0\n")
	write("charts/not-a-chart/README.md", "documentation only\n")
	write("README.md", "outside charts\n")
	git("rm", "-r", "charts/deleted")
	git("mv", "charts/renamed", "charts/moved")
	after := commit()

	for _, tc := range []struct {
		name, before, after string
		want                []string
	}{
		{"whole push", before, after, []string{"charts/alpha", "charts/beta", "charts/moved", "charts/new"}},
		{"initial push", strings.Repeat("0", 40), after, []string{"charts/alpha", "charts/beta", "charts/moved", "charts/new", "charts/unchanged"}},
		{"empty diff", after, after, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "BEFORE="+tc.before, "AFTER="+tc.after, "GITHUB_OUTPUT="+output)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("detect charts: %v\n%s", err, out)
			}
			out, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(string(out)), "charts=")), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("charts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHelmPublishPackagesBeforeUploading(t *testing.T) {
	script := helmPublishStep(t, "Package and push charts")
	for _, tc := range []struct {
		name, charts, fail string
		want               []string
	}{
		{"two charts", `["charts/alpha","charts/beta"]`, "", []string{"package charts/alpha", "package charts/beta", "login", "push alpha-1.0.0.tgz oci://ghcr.io/blinklabs-io/helm-charts/charts", "push beta-1.0.0.tgz oci://ghcr.io/blinklabs-io/helm-charts/charts"}},
		{"no charts", `[]`, "", nil},
		{"package failure", `["charts/alpha","charts/beta"]`, "package", []string{"package charts/alpha"}},
		{"login failure", `["charts/alpha"]`, "login", []string{"package charts/alpha", "login"}},
		{"push failure", `["charts/alpha","charts/beta"]`, "push", []string{"package charts/alpha", "package charts/beta", "login", "push alpha-1.0.0.tgz oci://ghcr.io/blinklabs-io/helm-charts/charts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeHelm := `#!/usr/bin/env python3
import os
from pathlib import Path
import sys
args = sys.argv[1:]
if args[0] == 'package':
    action = 'package'
    line = 'package ' + args[1]
    Path(args[3], Path(args[1]).name + '-1.0.0.tgz').write_text('chart')
elif args[:2] == ['registry', 'login']:
    action, line = 'login', 'login'
    assert args[2:] == ['ghcr.io', '--username', 'publisher', '--password-stdin']
    assert sys.stdin.read() == 'fixture-token\n'
    assert 'HELM_REGISTRY_CONFIG' in os.environ
else:
    action = 'push'
    assert Path(args[1]).is_file()
    assert 'HELM_REGISTRY_CONFIG' in os.environ
    line = 'push ' + Path(args[1]).name + ' ' + args[2]
with open(os.environ['HELM_LOG'], 'a') as log:
    log.write(line + '\n')
if action == os.environ['FAIL']:
    sys.exit(1)
`
			if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(fakeHelm), 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "helm.log")
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CHARTS="+tc.charts, "REPOSITORY=BlinkLabs-IO/Helm-Charts", "REGISTRY_USER=publisher",
				"REGISTRY_TOKEN=fixture-token", "HELM_LOG="+log, "FAIL="+tc.fail)
			out, err := cmd.CombinedOutput()
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("publish error = %v\n%s", err, out)
			}
			data, err := os.ReadFile(log)
			if err != nil && (len(tc.want) != 0 || !os.IsNotExist(err)) {
				t.Fatal(err)
			}
			var got []string
			if len(data) > 0 {
				got = strings.Split(strings.TrimSpace(string(data)), "\n")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Helm operations = %v, want %v", got, tc.want)
			}
		})
	}
}
