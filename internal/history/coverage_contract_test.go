package history

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func richHistoryFixture() (History, string, string) {
	compiled := validHistoryFixture()
	compiled.CommitLimit = 2
	compiled.Commits[0].Parent = testHistoryOtherHead
	compiled.Commits = append(compiled.Commits, CommitRecord{
		ID: testHistoryOtherHead, Date: "2026-08-30T00:00:00Z", Subject: "parent",
		ChangedFiles: []string{}, AddedSymbols: []string{}, RemovedSymbols: []string{}, ChangedSymbols: []string{},
	})
	oldID := rkcmodel.StableID("history-symbol", "go", "function", "example.Old")
	newID := rkcmodel.StableID("history-symbol", "go", "function", "example.New")
	compiled.Commits[0].ChangedFiles = []string{"new.go", "old.go"}
	compiled.Commits[0].AddedSymbols = []string{newID}
	compiled.Commits[0].RemovedSymbols = []string{oldID}
	compiled.Symbols = []SymbolHistory{
		{
			ID: oldID, Kind: "function", Name: "Old", QualifiedName: "example.Old", Language: "go",
			FirstObserved: testHistoryHead, LastObserved: testHistoryHead, Files: []string{"old.go"},
			CommitsTouching: []string{testHistoryHead},
			Signatures:      []SignatureSnapshot{{Commit: testHistoryHead, Signature: "func Old()", File: "old.go"}},
		},
		{
			ID: newID, Kind: "function", Name: "New", QualifiedName: "example.New", Language: "go",
			FirstObserved: testHistoryHead, LastObserved: testHistoryHead, Files: []string{"new.go"},
			CommitsTouching: []string{testHistoryHead},
			Signatures:      []SignatureSnapshot{{Commit: testHistoryHead, Signature: "func New()", File: "new.go"}},
		},
	}
	compiled.Refactors = []Refactor{{
		Commit: testHistoryHead, Language: "go", Kind: "function",
		From: "example.Old", To: "example.New", QualifiedFrom: "example.Old", QualifiedTo: "example.New",
	}}
	return compiled, oldID, newID
}

func rebindHistoryRevision(compiled *History, revision string) {
	compiled.SourceRevision = revision
	compiled.Commit = revision
	compiled.SourceID = historySourceID(compiled.RepositoryID, revision)
}

func TestCompiledHistoryValidationFailsAtEachTrustBoundary(t *testing.T) {
	thirdCommit := strings.Repeat("c", 40)
	tests := []struct {
		name string
		want string
		edit func(*History, string, string)
	}{
		{"repository identity", "repository_id", func(h *History, _, _ string) { h.RepositoryID = "foreign" }},
		{"invalid revision", "source_revision", func(h *History, _, _ string) { rebindHistoryRevision(h, "invalid") }},
		{"head differs from revision", "source_revision", func(h *History, _, _ string) { h.Commit = testHistoryOtherHead }},
		{"source identity", "source_id", func(h *History, _, _ string) { h.SourceID = "foreign" }},
		{"commit limit", "commit_limit", func(h *History, _, _ string) { h.CommitLimit = 0 }},
		{"empty commits", "commit count", func(h *History, _, _ string) { h.Commits = nil }},
		{"truncated partial window", "does not fill", func(h *History, _, _ string) { h.CommitLimit = 3; h.WindowTruncated = true }},
		{"duplicate commit", "duplicated", func(h *History, _, _ string) {
			h.Commits[1].ID = testHistoryHead
			h.Commits[0].Parent = testHistoryHead
		}},
		{"newest commit mismatch", "newest observed", func(h *History, _, _ string) { rebindHistoryRevision(h, thirdCommit) }},
		{"broken chain", "first-parent chain", func(h *History, _, _ string) { h.Commits[0].Parent = thirdCommit }},
		{"unsupported changed path", "unsupported path", func(h *History, _, _ string) { h.Commits[0].ChangedFiles = []string{"README.md"} }},
		{"truncated root", "next first-parent", func(h *History, _, _ string) { h.WindowTruncated = true }},
		{"complete non-root", "terminate", func(h *History, _, _ string) { h.Commits[1].Parent = thirdCommit }},
		{"invalid symbol language", "invalid or out of bounds", func(h *History, _, _ string) { h.Symbols[0].Language = "python" }},
		{"symbol semantic identity", "semantic identity", func(h *History, _, _ string) { h.Symbols[0].ID = "wrong" }},
		{"duplicate symbol", "duplicated", func(h *History, _, _ string) { h.Symbols = append(h.Symbols, h.Symbols[0]) }},
		{"first observation outside", "first observation", func(h *History, _, _ string) { h.Symbols[0].FirstObserved = thirdCommit }},
		{"last observation outside", "last observation", func(h *History, _, _ string) { h.Symbols[0].LastObserved = thirdCommit }},
		{"symbol file language", "unsupported path", func(h *History, _, _ string) { h.Symbols[0].Files = []string{"old.ts"} }},
		{"invalid signature", "invalid signature", func(h *History, _, _ string) { h.Symbols[0].Signatures[0].Signature = "" }},
		{"signature outside", "signature is outside", func(h *History, _, _ string) { h.Symbols[0].Signatures[0].Commit = thirdCommit }},
		{"event outside", "event is outside", func(h *History, _, _ string) { h.Symbols[0].CommitsTouching = []string{thirdCommit} }},
		{"unknown commit symbol", "unknown symbol", func(h *History, _, _ string) { h.Commits[0].AddedSymbols = []string{"unknown"} }},
		{"repeated commit symbol", "repeats symbol", func(h *History, _, newID string) { h.Commits[0].AddedSymbols = []string{newID, newID} }},
		{"invalid refactor", "refactor is invalid", func(h *History, _, _ string) {
			h.Refactors[0].QualifiedTo = h.Refactors[0].QualifiedFrom
			h.Refactors[0].To = h.Refactors[0].From
		}},
		{"refactor outside", "commit is outside", func(h *History, _, _ string) { h.Refactors[0].Commit = thirdCommit }},
		{"refactor source missing", "source is not", func(h *History, _, _ string) {
			h.Refactors[0].From = "example.Missing"
			h.Refactors[0].QualifiedFrom = "example.Missing"
		}},
		{"refactor target missing", "target is not", func(h *History, _, _ string) {
			h.Refactors[0].To = "example.Missing"
			h.Refactors[0].QualifiedTo = "example.Missing"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, oldID, newID := richHistoryFixture()
			if err := validateCompiledHistory(compiled); err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			test.edit(&compiled, oldID, newID)
			err := validateCompiledHistory(compiled)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHistoryParsersCoverRenameCopyAndMalformedRecords(t *testing.T) {
	date := "2026-08-31T00:00:00Z"
	if _, err := parseGitLog([]byte("incomplete\x00record\x00")); err == nil {
		t.Fatal("malformed Git log was accepted")
	}
	badParent := []byte(testHistoryHead + "\x00not-a-parent\x00" + date + "\x00subject\x00")
	if _, err := parseGitLog(badParent); err == nil {
		t.Fatal("invalid parent identity was accepted")
	}

	for name, data := range map[string][]byte{
		"empty":              []byte("\x00"),
		"truncated":          []byte("M\x00"),
		"unsafe rename":      []byte("R100\x00../old.go\x00new.go\x00"),
		"unsupported status": []byte("X\x00file.go\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseNameStatus(data); err == nil {
				t.Fatal("malformed name-status input was accepted")
			}
		})
	}

	for name, test := range map[string]struct {
		data []byte
		want []fileChange
	}{
		"cross-language rename": {
			[]byte("R100\x00old.go\x00new.ts\x00"),
			[]fileChange{{status: 'A', oldPath: "new.ts", newPath: "new.ts"}, {status: 'D', oldPath: "old.go", newPath: "old.go"}},
		},
		"rename to unsupported": {
			[]byte("R100\x00old.go\x00README.md\x00"),
			[]fileChange{{status: 'D', oldPath: "old.go", newPath: "old.go"}},
		},
		"rename from unsupported": {
			[]byte("R100\x00README.md\x00new.go\x00"),
			[]fileChange{{status: 'A', oldPath: "new.go", newPath: "new.go"}},
		},
		"copy into supported": {
			[]byte("C100\x00README.md\x00copy.go\x00"),
			[]fileChange{{status: 'C', oldPath: "README.md", newPath: "copy.go"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseNameStatus(test.data)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("changes = %+v, err %v, want %+v", got, err, test.want)
			}
		})
	}

	sorted, err := parseNameStatus([]byte("R100\x00z.go\x00same.go\x00R100\x00a.go\x00same.go\x00T\x00same.go\x00M\x00same.go\x00"))
	if err != nil || len(sorted) != 4 {
		t.Fatalf("sortable name-status records = %+v, err %v", sorted, err)
	}
}

func observation(name, qualified, signature, path string) symbolObservation {
	return symbolObservation{
		node:     rkcmodel.Node{Kind: "function", Name: name, QualifiedName: qualified, Signature: signature},
		language: "go", path: path,
	}
}

func TestLifecycleMoveAndRefactorBoundaries(t *testing.T) {
	id := "stable"
	delta := newCommitDelta()
	delta.added[id] = []symbolObservation{observation("A", "pkg.A", "func A()", "new.go")}
	delta.removed[id] = []symbolObservation{observation("A", "pkg.A", "func A()", "old.go")}
	reconcileMoves(&delta)
	if len(delta.added) != 0 || len(delta.removed) != 0 || len(delta.changed[id]) != 2 {
		t.Fatalf("reconciled move = %+v", delta)
	}

	if _, err := recordLifecycle(map[string]*SymbolHistory{}, id, testHistoryHead, nil); err == nil {
		t.Fatal("empty lifecycle event was accepted")
	}
	entries := map[string]*SymbolHistory{}
	base := observation("A", "pkg.A", "func A()", "a.go")
	if _, err := recordLifecycle(entries, id, testHistoryHead, []symbolObservation{base}); err != nil {
		t.Fatal(err)
	}
	changedKey := observation("A", "other.A", "func A()", "a.go")
	if _, err := recordLifecycle(entries, id, testHistoryHead, []symbolObservation{changedKey}); err == nil {
		t.Fatal("semantic-key mutation was accepted")
	}
	emptySignature := observation("A", "pkg.A", "", "a.go")
	if _, err := recordLifecycle(entries, id, testHistoryHead, []symbolObservation{emptySignature}); err != nil {
		t.Fatalf("empty signature should remain an observation: %v", err)
	}
	entry := entries[id]
	entry.Signatures = make([]SignatureSnapshot, MaximumSignaturesPerSymbol)
	for index := range entry.Signatures {
		entry.Signatures[index] = SignatureSnapshot{Commit: testHistoryHead, Signature: "signature " + string(rune('a'+index%26)), File: "a.go"}
	}
	truncated, err := recordLifecycle(entries, id, testHistoryHead, []symbolObservation{observation("A", "pkg.A", "new signature", "a.go")})
	if err != nil || !truncated || !entry.SignatureHistoryTruncated || len(entry.Signatures) != MaximumSignaturesPerSymbol {
		t.Fatalf("signature cap = truncated %v err %v entry %+v", truncated, err, entry)
	}

	withoutSignature := detectRefactors(testHistoryHead,
		map[string][]symbolObservation{"new": {observation("New", "pkg.New", "", "a.go")}},
		map[string][]symbolObservation{"old": {observation("Old", "pkg.Old", "", "a.go")}},
	)
	if len(withoutSignature) != 0 {
		t.Fatalf("signature-free refactor inferred: %+v", withoutSignature)
	}
	sameName := observation("Same", "pkg.Same", "func Same()", "a.go")
	if got := detectRefactors(testHistoryHead,
		map[string][]symbolObservation{"new": {sameName}},
		map[string][]symbolObservation{"old": {sameName}},
	); len(got) != 0 {
		t.Fatalf("identity-preserving change inferred as refactor: %+v", got)
	}

	if got := signatureKey(rkcmodel.Node{}); got != "" {
		t.Fatalf("empty signature key = %q", got)
	}
	if got := signatureKey(rkcmodel.Node{Name: "Missing", Signature: "func Present()"}); got != "func Present()" {
		t.Fatalf("absent-name signature key = %q", got)
	}
}

func TestHistoryMaterializationAndGitCommandBounds(t *testing.T) {
	if _, err := extractSymbolsAt(context.Background(), "git", t.TempDir(), testHistoryHead, "../unsafe.go"); err == nil {
		t.Fatal("unsafe materialization path was accepted")
	}
	root := gitFixture(t, fixtureCommit{"typescript", map[string]string{
		"value.ts": "export function Value(input: string): string { return input }\n",
	}})
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil || len(compiled.Symbols) == 0 || compiled.Symbols[0].Language != "typescript" {
		t.Fatalf("TypeScript history = %+v, err %v", compiled.Symbols, err)
	}
	if _, err := extractSymbolsAt(context.Background(), "git", root, strings.Repeat("f", 40), "value.ts"); err == nil {
		t.Fatal("missing revision materialized successfully")
	}

	if _, err := gitOutputBounded(context.Background(), "", root, 1, "status"); err == nil {
		t.Fatal("unconfigured bounded Git command was accepted")
	}
	if _, err := gitOutputBounded(context.Background(), filepath.Join(root, "missing-git"), root, 1, "status"); err == nil {
		t.Fatal("missing Git executable started")
	}
	if _, err := gitOutputBounded(context.Background(), "sh", root, 3, "-c", "printf 12345"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized Git output = %v", err)
	}
}

func writeHistoryCommand(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history-command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepositoryIdentityOmitsNonportableOrigins(t *testing.T) {
	root := t.TempDir()
	for name, output := range map[string]string{
		"empty":      "",
		"local path": "/private/repository",
		"file URL":   "file:///private/repository",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := repositorySourceReference(context.Background(), writeHistoryCommand(t, output), root)
			if err != nil || got != "" {
				t.Fatalf("source reference = %q, err %v", got, err)
			}
		})
	}
	if _, err := repositorySourceReference(context.Background(), writeHistoryCommand(t, "https://%zz"), root); err == nil {
		t.Fatal("malformed network origin was accepted")
	}
	if got := repositoryLabel(string(filepath.Separator)); got != "repository" {
		t.Fatalf("root repository label = %q", got)
	}

	missing := filepath.Join(root, "missing")
	if _, err := resolveHistoryRoot(missing); err == nil {
		t.Fatal("missing history root was accepted")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveHistoryRoot(file); err == nil {
		t.Fatal("regular file was accepted as a history root")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveHistoryRoot(link); err == nil {
		t.Fatal("symlink was accepted as a history root")
	}
}

func TestPreparedInputIdentityAndTrailingContentFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareInput(ctx, "unused"); err != context.Canceled {
		t.Fatalf("cancelled prepare = %v", err)
	}
	if _, err := ReadCompiledFile(ctx, Input{}); err != context.Canceled {
		t.Fatalf("cancelled read = %v", err)
	}
	if _, err := ReadCompiledFile(context.Background(), Input{SizeBytes: -1, SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("negative prepared size was accepted")
	}
	if _, _, err := readBoundedRegularFile("", 1); err == nil {
		t.Fatal("empty bounded-file path was accepted")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	content := writeHistoryFixture(t, path, validHistoryFixture())
	input := preparedFixture(path, content)
	wrongSize := input
	wrongSize.SizeBytes++
	if _, err := ReadCompiledFile(context.Background(), wrongSize); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("size mismatch = %v", err)
	}
	missing := input
	missing.Path = filepath.Join(dir, "missing.json")
	if _, err := ReadCompiledFile(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "read history") {
		t.Fatalf("missing prepared input = %v", err)
	}

	trailingContent := append(append([]byte(nil), content...), []byte("\n{}")...)
	trailing := filepath.Join(dir, "trailing.json")
	if err := os.WriteFile(trailing, trailingContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompiledFile(context.Background(), preparedFixture(trailing, trailingContent)); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing content = %v", err)
	}

	invalid := validHistoryFixture()
	invalid.SourceID = "wrong"
	invalidPath := filepath.Join(dir, "invalid.json")
	invalidContent := writeHistoryFixture(t, invalidPath, invalid)
	if _, err := ReadCompiledFile(context.Background(), preparedFixture(invalidPath, invalidContent)); err == nil || !strings.Contains(err.Error(), "validate history") {
		t.Fatalf("invalid compiled history = %v", err)
	}

	if validBoundedText("", 8, false) || !validBoundedText("", 8, true) || validBoundedText("safe", 0, false) {
		t.Fatal("bounded text empty/limit policy is inconsistent")
	}
	if validGitDate("") {
		t.Fatal("empty Git date was accepted")
	}
	if got := EscapeTerminalText("\U000E0001"); got != "\\U000E0001" {
		t.Fatalf("non-BMP format-control escape = %q", got)
	}
}

func TestHistoryImportIsIdempotentAndRejectsPartialAuthority(t *testing.T) {
	if _, err := Import(nil, validHistoryBundle(), validHistoryFixture()); err == nil {
		t.Fatal("nil import context was accepted")
	}
	if _, err := Import(context.Background(), nil, validHistoryFixture()); err == nil {
		t.Fatal("nil import bundle was accepted")
	}
	badSchema := validHistoryFixture()
	badSchema.SchemaVersion = "future"
	if _, err := Import(context.Background(), validHistoryBundle(), badSchema); err == nil {
		t.Fatal("unsupported history schema was accepted")
	}

	compiled, _, _ := richHistoryFixture()
	bundle := validHistoryBundle()
	bundle.Nodes = []rkcmodel.Node{
		{ID: "old", Kind: "function", QualifiedName: "example.Old", Language: "go"},
		{ID: "new", Kind: "function", QualifiedName: "example.New", Language: "go"},
	}
	first, err := Import(context.Background(), bundle, compiled)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Import(context.Background(), bundle, compiled)
	if err != nil {
		t.Fatal(err)
	}
	if first.SupersedesEdges != 1 || first.EvidenceRecords != 3 || second.SupersedesEdges != 0 || second.EvidenceRecords != 0 {
		t.Fatalf("idempotent import stats = first %+v second %+v", first, second)
	}
	if len(bundle.Edges) != 1 || len(bundle.Evidence) != 3 {
		t.Fatalf("idempotent import duplicated records: edges=%d evidence=%d", len(bundle.Edges), len(bundle.Evidence))
	}

	partial := validHistoryBundle()
	partial.Nodes = []rkcmodel.Node{{ID: "old", Kind: "function", QualifiedName: "example.Old", Language: "go"}}
	stats, err := Import(context.Background(), partial, compiled)
	if err != nil || stats.SupersedesEdges != 0 {
		t.Fatalf("partial target import = %+v, err %v", stats, err)
	}

	wrongLabel := validHistoryBundle()
	wrongLabel.Snapshot.RootName = "other"
	if _, err := Import(context.Background(), wrongLabel, validHistoryFixture()); err == nil {
		t.Fatal("mismatched repository label was accepted")
	}

	cancelled := validHistoryBundle()
	cancelled.Nodes = []rkcmodel.Node{{ID: "unmatched", Kind: "function", QualifiedName: "none", Language: "go"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Import(ctx, cancelled, validHistoryFixture()); err != context.Canceled {
		t.Fatalf("cancelled import = %v", err)
	}

	evidence := rkcmodel.Evidence{ID: "evidence"}
	existing := map[string]struct{}{"evidence": {}}
	if appendHistoryEvidence(&rkcmodel.Bundle{}, existing, "evidence", evidence) {
		t.Fatal("duplicate history evidence was appended")
	}
}
