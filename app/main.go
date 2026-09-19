package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Configuration types
// ---------------------------------------------------------------------------

type Config struct {
	Profiles     map[string]Profile `yaml:"profiles"`
	Discovery    Discovery          `yaml:"discovery"`
	Repositories []RepoConfig       `yaml:"repositories"`
}

// Discovery configures optional org-wide repository auto-discovery. When
// disabled (the default), the engine manages exactly the repositories listed in
// repos-config.yaml. When enabled, the engine additionally enumerates the
// organization's repositories and manages any that contain a profile marker
// file, so a new repository created from a template is picked up automatically
// without editing repos-config.yaml.
type Discovery struct {
	// Enabled turns auto-discovery on. Absent/false preserves the original
	// config-only behavior exactly.
	Enabled bool `yaml:"enabled"`
	// Organization is the GitHub org to scan (e.g. blinklabs-io).
	Organization string `yaml:"organization"`
	// MarkerPath is the in-repo path of the profile marker file. Defaults to
	// .blinklabs/profile.yml when empty.
	MarkerPath string `yaml:"marker_path"`
	// Topic, when set, restricts discovery to repositories carrying this GitHub
	// topic. Empty means every non-archived repository is considered.
	Topic string `yaml:"topic"`
}

// RepoMarker is the schema of the in-repository profile marker file. It carries
// the same profile/vars/overrides a repository would otherwise declare inline in
// repos-config.yaml, minus the repository name (which is derived from the repo
// the marker was found in).
type RepoMarker struct {
	Profile   string                      `yaml:"profile"`
	Vars      map[string]string           `yaml:"vars"`
	Overrides map[string]WorkflowOverride `yaml:"overrides"`
}

// defaultMarkerPath is used when Discovery.MarkerPath is empty.
const defaultMarkerPath = ".blinklabs/profile.yml"

// Profile is a reusable template for a class of repositories. A repository that
// references a profile inherits its settings, collaborators, branch protection
// and workflows, supplying only per-repo values via `vars` (and, for genuinely
// special cases, `overrides`).
type Profile struct {
	Settings         RepoSettings       `yaml:"settings"`
	Collaborators    []Collaborator     `yaml:"collaborators"`
	BranchProtection []BranchProtection `yaml:"branch_protection"`
	Workflows        []WorkflowConfig   `yaml:"workflows"`
}

// WorkflowOverride patches a single profile workflow for one repository. Only
// the fields that differ from the profile need to be set: triggers, matrix,
// and secrets replace the profile value wholesale, while permissions and params
// are merged into (and may override individual keys of) the profile's values.
type WorkflowOverride struct {
	Triggers    map[string]interface{} `yaml:"triggers"`
	Matrix      map[string]interface{} `yaml:"matrix"`
	Permissions map[string]string      `yaml:"permissions"`
	Params      map[string]string      `yaml:"params"`
	Secrets     map[string]string      `yaml:"secrets"`
}

type RepoConfig struct {
	Name    string `yaml:"name"`
	Profile string `yaml:"profile"`
	// Vars supplies the per-repo values substituted into ${var} placeholders in
	// the referenced profile's workflow params.
	Vars map[string]string `yaml:"vars"`
	// Overrides patches individual profile workflows, keyed by destination_file.
	Overrides        map[string]WorkflowOverride `yaml:"overrides"`
	Settings         RepoSettings                `yaml:"settings"`
	Collaborators    []Collaborator              `yaml:"collaborators"`
	BranchProtection []BranchProtection          `yaml:"branch_protection"`
	Workflows        []WorkflowConfig            `yaml:"workflows"`
}

type RepoSettings struct {
	// DeleteBranchOnMerge is a pointer so an explicit `false` in YAML is
	// distinguishable from an unset field (nil). This lets the profile guard in
	// expandProfiles detect an explicit override of `false`, and lets
	// syncRepoSettings leave the setting untouched when it is not specified.
	DeleteBranchOnMerge *bool `yaml:"delete_branch_on_merge"`
}

type Collaborator struct {
	Username   string `yaml:"username"`
	Permission string `yaml:"permission"`
}

type BranchProtection struct {
	Branch                         string   `yaml:"branch"`
	RequiredStatusChecks           []string `yaml:"required_status_checks"`
	RequireUpToDate                bool     `yaml:"require_up_to_date"`
	BypassPullRequest              bool     `yaml:"bypass_pull_request"`
	BypassActorsUsers              []string `yaml:"bypass_actors_users"`
	BypassActorsTeams              []string `yaml:"bypass_actors_teams"`
	BypassActorsApps               []string `yaml:"bypass_actors_apps"`
	RequiredApprovingReviewCount   int      `yaml:"required_approving_review_count"`
	DismissStaleReviews            bool     `yaml:"dismiss_stale_reviews"`
	RequireCodeOwnerReviews        bool     `yaml:"require_code_owner_reviews"`
	EnforceAdmins                  bool     `yaml:"enforce_admins"`
	RequireLinearHistory           bool     `yaml:"require_linear_history"`
	RequiredConversationResolution bool     `yaml:"required_conversation_resolution"`
	AllowForcePushes               bool     `yaml:"allow_force_pushes"`
	AllowDeletions                 bool     `yaml:"allow_deletions"`
	LockBranch                     bool     `yaml:"lock_branch"`
}

type WorkflowConfig struct {
	DestinationFile  string                 `yaml:"destination_file"`
	WorkflowName     string                 `yaml:"workflow_name"`
	ReusableWorkflow string                 `yaml:"reusable_workflow"`
	Triggers         map[string]interface{} `yaml:"triggers"`
	Matrix           map[string]interface{} `yaml:"matrix"`
	Permissions      map[string]string      `yaml:"permissions"`
	Params           map[string]string      `yaml:"params"`
	Secrets          map[string]string      `yaml:"secrets"`

	// Pipeline, when set, is the destination file of a combined workflow that
	// this entry becomes one job of, instead of a wrapper file of its own.
	//
	// GitHub starts every workflow file matching an event at once, and `needs:`
	// cannot name a job in another file, so separate wrappers can only ever run
	// flat: a repository whose lint, nilaway and test wrappers are three files
	// spends three runners on a change that does not compile. Grouping them into
	// one file makes `needs:` available, so the cheap check gates the expensive
	// ones and a failure costs one runner instead of all of them.
	Pipeline string `yaml:"pipeline"`

	// PipelineName is the `name:` of the combined workflow. Only one member need
	// set it; members leaving it empty inherit it. Ignored when Pipeline is
	// empty, where WorkflowName names the file's workflow as before.
	PipelineName string `yaml:"pipeline_name"`

	// JobID is this entry's job key inside its pipeline, and what other members
	// name in Needs. Defaults to DestinationFile without its extension.
	JobID string `yaml:"job_id"`

	// Needs lists JobIDs in the same pipeline that must succeed before this job
	// starts.
	Needs []string `yaml:"needs"`

	// PipelineTriggers is the combined workflow's `on:` block. Exactly one
	// member of a pipeline declares it.
	//
	// A pipeline that spans events needs this. Wrappers that ran on different
	// events could never gate each other, which is how a tag came to publish a
	// release while the lint and test wrappers for the same commit were still
	// running or already failing. Bringing them into one file means one `on:`
	// covering every event the pipeline serves, with If narrowing each job to
	// the events it belongs to.
	PipelineTriggers map[string]interface{} `yaml:"pipeline_triggers"`

	// If is the job's `if:` condition, used to restrict a job to some of the
	// events its pipeline runs on.
	//
	// Note for anyone adding a job: a skipped job's dependents skip too, so a
	// job must not need one that is conditional on a different event.
	If string `yaml:"if"`
}

// jobID returns the entry's job key inside a pipeline, defaulting to the
// destination file without its extension so a config that sets only `pipeline`
// and `needs` still reads naturally.
func (w WorkflowConfig) jobID() string {
	if w.JobID != "" {
		return w.JobID
	}
	base := w.DestinationFile
	if idx := strings.LastIndex(base, "."); idx > 0 {
		base = base[:idx]
	}
	return base
}

// templateData is the value passed into the workflow template.
type templateData struct {
	WorkflowName     string
	ReusableWorkflow string
	Params           map[string]string
	Secrets          map[string]string
	Permissions      map[string]string
	MatrixYAML       string
	TriggersYAML     string
}

// pipelineJobData is one caller job inside a combined workflow.
type pipelineJobData struct {
	JobID            string
	NeedsYAML        string
	If               string
	ReusableWorkflow string
	Params           map[string]string
	Secrets          map[string]string
	Permissions      map[string]string
	MatrixYAML       string
}

// pipelineData is the value passed into the pipeline template.
type pipelineData struct {
	WorkflowName string
	TriggersYAML string
	Jobs         []pipelineJobData
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	ctx := context.Background()

	// GH_TOKEN is injected by the central runner action via the GitHub App
	// token exchange step.
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		fmt.Println("Error: GH_TOKEN environment variable required")
		os.Exit(1)
	}

	client := github.NewClient(nil).WithAuthToken(token)

	configFile, err := os.ReadFile("repos-config.yaml")
	if err != nil {
		fmt.Printf("Failed to read config: %v\n", err)
		os.Exit(1)
	}

	var cfg Config
	if err := yaml.Unmarshal(configFile, &cfg); err != nil {
		fmt.Printf("Failed to parse config: %v\n", err)
		os.Exit(1)
	}

	// Optional org-wide auto-discovery. Disabled by default, so absent config
	// leaves the original config-only behavior untouched.
	if cfg.Discovery.Enabled {
		fmt.Printf("🔍 Discovering managed repositories in %s\n", cfg.Discovery.Organization)
		discovered, err := discoverRepositories(ctx, client.Repositories, cfg.Discovery)
		if err != nil {
			fmt.Printf("Failed to discover repositories: %v\n", err)
			os.Exit(1)
		}
		mergeDiscovered(&cfg, discovered)
		fmt.Printf("   Discovered %d marker-managed repositories\n", len(discovered))
	}

	if err := expandProfiles(&cfg); err != nil {
		fmt.Printf("Failed to expand profiles: %v\n", err)
		os.Exit(1)
	}

	for _, repo := range cfg.Repositories {
		owner, repoName := parseRepoString(repo.Name)
		fmt.Printf("⚡ Starting sync for %s/%s\n", owner, repoName)

		// Observe-Compare-Act loop per repo
		syncRepoSettings(ctx, client, owner, repoName, repo.Settings)
		syncCollaborators(ctx, client, owner, repoName, repo.Collaborators)
		if err := syncBranchProtection(ctx, client, owner, repoName, repo.BranchProtection); err != nil {
			fmt.Printf("Error syncing branch protection for %s/%s: %v\n", owner, repoName, err)
			os.Exit(1)
		}
		syncWorkflows(ctx, client, owner, repoName, repo.Workflows)
		removeObsoleteIssueCloseWorkflows(ctx, client, owner, repoName)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseRepoString(fullName string) (string, string) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		fmt.Printf("Error: invalid repo name %q — expected \"owner/repo\"\n", fullName)
		os.Exit(1)
	}
	return parts[0], parts[1]
}

// ---------------------------------------------------------------------------
// Profile expansion
// ---------------------------------------------------------------------------

// varPattern matches ${name} placeholders. GitHub Actions expressions use the
// ${{ ... }} form; because the character after "${" is "{" (not a letter), they
// are never matched here and pass through untouched.
var varPattern = regexp.MustCompile(`\$\{([a-zA-Z][a-zA-Z0-9_-]*)\}`)

// substituteVars replaces every ${name} placeholder in s with vars[name],
// returning an error if any referenced variable is undefined.
func substituteVars(s string, vars map[string]string) (string, error) {
	var missing []string
	out := varPattern.ReplaceAllStringFunc(s, func(match string) string {
		key := varPattern.FindStringSubmatch(match)[1]
		val, ok := vars[key]
		if !ok {
			missing = append(missing, key)
			return match
		}
		return val
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// cloneWorkflow returns a copy of w with independent Params and Permissions maps
// so callers can merge overrides and substitute vars without mutating the shared
// profile.
func cloneWorkflow(w WorkflowConfig) WorkflowConfig {
	out := w
	if w.Params != nil {
		params := make(map[string]string, len(w.Params))
		for k, v := range w.Params {
			params[k] = v
		}
		out.Params = params
	}
	if w.Permissions != nil {
		perms := make(map[string]string, len(w.Permissions))
		for k, v := range w.Permissions {
			perms[k] = v
		}
		out.Permissions = perms
	}
	return out
}

// applyOverride patches a workflow with a per-repo override. Triggers, matrix
// and secrets replace the profile value wholesale; permissions and params are
// merged (override keys win), so a repo can add a single permission or param
// without restating the profile's defaults.
func applyOverride(wf *WorkflowConfig, ov WorkflowOverride) {
	if ov.Triggers != nil {
		wf.Triggers = ov.Triggers
	}
	if ov.Matrix != nil {
		wf.Matrix = ov.Matrix
	}
	if ov.Secrets != nil {
		wf.Secrets = ov.Secrets
	}
	if len(ov.Permissions) > 0 {
		if wf.Permissions == nil {
			wf.Permissions = make(map[string]string, len(ov.Permissions))
		}
		for k, v := range ov.Permissions {
			wf.Permissions[k] = v
		}
	}
	if len(ov.Params) > 0 {
		if wf.Params == nil {
			wf.Params = make(map[string]string, len(ov.Params))
		}
		for k, v := range ov.Params {
			wf.Params[k] = v
		}
	}
}

func workflowExists(workflows []WorkflowConfig, destinationFile string) bool {
	for _, wf := range workflows {
		if wf.DestinationFile == destinationFile {
			return true
		}
	}
	return false
}

// expandProfiles resolves every repository that references a profile into a
// fully-materialized RepoConfig: profile settings/collaborators/branch
// protection are inherited, profile workflows are cloned, per-repo overrides
// are applied, and ${var} placeholders are substituted from the repo's vars.
// Repositories without a profile are left untouched.
func expandProfiles(cfg *Config) error {
	for i := range cfg.Repositories {
		if err := expandRepo(cfg, &cfg.Repositories[i]); err != nil {
			return err
		}
	}
	return nil
}

// expandRepo materializes a single profile-based repository in place, applying
// the profile's inheritance, per-repo overrides and ${var} substitution. A
// repository without a profile is left untouched. It is factored out of
// expandProfiles so discovered repositories can be expanded and validated one
// at a time — a bad marker then skips only that repository instead of aborting
// the whole run (see mergeDiscovered).
func expandRepo(cfg *Config, repo *RepoConfig) error {
	if repo.Profile == "" {
		return nil
	}

	profile, ok := cfg.Profiles[repo.Profile]
	if !ok {
		return fmt.Errorf("repository %q references unknown profile %q", repo.Name, repo.Profile)
	}
	if len(repo.Workflows) > 0 {
		return fmt.Errorf("repository %q sets both profile %q and explicit workflows", repo.Name, repo.Profile)
	}
	// Profile-based repos inherit settings/collaborators/branch_protection
	// from the profile; setting them directly would be silently discarded,
	// so reject it explicitly (mirrors the workflows check above). Because
	// DeleteBranchOnMerge is a *bool, an explicit `delete_branch_on_merge:
	// false` yields a non-nil pointer and is caught here too.
	if repo.Settings != (RepoSettings{}) {
		return fmt.Errorf("repository %q sets both profile %q and explicit settings; profile-based repos inherit settings from the profile", repo.Name, repo.Profile)
	}
	if len(repo.Collaborators) > 0 {
		return fmt.Errorf("repository %q sets both profile %q and explicit collaborators; profile-based repos inherit collaborators from the profile", repo.Name, repo.Profile)
	}
	if len(repo.BranchProtection) > 0 {
		return fmt.Errorf("repository %q sets both profile %q and explicit branch_protection; profile-based repos inherit branch_protection from the profile", repo.Name, repo.Profile)
	}

	repo.Settings = profile.Settings
	repo.Collaborators = profile.Collaborators
	repo.BranchProtection = profile.BranchProtection

	workflows := make([]WorkflowConfig, 0, len(profile.Workflows))
	for _, pwf := range profile.Workflows {
		wf := cloneWorkflow(pwf)
		if ov, ok := repo.Overrides[wf.DestinationFile]; ok {
			applyOverride(&wf, ov)
		}
		for k, v := range wf.Params {
			substituted, err := substituteVars(v, repo.Vars)
			if err != nil {
				return fmt.Errorf("repository %q workflow %q param %q: %w", repo.Name, wf.DestinationFile, k, err)
			}
			wf.Params[k] = substituted
		}
		workflows = append(workflows, wf)
	}

	for dest := range repo.Overrides {
		if !workflowExists(workflows, dest) {
			return fmt.Errorf("repository %q override targets unknown workflow %q", repo.Name, dest)
		}
	}

	repo.Workflows = workflows
	repo.Profile = ""
	repo.Vars = nil
	repo.Overrides = nil
	return nil
}

// ---------------------------------------------------------------------------
// Repository auto-discovery
// ---------------------------------------------------------------------------

// discoveryClient is the subset of *github.RepositoriesService that discovery
// needs. Defining it as an interface keeps discoverRepositories unit-testable
// with a fake, and *github.RepositoriesService satisfies it directly.
type discoveryClient interface {
	ListByOrg(ctx context.Context, org string, opts *github.RepositoryListByOrgOptions) ([]*github.Repository, *github.Response, error)
	GetContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error)
}

// parseMarker decodes a profile marker file into a RepoConfig for the given
// "owner/repo". The marker must name a profile; vars and overrides are optional.
func parseMarker(data []byte, fullName string) (RepoConfig, error) {
	var m RepoMarker
	if err := yaml.Unmarshal(data, &m); err != nil {
		return RepoConfig{}, fmt.Errorf("parse marker for %q: %w", fullName, err)
	}
	if strings.TrimSpace(m.Profile) == "" {
		return RepoConfig{}, fmt.Errorf("marker for %q does not specify a profile", fullName)
	}
	return RepoConfig{
		Name:      fullName,
		Profile:   m.Profile,
		Vars:      m.Vars,
		Overrides: m.Overrides,
	}, nil
}

// isNotFound reports whether err is a GitHub 404 (e.g. a repository without a
// marker file), which discovery treats as "not managed" rather than a failure.
func isNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

// discoverRepositories enumerates the organization's repositories and returns a
// RepoConfig for each non-archived repository that contains a valid profile
// marker file. Repositories without a marker (404) are skipped silently; a
// repository whose marker fails to parse is skipped with a warning so one bad
// marker cannot halt the whole run. Any other error reading a marker (e.g. 403,
// 500, network) is propagated so the run does not silently leave repositories
// unmanaged. When d.Topic is set, only repositories carrying that topic are
// considered.
func discoverRepositories(ctx context.Context, client discoveryClient, d Discovery) ([]RepoConfig, error) {
	if d.Organization == "" {
		return nil, errors.New("discovery enabled but no organization configured")
	}
	markerPath := d.MarkerPath
	if markerPath == "" {
		markerPath = defaultMarkerPath
	}

	opts := &github.RepositoryListByOrgOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var discovered []RepoConfig
	for {
		repos, resp, err := client.ListByOrg(ctx, d.Organization, opts)
		if err != nil {
			return nil, fmt.Errorf("list repositories for org %q: %w", d.Organization, err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			if d.Topic != "" && !hasTopic(r.Topics, d.Topic) {
				continue
			}
			fullName := r.GetFullName()
			owner, name := r.GetOwner().GetLogin(), r.GetName()
			if owner == "" || name == "" {
				continue
			}
			content, _, _, err := client.GetContents(ctx, owner, name, markerPath, nil)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				// A non-404 failure means we cannot tell whether the repo is
				// managed; fail loudly rather than silently leaving it stale.
				return nil, fmt.Errorf("read marker for %s: %w", fullName, err)
			}
			if content == nil {
				// marker_path resolved to a directory (or otherwise has no file
				// content); GetContent would panic, so treat it as no marker.
				fmt.Printf("  ⚠️ discovery: marker path for %s is not a file; skipping\n", fullName)
				continue
			}
			decoded, err := content.GetContent()
			if err != nil {
				fmt.Printf("  ⚠️ discovery: decoding marker for %s: %v\n", fullName, err)
				continue
			}
			repoCfg, err := parseMarker([]byte(decoded), fullName)
			if err != nil {
				fmt.Printf("  ⚠️ discovery: %v\n", err)
				continue
			}
			discovered = append(discovered, repoCfg)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return discovered, nil
}

func hasTopic(topics []string, want string) bool {
	for _, t := range topics {
		if t == want {
			return true
		}
	}
	return false
}

// mergeDiscovered appends discovered repositories to cfg.Repositories. A
// repository already listed explicitly in repos-config.yaml takes precedence and
// the discovered entry is skipped, so teams can migrate to markers incrementally
// while pinning special cases in the central config. Repository names are
// compared case-insensitively because GitHub owner/repo slugs are
// case-insensitive. Each discovered repository is expanded in isolation, so a
// bad marker (an unknown profile, an undefined variable, or an override
// targeting an unknown workflow) is skipped with a warning rather than aborting
// the whole run — a typo in one newly onboarded repository cannot block
// governance for every repository. Explicit config still fails fast in
// expandProfiles.
func mergeDiscovered(cfg *Config, discovered []RepoConfig) {
	seen := make(map[string]bool, len(cfg.Repositories))
	for _, r := range cfg.Repositories {
		seen[strings.ToLower(r.Name)] = true
	}
	for _, r := range discovered {
		key := strings.ToLower(r.Name)
		if seen[key] {
			fmt.Printf("  discovery: %s is pinned in repos-config.yaml; skipping marker\n", r.Name)
			continue
		}
		if _, ok := cfg.Profiles[r.Profile]; !ok {
			fmt.Printf("  ⚠️ discovery: %s references unknown profile %q; skipping\n", r.Name, r.Profile)
			continue
		}
		// Expand the discovered repository in isolation so a bad marker (an
		// undefined variable or an override targeting an unknown workflow)
		// skips only this repository rather than aborting governance for every
		// repository. Working on a copy leaves the original slice and the shared
		// profile untouched if expansion fails.
		expanded := r
		if err := expandRepo(cfg, &expanded); err != nil {
			fmt.Printf("  ⚠️ discovery: %s: %v; skipping\n", r.Name, err)
			continue
		}
		seen[key] = true
		cfg.Repositories = append(cfg.Repositories, expanded)
	}
}

// ---------------------------------------------------------------------------
// Sync functions
// ---------------------------------------------------------------------------

func syncBranchProtection(ctx context.Context, client *github.Client, owner, repo string, protections []BranchProtection) error {
	for _, bp := range protections {
		// Observe current state first so unmodeled settings can be preserved.
		current, _, getErr := client.Repositories.GetBranchProtection(ctx, owner, repo, bp.Branch)

		// Build desired protection request.
		req := &github.ProtectionRequest{
			EnforceAdmins:                  bp.EnforceAdmins,
			RequireLinearHistory:           github.Bool(bp.RequireLinearHistory),
			AllowForcePushes:               github.Bool(bp.AllowForcePushes),
			AllowDeletions:                 github.Bool(bp.AllowDeletions),
			RequiredConversationResolution: github.Bool(bp.RequiredConversationResolution),
			LockBranch:                     github.Bool(bp.LockBranch),
		}

		// Preserve existing push restrictions to avoid silently clearing them
		// on every unrelated update. The GitHub API replaces the full protection
		// object on PUT, so omitting Restrictions would remove all push rules.
		if getErr == nil && current.Restrictions != nil {
			r := current.Restrictions
			userLogins := make([]string, 0, len(r.Users))
			for _, u := range r.Users {
				userLogins = append(userLogins, u.GetLogin())
			}
			teamSlugs := make([]string, 0, len(r.Teams))
			for _, t := range r.Teams {
				teamSlugs = append(teamSlugs, t.GetSlug())
			}
			appSlugs := make([]string, 0, len(r.Apps))
			for _, a := range r.Apps {
				appSlugs = append(appSlugs, a.GetSlug())
			}
			req.Restrictions = &github.BranchRestrictionsRequest{
				Users: userLogins,
				Teams: teamSlugs,
				Apps:  appSlugs,
			}
		}

		if len(bp.RequiredStatusChecks) > 0 {
			req.RequiredStatusChecks = &github.RequiredStatusChecks{
				Strict:   bp.RequireUpToDate,
				Contexts: &bp.RequiredStatusChecks,
			}
		}

		if !bp.BypassPullRequest {
			prReq := &github.PullRequestReviewsEnforcementRequest{
				RequiredApprovingReviewCount: bp.RequiredApprovingReviewCount,
				DismissStaleReviews:          bp.DismissStaleReviews,
				RequireCodeOwnerReviews:      bp.RequireCodeOwnerReviews,
			}
			if len(bp.BypassActorsUsers) > 0 || len(bp.BypassActorsTeams) > 0 || len(bp.BypassActorsApps) > 0 {
				prReq.BypassPullRequestAllowancesRequest = &github.BypassPullRequestAllowancesRequest{
					Users: bp.BypassActorsUsers,
					Teams: bp.BypassActorsTeams,
					Apps:  bp.BypassActorsApps,
				}
			}
			req.RequiredPullRequestReviews = prReq
		}

		// Drift check: skip update if current state already matches desired.
		if getErr == nil {
			hasPR := current.GetRequiredPullRequestReviews() != nil
			wantsPR := !bp.BypassPullRequest

			var currentChecks []string
			currentStrict := false
			if c := current.GetRequiredStatusChecks(); c != nil {
				if c.Contexts != nil {
					currentChecks = *c.Contexts
				}
				currentStrict = c.Strict
			}

			currentEnforceAdmins := false
			if ea := current.GetEnforceAdmins(); ea != nil {
				currentEnforceAdmins = ea.Enabled
			}
			currentAllowForcePushes := false
			if afp := current.GetAllowForcePushes(); afp != nil {
				currentAllowForcePushes = afp.Enabled
			}
			currentAllowDeletions := false
			if ad := current.GetAllowDeletions(); ad != nil {
				currentAllowDeletions = ad.Enabled
			}
			currentLinear := false
			if rl := current.GetRequireLinearHistory(); rl != nil {
				currentLinear = rl.Enabled
			}
			currentConvRes := false
			if cr := current.GetRequiredConversationResolution(); cr != nil {
				currentConvRes = cr.Enabled
			}
			currentLock := false
			if lb := current.GetLockBranch(); lb != nil {
				currentLock = lb.GetEnabled()
			}

			// Only check PR-review subfields when PR reviews are desired;
			// when bypassed those fields are not sent in the request.
			prReviewsMatch := true
			if wantsPR {
				currentApprovals := 0
				currentDismiss := false
				currentCodeOwner := false
				var currentBypassUsers, currentBypassTeams, currentBypassApps []string
				if r := current.GetRequiredPullRequestReviews(); r != nil {
					currentApprovals = r.RequiredApprovingReviewCount
					currentDismiss = r.DismissStaleReviews
					currentCodeOwner = r.RequireCodeOwnerReviews
					if b := r.BypassPullRequestAllowances; b != nil {
						for _, u := range b.Users {
							currentBypassUsers = append(currentBypassUsers, u.GetLogin())
						}
						for _, t := range b.Teams {
							currentBypassTeams = append(currentBypassTeams, t.GetSlug())
						}
						for _, a := range b.Apps {
							currentBypassApps = append(currentBypassApps, a.GetSlug())
						}
					}
				}
				prReviewsMatch = currentApprovals == bp.RequiredApprovingReviewCount &&
					currentDismiss == bp.DismissStaleReviews &&
					currentCodeOwner == bp.RequireCodeOwnerReviews &&
					statusChecksEqual(currentBypassUsers, bp.BypassActorsUsers) &&
					statusChecksEqual(currentBypassTeams, bp.BypassActorsTeams) &&
					statusChecksEqual(currentBypassApps, bp.BypassActorsApps)
			}

			if hasPR == wantsPR &&
				statusChecksEqual(currentChecks, bp.RequiredStatusChecks) &&
				currentStrict == bp.RequireUpToDate &&
				prReviewsMatch &&
				currentEnforceAdmins == bp.EnforceAdmins &&
				currentAllowForcePushes == bp.AllowForcePushes &&
				currentAllowDeletions == bp.AllowDeletions &&
				currentLinear == bp.RequireLinearHistory &&
				currentConvRes == bp.RequiredConversationResolution &&
				currentLock == bp.LockBranch {
				fmt.Printf("✅ Branch protection on %s already matches desired state.\n", bp.Branch)
				continue
			}
		}

		action := "Updating"
		if getErr != nil {
			action = "Creating"
		}
		fmt.Printf("  %s branch protection on %s...\n", action, bp.Branch)
		if _, _, updateErr := client.Repositories.UpdateBranchProtection(ctx, owner, repo, bp.Branch, req); updateErr != nil {
			return fmt.Errorf("branch %s: %s protection: %w", bp.Branch, action, updateErr)
		}
		fmt.Printf("✅ Branch protection %s on %s\n", action, bp.Branch)
	}
	return nil
}

// renderTriggers marshals the triggers map to an indented YAML block suitable
// for embedding under the `on:` key. When triggers is nil/empty it defaults to
// the canonical issues.closed + workflow_dispatch pair.
func renderTriggers(triggers map[string]interface{}) (string, error) {
	if len(triggers) == 0 {
		triggers = map[string]interface{}{
			"issues": map[string]interface{}{
				"types": []string{"closed"},
			},
			"workflow_dispatch": nil,
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(triggers); err != nil {
		return "", err
	}
	_ = enc.Close()
	raw := strings.TrimRight(buf.String(), "\n")
	// yaml.v3 encodes nil values as "null"; GitHub Actions expects bare keys.
	raw = strings.ReplaceAll(raw, ": null", ":")
	// Indent every line by 2 spaces so it nests correctly under `on:`.
	lines := strings.Split(raw, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n"), nil
}

// renderMatrix marshals an optional strategy.matrix block for wrapper jobs.
// When matrix is nil/empty, the template omits the strategy block entirely.
func renderMatrix(matrix map[string]interface{}) (string, error) {
	if len(matrix) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(matrix); err != nil {
		return "", err
	}
	_ = enc.Close()
	raw := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(raw, "\n")
	for i, l := range lines {
		lines[i] = "        " + l
	}
	return strings.Join(lines, "\n"), nil
}

// statusChecksEqual reports whether two context slices contain the same set of
// check names, independent of order (GitHub may return them in any sequence).
func statusChecksEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := make([]string, len(a))
	bc := make([]string, len(b))
	copy(ac, a)
	copy(bc, b)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func syncRepoSettings(ctx context.Context, client *github.Client, owner, repo string, settings RepoSettings) {
	current, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		fmt.Printf("  Error getting repo details: %v\n", err)
		return
	}

	// An unset (nil) DeleteBranchOnMerge means the setting is unmanaged; leave
	// the repository's current value untouched.
	if settings.DeleteBranchOnMerge == nil {
		fmt.Println("✅ No repository settings to reconcile.")
		return
	}

	if current.GetDeleteBranchOnMerge() != *settings.DeleteBranchOnMerge {
		fmt.Println("  Updating repository settings...")
		update := &github.Repository{
			DeleteBranchOnMerge: github.Bool(*settings.DeleteBranchOnMerge),
		}
		_, _, _ = client.Repositories.Edit(ctx, owner, repo, update)
	} else {
		fmt.Println("✅ Repository settings already match desired state.")
	}
}

func syncCollaborators(ctx context.Context, client *github.Client, owner, repo string, collaborators []Collaborator) {
	for _, c := range collaborators {
		perm, _, err := client.Repositories.GetPermissionLevel(ctx, owner, repo, c.Username)
		if err == nil && perm.GetPermission() == c.Permission {
			fmt.Printf("✅ Collaborator %s already has permission %s\n", c.Username, c.Permission)
			continue
		}
		fmt.Printf("  Updating collaborator %s to permission %s\n", c.Username, c.Permission)
		opts := &github.RepositoryAddCollaboratorOptions{Permission: c.Permission}
		_, _, _ = client.Repositories.AddCollaborator(ctx, owner, repo, c.Username, opts)
	}
}

// buildWorkflowTemplate parses the wrapper template from templatePath using the
// [[ ]] delimiters so GitHub Actions ${{ }} expressions inside param values are
// emitted verbatim.
//
// quoteForYAML wraps values that look like JSON arrays (start with "[") in YAML
// single-quotes so they are parsed as strings rather than sequences, and renders
// multiline values as an indented block scalar (|-).
func buildWorkflowTemplate(templatePath string) (*template.Template, error) {
	funcMap := template.FuncMap{
		"quoteForYAML": func(s string) string {
			if strings.HasPrefix(strings.TrimSpace(s), "[") {
				return "'" + s + "'"
			}
			// Multiline values: render as an indented block scalar (|-).
			// The "with:" key is at 4-space indent, param keys at 6 spaces,
			// so block content sits at 8 spaces.
			if strings.Contains(s, "\n") {
				trimmed := strings.TrimRight(s, "\n")
				lines := strings.Split(trimmed, "\n")
				result := "|-"
				for _, line := range lines {
					result += "\n        " + line
				}
				return result
			}
			return s
		},
	}
	// The template's name has to match the base of the parsed file or Execute
	// finds nothing to run, so it is derived rather than hardcoded: this builder
	// serves both workflow.tmpl and pipeline.tmpl.
	name := templatePath
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}
	return template.New(name).Delims("[[", "]]").Funcs(funcMap).ParseFiles(templatePath)
}

// groupPipelines splits a repository's workflows into the entries that render a
// wrapper file each, and the pipelines that render one combined file each.
// Pipeline order follows first appearance in the config, and job order within a
// pipeline follows the config too, so a generated file stays stable across runs.
func groupPipelines(
	workflows []WorkflowConfig,
) (standalone []WorkflowConfig, pipelines [][]WorkflowConfig) {
	index := make(map[string]int)
	for _, wf := range workflows {
		if wf.Pipeline == "" {
			standalone = append(standalone, wf)
			continue
		}
		if pos, ok := index[wf.Pipeline]; ok {
			pipelines[pos] = append(pipelines[pos], wf)
			continue
		}
		index[wf.Pipeline] = len(pipelines)
		pipelines = append(pipelines, []WorkflowConfig{wf})
	}
	return standalone, pipelines
}

// validatePipeline rejects a pipeline that could not run as written.
//
// Triggers are checked because a combined file has one `on:` block: members
// that disagree about when they run cannot share a file, and silently taking
// the first member's triggers would stop the others from running at all. Needs
// are checked because GitHub fails an entire workflow that names a job it
// cannot resolve, which would take out every job in the pipeline rather than
// the one that was misconfigured.
func validatePipeline(members []WorkflowConfig) error {
	if len(members) == 0 {
		return errors.New("pipeline has no members")
	}

	file := members[0].Pipeline

	// A pipeline declares its `on:` block once, or every member declares the
	// same triggers. The second form is what a pipeline confined to one event
	// looks like, and predates pipeline_triggers.
	declared := 0
	for _, wf := range members {
		if len(wf.PipelineTriggers) > 0 {
			declared++
		}
	}
	if declared > 1 {
		return fmt.Errorf(
			"pipeline %q: %d members declare pipeline_triggers; a combined "+
				"workflow has one on: block, so exactly one member declares it",
			file,
			declared,
		)
	}

	merged := pipelineTriggers(members)
	if _, err := renderTriggers(merged); err != nil {
		return fmt.Errorf("renderTriggers for pipeline %q: %w", file, err)
	}

	ids := make(map[string]bool, len(members))
	for _, wf := range members {
		id := wf.jobID()
		if id == "" {
			return fmt.Errorf(
				"pipeline %q: entry %q resolves to an empty job id",
				file,
				wf.DestinationFile,
			)
		}
		if ids[id] {
			return fmt.Errorf("pipeline %q: duplicate job id %q", file, id)
		}
		ids[id] = true
	}

	conditions := make(map[string]string, len(members))
	for _, wf := range members {
		conditions[wf.jobID()] = wf.If
	}

	for _, wf := range members {
		// The pipeline's on: block is the union of its members', so a member
		// that used to run on fewer events now runs on all of them unless its
		// `if` says otherwise. That is the point for a check: widening lint and
		// the tests to a repository's pushes is what lets a release depend on
		// them.
		//
		// It is not the point for a job that writes. A publishing job whose
		// wrapper only ever ran on a tag would, on joining a pipeline that also
		// runs on pull requests, start publishing from pull requests -- with
		// its own write grant. Such a job has to say which events it belongs
		// to.
		if writesSomething(wf.Permissions) && wf.If == "" && triggersInclude(merged, "pull_request") {
			return fmt.Errorf(
				"pipeline %q: %q requests a write permission and declares no "+
					"`if`, but the pipeline runs on pull_request; give it an "+
					"`if` naming the events it belongs to, or leave it as a "+
					"separate wrapper",
				file,
				wf.DestinationFile,
			)
		}
		for _, need := range wf.Needs {
			if !ids[need] {
				return fmt.Errorf(
					"pipeline %q: job %q needs %q, which is not a job in this pipeline",
					file,
					wf.jobID(),
					need,
				)
			}
			if need == wf.jobID() {
				return fmt.Errorf("pipeline %q: job %q needs itself", file, need)
			}
			// A skipped job's dependents skip with it. A job guarded by a
			// narrower condition than the one it needs never runs, so a release
			// job gated on a pull-request-only build would silently never
			// publish -- a green run that did nothing.
			if conditions[need] != "" && conditions[need] != wf.If {
				return fmt.Errorf(
					"pipeline %q: job %q needs %q, which is conditional on "+
						"%q; a skipped job skips its dependents, so %q would "+
						"never run when %q is skipped",
					file,
					wf.jobID(),
					need,
					conditions[need],
					wf.jobID(),
					need,
				)
			}
		}
	}

	return validateNoCycle(file, members)
}

// validateNoCycle rejects a dependency cycle among a pipeline's jobs.
//
// Every job in a cycle waits for another job in it, so none can ever start.
// GitHub rejects the workflow outright, which takes out the whole file rather
// than the jobs involved -- the same blast radius as an unresolvable `needs`,
// and not something the self-reference check catches: `a` needing `b` while `b`
// needs `a` contains no self-reference at all.
func validateNoCycle(file string, members []WorkflowConfig) error {
	needs := make(map[string][]string, len(members))
	order := make([]string, 0, len(members))
	for _, wf := range members {
		id := wf.jobID()
		needs[id] = wf.Needs
		order = append(order, id)
	}

	// Iterative depth-first search, colouring each job white (absent), grey
	// (on the current path) or black (fully explored). Meeting a grey job is a
	// back edge, which is a cycle.
	const (
		grey  = 1
		black = 2
	)
	state := make(map[string]int, len(needs))

	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch state[id] {
		case black:
			return nil
		case grey:
			return fmt.Errorf(
				"pipeline %q: jobs %s form a dependency cycle; every job in "+
					"it waits for another, so none can start",
				file,
				strings.Join(append(path, id), " -> "),
			)
		}
		state[id] = grey
		for _, need := range needs[id] {
			if err := visit(need, append(path, id)); err != nil {
				return err
			}
		}
		state[id] = black
		return nil
	}

	for _, id := range order {
		if err := visit(id, nil); err != nil {
			return err
		}
	}
	return nil
}

// writesSomething reports whether a job asks for any write grant, which is what
// separates a check from a job that can change something outside the run.
func writesSomething(permissions map[string]string) bool {
	for _, level := range permissions {
		if strings.TrimSpace(level) == "write" {
			return true
		}
	}
	return false
}

// triggersInclude reports whether a merged `on:` block covers an event.
func triggersInclude(triggers map[string]interface{}, event string) bool {
	_, ok := triggers[event]
	return ok
}

// pipelineTriggers returns the combined workflow's `on:` block: an explicit
// pipeline_triggers when a member declares one, otherwise the union of what the
// members would each have run on.
func pipelineTriggers(members []WorkflowConfig) map[string]interface{} {
	for _, wf := range members {
		if len(wf.PipelineTriggers) > 0 {
			return wf.PipelineTriggers
		}
	}
	merged := map[string]interface{}{}
	for _, wf := range members {
		for event, filter := range wf.Triggers {
			mergeTrigger(merged, event, filter)
		}
	}
	return merged
}

// mergeTrigger folds one member's trigger into the pipeline's `on:` block.
//
// The union has to be the more permissive of the two, or a member stops running
// on an event it used to run on. That makes a filter present on one member and
// absent on another collapse to no filter: a bare `pull_request:` means every
// pull request, so it cannot be narrowed by another member's branches or paths
// list without silencing the bare one. Each member's `if` is what narrows it
// back to the events that member belongs to.
//
// Note that a paths filter cannot be expressed as an `if`, so folding a
// paths-filtered wrapper into a pipeline drops that filter. Do that only where
// the filter was under-inclusive to begin with; a filter that accurately
// describes what the job depends on is cheaper left in its own wrapper.
func mergeTrigger(merged map[string]interface{}, event string, filter interface{}) {
	existing, seen := merged[event]
	if !seen {
		merged[event] = filter
		return
	}
	// Either side unfiltered wins: it is the broader of the two.
	if filter == nil || existing == nil {
		merged[event] = nil
		return
	}

	left, lok := existing.(map[string]interface{})
	right, rok := filter.(map[string]interface{})
	if !lok || !rok {
		merged[event] = nil
		return
	}

	// `branches` and `tags` select refs and GitHub ORs them: a push matches if
	// it matches either, and a filter naming only `tags` does not match a
	// branch push at all. So a side that names no ref key matches every ref,
	// and the union then names none either. Intersecting the keys instead is
	// what silently drops `branches` when merging a lint wrapper that runs on
	// main with a publish wrapper that runs on tags, leaving lint running on
	// tags only.
	//
	// `paths` is different: it ANDs with the ref selection, so a side without
	// one is again the broader, and the union carries no paths.
	refKeys := []string{"branches", "branches-ignore", "tags", "tags-ignore"}
	pathKeys := []string{"paths", "paths-ignore"}

	out := map[string]interface{}{}
	mergeGroup := func(keys []string) {
		leftHas, rightHas := false, false
		for _, k := range keys {
			if _, ok := left[k]; ok {
				leftHas = true
			}
			if _, ok := right[k]; ok {
				rightHas = true
			}
		}
		if !leftHas || !rightHas {
			return // one side is unfiltered in this dimension; so is the union
		}
		for _, k := range keys {
			lv, lok := left[k]
			rv, rok := right[k]
			switch {
			case lok && rok:
				out[k] = unionLists(lv, rv)
			case lok:
				out[k] = lv
			case rok:
				out[k] = rv
			}
		}
	}
	mergeGroup(refKeys)
	mergeGroup(pathKeys)

	// Anything neither group covers (inputs, types, ...) only survives when
	// both sides agree; a difference there is broadened away.
	for key := range left {
		if isIn(key, refKeys) || isIn(key, pathKeys) {
			continue
		}
		if rv, ok := right[key]; ok {
			out[key] = unionLists(left[key], rv)
		}
	}
	if event == "pull_request" {
		defaultTypes := []interface{}{"opened", "synchronize", "reopened"}
		if lv, lok := left["types"]; lok {
			if _, rok := right["types"]; !rok {
				out["types"] = unionLists(lv, defaultTypes)
			}
		} else if rv, rok := right["types"]; rok {
			out["types"] = unionLists(defaultTypes, rv)
		}
	}

	if len(out) == 0 {
		merged[event] = nil
		return
	}
	merged[event] = out
}

func isIn(key string, keys []string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// unionLists merges two YAML sequences, preserving order and dropping repeats.
func unionLists(a, b interface{}) interface{} {
	left, lok := a.([]interface{})
	right, rok := b.([]interface{})
	if !lok || !rok {
		return a
	}
	seen := map[string]bool{}
	var out []interface{}
	for _, v := range append(append([]interface{}{}, left...), right...) {
		key := fmt.Sprint(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	return out
}

// pipelineWorkflowName picks the combined workflow's `name:`. The first member
// that sets pipeline_name wins; with none set the pipeline's file name stands
// in, so a misconfigured pipeline still renders something identifiable rather
// than an empty name.
func pipelineWorkflowName(members []WorkflowConfig) string {
	for _, wf := range members {
		if wf.PipelineName != "" {
			return wf.PipelineName
		}
	}
	name := members[0].Pipeline
	if idx := strings.LastIndex(name, "."); idx > 0 {
		name = name[:idx]
	}
	return name
}

// renderNeeds renders a job's needs list as an inline YAML sequence.
func renderNeeds(needs []string) string {
	if len(needs) == 0 {
		return ""
	}
	return "[" + strings.Join(needs, ", ") + "]"
}

// renderPipeline renders one combined workflow file whose jobs call the
// reusable workflows that were previously a wrapper file each.
func renderPipeline(tmpl *template.Template, members []WorkflowConfig) ([]byte, error) {
	if err := validatePipeline(members); err != nil {
		return nil, err
	}

	triggersYAML, err := renderTriggers(pipelineTriggers(members))
	if err != nil {
		return nil, fmt.Errorf("renderTriggers: %w", err)
	}

	data := pipelineData{
		WorkflowName: pipelineWorkflowName(members),
		TriggersYAML: triggersYAML,
	}
	for _, wf := range members {
		matrixYAML, merr := renderMatrix(wf.Matrix)
		if merr != nil {
			return nil, fmt.Errorf("renderMatrix for %q: %w", wf.DestinationFile, merr)
		}
		data.Jobs = append(data.Jobs, pipelineJobData{
			JobID:            wf.jobID(),
			NeedsYAML:        renderNeeds(wf.Needs),
			If:               wf.If,
			ReusableWorkflow: wf.ReusableWorkflow,
			Params:           wf.Params,
			Secrets:          wf.Secrets,
			Permissions:      wf.Permissions,
			MatrixYAML:       matrixYAML,
		})
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// supersededWorkflowPaths lists the wrapper files a repository no longer needs
// because their entries were folded into a pipeline.
//
// Deleting them is not tidiness. A wrapper left behind keeps matching the same
// events and keeps starting its own run, so the repository would pay for both
// the pipeline and every wrapper it replaced -- more fan-out than before the
// change, and two check runs claiming the same name.
func supersededWorkflowPaths(workflows []WorkflowConfig) []string {
	keep := make(map[string]bool)
	for _, wf := range workflows {
		if wf.Pipeline != "" {
			keep[wf.Pipeline] = true
		}
	}
	var paths []string
	for _, wf := range workflows {
		if wf.Pipeline == "" || wf.DestinationFile == "" {
			continue
		}
		// A pipeline that reuses one of its members' file names replaces that
		// file rather than superseding it.
		if keep[wf.DestinationFile] {
			continue
		}
		paths = append(paths, ".github/workflows/"+wf.DestinationFile)
	}
	sort.Strings(paths)
	return paths
}

// renderWorkflow renders a single workflow wrapper file from the parsed template.
func renderWorkflow(tmpl *template.Template, wf WorkflowConfig) ([]byte, error) {
	triggersYAML, err := renderTriggers(wf.Triggers)
	if err != nil {
		return nil, fmt.Errorf("renderTriggers: %w", err)
	}
	matrixYAML, err := renderMatrix(wf.Matrix)
	if err != nil {
		return nil, fmt.Errorf("renderMatrix: %w", err)
	}
	data := templateData{
		WorkflowName:     wf.WorkflowName,
		ReusableWorkflow: wf.ReusableWorkflow,
		Params:           wf.Params,
		Secrets:          wf.Secrets,
		Permissions:      wf.Permissions,
		MatrixYAML:       matrixYAML,
		TriggersYAML:     triggersYAML,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func syncWorkflows(ctx context.Context, client *github.Client, owner, repo string, workflows []WorkflowConfig) {
	// Fetch repo info once to get the actual default branch name.
	repoInfo, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		fmt.Printf("  Error getting repo info: %v\n", err)
		return
	}
	defaultBranch := repoInfo.GetDefaultBranch()

	tmpl, err := buildWorkflowTemplate("templates/workflow.tmpl")
	if err != nil {
		fmt.Printf("  Template compilation error: %v\n", err)
		return
	}

	standalone, pipelines := groupPipelines(workflows)

	for _, wf := range standalone {
		desiredContent, err := renderWorkflow(tmpl, wf)
		if err != nil {
			fmt.Printf("  Render error for %s: %v\n", wf.DestinationFile, err)
			continue
		}
		if werr := upsertWorkflowFile(
			ctx, client, owner, repo, defaultBranch, wf.DestinationFile, desiredContent,
		); werr != nil {
			fmt.Printf("  Error writing %s: %v\n", wf.DestinationFile, werr)
		}
	}

	if len(pipelines) > 0 {
		pipelineTmpl, perr := buildWorkflowTemplate("templates/pipeline.tmpl")
		if perr != nil {
			fmt.Printf("  Pipeline template compilation error: %v\n", perr)
			return
		}
		for _, members := range pipelines {
			desiredContent, rerr := renderPipeline(pipelineTmpl, members)
			if rerr != nil {
				fmt.Printf("  Render error for pipeline %s: %v\n", members[0].Pipeline, rerr)
				// Leave the superseded wrappers in place: they are this
				// repository's only CI until the pipeline renders.
				continue
			}
			if werr := upsertWorkflowFile(
				ctx, client, owner, repo, defaultBranch, members[0].Pipeline, desiredContent,
			); werr != nil {
				fmt.Printf("  Error writing pipeline %s: %v\n", members[0].Pipeline, werr)
				// Deleting the wrappers now would leave the repository with no
				// CI at all: the pipeline meant to replace them is not there.
				// They are still correct, so leave them and retry next run.
				continue
			}
			removeWorkflowFiles(
				ctx, client, owner, repo, defaultBranch, supersededWorkflowPaths(members),
			)
		}
	}
}

// upsertWorkflowFile writes one generated workflow file, skipping the push when
// the repository already holds exactly that content.
//
// It returns an error rather than only logging one because a caller may have to
// act on the failure: syncWorkflows deletes the wrappers a pipeline supersedes,
// and doing that after a failed pipeline write would leave the repository with
// no CI at all.
func upsertWorkflowFile(
	ctx context.Context,
	client *github.Client,
	owner, repo, defaultBranch, destinationFile string,
	desiredContent []byte,
) error {
	path := fmt.Sprintf(".github/workflows/%s", destinationFile)
	fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, nil)

	if err == nil {
		// File exists — GetContent() already returns the decoded string.
		existingContent, decErr := fileContent.GetContent()
		if decErr == nil && existingContent == string(desiredContent) {
			fmt.Printf("✅ Workflow file %s matches perfectly. Skipping push.\n", destinationFile)
			return nil
		}
		fmt.Printf("  Drift detected in %s. Overwriting file content...\n", destinationFile)
		opts := &github.RepositoryContentFileOptions{
			Message: github.String(fmt.Sprintf("chore: central update of %s", destinationFile)),
			Content: desiredContent,
			SHA:     fileContent.SHA,
			Branch:  github.String(defaultBranch),
		}
		if _, _, updateErr := client.Repositories.UpdateFile(ctx, owner, repo, path, opts); updateErr != nil {
			fmt.Printf("  Error updating %s: %v\n", destinationFile, updateErr)
			return fmt.Errorf("updating %s: %w", destinationFile, updateErr)
		}
		return nil
	}

	// File does not exist — create it
	fmt.Printf("  Creating missing workflow file %s...\n", destinationFile)
	opts := &github.RepositoryContentFileOptions{
		Message: github.String(fmt.Sprintf("chore: provision %s", destinationFile)),
		Content: desiredContent,
		Branch:  github.String(defaultBranch),
	}
	if _, _, createErr := client.Repositories.CreateFile(ctx, owner, repo, path, opts); createErr != nil {
		fmt.Printf("  Error creating %s: %v\n", destinationFile, createErr)
		return fmt.Errorf("creating %s: %w", destinationFile, createErr)
	}
	return nil
}

// removeWorkflowFiles deletes generated wrapper files that are no longer
// wanted. A missing file is success, so this is safe to run on every
// reconciliation.
func removeWorkflowFiles(
	ctx context.Context,
	client *github.Client,
	owner, repo, defaultBranch string,
	paths []string,
) {
	for _, workflowPath := range paths {
		fileContent, _, _, getErr := client.Repositories.GetContents(ctx, owner, repo, workflowPath, nil)
		if getErr != nil {
			if isNotFound(getErr) {
				continue
			}
			fmt.Printf("  Error checking %s for cleanup: %v\n", workflowPath, getErr)
			continue
		}
		if fileContent == nil || fileContent.SHA == nil {
			continue
		}

		fmt.Printf("  Removing superseded %s...\n", workflowPath)
		opts := &github.RepositoryContentFileOptions{
			Message: github.String(fmt.Sprintf(
				"chore: remove superseded %s",
				strings.TrimPrefix(workflowPath, ".github/workflows/"),
			)),
			SHA:    fileContent.SHA,
			Branch: github.String(defaultBranch),
		}
		if _, _, delErr := client.Repositories.DeleteFile(ctx, owner, repo, workflowPath, opts); delErr != nil {
			fmt.Printf("  Error deleting %s: %v\n", workflowPath, delErr)
			continue
		}
		fmt.Printf("✅ Removed %s from %s/%s\n", workflowPath, owner, repo)
	}
}

// obsoleteIssueCloseWorkflowPaths are legacy wrappers that are no longer
// needed in downstream repositories after reusable-workflow migration changes.
var obsoleteIssueCloseWorkflowPaths = []string{
	".github/workflows/update-issue-on-close.yml",
	".github/workflows/test-issue-on-close.yml",
}

// removeObsoleteIssueCloseWorkflows deletes legacy issue-close wrappers from a
// managed repository's default branch when present. Missing files (404) are
// treated as success so this cleanup can run on every reconciliation.
func removeObsoleteIssueCloseWorkflows(ctx context.Context, client *github.Client, owner, repo string) {
	repoInfo, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		fmt.Printf("  Error getting repo info for cleanup: %v\n", err)
		return
	}
	defaultBranch := repoInfo.GetDefaultBranch()

	for _, workflowPath := range obsoleteIssueCloseWorkflowPaths {
		fileContent, _, _, getErr := client.Repositories.GetContents(ctx, owner, repo, workflowPath, nil)
		if getErr != nil {
			if isNotFound(getErr) {
				continue
			}
			fmt.Printf("  Error checking %s for cleanup: %v\n", workflowPath, getErr)
			continue
		}
		if fileContent == nil || fileContent.SHA == nil {
			continue
		}

		fmt.Printf("  Removing obsolete %s...\n", workflowPath)
		opts := &github.RepositoryContentFileOptions{
			Message: github.String(fmt.Sprintf("chore: remove obsolete %s", strings.TrimPrefix(workflowPath, ".github/workflows/"))),
			SHA:     fileContent.SHA,
			Branch:  github.String(defaultBranch),
		}
		if _, _, delErr := client.Repositories.DeleteFile(ctx, owner, repo, workflowPath, opts); delErr != nil {
			fmt.Printf("  Error deleting %s: %v\n", workflowPath, delErr)
			continue
		}
		fmt.Printf("✅ Removed %s from %s/%s\n", workflowPath, owner, repo)
	}
}
