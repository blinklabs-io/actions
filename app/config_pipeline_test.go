package main

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadRealConfig parses the repository's actual repos-config.yaml and expands
// profiles, exactly as main() does before syncing.
func loadRealConfig(t *testing.T) Config {
	t.Helper()

	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatalf("read repos-config.yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse repos-config.yaml: %v", err)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatalf("expandProfiles: %v", err)
	}
	return cfg
}

func TestConfiguredDependabotManifestsValidate(t *testing.T) {
	cfg := loadRealConfig(t)
	if err := validateDependabotConfigs(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
}

// TestConfiguredPipelinesRender renders every pipeline the real config declares
// and checks the result is a workflow GitHub would accept.
//
// The unit tests above cover the generator with fixtures; this one covers the
// config. A pipeline whose members disagree about triggers, or whose needs name
// a job that is not in the group, produces a file that fails for every job in
// it, and the bot would push that to a live repository. Failing here instead
// costs nothing.
func TestConfiguredPipelinesRender(t *testing.T) {
	cfg := loadRealConfig(t)
	tmpl := newPipelineTemplate(t)

	pipelineCount := 0
	for _, repo := range cfg.Repositories {
		_, pipelines := groupPipelines(repo.Workflows)
		for _, members := range pipelines {
			pipelineCount++
			out, err := renderPipeline(tmpl, members)
			if err != nil {
				t.Errorf("%s pipeline %s: %v", repo.Name, members[0].Pipeline, err)
				continue
			}
			var parsed parsedWorkflow
			if err := yaml.Unmarshal(out, &parsed); err != nil {
				t.Errorf("%s pipeline %s is not valid YAML: %v\n%s",
					repo.Name, members[0].Pipeline, err, out)
				continue
			}
			if len(parsed.Jobs) != len(members) {
				t.Errorf("%s pipeline %s rendered %d jobs, want %d",
					repo.Name, members[0].Pipeline, len(parsed.Jobs), len(members))
			}
			if parsed.Name == "" {
				t.Errorf("%s pipeline %s rendered an empty workflow name",
					repo.Name, members[0].Pipeline)
			}
		}
	}

	if pipelineCount == 0 {
		t.Error("no pipelines configured; this check would pass vacuously")
	}
}

// TestGoTestSkipsPushToMain keeps the Go suite from re-running on a push to
// main for repositories whose pull requests already ran it. The org shares 20
// concurrent jobs, so every merge re-running the suite costs every other
// repository queue time.
func TestGoTestSkipsPushToMain(t *testing.T) {
	cfg := loadRealConfig(t)
	want := map[string]bool{"blinklabs-io/bursa": true, "blinklabs-io/gouroboros": true}
	for _, repo := range cfg.Repositories {
		if !want[repo.Name] {
			continue
		}
		delete(want, repo.Name)
		found := false
		for _, workflow := range repo.Workflows {
			if workflow.DestinationFile != "go-test.yml" {
				continue
			}
			found = true
			if push, ok := workflow.Triggers["push"].(map[string]interface{}); ok {
				if _, ok := push["branches"]; ok {
					t.Errorf("%s go-test runs on push to branches %v", repo.Name, push["branches"])
				}
			}
			if workflow.Concurrency == nil || workflow.Concurrency.CancelInProgress != "true" {
				t.Errorf("%s go-test does not cancel superseded pull request runs", repo.Name)
			}
			if _, ok := workflow.Params["cross-build-max-parallel"]; ok {
				t.Errorf("%s go-test sets cross-build-max-parallel, which the single cross-build job ignores", repo.Name)
			}
		}
		if !found {
			t.Errorf("%s go-test workflow is missing", repo.Name)
		}
	}
	for name := range want {
		t.Errorf("%s is missing from repos-config.yaml", name)
	}
}

// TestConfiguredPipelinesGate checks that every configured pipeline actually
// stages something. Grouping workflows without a needs edge changes nothing
// that matters: GitHub's concurrency limit counts jobs, not workflow runs, so
// N wrappers and one file with N parallel jobs cost the same. The gain comes
// only from a dependency that keeps the expensive jobs from starting.
func TestConfiguredPipelinesGate(t *testing.T) {
	cfg := loadRealConfig(t)

	for _, repo := range cfg.Repositories {
		_, pipelines := groupPipelines(repo.Workflows)
		for _, members := range pipelines {
			gated := false
			for _, wf := range members {
				if len(wf.Needs) > 0 {
					gated = true
					break
				}
			}
			if !gated {
				t.Errorf(
					"%s pipeline %s has no needs edge; grouping without one "+
						"costs the same runners and gains no fail-fast, so "+
						"leave those entries as separate wrappers",
					repo.Name,
					members[0].Pipeline,
				)
			}
		}
	}
}

func TestConfiguredPipelinePathFiltersMatch(t *testing.T) {
	cfg := loadRealConfig(t)
	for _, repo := range cfg.Repositories {
		_, pipelines := groupPipelines(repo.Workflows)
		for _, members := range pipelines {
			if err := validatePipelinePathFilters(members); err != nil {
				t.Errorf("%s pipeline %s path filters: %v", repo.Name, members[0].Pipeline, err)
			}
		}
	}
}

func TestConfiguredPrivilegedWorkflowsUseSafeRefs(t *testing.T) {
	cfg := loadRealConfig(t)
	t.Setenv("GITHUB_REPOSITORY", "blinklabs-io/actions")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	if err := resolvePinnedWorkflowRefs(&cfg, func(string) (string, []byte, error) {
		return strings.Repeat("a", 40), []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, repo := range cfg.Repositories {
		for _, wf := range repo.Workflows {
			if !strings.HasPrefix(
				wf.ReusableWorkflow,
				"blinklabs-io/actions/.github/workflows/",
			) {
				continue
			}
			found++
			_, ref, ok := strings.Cut(wf.ReusableWorkflow, "@")
			if !ok {
				t.Errorf(
					"%s %s has no ref in %q",
					repo.Name,
					wf.DestinationFile,
					wf.ReusableWorkflow,
				)
				continue
			}
			_, shaErr := hex.DecodeString(ref)
			fullSHA := len(ref) == 40 && shaErr == nil && strings.ToLower(ref) == ref
			mustPin := len(wf.Secrets) > 0
			for _, permission := range wf.Permissions {
				if permission == "write" {
					mustPin = true
					break
				}
			}
			if (mustPin && !fullSHA) || (!mustPin && ref != "main" && !fullSHA) {
				t.Errorf(
					"%s %s uses reusable workflow ref %q; credentials or write permissions require a full commit SHA, otherwise use main or a full commit SHA",
					repo.Name,
					wf.DestinationFile,
					wf.ReusableWorkflow,
				)
			}
		}
	}
	if found == 0 {
		t.Fatal("no Actions reusable workflows found in config")
	}
}

func TestTxpumpReusableWorkflowsUseSafeRefs(t *testing.T) {
	const (
		repoName       = "blinklabs-io/cardano-txpump"
		workflowPrefix = "blinklabs-io/actions/.github/workflows/"
		wantWorkflows  = 4
	)
	cfg := loadRealConfig(t)
	foundRepo := false
	foundWorkflows := 0
	for _, repo := range cfg.Repositories {
		if repo.Name != repoName {
			continue
		}
		foundRepo = true
		for _, wf := range repo.Workflows {
			if !strings.HasPrefix(wf.ReusableWorkflow, workflowPrefix) {
				continue
			}
			foundWorkflows++
			_, ref, ok := strings.Cut(wf.ReusableWorkflow, "@")
			_, shaErr := hex.DecodeString(ref)
			fullSHA := len(ref) == 40 && shaErr == nil && strings.ToLower(ref) == ref
			publishWorkflow := strings.Contains(wf.ReusableWorkflow, "/reuseable-publish.yml@")
			if !ok || (publishWorkflow && !fullSHA) || (!publishWorkflow && ref != "main") {
				want := "@main for CI workflows and a full commit SHA for publish"
				t.Errorf("%s %s uses reusable workflow ref %q, want %s", repo.Name, wf.DestinationFile, wf.ReusableWorkflow, want)
			}
		}
	}
	if !foundRepo {
		t.Fatalf("repository %s not found in config", repoName)
	}
	if foundWorkflows != wantWorkflows {
		t.Errorf("txpump has %d actions reusable workflows, want %d", foundWorkflows, wantWorkflows)
	}
}

func TestConfiguredCustomPublishBuildsUseMatrixTarget(t *testing.T) {
	cfg := loadRealConfig(t)
	for _, repo := range cfg.Repositories {
		for _, wf := range repo.Workflows {
			command := wf.Params["binary-build-command"]
			if command == "" {
				continue
			}
			if !strings.Contains(command, `GOOS="$MATRIX_OS" GOARCH="$MATRIX_ARCH" go build`) {
				t.Errorf("%s %s custom binary build does not use the matrix target", repo.Name, wf.DestinationFile)
			}
		}
	}
}

// TestConfiguredPipelinesSupersedeTheirWrappers checks that every entry folded
// into a pipeline is listed for removal. A wrapper left in a repository keeps
// matching the same events and keeps starting its own run, so a half-applied
// migration raises fan-out rather than lowering it.
func TestConfiguredPipelinesSupersedeTheirWrappers(t *testing.T) {
	cfg := loadRealConfig(t)

	for _, repo := range cfg.Repositories {
		_, pipelines := groupPipelines(repo.Workflows)
		for _, members := range pipelines {
			superseded := make(map[string]bool)
			for _, path := range supersededWorkflowPaths(members, repo.Workflows) {
				superseded[path] = true
			}
			for _, wf := range members {
				path := ".github/workflows/" + wf.DestinationFile
				if wf.DestinationFile == wf.Pipeline {
					continue // replaced in place, not superseded
				}
				if !superseded[path] {
					t.Errorf(
						"%s: %s was folded into %s but is not scheduled for removal",
						repo.Name,
						wf.DestinationFile,
						wf.Pipeline,
					)
				}
			}
		}
	}
}

func TestConfiguredConventionalCommitsPermissions(t *testing.T) {
	cfg := loadRealConfig(t)
	wrapperTmpl, err := buildWorkflowTemplate("../templates/workflow.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	pipelineTmpl := newPipelineTemplate(t)

	check := func(repo string, workflow WorkflowConfig, workflows []WorkflowConfig) {
		t.Helper()
		const prefix = "blinklabs-io/actions/.github/workflows/reuseable-conventional-commits.yml@"
		if !strings.HasPrefix(workflow.ReusableWorkflow, prefix) || len(workflow.ReusableWorkflow) == len(prefix) {
			t.Errorf("%s: unexpected conventional commits workflow %q", repo, workflow.ReusableWorkflow)
			return
		}
		var out []byte
		var err error
		jobID := "orchestrate"
		if workflow.Pipeline == "" {
			out, err = renderWorkflow(wrapperTmpl, workflow)
		} else {
			_, pipelines := groupPipelines(workflows)
			for _, members := range pipelines {
				if members[0].Pipeline == workflow.Pipeline {
					out, err = renderPipeline(pipelineTmpl, members)
					break
				}
			}
			jobID = workflow.jobID()
		}
		if err != nil || len(out) == 0 {
			t.Errorf("%s: render caller: %v", repo, err)
			return
		}
		var rendered struct {
			Permissions map[string]string `yaml:"permissions"`
			Jobs        map[string]struct {
				Uses        string            `yaml:"uses"`
				Permissions map[string]string `yaml:"permissions"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(out, &rendered); err != nil {
			t.Errorf("%s: parse rendered caller: %v", repo, err)
			return
		}
		job, ok := rendered.Jobs[jobID]
		if !ok || job.Uses != workflow.ReusableWorkflow {
			t.Errorf("%s: caller lost reusable workflow reference", repo)
		}
		permissions := rendered.Permissions
		if workflow.Pipeline != "" {
			permissions = job.Permissions
		} else if job.Permissions != nil {
			t.Errorf("%s: unexpected job-level permissions: %v", repo, job.Permissions)
		}
		if len(permissions) != 1 || permissions["contents"] != "read" {
			t.Errorf("%s: caller permissions = %v, want contents: read only", repo, permissions)
		}
	}

	checked := 0
	for _, repo := range cfg.Repositories {
		for _, workflow := range repo.Workflows {
			if workflow.DestinationFile != "conventional-commits.yml" {
				continue
			}
			checked++
			check(repo.Name, workflow, repo.Workflows)
		}
	}
	if checked == 0 {
		t.Fatal("no conventional commits callers checked")
	}

	// Exercise the pipeline path even while all configured callers are standalone.
	workflow := WorkflowConfig{
		DestinationFile:  "conventional-commits.yml",
		Pipeline:         "ci.yml",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-conventional-commits.yml@main",
		Triggers:         map[string]interface{}{"pull_request": nil},
		Permissions:      map[string]string{"contents": "read"},
	}
	check("pipeline fixture", workflow, []WorkflowConfig{workflow})
}
