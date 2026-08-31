package history

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type fixtureCommit struct {
	subject string
	files   map[string]string
}

func gitFixture(t *testing.T, commits ...fixtureCommit) string {
	t.Helper()
	root := t.TempDir()
	runGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		return string(output)
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Private Test Author")
	runGit("config", "commit.gpgsign", "false")
	previous := make(map[string]struct{})
	for _, commit := range commits {
		for path := range previous {
			if _, retained := commit.files[path]; retained {
				continue
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
				t.Fatal(err)
			}
			delete(previous, path)
		}
		for path, content := range commit.files {
			full := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			previous[path] = struct{}{}
		}
		runGit("add", "-A")
		runGit("commit", "-q", "-m", commit.subject)
	}
	return root
}

func symbolByName(t *testing.T, compiled History, name string) SymbolHistory {
	t.Helper()
	for _, symbol := range compiled.Symbols {
		if symbol.Name == name {
			return symbol
		}
	}
	t.Fatalf("symbol %q missing: %+v", name, compiled.Symbols)
	return SymbolHistory{}
}

func TestHistoryBuildMaintainsPerFileStateAndExactDeltas(t *testing.T) {
	root := gitFixture(t,
		fixtureCommit{"add alpha", map[string]string{
			"a.go": "package a\n\nfunc Alpha() string { return \"a\" }\n",
		}},
		fixtureCommit{"add beta", map[string]string{
			"a.go": "package a\n\nfunc Alpha() string { return \"a\" }\n",
			"b.go": "package a\n\nfunc Beta() string { return \"b\" }\n",
		}},
		fixtureCommit{"change alpha", map[string]string{
			"a.go": "package a\n\nfunc Alpha(value string) string { return value }\n",
			"b.go": "package a\n\nfunc Beta() string { return \"b\" }\n",
		}},
	)
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.SchemaVersion != SchemaVersion || compiled.Commit == "" || compiled.CommitLimit != DefaultMaxCommits {
		t.Fatalf("history metadata = %+v", compiled)
	}
	wantRepositoryID := rkcmodel.StableID("repository", filepath.Base(root))
	if compiled.RepositoryID != wantRepositoryID || compiled.SourceReference != "" ||
		compiled.SourceRevision != compiled.Commit || compiled.RevisionPolicy != RevisionPolicyExactHead ||
		compiled.AncestryPolicy != AncestryPolicyFirstParent ||
		compiled.SourceID != historySourceID(wantRepositoryID, compiled.Commit) {
		t.Fatalf("history source affinity = %+v", compiled)
	}
	if compiled.Repository != filepath.Base(root) || filepath.IsAbs(compiled.Repository) ||
		strings.Contains(fmt.Sprint(compiled), "Private Test Author") ||
		strings.Contains(fmt.Sprint(compiled), root) {
		t.Fatalf("compiled history disclosed host or author state: %+v", compiled)
	}
	if len(compiled.Commits) != 3 || compiled.Commits[0].Subject != "change alpha" {
		t.Fatalf("commit ordering = %+v", compiled.Commits)
	}
	alpha := symbolByName(t, compiled, "Alpha")
	if alpha.FirstObserved == "" || alpha.LastObserved != compiled.Commits[0].ID {
		t.Fatalf("Alpha observations = %+v", alpha)
	}
	if len(alpha.CommitsTouching) != 2 || len(alpha.Signatures) != 2 {
		t.Fatalf("unchanged-file commit was incorrectly attributed to Alpha: %+v", alpha)
	}
	beta := symbolByName(t, compiled, "Beta")
	if len(beta.CommitsTouching) != 1 {
		t.Fatalf("Beta events = %+v", beta)
	}
	if len(compiled.Commits[0].ChangedSymbols) != 1 || len(compiled.Commits[0].AddedSymbols) != 0 {
		t.Fatalf("signature delta = %+v", compiled.Commits[0])
	}
	if len(compiled.Commits[1].AddedSymbols) != 1 || len(compiled.Commits[1].ChangedSymbols) != 0 {
		t.Fatalf("addition delta = %+v", compiled.Commits[1])
	}
	if len(compiled.Refactors) != 0 {
		t.Fatalf("unrelated consecutive file changes fabricated a refactor: %+v", compiled.Refactors)
	}
}

func TestHistoryDeletionUsesParentBlob(t *testing.T) {
	root := gitFixture(t,
		fixtureCommit{"add doomed", map[string]string{
			"doomed.go": "package doomed\n\nfunc Doomed() {}\n",
		}},
		fixtureCommit{"delete doomed", map[string]string{
			"remaining.go": "package doomed\n\nfunc Remaining() {}\n",
		}},
	)
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	doomed := symbolByName(t, compiled, "Doomed")
	if doomed.LastObserved != compiled.Commits[0].ID || !containsString(doomed.Files, "doomed.go") {
		t.Fatalf("deleted lifecycle = %+v", doomed)
	}
	if len(compiled.Commits[0].RemovedSymbols) != 1 {
		t.Fatalf("deletion delta = %+v", compiled.Commits[0])
	}
}

func TestHistoryRenameAndMoveCandidatesAreUniqueAndLanguageBound(t *testing.T) {
	root := gitFixture(t,
		fixtureCommit{"original", map[string]string{
			"pkg/a.go": "package pkg\n\nfunc Widget() string { return \"w\" }\n",
		}},
		fixtureCommit{"renamed", map[string]string{
			"pkg/a.go": "package pkg\n\nfunc Gadget() string { return \"w\" }\n",
		}},
		fixtureCommit{"moved", map[string]string{
			"other/b.go": "package other\n\nfunc Gadget() string { return \"w\" }\n",
		}},
	)
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Refactors) != 2 {
		t.Fatalf("refactor candidates = %+v", compiled.Refactors)
	}
	for _, refactor := range compiled.Refactors {
		if refactor.Language != "go" || refactor.QualifiedFrom == refactor.QualifiedTo {
			t.Fatalf("refactor lost semantic identity: %+v", refactor)
		}
	}

	// Ambiguous same-shape replacement pairs are deliberately not inferred.
	ambiguous := gitFixture(t,
		fixtureCommit{"two originals", map[string]string{
			"a.go": "package a\n\nfunc A() {}\nfunc B() {}\n",
		}},
		fixtureCommit{"two replacements", map[string]string{
			"a.go": "package a\n\nfunc C() {}\nfunc D() {}\n",
		}},
	)
	result, err := Build(context.Background(), Options{Repository: ambiguous})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refactors) != 0 {
		t.Fatalf("ambiguous replacements inferred as refactors: %+v", result.Refactors)
	}
}

func TestHistoryWindowIsExplicitlyObservedAndTruncated(t *testing.T) {
	root := gitFixture(t,
		fixtureCommit{"one", map[string]string{"a.go": "package a\n\nfunc A() {}\n"}},
		fixtureCommit{"two", map[string]string{"a.go": "package a\n\nfunc A(x int) {}\n"}},
	)
	compiled, err := Build(context.Background(), Options{Repository: root, MaxCommits: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Commits) != 1 || !compiled.WindowTruncated || compiled.CommitLimit != 1 {
		t.Fatalf("bounded observation metadata = %+v", compiled)
	}
	a := symbolByName(t, compiled, "A")
	if a.FirstObserved != compiled.Commits[0].ID || a.LastObserved != compiled.Commits[0].ID {
		t.Fatalf("bounded lifecycle overclaimed: %+v", a)
	}
	if _, err := Build(context.Background(), Options{Repository: root, MaxCommits: -1}); err == nil {
		t.Fatal("negative commit limit was accepted")
	}
	if _, err := Build(context.Background(), Options{Repository: root, MaxCommits: MaximumCommits + 1}); err == nil {
		t.Fatal("oversized commit limit was accepted")
	}
}

func TestHistoryNameStatusIsNULDelimitedAndPathSafe(t *testing.T) {
	weird := "odd name part.go"
	root := gitFixture(t,
		fixtureCommit{"odd path", map[string]string{weird: "package odd\n\nfunc Odd() {}\n"}},
		fixtureCommit{"odd path changed", map[string]string{weird: "package odd\n\nfunc Odd(x int) {}\n"}},
	)
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled.Commits[0].ChangedFiles, []string{weird}) {
		t.Fatalf("changed paths = %q", compiled.Commits[0].ChangedFiles)
	}
	if _, err := parseNameStatus([]byte("M\x00../escape.go\x00")); err == nil {
		t.Fatal("traversal path was accepted")
	}
	if _, err := parseNameStatus([]byte("M\x00bad\tname.go\x00")); err == nil {
		t.Fatal("control-bearing path was accepted")
	}
	if _, err := parseNameStatus([]byte("R100\x00old.go\x00")); err == nil {
		t.Fatal("truncated rename record was accepted")
	}
}

func TestHistorySourceReferenceIsCanonicalAndCredentialFree(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"one", map[string]string{
		"a.go": "package a\n\nfunc A() {}\n",
	}})
	command := exec.Command(
		"git", "remote", "add", "origin",
		"https://user:secret@example.test/NeuroforgeIO/RKC.git?token=secret#secret",
	)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure fixture origin: %v\n%s", err, output)
	}
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	wantOrigin := "https://example.test/NeuroforgeIO/RKC.git"
	wantRepositoryID := rkcmodel.StableID("repository", wantOrigin)
	if compiled.SourceReference != wantOrigin || compiled.RepositoryID != wantRepositoryID ||
		compiled.SourceID != historySourceID(wantRepositoryID, compiled.SourceRevision) {
		t.Fatalf("canonical source affinity = %+v", compiled)
	}
	serialized := fmt.Sprint(compiled)
	if strings.Contains(serialized, "user") || strings.Contains(serialized, "secret") ||
		strings.Contains(serialized, root) {
		t.Fatalf("compiled history retained private source material: %s", serialized)
	}
}

func TestHistoryRejectsUnsafeGitLogText(t *testing.T) {
	record := func(subject, date, id string) []byte {
		return []byte(id + "\x00\x00" + date + "\x00" + subject + "\x00")
	}
	date := "2026-08-31T00:00:00Z"
	head := strings.Repeat("a", 40)
	if _, err := parseGitLog(record("safe", date, head)); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	for _, data := range [][]byte{
		record("bad\x1b[31m", date, head),
		record(strings.Repeat("s", MaximumCommitSubjectBytes+1), date, head),
		record("safe", "not-a-date", head),
		record("safe", date, strings.ToUpper(head)),
	} {
		if _, err := parseGitLog(data); err == nil {
			t.Fatal("unsafe Git log record was accepted")
		}
	}
}

func TestHistorySupportsOnlyImplementedExtractors(t *testing.T) {
	if analyzablePath("pkg/mod.py") || languageOf("pkg/mod.py") != "" {
		t.Fatal("unsupported Python history was advertised")
	}
	if !analyzablePath("pkg/mod.tsx") || languageOf("pkg/mod.tsx") != "typescript" {
		t.Fatal("TypeScript classification is wrong")
	}
	root := gitFixture(t, fixtureCommit{"mixed", map[string]string{
		"tool.py": "def helper():\n    pass\n",
		"main.go": "package main\n\nfunc Main() {}\n",
	}})
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(compiled.Commits[0].ChangedFiles, "tool.py") {
		t.Fatalf("unsupported Python path entered history: %+v", compiled.Commits[0])
	}
}

func TestHistoryNormalizesMultilineSignatures(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"multiline", map[string]string{
		"main.go": "package main\n\nfunc Multiline(\n\tvalue string,\n) string {\n\treturn value\n}\n",
	}})
	compiled, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	symbol := symbolByName(t, compiled, "Multiline")
	if len(symbol.Signatures) != 1 || strings.ContainsAny(symbol.Signatures[0].Signature, "\r\n\t") {
		t.Fatalf("signature was not normalized to one safe line: %+v", symbol.Signatures)
	}
}

func TestHistoryFailsClosedOnInvalidSourceAndChangedFileCap(t *testing.T) {
	invalid := gitFixture(t, fixtureCommit{"invalid", map[string]string{
		"nul.go": "package a\x00\n",
	}})
	if _, err := Build(context.Background(), Options{Repository: invalid}); err == nil ||
		!strings.Contains(err.Error(), "contains NUL") {
		t.Fatalf("invalid source result = %v", err)
	}

	files := make(map[string]string)
	for index := 0; index < MaximumChangedFiles+1; index++ {
		files[fmt.Sprintf("f%d.go", index)] = fmt.Sprintf("package a\n\nfunc F%d() {}\n", index)
	}
	large := gitFixture(t, fixtureCommit{"too many", files})
	if _, err := Build(context.Background(), Options{Repository: large}); err == nil ||
		!strings.Contains(err.Error(), "maximum") {
		t.Fatalf("changed-file cap result = %v", err)
	}
}

func TestHistoryValidationAndGitFailures(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"one", map[string]string{"a.go": "package a\n\nfunc A() {}\n"}})
	if _, err := Build(nil, Options{Repository: root}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := Build(context.Background(), Options{Repository: ""}); err == nil {
		t.Fatal("empty repository was accepted")
	}
	if _, err := Build(context.Background(), Options{Repository: t.TempDir()}); err == nil {
		t.Fatal("non-repository was accepted")
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), Options{Repository: nested}); err == nil ||
		!strings.Contains(err.Error(), "exact Git work tree") {
		t.Fatalf("nested non-repository result = %v", err)
	}

	writeFakeGit := func(t *testing.T, script string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fakegit")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Run("rev parse", func(t *testing.T) {
		if _, err := Build(context.Background(), Options{
			Repository: t.TempDir(), GitExecutable: writeFakeGit(t, "exit 1\n"),
		}); err == nil {
			t.Fatal("Git failure was accepted")
		}
	})
	t.Run("malformed log", func(t *testing.T) {
		git := writeFakeGit(t, "case \"$1:$2\" in\nrev-parse:--show-toplevel) pwd ;;\nrev-parse:HEAD) printf '%040d\\n' 0 ;;\nlog:*) printf 'bad\\000record\\000' ;;\nesac\n")
		if _, err := Build(context.Background(), Options{Repository: t.TempDir(), GitExecutable: git}); err == nil ||
			!strings.Contains(err.Error(), "enumerate commits") {
			t.Fatalf("malformed log result = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Build(ctx, Options{Repository: root}); !errorsIsContext(err) {
			t.Fatalf("cancelled build = %v", err)
		}
	})
}

func TestHistoryExternalWorkTreeAffinity(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"one", map[string]string{"a.go": "package a\n\nfunc A() {}\n"}})
	gitDirectoryOutput, err := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	t.Setenv("GIT_DIR", strings.TrimSpace(string(gitDirectoryOutput)))
	t.Setenv("GIT_WORK_TREE", workTree)
	compiled, err := Build(context.Background(), Options{Repository: workTree})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.SourceRevision == "" || len(compiled.Commits) != 1 {
		t.Fatalf("external work-tree history = %+v", compiled)
	}
}

func TestHistoryRejectsUnpairedAffinityEnvironment(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"one", map[string]string{"a.go": "package a\n\nfunc A() {}\n"}})
	gitDirectoryOutput, err := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", strings.TrimSpace(string(gitDirectoryOutput)))
	t.Setenv("GIT_WORK_TREE", "")
	if _, err := Build(context.Background(), Options{Repository: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "unpaired Git affinity") {
		t.Fatalf("unpaired affinity result = %v", err)
	}
}

func errorsIsContext(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func TestHistoryDeterministicOrderingAndSignatureKey(t *testing.T) {
	root := gitFixture(t, fixtureCommit{"one", map[string]string{
		"a.go": "package a\n\nfunc B() {}\nfunc A() {}\n",
	}})
	first, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), Options{Repository: root})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("history output is not deterministic")
	}
	if signatureKey(rkcmodel.Node{Name: "Alpha", Signature: "func Alpha() string"}) !=
		signatureKey(rkcmodel.Node{Name: "Beta", Signature: "func Beta() string"}) {
		t.Fatal("renamed signatures do not share a key")
	}
	if signatureKey(rkcmodel.Node{Name: "Alpha", Signature: "func Alpha() string"}) ==
		signatureKey(rkcmodel.Node{Name: "Beta", Signature: "func Beta(x int) string"}) {
		t.Fatal("different signatures share a key")
	}
}
