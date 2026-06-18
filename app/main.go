package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	RequiredApprovingReviewCount   int      `yaml:"required_approving_review_count"`
	DismissStaleReviews            bool     `yaml:"dismiss_stale_reviews"`
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
		syncBranchProtection(ctx, client, owner, repoName, repo.BranchProtection)
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

func syncBranchProtection(ctx context.Context, client *github.Client, owner, repo string, protections []BranchProtection) {
	for _, bp := range protections {
		// Build desired protection request
		req := &github.ProtectionRequest{
			EnforceAdmins:                  bp.EnforceAdmins,
			Restrictions:                   nil,
			RequireLinearHistory:           github.Bool(bp.RequireLinearHistory),
			AllowForcePushes:               github.Bool(bp.AllowForcePushes),
			AllowDeletions:                 github.Bool(bp.AllowDeletions),
			RequiredConversationResolution: github.Bool(bp.RequiredConversationResolution),
			LockBranch:                     github.Bool(bp.LockBranch),
		}

		if len(bp.RequiredStatusChecks) > 0 {
			req.RequiredStatusChecks = &github.RequiredStatusChecks{
				Strict:   bp.RequireUpToDate,
				Contexts: &bp.RequiredStatusChecks,
			}
		}

		if !bp.BypassPullRequest {
			req.RequiredPullRequestReviews = &github.PullRequestReviewsEnforcementRequest{
				RequiredApprovingReviewCount: bp.RequiredApprovingReviewCount,
				DismissStaleReviews:          bp.DismissStaleReviews,
			}
		}

		// Observe current state
		current, _, err := client.Repositories.GetBranchProtection(ctx, owner, repo, bp.Branch)
		if err == nil {
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

			currentApprovals := 0
			currentDismiss := false
			if r := current.GetRequiredPullRequestReviews(); r != nil {
				currentApprovals = r.RequiredApprovingReviewCount
				currentDismiss = r.DismissStaleReviews
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

			if hasPR == wantsPR &&
				stringSlicesEqual(currentChecks, bp.RequiredStatusChecks) &&
				currentStrict == bp.RequireUpToDate &&
				currentApprovals == bp.RequiredApprovingReviewCount &&
				currentDismiss == bp.DismissStaleReviews &&
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

		fmt.Printf("  Updating branch protection on %s...\n", bp.Branch)
		if _, _, updateErr := client.Repositories.UpdateBranchProtection(ctx, owner, repo, bp.Branch, req); updateErr != nil {
			fmt.Printf("  Error updating branch protection on %s: %v\n", bp.Branch, updateErr)
		} else {
			fmt.Printf("✅ Branch protection updated on %s\n", bp.Branch)
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
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
