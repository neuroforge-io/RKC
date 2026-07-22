// Package envkeys extracts environment-variable contracts from template and
// example files. It does not read process environment values and never records
// probable secrets in clear text.
package envkeys

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.envkeys"
	PluginVersion = "0.2.0"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var secretWords = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "PRIVATE_KEY", "API_KEY", "CREDENTIAL"}

type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

func Extract(options Options) (rkcmodel.Fragment, error) {
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	seen := map[string]struct{}{}
	for _, file := range files {
		input, err := os.Open(filepath.Join(options.Root, filepath.FromSlash(file.Path)))
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
			defaultValue := value
			if secret && defaultValue != "" {
				defaultValue = "<redacted>"
			}
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
			fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
				ID: id, LogicalID: rkcmodel.StableID("logical", "environment_variable", name), Kind: "environment_variable",
				Name: name, QualifiedName: name, Language: "dotenv", Visibility: "deployment", PublicSurface: false,
				ArtifactID: file.ArtifactID, Source: source, EvidenceIDs: []string{evidenceID},
				Attributes: map[string]any{"default": defaultValue, "has_default": value != "", "secret_like": secret, "source_file": file.Path},
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
