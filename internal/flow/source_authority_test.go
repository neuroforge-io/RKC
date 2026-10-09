package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/lang/goast"
	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestFlowUsesPrivateSourceAuthorityForRedactedDisplayNames(t *testing.T) {
	root := t.TempDir()
	const source = "package sample\n\nfunc Run() int { return 1 }\n"
	const rawPath = "synthetic.go"
	if err := os.WriteFile(filepath.Join(root, rawPath), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(source))
	file := pluginapi.FileRef{ArtifactID: rkcmodel.StableID("artifact", rawPath),
		Path: rawPath, Language: "go", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(source))}
	fragment, err := goast.Extract(goast.Options{Root: root, Files: []pluginapi.FileRef{file}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{{ID: file.ArtifactID,
		Path: rawPath, Language: "go", SHA256: file.SHA256, SizeBytes: file.SizeBytes, Text: true}},
		Nodes: fragment.Nodes, Edges: fragment.Edges, Evidence: fragment.Evidence}
	secrets.SanitizeBundle(&bundle, []string{"synthetic"})
	if bundle.Artifacts[0].Path != "[REDACTED].go" {
		t.Fatal("fixture did not redact the canonical display path")
	}
	options := Options{Root: root, Files: []pluginapi.FileRef{file}, Artifacts: bundle.Artifacts, Bundle: bundle}
	got, stats, err := Analyze(context.Background(), options)
	if err != nil || stats.CFGFunctions != 1 || stats.ValueFunctions != 1 {
		t.Fatalf("private source authority lost analysis: %v %+v", err, stats)
	}
	data, err := json.Marshal(got)
	if err != nil || strings.Contains(string(data), rawPath) {
		t.Fatalf("raw source authority entered canonical flow records: %v %s", err, data)
	}
	for _, files := range [][]pluginapi.FileRef{nil, {}, {file, file}, {{ArtifactID: "different"}}} {
		options.Files = files
		got, stats, err := Analyze(context.Background(), options)
		if err != nil || stats.CFGFunctions != 0 || len(got.Diagnostics) == 0 {
			t.Fatalf("missing/ambiguous authority produced source facts: %v %+v %+v", err, stats, got)
		}
	}
}

func TestInventoriedFlowSourceRejectsChangedAndUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	const source = "package sample\n"
	const rawPath = "synthetic.go"
	absolute := filepath.Join(root, rawPath)
	if err := os.WriteFile(absolute, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(source))
	file := pluginapi.FileRef{ArtifactID: rkcmodel.StableID("artifact", rawPath), Path: rawPath,
		Language: "go", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(source))}
	artifact := rkcmodel.Artifact{ID: file.ArtifactID, Path: "[REDACTED].go", Language: "go",
		SHA256: file.SHA256, SizeBytes: file.SizeBytes, Text: true}
	for name, change := range map[string]func(*rkcmodel.Artifact, *pluginapi.FileRef){
		"binary":         func(a *rkcmodel.Artifact, _ *pluginapi.FileRef) { a.Text = false },
		"language":       func(_ *rkcmodel.Artifact, f *pluginapi.FileRef) { f.Language = "python" },
		"identity":       func(_ *rkcmodel.Artifact, f *pluginapi.FileRef) { f.ArtifactID = "other" },
		"path binding":   func(_ *rkcmodel.Artifact, f *pluginapi.FileRef) { f.Path = "other.go" },
		"digest binding": func(_ *rkcmodel.Artifact, f *pluginapi.FileRef) { f.SHA256 = strings.Repeat("a", 64) },
		"size binding":   func(_ *rkcmodel.Artifact, f *pluginapi.FileRef) { f.SizeBytes++ },
		"negative size":  func(a *rkcmodel.Artifact, f *pluginapi.FileRef) { a.SizeBytes = -1; f.SizeBytes = -1 },
		"overflow": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) {
			a.SizeBytes = int64(^uint64(0) >> 1)
			f.SizeBytes = a.SizeBytes
		},
		"invalid digest": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) { a.SHA256 = "invalid"; f.SHA256 = a.SHA256 },
		"changed digest": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) {
			a.SHA256 = strings.Repeat("a", 64)
			f.SHA256 = a.SHA256
		},
		"changed size": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) { a.SizeBytes++; f.SizeBytes = a.SizeBytes },
		"missing": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) {
			f.Path = "missing.go"
			f.ArtifactID = rkcmodel.StableID("artifact", f.Path)
			a.ID = f.ArtifactID
		},
		"escape": func(a *rkcmodel.Artifact, f *pluginapi.FileRef) {
			f.Path = "../synthetic.go"
			f.ArtifactID = rkcmodel.StableID("artifact", f.Path)
			a.ID = f.ArtifactID
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, f := artifact, file
			change(&a, &f)
			_, err := readInventoriedFlowSource(root, a, f)
			if err == nil || strings.Contains(err.Error(), "synthetic") {
				t.Fatalf("unsafe source accepted or private path disclosed: %v", err)
			}
		})
	}
	if data, err := readInventoriedFlowSource(root, artifact, file); err != nil || string(data) != source {
		t.Fatalf("verified original source rejected: %v", err)
	}
}
