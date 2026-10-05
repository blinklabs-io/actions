package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const supersedingPushGroup = "${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}"

type parsedConcurrency struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
}

func TestRenderWorkflowEmitsConcurrency(t *testing.T) {
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatalf("newWorkflowTemplate: %v", err)
	}
	wf := WorkflowConfig{
		DestinationFile:  "go-test.yml",
		WorkflowName:     "go-test",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
		Triggers:         map[string]interface{}{"pull_request": nil},
		Permissions:      map[string]string{"contents": "read"},
		Concurrency:      &Concurrency{Group: supersedingPushGroup, CancelInProgress: "true"},
	}

	out, err := renderWorkflow(tmpl, wf)
	if err != nil {
		t.Fatalf("renderWorkflow: %v", err)
	}

	var got parsedConcurrency
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("rendered workflow is not valid YAML: %v\n%s", err, out)
	}
	if got.Concurrency.Group != supersedingPushGroup {
		t.Errorf("group = %q, want %q\n%s", got.Concurrency.Group, supersedingPushGroup, out)
	}
	if !got.Concurrency.CancelInProgress {
		t.Errorf("cancel-in-progress = false, want true\n%s", out)
	}
	if strings.Index(string(out), "concurrency:") > strings.Index(string(out), "jobs:") {
		t.Errorf("concurrency must precede jobs\n%s", out)
	}
}

// A workflow that declares no concurrency must render byte-for-byte as before,
// or every managed repository's wrapper would be rewritten on the next sync.
func TestRenderWorkflowWithoutConcurrencyIsUnchanged(t *testing.T) {
	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatalf("newWorkflowTemplate: %v", err)
	}
	wf := WorkflowConfig{
		DestinationFile:  "go-test.yml",
		WorkflowName:     "go-test",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-go-test.yml@main",
		Triggers:         map[string]interface{}{"pull_request": nil},
		Permissions:      map[string]string{"contents": "read"},
	}

	out, err := renderWorkflow(tmpl, wf)
	if err != nil {
		t.Fatalf("renderWorkflow: %v", err)
	}
	if strings.Contains(string(out), "concurrency") {
		t.Errorf("unexpected concurrency block\n%s", out)
	}
	if !strings.Contains(string(out), "\n\njobs:\n") {
		t.Errorf("jobs block lost its blank-line separator\n%s", out)
	}
}

func TestRenderPipelineEmitsConcurrencyFromOneMember(t *testing.T) {
	members := goPipeline()
	members[0].Concurrency = &Concurrency{Group: supersedingPushGroup, CancelInProgress: "true"}

	out, err := renderPipeline(newPipelineTemplate(t), members)
	if err != nil {
		t.Fatalf("renderPipeline: %v", err)
	}

	var got parsedConcurrency
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("rendered pipeline is not valid YAML: %v\n%s", err, out)
	}
	if got.Concurrency.Group != supersedingPushGroup || !got.Concurrency.CancelInProgress {
		t.Errorf("concurrency = %+v\n%s", got.Concurrency, out)
	}
}

func TestRenderPipelineRejectsTwoConcurrencyDeclarations(t *testing.T) {
	members := goPipeline()
	members[0].Concurrency = &Concurrency{Group: "a", CancelInProgress: "true"}
	members[1].Concurrency = &Concurrency{Group: "b", CancelInProgress: "true"}

	if _, err := renderPipeline(newPipelineTemplate(t), members); err == nil ||
		!strings.Contains(err.Error(), "declare concurrency") {
		t.Fatalf("renderPipeline error = %v, want a two-declaration error", err)
	}
}

func TestRenderConcurrencyValidation(t *testing.T) {
	tests := []struct {
		name    string
		in      *Concurrency
		want    string
		wantErr string
	}{
		{name: "nil renders nothing", in: nil, want: ""},
		{
			name: "group only",
			in:   &Concurrency{Group: "release"},
			want: "concurrency:\n  group: \"release\"",
		},
		{
			name: "expression cancel",
			in:   &Concurrency{Group: "g", CancelInProgress: "${{ github.event_name == 'pull_request' }}"},
			want: "concurrency:\n  group: \"g\"\n  cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
		},
		{name: "empty group", in: &Concurrency{CancelInProgress: "true"}, wantErr: "non-empty group"},
		{name: "bad cancel value", in: &Concurrency{Group: "g", CancelInProgress: "yes"}, wantErr: "must be true, false or an expression"},
		{
			name:    "two expressions are not one expression",
			in:      &Concurrency{Group: "g", CancelInProgress: "${{ a }} && ${{ b }}"},
			wantErr: "must be true, false or an expression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderConcurrency(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("renderConcurrency: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyOverrideReplacesConcurrencyWithoutAliasing(t *testing.T) {
	wf := WorkflowConfig{Concurrency: &Concurrency{Group: "profile", CancelInProgress: "true"}}
	ov := WorkflowOverride{Concurrency: &Concurrency{Group: "repo", CancelInProgress: "false"}}

	applyOverride(&wf, ov)
	ov.Concurrency.Group = "mutated"

	if wf.Concurrency.Group != "repo" || wf.Concurrency.CancelInProgress != "false" {
		t.Errorf("concurrency = %+v, want the override's value", *wf.Concurrency)
	}
}

func TestCloneWorkflowDoesNotShareConcurrency(t *testing.T) {
	profile := WorkflowConfig{Concurrency: &Concurrency{Group: "profile", CancelInProgress: "true"}}

	clone := cloneWorkflow(profile)
	clone.Concurrency.Group = "changed"

	if profile.Concurrency.Group != "profile" {
		t.Errorf("clone mutated the shared profile: %+v", *profile.Concurrency)
	}
}
