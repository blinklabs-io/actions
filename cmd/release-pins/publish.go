package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const markerFormat = "<!-- release-pin-updater: %s -->"

var markerRE = regexp.MustCompile(`<!-- release-pin-updater: ([\w.-]+) -->`)
var closesRE = regexp.MustCompile(`(?m)^Closes #(\d+)$`)
var branchUnsafeRE = regexp.MustCompile(`[^a-z0-9-]`)

type author struct {
	Login string `json:"login"`
}
type openPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   string `json:"headRefName"`
	Base   string `json:"baseRefName"`
	Cross  bool   `json:"isCrossRepository"`
	Author author `json:"author"`
	URL    string `json:"url"`
}
type closedPR struct {
	Number   int     `json:"number"`
	Body     string  `json:"body"`
	MergedAt *string `json:"mergedAt"`
	URL      string  `json:"url"`
}
type openIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author author `json:"author"`
}

func upstreamLink(source string, urls map[string]string) string {
	alias := strings.SplitN(source, "/", 2)[0]
	if url := urls[alias]; url != "" {
		return url
	}
	if strings.HasPrefix(source, "oci://ghcr.io/") || strings.HasPrefix(source, "ghcr.io/") {
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(source, "oci://ghcr.io/"), "ghcr.io/"), "/", 2)
		if len(parts) == 2 {
			return "https://github.com/orgs/" + parts[0] + "/packages/container/package/" + strings.ReplaceAll(parts[1], "/", "%2F")
		}
		return source
	}
	if strings.HasPrefix(source, "docker.io/") || !strings.Contains(alias, ".") {
		return "https://hub.docker.com/r/" + strings.TrimPrefix(source, "docker.io/") + "/tags"
	}
	return source
}

func description(release string, pins []pin, urls map[string]string, issue int) string {
	var body strings.Builder
	fmt.Fprintf(&body, markerFormat+"\n\nUpdate published pins for the `%s` Helmfile release.\n\n| Pin | Current on main | Proposed | Source |\n| --- | --- | --- | --- |\n", release, release)
	for _, p := range pins {
		fmt.Fprintf(&body, "| %s | `%s` | `%s` | [upstream](%s) |\n", p.Kind, p.Current, p.Target, upstreamLink(p.Source, urls))
	}
	body.WriteString("\nRegistry artifact existence is checked before this PR is opened. Normal PR checks validate the deployment diff. No deployment or merge is automatic.\n")
	if issue > 0 {
		fmt.Fprintf(&body, "\nCloses #%d\n", issue)
	}
	return body.String()
}

func bodyFile(body string) (string, error) {
	file, err := os.CreateTemp("", "release-pin-*.md")
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err = file.WriteString(body); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func markerRelease(body string) string {
	match := markerRE.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

func mentionsRelease(text, release string) bool {
	text, release = strings.ToLower(text), strings.ToLower(release)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], release)
		if index < 0 {
			return false
		}
		index += offset
		boundary := func(c byte) bool { return c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
		if (index == 0 || !boundary(text[index-1])) && (index+len(release) == len(text) || !boundary(text[index+len(release)])) {
			return true
		}
		offset = index + 1
	}
	return false
}

func ghJSON(root string, target any, args ...string) error {
	out, err := command(root, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), target)
}

func publish(root, repo string, planned map[string][]pin, limit int, urls map[string]string) ([]string, bool, error) {
	actor := os.Getenv("RELEASE_PIN_ACTOR")
	if actor == "" {
		return nil, false, fmt.Errorf("RELEASE_PIN_ACTOR is required")
	}
	var prs []openPR
	if err := ghJSON(root, &prs, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "500", "--json", "number,title,body,headRefName,baseRefName,isCrossRepository,author,url"); err != nil {
		return nil, false, err
	}
	var closed []closedPR
	if err := ghJSON(root, &closed, "gh", "pr", "list", "--repo", repo, "--state", "closed", "--limit", "500", "--json", "number,body,mergedAt,url"); err != nil {
		return nil, false, err
	}
	var issues []openIssue
	if err := ghJSON(root, &issues, "gh", "issue", "list", "--repo", repo, "--state", "open", "--limit", "500", "--json", "number,title,body,author"); err != nil {
		return nil, false, err
	}
	trustedPRs := map[string]openPR{}
	unsafe := map[string]bool{}
	for _, pr := range prs {
		release := markerRelease(pr.Body)
		if release == "" {
			continue
		}
		match := closesRE.FindStringSubmatch(pr.Body)
		expected := ""
		if match != nil {
			expected = branchName(release, match[1])
		}
		if pr.Author.Login != actor || pr.Cross || pr.Base != "main" || pr.Head != expected || trustedPRs[release].Number != 0 {
			unsafe[release] = true
			continue
		}
		trustedPRs[release] = pr
	}
	abandoned := map[string]closedPR{}
	for _, pr := range closed {
		if release := markerRelease(pr.Body); release != "" && pr.MergedAt == nil {
			abandoned[release] = pr
		}
	}
	trustedIssues := map[string]openIssue{}
	for _, issue := range issues {
		release := markerRelease(issue.Body)
		if release == "" {
			continue
		}
		if issue.Author.Login != actor || trustedIssues[release].Number != 0 {
			unsafe[release] = true
			continue
		}
		trustedIssues[release] = issue
	}
	keys := make([]string, 0, len(planned))
	for release := range planned {
		keys = append(keys, release)
	}
	sort.Strings(keys)
	var results []string
	processed := 0
	failed := false
	for _, release := range keys {
		if processed >= limit {
			break
		}
		pins := planned[release]
		pr, existing := trustedPRs[release]
		if unsafe[release] {
			results = append(results, release+": untrusted or duplicate updater PR/issue marker needs manual review")
			failed = true
			continue
		}
		if !existing {
			if old, ok := abandoned[release]; ok {
				results = append(results, release+": closed unmerged updater PR needs manual review: "+old.URL)
				continue
			}
			blocked := false
			for _, item := range prs {
				if mentionsRelease(item.Title+"\n"+item.Body, release) {
					blocked = true
					break
				}
			}
			if blocked {
				results = append(results, release+": skipped because another open PR tracks the release")
				continue
			}
			for _, item := range issues {
				if markerRelease(item.Body) != release && mentionsRelease(item.Title+"\n"+item.Body, release) {
					blocked = true
					break
				}
			}
			if blocked {
				results = append(results, release+": skipped because another open issue tracks the release")
				continue
			}
		}
		issue, hasIssue := trustedIssues[release]
		activity, err := publishOne(root, repo, release, pins, urls, func() *openPR {
			if existing {
				return &pr
			}
			return nil
		}(), func() *openIssue {
			if hasIssue {
				return &issue
			}
			return nil
		}())
		if err != nil {
			results = append(results, fmt.Sprintf("%s: failed (%v)", release, err))
			failed = true
			continue
		}
		if activity != "" {
			results = append(results, activity)
			processed++
		}
	}
	return results, failed, nil
}

func branchName(release, number string) string {
	return "chore/update-" + branchUnsafeRE.ReplaceAllString(strings.ToLower(release), "-") + "-issue-" + number
}

func publishOne(root, repo, release string, pins []pin, urls map[string]string, existing *openPR, issue *openIssue) (string, error) {
	branch := ""
	number := 0
	start := "origin/main"
	resume := false
	if existing != nil {
		match := closesRE.FindStringSubmatch(existing.Body)
		if match == nil {
			return "", fmt.Errorf("existing updater PR has no closing issue")
		}
		number, _ = strconv.Atoi(match[1])
		branch = existing.Head
		if _, err := command(root, "git", "fetch", "origin", branch); err != nil {
			return "", err
		}
		start = "origin/" + branch
	} else {
		if issue == nil {
			file, err := bodyFile(description(release, pins, urls, 0))
			if err != nil {
				return "", err
			}
			defer os.Remove(file)
			url, err := command(root, "gh", "issue", "create", "--repo", repo, "--title", release+" release pins lag upstream", "--body-file", file)
			if err != nil {
				return "", err
			}
			number, err = strconv.Atoi(filepath.Base(url))
			if err != nil {
				return "", err
			}
		} else {
			number = issue.Number
		}
		branch = branchName(release, strconv.Itoa(number))
		if issue != nil {
			out, err := command(root, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
			if err != nil {
				return "", err
			}
			if out != "" {
				if _, err = command(root, "git", "fetch", "origin", branch); err != nil {
					return "", err
				}
				if err = verifyOrphan(root, branch, pins); err != nil {
					return "", err
				}
				start = "origin/" + branch
				resume = true
			}
		}
	}
	temporary, err := os.MkdirTemp("", "release-pin-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	worktree := filepath.Join(temporary, "worktree")
	if _, err = command(root, "git", "worktree", "add", "--detach", worktree, start); err != nil {
		return "", err
	}
	defer command(root, "git", "worktree", "remove", "--force", worktree)
	if existing != nil || resume {
		if _, err = command(worktree, "git", "merge", "origin/main", "--signoff", "-m", "chore: merge main into "+branch); err != nil {
			return "", err
		}
	}
	changed, err := applyPins(worktree, release, pins)
	if err != nil {
		return "", err
	}
	if len(changed) == 0 && existing == nil && !resume {
		return "", fmt.Errorf("no changes produced for new release PR: %s", release)
	}
	if len(changed) > 0 {
		if _, err = command(worktree, "git", "diff", "--check"); err != nil {
			return "", err
		}
		if _, err = command(worktree, append([]string{"git", "add", "--"}, changed...)...); err != nil {
			return "", err
		}
		if _, err = command(worktree, "git", "commit", "-s", "-m", "chore: update "+release+" release pins"); err != nil {
			return "", err
		}
	}
	ahead := false
	if existing != nil || resume {
		out, e := command(worktree, "git", "rev-list", "--count", "origin/"+branch+"..HEAD")
		if e != nil {
			return "", e
		}
		ahead = out != "0"
	}
	if len(changed) > 0 || ahead {
		if _, err = command(worktree, "git", "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return "", err
		}
	}
	body := description(release, pins, urls, number)
	if existing != nil && len(changed) == 0 {
		if existing.Body != body {
			file, e := bodyFile(body)
			if e != nil {
				return "", e
			}
			defer os.Remove(file)
			if _, e = command(worktree, "gh", "pr", "edit", strconv.Itoa(existing.Number), "--repo", repo, "--body-file", file); e != nil {
				return "", e
			}
			return release + ": refreshed " + existing.URL, nil
		}
		if ahead {
			return release + ": updated " + existing.URL, nil
		}
		return "", nil
	}
	file, err := bodyFile(body)
	if err != nil {
		return "", err
	}
	defer os.Remove(file)
	if existing != nil {
		if _, err = command(worktree, "gh", "pr", "edit", strconv.Itoa(existing.Number), "--repo", repo, "--body-file", file); err != nil {
			return "", err
		}
		return release + ": updated " + existing.URL, nil
	}
	url, err := command(worktree, "gh", "pr", "create", "--repo", repo, "--base", "main", "--head", branch, "--title", "chore: update "+release+" release pins", "--body-file", file)
	if err != nil {
		return "", err
	}
	return release + ": opened " + url, nil
}

func verifyOrphan(root, branch string, pins []pin) error {
	base, err := command(root, "git", "merge-base", "origin/main", "origin/"+branch)
	if err != nil {
		return err
	}
	out, err := command(root, "git", "diff", "--name-only", base, "origin/"+branch)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, p := range pins {
		allowed[p.Path] = true
	}
	if out == "" {
		return fmt.Errorf("existing branch needs manual review: %s", branch)
	}
	for _, path := range strings.Split(out, "\n") {
		if !allowed[path] {
			return fmt.Errorf("existing branch needs manual review: %s", branch)
		}
	}
	actor := strings.TrimPrefix(os.Getenv("RELEASE_PIN_ACTOR"), "app/") + "[bot]"
	out, err = command(root, "git", "rev-list", base+"..origin/"+branch)
	if err != nil {
		return err
	}
	if out == "" {
		return fmt.Errorf("existing branch has no updater commit: %s", branch)
	}
	for _, commit := range strings.Split(out, "\n") {
		name, e := command(root, "git", "show", "-s", "--format=%an", commit)
		if e != nil {
			return e
		}
		body, e := command(root, "git", "show", "-s", "--format=%B", commit)
		if e != nil {
			return e
		}
		if name != actor || !strings.Contains(body, "Signed-off-by: "+actor+" <") {
			return fmt.Errorf("existing branch has a non-updater or unsigned commit: %s", branch)
		}
	}
	return nil
}
