package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestScanRedactsCanonicalMarkdownAndIsDeterministic(t *testing.T) {
	root := t.TempDir()
	secret := "super-secret-value-7f4b60b17f"
	contents := "# Service\n\nThe configured api_key = \"" + secret + "\" must never leave this repository.\n"
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), contents)
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\n\nfunc Login() bool { return true }\n")

	opts := Options{
		Root: root, ToolVersion: "test", DisablePythonAST: true,
		DisableTypeScript: true, DisableOpenAPI: true, DisableJSONSchema: true,
		DisableManifests: true, DisableEnvKeys: true,
	}
	first, coverage, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("canonical bundle leaked source secret: %s", encoded)
	}
	if !strings.Contains(string(encoded), "[REDACTED]") && !strings.Contains(string(encoded), "****") {
		t.Fatalf("bundle has no visible redaction marker: %s", encoded)
	}
	if first.Snapshot.ContentDigest == "" || coverage.SnapshotID != first.Snapshot.ID || coverage.ArtifactsInventoried != 2 {
		t.Fatalf("incomplete scan result: snapshot=%+v coverage=%+v", first.Snapshot, coverage)
	}
	second, secondCoverage, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot.ID != second.Snapshot.ID || first.Snapshot.ContentDigest != second.Snapshot.ContentDigest || coverage.DeterministicOutputDigest != secondCoverage.DeterministicOutputDigest {
		t.Fatalf("repeat scan was not deterministic: %s/%s vs %s/%s", first.Snapshot.ID, coverage.DeterministicOutputDigest, second.Snapshot.ID, secondCoverage.DeterministicOutputDigest)
	}
}

func TestCollectSensitiveLiteralsDetectsInventoryTOCTOU(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "settings.env")
	data := []byte("api_key=0123456789abcdef0123456789\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	file := pluginapi.FileRef{Path: "settings.env", SizeBytes: int64(len(data)), SHA256: strings.Repeat("0", 64)}
	if _, err := collectSensitiveLiterals(root, []pluginapi.FileRef{file}); err == nil || !strings.Contains(err.Error(), "source changed after inventory") {
		t.Fatalf("expected TOCTOU error, got %v", err)
	}
	file.SHA256 = stringHex(digest[:])
	values, err := collectSensitiveLiterals(root, []pluginapi.FileRef{file})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != "0123456789abcdef0123456789" {
		t.Fatalf("sensitive values = %#v", values)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := collectSensitiveLiterals(root, []pluginapi.FileRef{file}); err == nil || !strings.Contains(err.Error(), "read source") {
		t.Fatalf("expected reread failure, got %v", err)
	}
}

func TestReverifyInventoriedSourcesRejectsAdapterMutationAndReplacement(t *testing.T) {
	t.Run("content mutation", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "source.go")
		original := []byte("package one\n")
		mustWritePipelineFile(t, path, string(original))
		digest := sha256.Sum256(original)
		file := pluginapi.FileRef{ArtifactID: "a", Path: "source.go", Materialized: path, SizeBytes: int64(len(original)), SHA256: stringHex(digest[:])}
		_, identities, err := collectSensitiveLiteralsAndIdentity(root, []pluginapi.FileRef{file})
		if err != nil {
			t.Fatal(err)
		}
		mustWritePipelineFile(t, path, "package two\n")
		if err := reverifyInventoriedSources(root, []pluginapi.FileRef{file}, identities); err == nil || !strings.Contains(err.Error(), "source changed after adapters") {
			t.Fatalf("post-adapter mutation was not rejected: %v", err)
		}
	})

	t.Run("same-content identity replacement", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "source.go")
		original := []byte("package fixture\n")
		mustWritePipelineFile(t, path, string(original))
		digest := sha256.Sum256(original)
		file := pluginapi.FileRef{ArtifactID: "a", Path: "source.go", Materialized: path, SizeBytes: int64(len(original)), SHA256: stringHex(digest[:])}
		_, identities, err := collectSensitiveLiteralsAndIdentity(root, []pluginapi.FileRef{file})
		if err != nil {
			t.Fatal(err)
		}
		replacement := filepath.Join(root, "replacement.tmp")
		mustWritePipelineFile(t, replacement, string(original))
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		if err := reverifyInventoriedSources(root, []pluginapi.FileRef{file}, identities); err == nil || !strings.Contains(err.Error(), "identity replaced") {
			t.Fatalf("post-adapter identity replacement was not rejected: %v", err)
		}
	})
}

func TestScanErrorsAndPluginFailureAreBounded(t *testing.T) {
	t.Parallel()
	if _, _, err := Scan(context.Background(), Options{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("expected missing root failure")
	}
	file := filepath.Join(t.TempDir(), "not-directory")
	mustWritePipelineFile(t, file, "x")
	if _, _, err := Scan(context.Background(), Options{Root: file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected non-directory failure, got %v", err)
	}

	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "bad.py"), "def broken(:\n")
	bundle, _, err := Scan(context.Background(), Options{
		Root: root, PythonInterpreter: filepath.Join(root, "missing-python"), PythonPlugin: "missing.py",
		DisableGoAST: true, DisableTypeScript: true, DisableFrameworks: true, DisableSecretScan: true,
	})
	if err != nil {
		t.Fatalf("adapter failures should be recorded, not crash the scan: %v", err)
	}
	found := false
	for _, diagnostic := range bundle.Diagnostics {
		if diagnostic.Code == "RKC-PY-2001" && diagnostic.Severity == "error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing bounded adapter diagnostic: %+v", bundle.Diagnostics)
	}
}

func TestPipelineFragmentMergeDedupeAndHeuristicResolution(t *testing.T) {
	t.Parallel()
	artifactID := rkcmodel.StableID("artifact", "x.go")
	unresolvedID := rkcmodel.StableID("node", "unresolved", "Target")
	targetID := rkcmodel.StableID("node", "function", "Target")
	evidenceID := rkcmodel.StableID("evidence", "one")
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: artifactID, Path: "x.go", Kind: "file", Status: "recorded"}},
		Nodes: []rkcmodel.Node{
			artifactNode(rkcmodel.Artifact{ID: artifactID, Path: "x.go", Kind: "file", Status: "recorded"}),
			{ID: "caller", LogicalID: "caller", Kind: "function", Name: "Caller", Attributes: map[string]any{}},
			{ID: targetID, LogicalID: targetID, Kind: "function", Name: "Target", QualifiedName: "pkg.Target", EvidenceIDs: []string{evidenceID}},
			{ID: targetID, Kind: "function", Name: "Target", Signature: "func Target()", EvidenceIDs: []string{evidenceID, ""}},
			{ID: unresolvedID, LogicalID: unresolvedID, Kind: "unresolved_symbol", Name: "pkg.Target"},
			{ID: "orphan", Kind: "unresolved_symbol", Name: "Orphan"},
		},
		Edges: []rkcmodel.Edge{
			{Kind: "calls", From: "caller", To: unresolvedID, Resolution: "unresolved", Confidence: .2, Attributes: map[string]any{"spelling": "pkg.Target"}},
		},
		Evidence: []rkcmodel.Evidence{{ID: evidenceID}, {ID: evidenceID}},
	}
	mergeFragment(&bundle, rkcmodel.Fragment{
		Artifacts:   []rkcmodel.Artifact{{ID: artifactID, Path: "x.go", Kind: "file", Status: "semantic_parsed", SHA256: "abc"}},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "d1"}, {ID: "d1"}},
		Documents:   []rkcmodel.Document{{ID: "doc"}, {ID: "doc", Title: "last"}},
		Claims:      []rkcmodel.Claim{{ID: "claim"}, {ID: "claim", Text: "last"}},
		Conflicts:   []rkcmodel.Conflict{{ID: "conflict"}, {ID: "conflict", SubjectID: "last"}},
		Paths:       []rkcmodel.ExecutionPath{{ID: "path"}, {ID: "path", Name: "last"}},
	})
	dedupeBundle(&bundle)
	resolveHeuristicEdges(&bundle)
	dedupeBundle(&bundle)
	updateArtifactNodes(&bundle)
	if len(bundle.Artifacts) != 1 || bundle.Artifacts[0].Status != "semantic_parsed" || len(bundle.Evidence) != 1 || len(bundle.Diagnostics) != 1 || len(bundle.Documents) != 1 || bundle.Documents[0].Title != "last" {
		t.Fatalf("dedupe result unexpected: %+v", bundle)
	}
	if len(bundle.Edges) != 1 || bundle.Edges[0].To != targetID || bundle.Edges[0].Resolution != rkcmodel.ResolutionSyntaxInferred || bundle.Edges[0].Confidence < .65 {
		t.Fatalf("edge was not uniquely resolved: %+v", bundle.Edges)
	}
	for _, node := range bundle.Nodes {
		if node.ID == "orphan" {
			t.Fatal("unreferenced unresolved node survived")
		}
	}
}

func TestPipelineSmallHelpers(t *testing.T) {
	t.Parallel()
	files := []pluginapi.FileRef{{ArtifactID: "a", Language: "go"}, {ArtifactID: "b", Language: "python"}}
	got := filterFiles(files, func(file pluginapi.FileRef) bool { return file.Language == "go" })
	if len(got) != 1 || got[0].ArtifactID != "a" {
		t.Fatalf("filterFiles = %+v", got)
	}
	parsed := map[string]struct{}{}
	markParsed(parsed, files)
	if len(parsed) != 2 {
		t.Fatalf("markParsed = %v", parsed)
	}
	if got := uniqueSorted([]string{"b", "", "a", "b"}); strings.Join(got, ",") != "a,b" {
		t.Fatalf("uniqueSorted = %v", got)
	}
	if firstNonEmpty(" ", "x", "y") != "x" || firstNonEmpty("", " ") != "" || maxFloat(2, 1) != 2 || maxFloat(1, 2) != 2 {
		t.Fatal("small helper mismatch")
	}
	for status, want := range map[string]int{"semantic_parsed": 5, "syntax_parsed": 4, "text": 3, "recorded": 2, "binary": 1} {
		if got := artifactRank(status); got != want {
			t.Errorf("artifactRank(%q)=%d want %d", status, got, want)
		}
	}
	if resolutionRank(rkcmodel.ResolutionCompilerResolved) <= resolutionRank(rkcmodel.ResolutionUnresolved) {
		t.Fatal("resolution ranking is inverted")
	}
	diagnostic := adapterError("CODE", "plugin", errors.New("boom"))
	if diagnostic.ID == "" || diagnostic.Message != "boom" {
		t.Fatalf("adapter diagnostic = %+v", diagnostic)
	}
	bundle := rkcmodel.Bundle{}
	handleFragment(&bundle, rkcmodel.Fragment{}, errors.New("failure"), "CODE", "plugin")
	if len(bundle.Diagnostics) != 1 {
		t.Fatalf("handleFragment failure = %+v", bundle)
	}
}

func TestScanIsRaceSafeForConcurrentReaders(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\nfunc One() {}\n")
	opts := Options{Root: root, DisablePythonAST: true, DisableTypeScript: true, DisableFrameworks: true, DisableSecretScan: true}
	const workers = 8
	var wg sync.WaitGroup
	errorsSeen := make(chan error, workers)
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bundle, _, err := Scan(context.Background(), opts)
			errorsSeen <- err
			ids <- bundle.Snapshot.ID
		}()
	}
	wg.Wait()
	close(errorsSeen)
	close(ids)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		} else if id != want {
			t.Fatalf("concurrent scan ID = %q, want %q", id, want)
		}
	}
}

func TestInspectGitAvailableAndUnavailable(t *testing.T) {
	t.Parallel()
	missing := inspectGit(context.Background(), t.TempDir())
	if !missing.Unavailable {
		t.Fatalf("non-repository Git info = %+v", missing)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	command := exec.Command("git", "init", "--quiet", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	mustWritePipelineFile(t, filepath.Join(root, "tracked.txt"), "tracked\n")
	command = exec.Command("git", "-C", root, "add", "tracked.txt")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	command = exec.Command("git", "-C", root, "-c", "user.name=RKC", "-c", "user.email=rkc@example.invalid", "commit", "--quiet", "-m", "fixture")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	info := inspectGit(context.Background(), root)
	if info.Unavailable || info.Commit == "" || info.Dirty {
		t.Fatalf("committed Git info = %+v", info)
	}
	mustWritePipelineFile(t, filepath.Join(root, "untracked.txt"), "dirty\n")
	if dirty := inspectGit(context.Background(), root); !dirty.Dirty {
		t.Fatalf("dirty Git info = %+v", dirty)
	}
}

func mustWritePipelineFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stringHex(data []byte) string {
	const digits = "0123456789abcdef"
	output := make([]byte, len(data)*2)
	for index, value := range data {
		output[index*2] = digits[value>>4]
		output[index*2+1] = digits[value&0x0f]
	}
	return string(output)
}
