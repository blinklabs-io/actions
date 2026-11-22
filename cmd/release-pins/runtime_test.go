// Copyright 2026 Blink Labs Software

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func cardanoFixture(t *testing.T) string {
	t.Helper()
	root := fixture(t)
	write(t, filepath.Join(root, helmfilePath), "releases:\n  - name: service\n    chart: "+cardanoChart+"\n    version: 0.11.0\n    values:\n      - service/values.yaml\n")
	write(t, filepath.Join(root, "helmfile-app/service/values.yaml"), "cardano_network: prime-testnet\ncardano_config: /opt/cardano/config/prime-testnet/configuration.yaml\nimage:\n  repository: "+cardanoImage+"\n  tag: 10.1.4-5\n")
	return root
}

func fakeContainer(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	engine := "docker"
	if runtime.GOOS == "darwin" {
		engine = "container"
	}
	file := filepath.Join(bin, engine)
	write(t, file, `#!/bin/sh
printf '%s\n' "$*" >> "$RUNTIME_CALLS"
case "$*" in
  run*11.1.3-2*) echo 'Aeson exception: key "Options" not found' >&2; exit 1;;
  run*9.0.0-1*) exit 124;;
esac
`)
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNTIME_CALLS", log)
	return log
}

func TestIncompatibleCardanoRuntimeBlocksWholeRelease(t *testing.T) {
	root := cardanoFixture(t)
	log := fakeContainer(t)
	r := fakeRegistry{map[string][]string{cardanoChart: {"0.12.0"}, cardanoImage: {"11.1.3-2"}}}
	planned, skipped, err := plan(root, r)
	if err != nil || len(planned) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], `key "Options" not found`) {
		t.Fatalf("planned=%v skipped=%v err=%v", planned, skipped, err)
	}
	calls := read(t, log)
	for _, expected := range []string{"--network none", "--cap-drop ALL", "cardano-node:11.1.3-2", "--config /opt/cardano/config/prime-testnet/configuration.yaml", "--shutdown-ipc 0", "rm --force"} {
		if !strings.Contains(calls, expected) {
			t.Fatalf("missing %q in %s", expected, calls)
		}
	}
	if !strings.Contains(read(t, filepath.Join(root, "helmfile-app/service/values.yaml")), "tag: 10.1.4-5") {
		t.Fatal("scan changed the current image")
	}
}

func TestCompatibleCardanoConfigurationAllowsChartUpdate(t *testing.T) {
	root := cardanoFixture(t)
	log := fakeContainer(t)
	r := fakeRegistry{map[string][]string{cardanoChart: {"0.12.0"}, cardanoImage: {"10.1.4-5"}}}
	planned, skipped, err := plan(root, r)
	if err != nil || len(skipped) != 0 || len(planned["service"]) != 1 {
		t.Fatalf("planned=%v skipped=%v err=%v", planned, skipped, err)
	}
	if !strings.Contains(read(t, log), "cardano-node:10.1.4-5") {
		t.Fatal("did not validate retained runtime")
	}
}

func TestCardanoRuntimeProbeUsesPrimaryImageTag(t *testing.T) {
	root := cardanoFixture(t)
	log := fakeContainer(t)
	relative := "helmfile-app/service/values.yaml"
	file := filepath.Join(root, relative)
	write(t, file, read(t, file)+"  digest: sha256:unused-by-chart\nmithril:\n  image:\n    repository: "+cardanoImage+"\n    tag: 10.1.4-5\n")
	specs, _, err := releases(root)
	if err != nil {
		t.Fatal(err)
	}
	pins := []pin{{Kind: "image", Path: relative, Source: cardanoImage, Ordinal: 1, Target: "11.1.3-2"}}
	if err := validateRuntime(root, specs[0], pins); err != nil {
		t.Fatal(err)
	}
	calls := read(t, log)
	if !strings.Contains(calls, cardanoImage+":10.1.4-5 ") || strings.Contains(calls, "@sha256:") {
		t.Fatalf("probe did not use the primary image's rendered reference: %s", calls)
	}
}

func TestRuntimeConfigurationOverridesAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name, override, want string
	}{
		{"later config", "cardano_config: /opt/cardano/config/custom/config.json\n", "--config /opt/cardano/config/custom/config.json"},
		{"nested unrelated template", "labels:\n  {{- range $key, $value := .Values.labels }}\n  {{ $key }}: {{ $value }}\n  {{- end }}\ncardano_config: /opt/cardano/config/custom/config.json\n", "--config /opt/cardano/config/custom/config.json"},
		{"quoted config key", "'cardano_config': /opt/cardano/config/custom/config.json\n", "unsupported runtime key"},
		{"quoted image key", "image:\n  'tag': 11.1.3-2\n", "unsupported runtime key"},
		{"root merge", "override: &override {image: {tag: 11.1.3-2}}\n<<: *override\n", "unsupported runtime key"},
		{"image merge", "override: &override {tag: 11.1.3-2}\nimage:\n  <<: *override\n", "unsupported runtime key"},
		{"root template injection", "someKey: value\n{{ toYaml .Values.overrides }}\n", "unsupported runtime key"},
		{"later image", "image:\n  tag: 11.1.3-2\n", "runtime initialization failed"},
		{"timeout", "image:\n  tag: 9.0.0-1\n", "runtime initialization failed"},
		{"dynamic", "cardano_config: {{ .Values.config }}\n", "dynamic runtime value"},
		{"conditional", "{{ if .Values.useNew }}\ncardano_config: /opt/cardano/config/new/config.json\n{{ end }}\n", "conditional runtime value"},
		{"duplicate", "cardano_network: mainnet\ncardano_network: preview\n", "ambiguous"},
		{"flow image override", "image: {tag: 11.1.3-2}\n", "unsupported"},
		{"null image override", "image: null\n", "unsupported"},
		{"templated image override", "image: {{ .Values.image }}\n", "dynamic runtime value"},
		{"external", "cardano_config: /mounted/config.json\n", "must be a bundled file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := cardanoFixture(t)
			log := fakeContainer(t)
			write(t, filepath.Join(root, helmfilePath), read(t, filepath.Join(root, helmfilePath))+"      - service/override.yaml\n")
			write(t, filepath.Join(root, "helmfile-app/service/override.yaml"), test.override)
			specs, _, err := releases(root)
			if err != nil {
				t.Fatal(err)
			}
			err = validateRuntime(root, specs[0], nil)
			if strings.HasPrefix(test.want, "--config ") {
				if err != nil || !strings.Contains(read(t, log), test.want) {
					t.Fatalf("err=%v calls=%s", err, read(t, log))
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want=%s", err, test.want)
			}
		})
	}
}

func TestUnsupportedCardanoReleaseValuesBlockProbe(t *testing.T) {
	for _, test := range []struct{ name, original, replacement string }{
		{"inline map", "      - service/values.yaml", "      - service/values.yaml\n      - image:\n          tag: 11.1.3-2"},
		{"dynamic path", "      - service/values.yaml", "      - service/{{ .Environment.Name }}.yaml"},
		{"inline list", "values:\n      - service/values.yaml", "values: [service/values.yaml]"},
		{"conditional path", "      - service/values.yaml", "      {{ if .Values.enabled }}\n      - service/values.yaml\n      {{ end }}"},
		{"unindented conditional path", "      - service/values.yaml", "{{ if .Values.enabled }}\n      - service/values.yaml\n{{ end }}"},
		{"conditional values", "    values:\n      - service/values.yaml", "    {{ if .Values.enabled }}\n    values:\n      - service/values.yaml\n    {{ end }}"},
		{"set override", "      - service/values.yaml", "      - service/values.yaml\n    set:\n      - name: image.tag\n        value: 11.1.3-2"},
		{"template injection", "      - service/values.yaml", "      - service/values.yaml\n    {{ readFile \"override.gotmpl\" }}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := cardanoFixture(t)
			log := fakeContainer(t)
			file := filepath.Join(root, helmfilePath)
			write(t, file, strings.Replace(read(t, file), test.original, test.replacement, 1))
			registry := fakeRegistry{map[string][]string{cardanoChart: {"0.12.0"}, cardanoImage: {"11.1.3-2"}}}
			planned, skipped, err := plan(root, registry)
			if err != nil || len(planned) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "static list of values files") {
				t.Fatalf("planned=%v skipped=%v err=%v", planned, skipped, err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("expected no runtime invocation, got log stat %v", err)
			}
		})
	}
}

func TestExistingPRWithInvalidRuntimeIsNotPushed(t *testing.T) {
	root := cardanoFixture(t)
	initGitFixture(t, root)
	fakeContainer(t)
	branch := branchName("service", "7")
	mustCommand(t, root, "git", "checkout", "-b", branch)
	file := filepath.Join(root, "helmfile-app/service/values.yaml")
	write(t, file, strings.ReplaceAll(read(t, file), "10.1.4-5", "11.1.3-2"))
	mustCommand(t, root, "git", "add", "helmfile-app")
	mustCommand(t, root, "git", "commit", "-s", "-m", "test: incompatible runtime")
	mustCommand(t, root, "git", "push", "origin", branch)
	previous := mustCommand(t, root, "git", "rev-parse", "HEAD")
	mustCommand(t, root, "git", "checkout", "main")
	p := pin{Release: "service", Kind: "chart", Path: helmfilePath, Current: "0.11.0", Target: "0.12.0", Source: cardanoChart}
	existing := &openPR{Number: 1, Head: branch, Body: description("service", []pin{p}, nil, 7)}
	fakeGH(t, "echo unexpected GitHub write >&2; exit 1\n")
	_, err := publishOne(root, "example/repo", "service", []pin{p}, nil, existing, &openIssue{Number: 7})
	if err == nil || !strings.Contains(err.Error(), `key "Options" not found`) {
		t.Fatalf("err=%v", err)
	}
	if current := mustCommand(t, root, "git", "rev-parse", "origin/"+branch); current != previous {
		t.Fatal("incompatible branch advanced")
	}
}
