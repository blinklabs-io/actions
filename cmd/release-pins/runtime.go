// Copyright 2026 Blink Labs Software

package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const cardanoImage = "ghcr.io/blinklabs-io/cardano-node"
const cardanoChart = "oci://ghcr.io/blinklabs-io/helm-charts/charts/cardano-node"

// validateRuntime checks configuration initialization, not synchronization or deployment readiness.
func validateRuntime(root string, spec releaseSpec, pins []pin) error {
	cardano := spec.Chart == cardanoChart
	for _, p := range pins {
		cardano = cardano || p.Kind == "image" && p.Source == cardanoImage
	}
	if !cardano {
		return nil
	}
	if spec.Chart != cardanoChart {
		return fmt.Errorf("cardano runtime configuration needs manual mapping for chart %s", spec.Chart)
	}
	if spec.UnsupportedValues {
		return fmt.Errorf("cardano runtime requires a static list of values files")
	}
	values := map[string]string{}
	for _, relative := range valuePaths(spec) {
		fields, err := yamlFields(filepath.Join(root, relative))
		if err != nil {
			return fmt.Errorf("cannot resolve runtime values: %w", err)
		}
		lines, err := readLines(filepath.Join(root, relative))
		if err != nil {
			return err
		}
		if !hasStaticRuntimeKeys(lines) {
			return fmt.Errorf("unsupported runtime key or template in %s", relative)
		}
		entries, err := imageEntries(root, relative)
		if err != nil {
			return err
		}
		targets := map[int]string{}
		for _, p := range pins {
			if p.Kind == "image" && p.Path == relative && p.Source == cardanoImage && p.Ordinal >= 0 && p.Ordinal < len(entries) {
				targets[entries[p.Ordinal].Line] = p.Target
			}
		}
		seen := map[string]bool{}
		for _, field := range fields {
			key := field.Parents + "\x00" + field.Key
			switch key {
			case "\x00image", "\x00cardano_network", "\x00cardano_config", "image\x00repository", "image\x00tag":
			default:
				continue
			}
			if seen[key] {
				return fmt.Errorf("ambiguous or dynamic runtime value %s in %s", field.Key, relative)
			}
			if conditionalLine(lines, field.Line) {
				return fmt.Errorf("conditional runtime value %s in %s", field.Key, relative)
			}
			seen[key] = true
			if field.Value == "" && (strings.Contains(lines[field.Line], "{{") || strings.Contains(lines[field.Line], "}}")) {
				return fmt.Errorf("dynamic runtime value %s in %s", field.Key, relative)
			}
			var value yaml.Node
			if err := yaml.Unmarshal([]byte(strings.TrimSpace(lines[field.Line])), &value); err != nil {
				return fmt.Errorf("unsupported runtime value %s in %s", field.Key, relative)
			}
			node := value.Content[0].Content[1]
			if key == "\x00image" && strings.TrimSpace(lines[field.Line]) == "image:" {
				continue
			}
			if key == "\x00image" || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Anchor != "" || strings.ContainsAny(node.Value, "\r\n") || strings.Contains(node.Value, "{{") {
				return fmt.Errorf("unsupported or dynamic runtime value %s in %s", field.Key, relative)
			}
			values[key] = node.Value
			if target, ok := targets[field.Line]; ok && key == "image\x00tag" {
				values[key] = target
			}
		}
	}
	if values["image\x00repository"] != cardanoImage {
		return fmt.Errorf("cardano runtime image needs manual source mapping")
	}
	network := values["\x00cardano_network"]
	if network == "" || strings.ContainsAny(network, "/\\ \t\n") || network == "." || network == ".." {
		return fmt.Errorf("cardano network must be a static directory name")
	}
	config := values["\x00cardano_config"]
	if config == "" {
		config = "/opt/cardano/config/" + network + "/config.json"
	}
	if !strings.HasPrefix(config, "/opt/cardano/config/") || path.Clean(config) != config {
		return fmt.Errorf("cardano configuration must be a bundled file: %s", config)
	}
	tag := values["image\x00tag"]
	if tag == "" || strings.ContainsAny(tag, " \t\n") {
		return fmt.Errorf("cardano image tag must be static")
	}
	return checkCardanoRuntime(cardanoImage+":"+tag, config, "/opt/cardano/config/"+network+"/topology.json")
}

func isNonRenderingAction(line string) bool {
	if !strings.HasPrefix(line, "{{") || !strings.HasSuffix(line, "}}") || strings.Count(line, "{{") != 1 || strings.Count(line, "}}") != 1 {
		return false
	}
	action := strings.TrimSpace(strings.Trim(line[2:len(line)-2], "-"))
	control := actionRE.FindStringSubmatch("{{ " + action)
	return control != nil && control[1] != "block" ||
		action == "else" || strings.HasPrefix(action, "else ") ||
		strings.HasPrefix(action, "/*") && strings.HasSuffix(action, "*/") ||
		assignmentRE.MatchString(action)
}

func hasStaticRuntimeKeys(lines []string) bool {
	inImage, seenRoot := false, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if strings.HasPrefix(trimmed, "{{") {
			if (indent == 0 || inImage || !seenRoot) && !isNonRenderingAction(trimmed) {
				return false
			}
			continue
		}
		if indent > 0 && !inImage && seenRoot {
			continue
		}
		match := keyRE.FindStringSubmatch(line)
		if match == nil || !seenRoot && indent != 0 {
			return false
		}
		if indent == 0 {
			seenRoot = true
			inImage = match[2] == "image"
		}
	}
	return true
}

func checkCardanoRuntime(reference, config, topology string) error {
	engine := "docker"
	if runtime.GOOS == "darwin" {
		engine = "container"
	}
	name := "release-pin-check-" + rand.Text()
	// No mounts, credentials, or network are passed to the candidate image.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pull := []string{"pull", reference}
	if engine == "container" {
		pull = append([]string{"image"}, pull...)
	}
	if out, err := exec.CommandContext(ctx, engine, pull...).CombinedOutput(); err != nil {
		return fmt.Errorf("runtime image pull failed for %s: %w: %.600s", reference, err, out)
	}
	cmd := exec.CommandContext(ctx, engine, "run", "--rm", "--name", name,
		"--network", "none", "--cap-drop", "ALL", "--cpus", "1", "--memory", "1G",
		"--entrypoint", "timeout", reference, "-k", "5", "30", "cardano-node", "run",
		"--config", config, "--topology", topology, "--database-path", "/tmp/db",
		"--socket-path", "/tmp/node.socket", "--host-addr", "127.0.0.1", "--shutdown-ipc", "0")
	// Closed stdin requests a graceful exit after configuration and tracing initialization.
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, engine, "rm", "--force", name).Run()
		message := strings.TrimSpace(string(out))
		if len(message) > 2400 {
			message = message[:1200] + "\n...\n" + message[len(message)-1200:]
		}
		return fmt.Errorf("runtime initialization failed for %s with %s: %w: %s", reference, config, err, message)
	}
	return nil
}
