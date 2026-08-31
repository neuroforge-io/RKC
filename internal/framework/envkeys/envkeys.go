// Package envkeys extracts environment-variable contracts from template and
// example files. It does not read process environment values and never records
// probable secrets in clear text.
package envkeys

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	// PluginID is the stable producer identity attached to every extracted
	// environment-contract fact.
	PluginID = "rkc.envkeys"
	// PluginVersion identifies the extractor semantics recorded in evidence.
	PluginVersion = "0.3.0"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var secretWords = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "PRIVATE_KEY", "API_KEY", "CREDENTIAL"}

// Options supplies the trusted repository root and admitted regular-file
// references to inspect; Extract never discovers additional paths itself.
type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

// Extract parses deterministic dotenv-style assignments into evidence-backed
// environment-variable nodes. Default values are classified and fingerprinted,
// never copied into the atlas; secret-like defaults are not fingerprinted so a
// low-entropy credential cannot be recovered by guessing. Unreadable files
// become diagnostics, and no process environment is consulted.
func Extract(options Options) (rkcmodel.Fragment, error) {
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	seen := map[string]struct{}{}
	for _, file := range files {
		input, err := sourcepath.OpenRegular(options.Root, file.Path)
		if err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-CFG-1001", err.Error()))
			continue
		}
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			parts := strings.SplitN(line, "=", 2)
			name := strings.TrimSpace(parts[0])
			if !keyPattern.MatchString(name) {
				continue
			}
			value := ""
			if len(parts) == 2 {
				value = strings.TrimSpace(parts[1])
			}
			secret := likelySecret(name)
			id := rkcmodel.StableID("node", "environment_variable", name)
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			evidenceID := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, fmt.Sprint(lineNumber), name)
			source := &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: lineNumber, EndLine: lineNumber}
			fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "declared", Method: "dotenv.assignment", Confidence: 1, Source: source,
				Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: name,
			})
			attributes := map[string]any{
				"has_default": value != "", "secret_like": secret, "source_file": file.Path,
			}
			if value != "" && !secret {
				attributes["default_sha256"] = privateValueDigest("dotenv-default", value)
				attributes["default_class"] = scalarClassification(value)
				attributes["default_length_bytes"] = len(value)
			}
			fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
				ID: id, LogicalID: rkcmodel.StableID("logical", "environment_variable", name), Kind: "environment_variable",
				Name: name, QualifiedName: name, Language: "dotenv", Visibility: "deployment", PublicSurface: false,
				ArtifactID: file.ArtifactID, Source: source, EvidenceIDs: []string{evidenceID},
				Attributes: attributes,
			})
			fragment.Edges = append(fragment.Edges, rkcmodel.Edge{
				ID: rkcmodel.StableID("edge", "declares", file.ArtifactID, id, evidenceID), Kind: "declares",
				From: file.ArtifactID, To: id, Resolution: "declared", Confidence: 1, Producer: PluginID, EvidenceIDs: []string{evidenceID},
			})
		}
		if err := scanner.Err(); err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-CFG-1002", err.Error()))
		}
		_ = input.Close()
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}

func privateValueDigest(domain, value string) string {
	digest := sha256.Sum256([]byte(PluginID + "\x00" + domain + "\x00" + value))
	return fmt.Sprintf("%x", digest)
}

func scalarClassification(value string) string {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return "empty"
	case strings.Contains(trimmed, "${") || strings.Contains(trimmed, "$(") || strings.Contains(trimmed, "{{"):
		return "expression"
	case lower == "true" || lower == "false" || lower == "yes" || lower == "no" || lower == "on" || lower == "off":
		return "boolean"
	case func() bool { _, err := strconv.ParseFloat(trimmed, 64); return err == nil }():
		return "number"
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return "url"
	case strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../"):
		return "path"
	default:
		return "literal"
	}
}

// IsCandidate reports whether a basename follows RKC's closed set of dotenv
// and environment-template naming conventions; directory names do not affect
// the decision.
func IsCandidate(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" {
		return true
	}
	return strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") || strings.Contains(base, "env.example") || strings.Contains(base, "env.sample") || strings.Contains(base, "env.template")
}

func likelySecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, word := range secretWords {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return false
}

func diagnostic(file pluginapi.FileRef, code, message string) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, code, message), Severity: "error", Code: code, Message: message, Stage: "framework_configuration", Plugin: PluginID, Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path}}
}
