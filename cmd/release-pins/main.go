package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func run(args []string) error {
	flags := flag.NewFlagSet("release-pins", flag.ContinueOnError)
	root := flags.String("root", "", "checkout containing Helmfile releases")
	repository := flags.String("repository", "", "owner/name of the checkout to publish to")
	publishUpdates := flags.Bool("publish", false, "create or update issues and PRs")
	limit := flags.Int("limit", 10, "maximum releases processed per run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return fmt.Errorf("--root is required")
	}
	if *limit < 1 {
		return fmt.Errorf("--limit must be positive")
	}
	if *publishUpdates && *repository == "" {
		return fmt.Errorf("--repository is required with --publish")
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	registry, err := newRegistry(absolute)
	if err != nil {
		return err
	}
	planned, skipped, err := plan(absolute, registry)
	if err != nil {
		return err
	}
	var report strings.Builder
	fmt.Fprintf(&report, "## Daily release pin scan\n\n%d releases have eligible updates.\n", len(planned))
	keys := make([]string, 0, len(planned))
	for release := range planned {
		keys = append(keys, release)
	}
	sort.Strings(keys)
	for _, release := range keys {
		var updates []string
		for _, p := range planned[release] {
			updates = append(updates, fmt.Sprintf("%s %s → %s", p.Kind, p.Current, p.Target))
		}
		fmt.Fprintf(&report, "- `%s`: %s\n", release, strings.Join(updates, ", "))
	}
	report.WriteString("\n### Needs manual source mapping or review\n")
	unique := map[string]bool{}
	for _, item := range skipped {
		unique[item] = true
	}
	items := make([]string, 0, len(unique))
	for item := range unique {
		items = append(items, item)
	}
	sort.Strings(items)
	for _, item := range items {
		fmt.Fprintf(&report, "- %s\n", item)
	}
	failed := false
	if *publishUpdates {
		activity, bad, err := publish(absolute, *repository, planned, *limit, registry.URLs)
		if err != nil {
			return err
		}
		failed = bad
		report.WriteString("\n### PR activity\n")
		for _, item := range activity {
			fmt.Fprintf(&report, "- %s\n", item)
		}
	}
	fmt.Print(report.String())
	if destination := os.Getenv("GITHUB_STEP_SUMMARY"); destination != "" {
		file, err := os.OpenFile(destination, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err = file.WriteString(report.String()); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
	}
	if registry.checked == 0 {
		return fmt.Errorf("no registries could be checked")
	}
	if failed {
		return fmt.Errorf("one or more releases failed; see summary")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release pin updater:", err)
		os.Exit(1)
	}
}
