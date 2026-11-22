package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

const canarySharedRef = "7328ad66dec3a12676f5a9d1948fa74946712613"

func TestSharedWorkflowRefsRecognizeSourceBoundaries(t *testing.T) {
	for _, source := range []string{"blinklabs-io/actions/", sharedWorkflowRepository, "other/repo/.github/workflows/check.yml@main"} {
		cfg := Config{Repositories: []RepoConfig{{SharedActionsRef: canarySharedRef,
			Workflows: []WorkflowConfig{{ReusableWorkflow: source}}}}}
		err := resolveSharedWorkflowRefs(&cfg, nil)
		if strings.HasPrefix(source, "other/") {
			if err != nil || cfg.Repositories[0].Workflows[0].ReusableWorkflow != source {
				t.Fatalf("foreign workflow changed: %v", err)
			}
		} else if err == nil {
			t.Fatal("empty path in recognized source accepted")
		}
	}
}

func TestSharedWorkflowRefsAfterProfileAndFanout(t *testing.T) {
	cfg := Config{
		ReleaseFanoutDispatchRef: strings.Repeat("b", 40),
		Profiles: map[string]Profile{"base": {Workflows: []WorkflowConfig{{
			DestinationFile: "check.yml", ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
			PinToLatestChange: true, Params: map[string]string{"run-build": "true"},
		}}}},
		Repositories: []RepoConfig{
			{Name: "org/canary", Profile: "base", SharedActionsRef: canarySharedRef,
				ReleaseFanout: []FanoutTarget{{Repo: "org/consumer", Type: "version-refs", Refs: []string{"version.txt"}}}},
			{Name: "org/unchanged", Profile: "base"},
		},
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(cfg.Repositories[1])
	calls := map[string]int{}
	if err := resolveSharedWorkflowRefs(&cfg, func(ref, path string) ([]byte, error) {
		if ref != canarySharedRef {
			t.Fatalf("read source at %q, want immutable canary revision", ref)
		}
		calls[path]++
		return []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, workflow := range cfg.Repositories[0].Workflows {
		if !strings.HasPrefix(workflow.ReusableWorkflow, sharedWorkflowRepository) ||
			!strings.HasSuffix(workflow.ReusableWorkflow, "@"+canarySharedRef) || workflow.PinToLatestChange {
			t.Fatalf("workflow not pinned to verified public source: %+v", workflow)
		}
		if workflow.DestinationFile == fanoutWorkflowFile && workflow.Secrets["token"] != "ORG_PROJECT_PAT" {
			t.Fatal("dispatch token mapping changed")
		}
	}
	if len(calls) != 2 || calls[".github/workflows/reuseable-release-dispatch.yml"] != 1 {
		t.Fatalf("injected fanout was not verified: %v", calls)
	}
	after, _ := json.Marshal(cfg.Repositories[1])
	if string(before) != string(after) {
		t.Fatal("unselected repository changed")
	}
}

func TestSharedWorkflowRefsFailBeforeChangingConfig(t *testing.T) {
	for _, test := range []struct {
		name, ref, second, content string
		readError                  bool
	}{
		{name: "mutable ref", ref: "main"},
		{name: "uppercase ref", ref: strings.Repeat("A", 40)},
		{name: "missing ref", ref: canarySharedRef, second: "reuseable-other.yml"},
		{name: "escaping path", ref: canarySharedRef, second: "../reuseable-other.yml@main"},
		{name: "missing source", ref: canarySharedRef, second: "reuseable-other.yml@main", readError: true},
		{name: "not callable", ref: canarySharedRef, second: "reuseable-other.yml@main", content: "on: push\n"},
		{name: "invalid YAML", ref: canarySharedRef, second: "reuseable-other.yml@main", content: "on: [\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Repositories: []RepoConfig{{SharedActionsRef: test.ref, Workflows: []WorkflowConfig{
				{ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main", PinToLatestChange: true},
				{ReusableWorkflow: "blinklabs-io/actions/.github/workflows/" + test.second},
			}}}}
			before, _ := json.Marshal(cfg)
			err := resolveSharedWorkflowRefs(&cfg, func(_ string, path string) ([]byte, error) {
				if path != ".github/workflows/reuseable-go-test.yml" {
					if test.readError {
						return nil, errors.New("source not found")
					}
					return []byte(test.content), nil
				}
				return []byte(callableWorkflowYAML), nil
			})
			if err == nil {
				t.Fatal("invalid public workflow source accepted")
			}
			after, _ := json.Marshal(cfg)
			if string(before) != string(after) {
				t.Fatal("invalid source left partial configuration changes")
			}
		})
	}
}

func TestFetchSharedWorkflowUsesImmutablePublicSource(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory", "missing", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/blinklabs-io/shared-actions/git/trees/"+canarySharedRef {
					if r.URL.Query().Get("recursive") != "1" {
						t.Error("source tree was not recursive")
					}
					mode := "100644"
					if kind == "symlink" {
						mode = "120000"
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"truncated": kind == "truncated",
						"tree": []map[string]string{{"path": ".github/workflows/reuseable-go-test.yml", "type": "blob", "mode": mode}}})
					return
				}
				if r.URL.Path != "/repos/blinklabs-io/shared-actions/contents/.github/workflows/reuseable-go-test.yml" ||
					r.URL.Query().Get("ref") != canarySharedRef {
					t.Errorf("unexpected source request %s", r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch kind {
				case "missing":
					w.WriteHeader(http.StatusNotFound)
				case "directory":
					_, _ = w.Write([]byte("[]"))
				default:
					_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "base64",
						"content": base64.StdEncoding.EncodeToString([]byte(callableWorkflowYAML))})
				}
			}))
			defer server.Close()
			client := github.NewClient(server.Client())
			client.BaseURL, _ = url.Parse(server.URL + "/")
			content, err := fetchSharedWorkflow(context.Background(), client, canarySharedRef, ".github/workflows/reuseable-go-test.yml")
			if kind == "file" {
				if err != nil || string(content) != callableWorkflowYAML {
					t.Fatalf("public source not read: %v", err)
				}
			} else if err == nil {
				t.Fatal("non-file public source accepted")
			}
		})
	}
}

func TestSharedCanaryRenderingPreservesLiveContracts(t *testing.T) {
	cfg := loadRealConfig(t)
	before := make([]string, len(cfg.Repositories))
	for i, repo := range cfg.Repositories {
		data, err := json.Marshal(repo)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = string(data)
	}
	if err := resolveSharedWorkflowRefs(&cfg, func(string, string) ([]byte, error) {
		return []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	selected := 0
	for i, repo := range cfg.Repositories {
		if repo.SharedActionsRef == "" {
			after, err := json.Marshal(repo)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != before[i] {
				t.Fatalf("non-canary repository %s changed", repo.Name)
			}
			continue
		}
		selected++
		if (repo.Name != "blinklabs-io/go-bip39" && repo.Name != "blinklabs-io/adder") || repo.SharedActionsRef != canarySharedRef {
			t.Fatalf("unexpected rollout target %s at %s", repo.Name, repo.SharedActionsRef)
		}
		standalone, pipelines := groupPipelines(repo.Workflows)
		outputs := map[string][]byte{}
		wrapper, err := newWorkflowTemplate(t)
		if err != nil {
			t.Fatal(err)
		}
		for _, wf := range standalone {
			outputs[wf.DestinationFile], err = renderWorkflow(wrapper, wf)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, members := range pipelines {
			outputs[members[0].Pipeline], err = renderPipeline(newPipelineTemplate(t), members)
			if err != nil {
				t.Fatal(err)
			}
		}
		fixturePath, wantOutputs := "testdata/shared-actions-canary/", 2
		if repo.Name == "blinklabs-io/adder" {
			fixturePath, wantOutputs = fixturePath+"adder/", 4
		}
		if len(outputs) != wantOutputs {
			t.Fatalf("canary outputs = %d, want %d current callers", len(outputs), wantOutputs)
		}
		for name, output := range outputs {
			baseline, err := os.ReadFile(fixturePath + name)
			if err != nil {
				t.Fatal(err)
			}
			expected := string(baseline)
			for _, old := range []string{
				"blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
				"blinklabs-io/actions/.github/workflows/reuseable-go-library-release.yml@e3dee08bef03cc5e245e18069f48644a6084a4f3",
				"blinklabs-io/actions/.github/workflows/reuseable-conventional-commits.yml@main",
				"blinklabs-io/actions/.github/workflows/reuseable-ci-docker-multiarch.yml@main",
				"blinklabs-io/actions/.github/workflows/reuseable-publish-desktop.yml@50f4ed21d4ead09f72ca3b696c4e607a8c2832be",
				"blinklabs-io/actions/.github/workflows/reuseable-release-dispatch.yml@e0caeb743570431ebc4e26aeea54cdf8ab6a0a33",
			} {
				path := strings.Split(strings.TrimPrefix(old, "blinklabs-io/actions/"), "@")[0]
				expected = strings.ReplaceAll(expected, old, sharedWorkflowRepository+path+"@"+canarySharedRef)
			}
			if string(output) != expected {
				t.Fatalf("%s changes the live contract beyond source refs\n%s", name, output)
			}
		}
	}
	if selected != 2 {
		t.Fatalf("rollout selects %d repositories, want the two authorized canaries", selected)
	}
}
