package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSharedFormattingGate(t *testing.T) {
	for _, path := range []string{
		"../.github/workflows/reuseable-go-test.yml",
		"../.github/workflows/reuseable-golangci-lint.yml",
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			workflow := readReusableWorkflow(t, path)
			var script string
			for _, job := range workflow.Jobs {
				for _, step := range job.Steps {
					if step.Name == "Check formatting" {
						script = step.Run
					}
				}
			}
			if script == "" {
				t.Fatal("formatting step missing")
			}

			for _, tc := range []struct {
				name       string
				output     string
				status     string
				fail       bool
				configured bool
			}{
				{name: "formatted", status: "0"},
				{name: "diff with successful tool exit", output: "formatting diff", status: "0", fail: true},
				{name: "diff with failing tool exit", output: "formatting diff", status: "1", fail: true},
				{name: "tool error without output", status: "2", fail: true},
				{name: "repository formatter difference", output: "goimports diff", status: "0", fail: true, configured: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					dir := t.TempDir()
					// With no repository config, an unselected formatter reports no diff.
					fake := `#!/bin/sh
enabled=false
for arg in "$@"; do
  [ "$arg" != "--enable=gofumpt" ] || enabled=true
done
if [ "$FORMAT_CONFIGURED" = true ]; then
  [ "$enabled" = false ] || exit 0
  enabled=true
fi
[ "$enabled" = true ] || exit 0
printf '%s' "$FORMAT_OUTPUT"
exit "$FORMAT_STATUS"
`
					if err := os.WriteFile(filepath.Join(dir, "golangci-lint"), []byte(fake), 0o755); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command("bash", "-e", "-c", script)
					cmd.Dir = dir
					cmd.Env = []string{
						"PATH=" + dir + ":/usr/bin:/bin",
						"FORMAT_OUTPUT=" + tc.output,
						"FORMAT_STATUS=" + tc.status,
					}
					if tc.configured {
						cmd.Env = append(cmd.Env, "FORMAT_CONFIGURED=true")
					}
					output, err := cmd.CombinedOutput()
					if (err != nil) != tc.fail {
						t.Fatalf("formatting gate error = %v, want failure %v; output: %s", err, tc.fail, output)
					}
				})
			}
		})
	}
}
