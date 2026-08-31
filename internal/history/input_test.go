package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func writeHistoryFixture(t *testing.T, path string, compiled History) []byte {
	t.Helper()
	content, err := json.Marshal(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return content
}

func preparedFixture(path string, content []byte) Input {
	sum := sha256.Sum256(content)
	return Input{Path: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content))}
}

func TestPrepareHistoryInputValidation(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "history.json")
	content := writeHistoryFixture(t, valid, validHistoryFixture())
	if _, err := PrepareInput(nil, valid); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := PrepareInput(context.Background(), ""); err == nil {
		t.Fatal("empty path was accepted")
	}
	if _, err := PrepareInput(context.Background(), filepath.Join(dir, "absent")); err == nil {
		t.Fatal("absent path was accepted")
	}
	symlink := filepath.Join(dir, "linked")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareInput(context.Background(), symlink); err == nil {
		t.Fatal("symlink was accepted")
	}
	input, err := PrepareInput(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	if input.SHA256 == "" || input.SizeBytes != int64(len(content)) {
		t.Fatalf("prepared input = %+v", input)
	}
	oversized := filepath.Join(dir, "oversized.json")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaximumCompiledHistoryBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareInput(context.Background(), oversized); err == nil {
		t.Fatal("oversized history was accepted")
	}
}

func TestReadCompiledFileValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	content := writeHistoryFixture(t, path, validHistoryFixture())
	input := preparedFixture(path, content)
	compiled, err := ReadCompiledFile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.SchemaVersion != SchemaVersion || compiled.SourceRevision != testHistoryHead {
		t.Fatalf("compiled history = %+v", compiled)
	}
	if _, err := ReadCompiledFile(context.Background(), Input{
		Path: path, SHA256: strings.Repeat("0", 64), SizeBytes: int64(len(content)),
	}); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompiledFile(
		context.Background(), preparedFixture(garbage, []byte("not json")),
	); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	unknown := filepath.Join(dir, "unknown.json")
	unknownContent := append(content[:len(content)-1], []byte(`,"unexpected":true}`)...)
	if err := os.WriteFile(unknown, unknownContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompiledFile(
		context.Background(), preparedFixture(unknown, unknownContent),
	); err == nil {
		t.Fatal("unknown history field was accepted")
	}
	if _, err := ReadCompiledFile(nil, input); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func TestCompiledHistoryRejectsUnsafeOrUnboundedText(t *testing.T) {
	validSymbol := func() (History, string) {
		compiled := validHistoryFixture()
		id := rkcmodel.StableID("history-symbol", "go", "function", "example.Safe")
		compiled.Commits[0].ChangedFiles = []string{"safe.go"}
		compiled.Commits[0].AddedSymbols = []string{id}
		compiled.Symbols = []SymbolHistory{{
			ID: id, Kind: "function", Name: "Safe", QualifiedName: "example.Safe",
			Language: "go", FirstObserved: testHistoryHead, LastObserved: testHistoryHead,
			Files: []string{"safe.go"}, CommitsTouching: []string{testHistoryHead},
			Signatures: []SignatureSnapshot{{
				Commit: testHistoryHead, Signature: "func Safe()", File: "safe.go",
			}},
		}}
		return compiled, id
	}
	tests := []struct {
		name   string
		mutate func(*History)
	}{
		{"absolute repository", func(value *History) { value.Repository = "/private/repository" }},
		{"repository control", func(value *History) { value.Repository = "repo\x1b[31m" }},
		{"oversized repository", func(value *History) {
			value.Repository = strings.Repeat("r", MaximumRepositoryLabelBytes+1)
		}},
		{"local source reference", func(value *History) { value.SourceReference = "file:///private/repo" }},
		{"source reference format control", func(value *History) {
			value.SourceReference = "https://example.test/repo\u202e.git"
		}},
		{"subject control", func(value *History) { value.Commits[0].Subject = "bad\x1b[31m" }},
		{"subject bidi control", func(value *History) { value.Commits[0].Subject = "bad\u202e" }},
		{"oversized subject", func(value *History) {
			value.Commits[0].Subject = strings.Repeat("s", MaximumCommitSubjectBytes+1)
		}},
		{"invalid date", func(value *History) { value.Commits[0].Date = "not-a-date" }},
		{"path control", func(value *History) { value.Commits[0].ChangedFiles = []string{"bad\t.go"} }},
		{"symbol name control", func(value *History) { value.Symbols[0].Name = "bad\nname" }},
		{"qualified name control", func(value *History) { value.Symbols[0].QualifiedName = "bad\x1bname" }},
		{"oversized signature", func(value *History) {
			value.Symbols[0].Signatures[0].Signature = strings.Repeat("s", MaximumSignatureBytes+1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, _ := validSymbol()
			test.mutate(&compiled)
			if err := validateCompiledHistory(compiled); err == nil {
				t.Fatal("unsafe or unbounded history text was accepted")
			}
		})
	}
}

func TestCompiledHistoryRequiresExactFirstParentChain(t *testing.T) {
	compiled := validHistoryFixture()
	compiled.CommitLimit = 2
	compiled.Commits[0].Parent = testHistoryOtherHead
	compiled.Commits = append(compiled.Commits, CommitRecord{
		ID: testHistoryOtherHead, Date: "2026-08-30T00:00:00Z", Subject: "parent",
		ChangedFiles: []string{}, AddedSymbols: []string{},
		RemovedSymbols: []string{}, ChangedSymbols: []string{},
	})
	if err := validateCompiledHistory(compiled); err != nil {
		t.Fatalf("valid first-parent chain rejected: %v", err)
	}
	compiled.Commits[0].Parent = strings.Repeat("c", 40)
	if err := validateCompiledHistory(compiled); err == nil {
		t.Fatal("broken first-parent chain was accepted")
	}
}

func TestEscapeTerminalTextNeutralizesControlsAndInvalidUTF8(t *testing.T) {
	input := string([]byte{'o', 'k', 0x1b, '[', '3', '1', 'm', '\n', 0xff}) + "\u202e"
	got := EscapeTerminalText(input)
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, '\n') ||
		strings.ContainsRune(got, '\u202e') || !strings.Contains(got, `\u001B`) ||
		!strings.Contains(got, `\xFF`) || !strings.Contains(got, `\u202E`) {
		t.Fatalf("terminal escape = %q", got)
	}
	for _, character := range got {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("escaped output retained unsafe rune %U", character)
		}
	}
}
