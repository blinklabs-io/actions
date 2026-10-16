package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func fanoutRepo(targets ...FanoutTarget) RepoConfig {
	return RepoConfig{Name: "blinklabs-io/dingo", ReleaseFanout: targets}
}

func TestValidateFanout(t *testing.T) {
	good := []FanoutTarget{
		{Repo: "blinklabs-io/helm-charts", Type: "helm-chart", Chart: "charts/dingo"},
		{Repo: "blinklabs-io/ansible-cardano", Type: "ansible-role", Role: "dingo"},
		{Repo: "blinklabs-io/cardano-up-packages", Type: "cardano-up-package", Package: "dingo"},
		{Repo: "blinklabs-io/dingo", Type: "version-refs", Refs: []string{"README.md"}},
	}
	if err := validateFanout(fanoutRepo(good...)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := map[string][]FanoutTarget{
		"owner/name":      {{Repo: "helm-charts", Type: "helm-chart", Chart: "c"}},
		"unknown type":    {{Repo: "o/r", Type: "npm"}},
		"missing chart":   {{Repo: "o/r", Type: "helm-chart"}},
		"missing role":    {{Repo: "o/r", Type: "ansible-role"}},
		"missing package": {{Repo: "o/r", Type: "cardano-up-package"}},
		"missing refs":    {{Repo: "o/r", Type: "version-refs"}},
		"duplicate": {
			{Repo: "o/r", Type: "ansible-role", Role: "a"},
			{Repo: "O/R", Type: "ansible-role", Role: "b"},
		},
	}
	for want, targets := range bad {
		err := validateFanout(fanoutRepo(targets...))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

const testFanoutRef = "0123456789abcdef0123456789abcdef01234567"

func TestInjectFanoutWorkflowEmptyRefInjectsNothing(t *testing.T) {
	repo := fanoutRepo(FanoutTarget{Repo: "o/r", Type: "version-refs", Refs: []string{"README.md"}})
	injectFanoutWorkflow(&repo, "")
	if len(repo.Workflows) != 0 {
		t.Fatalf("empty ref must not inject a wrapper: %+v", repo.Workflows)
	}
}

func TestValidateFanoutRef(t *testing.T) {
	for _, ok := range []string{"", testFanoutRef} {
		if err := validateFanoutRef(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"abc123",
		strings.ToUpper(testFanoutRef),
		strings.Repeat("g", 40),
		testFanoutRef + "0",
		"main",
	} {
		err := validateFanoutRef(bad)
		if err == nil || !strings.Contains(err.Error(), "release_fanout_dispatch_ref") {
			t.Errorf("%q must be rejected naming the key, got %v", bad, err)
		}
	}
}

func TestExpandProfilesRejectsInvalidFanoutRef(t *testing.T) {
	cfg := Config{ReleaseFanoutDispatchRef: "main"}
	if err := expandProfiles(&cfg); err == nil {
		t.Fatal("invalid release_fanout_dispatch_ref must fail expansion")
	}
}

func TestRealConfigWrapperIsPinnedOnceRefIsSet(t *testing.T) {
	const otherRef = "fedcba9876543210fedcba9876543210fedcba98"
	for name, fileRef := range map[string]string{"unset": "", "preset": otherRef} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../repos-config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var cfg Config
			if err := yaml.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.ReleaseFanoutDispatchRef = fileRef
			cfg.ReleaseFanoutDispatchRef = testFanoutRef
			if err := expandProfiles(&cfg); err != nil {
				t.Fatal(err)
			}
			for _, r := range cfg.Repositories {
				if r.Name != "blinklabs-io/dingo" {
					continue
				}
				var wrappers []WorkflowConfig
				for _, wf := range r.Workflows {
					if wf.DestinationFile == fanoutWorkflowFile {
						wrappers = append(wrappers, wf)
					}
				}
				if len(wrappers) != 1 {
					t.Fatalf("want exactly one %s, got %d", fanoutWorkflowFile, len(wrappers))
				}
				if !strings.HasSuffix(wrappers[0].ReusableWorkflow, "@"+testFanoutRef) || len(wrappers[0].Secrets) == 0 {
					t.Fatalf("unexpected wrapper: %+v", wrappers[0])
				}
				return
			}
			t.Fatal("dingo not in real config")
		})
	}
}

func TestInjectFanoutWorkflow(t *testing.T) {
	repo := fanoutRepo(FanoutTarget{Repo: "o/r", Type: "version-refs", Refs: []string{"README.md"}})
	injectFanoutWorkflow(&repo, testFanoutRef)
	injectFanoutWorkflow(&repo, testFanoutRef) // idempotent
	if len(repo.Workflows) != 1 {
		t.Fatalf("want exactly one workflow, got %d", len(repo.Workflows))
	}
	wf := repo.Workflows[0]
	if wf.DestinationFile != fanoutWorkflowFile ||
		!strings.HasSuffix(wf.ReusableWorkflow, "reuseable-release-dispatch.yml@"+testFanoutRef) ||
		wf.Secrets["token"] != "ORG_PROJECT_PAT" ||
		wf.Permissions["contents"] != "read" {
		t.Fatalf("unexpected wrapper: %+v", wf)
	}

	tmpl, err := newWorkflowTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderWorkflow(tmpl, wf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"release:", "- published", "token: ${{ secrets.ORG_PROJECT_PAT }}", "contents: read"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered wrapper missing %q:\n%s", want, out)
		}
	}

	none := RepoConfig{Name: "o/none"}
	injectFanoutWorkflow(&none, testFanoutRef)
	if len(none.Workflows) != 0 {
		t.Fatal("repo without release_fanout must not get a wrapper")
	}
}

func TestExpandProfilesInjectsAndValidatesFanout(t *testing.T) {
	var cfg Config
	raw := `
repositories:
  - name: blinklabs-io/dingo
    release_fanout:
      - repo: blinklabs-io/ansible-cardano
        type: ansible-role
        role: dingo
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ReleaseFanoutDispatchRef = testFanoutRef
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories[0].Workflows) != 1 {
		t.Fatalf("wrapper not injected: %+v", cfg.Repositories[0].Workflows)
	}

	bad := Config{Repositories: []RepoConfig{fanoutRepo(FanoutTarget{Repo: "o/r", Type: "nope"})}}
	if err := expandProfiles(&bad); err == nil {
		t.Fatal("invalid release_fanout must fail expansion")
	}
}

func TestNormalizeFanoutVersion(t *testing.T) {
	for in, want := range map[string]string{"v0.70.0": "0.70.0", "0.70.0": "0.70.0", " v1.2.3 ": "1.2.3"} {
		got, err := normalizeFanoutVersion(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"v0.70.0-rc1", "latest", "0.70", "", "v1.2.3.4", "v1.2.3+meta"} {
		if _, err := normalizeFanoutVersion(in); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
}

func planConfig() *Config {
	return &Config{Repositories: []RepoConfig{
		{Name: "blinklabs-io/dingo", ReleaseFanout: []FanoutTarget{
			{Repo: "blinklabs-io/ansible-cardano", Type: "ansible-role", Role: "dingo"},
			{Repo: "blinklabs-io/dingo", Type: "version-refs", Refs: []string{"README.md", "examples/**/*.md"}},
		}},
		{Name: "blinklabs-io/other"},
	}}
}

func TestFanoutPlan(t *testing.T) {
	entries, err := fanoutPlan(planConfig(), "blinklabs-io/dingo", "v0.70.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	e := entries[0]
	if e.Product != "dingo" || e.ProductRepo != "blinklabs-io/dingo" || e.Version != "0.70.0" ||
		e.Repo != "blinklabs-io/ansible-cardano" || e.Name != "ansible-cardano" || e.Type != "ansible-role" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	var spec FanoutTarget
	if err := json.Unmarshal([]byte(e.Spec), &spec); err != nil || spec.Role != "dingo" {
		t.Fatalf("spec not target JSON: %q (%v)", e.Spec, err)
	}

	for _, bad := range []string{"v0.70.0-rc1", "latest", "0.70"} {
		if _, err := fanoutPlan(planConfig(), "blinklabs-io/dingo", bad); err == nil {
			t.Errorf("version %q must fail the plan", bad)
		}
	}
	if _, err := fanoutPlan(planConfig(), "blinklabs-io/other", "v1.0.0"); err == nil {
		t.Error("repo without release_fanout must fail the plan")
	}
	if _, err := fanoutPlan(planConfig(), "blinklabs-io/unknown", "v1.0.0"); err == nil {
		t.Error("unknown repo must fail the plan")
	}
	if _, err := fanoutPlan(planConfig(), "BlinkLabs-IO/Dingo", "v1.0.0"); err != nil {
		t.Errorf("repo match must be case-insensitive: %v", err)
	}
}

func TestFanoutProducts(t *testing.T) {
	got := fanoutProducts(planConfig())
	if len(got) != 1 || got[0] != "blinklabs-io/dingo" {
		t.Fatalf("got %v", got)
	}
}

func TestRunFanoutCommand(t *testing.T) {
	if _, handled := runFanoutCommand([]string{"something-else"}, &bytes.Buffer{}, &bytes.Buffer{}); handled {
		t.Fatal("unknown subcommands must fall through to the sync engine")
	}
	if code, handled := runFanoutCommand([]string{"fanout-plan", "only-one-arg"}, &bytes.Buffer{}, &bytes.Buffer{}); !handled || code == 0 {
		t.Fatalf("bad usage must fail: code=%d handled=%v", code, handled)
	}
}

func TestFanoutRealConfigPlansDingo(t *testing.T) {
	raw, err := os.ReadFile("../repos-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := expandProfiles(&cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := fanoutPlan(&cfg, "blinklabs-io/dingo", "v0.70.0")
	if err != nil {
		t.Fatalf("dingo must be plannable from the real config: %v", err)
	}
	types := map[string]bool{}
	for _, e := range entries {
		types[e.Type] = true
	}
	for _, want := range []string{"ansible-role", "helm-chart", "cardano-up-package"} {
		if !types[want] {
			t.Errorf("real config missing %s target for dingo", want)
		}
	}
}
