package scipindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inspectFixture(t *testing.T, index []byte) (string, Input) {
	t.Helper()
	root := t.TempDir()
	path := writeIndex(t, root, index)
	sum := sha256.Sum256(index)
	return path, Input{Path: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(index))}
}

func TestInspectValidIndex(t *testing.T) {
	t.Parallel()
	document := message(
		fieldString(1, "main.py"),
		fieldMessage(2, occurrenceMessage(encodedLegacyRange(0, 0, 1, 1), "scip . . . main/", roleDefinition, 16, nil, nil)),
		fieldString(4, "Python"),
		fieldVarint(6, 3),
	)
	index := indexMessage("scip-python", "0.6.0", document, nil)
	_, input := inspectFixture(t, index)
	inspection, err := Inspect(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Tool != "scip-python" || inspection.ToolVersion != "0.6.0" {
		t.Fatalf("tool identity = %+v", inspection)
	}
	if inspection.Documents != 1 || inspection.Symbols != 0 || inspection.Occurrences != 1 ||
		inspection.ExternalDocuments != 0 || inspection.InvalidDocuments != 0 {
		t.Fatalf("counts = %+v", inspection)
	}
	if inspection.SHA256 != input.SHA256 || inspection.SizeBytes != input.SizeBytes {
		t.Fatalf("digest-bound identity = %+v", inspection)
	}
}

func TestInspectRejectsEscapingDocuments(t *testing.T) {
	t.Parallel()
	index := indexMessage("scip-go", "1", nil, nil)
	index = append(index, fieldMessage(2, message(fieldString(1, "../../stdlib/print.go"), fieldString(4, "Go"), fieldVarint(6, 1)))...)
	_, input := inspectFixture(t, index)
	if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("escaping document = %v", err)
	}
}

func TestInspectRejectsMalformedIndexes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	t.Run("metadata must be first", func(t *testing.T) {
		index := message(fieldMessage(2, message(fieldString(1, "main.go"), fieldString(4, "Go"))))
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "metadata must be the first field") {
			t.Fatalf("metadata order = %v", err)
		}
	})
	t.Run("metadata is missing", func(t *testing.T) {
		_, input := inspectFixture(t, message())
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "metadata is missing") {
			t.Fatalf("missing metadata = %v", err)
		}
	})
	t.Run("non-canonical path fails", func(t *testing.T) {
		index := indexMessage("scip-go", "1", message(fieldString(1, "a//b.go"), fieldString(4, "Go")), nil)
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "non-canonical") {
			t.Fatalf("non-canonical path = %v", err)
		}
	})
	t.Run("unsupported encoding fails", func(t *testing.T) {
		index := indexMessage("scip-go", "1", message(
			fieldString(1, "b.go"), fieldString(4, "Go"), fieldVarint(6, 9),
		), nil)
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "position_encoding") {
			t.Fatalf("unsupported encoding = %v", err)
		}
	})
	t.Run("invalid occurrence range fails", func(t *testing.T) {
		occurrence := occurrenceMessage(encodedLegacyRange(0, 9, 0, 1), "scip . . . x/", roleDefinition, 0, nil, nil)
		index := indexMessage("scip-go", "1", message(
			fieldString(1, "b.go"),
			fieldMessage(2, occurrence),
			fieldString(4, "Go"),
			fieldVarint(6, 1),
		), nil)
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil {
			t.Fatal("reversed range inspected successfully")
		}
	})
	t.Run("digest mismatch fails", func(t *testing.T) {
		index := indexMessage("scip-go", "1", nil, nil)
		path := writeIndex(t, root, index)
		input := Input{Path: path, SHA256: strings.Repeat("0", 64), SizeBytes: int64(len(index))}
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "digest changed") {
			t.Fatalf("digest mismatch = %v", err)
		}
	})
	t.Run("symlink input fails", func(t *testing.T) {
		index := indexMessage("scip-go", "1", nil, nil)
		path := writeIndex(t, root, index)
		link := filepath.Join(root, "linked.scip")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(index)
		input := Input{Path: link, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(index))}
		if _, err := Inspect(context.Background(), input); err == nil {
			t.Fatal("symlink input inspected successfully")
		}
	})
	t.Run("missing file fails", func(t *testing.T) {
		input := Input{Path: filepath.Join(root, "absent.scip"), SHA256: strings.Repeat("a", 64), SizeBytes: 1}
		if _, err := Inspect(context.Background(), input); err == nil {
			t.Fatal("missing file inspected successfully")
		}
	})
}

func TestInspectBounds(t *testing.T) {
	t.Parallel()
	t.Run("document limit", func(t *testing.T) {
		index := indexMessage("scip-go", "1", nil, nil)
		for count := 0; count <= maximumDocuments; count++ {
			index = append(index, fieldMessage(2, message(fieldString(1, "x.go"), fieldString(4, "Go"), fieldVarint(6, 1)))...)
		}
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "document limit") {
			t.Fatalf("document limit = %v", err)
		}
	})
	t.Run("occurrence limit", func(t *testing.T) {
		var occurrences []byte
		for count := 0; count <= maximumOccurrences; count++ {
			occurrences = append(occurrences, fieldMessage(2, occurrenceMessage(
				encodedLegacyRange(0, 0, 0, 1), "scip . . . x/", roleDefinition, 0, nil, nil,
			))...)
		}
		index := indexMessage("scip-go", "1", message(
			fieldString(1, "x.go"), fieldString(4, "Go"), fieldVarint(6, 1),
			occurrences,
		), nil)
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "occurrence limit") {
			t.Fatalf("occurrence limit = %v", err)
		}
	})
}

func TestInspectMalformedWireFailures(t *testing.T) {
	t.Parallel()
	validMetadata := message(fieldBytes(2, message(fieldString(1, "tool"), fieldString(2, "1"))))
	copyOf := func(index []byte, tail []byte) []byte {
		result := append([]byte(nil), index...)
		return append(result, tail...)
	}
	validIndex := indexMessage("tool", "1", nil, nil)
	cases := map[string]struct {
		index   []byte
		message string
	}{
		"nil context":        {copyOf(validIndex, nil), "context is required"},
		"invalid input":      {copyOf(validIndex, nil), "input is invalid"},
		"truncated varint":   {copyOf(validIndex, []byte{0x80}), "decode SCIP index"},
		"duplicate metadata": {copyOf(validIndex, fieldBytes(1, validMetadata)), "appears more than once"},
		"metadata wire":      {message(fieldVarint(1, 1)), "wire type"},
		"metadata overflow":  {message(append(encodeVarint(1<<3|2), encodeVarint(uint64(maximumMessageBytes)+1)...)), "decode SCIP metadata"},
		"document wire":      {copyOf(validIndex, fieldVarint(2, 1)), "wire type"},
		"symbol wire":        {copyOf(validIndex, fieldVarint(3, 1)), "wire type"},
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if name == "nil context" {
				ctx = nil
			}
			input := Input{Path: "", SHA256: "", SizeBytes: -1}
			if name != "invalid input" && name != "nil context" {
				path := filepath.Join(t.TempDir(), "x.scip")
				if err := os.WriteFile(path, fixture.index, 0o600); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(fixture.index)
				input = Input{Path: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(fixture.index))}
			}
			_, err := Inspect(ctx, input)
			if err == nil || !strings.Contains(err.Error(), fixture.message) {
				t.Fatalf("Inspect(%s) = %v; want %q", name, err, fixture.message)
			}
		})
	}
}

func TestInspectOpenAndParseFailures(t *testing.T) {
	t.Parallel()
	copyOf := func(index []byte, tail []byte) []byte {
		result := append([]byte(nil), index...)
		return append(result, tail...)
	}
	validIndex := indexMessage("tool", "1", nil, nil)
	t.Run("unreadable file fails to open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.scip")
		if err := os.WriteFile(path, validIndex, 0o000); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(validIndex)
		if _, err := Inspect(context.Background(), Input{Path: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(validIndex))}); err == nil ||
			!strings.Contains(err.Error(), "open SCIP index") {
			t.Fatalf("unreadable file = %v", err)
		}
	})
	t.Run("malformed metadata fails", func(t *testing.T) {
		index := message(fieldBytes(1, message(fieldString(2, "x"))))
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "decode SCIP metadata") {
			t.Fatalf("malformed metadata = %v", err)
		}
	})
	t.Run("oversized document field fails", func(t *testing.T) {
		index := copyOf(validIndex, append(encodeVarint(2<<3|2), encodeVarint(uint64(maximumDocumentBytes)+1)...))
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "decode SCIP document") {
			t.Fatalf("oversized document = %v", err)
		}
	})
	t.Run("malformed document fails", func(t *testing.T) {
		document := message(fieldString(1, "x.go"), fieldBytes(2, message(fieldVarint(2, 1))))
		index := indexMessage("tool", "1", document, nil)
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "decode SCIP document") {
			t.Fatalf("malformed document = %v", err)
		}
	})
	t.Run("malformed external symbol fails", func(t *testing.T) {
		index := copyOf(validIndex, fieldBytes(3, message(fieldVarint(1, 1))))
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "decode SCIP external symbol") {
			t.Fatalf("malformed external symbol = %v", err)
		}
	})
	t.Run("unsupported wire skip fails", func(t *testing.T) {
		index := copyOf(validIndex, encodeVarint(9<<3|7))
		_, input := inspectFixture(t, index)
		if _, err := Inspect(context.Background(), input); err == nil ||
			!strings.Contains(err.Error(), "skip SCIP field") {
			t.Fatalf("unsupported wire skip = %v", err)
		}
	})
}

func TestTextEncodingName(t *testing.T) {
	for encoding, want := range map[int32]string{0: "unspecified", 1: "utf8", 2: "utf16", 3: "utf32", 9: "unsupported(9)"} {
		if got := textEncodingName(encoding); got != want {
			t.Errorf("textEncodingName(%d) = %q, want %q", encoding, got, want)
		}
	}
}
