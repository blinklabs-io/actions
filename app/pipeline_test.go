package main

import (
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// newPipelineTemplate compiles the combined-workflow template with the same
// FuncMap and delimiters syncWorkflows uses, so these tests match production
// output exactly.
func newPipelineTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := buildWorkflowTemplate("../templates/pipeline.tmpl")
	if err != nil {
		t.Fatalf("buildWorkflowTemplate: %v", err)
	}
	return tmpl
}

// goPipeline is the shape this change exists to produce: lint gates the two
// expensive checks, which in turn gate the image build.
func goPipeline() []WorkflowConfig {
	triggers := map[string]interface{}{"pull_request": nil}
	return []WorkflowConfig{
		{
			DestinationFile:  "golangci-lint.yml",
			Pipeline:         "ci.yml",
			PipelineName:     "ci",
			ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-golangci-lint.yml@main",
			Triggers:         triggers,
		},
		{
			DestinationFile:  "nilaway.yml",
			Pipeline:         "ci.yml",
			ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-nilaway.yml@main",
			Triggers:         triggers,
			Needs:            []string{"golangci-lint"},
			Params:           map[string]string{"include-pkgs": "github.com/blinklabs-io/gouroboros"},
		},
		{
			DestinationFile:  "go-test.yml",
			Pipeline:         "ci.yml",
			ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
			Triggers:         triggers,
			Needs:            []string{"golangci-lint"},
		},
		{
			DestinationFile:  "ci-docker.yml",
			Pipeline:         "ci.yml",
			ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-ci-docker-multiarch.yml@main",
			Triggers:         triggers,
			Needs:            []string{"go-test"},
		},
	}
}

// parsedWorkflow is the part of a rendered workflow these tests read back.
type parsedWorkflow struct {
	Name string `yaml:"name"`
	Jobs map[string]struct {
		Needs       interface{}       `yaml:"needs"`
		Uses        string            `yaml:"uses"`
		With        map[string]string `yaml:"with"`
		Permissions map[string]string `yaml:"permissions"`
	} `yaml:"jobs"`
}

// TestRenderPipelineProducesOneFileWithChainedJobs is the core of the change:
// the entries that used to render a wrapper file each become jobs of one file,
// which is the only arrangement in which `needs:` can gate them at all.
func TestRenderPipelineProducesOneFileWithChainedJobs(t *testing.T) {
	out, err := renderPipeline(newPipelineTemplate(t), goPipeline())
	if err != nil {
		t.Fatalf("renderPipeline: %v", err)
	}

	var got parsedWorkflow
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("rendered pipeline is not valid YAML: %v\n%s", err, out)
	}

	if got.Name != "ci" {
		t.Errorf("workflow name = %q, want %q", got.Name, "ci")
	}
	if len(got.Jobs) != 4 {
		t.Fatalf("job count = %d, want 4\n%s", len(got.Jobs), out)
	}

	wantNeeds := map[string][]string{
		"golangci-lint": nil,
		"nilaway":       {"golangci-lint"},
		"go-test":       {"golangci-lint"},
		"ci-docker":     {"go-test"},
	}
	for id, want := range wantNeeds {
		job, ok := got.Jobs[id]
		if !ok {
			t.Errorf("no job %q in rendered pipeline", id)
			continue
		}
		if diff := needsOf(job.Needs); !equalStrings(diff, want) {
			t.Errorf("job %q needs = %v, want %v", id, diff, want)
		}
	}

	if got.Jobs["nilaway"].With["include-pkgs"] != "github.com/blinklabs-io/gouroboros" {
		t.Errorf("nilaway lost its include-pkgs param: %#v", got.Jobs["nilaway"].With)
	}
	if !strings.Contains(got.Jobs["go-test"].Uses, "reuseable-go-test.yml@main") {
		t.Errorf("go-test uses = %q", got.Jobs["go-test"].Uses)
	}
}

// TestRenderPipelineCarriesPerJobPermissions checks that a job needing an
// elevated grant keeps it. A combined file cannot use one workflow-level
// permissions block for jobs with different needs, so the grant moves onto the
// job; losing it would fail the release at the point it tries to write.
func TestRenderPipelineCarriesPerJobPermissions(t *testing.T) {
	triggers := map[string]interface{}{"push": map[string]interface{}{"tags": []interface{}{"v*.*.*"}}}
	members := []WorkflowConfig{
		{
			DestinationFile:  "publish.yml",
			Pipeline:         "publish.yml",
			PipelineName:     "publish",
			ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-library-release.yml@main",
			Triggers:         triggers,
			Permissions:      map[string]string{"contents": "write"},
		},
	}

	out, err := renderPipeline(newPipelineTemplate(t), members)
	if err != nil {
		t.Fatalf("renderPipeline: %v", err)
	}
	var got parsedWorkflow
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("rendered pipeline is not valid YAML: %v\n%s", err, out)
	}
	if got.Jobs["publish"].Permissions["contents"] != "write" {
		t.Errorf("publish job lost contents: write\n%s", out)
	}
}

// TestValidatePipelineRejectsMismatchedTriggers guards the one-on-block
// constraint. Silently taking the first member's triggers would stop the other
// members running at all, which is worse than refusing to render.
func TestValidatePipelineRejectsMismatchedTriggers(t *testing.T) {
	members := goPipeline()
	members[2].Triggers = map[string]interface{}{
		"push": map[string]interface{}{"branches": []interface{}{"main"}},
	}

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for members declaring different triggers")
	}
	if !strings.Contains(err.Error(), "different triggers") {
		t.Errorf("error = %v, want it to name the trigger mismatch", err)
	}
}

// TestValidatePipelineRejectsUnknownNeed catches a typo before it reaches a
// repository. GitHub fails the whole workflow when a job names an unresolvable
// dependency, so an unchecked typo takes out every job in the pipeline.
func TestValidatePipelineRejectsUnknownNeed(t *testing.T) {
	members := goPipeline()
	members[1].Needs = []string{"golangci-lnit"}

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for a need naming no job in the pipeline")
	}
	if !strings.Contains(err.Error(), "golangci-lnit") {
		t.Errorf("error = %v, want it to name the unresolved job", err)
	}
}

func TestValidatePipelineRejectsDuplicateJobID(t *testing.T) {
	members := goPipeline()
	members[1].JobID = "go-test"

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for two jobs sharing an id")
	}
	if !strings.Contains(err.Error(), "duplicate job id") {
		t.Errorf("error = %v, want it to name the duplicate", err)
	}
}

func TestValidatePipelineRejectsSelfNeed(t *testing.T) {
	members := goPipeline()
	members[1].Needs = []string{"nilaway"}

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for a job needing itself")
	}
}

// TestGroupPipelinesKeepsStandaloneEntries checks the change is additive: an
// entry with no pipeline still renders its own wrapper file, so repositories
// that have not been migrated are untouched.
func TestGroupPipelinesKeepsStandaloneEntries(t *testing.T) {
	workflows := append(goPipeline(), WorkflowConfig{
		DestinationFile:  "conventional-commits.yml",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-conventional-commits.yml@main",
		Triggers:         map[string]interface{}{"pull_request": nil},
	})

	standalone, pipelines := groupPipelines(workflows)
	if len(standalone) != 1 || standalone[0].DestinationFile != "conventional-commits.yml" {
		t.Errorf("standalone = %#v, want just conventional-commits.yml", standalone)
	}
	if len(pipelines) != 1 {
		t.Fatalf("pipeline count = %d, want 1", len(pipelines))
	}
	if len(pipelines[0]) != 4 {
		t.Errorf("pipeline member count = %d, want 4", len(pipelines[0]))
	}
	// Config order has to survive, or the generated file churns between runs
	// and the bot pushes a no-op commit every reconciliation.
	wantOrder := []string{"golangci-lint.yml", "nilaway.yml", "go-test.yml", "ci-docker.yml"}
	for i, wf := range pipelines[0] {
		if wf.DestinationFile != wantOrder[i] {
			t.Errorf("member %d = %q, want %q", i, wf.DestinationFile, wantOrder[i])
		}
	}
}

// TestSupersededWorkflowPathsListsFoldedWrappers is the check that keeps this
// change from making fan-out worse. A wrapper left behind keeps matching the
// same events and keeps starting its own run, so the repository would pay for
// the pipeline and every wrapper it replaced.
func TestSupersededWorkflowPathsListsFoldedWrappers(t *testing.T) {
	got := supersededWorkflowPaths(goPipeline())
	want := []string{
		".github/workflows/ci-docker.yml",
		".github/workflows/go-test.yml",
		".github/workflows/golangci-lint.yml",
		".github/workflows/nilaway.yml",
	}
	if !equalStrings(got, want) {
		t.Errorf("superseded = %v, want %v", got, want)
	}
}

// TestSupersededWorkflowPathsKeepsReusedFileName covers a pipeline that adopts
// one of its members' file names: that file is being replaced, not superseded,
// so deleting it would remove the pipeline that was just written.
func TestSupersededWorkflowPathsKeepsReusedFileName(t *testing.T) {
	members := goPipeline()
	for i := range members {
		members[i].Pipeline = "go-test.yml"
	}

	for _, path := range supersededWorkflowPaths(members) {
		if path == ".github/workflows/go-test.yml" {
			t.Fatalf("superseded list contains the pipeline's own file: %v",
				supersededWorkflowPaths(members))
		}
	}
}

func TestJobIDDefaultsToDestinationFileStem(t *testing.T) {
	tests := []struct {
		wf   WorkflowConfig
		want string
	}{
		{WorkflowConfig{DestinationFile: "go-test.yml"}, "go-test"},
		{WorkflowConfig{DestinationFile: "ci-docker.yaml"}, "ci-docker"},
		{WorkflowConfig{DestinationFile: "go-test.yml", JobID: "tests"}, "tests"},
	}
	for _, tc := range tests {
		if got := tc.wf.jobID(); got != tc.want {
			t.Errorf("jobID(%q) = %q, want %q", tc.wf.DestinationFile, got, tc.want)
		}
	}
}

// needsOf normalizes a decoded `needs:` value, which YAML gives back as a
// string for a single dependency and a sequence for several.
func needsOf(v interface{}) []string {
	switch n := v.(type) {
	case nil:
		return nil
	case string:
		return []string{n}
	case []interface{}:
		out := make([]string, 0, len(n))
		for _, e := range n {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
