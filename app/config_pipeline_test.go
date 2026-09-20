package main

import (
	"os"
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
			for _, path := range supersededWorkflowPaths(members) {
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
