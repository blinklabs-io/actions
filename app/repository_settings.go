package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML rejects unsupported settings instead of silently ignoring a policy.
func (s *RepoSettings) UnmarshalYAML(node *yaml.Node) error {
	type plainSettings RepoSettings
	known := make(map[string]bool)
	typ := reflect.TypeOf(RepoSettings{})
	for i := 0; i < typ.NumField(); i++ {
		known[typ.Field(i).Tag.Get("yaml")] = true
	}
	if err := validateSettingsKeys(node, known, make(map[*yaml.Node]bool)); err != nil {
		return err
	}
	return node.Decode((*plainSettings)(s))
}

func validateSettingsKeys(node *yaml.Node, known map[string]bool, visiting map[*yaml.Node]bool) error {
	if node == nil || visiting[node] {
		return errors.New("invalid or cyclic settings alias")
	}
	visiting[node] = true
	defer delete(visiting, node)
	switch node.Kind {
	case yaml.AliasNode:
		return validateSettingsKeys(node.Alias, known, visiting)
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				if value.Kind == yaml.SequenceNode {
					for _, source := range value.Content {
						if err := validateSettingsKeys(source, known, visiting); err != nil {
							return err
						}
					}
				} else if err := validateSettingsKeys(value, known, visiting); err != nil {
					return err
				}
			} else if key.Kind != yaml.ScalarNode || !known[key.Value] {
				return errors.New("unsupported repository setting")
			}
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil
		}
		fallthrough
	default:
		return errors.New("settings must be a mapping")
	}
	return nil
}

func validateRepoSettings(settings RepoSettings) error {
	if settings.DefaultBranch != nil && strings.TrimSpace(*settings.DefaultBranch) == "" {
		return errors.New("default_branch must name an existing branch")
	}
	for _, field := range []struct {
		name    string
		value   *string
		allowed []string
	}{
		{"squash_merge_commit_title", settings.SquashMergeCommitTitle, []string{"PR_TITLE", "COMMIT_OR_PR_TITLE"}},
		{"squash_merge_commit_message", settings.SquashMergeCommitMessage, []string{"PR_BODY", "COMMIT_MESSAGES", "BLANK"}},
		{"merge_commit_title", settings.MergeCommitTitle, []string{"PR_TITLE", "MERGE_MESSAGE"}},
		{"merge_commit_message", settings.MergeCommitMessage, []string{"PR_BODY", "PR_TITLE", "BLANK"}},
	} {
		if field.value != nil && !slices.Contains(field.allowed, *field.value) {
			return fmt.Errorf("unsupported %s value", field.name)
		}
	}
	if settings.SquashMergeCommitMessage != nil && settings.SquashMergeCommitTitle == nil {
		return errors.New("squash_merge_commit_message requires squash_merge_commit_title")
	}
	if settings.MergeCommitMessage != nil && settings.MergeCommitTitle == nil {
		return errors.New("merge_commit_message requires merge_commit_title")
	}
	if settings.AllowMergeCommit != nil && settings.AllowSquashMerge != nil && settings.AllowRebaseMerge != nil &&
		!*settings.AllowMergeCommit && !*settings.AllowSquashMerge && !*settings.AllowRebaseMerge {
		return errors.New("at least one merge method must remain enabled")
	}
	return nil
}
