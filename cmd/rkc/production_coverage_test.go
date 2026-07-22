package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/snapshot"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// These tests deliberately exercise the command boundary. They assert the
// returned contract and filesystem effects instead of merely invoking parsers
// to inflate statement coverage.
func TestRunDispatchesEveryProductionCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"scan", []string{"scan", "--definitely-invalid"}, "flag provided but not defined"},
		{"serve", []string{"serve", "--definitely-invalid"}, "flag provided but not defined"},
		{"check", []string{"check", "--definitely-invalid"}, "flag provided but not defined"},
		{"init", []string{"init", "unexpected"}, "does not accept positional"},
		{"synthesize", []string{"synthesize", "--limit", "0"}, "limit must be"},
		{"components", []string{"components", "--definitely-invalid"}, "flag provided but not defined"},
		{"doctor", []string{"doctor", "--definitely-invalid"}, "flag provided but not defined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run(%v) = %v, want error containing %q", test.args, err, test.want)
			}
		})
	}
}

func TestPluginDispatchAndFailureContracts(t *testing.T) {
	for _, command := range []string{"list", "validate", "lock", "verify"} {
		err := runPlugins([]string{command, "--definitely-invalid"})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("plugins %s invalid flag = %v", command, err)
		}
	}

	root := t.TempDir()
	pluginDir := filepath.Join(root, "python-ast")
	for _, name := range []string{"plugin.json", "extractor.py"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "python-ast", name))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(pluginDir, name), string(data))
	}
	text, err := captureStdout(t, func() error { return runPlugins([]string{"list", "--root", root}) })
	if err != nil || !strings.Contains(text, "rkc.python-ast") || !strings.Contains(text, "native-worker") {
		t.Fatalf("text plugin inventory = %q, %v", text, err)
	}

	manifestPath := filepath.Join(pluginDir, "plugin.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["limits"].(map[string]any)["memory_mib"] = float64(5000)
	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, manifestPath, string(updated))
	err = runPlugins([]string{"validate", "--root", root})
	if err == nil || !strings.Contains(err.Error(), "memory exceeds policy") {
		t.Fatalf("policy-invalid plugin validation = %v", err)
	}

	writeTestFile(t, manifestPath, "{")
	if err := runPlugins([]string{"list", "--root", root}); err == nil || !strings.Contains(err.Error(), "decode plugin manifest") {
		t.Fatalf("malformed plugin discovery = %v", err)
	}

	empty := t.TempDir()
	parentFile := filepath.Join(empty, "parent")
	writeTestFile(t, parentFile, "not a directory")
	if err := runPlugins([]string{"lock", "--root", empty, "--out", filepath.Join(parentFile, "lock.json")}); err == nil {
		t.Fatal("plugin lock accepted a regular-file parent")
	}
	directoryOutput := filepath.Join(empty, "lock-directory")
	if err := os.Mkdir(directoryOutput, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runPlugins([]string{"lock", "--root", empty, "--out", directoryOutput}); err == nil {
		t.Fatal("plugin lock overwrote a directory")
	}
	if err := runPlugins([]string{"verify", "--root", empty, "--lock", filepath.Join(empty, "missing.lock")}); err == nil {
		t.Fatal("plugin verification accepted a missing lock")
	}

	// A text-mode verification failure must be an actionable command error;
	// JSON mode intentionally reports the same failure as data.
	validRoot := t.TempDir()
	validPlugin := filepath.Join(validRoot, "python-ast")
	for _, name := range []string{"plugin.json", "extractor.py"} {
		data, readErr := os.ReadFile(filepath.Join("..", "..", "plugins", "python-ast", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		writeTestFile(t, filepath.Join(validPlugin, name), string(data))
	}
	lock := filepath.Join(validRoot, "plugins.lock.json")
	if _, err := captureStdout(t, func() error { return runPlugins([]string{"lock", "--root", validRoot, "--out", lock}) }); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(validPlugin, "extractor.py"), "tampered\n")
	if err := runPlugins([]string{"verify", "--root", validRoot, "--lock", lock}); err == nil || !strings.Contains(err.Error(), "artifact digest") {
		t.Fatalf("text plugin verification failure = %v", err)
	}
}

func TestSnapshotDispatchAndFailureContracts(t *testing.T) {
	for _, command := range []string{"list", "show", "export", "recover", "set-current"} {
		err := runSnapshots([]string{command, "--definitely-invalid"})
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("snapshots %s invalid flag = %v", command, err)
		}
	}

	_, _, state := makeScannedFixture(t)
	store, err := snapshot.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CurrentID()
	if err != nil {
		t.Fatal(err)
	}
	if text, err := captureStdout(t, func() error {
		return runSnapshots([]string{"set-current", "--state-dir", state, id})
	}); err != nil || !strings.Contains(text, "Current snapshot: "+id) {
		t.Fatalf("text set-current = %q, %v", text, err)
	}
	if err := runSnapshotsSetCurrent([]string{"--state-dir", state, "missing"}); err == nil {
		t.Fatal("set-current accepted an absent snapshot")
	}
	if err := runSnapshotsShow([]string{"--state-dir", state, "missing"}); err == nil {
		t.Fatal("show accepted an absent snapshot")
	}
	if err := runSnapshotsExport([]string{"--state-dir", state, "one", "two"}); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("multi-ID snapshot export = %v", err)
	}
	if err := runSnapshotsExport([]string{"--state-dir", state, "missing"}); err == nil {
		t.Fatal("export accepted an absent snapshot")
	}

	fileState := filepath.Join(t.TempDir(), "state-file")
	writeTestFile(t, fileState, "not a snapshot store")
	for name, call := range map[string]func() error{
		"set-current": func() error { return runSnapshotsSetCurrent([]string{"--state-dir", fileState, "id"}) },
		"list":        func() error { return runSnapshotsList([]string{"--state-dir", fileState}) },
		"show":        func() error { return runSnapshotsShow([]string{"--state-dir", fileState}) },
		"export":      func() error { return runSnapshotsExport([]string{"--state-dir", fileState}) },
		"recover":     func() error { return runSnapshotsRecover([]string{"--state-dir", fileState}) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s accepted a regular file as snapshot store", name)
		}
	}

	// Corrupting CURRENT after a successful open exercises the command's
	// current-resolution failure without weakening Store.Open validation.
	currentless := t.TempDir()
	currentlessStore, err := snapshot.Open(currentless)
	if err != nil {
		t.Fatal(err)
	}
	if currentlessStore.Root() == "" {
		t.Fatal("snapshot store root is empty")
	}
	if err := runSnapshotsShow([]string{"--state-dir", currentless, "--current"}); err == nil {
		t.Fatal("show --current succeeded without CURRENT")
	}
	if err := runSnapshotsExport([]string{"--state-dir", currentless}); err == nil {
		t.Fatal("default export succeeded without CURRENT")
	}
}

func TestCheckReportsEveryQualityAndDocumentFailure(t *testing.T) {
	root := t.TempDir()
	coveragePath := filepath.Join(root, "coverage.json")
	coverage := rkcmodel.Coverage{
		SnapshotID: "coverage-snapshot", TextArtifacts: 1, SymbolsTotal: 1, PublicSymbols: 1,
		EdgesTotal: 1, ClaimsTotal: 1, UnresolvedEdges: 2, HighConfidenceSecretFindings: 2,
		DiagnosticsBySeverity: map[string]int{"error": 2, "fatal": 1},
	}
	coverageData, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, coveragePath, string(coverageData))
	err = runCheck([]string{
		"--coverage", coveragePath, "--verify-bundle=false", "--verify-files=false", "--require-secret-redaction=false",
		"--min-inventory-accounting", "1", "--min-syntax-parse", "1", "--min-semantic-parse", "1",
		"--min-symbol-evidence", "1", "--min-public-documentation", "1", "--min-edge-resolution", "1",
		"--min-claim-citation", "1", "--max-errors", "0", "--max-fatal", "0", "--max-unresolved", "0",
		"--max-high-confidence-secrets", "0", "--require-digest=true",
	})
	if err == nil {
		t.Fatal("quality gate accepted an all-zero coverage report")
	}
	for _, want := range []string{
		"inventory accounting", "syntax parse", "semantic parse", "symbol evidence", "public documentation",
		"edge resolution", "claim citation", "errors 2", "fatal diagnostics 1", "unresolved edges 2",
		"high-confidence secret findings 2", "deterministic output digest is missing",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("quality failure omitted %q: %v", want, err)
		}
	}

	coverage.InventoryAccountingRatio = 1
	coverage.DiagnosticsBySeverity = map[string]int{}
	coverage.UnresolvedEdges = 0
	coverage.HighConfidenceSecretFindings = 0
	coverage.DeterministicOutputDigest = "present"
	coverage.TextArtifacts, coverage.SymbolsTotal, coverage.PublicSymbols, coverage.EdgesTotal, coverage.ClaimsTotal = 0, 0, 0, 0, 0
	coverageData, _ = json.Marshal(coverage)
	writeTestFile(t, coveragePath, string(coverageData))
	text, err := captureStdout(t, func() error {
		return runCheck([]string{"--coverage", coveragePath, "--verify-bundle=false", "--verify-files=false", "--require-secret-redaction=false"})
	})
	if err != nil || !strings.Contains(text, "Quality gate passed for coverage-snapshot") {
		t.Fatalf("text quality success = %q, %v", text, err)
	}

	writeTestFile(t, coveragePath, "{")
	if err := runCheck([]string{"--coverage", coveragePath}); err == nil || !strings.Contains(err.Error(), "decode coverage") {
		t.Fatalf("malformed coverage = %v", err)
	}
	if err := runCheck([]string{"--coverage", filepath.Join(root, "missing.json")}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing coverage = %v", err)
	}
	if err := runCheck([]string{"--config", filepath.Join(root, "missing-config.json")}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing check configuration = %v", err)
	}
	configA := filepath.Join(root, "a.json")
	configB := filepath.Join(root, "b.json")
	defaultData, _ := json.Marshal(defaultConfiguration())
	writeTestFile(t, configA, string(defaultData))
	writeTestFile(t, configB, string(defaultData))
	if err := runCheck([]string{"--config", configA, "--config", configB}); err == nil || !strings.Contains(err.Error(), "only once") {
		t.Fatalf("duplicate check configuration = %v", err)
	}

	writeTestFile(t, coveragePath, string(coverageData))
	writeTestFile(t, filepath.Join(root, "bundle.json"), "{")
	err = runCheck([]string{"--coverage", coveragePath, "--verify-files=false", "--require-secret-redaction=false"})
	if err == nil || !strings.Contains(err.Error(), "decode bundle") {
		t.Fatalf("malformed bundle check = %v", err)
	}
	writeTestFile(t, filepath.Join(root, "rkc-export-manifest.json"), "{")
	err = runCheck([]string{"--coverage", coveragePath, "--verify-bundle=false", "--require-secret-redaction=false"})
	if err == nil || !strings.Contains(err.Error(), "decode export manifest") {
		t.Fatalf("malformed export manifest check = %v", err)
	}

	if failures := verifySecretRedactionPolicy(filepath.Join(root, "missing-policy")); len(failures) != 1 || !strings.Contains(failures[0], "read export policy") {
		t.Fatalf("missing export policy failures = %v", failures)
	}
	writeTestFile(t, filepath.Join(root, "rkc.export-policy.json"), "{")
	if failures := verifySecretRedactionPolicy(root); len(failures) != 1 || !strings.Contains(failures[0], "decode export policy") {
		t.Fatalf("malformed export policy failures = %v", failures)
	}
	if failures := verifyExportFiles(root, filepath.Join(root, "missing-manifest.json")); len(failures) != 1 || !strings.Contains(failures[0], "read export manifest") {
		t.Fatalf("missing export manifest failures = %v", failures)
	}
}

func TestGraphDoctorScanAndSynthesisValidationFailures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for name, call := range map[string]func() error{
		"path-load":       func() error { return runPath([]string{"--dir", missing, "--from", "a", "--to", "b"}) },
		"impact-load":     func() error { return runImpact([]string{"--dir", missing, "--node", "a"}) },
		"components-load": func() error { return runComponents([]string{"--dir", missing}) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}

	_, output, _ := makeScannedFixture(t)
	for name, call := range map[string]func() error{
		"path-source": func() error { return runPath([]string{"--dir", output, "--from", "missing", "--to", "Beta"}) },
		"path-target": func() error { return runPath([]string{"--dir", output, "--from", "Alpha", "--to", "missing"}) },
		"path-options": func() error {
			return runPath([]string{"--dir", output, "--from", "Alpha", "--to", "Beta", "--direction", "sideways"})
		},
		"impact-seed":    func() error { return runImpact([]string{"--dir", output, "--node", "missing"}) },
		"impact-options": func() error { return runImpact([]string{"--dir", output, "--node", "Alpha", "--depth", "-1"}) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}
	if text, err := captureStdout(t, func() error {
		return runComponents([]string{"--dir", output, "--limit", "0", "--json"})
	}); err != nil || !strings.Contains(text, `"truncated": true`) {
		t.Fatalf("bounded components = %q, %v", text, err)
	}

	badConfig := filepath.Join(t.TempDir(), "bad-config.json")
	writeTestFile(t, badConfig, `{"unknown":true}`)
	if err := runDoctor([]string{"--config", badConfig, "--repository", output, "--json"}); err == nil || !strings.Contains(err.Error(), "fatal problem") {
		t.Fatalf("doctor invalid configuration = %v", err)
	}
	fileRepository := filepath.Join(t.TempDir(), "repository-file")
	writeTestFile(t, fileRepository, "file")
	if err := runDoctor([]string{"--repository", fileRepository, "--json"}); err == nil || !strings.Contains(err.Error(), "fatal problem") {
		t.Fatalf("doctor regular-file repository = %v", err)
	}
	text, err := captureStdout(t, func() error { return runDoctor([]string{"--repository", output}) })
	if err != nil || !strings.Contains(text, "Result: fatal=0") || !strings.Contains(text, "go-runtime") {
		t.Fatalf("doctor text report = %q, %v", text, err)
	}
	failingExecutable := filepath.Join(t.TempDir(), "failing-tool")
	writeExecutable(t, failingExecutable, "#!/bin/sh\nprintf 'refused'\nexit 7\n")
	check := executableCheck("failing", failingExecutable, "--version", true)
	if check.Status != "fail" || !check.Fatal || !strings.Contains(check.Detail, "refused") {
		t.Fatalf("failing executable check = %+v", check)
	}

	validationCases := []struct {
		name string
		args []string
		want string
	}{
		{"flags", []string{"--definitely-invalid"}, "flag provided but not defined"},
		{"context-low", []string{"--context", "511"}, "context must be"},
		{"output-over-context", []string{"--context", "512", "--max-output", "513"}, "max-output"},
		{"rss-low", []string{"--max-rss-mib", "255"}, "max-rss-mib"},
		{"threads-high", []string{"--threads", "65"}, "threads must be"},
		{"batch-low", []string{"--batch-size", "0"}, "batch-size"},
		{"dataset", []string{"--dir", missing}, "no such file"},
		{"subject", []string{"--dir", output, "--node", "missing", "--packet-only"}, "not found"},
		{"no-subjects", []string{"--dir", output, "--query", "definitely-no-such-symbol", "--packet-only"}, "no eligible"},
		{"model-required", []string{"--dir", output, "--query", "Alpha", "--provider", "llama.cpp"}, "--model is required"},
		{"inference-contract", []string{"--dir", output, "--query", "Alpha", "--provider", "llama.cpp", "--model", "missing.gguf", "--allow-inference"}, "incompatible"},
		{"model-lock", []string{"--dir", output, "--query", "Alpha", "--provider", "llama.cpp", "--model", "missing.gguf", "--model-lock", "missing.lock"}, "read model lock"},
		{"implicit-llama", []string{"--dir", output, "--query", "Alpha", "--model", "missing.gguf", "--model-lock", "missing.lock"}, "read model lock"},
	}
	for _, test := range validationCases {
		t.Run("synthesize-"+test.name, func(t *testing.T) {
			err := runSynthesize(test.args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("runSynthesize(%v) = %v, want %q", test.args, err, test.want)
			}
		})
	}
	temperatureConfig := defaultConfiguration()
	temperatureConfig.Model.Provider = "llama.cpp"
	temperatureConfig.Model.ModelPath = "missing.gguf"
	temperatureConfig.Model.Temperature = 0.1
	temperatureData, err := json.Marshal(temperatureConfig)
	if err != nil {
		t.Fatal(err)
	}
	temperaturePath := filepath.Join(t.TempDir(), "temperature.json")
	writeTestFile(t, temperaturePath, string(temperatureData))
	if err := runSynthesize([]string{"--config", temperaturePath, "--dir", output, "--query", "Alpha"}); err == nil || !strings.Contains(err.Error(), "temperature must be 0") {
		t.Fatalf("nondeterministic synthesis temperature = %v", err)
	}
	if _, err := resolveSynthesisOutput("", "", "", "profile"); err == nil || !strings.Contains(err.Error(), "dataset root is empty") {
		t.Fatalf("empty dataset synthesis output = %v", err)
	}
	if _, err := resolveSynthesisOutput("", missing, "", "profile"); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("missing dataset synthesis output = %v", err)
	}
}

func TestSynthesizeRunsQualifiedHermeticLlamaCLI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the production resource envelope is Linux-specific")
	}
	repository, output, _ := makeScannedFixture(t)
	model := newCLIModelFixture(t)
	derived := filepath.Join(t.TempDir(), "qualified-synthesis")
	stdout, err := captureStdout(t, func() error {
		return runSynthesize([]string{
			"--dir", output, "--repo-root", repository, "--out", derived,
			"--provider", "llama.cpp", "--model", model.modelPath,
			"--llama-cli", model.executablePath, "--model-lock", model.lockPath,
			"--runtime-receipt", model.receiptPath, "--query", "Alpha", "--limit", "1",
			"--context", "512", "--max-output", "64", "--max-rss-mib", "512",
			"--threads", "1", "--batch-size", "32", "--timeout", "30s", "--json",
		})
	})
	if errors.Is(err, resourceguard.ErrHigherPriorityActive) || errors.Is(err, resourceguard.ErrLowPriorityEnvelope) {
		t.Skipf("production model guard correctly refused this test environment: %v", err)
	}
	if err != nil {
		t.Fatalf("qualified llama.cpp synthesis: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, `"status": "completed"`) || !strings.Contains(stdout, `"accepted_claims": 1`) {
		t.Fatalf("qualified synthesis summary = %s", stdout)
	}
	manifestData, err := os.ReadFile(filepath.Join(derived, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest synthesisManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.PacketOnly || manifest.Provider != "llamacpp-cli" || manifest.AcceptedClaims != 1 || manifest.ResponsesReceived != 1 || manifest.ModelBinding == nil {
		t.Fatalf("qualified synthesis manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(derived, "summaries")); err != nil {
		t.Fatalf("accepted claim was not rendered: %v", err)
	}
}

func TestSynthesizeAuditsRejectedFailedAndPartialModelRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the production resource envelope is Linux-specific")
	}
	repository, output, _ := makeScannedFixture(t)

	t.Run("rejected response is retained", func(t *testing.T) {
		model := newCLIModelFixtureWithScript(t, `#!/bin/sh
set -eu
printf '%s\n' '{"claims":[{"text":"Unsupported output.","category":"purpose","certainty":"supported","evidence_ids":["outside-packet"]}],"unresolved_questions":[]}'
`)
		derived := filepath.Join(t.TempDir(), "rejected")
		_, err := captureStdout(t, func() error {
			return runSynthesize(qualifiedSynthesisArgs(output, repository, derived, model, "--node", "Alpha"))
		})
		skipGuardRefusal(t, err)
		if err == nil || !strings.Contains(err.Error(), "no publishable output") {
			t.Fatalf("rejected response error = %v", err)
		}
		manifest := readSynthesisManifest(t, derived)
		if manifest.Status != "rejected" || manifest.ResponsesReceived != 1 || manifest.RejectedClaims != 1 || manifest.AcceptedClaims != 0 {
			t.Fatalf("rejected response manifest = %+v", manifest)
		}
		data, readErr := os.ReadFile(filepath.Join(derived, "diagnostics.jsonl"))
		if readErr != nil || !strings.Contains(string(data), "RKC-MDL-001") {
			t.Fatalf("rejection diagnostics = %q, %v", data, readErr)
		}
	})

	t.Run("generation failure is retained", func(t *testing.T) {
		model := newCLIModelFixtureWithScript(t, "#!/bin/sh\nprintf 'controlled model failure' >&2\nexit 7\n")
		derived := filepath.Join(t.TempDir(), "failed")
		_, err := captureStdout(t, func() error {
			return runSynthesize(qualifiedSynthesisArgs(output, repository, derived, model, "--node", "Alpha"))
		})
		skipGuardRefusal(t, err)
		if err == nil || !strings.Contains(err.Error(), "no publishable output") {
			t.Fatalf("retained generation failure = %v", err)
		}
		manifest := readSynthesisManifest(t, derived)
		if manifest.Status != "rejected" || manifest.FailedSubjects != 1 || manifest.ResponsesReceived != 0 {
			t.Fatalf("failed response manifest = %+v", manifest)
		}
		data, readErr := os.ReadFile(filepath.Join(derived, "records.jsonl"))
		if readErr != nil || !strings.Contains(string(data), "controlled model failure") {
			t.Fatalf("failed response audit = %q, %v", data, readErr)
		}
	})

	t.Run("fail-fast never publishes staging", func(t *testing.T) {
		model := newCLIModelFixtureWithScript(t, "#!/bin/sh\nprintf 'fail-fast model failure' >&2\nexit 8\n")
		derived := filepath.Join(t.TempDir(), "fail-fast")
		_, err := captureStdout(t, func() error {
			return runSynthesize(qualifiedSynthesisArgs(output, repository, derived, model, "--node", "Alpha", "--continue-on-error=false"))
		})
		skipGuardRefusal(t, err)
		if err == nil || !strings.Contains(err.Error(), "fail-fast model failure") {
			t.Fatalf("fail-fast generation error = %v", err)
		}
		if _, statErr := os.Lstat(derived); !os.IsNotExist(statErr) {
			t.Fatalf("fail-fast synthesis published output: %v", statErr)
		}
	})

	t.Run("mixed outcomes publish partial audit", func(t *testing.T) {
		model := newCLIModelFixtureWithScript(t, `#!/bin/sh
set -eu
self=$0
resolved=$(readlink "$0" 2>/dev/null || true)
[ -z "$resolved" ] || self=$resolved
state="$self.invoked"
if [ -e "$state" ]; then
  printf 'second subject failed' >&2
  exit 9
fi
: > "$state"
prompt=
previous=
for argument in "$@"; do
  if [ "$previous" = "-f" ]; then prompt=$argument; fi
  previous=$argument
done
evidence=$(sed -n 's/.*"evidence":\[{"id":"\([^"]*\)".*/\1/p' "$prompt")
[ -n "$evidence" ] || exit 92
printf '{"claims":[{"text":"Symbol exists.","category":"purpose","certainty":"supported","evidence_ids":["%s"]}],"unresolved_questions":[]}\n' "$evidence"
`)
		derived := filepath.Join(t.TempDir(), "partial")
		stdout, err := captureStdout(t, func() error {
			return runSynthesize(qualifiedSynthesisArgs(output, repository, derived, model, "--node", "Alpha", "--node", "Beta", "--limit", "2"))
		})
		skipGuardRefusal(t, err)
		if err != nil || !strings.Contains(stdout, `"status": "partial"`) {
			t.Fatalf("partial synthesis = %q, %v", stdout, err)
		}
		manifest := readSynthesisManifest(t, derived)
		if manifest.Status != "partial" || manifest.AcceptedClaims != 1 || manifest.FailedSubjects != 1 || manifest.PacketsWritten != 2 {
			t.Fatalf("partial response manifest = %+v", manifest)
		}
	})
}

type cliModelFixture struct {
	modelPath      string
	executablePath string
	lockPath       string
	receiptPath    string
}

func newCLIModelFixture(t *testing.T) cliModelFixture {
	t.Helper()
	return newCLIModelFixtureWithScript(t, `#!/bin/sh
set -eu
prompt=
previous=
for argument in "$@"; do
  if [ "$previous" = "-f" ]; then prompt=$argument; fi
  previous=$argument
done
[ -n "$prompt" ] || exit 91
evidence=$(sed -n 's/.*"evidence":\[{"id":"\([^"]*\)".*/\1/p' "$prompt")
[ -n "$evidence" ] || exit 92
printf '{"claims":[{"text":"`+"`Alpha`"+` exists.","category":"purpose","certainty":"supported","evidence_ids":["%s"]}],"unresolved_questions":[]}\n' "$evidence"
`)
}

func newCLIModelFixtureWithScript(t *testing.T, script string) cliModelFixture {
	t.Helper()
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	bin := filepath.Join(runtimeRoot, "build", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(root, "fixture.gguf")
	writeTestFile(t, modelPath, "GGUFtest")
	executablePath := filepath.Join(bin, "llama-cli")
	writeExecutable(t, executablePath, script)
	modelSHA := digestFile(t, modelPath)
	executableSHA := digestFile(t, executablePath)
	modelRevision := strings.Repeat("a", 40)
	runtimeRevision := strings.Repeat("c", 40)
	sourceSHA := strings.Repeat("b", 64)
	defaultGeneration := "generation"
	lock := map[string]any{
		"$schema": "../schemas/model-lock.schema.json", "schema_version": "1.0.0",
		"default_generation_model": defaultGeneration, "default_embedding_model": nil,
		"llama_cpp": map[string]any{
			"repository": "https://github.com/ggml-org/llama.cpp", "tag": "b1", "commit": runtimeRevision,
			"license_spdx": "MIT", "license_url": "https://github.com/ggml-org/llama.cpp/blob/commit/LICENSE",
			"source_asset_id": "source", "cmake": map[string]any{},
		},
		"assets": []map[string]any{
			{"id": "source", "kind": "source-archive", "status": "runtime-pinned", "default_eligible": false,
				"repository": "https://github.com/ggml-org/llama.cpp", "revision": runtimeRevision, "filename": "source.tar.gz",
				"url": "https://example.com/source.tar.gz", "allowed_hosts": []string{"example.com"}, "sha256": sourceSHA,
				"size_bytes": 123, "license_spdx": "MIT", "license_url": "https://example.com/license",
				"redistribution": "not-bundled-download-on-demand", "quantization": nil, "native_context_tokens": nil,
				"qualification_spec": nil, "extraction_root": "source"},
			{"id": "generation", "kind": "generation-model", "status": "qualified", "default_eligible": true,
				"repository": "https://example.com/model", "revision": modelRevision, "filename": "fixture.gguf",
				"url": "https://example.com/fixture.gguf", "allowed_hosts": []string{"example.com"}, "sha256": modelSHA,
				"size_bytes": 8, "license_spdx": "Apache-2.0", "license_url": "https://example.com/license",
				"redistribution": "not-bundled-download-on-demand", "quantization": "Q4_K_M", "native_context_tokens": 32768,
				"qualification_spec": "models/qualification/fixture.json", "extraction_root": nil},
			{"id": "embedding", "kind": "embedding-model", "status": "unqualified", "default_eligible": false,
				"repository": "https://example.com/embedding", "revision": strings.Repeat("d", 40), "filename": "embedding.gguf",
				"url": "https://example.com/embedding.gguf", "allowed_hosts": []string{"example.com"}, "sha256": strings.Repeat("e", 64),
				"size_bytes": 8, "license_spdx": "Apache-2.0", "license_url": "https://example.com/license",
				"redistribution": "not-bundled-download-on-demand", "quantization": "Q8_0", "native_context_tokens": 8192,
				"qualification_spec": "models/qualification/fixture.json", "extraction_root": nil},
		},
	}
	lockData, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "models.lock.json")
	writeTestFile(t, lockPath, string(lockData))
	lockDigest := sha256.Sum256(lockData)
	receipt := map[string]any{
		"schema_version": "1.0.0", "runtime": "llama.cpp", "tag": "b1", "commit": runtimeRevision,
		"source_sha256": sourceSHA, "source_size_bytes": 123, "lock_sha256": hex.EncodeToString(lockDigest[:]),
		"profile": "native", "cmake": "cmake version 3.30", "configure_argv": []string{"cmake", "-S", "source"},
		"build_argv": []string{"cmake", "--build", "build"}, "platform": "test", "machine": "test", "python": "3",
		"binaries": []map[string]any{
			{"path": "build/bin/llama-bench", "sha256": strings.Repeat("1", 64), "size_bytes": 1},
			{"path": "build/bin/llama-cli", "sha256": executableSHA, "size_bytes": fileSize(t, executablePath)},
			{"path": "build/bin/llama-embedding", "sha256": strings.Repeat("2", 64), "size_bytes": 1},
			{"path": "build/bin/llama-server", "sha256": strings.Repeat("3", 64), "size_bytes": 1},
		},
		"qualification_status": "not-run", "default_model_status": "none",
	}
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(runtimeRoot, "rkc-llama-runtime.json")
	writeTestFile(t, receiptPath, string(receiptData))
	return cliModelFixture{modelPath: modelPath, executablePath: executablePath, lockPath: lockPath, receiptPath: receiptPath}
}

func qualifiedSynthesisArgs(output, repository, derived string, model cliModelFixture, selection ...string) []string {
	args := []string{
		"--dir", output, "--repo-root", repository, "--out", derived,
		"--provider", "llama.cpp", "--model", model.modelPath,
		"--llama-cli", model.executablePath, "--model-lock", model.lockPath,
		"--runtime-receipt", model.receiptPath, "--context", "512", "--max-output", "64",
		"--max-rss-mib", "512", "--threads", "1", "--batch-size", "32", "--timeout", "30s", "--json",
	}
	return append(args, selection...)
}

func skipGuardRefusal(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, resourceguard.ErrHigherPriorityActive) || errors.Is(err, resourceguard.ErrLowPriorityEnvelope) {
		t.Skipf("production model guard correctly refused this test environment: %v", err)
	}
}

func readSynthesisManifest(t *testing.T, root string) synthesisManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest synthesisManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
