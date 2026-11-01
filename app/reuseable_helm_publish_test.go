package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func helmRegistryFixture(t *testing.T, manifestStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, password, ok := r.BasicAuth()
			if !ok || user != "publisher" || password != "fixture-token" ||
				r.URL.Query().Get("service") != "ghcr.io" ||
				!strings.HasSuffix(r.URL.Query().Get("scope"), ":pull,push") {
				t.Errorf("invalid registry token request: %s", r.URL)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"fixture-bearer"}`))
		case "/v2/blinklabs-io/helm-charts/charts/alpha/manifests/1.0.0",
			"/v2/blinklabs-io/helm-charts/charts/beta/manifests/1.0.0",
			"/v2/blinklabs-io/helm-charts/charts/alpha/manifests/1.0.1":
			if r.Method != http.MethodHead || r.Header.Get("Authorization") != "Bearer fixture-bearer" {
				t.Errorf("invalid manifest request: %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(manifestStatus)
		default:
			t.Errorf("unexpected registry request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
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

func TestHelmPublishRejectsUnbumpedVersions(t *testing.T) {
	script := helmPublishStep(t, "Verify chart version bumps")
	for _, tc := range []struct {
		name, oldVersion, newVersion string
		wantFailure                  bool
	}{
		{"increase", "1.0.0", "1.0.1", false},
		{"same version", "1.0.0", "1.0.0", true},
		{"decrease", "1.2.0", "1.1.0", true},
		{"prerelease increase", "1.0.0-rc.1", "1.0.0-rc.2", false},
		{"build metadata only", "1.0.0+one", "1.0.0+two", true},
		{"new chart", "", "1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			git("init", "--initial-branch=main")
			chart := filepath.Join(dir, "charts", "alpha")
			if err := os.MkdirAll(chart, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.oldVersion != "" {
				if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"),
					[]byte("apiVersion: v2\nname: alpha\nversion: "+tc.oldVersion+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("baseline\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-s", "-m", "test: establish chart baseline")
			before := git("rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"),
				[]byte("apiVersion: v2\nname: alpha\nversion: "+tc.newVersion+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			bin := t.TempDir()
			fakeHelm := `#!/usr/bin/env python3
from pathlib import Path
import sys
assert sys.argv[1:3] == ['show', 'chart']
metadata = Path(sys.argv[3], 'Chart.yaml').read_text()
print(metadata)
`
			if err := os.WriteFile(filepath.Join(bin, "helm"), []byte(fakeHelm), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CHARTS=[\"charts/alpha\"]", "BEFORE="+before)
			out, err := cmd.CombinedOutput()
			if tc.wantFailure {
				if err == nil || !strings.Contains(string(out), "version must increase") {
					t.Fatalf("unbumped chart was accepted or failed elsewhere: %v\n%s", err, out)
				}
			} else if err != nil {
				t.Fatalf("valid chart version rejected: %v\n%s", err, out)
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
			registry := helmRegistryFixture(t, http.StatusNotFound)
			defer registry.Close()
			registryScript := strings.ReplaceAll(script, "https://ghcr.io", registry.URL)
			fakeHelm := `#!/usr/bin/env python3
import os
from pathlib import Path
import sys
args = sys.argv[1:]
if args[:2] == ['show', 'chart']:
    name = Path(args[2]).name.split('-')[0]
    print('name: ' + name)
    print('version: 1.0.0')
    sys.exit(0)
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
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", registryScript)
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

func TestHelmPublishExistingVersion(t *testing.T) {
	script := helmPublishStep(t, "Package and push charts")
	for _, tc := range []struct {
		name, remote, wantError string
		wantPush                bool
	}{
		{"unpublished version", "missing", "", true},
		{"identical retry", "same", "", false},
		{"conflicting version", "different", "different contents", false},
		{"registry lookup failure", "unauthorized", "Unable to check published chart version", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			status := http.StatusOK
			if tc.remote == "missing" {
				status = http.StatusNotFound
			} else if tc.remote == "unauthorized" {
				status = http.StatusForbidden
			}
			registry := helmRegistryFixture(t, status)
			defer registry.Close()
			registryScript := strings.ReplaceAll(script, "https://ghcr.io", registry.URL)

			bin := t.TempDir()
			fakeHelm := `#!/usr/bin/env python3
import io
import os
from pathlib import Path
import shutil
import sys
import tarfile

args = sys.argv[1:]
log = Path(os.environ['HELM_LOG'])
cached = log.with_name('local.tgz')
def record(value):
    with log.open('a') as output:
        output.write(value + '\n')
def archive(path, contents):
    with tarfile.open(path, 'w:gz') as bundle:
        member = tarfile.TarInfo('alpha/Chart.yaml')
        member.mode = 0o644
        member.size = len(contents)
        bundle.addfile(member, io.BytesIO(contents))
if args[0] == 'package':
    record('package')
    path = Path(args[3], 'alpha-1.0.1.tgz')
    archive(path, b'chart version 1.0.1')
    shutil.copyfile(path, cached)
elif args[:2] == ['registry', 'login']:
    record('login')
    assert sys.stdin.read() == 'fixture-token\n'
elif args[:2] == ['show', 'chart']:
    print('name: alpha\nversion: 1.0.1')
elif args[0] == 'pull':
    record('pull')
    remote = os.environ['REMOTE_MODE']
    if remote == 'missing':
        print('Error: chart:1.0.1: not found', file=sys.stderr)
        sys.exit(1)
    if remote == 'unauthorized':
        print('Error: unauthorized', file=sys.stderr)
        sys.exit(1)
    path = Path(args[args.index('--destination') + 1], 'alpha-1.0.1.tgz')
    if remote == 'same':
        shutil.copyfile(cached, path)
    else:
        archive(path, b'different chart')
elif args[0] == 'push':
    record('push')
else:
    raise SystemExit(f'unexpected Helm call: {args}')
`
			if err := os.WriteFile(filepath.Join(bin, "helm"), []byte(fakeHelm), 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "helm.log")
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", registryScript)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CHARTS=[\"charts/alpha\"]",
				"BEFORE="+strings.Repeat("0", 40),
				"REPOSITORY=BlinkLabs-IO/Helm-Charts", "REGISTRY_USER=publisher",
				"REGISTRY_TOKEN=fixture-token", "HELM_LOG="+log, "REMOTE_MODE="+tc.remote)
			out, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("publish failed: %v\n%s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("publish error = %v, want %q\n%s", err, tc.wantError, out)
			}
			operations, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			pushed := strings.Contains(string(operations), "push\n")
			if pushed != tc.wantPush {
				t.Fatalf("push = %v, want %v; operations: %s", pushed, tc.wantPush, operations)
			}
		})
	}
}

func TestHelmPublishFetchAuthenticates(t *testing.T) {
	script := helmPublishStep(t, "Detect changed charts")
	remote, client := t.TempDir(), t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(remote, "init", "--initial-branch=main")
	chart := filepath.Join(remote, "charts", "alpha")
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: alpha\nversion: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := func() string {
		git(remote, "add", ".")
		git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-s", "-m", "test: update chart fixture")
		return git(remote, "rev-parse", "HEAD")
	}
	before := commit()
	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), []byte("changed: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := commit()
	git(client, "init", "--initial-branch=main")
	git(client, "remote", "add", "origin", "file://"+remote)
	git(client, "fetch", "--depth=1", "origin", after)
	git(client, "checkout", "--detach", "FETCH_HEAD")

	bin := t.TempDir()
	fakeGit := `#!/usr/bin/env python3
import base64
import os
from pathlib import Path
import sys
if sys.argv[1] == 'fetch':
    expected = 'AUTHORIZATION: basic ' + base64.b64encode(b'x-access-token:fixture-token').decode()
    if (os.environ.get('GIT_CONFIG_COUNT') != '1' or
        os.environ.get('GIT_CONFIG_KEY_0') != 'http.https://github.com/.extraheader' or
        os.environ.get('GIT_CONFIG_VALUE_0') != expected):
        raise SystemExit('fetch omitted authentication header')
    Path(os.environ['FETCH_LOG']).write_text('authenticated fetch')
os.execv(os.environ['REAL_GIT'], [os.environ['REAL_GIT']] + sys.argv[1:])
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	output, fetchLog := filepath.Join(bin, "output"), filepath.Join(bin, "fetch.log")
	cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
	cmd.Dir = client
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GIT="+realGit, "BEFORE="+before, "AFTER="+after, "GITHUB_OUTPUT="+output,
		"FETCH_TOKEN=fixture-token", "GITHUB_SERVER_URL=https://github.com", "FETCH_LOG="+fetchLog)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detect charts with authenticated fetch: %v\n%s", err, out)
	}
	if _, err := os.Stat(fetchLog); err != nil {
		t.Fatal("missing-before case did not fetch from origin")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "charts=[\"charts/alpha\"]\n" {
		t.Fatalf("detected charts after fetch = %s", data)
	}
}
