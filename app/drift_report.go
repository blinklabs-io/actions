package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

type driftReport struct {
	writer         io.Writer
	repos          []*driftRepo
	active         *driftRepo
	configRevision string
}

type driftRepo struct {
	name        string
	public      bool
	entries     []*driftEntry
	diagnostics []string
}

// diagnostics are fixed application messages and are retained only for public repos.
func (r *driftReport) diagnostic(message string) {
	if r.active != nil && r.active.public {
		r.active.diagnostics = append(r.active.diagnostics, message)
	}
}

type driftEntry struct {
	kind   string
	target string
	before string
	after  string
	status string
	commit string
}

func newDriftReport(writer io.Writer) *driftReport {
	return &driftReport{writer: writer}
}

func (r *driftReport) beginRepo(name string, public bool) {
	repo := &driftRepo{public: public}
	if public {
		repo.name = name
	}
	r.repos = append(r.repos, repo)
	r.active = repo
}

// change accepts only report-safe metadata. Callers must pass summaries or
// hashes for managed files, since their raw contents may contain secrets.
func (r *driftReport) change(kind, target, before, after string) *driftEntry {
	entry := &driftEntry{}
	if r.active.public {
		entry.kind = kind
		entry.target = target
		entry.before = before
		entry.after = after
	}
	r.active.entries = append(r.active.entries, entry)
	return entry
}

// checkpoint records the observed difference before a correction is attempted.
func (r *driftReport) checkpoint(entry *driftEntry) error {
	var output strings.Builder
	if !r.active.public {
		output.WriteString("Private or unknown repository: 1 planned change.\n\n")
	} else {
		var detail strings.Builder
		fmt.Fprintf(&detail, "%s: %s %s [planned]\n", driftLine(r.active.name), driftLine(entry.kind), driftLine(entry.target))
		driftDiff(&detail, "-", entry.before)
		driftDiff(&detail, "+", entry.after)
		driftFence(&output, detail.String())
	}
	_, err := io.WriteString(r.writer, output.String())
	return err
}

func (r *driftReport) render() error {
	var output strings.Builder
	output.WriteString("## Governance drift report\n\n")
	if r.configRevision != "" {
		fmt.Fprintf(&output, "Configuration revision: `%s`\n\n", driftLine(r.configRevision))
	}
	counts := map[string]int{}
	privateCounts := map[string]int{}
	privateRepos := 0
	for _, repo := range r.repos {
		if !repo.public {
			privateRepos++
		}
		for _, entry := range repo.entries {
			status := entry.status
			if status == "" {
				status = "pending"
			}
			counts[status]++
			if !repo.public {
				privateCounts[status]++
			}
		}
	}
	fmt.Fprintf(&output, "Checked %d repository entries; %d private or unknown.\n\n", len(r.repos), privateRepos)
	output.WriteString("Scope: configured repository settings, configured collaborators, branch protections, and managed files. Unmanaged items, other collaborators, and rulesets are not checked.\n\n")
	for _, status := range []string{"matched", "unmanaged", "corrected", "invited", "failed", "unchecked", "skipped", "pending"} {
		fmt.Fprintf(&output, "- %s: %d\n", status, counts[status])
	}
	output.WriteString("\n")
	if privateRepos > 0 {
		fmt.Fprintf(&output, "Private or unknown items: %d matched, %d unmanaged, %d corrected, %d invited, %d failed, %d unchecked, %d skipped, %d pending.\n\n", privateCounts["matched"], privateCounts["unmanaged"], privateCounts["corrected"], privateCounts["invited"], privateCounts["failed"], privateCounts["unchecked"], privateCounts["skipped"], privateCounts["pending"])
	}

	for _, repo := range r.repos {
		if !repo.public || len(repo.entries) == 0 {
			continue
		}
		var detail strings.Builder
		fmt.Fprintf(&detail, "%s (%d item(s))\n", driftLine(repo.name), len(repo.entries))
		for _, message := range repo.diagnostics {
			fmt.Fprintf(&detail, "diagnostic: %s\n", driftLine(message))
		}
		for _, entry := range repo.entries {
			status := entry.status
			if status == "" {
				status = "pending"
			}
			fmt.Fprintf(&detail, "\n%s %s [%s]\n",
				driftLine(entry.kind), driftLine(entry.target), driftLine(status))
			driftDiff(&detail, "-", entry.before)
			driftDiff(&detail, "+", entry.after)
			if entry.commit != "" {
				fmt.Fprintf(&detail, "commit: %s\n", driftLine(entry.commit))
			}
		}
		driftFence(&output, detail.String())
	}
	_, err := io.WriteString(r.writer, output.String())
	return err
}

func driftDiff(out *strings.Builder, prefix, value string) {
	if value == "" {
		return
	}
	for _, line := range strings.Split(value, "\n") {
		fmt.Fprintf(out, "%s %s\n", prefix, driftLine(line))
	}
}

func driftLine(value string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '\uFFFD'
		}
		return r
	}, value)
	return strings.ReplaceAll(clean, "##[", "# #[")
}

func driftFence(out *strings.Builder, detail string) {
	width := 3
	run := 0
	for _, r := range detail {
		if r == '~' {
			run++
			if run >= width {
				width = run + 1
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("~", width)
	fmt.Fprintf(out, "%sdiff\n%s%s\n\n", fence, detail, fence)
}
