package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	Teams            []TeamConfig       `yaml:"teams"`
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

type TeamConfig struct {
	Name       string `yaml:"name"`
	Permission string `yaml:"permission"`
}

type BranchProtection struct {
	Branch               string   `yaml:"branch"`
	RequiredStatusChecks []string `yaml:"required_status_checks"`
	BypassPullRequest    bool     `yaml:"bypass_pull_request"`
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
			// File exists — decode and compare
			rawEncoded, _ := fileContent.GetContent()
			decoded, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(rawEncoded, "\n", ""))
			if string(decoded) == string(desiredContent) {
				fmt.Printf("✅ Workflow file %s matches perfectly. Skipping push.\n", wf.DestinationFile)
				continue
			}
			fmt.Printf("  Drift detected in %s. Overwriting file content...\n", wf.DestinationFile)
			opts := &github.RepositoryContentFileOptions{
				Message: github.String(fmt.Sprintf("chore: central update of %s", wf.DestinationFile)),
				Content: desiredContent,
				SHA:     fileContent.SHA,
				Branch:  github.String("main"),
			}
			_, _, _ = client.Repositories.UpdateFile(ctx, owner, repo, path, opts)
		} else {
			// File does not exist — create it
			fmt.Printf("  Creating missing workflow file %s...\n", wf.DestinationFile)
			opts := &github.RepositoryContentFileOptions{
				Message: github.String(fmt.Sprintf("chore: provision %s", wf.DestinationFile)),
				Content: desiredContent,
				Branch:  github.String("main"),
			}
			_, _, _ = client.Repositories.CreateFile(ctx, owner, repo, path, opts)
		}
	}
}
