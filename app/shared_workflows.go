package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

const sharedWorkflowRepository = "blinklabs-io/shared-actions/"

var reusableWorkflowPath = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-]+\.ya?ml$`)

// resolveSharedWorkflowRefs runs after profile expansion and fanout injection.
// All public sources are verified before changing any in-memory workflow refs.
func resolveSharedWorkflowRefs(cfg *Config, read func(ref, path string) ([]byte, error)) error {
	type replacement struct {
		repository, workflow int
		ref                  string
	}
	var replacements []replacement
	verified := make(map[string]bool)
	for i := range cfg.Repositories {
		repo := &cfg.Repositories[i]
		if repo.SharedActionsRef == "" {
			continue
		}
		if !commitSHAPattern.MatchString(repo.SharedActionsRef) {
			return errors.New("shared_actions_ref must be a full lowercase commit SHA")
		}
		for j := range repo.Workflows {
			workflow := &repo.Workflows[j]
			var source string
			matched := false
			for _, prefix := range []string{"blinklabs-io/actions/", sharedWorkflowRepository} {
				if strings.HasPrefix(workflow.ReusableWorkflow, prefix) {
					source = strings.TrimPrefix(workflow.ReusableWorkflow, prefix)
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			path, oldRef, ok := strings.Cut(source, "@")
			if !ok || oldRef == "" || strings.Contains(oldRef, "@") || !reusableWorkflowPath.MatchString(path) {
				return errors.New("invalid reusable workflow path or reference")
			}
			key := repo.SharedActionsRef + ":" + path
			if !verified[key] {
				if read == nil {
					return errors.New("public workflow source reader required")
				}
				contents, err := read(repo.SharedActionsRef, path)
				if err != nil {
					return fmt.Errorf("read public workflow source: %w", err)
				}
				var declaration struct {
					On yaml.Node `yaml:"on"`
				}
				if err := yaml.Unmarshal(contents, &declaration); err != nil {
					return fmt.Errorf("parse public workflow source: %w", err)
				}
				if !declaresWorkflowCall(declaration.On) {
					return errors.New("public workflow source must declare workflow_call")
				}
				verified[key] = true
			}
			replacements = append(replacements, replacement{i, j, sharedWorkflowRepository + path + "@" + repo.SharedActionsRef})
		}
	}
	for _, update := range replacements {
		workflow := &cfg.Repositories[update.repository].Workflows[update.workflow]
		workflow.ReusableWorkflow = update.ref
		// The public revision was checked directly; local actions history cannot resolve it.
		workflow.PinToLatestChange = false
	}
	return nil
}

func fetchSharedWorkflow(ctx context.Context, client *github.Client, ref, path string) ([]byte, error) {
	if !commitSHAPattern.MatchString(ref) || !reusableWorkflowPath.MatchString(path) {
		return nil, errors.New("invalid public workflow source")
	}
	// The Contents API dereferences symlinks. Inspect the immutable Git mode first.
	tree, _, err := client.Git.GetTree(ctx, "blinklabs-io", "shared-actions", ref, true)
	if err != nil {
		return nil, err
	}
	if tree == nil || tree.GetTruncated() {
		return nil, errors.New("incomplete public workflow tree")
	}
	regular := false
	for _, entry := range tree.Entries {
		if entry.GetPath() == path && entry.GetType() == "blob" &&
			(entry.GetMode() == "100644" || entry.GetMode() == "100755") {
			regular = true
			break
		}
	}
	if !regular {
		return nil, errors.New("public workflow source must be a regular Git file")
	}
	file, _, _, err := client.Repositories.GetContents(ctx, "blinklabs-io", "shared-actions", path,
		&github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, err
	}
	if file == nil || file.GetType() != "file" {
		return nil, errors.New("public workflow source must be a regular file")
	}
	contents, err := file.GetContent()
	if err != nil {
		return nil, err
	}
	return []byte(contents), nil
}
