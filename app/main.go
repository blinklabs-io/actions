package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	Repositories []RepoConfig `yaml:"repositories"`
}

type RepoConfig struct {
	Name             string             `yaml:"name"`
	Settings         RepoSettings       `yaml:"settings"`
	Collaborators    []Collaborator     `yaml:"collaborators"`
	BranchProtection []BranchProtection `yaml:"branch_protection"`
	Workflows        []WorkflowConfig   `yaml:"workflows"`
}

type RepoSettings struct {
	DeleteBranchOnMerge bool `yaml:"delete_branch_on_merge"`
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
	DestinationFile  string            `yaml:"destination_file"`
	WorkflowName     string            `yaml:"workflow_name"`
	ReusableWorkflow string            `yaml:"reusable_workflow"`
	Params           map[string]string `yaml:"params"`
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

	if current.GetDeleteBranchOnMerge() != settings.DeleteBranchOnMerge {
		fmt.Println("  Updating repository settings...")
		update := &github.Repository{
			DeleteBranchOnMerge: github.Bool(settings.DeleteBranchOnMerge),
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

func syncWorkflows(ctx context.Context, client *github.Client, owner, repo string, workflows []WorkflowConfig) {
	// Fetch repo info once to get the actual default branch name.
	repoInfo, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		fmt.Printf("  Error getting repo info: %v\n", err)
		return
	}
	defaultBranch := repoInfo.GetDefaultBranch()

	// Use [[ ]] as template delimiters so GitHub Actions ${{ }} expressions
	// inside param values are emitted verbatim.
	tmpl, err := template.New("workflow.tmpl").Delims("[[", "]]").ParseFiles("templates/workflow.tmpl")
	if err != nil {
		fmt.Printf("  Template compilation error: %v\n", err)
		return
	}

	for _, wf := range workflows {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, wf); err != nil {
			fmt.Printf("  Template execution error for %s: %v\n", wf.DestinationFile, err)
			continue
		}
		desiredContent := buf.Bytes()

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
