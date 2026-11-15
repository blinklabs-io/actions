//go:build windows && desktopintegration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDesktopWindowsCGO(t *testing.T) {
	wf := loadDesktopWorkflow(t)
	var script string
	for _, step := range wf.Jobs["build-binaries"].Steps {
		if step.Name == "Build binary (Windows)" {
			script = step.Run
			break
		}
	}
	if script == "" {
		t.Fatal("Windows desktop build step missing")
	}
	location := "MSYS2_AMD64_LOC"
	if runtime.GOARCH == "arm64" {
		location = "MSYS2_ARM64_LOC"
	}
	if os.Getenv(location) == "" {
		t.Fatalf("%s must identify an MSYS2 installation with the native C/C++ compiler", location)
	}

	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module desktopfixture\n\ngo 1.26\n",
		"Makefile": "build:\n\tCGO_ENABLED=0 go build -o cli.exe ./cli\n" +
			"build-tray:\n\tCGO_ENABLED=1 go build -x -o tray.exe ./tray\n",
		"cli/main.go": "package main\nfunc main() {}\n",
		"tray/main.go": `package main
/* int answer(void); */
import "C"
func main() {
	if C.answer() != 42 {
		panic("unexpected C++ result")
	}
}
`,
		"tray/answer.cc": `#include <string>
extern "C" int answer(void) {
	return std::stoi(std::string("42"));
}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Match the Actions pwsh wrapper, including propagation of native command failures.
	script = "$ErrorActionPreference = 'Stop'\n" + script + `
if (Test-Path -LiteralPath variable:\LASTEXITCODE) { exit $LASTEXITCODE }
`
	path := filepath.Join(dir, "build.ps1")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MATRIX_OS", "windows")
	t.Setenv("MATRIX_ARCH", runtime.GOARCH)
	t.Setenv("MATRIX_TRAY", "true")
	t.Setenv("GOWORK", "off")
	// Rebuild runtime/cgo instead of accepting a cached object from another compiler.
	t.Setenv("GOCACHE", filepath.Join(dir, "cache"))
	cmd := exec.CommandContext(t.Context(), "pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-File", path)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Windows desktop build failed: %v\n%s", err, output)
	}
	for _, binary := range []string{"cli.exe", "tray.exe"} {
		if _, err := os.Stat(filepath.Join(dir, binary)); err != nil {
			t.Fatalf("Windows desktop build did not produce %s: %v", binary, err)
		}
	}
	t.Logf("Windows C/C++ build succeeded:\n%s", output)
}
