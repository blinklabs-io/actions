package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

func TestFreeModeSkipsOnlyLivePrivateProtection(t *testing.T) {
	t.Chdir("..")
	for _, free := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			t.Run(fmt.Sprintf("free=%t/private=%t", free, private), func(t *testing.T) {
				protectionReads, settingsWrites := 0, 0
				fake := &recordingGitHub{contents: map[string]string{}}
				client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
					r := httptest.NewRecorder()
					r.Header().Set("Content-Type", "application/json")
					switch {
					case req.URL.Path == "/repos/example/project" && req.Method == http.MethodGet:
						_, _ = fmt.Fprintf(r, `{"id":1,"full_name":"example/project","private":%t,"visibility":%q,"default_branch":"main","has_issues":false}`, private, map[bool]string{true: "private", false: "public"}[private])
					case req.URL.Path == "/repos/example/project" && req.Method == http.MethodPatch:
						settingsWrites++
						_, _ = io.WriteString(r, `{}`)
					case strings.Contains(req.URL.Path, "/branches/main/protection"):
						protectionReads++
						r.WriteHeader(403)
						_, _ = io.WriteString(r, `{"message":"Forbidden"}`)
					default:
						if req.Method != http.MethodGet {
							t.Fatalf("unexpected write %s %s", req.Method, req.URL.Path)
						}
						fake.handler().ServeHTTP(r, req)
					}
					return r.Result(), nil
				})})
				var cfg Config
				if err := yaml.Unmarshal([]byte(fmt.Sprintf(`github_free: %t
repositories:
  - name: example/project
    private: %t
    settings:
      default_branch: main
      has_issues: true
    branch_protection:
      - branch: main
`, free, !private)), &cfg); err != nil {
					t.Fatal(err)
				}
				if err := expandProfiles(&cfg); err != nil {
					t.Fatal(err)
				}
				var output strings.Builder
				report := newDriftReport(&output)
				err := reconcileRepositories(context.Background(), client, cfg.Repositories, report)
				skip := free && private
				if (err == nil) != skip || (protectionReads == 0) != skip || (settingsWrites == 1) != skip {
					t.Fatalf("error=%v protection reads=%d settings writes=%d", err, protectionReads, settingsWrites)
				}
				if err := report.render(); err != nil {
					t.Fatal(err)
				}
				if skip && (!strings.Contains(output.String(), "skipped: 1") || strings.Contains(output.String(), "example/project")) {
					t.Fatalf("incorrect private skip report: %s", output.String())
				}
			})
		}
	}
}
