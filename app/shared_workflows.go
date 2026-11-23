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

// validateSharedWorkflowRefs verifies explicitly configured public sources
// before reconciliation. Rendering preserves each configured source verbatim.
func validateSharedWorkflowRefs(cfg *Config, read func(ref, path string) ([]byte, error)) error {
	verified := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		for _, workflow := range repo.Workflows {
			if !strings.HasPrefix(workflow.ReusableWorkflow, sharedWorkflowRepository) {
				continue
			}
			path, ref, ok := strings.Cut(strings.TrimPrefix(workflow.ReusableWorkflow, sharedWorkflowRepository), "@")
			if !ok || !commitSHAPattern.MatchString(ref) || !reusableWorkflowPath.MatchString(path) {
				return errors.New("invalid reusable workflow path or reference")
			}
			if workflow.PinToLatestChange {
				return errors.New("public workflow sources require explicit immutable references")
			}
			key := ref + ":" + path
			if !verified[key] {
				if read == nil {
					return errors.New("public workflow source reader required")
				}
				contents, err := read(ref, path)
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
		}
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
