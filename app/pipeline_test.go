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

// TestValidatePipelineRejectsUnconditionalWriteJob guards the dangerous half of
// the trigger union. Widening a check to more events is the point of a
// pipeline: lint and the tests have to run on a repository's pushes for a
// release to be able to depend on them. Widening a job that WRITES is not. A
// publish wrapper that only ever ran on a tag would, on joining a pipeline that
// also runs on pull requests, publish from pull requests using its own write
// grant.
func TestValidatePipelineRejectsUnconditionalWriteJob(t *testing.T) {
	members := goPipeline()
	members = append(members, WorkflowConfig{
		DestinationFile:  "publish.yml",
		Pipeline:         "ci.yml",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-library-release.yml@main",
		Triggers:         map[string]interface{}{"push": map[string]interface{}{"tags": []interface{}{"v*"}}},
		Permissions:      map[string]string{"contents": "write"},
		Needs:            []string{"go-test"},
	})

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for an unconditional write job in a pull_request pipeline")
	}
	if !strings.Contains(err.Error(), "write permission") {
		t.Errorf("error = %v, want it to name the write grant", err)
	}
}

// TestValidatePipelineAcceptsConditionalWriteJob is the shape this change
// exists to make possible: a release job sharing a file with the tests that
// gate it, restricted to the events it belongs to.
func TestValidatePipelineAcceptsConditionalWriteJob(t *testing.T) {
	members := goPipeline()
	members = append(members, WorkflowConfig{
		DestinationFile:  "publish.yml",
		Pipeline:         "ci.yml",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-library-release.yml@main",
		Triggers:         map[string]interface{}{"push": map[string]interface{}{"tags": []interface{}{"v*"}}},
		Permissions:      map[string]string{"contents": "write"},
		If:               "github.event_name == 'push' && github.ref_type == 'tag'",
		Needs:            []string{"go-test"},
	})

	if err := validatePipeline(members); err != nil {
		t.Fatalf("validatePipeline: %v", err)
	}
}

// TestValidatePipelineAcceptsNarrowedMember is the other half: a member that
// runs on fewer events than the pipeline is fine once it says which events it
// belongs to, which is what lets a publish job share a file with the tests that
// gate it.
func TestValidatePipelineAcceptsNarrowedMember(t *testing.T) {
	members := goPipeline()
	members[2].Triggers = map[string]interface{}{
		"push": map[string]interface{}{"branches": []interface{}{"main"}},
	}
	members[2].If = "github.event_name == 'push'"
	// ci-docker needs go-test, which is now conditional, so drop that edge
	// rather than trip the skipped-dependency check this test is not about.
	members[3].Needs = []string{"golangci-lint"}

	if err := validatePipeline(members); err != nil {
		t.Fatalf("validatePipeline: %v", err)
	}
}

// TestPipelineTriggersUnionsMembers checks the `on:` block covers every event
// any member ran on. A union that dropped one would stop that member running at
// all, which is a silent loss of a check rather than a visible failure.
func TestPipelineTriggersUnionsMembers(t *testing.T) {
	members := goPipeline()
	members[2].Triggers = map[string]interface{}{
		"push": map[string]interface{}{"branches": []interface{}{"main"}},
	}
	members[2].If = "github.event_name == 'push'"

	got := pipelineTriggers(members)
	if _, ok := got["pull_request"]; !ok {
		t.Errorf("union lost pull_request: %#v", got)
	}
	if _, ok := got["push"]; !ok {
		t.Errorf("union lost push: %#v", got)
	}
}

// TestPipelineTriggersUnfilteredWins checks that a filter on one member cannot
// narrow another member that had none. A bare `pull_request:` means every pull
// request; intersecting it with someone else's branches list would silence it
// on the branches it used to cover.
func TestPipelineTriggersUnfilteredWins(t *testing.T) {
	members := []WorkflowConfig{
		{
			DestinationFile: "golangci-lint.yml",
			Pipeline:        "ci.yml",
			Triggers:        map[string]interface{}{"pull_request": nil},
		},
		{
			DestinationFile: "ci-docker.yml",
			Pipeline:        "ci.yml",
			Triggers: map[string]interface{}{"pull_request": map[string]interface{}{
				"branches": []interface{}{"main"},
				"paths":    []interface{}{"Dockerfile"},
			}},
			If: "github.event_name == 'pull_request'",
		},
	}

	got := pipelineTriggers(members)
	if got["pull_request"] != nil {
		t.Errorf("pull_request = %#v, want nil (unfiltered)", got["pull_request"])
	}
}

// TestPipelineTriggersKeepsBranchAndTagPushes is the regression that rendering
// the real config exposed. `branches` and `tags` are OR'd ref selectors, and a
// push filter naming only `tags` does not match a branch push at all. Merging a
// lint wrapper that runs on main with a publish wrapper that runs on tags by
// intersecting filter keys drops `branches`, which stops lint running on main
// while the workflow still looks correct.
func TestPipelineTriggersKeepsBranchAndTagPushes(t *testing.T) {
	members := []WorkflowConfig{
		{
			DestinationFile: "golangci-lint.yml",
			Pipeline:        "ci.yml",
			Triggers: map[string]interface{}{
				"pull_request": nil,
				"push": map[string]interface{}{
					"branches": []interface{}{"main"},
					"tags":     []interface{}{"v*"},
				},
			},
		},
		{
			DestinationFile: "publish.yml",
			Pipeline:        "ci.yml",
			Triggers: map[string]interface{}{
				"push": map[string]interface{}{"tags": []interface{}{"v*.*.*"}},
			},
			If: "github.event_name == 'push' && github.ref_type == 'tag'",
		},
	}

	push, ok := pipelineTriggers(members)["push"].(map[string]interface{})
	if !ok {
		t.Fatalf("push filter = %#v, want a mapping", pipelineTriggers(members)["push"])
	}
	branches, ok := push["branches"].([]interface{})
	if !ok || len(branches) != 1 || branches[0] != "main" {
		t.Errorf("push.branches = %#v, want [main]; dropping it stops lint running on main", push["branches"])
	}
	tags, _ := push["tags"].([]interface{})
	if len(tags) != 2 {
		t.Errorf("push.tags = %#v, want both tag patterns", push["tags"])
	}
}

// TestPipelineTriggersDropsPathsWhenOneSideHasNone checks the other dimension:
// paths ANDs with the ref selection, so a member without a paths filter must
// not be narrowed by one that has it.
func TestPipelineTriggersDropsPathsWhenOneSideHasNone(t *testing.T) {
	members := []WorkflowConfig{
		{
			DestinationFile: "go-test.yml",
			Pipeline:        "ci.yml",
			Triggers: map[string]interface{}{
				"pull_request": map[string]interface{}{"branches": []interface{}{"main"}},
			},
		},
		{
			DestinationFile: "ci-docker.yml",
			Pipeline:        "ci.yml",
			Triggers: map[string]interface{}{
				"pull_request": map[string]interface{}{
					"branches": []interface{}{"main"},
					"paths":    []interface{}{"Dockerfile"},
				},
			},
			If: "github.event_name == 'pull_request'",
		},
	}

	pr, ok := pipelineTriggers(members)["pull_request"].(map[string]interface{})
	if !ok {
		t.Fatalf("pull_request = %#v, want a mapping", pipelineTriggers(members)["pull_request"])
	}
	if _, has := pr["paths"]; has {
		t.Errorf("pull_request kept paths %#v; go-test had none and would be narrowed", pr["paths"])
	}
}

// TestValidatePipelineRejectsNeedOnConditionalJob covers the trap that makes a
// green run meaningless: a skipped job skips its dependents, so a job needing
// one that is conditional on a different event never runs at all.
func TestValidatePipelineRejectsNeedOnConditionalJob(t *testing.T) {
	members := goPipeline()
	members[0].If = "github.event_name == 'pull_request'"

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for a job needing a differently-conditional job")
	}
	if !strings.Contains(err.Error(), "never run") {
		t.Errorf("error = %v, want it to explain the skip cascade", err)
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

// TestValidatePipelineRejectsDependencyCycle covers a cycle among several jobs,
// which the self-reference check does not catch: `a` needing `b` while `b`
// needs `a` contains no self-reference. Every job in a cycle waits for another
// in it, so none can start and GitHub rejects the whole file.
func TestValidatePipelineRejectsDependencyCycle(t *testing.T) {
	members := goPipeline()
	// golangci-lint <- nilaway is already there; close the loop.
	members[0].Needs = []string{"nilaway"}

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for a dependency cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to name the cycle", err)
	}
}

// TestValidatePipelineRejectsLongerCycle checks the detection is a graph walk
// rather than a pairwise check.
func TestValidatePipelineRejectsLongerCycle(t *testing.T) {
	members := goPipeline()
	members[0].Needs = []string{"ci-docker"} // lint <- docker <- go-test <- lint

	err := validatePipeline(members)
	if err == nil {
		t.Fatal("expected an error for a three-job dependency cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to name the cycle", err)
	}
}

// TestValidatePipelineAcceptsDiamond checks the walk does not mistake a job
// reached by two paths for a cycle, which would reject a legitimate fan-in.
func TestValidatePipelineAcceptsDiamond(t *testing.T) {
	members := goPipeline()
	members[3].Needs = []string{"go-test", "nilaway"}

	if err := validatePipeline(members); err != nil {
		t.Fatalf("validatePipeline rejected a diamond: %v", err)
	}
}
