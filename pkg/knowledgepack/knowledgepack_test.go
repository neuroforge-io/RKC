package knowledgepack

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func sampleInput(name string) Input {
	body := "# " + name + "\n\nEvidence stays attached to the original source.\n"
	location := &rkcmodel.SourceRange{ArtifactID: "artifact", Path: "guide.md", StartLine: 1, EndLine: 3}
	return Input{Integrity: "verified", ArtifactBodies: map[string]string{"artifact": body}, Bundle: rkcmodel.Bundle{
		Snapshot:  rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "snapshot-" + name, RepositoryID: "sample-repository", RootName: name, ContentDigest: digest([]byte(name)), Status: "committed", Tool: rkcmodel.ToolInfo{Name: "rkc", Version: "test"}},
		Artifacts: []rkcmodel.Artifact{{ID: "artifact", Path: "guide.md", Kind: "document", Text: true, Status: "text", SHA256: digest([]byte(body)), LineCount: 3, LicenseExpression: "Apache-2.0"}},
		Nodes:     []rkcmodel.Node{{ID: "node", Name: "Guide", Kind: "module", ArtifactID: "artifact", Source: location, EvidenceIDs: []string{"evidence"}, Attributes: map[string]any{"description": "Source-cited documentation."}}},
		Evidence:  []rkcmodel.Evidence{{ID: "evidence", Kind: "documentation_asserted", Method: "markdown", Confidence: 0.8, Source: location}},
		Claims:    []rkcmodel.Claim{{ID: "claim", SubjectID: "node", Text: "The guide preserves citations.", Certainty: "inferred", Validation: "pending", Generator: "test-model", EvidenceIDs: []string{"evidence"}}},
		Documents: []rkcmodel.Document{{ID: "document", Kind: "guide", Title: "Generated guide", Generator: "test", Status: "draft", Sections: []rkcmodel.DocumentSection{{ID: "section", Ordinal: 0, Heading: "Scope", Markdown: "Keep the evidence.", EvidenceIDs: []string{"evidence"}}}}},
	}}
}

func mustPack(t *testing.T, inputs ...Input) Pack {
	t.Helper()
	pack, err := Build(context.Background(), inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func mustWritePack(t *testing.T, pack Pack) string {
	t.Helper()
	root := t.TempDir()
	if _, err := Write(context.Background(), root, pack); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPackDeterministicAcrossInputOrderingAndRoundTrip(t *testing.T) {
	left := mustPack(t, sampleInput("alpha"), sampleInput("beta"))
	right := mustPack(t, sampleInput("beta"), sampleInput("alpha"))
	if !reflect.DeepEqual(left, right) {
		t.Fatal("input ordering changed knowledge pack")
	}
	leftRoot, rightRoot := mustWritePack(t, left), mustWritePack(t, right)
	for _, name := range append(append([]string{}, payloadNames...), ManifestName, "rkc-export-manifest.json") {
		a, err := os.ReadFile(filepath.Join(leftRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(rightRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("nondeterministic %s", name)
		}
	}
	verified, err := Verify(context.Background(), leftRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.OK || verified.Manifest.SourcesCount != 2 || verified.Manifest.UnitsCount != 8 {
		t.Fatalf("unexpected verification: %+v", verified)
	}
	if left.Sources[0].GroupID != left.Sources[1].GroupID {
		t.Fatal("repository snapshots lost source grouping")
	}
	for _, unit := range left.Units {
		if unit.Kind == "claim" && (unit.Certainty != "inferred" || unit.Validation != "pending" || unit.Generator != "test-model") {
			t.Fatal("claim certainty was promoted")
		}
		if len(unit.Citations) == 0 {
			t.Fatal("sample evidence citation missing")
		}
		if unit.Kind == "artifact" && (unit.MetadataOnly || unit.LicenseExpression != "Apache-2.0") {
			t.Fatal("source body or license lost")
		}
	}
}

func TestTruncationMetadataAndRedactionRemainExplicit(t *testing.T) {
	input := sampleInput("bounded")
	input.ArtifactBodies["artifact"] = strings.Repeat("界", 300)
	input.Bundle.Nodes[0].Attributes["description"] = "api_key = ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	pack, err := Build(context.Background(), []Input{input}, Options{MaxUnitTextBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Quality.TruncatedUnits != 1 {
		t.Fatalf("truncation report: %+v", pack.Quality)
	}
	for _, unit := range pack.Units {
		if !utf8.ValidString(unit.Text) {
			t.Fatal("truncated in middle of UTF-8 rune")
		}
		if strings.Contains(unit.Text, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Fatal("secret retained")
		}
		if unit.Kind == "artifact" && (!unit.Truncated || unit.OriginalTextBytes != 900 || len(unit.Text) > 256) {
			t.Fatalf("wrong bounds: %+v", unit)
		}
	}
	if _, err := Verify(context.Background(), mustWritePack(t, pack)); err != nil {
		t.Fatal(err)
	}
	input.ArtifactBodies = nil
	pack = mustPack(t, input)
	if pack.Quality.MetadataOnlyUnits != 1 {
		t.Fatalf("missing body must stay metadata-only: %+v", pack.Quality)
	}
}

func TestBuilderRejectsUnverifiedDuplicateInvalidAndResourceExcess(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"legacy":                    func(input *Input) { input.Integrity = "legacy_unverified" },
		"dangling-evidence":         func(input *Input) { input.Bundle.Nodes[0].EvidenceIDs = []string{"missing"} },
		"body-for-missing-artifact": func(input *Input) { input.ArtifactBodies["missing"] = "secret" },
		"excluded-body":             func(input *Input) { input.Bundle.Artifacts[0].Status = "excluded" },
		"utf8":                      func(input *Input) { input.ArtifactBodies["artifact"] = string([]byte{255}) },
	} {
		t.Run(name, func(t *testing.T) {
			input := sampleInput(name)
			mutate(&input)
			if _, err := Build(context.Background(), []Input{input}, Options{}); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	input := sampleInput("duplicate")
	if _, err := Build(context.Background(), []Input{input, input}, Options{}); err == nil {
		t.Fatal("duplicate source accepted")
	}
	for _, options := range []Options{{MaxUnits: 1}, {MaxTotalTextBytes: 1}, {MaxUnitTextBytes: 255}, {MaxUnits: MaximumUnits + 1}} {
		if _, err := Build(context.Background(), []Input{sampleInput("limit")}, options); err == nil {
			t.Fatalf("excess accepted: %+v", options)
		}
	}
	builder, _ := New(Options{MaxUnits: 1})
	if err := builder.Add(context.Background(), sampleInput("poison")); err == nil {
		t.Fatal("limit not enforced")
	}
	if _, err := builder.Finish(); err == nil {
		t.Fatal("partial source admitted after failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, []Input{sampleInput("cancel")}, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}

func rebindPayload(t *testing.T, root, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	for index, file := range manifest.Files {
		if file.Path == name {
			manifest.Files[index].SHA256 = digest(data)
			manifest.Files[index].SizeBytes = int64(len(data))
		}
	}
	manifest.PackID = PackID(manifest.Files)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "rkc-export-manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestVerifyRejectsHashAndSemanticTampering(t *testing.T) {
	for _, kind := range []string{"raw-tamper", "unit-text", "source-reference", "quality", "duplicate-key", "unknown-field", "duplicate-unit", "wrong-size", "extra-file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := mustWritePack(t, mustPack(t, sampleInput("tamper")))
			units, err := os.ReadFile(filepath.Join(root, "units.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "raw-tamper":
				if err := os.WriteFile(filepath.Join(root, "units.jsonl"), append(units, ' '), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unit-text", "source-reference", "unknown-field":
				lines := strings.Split(strings.TrimSuffix(string(units), "\n"), "\n")
				var unit map[string]any
				if err := json.Unmarshal([]byte(lines[0]), &unit); err != nil {
					t.Fatal(err)
				}
				if kind == "unit-text" {
					unit["text"] = "Replaced text"
				} else if kind == "source-reference" {
					unit["source_id"] = "missing"
				} else {
					unit["extra_authority"] = "system"
				}
				data, err := json.Marshal(unit)
				if err != nil {
					t.Fatal(err)
				}
				lines[0] = string(data)
				rebindPayload(t, root, "units.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
			case "quality":
				rebindPayload(t, root, "quality.json", []byte(`{"schema_version":"rkc-knowledge-pack/v1"}`))
			case "duplicate-key":
				rebindPayload(t, root, "units.jsonl", []byte(strings.Replace(string(units), `"kind":`, `"kind":"artifact","kind":`, 1)))
			case "duplicate-unit":
				rebindPayload(t, root, "units.jsonl", append(units, []byte(strings.Split(string(units), "\n")[0]+"\n")...))
			case "wrong-size":
				data, _ := os.ReadFile(filepath.Join(root, ManifestName))
				var manifest Manifest
				_ = json.Unmarshal(data, &manifest)
				manifest.Files[0].SizeBytes++
				manifest.PackID = PackID(manifest.Files)
				data, _ = json.Marshal(manifest)
				if err := os.WriteFile(filepath.Join(root, ManifestName), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				if err := os.WriteFile(filepath.Join(root, "surprise.txt"), []byte("unknown"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "units.jsonl")
				if err := os.WriteFile(target, units, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, "units.jsonl")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, "units.jsonl")); err != nil {
					t.Skip(err)
				}
			}
			if _, err := Verify(context.Background(), root); err == nil {
				t.Fatal("tampered pack accepted")
			}
		})
	}
}

func TestWriteDoesNotReplaceFilesAndPortablePackNeedsNoOwnershipMarker(t *testing.T) {
	pack := mustPack(t, sampleInput("portable"))
	root := mustWritePack(t, pack)
	if _, err := Write(context.Background(), root, pack); err == nil {
		t.Fatal("existing files replaced")
	}
	if err := os.Remove(filepath.Join(root, "rkc-export-manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}
