package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

// fanoutWorkflowFile is the generated wrapper that announces a published
// release to the central release-fanout workflow.
const fanoutWorkflowFile = "release-fanout.yml"

// FanoutTarget is one downstream repository that pins a product's version.
// Type selects the handler under scripts/fanout/handlers; the remaining
// fields are the per-type inputs that handler needs.
type FanoutTarget struct {
	Repo    string   `yaml:"repo" json:"repo"`
	Type    string   `yaml:"type" json:"type"`
	Chart   string   `yaml:"chart,omitempty" json:"chart,omitempty"`
	Role    string   `yaml:"role,omitempty" json:"role,omitempty"`
	Package string   `yaml:"package,omitempty" json:"package,omitempty"`
	Refs    []string `yaml:"refs,omitempty" json:"refs,omitempty"`
}

// validateFanout rejects a release_fanout list that a handler could not act on.
func validateFanout(repo RepoConfig) error {
	seen := map[string]bool{}
	for i, t := range repo.ReleaseFanout {
		where := fmt.Sprintf("repository %q release_fanout[%d]", repo.Name, i)
		parts := strings.Split(t.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("%s: repo %q must be owner/name", where, t.Repo)
		}
		var field string
		var missing bool
		switch t.Type {
		case "helm-chart":
			field, missing = "chart", t.Chart == ""
		case "ansible-role":
			field, missing = "role", t.Role == ""
		case "cardano-up-package":
			field, missing = "package", t.Package == ""
		case "version-refs":
			field, missing = "refs", len(t.Refs) == 0
		default:
			return fmt.Errorf("%s: unknown type %q", where, t.Type)
		}
		if missing {
			return fmt.Errorf("%s: type %q is missing %s", where, t.Type, field)
		}
		for j, ref := range t.Refs {
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("%s: refs[%d] is empty", where, j)
			}
		}
		key := strings.ToLower(t.Repo) + "|" + t.Type
		if seen[key] {
			return fmt.Errorf("%s: duplicate %s target for %s", where, t.Type, t.Repo)
		}
		seen[key] = true
	}
	return nil
}

// injectFanoutWorkflow adds the dispatch wrapper to a repository that declares
// release_fanout, unless it already defines that file. The wrapper passes a
// secret, so it is pinned to ref; an empty ref injects nothing.
func injectFanoutWorkflow(repo *RepoConfig, ref string) {
	if ref == "" || len(repo.ReleaseFanout) == 0 || workflowExists(repo.Workflows, fanoutWorkflowFile) {
		return
	}
	repo.Workflows = append(repo.Workflows, WorkflowConfig{
		DestinationFile:  fanoutWorkflowFile,
		WorkflowName:     "release-fanout",
		ReusableWorkflow: "blinklabs-io/actions/.github/workflows/reuseable-release-dispatch.yml@" + ref,
		Triggers: map[string]interface{}{
			"release": map[string]interface{}{"types": []string{"published"}},
		},
		Permissions: map[string]string{"contents": "read"},
		Secrets:     map[string]string{"token": "ORG_PROJECT_PAT"},
	})
}

// generatedWorkflowHeader is the first line of every workflow the engine
// renders; its presence marks a file as safe for the engine to delete.
const generatedWorkflowHeader = "# Generated automatically by org-governance-bot. Do not edit manually."

// fanoutWrapperObsolete reports whether an existing release-fanout wrapper
// should be removed: the desired workflows no longer include it and the file
// carries the generated header, so a hand-written file of that name is kept.
func fanoutWrapperObsolete(desired []WorkflowConfig, existing []byte) bool {
	for _, wf := range desired {
		if wf.DestinationFile == fanoutWorkflowFile || wf.Pipeline == fanoutWorkflowFile {
			return false
		}
	}
	first, _, _ := strings.Cut(string(existing), "\n")
	return strings.TrimSuffix(first, "\r") == generatedWorkflowHeader
}

// stageObsoleteFanoutWrapper stages removal of a generated release-fanout
// wrapper left behind after fan-out was disabled for the repository.
func stageObsoleteFanoutWrapper(
	ctx context.Context,
	client *github.Client,
	owner, repo string,
	batch *commitBatch,
	desired []WorkflowConfig,
) {
	path := ".github/workflows/" + fanoutWorkflowFile
	fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		if !isNotFound(err) {
			fmt.Printf("  Error checking %s for cleanup: %v\n", path, err)
		}
		return
	}
	if fileContent == nil || fileContent.SHA == nil {
		return
	}
	existing, err := fileContent.GetContent()
	if err != nil || !fanoutWrapperObsolete(desired, []byte(existing)) {
		return
	}
	fmt.Printf("  Staging removal of %s...\n", path)
	batch.remove(path, "remove "+fanoutWorkflowFile)
}

var fanoutRefPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validateFanoutRef accepts an empty ref or a full lowercase commit SHA.
func validateFanoutRef(ref string) error {
	if ref != "" && !fanoutRefPattern.MatchString(ref) {
		return fmt.Errorf("release_fanout_dispatch_ref %q must be a 40-character lowercase commit SHA", ref)
	}
	return nil
}

var fanoutVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// normalizeFanoutVersion strips a leading "v" and accepts only plain release
// versions, so prereleases and build metadata never reach a target.
func normalizeFanoutVersion(v string) (string, error) {
	n := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !fanoutVersionPattern.MatchString(n) {
		return "", fmt.Errorf("%q is not a release version (want vX.Y.Z)", v)
	}
	return n, nil
}

// fanoutMatrixEntry is one apply job. Spec carries the target as compact JSON
// so handlers read their own fields with jq.
type fanoutMatrixEntry struct {
	ProductRepo string `json:"product_repo"`
	Product     string `json:"product"`
	Version     string `json:"version"`
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Spec        string `json:"spec"`
}

func fanoutPlan(cfg *Config, productRepo, version string) ([]fanoutMatrixEntry, error) {
	n, err := normalizeFanoutVersion(version)
	if err != nil {
		return nil, err
	}
	for _, r := range cfg.Repositories {
		if !strings.EqualFold(r.Name, productRepo) {
			continue
		}
		if len(r.ReleaseFanout) == 0 {
			return nil, fmt.Errorf("repository %q has no release_fanout", productRepo)
		}
		product := r.Name[strings.Index(r.Name, "/")+1:]
		entries := make([]fanoutMatrixEntry, 0, len(r.ReleaseFanout))
		for _, t := range r.ReleaseFanout {
			spec, err := json.Marshal(t)
			if err != nil {
				return nil, err
			}
			entries = append(entries, fanoutMatrixEntry{
				ProductRepo: r.Name,
				Product:     product,
				Version:     n,
				Repo:        t.Repo,
				Name:        t.Repo[strings.Index(t.Repo, "/")+1:],
				Type:        t.Type,
				Spec:        string(spec),
			})
		}
		return entries, nil
	}
	return nil, fmt.Errorf("repository %q is not in repos-config.yaml", productRepo)
}

func fanoutProducts(cfg *Config) []string {
	var out []string
	for _, r := range cfg.Repositories {
		if len(r.ReleaseFanout) > 0 {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

func loadFanoutConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if err := expandProfiles(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// runFanoutCommand handles the read-only fanout subcommands. handled is false
// for any other argument so main falls through to the sync engine.
func runFanoutCommand(args []string, stdout, stderr io.Writer) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "fanout-plan":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: fanout-plan <owner/repo> <version>")
			return 2, true
		}
		cfg, err := loadFanoutConfig("repos-config.yaml")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1, true
		}
		entries, err := fanoutPlan(cfg, args[1], args[2])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1, true
		}
		if err := json.NewEncoder(stdout).Encode(entries); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, true
		}
		return 0, true
	case "fanout-products":
		cfg, err := loadFanoutConfig("repos-config.yaml")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1, true
		}
		for _, p := range fanoutProducts(cfg) {
			fmt.Fprintln(stdout, p)
		}
		return 0, true
	}
	return 0, false
}
