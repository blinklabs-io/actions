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
		repo := &cfg.Repositories[i]
		if repo.Profile == "" {
			continue
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
	}
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
// marker file. Repositories without a marker are skipped silently; a repository
// whose marker fails to parse is skipped with a warning so one bad marker cannot
// halt the whole run. When d.Topic is set, only repositories carrying that topic
// are considered.
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
				fmt.Printf("  ⚠️ discovery: reading marker for %s: %v\n", fullName, err)
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
// while pinning special cases in the central config.
func mergeDiscovered(cfg *Config, discovered []RepoConfig) {
	explicit := make(map[string]bool, len(cfg.Repositories))
	for _, r := range cfg.Repositories {
		explicit[r.Name] = true
	}
	for _, r := range discovered {
		if explicit[r.Name] {
			fmt.Printf("  discovery: %s is pinned in repos-config.yaml; skipping marker\n", r.Name)
			continue
		}
		cfg.Repositories = append(cfg.Repositories, r)
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
	return template.New("workflow.tmpl").Delims("[[", "]]").Funcs(funcMap).ParseFiles(templatePath)
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

	for _, wf := range workflows {
		desiredContent, err := renderWorkflow(tmpl, wf)
		if err != nil {
			fmt.Printf("  Render error for %s: %v\n", wf.DestinationFile, err)
			continue
		}

		path := fmt.Sprintf(".github/workflows/%s", wf.DestinationFile)
		fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, nil)

		if err == nil {
			// File exists — GetContent() already returns the decoded string.
			existingContent, decErr := fileContent.GetContent()
			if decErr == nil && existingContent == string(desiredContent) {
				fmt.Printf("✅ Workflow file %s matches perfectly. Skipping push.\n", wf.DestinationFile)
				continue
			}
			fmt.Printf("  Drift detected in %s. Overwriting file content...\n", wf.DestinationFile)
			opts := &github.RepositoryContentFileOptions{
				Message: github.String(fmt.Sprintf("chore: central update of %s", wf.DestinationFile)),
				Content: desiredContent,
				SHA:     fileContent.SHA,
				Branch:  github.String(defaultBranch),
			}
			if _, _, updateErr := client.Repositories.UpdateFile(ctx, owner, repo, path, opts); updateErr != nil {
				fmt.Printf("  Error updating %s: %v\n", wf.DestinationFile, updateErr)
			}
		} else {
			// File does not exist — create it
			fmt.Printf("  Creating missing workflow file %s...\n", wf.DestinationFile)
			opts := &github.RepositoryContentFileOptions{
				Message: github.String(fmt.Sprintf("chore: provision %s", wf.DestinationFile)),
				Content: desiredContent,
				Branch:  github.String(defaultBranch),
			}
			if _, _, createErr := client.Repositories.CreateFile(ctx, owner, repo, path, opts); createErr != nil {
				fmt.Printf("  Error creating %s: %v\n", wf.DestinationFile, createErr)
			}
		}
	}
}
