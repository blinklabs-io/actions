package main

import (
	"encoding/hex"
	"os"
	"reflect"
	"sort"
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
	const (
		actionsPrefix = "blinklabs-io/actions/.github/workflows/"
		sharedPrefix  = "blinklabs-io/shared-actions/.github/workflows/"
		dockerSHA     = "d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
	)
	adderCalls := map[string]string{
		"conventional-commits.yml": "reuseable-conventional-commits.yml",
		"go-test.yml":              "reuseable-go-test.yml",
		"ci-docker.yml":            "reuseable-ci-docker-multiarch.yml",
		"publish.yml":              "reuseable-publish-desktop.yml",
		"release-fanout.yml":       "reuseable-release-dispatch.yml",
	}
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var source Config
	if err := yaml.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	fanoutProfiles := map[string]bool{
		"docker-standard-fanout":     true,
		"docker-actions-only-fanout": true,
	}
	fanoutTriggers := map[string]interface{}{
		"workflow_run": map[string]interface{}{
			"workflows": []interface{}{"publish"},
			"types":     []interface{}{"completed"},
		},
	}
	dockerRepos := make(map[string]bool)
	for _, repo := range source.Repositories {
		if !strings.HasPrefix(repo.Profile, "docker-") {
			continue
		}
		dockerRepos[repo.Name] = true
		if len(repo.ReleaseFanout) == 0 {
			continue
		}
		declared := false
		for _, wf := range source.Profiles[repo.Profile].Workflows {
			if wf.DestinationFile == "release-fanout.yml" {
				declared = true
			}
		}
		if !fanoutProfiles[repo.Profile] || !declared {
			t.Errorf("%s must declare release-fanout.yml in a Docker fanout profile", repo.Name)
		}
	}
	if len(dockerRepos) == 0 {
		t.Fatal("no Docker profile repositories found")
	}
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
		var dockerCalls map[string]string
		if dockerRepos[repo.Name] {
			dockerCalls = map[string]string{
				"conventional-commits.yml": "reuseable-conventional-commits.yml",
				"ci-docker.yml":            "reuseable-ci-docker-multiarch.yml",
				"publish.yml":              "reuseable-publish-docker-multiarch.yml",
			}
			if len(repo.ReleaseFanout) > 0 {
				dockerCalls["release-fanout.yml"] = "reuseable-release-dispatch.yml"
			}
		}
		for _, wf := range repo.Workflows {
			if dockerCalls != nil {
				if wf.DestinationFile == "release-fanout.yml" {
					if wf.Secrets["token"] != "ORG_PROJECT_PAT" {
						t.Errorf("%s release-fanout.yml must map token to ORG_PROJECT_PAT", repo.Name)
					}
					if !reflect.DeepEqual(wf.Triggers, fanoutTriggers) {
						t.Errorf("%s release-fanout.yml must run when publish completes", repo.Name)
					}
				}
				file, expected := dockerCalls[wf.DestinationFile]
				want := sharedPrefix + file + "@" + dockerSHA
				if !expected || wf.ReusableWorkflow != want {
					t.Errorf("%s %s must use the reviewed public Docker workflow %q, got %q", repo.Name, wf.DestinationFile, want, wf.ReusableWorkflow)
				}
				delete(dockerCalls, wf.DestinationFile)
			}
			if repo.Name == "blinklabs-io/adder" {
				file, expected := adderCalls[wf.DestinationFile]
				want := sharedPrefix + file + "@" + wantDesktopReusableSHA
				if !expected || wf.ReusableWorkflow != want {
					t.Errorf("Adder %s must use the reviewed public workflow %q, got %q", wf.DestinationFile, want, wf.ReusableWorkflow)
				}
				delete(adderCalls, wf.DestinationFile)
			}
			if !strings.HasPrefix(wf.ReusableWorkflow, actionsPrefix) && !strings.HasPrefix(wf.ReusableWorkflow, sharedPrefix) {
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
		if len(dockerCalls) != 0 {
			t.Errorf("%s is missing expected public Docker workflow calls: %v", repo.Name, dockerCalls)
		}
	}
	if len(adderCalls) != 0 {
		t.Errorf("Adder is missing expected public workflow calls: %v", adderCalls)
	}
	if found == 0 {
		t.Fatal("no Actions reusable workflows found in config")
	}
}

func TestMigratedGoWorkflowsUseReviewedPublicRevision(t *testing.T) {
	const (
		workflowPrefix = "blinklabs-io/shared-actions/.github/workflows/"
		workflowSHA    = "d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
	)
	wantCounts := map[string]int{
		"blinklabs-io/tx-submit-api":              5,
		"blinklabs-io/tx-submit-api-mirror":       5,
		"blinklabs-io/txtop":                      4,
		"blinklabs-io/cardano-txpump":             4,
		"blinklabs-io/shai":                       4,
		"blinklabs-io/plutigo":                    3,
		"blinklabs-io/gouroboros":                 3,
		"blinklabs-io/ouroboros-mock":             4,
		"blinklabs-io/bark":                       3,
		"blinklabs-io/cardano-models":             3,
		"blinklabs-io/merkle-patricia-forestry":   3,
		"blinklabs-io/dmq-lib":                    3,
		"blinklabs-io/go-scls":                    4,
		"blinklabs-io/dns-cli":                    2,
		"blinklabs-io/buidler-fest-2024-workshop": 2,
	}
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	profileCalls := map[string]string{
		"conventional-commits.yml": "reuseable-conventional-commits.yml",
		"go-test.yml":              "reuseable-go-test.yml",
	}
	for _, wf := range cfg.Profiles["go-ci-standard"].Workflows {
		file, expected := profileCalls[wf.DestinationFile]
		want := workflowPrefix + file + "@" + workflowSHA
		if !expected || wf.ReusableWorkflow != want || wf.PinToLatestChange {
			t.Errorf("go-ci-standard %s must use the reviewed public workflow %q, got %q", wf.DestinationFile, want, wf.ReusableWorkflow)
		}
		delete(profileCalls, wf.DestinationFile)
	}
	if len(profileCalls) != 0 {
		t.Errorf("go-ci-standard is missing public workflow calls: %v", profileCalls)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, repo := range cfg.Repositories {
		want, migrated := wantCounts[repo.Name]
		if !migrated {
			continue
		}
		delete(wantCounts, repo.Name)
		if len(repo.Workflows) != want {
			t.Errorf("%s has %d workflows, want %d", repo.Name, len(repo.Workflows), want)
		}
		for _, wf := range repo.Workflows {
			path, ref, ok := strings.Cut(wf.ReusableWorkflow, "@")
			if !ok || !strings.HasPrefix(path, workflowPrefix) || ref != workflowSHA || wf.PinToLatestChange {
				t.Errorf("%s %s must use %s at reviewed revision %s, got %q", repo.Name, wf.DestinationFile, workflowPrefix, workflowSHA, wf.ReusableWorkflow)
			}
		}
	}
	if len(wantCounts) != 0 {
		t.Errorf("migrated repositories missing from config: %v", wantCounts)
	}
}

func TestExplicitReleaseDispatchersUseReviewedContract(t *testing.T) {
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var source Config
	if err := yaml.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	isDispatcher := func(wf WorkflowConfig) bool {
		return wf.DestinationFile == fanoutWorkflowFile || strings.Contains(wf.ReusableWorkflow, "/reuseable-release-dispatch.yml@")
	}
	declared := map[string]bool{}
	for _, repo := range source.Repositories {
		for _, workflows := range [][]WorkflowConfig{repo.Workflows, source.Profiles[repo.Profile].Workflows} {
			for _, wf := range workflows {
				if isDispatcher(wf) {
					declared[repo.Name] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no explicit release dispatchers configured")
	}
	for _, name := range []string{"blinklabs-io/tx-submit-api", "blinklabs-io/tx-submit-api-mirror"} {
		if !declared[name] {
			t.Errorf("%s must declare its release dispatcher explicitly", name)
		}
	}
	wt, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	pt := newPipelineTemplate(t)
	for _, repo := range loadRealConfig(t).Repositories {
		if !declared[repo.Name] {
			continue
		}
		delete(declared, repo.Name)
		publishers := map[string]bool{}
		addPublisher := func(body []byte, err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			var rendered struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(body, &rendered); err != nil {
				t.Fatal(err)
			}
			if rendered.Name == "" {
				t.Fatal("rendered publisher has no workflow name")
			}
			publishers[rendered.Name] = true
		}
		standalone, pipelines := groupPipelines(repo.Workflows)
		for _, wf := range standalone {
			if strings.Contains(wf.ReusableWorkflow, "/reuseable-publish") {
				addPublisher(renderWorkflow(wt, wf))
			}
		}
		for _, members := range pipelines {
			for _, wf := range members {
				if strings.Contains(wf.ReusableWorkflow, "/reuseable-publish") {
					addPublisher(renderPipeline(pt, members))
					break
				}
			}
		}
		if len(publishers) == 0 {
			t.Fatalf("%s explicit dispatcher has no rendered publisher", repo.Name)
		}
		var names []string
		for name := range publishers {
			names = append(names, name)
		}
		sort.Strings(names)
		var workflowNames []interface{}
		for _, name := range names {
			workflowNames = append(workflowNames, name)
		}
		wantTriggers := map[string]interface{}{
			"workflow_run": map[string]interface{}{
				"workflows": workflowNames,
				"types":     []interface{}{"completed"},
			},
		}
		found := false
		for _, wf := range repo.Workflows {
			if !isDispatcher(wf) {
				continue
			}
			found = true
			sha := "d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
			if repo.Name == "blinklabs-io/adder" {
				sha = wantDesktopReusableSHA
			}
			want := "blinklabs-io/shared-actions/.github/workflows/reuseable-release-dispatch.yml@" + sha
			if wf.ReusableWorkflow != want || wf.PinToLatestChange {
				t.Errorf("%s %s must use the reviewed public dispatcher %q, got %q", repo.Name, wf.DestinationFile, want, wf.ReusableWorkflow)
			}
			if !reflect.DeepEqual(wf.Secrets, map[string]string{"token": "ORG_PROJECT_PAT"}) {
				t.Errorf("%s %s must map only token to ORG_PROJECT_PAT", repo.Name, wf.DestinationFile)
			}
			if !reflect.DeepEqual(wf.Permissions, map[string]string{"contents": "read"}) {
				t.Errorf("%s %s must grant only contents: read", repo.Name, wf.DestinationFile)
			}
			if !reflect.DeepEqual(wf.Triggers, wantTriggers) {
				t.Errorf("%s %s must run on workflow_run completed for rendered publishers %v", repo.Name, wf.DestinationFile, names)
			}
		}
		if !found {
			t.Errorf("%s is missing its explicit release dispatcher", repo.Name)
		}
	}
	if len(declared) != 0 {
		t.Errorf("explicit dispatcher repositories missing from expanded config: %v", declared)
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

func TestStandardProfileConventionalCommitsRevision(t *testing.T) {
	const want = "blinklabs-io/shared-actions/.github/workflows/reuseable-conventional-commits.yml@d90d0eef9c2d00ecac430e1b184ac2993ac768fb"
	data, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var source Config
	if err := yaml.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	pending := make(map[string]bool)
	for _, repo := range source.Repositories {
		if repo.Profile == "repository-standard" {
			pending[repo.Name] = true
		}
	}
	if len(pending) == 0 {
		t.Fatal("no standard profile repositories found")
	}
	for _, repo := range loadRealConfig(t).Repositories {
		if !pending[repo.Name] {
			continue
		}
		for _, workflow := range repo.Workflows {
			if workflow.DestinationFile != "conventional-commits.yml" {
				continue
			}
			if workflow.ReusableWorkflow != want || workflow.PinToLatestChange {
				t.Errorf("%s must use the reviewed standard profile workflow %q without dynamic pinning, got %q (pin_to_latest_change: %t)", repo.Name, want, workflow.ReusableWorkflow, workflow.PinToLatestChange)
			}
			delete(pending, repo.Name)
		}
	}
	for name := range pending {
		t.Errorf("%s has no standard profile Conventional Commits caller", name)
	}
}

func TestConfiguredConventionalCommitsPermissions(t *testing.T) {
	cfg := loadRealConfig(t)
	t.Setenv("GITHUB_REPOSITORY", "blinklabs-io/actions")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	if err := resolvePinnedWorkflowRefs(&cfg, func(string) (string, []byte, error) {
		return strings.Repeat("a", 40), []byte(callableWorkflowYAML), nil
	}); err != nil {
		t.Fatal(err)
	}
	wrapperTmpl, err := buildWorkflowTemplate("../templates/workflow.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	pipelineTmpl := newPipelineTemplate(t)

	check := func(repo string, workflow WorkflowConfig, workflows []WorkflowConfig) {
		t.Helper()
		path, ref, ok := strings.Cut(workflow.ReusableWorkflow, "@")
		if !ok || ref == "" || (path != "blinklabs-io/actions/.github/workflows/reuseable-conventional-commits.yml" && path != "blinklabs-io/shared-actions/.github/workflows/reuseable-conventional-commits.yml") {
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
