package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	helmfilePath = "helmfile-app/helmfile-demeter.yaml.gotmpl"
	reposPath    = "helmfile-app/helmfile-repos.yaml"
)

var (
	semverRE     = regexp.MustCompile(`^(v?)(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(\d+))?$`)
	releaseRE    = regexp.MustCompile(`^  - name: (.+?)\s*$`)
	fieldRE      = regexp.MustCompile(`^    (chart|version):\s*(\S+)\s*$`)
	valueFileRE  = regexp.MustCompile(`^      - ([\w./-]+\.ya?ml(?:\.gotmpl)?)\s*$`)
	keyRE        = regexp.MustCompile(`^(\s*)([\w-]+):(?:\s*(.*))?$`)
	rangeRE      = regexp.MustCompile(`^\s*\{\{-?\s*range \$(\w+) := list((?:\s+"[\w.-]+")+)\s*-?\}\}\s*$`)
	quotedRE     = regexp.MustCompile(`"([^"]+)"`)
	assignmentRE = regexp.MustCompile(`^\$\w+\s*:?=\s+`)
	actionRE     = regexp.MustCompile(`\{\{-?\s*(if|range|with|define|block|end)\b`)
)

var imageSources = map[string]string{
	"helmfile-app/butane-oracle/values.production.yaml.gotmpl": "sundaeswap/butane-oracle",
	"helmfile-app/butane-oracle/values.test.yaml.gotmpl":       "sundaeswap/butane-oracle",
	"helmfile-app/dolos/values.mainnet.yaml.gotmpl":            "ghcr.io/txpipe/dolos",
	"helmfile-app/dolos/values.preprod.yaml.gotmpl":            "ghcr.io/txpipe/dolos",
	"helmfile-app/dolos/values.preview.yaml.gotmpl":            "ghcr.io/txpipe/dolos",
	"helmfile-app/grafana-pdc/values.yaml.gotmpl":              "grafana/pdc-agent",
	"helmfile-app/grafana-pdc/values.demeter.global.gotmpl":    "grafana/pdc-agent",
	"helmfile-app/tosidrop/values.mainnet.yaml.gotmpl":         "ghcr.io/tosidrop/vm-frontend",
	"helmfile-app/tracing/tempo.yaml":                          "docker.io/grafana/tempo",
}

type pin struct {
	Release, Kind, Path             string
	Ordinal                         int
	Current, Target, Source, Digest string
}

type releaseSpec struct {
	Name, Chart, Version string
	Values               []string
	ChartOwner           string
	UnsupportedValues    bool
}

type releaseBlock struct {
	Start, End           int
	Name, Binding, Value string
}

type yamlField struct {
	Line, Indent int
	Key, Value   string
	Parents      string
}

type imageEntry struct {
	Ordinal, Line       int
	Tag, Source, Digest string
}

type registryClient interface {
	ChartVersions(string) ([]string, error)
	ImageVersions(string) ([]string, error)
	Digest(string) (string, error)
	Verify(pin) error
}

type registry struct {
	URLs     map[string]string
	tags     map[string][]string
	prepared map[string]bool
	checked  int
}

func command(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if failure, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(failure.Stderr))
		}
		if len(stderr) > 600 {
			stderr = stderr[:600]
		}
		return "", fmt.Errorf("%s failed: %s: %w", strings.Join(args[:min(3, len(args))], " "), stderr, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

func semver(value string) (string, [4]int, bool, bool) {
	parts := semverRE.FindStringSubmatch(value)
	if parts == nil {
		return "", [4]int{}, false, false
	}
	var nums [4]int
	for i := 1; i <= 3; i++ {
		value, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return "", [4]int{}, false, false
		}
		nums[i-1] = value
	}
	if parts[5] != "" {
		value, err := strconv.Atoi(parts[5])
		if err != nil {
			return "", [4]int{}, false, false
		}
		nums[3] = value
	}
	return parts[1], nums, parts[5] != "", true
}

func newer(current string, tags []string) string {
	prefix, old, build, ok := semver(current)
	if !ok {
		return ""
	}
	best, newest := old, ""
	for _, tag := range tags {
		p, n, b, valid := semver(tag)
		if !valid || p != prefix || b != build {
			continue
		}
		greater := false
		for i := range n {
			if n[i] != best[i] {
				greater = n[i] > best[i]
				break
			}
		}
		if greater {
			best, newest = n, tag
		}
	}
	return newest
}

func scalar(line string) string {
	match := keyRE.FindStringSubmatch(line)
	if match == nil || match[3] == "" {
		return ""
	}
	value := strings.TrimSpace(strings.SplitN(match[3], " #", 2)[0])
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[0] == value[len(value)-1] {
		value = value[1 : len(value)-1]
	}
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return ""
	}
	return value
}

func replaceScalar(line, target string) (string, error) {
	match := keyRE.FindStringSubmatchIndex(line)
	if match == nil || match[6] < 0 {
		return "", fmt.Errorf("cannot replace scalar: %s", line)
	}
	raw := line[match[6]:match[7]]
	parts := strings.SplitN(raw, " #", 2)
	quote := ""
	trimmed := strings.TrimSpace(parts[0])
	if strings.HasPrefix(trimmed, "\"") {
		quote = "\""
	} else if strings.HasPrefix(trimmed, "'") {
		quote = "'"
	}
	result := line[:match[6]] + quote + target + quote
	if len(parts) == 2 {
		result += " #" + parts[1]
	}
	return result, nil
}

func matchingEnd(lines []string, start int) int {
	depth := 0
	for i := start; i < len(lines); i++ {
		for _, match := range actionRE.FindAllStringSubmatch(lines[i], -1) {
			if match[1] == "end" {
				depth--
			} else {
				depth++
			}
			if depth == 0 {
				return i
			}
		}
	}
	return len(lines)
}

func conditionalLine(lines []string, line int) bool {
	for i := 0; i <= line; i++ {
		if match := actionRE.FindStringSubmatch(lines[i]); match != nil && match[1] != "end" && matchingEnd(lines, i) >= line {
			return true
		}
	}
	return false
}

func substitute(text, variable, value string) string {
	re := regexp.MustCompile(`\{\{-?\s*\$` + regexp.QuoteMeta(variable) + `\s*-?\}\}`)
	return re.ReplaceAllString(text, value)
}

func releaseBlocks(lines []string) []releaseBlock {
	type r struct {
		start, end int
		variable   string
		values     []string
	}
	var ranges []r
	for i, line := range lines {
		if match := rangeRE.FindStringSubmatch(line); match != nil {
			item := r{start: i, end: matchingEnd(lines, i), variable: match[1]}
			for _, quoted := range quotedRE.FindAllStringSubmatch(match[2], -1) {
				item.values = append(item.values, quoted[1])
			}
			ranges = append(ranges, item)
		}
	}
	var starts []releaseBlock
	for i, line := range lines {
		if match := releaseRE.FindStringSubmatch(line); match != nil {
			starts = append(starts, releaseBlock{Start: i, Name: match[1]})
		}
	}
	var blocks []releaseBlock
	for i, item := range starts {
		item.End = len(lines)
		if i+1 < len(starts) {
			item.End = starts[i+1].Start
		}
		var ranged *r
		for j := range ranges {
			if ranges[j].start < item.Start && item.Start < ranges[j].end {
				ranged = &ranges[j]
				break
			}
		}
		if ranged == nil {
			blocks = append(blocks, item)
			continue
		}
		item.End = min(item.End, ranged.end)
		for _, value := range ranged.values {
			blocks = append(blocks, releaseBlock{Start: item.Start, End: item.End, Name: substitute(item.Name, ranged.variable, value), Binding: ranged.variable, Value: value})
		}
	}
	return blocks
}

func releases(root string) ([]releaseSpec, []string, error) {
	lines, err := readLines(filepath.Join(root, helmfilePath))
	if err != nil {
		return nil, nil, err
	}
	var found []releaseSpec
	var skipped []string
	owners := map[int]string{}
	for _, block := range releaseBlocks(lines) {
		fields := map[string]string{}
		var values []string
		blockLines := make([]string, 0, block.End-block.Start)
		for _, line := range lines[block.Start:block.End] {
			if block.Binding != "" {
				line = substitute(line, block.Binding, block.Value)
			}
			blockLines = append(blockLines, line)
			if match := fieldRE.FindStringSubmatch(line); match != nil {
				fields[match[1]] = match[2]
			}
			if match := valueFileRE.FindStringSubmatch(line); match != nil {
				values = append(values, match[1])
			}
		}
		if strings.Contains(block.Name, "{{") || fields["chart"] == "" || strings.Contains(fields["chart"], "{{") {
			skipped = append(skipped, block.Name+": dynamic release name or chart")
			continue
		}
		owner := block.Name
		if block.Binding != "" {
			if prior, ok := owners[block.Start]; ok {
				owner = prior
			} else {
				owners[block.Start] = block.Name
			}
		}
		if owner == block.Name {
			owner = ""
		}
		found = append(found, releaseSpec{Name: block.Name, Chart: fields["chart"], Version: fields["version"], Values: values, ChartOwner: owner, UnsupportedValues: hasUnsupportedReleaseValues(blockLines)})
	}
	return found, skipped, nil
}

func hasUnsupportedReleaseValues(lines []string) bool {
	inValues, seenValues := false, false
	for i, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if match := keyRE.FindStringSubmatch(line); match != nil && indent == 4 {
			inValues = match[2] == "values"
			switch match[2] {
			case "values":
				if seenValues || match[3] != "" || conditionalLine(lines, i+1) {
					return true
				}
				seenValues = true
			case "valuesTemplate", "set", "setString", "setTemplate", "inherit", "secrets":
				return true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "{{") {
			if inValues && indent >= 4 || indent <= 6 && !isNonRenderingAction(trimmed) {
				return true
			}
			continue
		}
		if indent < 4 {
			inValues = false
			continue
		}
		if inValues {
			match := valueFileRE.FindStringSubmatch(line)
			if match == nil || !filepath.IsLocal(match[1]) || conditionalLine(lines, i+1) {
				return true
			}
		} else if indent == 4 || valueFileRE.MatchString(line) {
			return true
		}
	}
	return false
}

func repoURLs(root string) (map[string]string, error) {
	lines, err := readLines(filepath.Join(root, reposPath))
	if err != nil {
		return nil, err
	}
	urls := map[string]string{}
	name := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "  - name: ") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "  - name: "))
		}
		if name != "" && strings.HasPrefix(line, "    url: https://") {
			urls[name] = strings.TrimSpace(strings.TrimPrefix(line, "    url: "))
		}
	}
	return urls, nil
}

func yamlFields(path string) ([]yamlField, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var stack, fields []yamlField
	for i, line := range lines {
		match := keyRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])
		for len(stack) > 0 && stack[len(stack)-1].Indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parents := make([]string, len(stack))
		for j, item := range stack {
			parents[j] = item.Key
		}
		field := yamlField{Line: i, Indent: indent, Key: match[2], Value: scalar(line), Parents: strings.Join(parents, "\x00")}
		fields = append(fields, field)
		if match[3] == "" {
			stack = append(stack, field)
		}
	}
	return fields, nil
}

func imageEntries(root, relative string) ([]imageEntry, error) {
	fields, err := yamlFields(filepath.Join(root, relative))
	if err != nil {
		return nil, err
	}
	repositories, digests := map[string]string{}, map[string]string{}
	for _, field := range fields {
		if field.Key == "repository" && field.Value != "" {
			repositories[field.Parents] = field.Value
		}
		if field.Key == "digest" && field.Value != "" {
			digests[field.Parents] = field.Value
		}
	}
	var entries []imageEntry
	for _, field := range fields {
		if field.Key != "tag" {
			continue
		}
		_, mapped := imageSources[relative]
		if !strings.Contains("\x00"+field.Parents+"\x00", "\x00image\x00") && !mapped {
			continue
		}
		source := repositories[field.Parents]
		if source == "" {
			source = imageSources[relative]
		}
		entries = append(entries, imageEntry{Ordinal: len(entries), Line: field.Line, Tag: field.Value, Source: source, Digest: digests[field.Parents]})
	}
	return entries, nil
}

func valuePaths(spec releaseSpec) []string {
	paths := make([]string, 0, len(spec.Values)+1)
	for _, value := range spec.Values {
		paths = append(paths, "helmfile-app/"+value)
	}
	if strings.HasPrefix(spec.Chart, "charts/") {
		paths = append(paths, "helmfile-app/"+spec.Chart+"/values.yaml")
	}
	return paths
}

func newRegistry(root string) (*registry, error) {
	urls, err := repoURLs(root)
	if err != nil {
		return nil, err
	}
	return &registry{URLs: urls, tags: map[string][]string{}, prepared: map[string]bool{}}, nil
}

func (r *registry) ChartVersions(source string) ([]string, error) {
	key := "chart:" + source
	if tags, ok := r.tags[key]; ok {
		return tags, nil
	}
	var tags []string
	if strings.HasPrefix(source, "oci://") {
		found, err := skopeoTags(strings.TrimPrefix(source, "oci://"))
		if err != nil {
			return nil, err
		}
		tags = found
	} else if parts := strings.SplitN(source, "/", 2); len(parts) == 2 && r.URLs[parts[0]] != "" {
		if !r.prepared[parts[0]] {
			if _, err := command("", "helm", "repo", "add", parts[0], r.URLs[parts[0]], "--force-update"); err != nil {
				return nil, err
			}
			if _, err := command("", "helm", "repo", "update", parts[0]); err != nil {
				return nil, err
			}
			r.prepared[parts[0]] = true
		}
		out, err := command("", "helm", "search", "repo", source, "--versions", "--output", "json")
		if err != nil {
			return nil, err
		}
		var result []struct{ Name, Version string }
		if err = json.Unmarshal([]byte(out), &result); err != nil {
			return nil, err
		}
		for _, item := range result {
			if item.Name == source {
				tags = append(tags, item.Version)
			}
		}
	} else {
		return nil, fmt.Errorf("chart repository is not configured: %s", source)
	}
	r.tags[key] = tags
	r.checked++
	return tags, nil
}

func (r *registry) ImageVersions(source string) ([]string, error) {
	key := "image:" + source
	if tags, ok := r.tags[key]; ok {
		return tags, nil
	}
	tags, err := skopeoTags(source)
	if err != nil {
		return nil, err
	}
	r.tags[key] = tags
	r.checked++
	return tags, nil
}

func skopeoTags(source string) ([]string, error) {
	out, err := command("", "skopeo", "list-tags", "docker://"+source)
	if err != nil {
		return nil, err
	}
	var result struct{ Tags []string }
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, err
	}
	return result.Tags, nil
}

func (r *registry) Digest(reference string) (string, error) {
	cmd := exec.Command("skopeo", "inspect", "--raw", "docker://"+reference)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(out)), nil
}

func (r *registry) Verify(p pin) error {
	if p.Kind == "chart" && !strings.HasPrefix(p.Source, "oci://") {
		_, err := command("", "helm", "show", "chart", p.Source, "--version", p.Target)
		return err
	}
	ref := strings.TrimPrefix(p.Source, "oci://") + ":" + p.Target
	digest, err := r.Digest(ref)
	if err != nil {
		return err
	}
	if p.Digest != "" && digest != p.Digest {
		return fmt.Errorf("digest changed while checking %s", ref)
	}
	return nil
}

func plan(root string, registry registryClient) (map[string][]pin, []string, error) {
	specs, skipped, err := releases(root)
	if err != nil {
		return nil, nil, err
	}
	users := map[string]map[string]bool{}
	for _, spec := range specs {
		for _, path := range valuePaths(spec) {
			if users[path] == nil {
				users[path] = map[string]bool{}
			}
			users[path][spec.Name] = true
		}
	}
	result := map[string][]pin{}
	for _, spec := range specs {
		var pins []pin
		if spec.ChartOwner != "" {
			skipped = append(skipped, fmt.Sprintf("%s: chart version shared with %s", spec.Name, spec.ChartOwner))
		} else if spec.Version != "" && !strings.HasPrefix(spec.Chart, "charts/") && !strings.HasPrefix(spec.Chart, "../") {
			tags, e := registry.ChartVersions(spec.Chart)
			if e == nil {
				if target := newer(spec.Version, tags); target != "" {
					p := pin{Release: spec.Name, Kind: "chart", Path: helmfilePath, Current: spec.Version, Target: target, Source: spec.Chart}
					e = registry.Verify(p)
					if e == nil {
						pins = append(pins, p)
					}
				}
			}
			if e != nil {
				skipped = append(skipped, fmt.Sprintf("%s: chart check failed (%v)", spec.Name, e))
			}
		}
		for _, relative := range valuePaths(spec) {
			path := filepath.Join(root, relative)
			info, e := os.Stat(path)
			if e != nil || info.IsDir() || filepath.Base(path) == "secrets.yaml" {
				skipped = append(skipped, fmt.Sprintf("%s: values file unavailable for static scan: %s", spec.Name, relative))
				continue
			}
			if len(users[relative]) > 1 {
				skipped = append(skipped, fmt.Sprintf("%s: shared values file needs coordinated update: %s", spec.Name, relative))
				continue
			}
			fields, e := yamlFields(path)
			if e != nil {
				return nil, nil, e
			}
			for _, field := range fields {
				if field.Key == "image" && field.Value != "" {
					skipped = append(skipped, fmt.Sprintf("%s: direct image reference needs manual source mapping: %s", spec.Name, relative))
					break
				}
			}
			entries, e := imageEntries(root, relative)
			if e != nil {
				return nil, nil, e
			}
			for _, entry := range entries {
				if entry.Source == "" {
					skipped = append(skipped, fmt.Sprintf("%s: image pin cannot be resolved safely: %s", spec.Name, relative))
					continue
				}
				if _, _, _, ok := semver(entry.Tag); !ok {
					skipped = append(skipped, fmt.Sprintf("%s: image pin cannot be resolved safely: %s", spec.Name, relative))
					continue
				}
				tags, e := registry.ImageVersions(entry.Source)
				if e == nil {
					if target := newer(entry.Tag, tags); target != "" {
						digest := ""
						if entry.Digest != "" {
							digest, e = registry.Digest(entry.Source + ":" + target)
						}
						p := pin{Release: spec.Name, Kind: "image", Path: relative, Ordinal: entry.Ordinal, Current: entry.Tag, Target: target, Source: entry.Source, Digest: digest}
						if e == nil {
							e = registry.Verify(p)
						}
						if e == nil {
							pins = append(pins, p)
						}
					}
				}
				if e != nil {
					skipped = append(skipped, fmt.Sprintf("%s: image check failed for %s (%v)", spec.Name, relative, e))
				}
			}
		}
		if len(pins) > 0 {
			if err := validateRuntime(root, spec, pins); err != nil {
				skipped = append(skipped, fmt.Sprintf("%s: %v", spec.Name, err))
				continue
			}
			result[spec.Name] = pins
		}
	}
	return result, skipped, nil
}

func applyPins(root, release string, pins []pin) ([]string, error) {
	specs, _, err := releases(root)
	if err != nil {
		return nil, err
	}
	var spec *releaseSpec
	for i := range specs {
		if specs[i].Name == release {
			spec = &specs[i]
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("release disappeared from branch: %s", release)
	}
	changed := map[string]bool{}
	for _, p := range pins {
		if p.Kind == "chart" && spec.Chart != p.Source {
			return nil, fmt.Errorf("chart source changed on branch: %s", release)
		}
		if p.Kind == "image" {
			found := false
			for _, value := range valuePaths(*spec) {
				if value == p.Path {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("image values file changed on branch: %s", release)
			}
		}
		path := filepath.Join(root, p.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		index := -1
		if p.Kind == "chart" {
			for _, block := range releaseBlocks(lines) {
				if block.Name == release {
					for i := block.Start; i < block.End; i++ {
						if strings.HasPrefix(lines[i], "    version:") {
							if index != -1 {
								return nil, fmt.Errorf("chart version is ambiguous for %s", release)
							}
							index = i
						}
					}
					break
				}
			}
		} else {
			entries, err := imageEntries(root, p.Path)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Ordinal == p.Ordinal {
					if entry.Source != p.Source {
						return nil, fmt.Errorf("image source changed on branch: %s: %s", release, p.Path)
					}
					index = entry.Line
					break
				}
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("pin is ambiguous for %s: %s", release, p.Path)
		}
		current := scalar(lines[index])
		_, old, _, oldOK := semver(current)
		_, target, _, targetOK := semver(p.Target)
		if !oldOK || !targetOK {
			return nil, fmt.Errorf("pin changed to unsupported form: %s", p.Path)
		}
		greater := false
		for i := range old {
			if old[i] != target[i] {
				greater = old[i] > target[i]
				break
			}
		}
		if old == target || greater {
			continue
		}
		lines[index], err = replaceScalar(lines[index], p.Target)
		if err != nil {
			return nil, err
		}
		if p.Kind == "image" && p.Digest != "" {
			fields, err := yamlFields(path)
			if err != nil {
				return nil, err
			}
			parents := ""
			for _, field := range fields {
				if field.Line == index {
					parents = field.Parents
					break
				}
			}
			digestIndex := -1
			for _, field := range fields {
				if field.Key == "digest" && field.Parents == parents {
					if digestIndex != -1 {
						return nil, fmt.Errorf("image digest is ambiguous for %s: %s", release, p.Path)
					}
					digestIndex = field.Line
				}
			}
			if digestIndex < 0 {
				return nil, fmt.Errorf("image digest is ambiguous for %s: %s", release, p.Path)
			}
			lines[digestIndex], err = replaceScalar(lines[digestIndex], p.Digest)
			if err != nil {
				return nil, err
			}
		}
		if err = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return nil, err
		}
		changed[p.Path] = true
	}
	var paths []string
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
