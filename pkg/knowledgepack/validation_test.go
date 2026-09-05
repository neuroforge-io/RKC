package knowledgepack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func rewriteJSON(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMalformedManifestAndOwnershipBoundaries(t *testing.T) {
	for _, kind := range []string{"version", "identity", "counts", "missing-option", "invalid-option", "receipts", "filename", "digest", "negative-size", "missing-payload", "missing-manifest", "invalid-manifest-json", "invalid-options", "unknown-options", "oversized-readme", "invalid-outer-json", "wrong-outer", "invalid-marker-json", "wrong-marker", "wrong-snapshot", "directory-marker"} {
		t.Run(kind, func(t *testing.T) {
			root := mustWritePack(t, mustPack(t, sampleInput("envelope")))
			data, _ := os.ReadFile(filepath.Join(root, ManifestName))
			var manifest Manifest
			_ = json.Unmarshal(data, &manifest)
			changeManifest := true
			switch kind {
			case "version":
				manifest.SchemaVersion = "future"
			case "identity":
				manifest.PackID = "sha256:" + strings.Repeat("0", 64)
			case "counts":
				manifest.SourcesCount = 0
			case "missing-option":
				manifest.Options.MaxUnits = 0
			case "invalid-option":
				manifest.Options.MaxUnits = -1
			case "receipts":
				manifest.Files = manifest.Files[:4]
				manifest.PackID = PackID(manifest.Files)
			case "filename":
				manifest.Files[0].Path = "../README.md"
				manifest.PackID = PackID(manifest.Files)
			case "digest":
				manifest.Files[0].SHA256 = "invalid"
				manifest.PackID = PackID(manifest.Files)
			case "negative-size":
				manifest.Files[0].SizeBytes = -1
				manifest.PackID = PackID(manifest.Files)
			default:
				changeManifest = false
			}
			if changeManifest {
				rewriteJSON(t, root, ManifestName, manifest)
			} else {
				switch kind {
				case "missing-payload":
					_ = os.Remove(filepath.Join(root, "units.jsonl"))
				case "missing-manifest":
					_ = os.Remove(filepath.Join(root, ManifestName))
				case "invalid-manifest-json":
					_ = os.WriteFile(filepath.Join(root, ManifestName), []byte("{broken"), 0o600)
				case "invalid-options":
					rebindPayload(t, root, "options.json", []byte("{}"))
				case "unknown-options":
					rebindPayload(t, root, "options.json", []byte(`{"unknown":1}`))
				case "oversized-readme":
					rebindPayload(t, root, "README.md", bytes.Repeat([]byte("x"), 1024*1024+1))
				case "invalid-outer-json":
					_ = os.WriteFile(filepath.Join(root, "rkc-export-manifest.json"), []byte("null trailing"), 0o600)
				case "wrong-outer":
					rewriteJSON(t, root, "rkc-export-manifest.json", ownershipManifest{SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: "wrong"})
				case "invalid-marker-json":
					_ = os.WriteFile(filepath.Join(root, safeoutput.MarkerName), []byte("{"), 0o600)
				case "wrong-marker":
					rewriteJSON(t, root, safeoutput.MarkerName, safeoutput.Marker{SchemaVersion: "1.0", Producer: "rkc", Kind: "atlas", SnapshotID: manifest.PackID})
				case "wrong-snapshot":
					rewriteJSON(t, root, safeoutput.MarkerName, safeoutput.Marker{SchemaVersion: "1.0", Producer: "rkc", Kind: "knowledge", SnapshotID: "wrong"})
				case "directory-marker":
					_ = os.Mkdir(filepath.Join(root, safeoutput.MarkerName), 0o700)
				}
			}
			if _, err := Verify(context.Background(), root); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
	root := mustWritePack(t, mustPack(t, sampleInput("owned")))
	data, _ := os.ReadFile(filepath.Join(root, ManifestName))
	var manifest Manifest
	_ = json.Unmarshal(data, &manifest)
	for _, marker := range []safeoutput.Marker{{SchemaVersion: "1.0", Producer: "rkc", Kind: "knowledge", SnapshotID: manifest.PackID}, {SchemaVersion: "1.0", Producer: "rkc", Kind: "staging"}} {
		rewriteJSON(t, root, safeoutput.MarkerName, marker)
		if _, err := Verify(context.Background(), root); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRehashedSourceAndUnitRecordsCannotBypassSemanticChecks(t *testing.T) {
	for _, kind := range []string{"invalid-source", "source-group", "duplicate-source", "too-many-sources", "too-many-units", "unknown-kind", "section-shape", "bad-citation", "bad-range", "bad-relation", "bad-truncation", "null-citations", "text-total", "empty-line", "line-too-large", "readme"} {
		t.Run(kind, func(t *testing.T) {
			pack := mustPack(t, sampleInput("semantic"))
			root := mustWritePack(t, pack)
			if strings.Contains(kind, "source") {
				data, _ := os.ReadFile(filepath.Join(root, "sources.jsonl"))
				var source Source
				_ = json.Unmarshal(bytes.TrimSpace(data), &source)
				switch kind {
				case "invalid-source":
					source.Integrity = "unverified"
				case "source-group":
					source.GroupID = "other-group"
				case "duplicate-source":
					rebindPayload(t, root, "sources.jsonl", append(data, data...))
				case "too-many-sources":
					var records []byte
					var candidates []Source
					for index := 0; index < MaximumSources+1; index++ {
						candidate := source
						candidate.SnapshotID = fmt.Sprintf("s%d", index)
						candidate.Coverage.SnapshotID = candidate.SnapshotID
						candidate.SourceID = rkcmodel.StableID("knowledge_source", candidate.SnapshotID, candidate.BundleSHA256)
						candidates = append(candidates, candidate)
					}
					sort.Slice(candidates, func(i, j int) bool { return candidates[i].SourceID < candidates[j].SourceID })
					for _, candidate := range candidates {
						encoded, _ := json.Marshal(candidate)
						records = append(records, append(encoded, '\n')...)
					}
					rebindPayload(t, root, "sources.jsonl", records)
				}
				if kind == "invalid-source" || kind == "source-group" {
					encoded, _ := json.Marshal(source)
					rebindPayload(t, root, "sources.jsonl", append(encoded, '\n'))
				}
			} else if kind == "readme" {
				rebindPayload(t, root, "README.md", []byte("Replacing license and trust guidance."))
			} else if kind == "empty-line" {
				rebindPayload(t, root, "units.jsonl", []byte("\n"))
			} else if kind == "line-too-large" {
				rebindPayload(t, root, "units.jsonl", bytes.Repeat([]byte("x"), maximumJSONLineBytes+1))
			} else {
				unit := pack.Units[0]
				switch kind {
				case "unknown-kind":
					unit.Kind = "system-authority"
					unit.ID = unitID(unit)
				case "section-shape":
					unit.Kind = "document_section"
					unit.SectionID = ""
					unit.ID = unitID(unit)
				case "bad-citation":
					unit.Citations = []Citation{{Kind: "", Confidence: 2}}
				case "bad-range":
					unit.Citations = []Citation{{Kind: "artifact", Confidence: 1, Source: &rkcmodel.SourceRange{Path: "file", StartByte: 12, EndByte: 3}}}
				case "bad-relation":
					unit.Relations = []Relation{{Kind: "calls"}}
				case "bad-truncation":
					unit.OriginalTextBytes++
				case "null-citations":
					unit.Citations = nil
				case "text-total", "too-many-units":
					data, _ := os.ReadFile(filepath.Join(root, ManifestName))
					var manifest Manifest
					_ = json.Unmarshal(data, &manifest)
					if kind == "text-total" {
						manifest.Options.MaxTotalTextBytes = 1
					} else {
						manifest.Options.MaxUnits = 1
						manifest.UnitsCount = 1
					}
					rewriteJSON(t, root, ManifestName, manifest)
					encoded, _ := json.Marshal(manifest.Options)
					rebindPayload(t, root, "options.json", encoded)
				}
				if kind != "text-total" && kind != "too-many-units" {
					encoded, _ := json.Marshal(unit)
					rebindPayload(t, root, "units.jsonl", append(encoded, '\n'))
				}
			}
			if _, err := Verify(context.Background(), root); err == nil {
				t.Fatal("malformed self-consistent records accepted")
			}
		})
	}
}

func TestPublicLibraryStateAndSerializationBoundaries(t *testing.T) {
	var absent *Builder
	if err := absent.Add(context.Background(), Input{}); err == nil {
		t.Fatal("nil builder accepted")
	}
	if _, err := absent.Finish(); err == nil {
		t.Fatal("nil builder finished")
	}
	empty, _ := New(Options{})
	if _, err := empty.Finish(); err == nil {
		t.Fatal("empty builder finished")
	}
	if err := empty.Add(nil, sampleInput("nil")); err == nil {
		t.Fatal("nil context admitted")
	}
	if err := empty.Add(context.Background(), sampleInput("poisoned")); err == nil {
		t.Fatal("failed builder reused")
	}
	full, _ := New(Options{})
	full.pack.Sources = make([]Source, MaximumSources)
	if err := full.Add(context.Background(), sampleInput("excess")); err == nil {
		t.Fatal("source limit bypassed")
	}
	input := sampleInput("unencodable")
	input.Bundle.Nodes[0].Attributes["invalid"] = make(chan int)
	if _, err := Build(context.Background(), []Input{input}, Options{}); err == nil {
		t.Fatal("unencodable attributes accepted")
	}
	input = sampleInput("originless")
	input.Bundle.Snapshot.RepositoryID = ""
	input.ArtifactBodies = nil
	input.Bundle.Nodes[0].Attributes = nil
	input.Bundle.Nodes[0].Signature = "func Sample()"
	input.Bundle.Documents[0].Sections[0].PlainText = "Plain source section."
	pack := mustPack(t, input)
	if _, err := Verify(context.Background(), mustWritePack(t, pack)); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []Unit{{Text: strings.Repeat("x", 8*1024*1024+1)}, {Citations: make([]Citation, maximumReferences+1)}, {Relations: make([]Relation, maximumReferences+1)}, {Text: "valid", Language: strings.Repeat("x", maximumJSONLineBytes)}} {
		builder, _ := New(Options{})
		if err := builder.addUnit(unit); err == nil {
			t.Fatal("oversized unit admitted")
		}
	}
	builder, _ := New(Options{})
	builder.encodedBytes = maximumPayloadBytes
	if err := builder.addUnit(Unit{Text: "excess"}); err == nil {
		t.Fatal("serialized budget bypassed")
	}
	if _, err := Write(nil, t.TempDir(), pack); err == nil {
		t.Fatal("nil context write accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Write(ctx, t.TempDir(), pack); !errors.Is(err, context.Canceled) {
		t.Fatalf("write ignored cancellation: %v", err)
	}
	if _, err := Verify(nil, t.TempDir()); err == nil {
		t.Fatal("nil context verify accepted")
	}
	if _, err := Verify(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify ignored cancellation: %v", err)
	}
	if _, err := Verify(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing pack accepted")
	}
	if _, err := encodeJSONFile(t.TempDir(), "bad.json", make(chan int)); err == nil {
		t.Fatal("invalid JSON encoded")
	}
	writer := &boundedWriter{writer: &bytes.Buffer{}, remaining: 1}
	if _, err := writer.Write([]byte("ab")); err == nil {
		t.Fatal("payload budget bypassed")
	}
	for _, data := range [][]byte{[]byte{255}, []byte(`{} {}`), []byte(strings.Repeat("[", 66) + strings.Repeat("]", 66)), []byte(`{"a":`), []byte(`]`)} {
		var target any
		if err := decodeStrict(data, &target); err == nil {
			t.Fatal("ambiguous or malformed JSON accepted")
		}
	}
}

func TestWriterRefusesInconsistentCallerBuiltPack(t *testing.T) {
	for _, kind := range []string{"options", "sources", "source-provenance", "source-order", "unit-order", "unit-reference", "quality", "total-text"} {
		t.Run(kind, func(t *testing.T) {
			pack := mustPack(t, sampleInput("manual"))
			switch kind {
			case "options":
				pack.Options.MaxUnits = 0
			case "sources":
				pack.Sources = nil
			case "source-provenance":
				pack.Sources[0].Integrity = "legacy_unverified"
			case "source-order":
				pack.Sources = append(pack.Sources, pack.Sources[0])
			case "unit-order":
				pack.Units = append(pack.Units, pack.Units[0])
			case "unit-reference":
				pack.Units[0].SourceID = "missing"
			case "quality":
				pack.Quality.TextBytes++
			case "total-text":
				pack.Options.MaxTotalTextBytes = 1
			}
			if _, err := Write(context.Background(), t.TempDir(), pack); err == nil {
				t.Fatal("inconsistent pack was written")
			}
		})
	}
}
