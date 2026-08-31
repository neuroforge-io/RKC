package scipindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestExtractCompilerSymbolsReferencesRelationshipsAndDiagnostics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := []byte("class Child:\n    def run(self):\n        return helper()\ndef helper():\n    return 1\n")
	sourcePath := filepath.Join(root, "main.py")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	child := "python project app 1 Child#"
	run := "python project app 1 Child#run()."
	helper := "python project app 1 helper()."
	base := "python project base 1 Base#"

	childDefinition := occurrenceMessage(typedRange(8, 0, 6, 11), child, roleDefinition, 19, nil, nil)
	runDefinition := occurrenceMessage(
		typedRange(8, 1, 8, 11),
		run, roleDefinition, 16, nil,
		typedEnclosingRange(11, 1, 4, 3, 0),
	)
	helperReference := occurrenceMessage(
		typedRange(8, 2, 15, 21),
		helper, roleRead, 15,
		[]byte(fieldMessage(6, diagnosticMessage(2, "PY-WARN", "compiler warning", "scip-python"))),
		nil,
	)
	helperDefinition := occurrenceMessage(typedRange(8, 3, 4, 10), helper, roleDefinition, 16, nil, nil)
	document := message(
		fieldString(1, "main.py"),
		fieldMessage(2, childDefinition),
		fieldMessage(2, runDefinition),
		fieldMessage(2, helperReference),
		fieldMessage(2, helperDefinition),
		fieldMessage(3, symbolMessage(child, "Child", 7, "class Child", "", relationshipMessage(base, false, true, false, false))),
		fieldMessage(3, symbolMessage(run, "run", 26, "def run(self)", child, nil)),
		fieldMessage(3, symbolMessage(helper, "helper", 17, "def helper()", "", nil)),
		fieldString(4, "Python"),
		fieldString(5, string(source)),
		fieldVarint(6, 3),
	)
	index := indexMessage("scip-python", "0.6.0", document, symbolMessage(base, "Base", 7, "", "", nil))
	indexPath := writeIndex(t, root, index)
	inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkGeneratedByCurrentProcess(inputs[0]); err != nil {
		t.Fatal(err)
	}
	inputs, _, err = PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	artifactID := rkcmodel.StableID("artifact", "main.py")
	fragment, err := Extract(context.Background(), Options{
		Root: root, Inputs: inputs,
		Files: []pluginapi.FileRef{{
			ArtifactID: artifactID, Path: "main.py", Language: "python",
			SHA256: digest(source), SizeBytes: int64(len(source)), Materialized: sourcePath,
		}},
		Artifacts: []rkcmodel.Artifact{{
			ID: artifactID, Path: "main.py", Kind: "source", Language: "python",
			SHA256: digest(source), SizeBytes: int64(len(source)), Text: true, Status: "text",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Artifacts) != 1 || fragment.Artifacts[0].Status != "semantic_parsed" {
		t.Fatalf("semantic artifact = %+v", fragment.Artifacts)
	}
	childID := rkcmodel.StableID("node", "scip", child)
	runID := rkcmodel.StableID("node", "scip", run)
	helperID := rkcmodel.StableID("node", "scip", helper)
	baseID := rkcmodel.StableID("node", "scip", base)
	assertNodeKind(t, fragment.Nodes, childID, "class")
	assertNodeKind(t, fragment.Nodes, runID, "method")
	assertNodeKind(t, fragment.Nodes, helperID, "function")
	assertNodeKind(t, fragment.Nodes, baseID, "class")
	assertEdge(t, fragment.Edges, "implements", childID, baseID)
	assertEdge(t, fragment.Edges, "reads", runID, helperID)
	if len(fragment.Diagnostics) != 1 ||
		fragment.Diagnostics[0].Code != "PY-WARN" ||
		fragment.Diagnostics[0].Severity != "warning" {
		t.Fatalf("compiler diagnostics = %+v", fragment.Diagnostics)
	}
	if len(fragment.Evidence) == 0 {
		t.Fatal("compiler evidence is empty")
	}
	for _, evidence := range fragment.Evidence {
		if evidence.Kind != "compiler_resolved" ||
			evidence.InputDigest != inputs[0].SHA256 {
			t.Fatalf("evidence is not index-bound: %+v", evidence)
		}
	}
}

func TestExtractRejectsUnsafeAmbiguousAndChangedInputs(t *testing.T) {
	t.Parallel()
	t.Run("metadata must be first", func(t *testing.T) {
		root := t.TempDir()
		indexPath := writeIndex(t, root, message(fieldMessage(2, message(fieldString(1, "main.go")))))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(context.Background(), Options{Root: root, Inputs: inputs}); err == nil ||
			!strings.Contains(err.Error(), "metadata must be the first field") {
			t.Fatalf("Extract(metadata order) = %v", err)
		}
	})
	t.Run("external documents fail closed", func(t *testing.T) {
		root := t.TempDir()
		indexPath := writeIndex(t, root, indexMessage(
			"scip-go", "1", message(fieldString(1, "../outside.go"), fieldString(4, "Go")), nil,
		))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(context.Background(), Options{Root: root, Inputs: inputs}); err == nil ||
			!strings.Contains(err.Error(), "not canonical") {
			t.Fatalf("Extract(external document) = %v", err)
		}
	})
	t.Run("non-canonical paths fail closed", func(t *testing.T) {
		root := t.TempDir()
		indexPath := writeIndex(t, root, indexMessage(
			"scip-go", "1", message(fieldString(1, "a//b.go"), fieldString(4, "Go")), nil,
		))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(context.Background(), Options{Root: root, Inputs: inputs}); err == nil ||
			!strings.Contains(err.Error(), "not canonical") {
			t.Fatalf("Extract(non-canonical path) = %v", err)
		}
	})
	t.Run("unsupported positions", func(t *testing.T) {
		root := t.TempDir()
		source := []byte("package main\n")
		if err := os.WriteFile(filepath.Join(root, "main.go"), source, 0o600); err != nil {
			t.Fatal(err)
		}
		document := message(
			fieldString(1, "main.go"),
			fieldMessage(2, occurrenceMessage(encodedLegacyRange(0, 0, 7), "scip . . . main/", roleDefinition, 14, nil, nil)),
			fieldString(4, "Go"),
			fieldString(5, string(source)),
			// 4 is outside the SCIP PositionEncoding enum (0..3).
			fieldVarint(6, 4),
		)
		indexPath := writeIndex(t, root, indexMessage("scip-go", "1", document, nil))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		artifactID := rkcmodel.StableID("artifact", "main.go")
		_, err = Extract(context.Background(), Options{
			Root: root, Inputs: inputs,
			Files: []pluginapi.FileRef{{
				ArtifactID: artifactID, Path: "main.go", SHA256: digest(source), SizeBytes: int64(len(source)),
			}},
			Artifacts: []rkcmodel.Artifact{{
				ID: artifactID, Path: "main.go", SHA256: digest(source), SizeBytes: int64(len(source)), Text: true, Status: "text",
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "position_encoding") {
			t.Fatalf("Extract(unsupported encoding) = %v", err)
		}
	})
	t.Run("unspecified positions fail and explicit positions work", func(t *testing.T) {
		root := t.TempDir()
		source := []byte("package main\n")
		if err := os.WriteFile(filepath.Join(root, "main.go"), source, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, encoding := range []uint64{0, 3} {
			document := message(
				fieldString(1, "main.go"),
				fieldMessage(2, occurrenceMessage(encodedLegacyRange(0, 0, 7), "scip . . . main/", roleDefinition, 14, nil, nil)),
				fieldString(4, "Go"),
				fieldString(5, string(source)),
				fieldVarint(6, encoding),
			)
			indexPath := writeIndex(t, root, indexMessage("scip-go", "1", document, nil))
			inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
			if err != nil {
				t.Fatal(err)
			}
			if err := MarkGeneratedByCurrentProcess(inputs[0]); err != nil {
				t.Fatal(err)
			}
			inputs, _, err = PrepareInputs(context.Background(), []string{indexPath})
			if err != nil {
				t.Fatal(err)
			}
			artifactID := rkcmodel.StableID("artifact", "main.go")
			result, err := Extract(context.Background(), Options{
				Root: root, Inputs: inputs,
				Files: []pluginapi.FileRef{{
					ArtifactID: artifactID, Path: "main.go", SHA256: digest(source), SizeBytes: int64(len(source)),
				}},
				Artifacts: []rkcmodel.Artifact{{
					ID: artifactID, Path: "main.go", SHA256: digest(source), SizeBytes: int64(len(source)), Text: true, Status: "text",
				}},
			})
			if encoding == 0 {
				if err == nil || !strings.Contains(err.Error(), "ambiguous or unsupported") {
					t.Fatalf("Extract(encoding 0) = %v", err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("Extract(encoding %d) = %v", encoding, err)
			}
			if len(result.Artifacts) != 1 || result.Artifacts[0].Status != "semantic_parsed" {
				t.Fatalf("Extract(encoding %d) semantic artifact = %+v", encoding, result.Artifacts)
			}
			nodeID := rkcmodel.StableID("node", "scip", "scip . . . main/")
			found := false
			for _, node := range result.Nodes {
				if node.ID == nodeID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Extract(encoding %d) produced no definition node", encoding)
			}
		}
	})
	t.Run("prepared digest mismatch", func(t *testing.T) {
		root := t.TempDir()
		indexPath := writeIndex(t, root, indexMessage("scip-go", "1", nil, nil))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		inputs[0].SHA256 = strings.Repeat("0", 64)
		if _, err := Extract(context.Background(), Options{Root: root, Inputs: inputs}); err == nil ||
			!strings.Contains(err.Error(), "digest changed") {
			t.Fatalf("Extract(digest mismatch) = %v", err)
		}
	})
}

func TestPrepareInputsCanonicalizesSortsAndRejectsSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := writeNamedIndex(t, root, "b.scip", indexMessage("b", "1", nil, nil))
	second := writeNamedIndex(t, root, "a.scip", indexMessage("a", "1", nil, nil))
	inputs, digest, err := PrepareInputs(context.Background(), []string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Path != second || len(digest) != 64 {
		t.Fatalf("prepared inputs = %+v, digest %q", inputs, digest)
	}
	if _, _, err := PrepareInputs(context.Background(), []string{first, first}); err == nil {
		t.Fatal("PrepareInputs accepted a duplicate")
	}
	link := filepath.Join(root, "link.scip")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareInputs(context.Background(), []string{link}); err == nil ||
		!strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("PrepareInputs(symlink) = %v", err)
	}
	if err := VerifyInputs(context.Background(), inputs); err != nil {
		t.Fatalf("VerifyInputs(unchanged) = %v", err)
	}
	if err := os.WriteFile(second, indexMessage("changed", "2", nil, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyInputs(context.Background(), inputs); err == nil ||
		!strings.Contains(err.Error(), "changed during the scan") {
		t.Fatalf("VerifyInputs(changed) = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := PrepareInputs(cancelled, []string{first}); err == nil {
		t.Fatal("PrepareInputs accepted a cancelled context")
	}
}

func TestPrepareInputsCoalescesIdenticalBytesWithStrongestAuthority(t *testing.T) {
	root := t.TempDir()
	original := writeNamedIndex(t, root, "original.scip", indexMessage("same", "1", nil, nil))
	duplicate := filepath.Join(root, "duplicate.scip")
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duplicate, data, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInput, _, err := PrepareInputs(context.Background(), []string{original})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkGeneratedByCurrentProcess(originalInput[0]); err != nil {
		t.Fatal(err)
	}
	inputs, _, err := PrepareInputs(context.Background(), []string{duplicate, original})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].Path != original || !inputs[0].CompilerAuthenticated() {
		t.Fatalf("coalesced input = %+v", inputs)
	}
}

func TestWireReaderRejectsOverflowAndGroups(t *testing.T) {
	t.Parallel()
	overflow := bytes.Repeat([]byte{0xff}, 10)
	reader := newMessageReader(overflow)
	if _, err := reader.varint(); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("varint overflow = %v", err)
	}
	reader = newMessageReader(nil)
	if err := reader.skip(3); err == nil || !strings.Contains(err.Error(), "groups") {
		t.Fatalf("skip group = %v", err)
	}
}

func assertNodeKind(t *testing.T, nodes []rkcmodel.Node, id, kind string) {
	t.Helper()
	for _, node := range nodes {
		if node.ID == id {
			if node.Kind != kind {
				t.Fatalf("node %s kind = %s, want %s", id, node.Kind, kind)
			}
			return
		}
	}
	t.Fatalf("node %s is missing", id)
}

func assertEdge(t *testing.T, edges []rkcmodel.Edge, kind, from, to string) {
	t.Helper()
	for _, edge := range edges {
		if edge.Kind == kind && edge.From == from && edge.To == to {
			if edge.Resolution != rkcmodel.ResolutionCompilerResolved {
				t.Fatalf("edge resolution = %s", edge.Resolution)
			}
			return
		}
	}
	t.Fatalf("%s edge %s -> %s is missing: %+v", kind, from, to, edges)
}

func writeIndex(t *testing.T, root string, data []byte) string {
	t.Helper()
	return writeNamedIndex(t, root, "index.scip", data)
}

func writeNamedIndex(t *testing.T, root, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexMessage(tool, version string, document, external []byte) []byte {
	toolInfo := message(fieldString(1, tool), fieldString(2, version))
	metadata := message(fieldMessage(2, toolInfo), fieldString(3, "file:///workspace"), fieldVarint(4, 1))
	fields := [][]byte{fieldMessage(1, metadata)}
	if document != nil {
		fields = append(fields, fieldMessage(2, document))
	}
	if external != nil {
		fields = append(fields, fieldMessage(3, external))
	}
	return message(fields...)
}

func symbolMessage(symbol, display string, kind int32, signature, enclosing string, relation []byte) []byte {
	fields := [][]byte{
		fieldString(1, symbol),
		fieldString(3, "Compiler documentation for "+display),
		fieldVarint(5, uint64(kind)),
		fieldString(6, display),
	}
	if relation != nil {
		fields = append(fields, fieldMessage(4, relation))
	}
	if signature != "" {
		fields = append(fields, fieldMessage(7, message(fieldString(4, "python"), fieldString(5, signature))))
	}
	if enclosing != "" {
		fields = append(fields, fieldString(8, enclosing))
	}
	return message(fields...)
}

func relationshipMessage(symbol string, reference, implementation, typeDefinition, definition bool) []byte {
	return message(
		fieldString(1, symbol),
		fieldVarint(2, boolVarint(reference)),
		fieldVarint(3, boolVarint(implementation)),
		fieldVarint(4, boolVarint(typeDefinition)),
		fieldVarint(5, boolVarint(definition)),
	)
}

func diagnosticMessage(severity int32, code, text, source string) []byte {
	return message(
		fieldVarint(1, uint64(severity)),
		fieldString(2, code),
		fieldString(3, text),
		fieldString(4, source),
	)
}

func occurrenceMessage(rangeField []byte, symbol string, roles, syntax int32, extra, enclosing []byte) []byte {
	return message(
		rangeField,
		fieldString(2, symbol),
		fieldVarint(3, uint64(roles)),
		fieldVarint(5, uint64(syntax)),
		extra,
		enclosing,
	)
}

func encodedLegacyRange(values ...int32) []byte {
	var packed []byte
	for _, value := range values {
		packed = append(packed, encodeVarint(uint64(value))...)
	}
	return fieldBytes(1, packed)
}

func typedRange(field int, values ...int32) []byte {
	var rangeMessage []byte
	for index, value := range values {
		rangeMessage = append(rangeMessage, fieldVarint(index+1, uint64(value))...)
	}
	return fieldMessage(field, rangeMessage)
}

func typedEnclosingRange(field int, values ...int32) []byte {
	return typedRange(field, values...)
}

func fieldString(field int, value string) []byte {
	return fieldBytes(field, []byte(value))
}

func fieldMessage(field int, value []byte) []byte {
	return fieldBytes(field, value)
}

func fieldBytes(field int, value []byte) []byte {
	if value == nil {
		return nil
	}
	output := encodeVarint(uint64(field<<3 | 2))
	output = append(output, encodeVarint(uint64(len(value)))...)
	return append(output, value...)
}

func fieldVarint(field int, value uint64) []byte {
	output := encodeVarint(uint64(field << 3))
	return append(output, encodeVarint(value)...)
}

func message(fields ...[]byte) []byte {
	return bytes.Join(fields, nil)
}

func encodeVarint(value uint64) []byte {
	var output []byte
	for value >= 0x80 {
		output = append(output, byte(value)|0x80)
		value >>= 7
	}
	return append(output, byte(value))
}

func boolVarint(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
