package knowledgepack

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestVerifyRejectsCaseAliasesInsteadOfHashingDifferentText(t *testing.T) {
	for _, order := range []string{"alias-first", "alias-last", "alias-only"} {
		t.Run(order, func(t *testing.T) {
			root := mustWritePack(t, mustPack(t, sampleInput("case-alias")))
			data, err := os.ReadFile(filepath.Join(root, "units.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
			var record map[string]json.RawMessage
			if err := json.Unmarshal(lines[0], &record); err != nil {
				t.Fatal(err)
			}
			originalText := record["text"]
			if order == "alias-only" {
				lines[0] = bytes.Replace(lines[0], []byte(`"text":`), []byte(`"Text":`), 1)
			} else {
				// Python/JavaScript read the changed lowercase text. A normal Go
				// decoder accepts the alias and, when it is last, hashes the old text.
				changed := bytes.Replace(lines[0], append([]byte(`"text":`), originalText...), []byte(`"text":"Unverified machine-consumer text."`), 1)
				alias := append([]byte(`"Text":`), originalText...)
				if order == "alias-first" {
					lines[0] = append(append(append([]byte{'{'}, alias...), ','), changed[1:]...)
				} else {
					lines[0] = append(append(append(changed[:len(changed)-1], ','), alias...), '}')
				}
			}
			rebindPayload(t, root, "units.jsonl", append(bytes.Join(lines, []byte("\n")), '\n'))
			if _, err := Verify(context.Background(), root); err == nil || !strings.Contains(err.Error(), `unknown JSON field "Text"`) {
				t.Fatalf("case alias must fail before text interpretation, got %v", err)
			}
		})
	}
}

func TestVerifyRejectsAliasesAcrossPackMetadataAndNestedRecords(t *testing.T) {
	for _, test := range []struct{ name, payload, field, alias string }{
		{"unit-citation", "units.jsonl", "evidence_id", "Evidence_ID"},
		{"citation-source-pointer", "units.jsonl", "start_line", "Start_Line"},
		{"source-coverage", "sources.jsonl", "nodes_total", "Nodes_Total"},
		{"source-record", "sources.jsonl", "source_id", "Source_ID"},
		{"quality-record", "quality.json", "units_count", "Units_Count"},
		{"options-record", "options.json", "max_units", "Max_Units"},
		{"manifest-record", ManifestName, "pack_id", "Pack_ID"},
		{"manifest-file-list", ManifestName, "size_bytes", "Size_Bytes"},
		{"manifest-options", ManifestName, "max_units", "Max_Units"},
		{"ownership-record", "rkc-export-manifest.json", "snapshot_id", "Snapshot_ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := mustWritePack(t, mustPack(t, sampleInput(test.name)))
			data, err := os.ReadFile(filepath.Join(root, test.payload))
			if err != nil {
				t.Fatal(err)
			}
			needle := []byte(`"` + test.field + `":`)
			if !bytes.Contains(data, needle) {
				t.Fatalf("fixture lacks %s", test.field)
			}
			// The coverage case targets a property found only in the nested
			// canonical Coverage struct, not its enclosing Source record.
			changed := bytes.ReplaceAll(data, needle, []byte(`"`+test.alias+`":`))
			if test.payload == ManifestName || test.payload == "rkc-export-manifest.json" {
				if err := os.WriteFile(filepath.Join(root, test.payload), changed, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				rebindPayload(t, root, test.payload, changed)
			}
			if _, err := Verify(context.Background(), root); err == nil || !strings.Contains(err.Error(), "case-sensitive") {
				t.Fatalf("aliased wire field accepted: %v", err)
			}
		})
	}
	root := mustWritePack(t, mustPack(t, sampleInput("marker-alias")))
	if err := os.WriteFile(filepath.Join(root, safeoutput.MarkerName), []byte(`{"schema_version":"1.0","Producer":"rkc","kind":"staging"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), root); err == nil || !strings.Contains(err.Error(), `unknown JSON field "Producer"`) {
		t.Fatalf("marker alias accepted: %v", err)
	}
}

func TestStrictDecoderPreservesMapsOptionalFieldsAndTypedValidation(t *testing.T) {
	type child struct {
		Text string `json:"text,omitempty"`
	}
	type envelope struct {
		Children map[string][]*child `json:"children"`
		Labels   map[string]any      `json:"labels"`
		Array    [1]child            `json:"array"`
		Optional string              `json:"optional,omitempty"`
	}
	var value envelope
	valid := []byte(`{"children":{"Text":[{"text":"one"},{}],"text":[null]},"labels":{"Text":1,"text":2,"nested":{"Start_Line":3,"start_line":4}},"array":[{}]}`)
	if err := decodeStrict(valid, &value); err != nil {
		t.Fatal(err)
	}
	if value.Children["Text"][0].Text != "one" || value.Children["Text"][1].Text != "" || value.Children["text"][0] != nil || len(value.Labels) != 3 || value.Optional != "" {
		t.Fatalf("valid dictionary/optional values changed: %+v", value)
	}
	for _, data := range []string{
		`{"children":{"Text":[{"Text":"alias"}]}}`,
		`{"array":[{"Text":"alias"}]}`,
		`{"optional":"a","optional":"b"}`,
		`{"labels":{"Text":1,"Text":2}}`,
		`{"optional":123}`,
		`{"unknown":1}`,
		`{"children":[]}`,
	} {
		if err := decodeStrict([]byte(data), &value); err == nil {
			t.Fatalf("invalid JSON accepted: %s", data)
		}
	}
	var unit Unit
	for _, data := range []string{`{"Text":"alias"}`, `{"teXt":"alias"}`, `{"\u0054ext":"alias"}`, `{"ſource_id":"unicode-case-alias"}`} {
		if err := decodeStrict([]byte(data), &unit); err == nil || !strings.Contains(err.Error(), "case-sensitive") {
			t.Fatalf("wire alias accepted: %s (%v)", data, err)
		}
	}
	// Structs in actual wire arrays and optional pointers keep their JSON tags,
	// including omitted zero coordinates. The decoder never requires omitempty.
	for _, original := range mustPack(t, sampleInput("round-trip")).Units {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Unit
		if err := decodeStrict(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(original, decoded) {
			t.Fatal("ordinary unit no longer round trips")
		}
	}
	var source Source
	if err := decodeStrict([]byte(`{"coverage":{"snapshot_id":"s","node_kinds":{"HTTP":1,"http":2}}}`), &source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.Coverage.NodeKinds, map[string]int{"HTTP": 1, "http": 2}) {
		t.Fatal("coverage map keys were folded")
	}
	var location *rkcmodel.SourceRange
	if err := decodeStrict([]byte(`{"path":"guide.md"}`), &location); err != nil || location.Path != "guide.md" {
		t.Fatalf("optional pointer decoding changed: %+v %v", location, err)
	}
}
