// Package secretpack projects secret-detector findings into the repository
// graph. It records fingerprints and source ranges only. Credential values are
// never copied into nodes, evidence, diagnostics, or generated prose.
package secretpack

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/repository-knowledge-compiler/rkc/internal/security/secrets"
	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.secret-scan"
	PluginVersion = "0.2.0"
)

type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

func Extract(options Options) (rkcmodel.Fragment, error) {
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	fragment := rkcmodel.Fragment{}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(options.Root, filepath.FromSlash(file.Path)))
		if err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, rkcmodel.Diagnostic{
				ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, err.Error()), Severity: "error", Code: "RKC-SEC-1001",
				Message: "secret scan could not read artifact: " + err.Error(), Stage: "security_scan", Plugin: PluginID,
				Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path},
			})
			continue
		}
		for ordinal, finding := range secrets.Scan(data) {
			source := &rkcmodel.SourceRange{
				ArtifactID: file.ArtifactID, Path: file.Path, StartByte: int64(finding.StartByte), EndByte: int64(finding.EndByte),
				StartLine: finding.StartLine, StartColumn: finding.StartColumn, EndLine: finding.EndLine, EndColumn: finding.EndColumn,
			}
			evidenceID := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, finding.Kind, finding.Fingerprint, fmt.Sprint(ordinal))
			fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "security_scan", Method: "credential_pattern", Confidence: finding.Confidence,
				Source: source, Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256,
				Detail:     finding.Kind + ":" + finding.Fingerprint,
				Attributes: map[string]any{"fingerprint": finding.Fingerprint, "secret_kind": finding.Kind, "key_name": finding.KeyName, "value_retained": false},
			})
			nodeID := rkcmodel.StableID("node", "secret", file.Path, finding.Kind, finding.Fingerprint)
			fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
				ID: nodeID, LogicalID: rkcmodel.StableID("logical", "secret_finding", file.Path, finding.Kind, finding.Fingerprint),
				Kind: "secret", Name: finding.Kind, QualifiedName: fmt.Sprintf("%s:%d %s", file.Path, finding.StartLine, finding.Kind),
				Language: file.Language, Visibility: "restricted", ArtifactID: file.ArtifactID, Source: source, EvidenceIDs: []string{evidenceID},
				Attributes: map[string]any{"confidence": finding.Confidence, "fingerprint": finding.Fingerprint, "key_name": finding.KeyName, "value_retained": false, "requires_review": true},
			})
			fragment.Edges = append(fragment.Edges, rkcmodel.Edge{
				ID: rkcmodel.StableID("edge", "contains", file.ArtifactID, nodeID, evidenceID), Kind: "contains", From: file.ArtifactID, To: nodeID,
				Resolution: "syntax_inferred", Confidence: finding.Confidence, Producer: PluginID, EvidenceIDs: []string{evidenceID},
			})
			severity := "warning"
			code := "RKC-SEC-1101"
			if finding.Confidence >= 0.95 {
				code = "RKC-SEC-1102"
			}
			fragment.Diagnostics = append(fragment.Diagnostics, rkcmodel.Diagnostic{
				ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, finding.Kind, finding.Fingerprint), Severity: severity, Code: code,
				Message: fmt.Sprintf("potential %s detected; value withheld; fingerprint %s", finding.Kind, finding.Fingerprint),
				Source:  source, Stage: "security_scan", Plugin: PluginID,
				Attributes: map[string]any{"confidence": finding.Confidence, "fingerprint": finding.Fingerprint, "secret_kind": finding.Kind, "value_retained": false},
			})
		}
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}
