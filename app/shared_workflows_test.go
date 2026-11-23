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

func TestSharedWorkflowRefsPreserveConfiguredSources(t *testing.T) {
	public := sharedWorkflowRepository + ".github/workflows/reuseable-go-test.yml@" + canarySharedRef
	cfg := Config{Repositories: []RepoConfig{{Workflows: []WorkflowConfig{
		{ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main"},
		{ReusableWorkflow: public}, {ReusableWorkflow: public},
		{ReusableWorkflow: "other/repo/.github/workflows/check.yml@main"},
	}}}}
	before, _ := json.Marshal(cfg)
	calls := 0
	if err := validateSharedWorkflowRefs(&cfg, func(ref, path string) ([]byte, error) {
		calls++
		if ref != canarySharedRef || path != ".github/workflows/reuseable-go-test.yml" {
			t.Fatalf("unexpected source %s %s", ref, path)
		}
		return []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("public source reads = %d, want one cached read", calls)
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("source validation changed configuration")
	}
}

func TestSharedWorkflowRefsAfterProfileAndFanout(t *testing.T) {
	cfg := Config{
		ReleaseFanoutDispatchRef: strings.Repeat("b", 40),
		Profiles: map[string]Profile{"public": {Workflows: []WorkflowConfig{{
			DestinationFile: "check.yml", ReusableWorkflow: sharedWorkflowRepository + ".github/workflows/reuseable-go-test.yml@" + canarySharedRef,
			Params: map[string]string{"run-build": "true"},
		}}}},
		Repositories: []RepoConfig{{Name: "org/canary", Profile: "public",
			ReleaseFanout: []FanoutTarget{{Repo: "org/consumer", Type: "version-refs", Refs: []string{"version.txt"}}}}},
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := validateSharedWorkflowRefs(&cfg, func(_, path string) ([]byte, error) {
		paths = append(paths, path)
		return []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != ".github/workflows/reuseable-go-test.yml" {
		t.Fatalf("unexpected public sources: %v", paths)
	}
	for _, wf := range cfg.Repositories[0].Workflows {
		if wf.DestinationFile == fanoutWorkflowFile && wf.ReusableWorkflow != "blinklabs-io/actions/.github/workflows/reuseable-release-dispatch.yml@"+cfg.ReleaseFanoutDispatchRef {
			t.Fatal("explicit public source changed default fanout source")
		}
	}
}

func TestSharedWorkflowRefsRejectInvalidSources(t *testing.T) {
	for _, test := range []struct {
		name, source, content string
		pin, readError        bool
	}{
		{name: "empty source", source: sharedWorkflowRepository},
		{name: "mutable ref", source: sharedWorkflowRepository + ".github/workflows/check.yml@main"},
		{name: "uppercase ref", source: sharedWorkflowRepository + ".github/workflows/check.yml@" + strings.Repeat("A", 40)},
		{name: "uppercase owner", source: "BlinkLabs-IO/shared-actions/.github/workflows/check.yml@main"},
		{name: "uppercase repository", source: "blinklabs-io/Shared-Actions/.github/workflows/check.yml@" + canarySharedRef},
		{name: "missing ref", source: sharedWorkflowRepository + ".github/workflows/check.yml"},
		{name: "escaping path", source: sharedWorkflowRepository + ".github/workflows/../check.yml@" + canarySharedRef},
		{name: "missing source", readError: true},
		{name: "not callable", content: "on: push\n"},
		{name: "invalid YAML", content: "on: [\n"},
		{name: "local pin resolution", pin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.source == "" {
				test.source = sharedWorkflowRepository + ".github/workflows/check.yml@" + canarySharedRef
			}
			cfg := Config{Repositories: []RepoConfig{{Workflows: []WorkflowConfig{
				{ReusableWorkflow: sharedWorkflowRepository + ".github/workflows/reuseable-go-test.yml@" + canarySharedRef},
				{ReusableWorkflow: test.source, PinToLatestChange: test.pin},
			}}}}
			before, _ := json.Marshal(cfg)
			err := validateSharedWorkflowRefs(&cfg, func(_ string, path string) ([]byte, error) {
				if path == ".github/workflows/reuseable-go-test.yml" {
					return []byte(callableWorkflowYAML), nil
				}
				if test.readError {
					return nil, errors.New("source not found")
				}
				if test.content == "" {
					return []byte(callableWorkflowYAML), nil
				}
				return []byte(test.content), nil
			})
			if err == nil {
				t.Fatal("invalid public source accepted")
			}
			after, _ := json.Marshal(cfg)
			if string(before) != string(after) {
				t.Fatal("invalid source changed configuration")
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
	if err := validateSharedWorkflowRefs(&cfg, func(string, string) ([]byte, error) {
		return []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, repo := range cfg.Repositories {
		isCanary := repo.Name == "blinklabs-io/go-bip39" || repo.Name == "blinklabs-io/adder"
		publicCalls := 0
		for _, wf := range repo.Workflows {
			if strings.HasPrefix(wf.ReusableWorkflow, sharedWorkflowRepository) {
				publicCalls++
				if !isCanary || !strings.HasSuffix(wf.ReusableWorkflow, "@"+canarySharedRef) {
					t.Fatalf("unexpected public source for %s: %s", repo.Name, wf.ReusableWorkflow)
				}
			} else if isCanary {
				t.Fatalf("canary %s still uses %s", repo.Name, wf.ReusableWorkflow)
			}
		}
		if !isCanary {
			continue
		}
		wantCalls := 3
		if repo.Name == "blinklabs-io/adder" {
			wantCalls = 5
		}
		if publicCalls != wantCalls {
			t.Fatalf("%s has %d public calls, want %d", repo.Name, publicCalls, wantCalls)
		}
		selected++
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
