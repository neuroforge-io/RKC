package docparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func sourceTestFile(t *testing.T, root, path, language, body string) pluginapi.FileRef {
	t.Helper()
	writeMarkdownTestFile(t, root, path, body)
	digest := sha256.Sum256([]byte(body))
	return pluginapi.FileRef{ArtifactID: "artifact-" + path, Path: path, Language: language, SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(body))}
}

func TestSourceDocumentsMessyRecordsAndDeterministicProvenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	jsonBody := "\ufeff" // BOM is source data; an affected JSON record is invalid, never silently normalized.
	jsonBody += "{\"name\":\"初回\",\"number\":9007199254740993}\r\n\nnot json but useful field notes\r\n{\"name\":\"last\"}\n"
	files := []pluginapi.FileRef{
		sourceTestFile(t, root, "notes.txt", "text", "Research café\r\nInstructions: preserve as data.\r\n```unsafe\r\n"),
		sourceTestFile(t, root, "events.ndjson", "jsonl", jsonBody),
		sourceTestFile(t, root, "records.csv", "csv", "name,description,password\r\nAlice,\"quoted, line\r\ncontinued\",csv-private-value-8274\r\nBob,done,another-private-value-26\r\n"),
	}
	got, err := ExtractSources(context.Background(), Options{Root: root, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	other, err := ExtractSources(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{files[2], files[0], files[1]}})
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("source extraction depends on input order: %v", err)
	}
	if len(got.Documents) != 3 || len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "RKC-DATA-1005" {
		t.Fatalf("messy export accounting: documents=%d diagnostics=%+v", len(got.Documents), got.Diagnostics)
	}
	encoded, _ := json.Marshal(got)
	for _, credential := range []string{"csv-private-value-8274", "another-private-value-26"} {
		if bytes.Contains(encoded, []byte(credential)) {
			t.Fatalf("canonical document leaked sensitive CSV column: %q", credential)
		}
	}
	for _, document := range got.Documents {
		if document.Generator != SourcePluginID || document.Attributes["producer_verified"] != false || document.Status != "validated" {
			t.Fatalf("unverified export authority changed: %+v", document)
		}
		if document.Path == "events.ndjson" {
			if len(document.Sections) != 3 || document.Attributes["record_count"] != 3 || document.Attributes["invalid_record_count"] != 2 {
				t.Fatalf("JSONL did not retain malformed records: %+v", document)
			}
			if !strings.Contains(document.Sections[0].PlainText, "9007199254740993") || document.Sections[1].Attributes["start_line"] != 3 {
				t.Fatalf("JSONL lost integer precision or original lines: %+v", document.Sections)
			}
		}
		if document.Path == "records.csv" {
			if len(document.Sections) != 3 || !strings.Contains(document.Sections[1].PlainText, "Column 2 (description): quoted, line\ncontinued") || !strings.Contains(document.Sections[1].PlainText, "[REDACTED]") {
				t.Fatalf("quoted multiline CSV projection = %+v", document.Sections)
			}
			if document.Sections[1].Attributes["start_line"] != 2 || document.Sections[1].Attributes["end_line"] != 3 {
				t.Fatalf("multiline record range = %+v", document.Sections[1].Attributes)
			}
		}
	}
	byPath := map[string]pluginapi.FileRef{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	for _, evidence := range got.Evidence {
		file := byPath[evidence.Source.Path]
		if evidence.Kind != "documentation_asserted" || evidence.Tool != SourcePluginID || evidence.InputDigest != file.SHA256 || evidence.Source.ArtifactID != file.ArtifactID || evidence.Source.EndByte > file.SizeBytes || evidence.Source.StartLine < 1 {
			t.Fatalf("invalid evidence binding: %+v", evidence)
		}
	}
}

func TestSourceDocumentBoundariesTruncationAndCSVRecovery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, language, body string
		wantParts, records   int
		complete             bool
	}{
		{"plain boundary", "text", strings.Repeat("α", MaximumSourceSectionBytes), 2, 0, true},
		{"record section cap", "jsonl", strings.Repeat("{\"a\":1}\n", MaximumSourceSections+3), MaximumSourceSections, MaximumSourceSections + 3, false},
		{"long JSON record", "jsonl", "{\"note\":\"" + strings.Repeat("α", MaximumSourceSectionBytes) + "\"}\n", 1, 1, false},
		{"wide CSV projection", "csv", strings.Repeat("α", MaximumSourceSectionBytes) + ",password\nvalue,private-fixture-value\n", 2, 2, false},
		{"broken CSV suffix", "csv", "name,password\nAlice,short-pass\nBob,\"unterminated\nOther,still-secret", 3, 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := parseSource(context.Background(), []byte(test.body), test.language)
			if len(got.parts) != test.wantParts || got.records != test.records || got.complete != test.complete {
				t.Fatalf("source receipt = %+v", got)
			}
			for _, part := range got.parts {
				if len(part.body) > MaximumSourceSectionBytes || !utf8.ValidString(part.body) {
					t.Fatalf("invalid section boundary: %+v", part)
				}
				if test.name == "broken CSV suffix" && (strings.Contains(part.body, "still-secret") || strings.Contains(part.body, "short-pass")) {
					t.Fatal("malformed CSV exposed a sensitive suffix")
				}
			}
		})
	}
}

func TestSourceDocumentsResourcePathDigestAndCancellationLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	valid := sourceTestFile(t, root, "valid.log", "log", "event happened according to this log\n")
	mismatch := valid
	mismatch.SHA256 = strings.Repeat("0", 64)
	large := valid
	large.SizeBytes = MaximumSourceFileBytes + 1
	invalid := sourceTestFile(t, root, "invalid.txt", "text", string([]byte{0xff, '\n'}))
	outside := valid
	outside.Path = "../outside.log"
	for _, test := range []struct {
		file pluginapi.FileRef
		code string
	}{{mismatch, "RKC-DATA-1002"}, {large, "RKC-DATA-1001"}, {invalid, "RKC-DATA-1003"}, {outside, "RKC-DATA-1002"}} {
		fragment, err := ExtractSources(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{test.file}})
		if err != nil || len(fragment.Documents) != 0 || len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != test.code {
			t.Fatalf("refused source = %+v, %v; want %s", fragment, err, test.code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractSources(ctx, Options{Root: root, Files: []pluginapi.FileRef{valid}}); err != context.Canceled {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := ExtractSources(nil, Options{}); err == nil {
		t.Fatal("nil context succeeded")
	}
	if err := os.Symlink(filepath.Join(root, "valid.log"), filepath.Join(root, "linked.log")); err == nil {
		linked := valid
		linked.Path = "linked.log"
		fragment, err := ExtractSources(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{linked}})
		if err != nil || len(fragment.Documents) != 0 {
			t.Fatalf("symlink document was read: %+v %v", fragment, err)
		}
	}
}

func TestSourceRedactionsSensitiveCSVColumnsKeepOriginalLayout(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ language, body string }{
		{"csv", "name,password,description\r\nAlice,\"p@ss,with\"\"quotes\",\"safe\r\ncontinuation\"\r\n"},
		{"tsv", "name\tapi_key\tdescription\nAlice\tshort-key\tremains searchable\n"},
		{"csv", "name,password\nAlice,mypass123\nBob,missing,extra\n"},
		{"csv", "name,notes\nAlice,\"{\"\"api_key\"\":\"\"embedded-key-9823\"\"}\"\n"},
	} {
		redacted := RedactSource([]byte(test.body), test.language)
		if len(redacted) != len(test.body) || strings.Count(string(redacted), "\n") != strings.Count(test.body, "\n") {
			t.Fatalf("redaction changed source layout: %q", redacted)
		}
		for _, value := range []string{"p@ss", "short-key", "mypass123", "missing", "embedded-key-9823"} {
			if bytes.Contains(redacted, []byte(value)) {
				t.Fatalf("CSV credential survived: %q", redacted)
			}
		}
		for _, finding := range SourceRedactions([]byte(test.body), test.language) {
			if finding.StartLine < 1 || finding.EndLine < finding.StartLine || len(finding.Fingerprint) != 16 {
				t.Fatalf("missing redaction receipt: %+v", finding)
			}
		}
	}
}

func TestSourceCancellationWithholdsPartialRecordsAndRedactions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, language := range []string{"text", "jsonl", "csv", "tsv"} {
		if output, err := RedactSourceContext(ctx, []byte("policy,password\nnotes,private-data\n"), language); err != context.Canceled || output != nil {
			t.Fatalf("cancelled redaction returned data for %s: %q, %v", language, output, err)
		}
	}
	if _, err := RedactSourceContext(nil, []byte("text"), "text"); err == nil {
		t.Fatal("nil redaction context succeeded")
	}
	readerCtx, readerCancel := context.WithCancel(context.Background())
	reader := sourceContextReader{ctx: readerCtx, reader: &cancelSourceReader{cancel: readerCancel}}
	buffer := make([]byte, 8)
	if count, err := reader.Read(buffer); count != 1 || err != nil {
		t.Fatalf("initial context read = %d, %v", count, err)
	}
	if count, err := reader.Read(buffer); count != 0 || err != context.Canceled {
		t.Fatalf("cancelled context read = %d, %v", count, err)
	}
}

type cancelSourceReader struct{ cancel context.CancelFunc }

func (reader *cancelSourceReader) Read(buffer []byte) (int, error) {
	buffer[0] = 'x'
	reader.cancel()
	return 1, nil
}
