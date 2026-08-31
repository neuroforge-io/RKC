package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
)

type failingSCIPReader struct{}

func (failingSCIPReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type failingSCIPWriter struct{}

func (failingSCIPWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type shortSCIPWriter struct{}

func (shortSCIPWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}

func encodeScipVarint(value uint64) []byte {
	var output []byte
	for {
		byteValue := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			byteValue |= 0x80
		}
		output = append(output, byteValue)
		if value == 0 {
			return output
		}
	}
}

func scipFieldBytes(field int, value []byte) []byte {
	if value == nil {
		return nil
	}
	output := encodeScipVarint(uint64(field<<3 | 2))
	output = append(output, encodeScipVarint(uint64(len(value)))...)
	return append(output, value...)
}

func scipFieldString(field int, value string) []byte {
	return scipFieldBytes(field, []byte(value))
}

func scipFieldVarint(field int, value uint64) []byte {
	output := encodeScipVarint(uint64(field << 3))
	return append(output, encodeScipVarint(value)...)
}

func scipMessage(fields ...[]byte) []byte {
	var output []byte
	for _, field := range fields {
		output = append(output, field...)
	}
	return output
}

func scipLegacyRange(field int, values ...uint32) []byte {
	var packed []byte
	for _, value := range values {
		packed = append(packed, encodeScipVarint(uint64(value))...)
	}
	return scipFieldBytes(field, packed)
}

// minimalValidIndex builds a tiny structurally valid metadata-only SCIP index.
// Source-bearing generation is exercised with validSourceIndex below.
func minimalValidIndex() []byte {
	return scipMessage(scipFieldBytes(1, validSCIPMetadata()))
}

func validSCIPMetadata() []byte {
	metadata := scipMessage(
		scipFieldBytes(2, scipMessage(
			scipFieldString(1, "test-indexer"),
			scipFieldString(2, "1.0"),
		)),
		scipFieldString(3, "file:///repo"),
		scipFieldVarint(4, 1),
	)
	return metadata
}

func validSourceIndex(source string, embedText bool) []byte {
	occurrence := scipMessage(
		scipLegacyRange(1, 0, 0, 0, 1),
		scipFieldString(2, "scip . . . main/"),
		scipFieldVarint(3, 1),
	)
	document := scipMessage(
		scipFieldString(1, "main.go"),
		scipFieldBytes(2, occurrence),
		scipFieldString(4, "Go"),
	)
	if embedText {
		document = append(document, scipFieldString(5, source)...)
	}
	document = append(document, scipFieldVarint(6, 1)...)
	return scipMessage(scipFieldBytes(1, validSCIPMetadata()), scipFieldBytes(2, document))
}

func writeFakeIndexer(t *testing.T, directory string, script string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-indexer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScipLanguageSpecs(t *testing.T) {
	for name, want := range map[string]string{
		"go": "scip-go", "python": "scip-python", "typescript": "scip-typescript",
		"js": "scip-typescript", "c++": "scip-clang", "rust": "rust-analyzer",
		"java": "scip-java", "kotlin": "scip-java", "scala": "scip-java",
		"csharp": "scip-dotnet", "ruby": "scip-ruby", "  GO ": "scip-go",
	} {
		spec, ok := scipLanguageSpec(name)
		if !ok || spec.tool != want {
			t.Errorf("scipLanguageSpec(%q) = %+v, %v; want tool %q", name, spec, ok, want)
		}
	}
	if _, ok := scipLanguageSpec("brainfuck"); ok {
		t.Fatal("unsupported language resolved")
	}
}

func TestScipPinAndLoadRoundTrip(t *testing.T) {
	directory := t.TempDir()
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--version", "v9.9", "--lock", lockPath}); err != nil {
		t.Fatal(err)
	}
	entry, pinned, err := loadScipIndexerEntry(lockPath, "go")
	if err != nil || !pinned {
		t.Fatalf("loadScipIndexerEntry = %+v, %v, %v", entry, pinned, err)
	}
	digest, err := sha256File(tool)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SHA256 != digest || entry.Tool != "fake-indexer" || entry.Version != "v9.9" {
		t.Fatalf("pinned entry = %+v", entry)
	}
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(lockBytes), tool) || strings.Contains(string(lockBytes), "pinned_at") || strings.Contains(string(lockBytes), `"path"`) {
		t.Fatalf("lock contains host-specific metadata: %s", lockBytes)
	}
	// Re-pinning the same language replaces the entry.
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--version", "v10", "--lock", lockPath}); err != nil {
		t.Fatal(err)
	}
	entry, _, err = loadScipIndexerEntry(lockPath, "go")
	if err != nil || entry.Version != "v10" {
		t.Fatalf("re-pinned entry = %+v, %v", entry, err)
	}
	if _, _, err := loadScipIndexerEntry(lockPath, "python"); err != nil || pinnedForLanguage(lockPath, "python") {
		t.Fatal("unrelated language was pinned")
	}
}

func pinnedForLanguage(lockPath, language string) bool {
	entry, pinned, _ := loadScipIndexerEntry(lockPath, language)
	return pinned || entry.Language == language
}

func TestScipPinRejectsUnsafeTools(t *testing.T) {
	directory := t.TempDir()
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := runScipPin([]string{"--language", "go", "--tool", filepath.Join(directory, "missing"), "--lock", lockPath}); err == nil {
		t.Fatal("missing tool was pinned")
	}
	subdirectory := filepath.Join(directory, "dir")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", subdirectory, "--lock", lockPath}); err == nil {
		t.Fatal("directory was pinned")
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "linked")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", symlink, "--lock", lockPath}); err == nil {
		t.Fatal("symlink was pinned")
	}
	if err := runScipPin([]string{"--language", "unknown", "--tool", target, "--lock", lockPath}); err == nil {
		t.Fatal("unknown language was pinned")
	}
	if err := runScipPin([]string{"--language", "go", "--tool", target, "--lock", "relative-lock.json"}); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative operator lock was not rejected: %v", err)
	}
}

func TestScipGenerateRejectsRepositoryControlledLock(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	inside := filepath.Join(repository, "indexers.lock.json")
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", inside, "--no-pin-check", repository,
	})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("repository-controlled lock was not rejected: %v", err)
	}
	err = runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", "indexers.lock.json", "--no-pin-check", repository,
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative generation lock was not rejected: %v", err)
	}
}

func TestStageScipExecutableUsesVerifiedPrivateCopy(t *testing.T) {
	directory := t.TempDir()
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	digest, err := sha256File(tool)
	if err != nil {
		t.Fatal(err)
	}
	staged, stagedDigest, cleanup, err := stageScipExecutable(context.Background(), tool, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if stagedDigest != digest || filepath.Dir(staged) == filepath.Dir(tool) {
		t.Fatalf("staged executable = %q %q", staged, stagedDigest)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(staged)
	if err := command.Run(); err != nil {
		t.Fatalf("private copy changed with original: %v", err)
	}
	if _, _, cleanupMismatch, err := stageScipExecutable(context.Background(), tool, digest); err == nil {
		cleanupMismatch()
		t.Fatal("mismatched executable retained pinned authority")
	}
	link := filepath.Join(directory, "tool-link")
	if err := os.Symlink(tool, link); err != nil {
		t.Fatal(err)
	}
	if _, _, cleanupLink, err := stageScipExecutable(context.Background(), link, ""); err == nil {
		cleanupLink()
		t.Fatal("symlink executable was staged")
	}
}

func TestScipGenerateRejectsInvalidInputs(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	ctx := context.Background()
	for name, args := range map[string][]string{
		"unknown language":     {"--language", "brainfuck", "--tool", tool, repository},
		"missing repository":   {"--language", "go", "--tool", tool},
		"two repositories":     {"--language", "go", "--tool", tool, repository, repository},
		"empty output name":    {"--language", "go", "--tool", tool, "--output", "", repository},
		"absolute output name": {"--language", "go", "--tool", tool, "--output", "/tmp/x.scip", repository},
		"escaping output name": {"--language", "go", "--tool", tool, "--output", "../x.scip", repository},
		"nested output escape": {"--language", "go", "--tool", tool, "--output", "safe/../../x.scip", repository},
		"negative timeout":     {"--language", "go", "--tool", tool, "--timeout", "-1s", repository},
		"missing tool":         {"--language", "go", "--tool", filepath.Join(directory, "absent"), repository},
	} {
		if err := runScipGenerate(ctx, args); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}
	t.Setenv("PATH", directory)
	if err := runScipGenerate(ctx, []string{"--language", "go", repository}); err == nil {
		t.Fatal("missing canonical tool on PATH unexpectedly succeeded")
	}
}

func TestScipGeneratePublishesValidatedIndex(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexBytes := validSourceIndex("package main\n", false)
	indexHex := hex.EncodeToString(indexBytes)
	tool := writeFakeIndexer(t, directory, "printf '"+indexHex+"' | xxd -r -p > index.scip\n")
	out := filepath.Join(directory, "out")
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
	})
	if err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(out, "go.scip")
	data, err := os.ReadFile(published)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(data, indexBytes) || !bytes.Contains(data, []byte("package main\n")) {
		t.Fatal("published index was not source-sealed after the compiler run")
	}
	manifestData, err := os.ReadFile(filepath.Join(out, scipManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion string               `json:"schema_version"`
		Indexes       []scipGeneratedIndex `json:"indexes"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != scipindex.ManifestSchemaVersion || len(manifest.Indexes) != 1 ||
		manifest.Indexes[0].Language != "go" || manifest.Indexes[0].Documents != 1 ||
		manifest.Indexes[0].Symbols != 0 || manifest.Indexes[0].Occurrences != 1 ||
		manifest.Indexes[0].SourceBinding == nil ||
		manifest.Indexes[0].SourceBinding.DocumentCount != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Indexes[0].SHA256 != indexDigest(data) || manifest.Indexes[0].SizeBytes != int64(len(data)) {
		t.Fatalf("manifest does not bind the sealed index: %+v", manifest.Indexes[0])
	}
	// The generated index is a normal --scip-index input for a scan.
	inputs, _, err := scipindex.PrepareInputs(context.Background(), []string{published})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("prepared generated index = %v, %v", inputs, err)
	}
	if inputs[0].SourceBinding == nil || inputs[0].SourceBinding.SourceSHA256 == "" {
		t.Fatalf("generated source binding was not auto-loaded: %+v", inputs[0])
	}
}

func TestScipGenerateRequiresPinOrExplicitBypass(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, repository,
	})
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned indexer = %v", err)
	}
}

func indexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestScipGenerateFailsClosedOnInvalidToolOutput(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "printf 'garbage' > index.scip\n")
	out := filepath.Join(directory, "out")
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
	})
	if err == nil || !strings.Contains(err.Error(), "strict validation") {
		t.Fatalf("invalid tool output = %v; want strict validation failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "go.scip")); !os.IsNotExist(statErr) {
		t.Fatal("invalid tool output was published")
	}
}

func TestScipGenerateEnforcesPinnedDigest(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexHex := hex.EncodeToString(minimalValidIndex())
	lockPath := filepath.Join(directory, "indexers.lock.json")
	tool := writeFakeIndexer(t, directory, "printf '"+indexHex+"' | xxd -r -p > index.scip\n")
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", lockPath}); err != nil {
		t.Fatal(err)
	}
	// A tampered tool no longer matches the pin and must refuse.
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", lockPath, "--out", filepath.Join(directory, "out"), repository,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned") {
		t.Fatalf("tampered tool = %v; want pinned-digest refusal", err)
	}
	// Explicit --no-pin-check runs the tampered tool anyway.
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", lockPath,
		"--no-pin-check", "--out", filepath.Join(directory, "out2"), repository,
	}); err == nil {
		t.Fatal("tampered tool with --no-pin-check unexpectedly produced an index")
	}
}

func TestScipGenerateIntegrationSeam(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexHex := hex.EncodeToString(minimalValidIndex())
	tool := writeFakeIndexer(t, directory, "printf '"+indexHex+"' | xxd -r -p > index.scip\n")
	out := filepath.Join(directory, "atlas")
	paths, err := generateSCIPIndexes(context.Background(), []string{"go"}, repository, out, tool, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], filepath.Join("atlas.rkc-derived", "scip", "go.scip")) {
		t.Fatalf("generated paths = %v", paths)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal(err)
	}
	if paths, err := generateSCIPIndexes(context.Background(), nil, repository, out, tool, "", true); err != nil || len(paths) != 0 {
		t.Fatalf("empty language set = %v, %v", paths, err)
	}
	if _, err := generateSCIPIndexes(context.Background(), []string{"brainfuck"}, repository, out, tool, "", true); err == nil {
		t.Fatal("unsupported generation language succeeded")
	}
}

func TestScipAdmissionFlagGrammar(t *testing.T) {
	help, err := validateDirectCommandAdmission("scan", []string{
		"--no-python", "--scip-generate", "go", "--scip-tool", "/x", "--scip-lock", "/y",
		"--out", "/tmp/out", "--force", ".",
	})
	if err != nil || help {
		t.Fatalf("scan admission with scip flags = %v, %v", help, err)
	}
	help, err = validateDirectCommandAdmission("quickstart", []string{
		"--scip-generate", "go", "--scip-index", "/i.scip",
		"--trace", "/trace.json", "--history", "/history.json", ".",
	})
	if err != nil || help {
		t.Fatalf("quickstart admission with scip flags = %v, %v", help, err)
	}
	if _, err := validateDirectCommandAdmission("scan", []string{"--scip-tool"}); err == nil {
		t.Fatal("missing scip-tool value was admitted")
	}
}

func TestScipVerifyReportsSummary(t *testing.T) {
	directory := t.TempDir()
	indexPath := filepath.Join(directory, "index.scip")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipVerify([]string{"--index", indexPath, "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := runScipVerify([]string{"--index", filepath.Join(directory, "missing.scip")}); err == nil {
		t.Fatal("missing index verified")
	}
	if err := runScipVerify([]string{}); err == nil {
		t.Fatal("verify without --index succeeded")
	}
}

func TestScipIndexerEnvironment(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	t.Setenv("ROCR_VISIBLE_DEVICES", "1")
	environment := scipIndexerEnvironment()
	joined := strings.Join(environment, "\n")
	for _, required := range []string{"GOMAXPROCS=1", "OMP_NUM_THREADS=1", "CUDA_VISIBLE_DEVICES=-1", "ROCR_VISIBLE_DEVICES=-1"} {
		if !strings.Contains(joined, required) {
			t.Errorf("indexer environment is missing %q", required)
		}
	}
	values := map[string]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	for _, name := range []string{"GOMAXPROCS", "OMP_NUM_THREADS", "MKL_NUM_THREADS"} {
		if values[name] != "1" {
			t.Errorf("indexer environment %s = %q; want 1", name, values[name])
		}
	}
	if values["CUDA_VISIBLE_DEVICES"] != "-1" {
		t.Errorf("ambient CUDA_VISIBLE_DEVICES was not neutralized: %q", values["CUDA_VISIBLE_DEVICES"])
	}
}

func TestScipLockReadErrorAndPinSchema(t *testing.T) {
	directory := t.TempDir()
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := os.WriteFile(lockPath, []byte(`{}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadScipIndexerEntry(lockPath, "go"); err == nil {
		t.Fatal("unreadable lock was accepted")
	}
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	if err := os.Chmod(lockPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(`{"schema_version":"9.9","indexers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", lockPath}); err == nil {
		t.Fatal("pin with an unsupported lock schema succeeded")
	}
}

func TestScipVerifyRejectsInvalidIndex(t *testing.T) {
	directory := t.TempDir()
	garbage := filepath.Join(directory, "garbage.scip")
	if err := os.WriteFile(garbage, []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipVerify([]string{"--index", garbage}); err == nil {
		t.Fatal("garbage index verified")
	}
}

func TestScipLockSchemaEnforcement(t *testing.T) {
	directory := t.TempDir()
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := os.WriteFile(lockPath, []byte(`{"schema_version":"9.9","indexers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadScipIndexerEntry(lockPath, "go"); err == nil ||
		!strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unsupported lock schema = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadScipIndexerEntry(lockPath, "go"); err == nil {
		t.Fatal("malformed lock was accepted")
	}
}

func TestScipDispatch(t *testing.T) {
	if err := runScip(nil); err != nil {
		t.Fatalf("empty dispatch = %v", err)
	}
	if err := runScip([]string{"help"}); err != nil {
		t.Fatalf("help dispatch = %v", err)
	}
	if err := runScip([]string{"--help"}); err != nil {
		t.Fatalf("--help dispatch = %v", err)
	}
	if err := runScip([]string{"languages"}); err != nil {
		t.Fatalf("languages dispatch = %v", err)
	}
	if err := runScip([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown scip subcommand") {
		t.Fatalf("unknown dispatch = %v", err)
	}
	// Help requests on every subcommand resolve locally without admission.
	for _, subcommand := range []string{"generate", "verify", "pin", "languages"} {
		err := runScip([]string{subcommand, "--help"})
		if err != nil && !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s --help dispatch = %v", subcommand, err)
		}
	}
}

func TestScipVerifyAndLanguagesPlainOutput(t *testing.T) {
	directory := t.TempDir()
	indexPath := filepath.Join(directory, "index.scip")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipVerify([]string{"--index", indexPath}); err != nil {
		t.Fatal(err)
	}
	if err := runScipLanguages([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if err := runScipLanguages([]string{"unexpected"}); err == nil {
		t.Fatal("languages positional argument was accepted")
	}
	if err := runScipPin([]string{"--json", "--language", "go", "--tool", filepath.Join(directory, "absent")}); err == nil {
		t.Fatal("pin --json with a missing tool succeeded")
	}
	if err := runScipPin([]string{"unexpected"}); err == nil {
		t.Fatal("pin positional argument was accepted")
	}
	if err := runScipPin([]string{"--language", "go"}); err == nil {
		t.Fatal("pin without --tool succeeded")
	}
	if err := runScipPin([]string{"--language", "brainfuck", "--tool", filepath.Join(directory, "absent")}); err == nil {
		t.Fatal("pin with an unknown language succeeded")
	}
}

func TestScipRepositoryRootValidation(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveScipRepositoryRoot(file); err == nil {
		t.Fatal("a regular file resolved as a repository")
	}
	target := filepath.Join(directory, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveScipRepositoryRoot(link); err == nil {
		t.Fatal("a symlink resolved as a repository")
	}
	if _, err := resolveScipRepositoryRoot(""); err == nil {
		t.Fatal("an empty path resolved as a repository")
	}
	if _, err := resolveScipRepositoryRoot(filepath.Join(directory, "absent")); err == nil {
		t.Fatal("an absent path resolved as a repository")
	}
	if got := scipLanguageCanonical("unknown-language"); got != "unknown-language" {
		t.Fatalf("scipLanguageCanonical(unknown) = %q", got)
	}
}

func TestScipGenerateIntegrationSeamEmptyOutput(t *testing.T) {
	if _, err := generateSCIPIndexes(context.Background(), []string{"go"}, ".", "", "", "", false); err == nil ||
		!strings.Contains(err.Error(), "requires an output atlas path") {
		t.Fatalf("empty output atlas = %v", err)
	}
}

func TestScipGenerateFlagAndRepositoryValidation(t *testing.T) {
	directory := t.TempDir()
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	ctx := context.Background()
	if err := runScipGenerate(ctx, []string{"--language", "go", "--tool", tool, "--undefined-flag", directory}); err == nil {
		t.Fatal("undefined flag was accepted")
	}
	if err := runScipGenerate(ctx, []string{"--language", "go", "--tool", tool, filepath.Join(directory, "absent")}); err == nil {
		t.Fatal("absent repository was accepted")
	}
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	malformedLock := filepath.Join(directory, "malformed-lock.json")
	if err := os.WriteFile(malformedLock, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(ctx, []string{"--language", "go", "--tool", tool, "--lock", malformedLock, repository}); err == nil {
		t.Fatal("malformed lock was accepted")
	}
}

func TestScipGenerateJsonSummary(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexHex := hex.EncodeToString(minimalValidIndex())
	tool := writeFakeIndexer(t, directory, "printf '"+indexHex+"' | xxd -r -p > index.scip\n")
	out := filepath.Join(directory, "out")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, "--json", repository,
	}); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(filepath.Join(out, scipManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestData), `"language": "go"`) {
		t.Fatalf("manifest = %s", manifestData)
	}
}

func TestScipGenerateRequiresPortableToolResolution(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", lockPath}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(directory, "empty"))
	if err := os.MkdirAll(filepath.Join(directory, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(directory, "out")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--lock", lockPath, "--out", out, repository,
	}); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("host-local pinned path was reused: %v", err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", lockPath, "--out", out, repository,
	}); err != nil {
		t.Fatalf("explicit pinned tool failed: %v", err)
	}
	// Removing the explicit tool entirely must fail with a resolution error.
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--lock", lockPath, "--out", filepath.Join(directory, "out2"), repository,
	}); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("missing pinned tool = %v; want resolution error", err)
	}
}

func TestScipManifestCorruptAndReplace(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexHex := hex.EncodeToString(minimalValidIndex())
	tool := writeFakeIndexer(t, directory, "printf '"+indexHex+"' | xxd -r -p > index.scip\n")
	out := filepath.Join(directory, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	// A corrupt manifest fails the publish, not silently overwrites.
	if err := os.WriteFile(filepath.Join(out, scipManifestName), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
	}); err == nil {
		t.Fatal("corrupt manifest was accepted")
	}
	// Replacing the corrupt manifest with a healthy one lets the same
	// language regenerate, then a second language appends its own entry.
	if err := os.Remove(filepath.Join(out, scipManifestName)); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool = writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "python", "--tool", tool, "--no-pin-check", "--out", out, repository,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, scipManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"language": "go"`) != 1 || strings.Count(string(data), `"language": "python"`) != 1 {
		t.Fatalf("manifest merge = %s", data)
	}
}

func TestScipGenerateDefaultOutputDirectoryAndToolArgs(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(directory, "argv.txt")
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\nprintf '%s\\n' \"$@\" > '"+argvPath+"'\n")
	// No --out: the index publishes to <repository>/.rkc-scip.
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", repository,
	}); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(repository, ".rkc-scip", "go.scip")
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("default output directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository, ".rkc-scip", scipManifestName)); err != nil {
		t.Fatal(err)
	}
	// --tool-args replaces the canonical argument vector exactly.
	argvFile := filepath.Join(directory, "argv2.txt")
	tool = writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\nprintf '%s\\n' \"$@\" > '"+argvFile+"'\n")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check",
		"--tool-args", "index", "--tool-args", ".", "--tool-args", "--project-name", "--tool-args", "custom",
		"--out", filepath.Join(directory, "out2"), repository,
	}); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(argv)
	for _, expected := range []string{"index", ".", "--project-name", "custom"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("tool argument vector %q is missing %q", joined, expected)
		}
	}
	if strings.Contains(joined, "./...") {
		t.Fatalf("tool argument vector retained the canonical default: %q", joined)
	}
}

func TestScipGenerateToolOnPATH(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	// Rename the fake to the canonical tool name and put it on PATH.
	if err := os.Rename(tool, filepath.Join(directory, "scip-go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--no-pin-check", "--out", filepath.Join(directory, "out"), repository,
	}); err != nil {
		t.Fatalf("PATH tool resolution failed: %v", err)
	}
}

func TestScipGenerateMissingAndUnreadableOutputs(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	// The tool exits successfully without producing an index.
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", filepath.Join(directory, "out"), repository,
	}); err == nil || !strings.Contains(err.Error(), "prepared indexer output") {
		t.Fatalf("missing indexer output = %v", err)
	}
	// The tool output exists but cannot be copied.
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool = writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip && chmod 000 index.scip\n")
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", filepath.Join(directory, "out2"), repository,
	}); err == nil {
		t.Fatal("unreadable indexer output was published")
	}
	_ = os.Chmod
}

func TestScipGenerateUnreadablePinnedTool(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	lockPath := filepath.Join(directory, "indexers.lock.json")
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", lockPath}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tool, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--lock", lockPath, "--out", filepath.Join(directory, "out"), repository,
	}); err == nil {
		t.Fatal("unreadable pinned tool was accepted")
	}
}

func TestScipManifestReadError(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	out := filepath.Join(directory, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, scipManifestName), []byte(`{}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
	}); err == nil {
		t.Fatal("unreadable manifest was accepted")
	}
}

func TestScipGenerateTimeout(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "sleep 5\n")
	started := time.Now()
	err := runScipGenerate(context.Background(), []string{
		"--language", "go", "--tool", tool, "--no-pin-check", "--timeout", "1s", "--out", filepath.Join(directory, "out"), repository,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow indexer = %v; want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestScipGenerateAdmissionFailsClosed(t *testing.T) {
	// The guarded-child marker forces the admission path; the test process is
	// not inside the resource envelope, so admission fails closed before any
	// generation starts.
	t.Setenv("RKC_GUARDED_DIRECT_CHILD", "scip")
	if err := runScipGenerateWithAdmission([]string{"--language", "go", "--tool", "/nonexistent-tool"}); err == nil {
		t.Fatal("admission with a guarded-child marker unexpectedly succeeded")
	}
}

func TestScipGeneratePublishFailures(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// --out is a regular file: MkdirAll fails closed.
	outFile := filepath.Join(directory, "outfile")
	if err := os.WriteFile(outFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	if err := runScipGenerate(ctx, []string{"--language", "go", "--tool", tool, "--no-pin-check", "--out", outFile, repository}); err == nil {
		t.Fatal("--out as a file was accepted")
	}
	// The output directory exists but is not writable: the copy fails closed.
	outDir := filepath.Join(directory, "outdir")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := runScipGenerate(ctx, []string{"--language", "go", "--tool", tool, "--no-pin-check", "--out", outDir, repository}); err == nil {
		t.Fatal("unwritable output directory was accepted")
	}
	_ = os.Chmod(outDir, 0o700)
}

func TestScipManifestReplacement(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(directory, "index.bin")
	if err := os.WriteFile(indexPath, minimalValidIndex(), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := writeFakeIndexer(t, directory, "/bin/cp '"+indexPath+"' index.scip\n")
	out := filepath.Join(directory, "out")
	for attempt := 0; attempt < 2; attempt++ {
		if err := runScipGenerate(context.Background(), []string{
			"--language", "go", "--tool", tool, "--no-pin-check", "--out", out, repository,
		}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(out, scipManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"language": "go"`) != 1 {
		t.Fatalf("manifest replacement = %s", data)
	}
}

func TestScipPinErrorPaths(t *testing.T) {
	directory := t.TempDir()
	tool := writeFakeIndexer(t, directory, "exit 0\n")
	lockDir := filepath.Join(directory, "lockdir")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(lockDir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", malformed}); err == nil {
		t.Fatal("pin with a malformed lock succeeded")
	}
	badSchema := filepath.Join(lockDir, "badschema.json")
	if err := os.WriteFile(badSchema, []byte(`{"schema_version":"9.9","indexers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", badSchema}); err == nil {
		t.Fatal("pin with an unsupported lock schema succeeded")
	}
	unknownField := filepath.Join(lockDir, "unknown.json")
	if err := os.WriteFile(unknownField, []byte(`{"schema_version":"1.0","indexers":[],"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", unknownField}); err == nil {
		t.Fatal("pin with an unknown lock field succeeded")
	}
	readOnly := filepath.Join(lockDir, "readonly")
	if err := os.MkdirAll(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := runScipPin([]string{"--language", "go", "--tool", tool, "--lock", filepath.Join(readOnly, "lock.json")}); err == nil {
		t.Fatal("pin into an unwritable directory succeeded")
	}
}

func TestValidateSCIPIndexerLockRejectsUntrustedMetadata(t *testing.T) {
	validDigest := strings.Repeat("a", sha256.Size*2)
	valid := scipIndexerEntry{Language: "go", Tool: "scip-go", Version: "1.0", SHA256: validDigest}
	tests := []struct {
		name string
		lock scipIndexerLock
	}{
		{
			name: "too many indexers",
			lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: func() []scipIndexerEntry {
				entries := make([]scipIndexerEntry, len(scipLanguageSpecs)+1)
				for index := range entries {
					entries[index] = valid
				}
				return entries
			}()},
		},
		{name: "alias is not canonical", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{{Language: "ts", Tool: "scip-typescript", SHA256: validDigest}}}},
		{name: "duplicate language", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{valid, valid}}},
		{name: "unsafe tool", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{{Language: "go", Tool: "../scip-go", SHA256: validDigest}}}},
		{name: "unsafe version", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{{Language: "go", Tool: "scip-go", Version: "1\n2", SHA256: validDigest}}}},
		{name: "short digest", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{{Language: "go", Tool: "scip-go", SHA256: "abc"}}}},
		{name: "non hexadecimal digest", lock: scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{{Language: "go", Tool: "scip-go", SHA256: strings.Repeat("z", sha256.Size*2)}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSCIPIndexerLock(test.lock); err == nil {
				t.Fatal("unsafe lock metadata was accepted")
			}
		})
	}
	if err := validateSCIPIndexerLock(scipIndexerLock{SchemaVersion: scipIndexerLockSchemaVersion, Indexers: []scipIndexerEntry{valid}}); err != nil {
		t.Fatalf("valid lock metadata = %v", err)
	}
}

func TestCopySCIPExecutableFailsClosed(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copySCIPExecutable(cancelled, &bytes.Buffer{}, bytes.NewReader([]byte("tool")), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy = %v", err)
	}
	if _, err := copySCIPExecutable(context.Background(), failingSCIPWriter{}, bytes.NewReader([]byte("tool")), 4); err == nil || err.Error() != "write failed" {
		t.Fatalf("writer failure = %v", err)
	}
	if _, err := copySCIPExecutable(context.Background(), shortSCIPWriter{}, bytes.NewReader([]byte("tool")), 4); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	if _, err := copySCIPExecutable(context.Background(), &bytes.Buffer{}, failingSCIPReader{}, 4); err == nil || err.Error() != "read failed" {
		t.Fatalf("reader failure = %v", err)
	}
	var destination bytes.Buffer
	written, err := copySCIPExecutable(context.Background(), &destination, bytes.NewReader([]byte("tool")), 4)
	if err != nil || written != 4 || destination.String() != "tool" {
		t.Fatalf("healthy copy = %d, %q, %v", written, destination.String(), err)
	}
}

func TestStageSCIPExecutableRejectsUnsafeSources(t *testing.T) {
	if _, _, _, err := stageScipExecutable(nil, "/missing", ""); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, _, _, err := stageScipExecutable(context.Background(), "/missing", ""); err == nil {
		t.Fatal("missing indexer was accepted")
	}
	directory := t.TempDir()
	tool := filepath.Join(directory, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "tool-link")
	if err := os.Symlink(tool, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := stageScipExecutable(context.Background(), link, ""); err == nil {
		t.Fatal("symlink indexer was accepted")
	}
	if _, _, _, err := stageScipExecutable(context.Background(), directory, ""); err == nil {
		t.Fatal("directory indexer was accepted")
	}
	sparse := filepath.Join(directory, "oversized")
	if err := os.WriteFile(sparse, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparse, maximumSCIPIndexerExecutableBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := stageScipExecutable(context.Background(), sparse, ""); err == nil {
		t.Fatal("oversized indexer was accepted")
	}
	if _, _, _, err := stageScipExecutable(context.Background(), tool, strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("mismatched digest was accepted")
	}
}

func TestSCIPMetadataReadersFailClosed(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "metadata.json")
	if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readBoundedRegularFile(regular, 2); err != nil || string(data) != "{}" {
		t.Fatalf("bounded regular read = %q, %v", data, err)
	}
	if _, err := readBoundedRegularFile(regular, 1); err == nil {
		t.Fatal("oversized metadata was accepted")
	}
	link := filepath.Join(directory, "metadata-link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedRegularFile(link, 2); err == nil {
		t.Fatal("symlink metadata was accepted")
	}
	if _, err := readBoundedRegularFile(directory, 4096); err == nil {
		t.Fatal("directory metadata was accepted")
	}
	var decoded map[string]any
	if err := decodeStrictJSON([]byte("{} {}"), &decoded); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing JSON = %v", err)
	}
}
