package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/snapshot"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestRunDispatchAndUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		output, err := captureStdout(t, func() error { return run(args) })
		if err != nil || !strings.Contains(output, "Repository Knowledge Compiler") {
			t.Fatalf("run(%v): output=%q err=%v", args, output, err)
		}
	}
	for _, arg := range []string{"version", "--version", "-version"} {
		output, err := captureStdout(t, func() error { return run([]string{arg}) })
		if err != nil || strings.TrimSpace(output) != version {
			t.Fatalf("run(%q): output=%q err=%v", arg, output, err)
		}
	}
	if err := run([]string{"definitely-unknown"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
	for _, args := range [][]string{
		{"query"}, {"search"}, {"answer"}, {"ask"}, {"path"}, {"impact"},
		{"snapshots"}, {"plugins"}, {"diff"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%v) unexpectedly succeeded", args)
		}
	}
}

func TestInitCommandStdoutCreateAndForce(t *testing.T) {
	output, err := captureStdout(t, func() error { return runInit([]string{"--stdout"}) })
	if err != nil {
		t.Fatal(err)
	}
	var cfg Configuration
	if err := json.Unmarshal([]byte(output), &cfg); err != nil {
		t.Fatalf("decode stdout configuration: %v\n%s", err, output)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stdout configuration invalid: %v", err)
	}

	path := filepath.Join(t.TempDir(), "nested", "rkc.json")
	output, err = captureStdout(t, func() error { return runInit([]string{"--path", path}) })
	if err != nil || !strings.Contains(output, "Wrote ") {
		t.Fatalf("create: output=%q err=%v", output, err)
	}
	if _, err := loadConfiguration(path); err != nil {
		t.Fatalf("written config invalid: %v", err)
	}
	writeTestFile(t, path, "sentinel")
	if err := runInit([]string{"--path", path}); err == nil || !strings.Contains(err.Error(), "use --force") {
		t.Fatalf("existing config error = %v", err)
	}
	if _, err := captureStdout(t, func() error { return runInit([]string{"--path", path, "--force"}) }); err != nil {
		t.Fatalf("forced create: %v", err)
	}
	if _, err := loadConfiguration(path); err != nil {
		t.Fatalf("forced config invalid: %v", err)
	}
	if err := runInit([]string{"unexpected"}); err == nil {
		t.Fatal("init accepted a positional argument")
	}
}

func TestDoctorCommandReportsPassAndFatalFailure(t *testing.T) {
	repository := t.TempDir()
	output, err := captureStdout(t, func() error {
		return runDoctor([]string{"--repository", repository, "--json"})
	})
	if err != nil {
		t.Fatalf("doctor valid repository: %v\n%s", err, output)
	}
	var result struct {
		OK            bool          `json:"ok"`
		FatalFailures int           `json:"fatal_failures"`
		Checks        []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.FatalFailures != 0 || len(result.Checks) < 5 {
		t.Fatalf("unexpected doctor result: %+v", result)
	}

	missing := filepath.Join(repository, "missing")
	output, err = captureStdout(t, func() error {
		return runDoctor([]string{"--repository", missing, "--json"})
	})
	if err == nil || !strings.Contains(err.Error(), "fatal problem") {
		t.Fatalf("missing repository doctor error = %v", err)
	}
	if json.Unmarshal([]byte(output), &result) != nil || result.OK || result.FatalFailures == 0 {
		t.Fatalf("unexpected failed doctor result: %s", output)
	}
	check := executableCheck("missing", "rkc-command-that-cannot-exist", "--version", false)
	if check.Status != "fail" || check.Fatal {
		t.Fatalf("missing executable check = %+v", check)
	}
}

func TestScanSafePublicationAndInvalidFlags(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	writeTestFile(t, filepath.Join(repository, "readme.txt"), "hello\n")
	if err := runScan([]string{"one", "two"}); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two repositories error = %v", err)
	}

	configOne := filepath.Join(root, "one.json")
	configTwo := filepath.Join(root, "two.json")
	for _, path := range []string{configOne, configTwo} {
		data, _ := json.Marshal(defaultConfiguration())
		writeTestFile(t, path, string(data))
	}
	if err := runScan([]string{"--config", configOne, "--config", configTwo, repository}); err == nil || !strings.Contains(err.Error(), "only once") {
		t.Fatalf("duplicate config error = %v", err)
	}
	if err := runScan([]string{"--out", repository, "--no-plugins", "--no-frameworks", repository}); err == nil {
		t.Fatal("scan accepted repository root as output")
	}

	unowned := filepath.Join(root, "unowned")
	writeTestFile(t, filepath.Join(unowned, "sentinel"), "keep")
	for _, force := range []bool{false, true} {
		args := []string{"--out", unowned, "--no-plugins", "--no-frameworks", repository}
		if force {
			args = append([]string{"--force"}, args...)
		}
		if err := runScan(args); err == nil {
			t.Fatalf("scan replaced unowned output with force=%t", force)
		}
		data, err := os.ReadFile(filepath.Join(unowned, "sentinel"))
		if err != nil || string(data) != "keep" {
			t.Fatalf("unowned sentinel changed with force=%t: data=%q err=%v", force, data, err)
		}
	}

	output := filepath.Join(root, "atlas")
	state := filepath.Join(root, "state")
	jsonSummary, err := captureStdout(t, func() error {
		return runScan([]string{"--out", output, "--state-dir", state, "--no-plugins", "--no-frameworks", "--json", repository})
	})
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, jsonSummary)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(jsonSummary), &summary); err != nil || summary["snapshot_id"] == "" {
		t.Fatalf("invalid scan summary: %v %s", err, jsonSummary)
	}
	marker, err := os.ReadFile(filepath.Join(output, ".rkc-generated.json"))
	if err != nil || !strings.Contains(string(marker), `"kind": "atlas"`) {
		t.Fatalf("output marker missing: %q err=%v", marker, err)
	}
	if _, err := captureStdout(t, func() error {
		return runScan([]string{"--out", output, "--state-dir", filepath.Join(root, "state-2"), "--no-plugins", "--no-frameworks", "--force", repository})
	}); err != nil {
		t.Fatalf("replace owned output: %v", err)
	}
}

func TestScanRejectsOutputSnapshotOverlapWithoutMutation(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	writeTestFile(t, filepath.Join(repository, "source.txt"), "source")
	for _, test := range []struct {
		name  string
		out   string
		state string
	}{
		{name: "state-inside-output", out: filepath.Join(root, "shared"), state: filepath.Join(root, "shared", "state")},
		{name: "output-inside-state", out: filepath.Join(root, "shared", "atlas"), state: filepath.Join(root, "shared")},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeTestFile(t, filepath.Join(test.out, "out-sentinel"), "output")
			writeTestFile(t, filepath.Join(test.state, "state-sentinel"), "state")
			err := runScan([]string{"--out", test.out, "--state-dir", test.state, "--force", "--no-plugins", "--no-frameworks", repository})
			if !errors.Is(err, safeoutput.ErrUnsafeTarget) {
				t.Fatalf("overlapping scan error = %v", err)
			}
			for path, expected := range map[string]string{
				filepath.Join(test.out, "out-sentinel"):     "output",
				filepath.Join(test.state, "state-sentinel"): "state",
			} {
				data, readErr := os.ReadFile(path)
				if readErr != nil || string(data) != expected {
					t.Fatalf("overlap rejection mutated %s: data=%q err=%v", path, data, readErr)
				}
			}
		})
	}
}

func TestScanCancellationAndMaxFilePolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(t.TempDir(), "cancelled")
	if err := runScanContext(ctx, []string{"--out", target}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan error = %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("cancelled scan published output: %v", err)
	}

	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	writeTestFile(t, filepath.Join(repository, "large.txt"), "larger-than-limit")
	output := filepath.Join(root, "atlas")
	if err := runScan([]string{"--out", output, "--max-file-bytes", "4", "--max-text-bytes", "4", "--no-plugins", "--no-frameworks", repository}); err != nil {
		t.Fatalf("bounded scan: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle rkcmodel.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Artifacts) != 1 || bundle.Artifacts[0].Status != "oversized" || bundle.Artifacts[0].SHA256 != "" {
		t.Fatalf("oversized artifact was read or hashed: %+v", bundle.Artifacts)
	}
	if got := bundle.Snapshot.Policy["max_file_bytes"]; got != float64(4) {
		t.Fatalf("snapshot max_file_bytes policy = %#v", got)
	}
	cfg := defaultConfiguration()
	cfg.Inventory.MaxFileBytes = 4
	cfg.Inventory.MaxTextBytes = 4
	cfg.Plugins.Enabled = false
	cfg.Frameworks.Enabled = false
	if bundle.Snapshot.ConfigDigest != cfg.Digest() || bundle.Snapshot.PolicyDigest != cfg.PolicyDigest() {
		t.Fatalf("snapshot digests do not bind effective file policy: config=%s/%s policy=%s/%s", bundle.Snapshot.ConfigDigest, cfg.Digest(), bundle.Snapshot.PolicyDigest, cfg.PolicyDigest())
	}
}

func TestQueryAndGraphCommands(t *testing.T) {
	_, output, _ := makeScannedFixture(t)
	for _, args := range [][]string{
		{"--dir", output, "--json", "Alpha"},
		{"--dir", output, "--kinds", "function", "--languages", "go", "Alpha"},
	} {
		text, err := captureStdout(t, func() error { return runQuery(args) })
		if err != nil || !strings.Contains(text, "Alpha") {
			t.Fatalf("query %v: output=%q err=%v", args, text, err)
		}
	}
	if err := runQuery([]string{"--dir", output}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("empty query error = %v", err)
	}
	for _, command := range []struct {
		name string
		call func() error
	}{
		{"path", func() error {
			return runPath([]string{"--dir", output, "--from", "Alpha", "--to", "Beta", "--direction", "both", "--json"})
		}},
		{"impact", func() error {
			return runImpact([]string{"--dir", output, "--node", "Alpha", "--direction", "both", "--json"})
		}},
		{"components", func() error { return runComponents([]string{"--dir", output, "--json"}) }},
	} {
		text, err := captureStdout(t, command.call)
		if err != nil || !json.Valid([]byte(text)) {
			t.Fatalf("%s: output=%q err=%v", command.name, text, err)
		}
	}
	if err := runPath([]string{"--dir", output, "--from", "Alpha"}); err == nil {
		t.Fatal("path accepted missing --to")
	}
	if err := runImpact([]string{"--dir", output}); err == nil {
		t.Fatal("impact accepted missing --node")
	}
}

func TestSynthesizeValidationAndOwnedOutput(t *testing.T) {
	repository, output, _ := makeScannedFixture(t)
	for _, args := range [][]string{
		{"--limit", "0"},
		{"--task", "invent_everything"},
	} {
		if err := runSynthesize(args); err == nil {
			t.Fatalf("synthesize accepted invalid args %v", args)
		}
	}
	if err := runSynthesize([]string{"--dir", output, "--query", "Alpha", "--limit", "1"}); err == nil || !strings.Contains(err.Error(), "provider is disabled") {
		t.Fatalf("disabled provider error = %v", err)
	}
	if err := runSynthesize([]string{"--dir", output, "--query", "Alpha", "--provider", "unknown", "--limit", "1"}); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("unknown provider error = %v", err)
	}
	derived := filepath.Join(filepath.Dir(output), "synthesis")
	text, err := captureStdout(t, func() error {
		return runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", derived, "--packet-only", "--query", "Alpha", "--limit", "1", "--json"})
	})
	if err != nil || !strings.Contains(text, `"packet_only": true`) {
		t.Fatalf("packet synthesis: output=%q err=%v", text, err)
	}
	if _, err := os.Stat(filepath.Join(derived, ".rkc-generated.json")); err != nil {
		t.Fatalf("synthesis ownership marker: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", derived, "--packet-only", "--query", "Alpha", "--limit", "1", "--force"})
	}); err != nil {
		t.Fatalf("replace owned synthesis: %v", err)
	}
	if err := runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", repository, "--packet-only", "--query", "Alpha", "--limit", "1", "--force"}); err == nil {
		t.Fatal("synthesis accepted repository root as output")
	}
	if err := runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", filepath.Join(output, "derived", "synthesis"), "--packet-only", "--query", "Alpha", "--limit", "1", "--force"}); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("synthesis accepted output inside verified dataset: %v", err)
	}
}

func TestPluginCommandsLifecycleAndTamperDetection(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "python-ast")
	for _, name := range []string{"plugin.json", "extractor.py"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "python-ast", name))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(pluginDir, name), string(data))
	}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"list", func() error { return runPluginsList([]string{"--root", root, "--json"}) }},
		{"validate", func() error { return runPluginsValidate([]string{"--root", root, "--json"}) }},
	} {
		output, err := captureStdout(t, test.call)
		if err != nil || !json.Valid([]byte(output)) {
			t.Fatalf("plugins %s: output=%q err=%v", test.name, output, err)
		}
	}
	lock := filepath.Join(root, "plugins.lock.json")
	if _, err := captureStdout(t, func() error { return runPluginsLock([]string{"--root", root, "--out", lock}) }); err != nil {
		t.Fatalf("plugins lock: %v", err)
	}
	output, err := captureStdout(t, func() error { return runPluginsVerify([]string{"--root", root, "--lock", lock, "--json"}) })
	if err != nil || !strings.Contains(output, `"passed": true`) {
		t.Fatalf("plugins verify: output=%q err=%v", output, err)
	}
	writeTestFile(t, filepath.Join(pluginDir, "extractor.py"), "tampered\n")
	output, err = captureStdout(t, func() error { return runPluginsVerify([]string{"--root", root, "--lock", lock, "--json"}) })
	if err != nil {
		t.Fatalf("JSON verification should report failures without command error: %v", err)
	}
	if !strings.Contains(output, `"passed": false`) || !strings.Contains(output, "artifact digest") {
		t.Fatalf("tamper was not reported: %s", output)
	}
	if err := runPlugins(nil); err == nil || runPlugins([]string{"unknown"}) == nil {
		t.Fatal("invalid plugins dispatch accepted")
	}
}

func TestSnapshotCommandsLifecycle(t *testing.T) {
	_, _, state := makeScannedFixture(t)
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"list", func() error { return runSnapshotsList([]string{"--state-dir", state, "--json"}) }},
		{"show-current", func() error { return runSnapshotsShow([]string{"--state-dir", state, "--current", "--json"}) }},
	} {
		output, err := captureStdout(t, test.call)
		if err != nil || !json.Valid([]byte(output)) {
			t.Fatalf("snapshots %s: output=%q err=%v", test.name, output, err)
		}
	}
	store, err := snapshot.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CurrentID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return runSnapshotsShow([]string{"--state-dir", state, id}) }); err != nil {
		t.Fatalf("show explicit snapshot: %v", err)
	}
	if err := runSnapshotsShow([]string{"--state-dir", state, "one", "two"}); err == nil {
		t.Fatal("snapshot show accepted two IDs")
	}
	if err := os.Remove(filepath.Join(state, "CURRENT")); err != nil {
		t.Fatal(err)
	}
	setCurrent, err := captureStdout(t, func() error {
		return runSnapshotsSetCurrent([]string{"--state-dir", state, "--json", id})
	})
	if err != nil || !strings.Contains(setCurrent, `"current": "`+id+`"`) {
		t.Fatalf("snapshot set-current: output=%q err=%v", setCurrent, err)
	}
	if current, err := store.CurrentID(); err != nil || current != id {
		t.Fatalf("CURRENT after repair = %q, %v", current, err)
	}
	if err := runSnapshotsSetCurrent([]string{"--state-dir", state}); err == nil {
		t.Fatal("snapshot set-current accepted a missing ID")
	}
	exportDir := filepath.Join(filepath.Dir(state), "snapshot-export")
	if _, err := captureStdout(t, func() error {
		return runSnapshotsExport([]string{"--state-dir", state, "--out", exportDir, "--include-sources"})
	}); err != nil {
		t.Fatalf("snapshot export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(exportDir, "bundle.json")); err != nil {
		t.Fatalf("snapshot export bundle: %v", err)
	}

	transaction, err := store.Begin("abandoned", nil)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := captureStdout(t, func() error { return runSnapshotsRecover([]string{"--state-dir", state, "--json"}) })
	if err != nil || strings.Contains(recovery, "abandoned") {
		t.Fatalf("snapshot recover removed live transaction: output=%q err=%v", recovery, err)
	}
	if err := transaction.Abort("test cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := runSnapshots(nil); err == nil || runSnapshots([]string{"unknown"}) == nil {
		t.Fatal("invalid snapshots dispatch accepted")
	}
}

func TestCheckCommandAndManifestVerification(t *testing.T) {
	_, output, _ := makeScannedFixture(t)
	text, err := captureStdout(t, func() error {
		return runCheck([]string{"--coverage", filepath.Join(output, "coverage.json"), "--json"})
	})
	if err != nil || !strings.Contains(text, `"passed": true`) {
		t.Fatalf("check: output=%q err=%v", text, err)
	}
	if err := runCheck([]string{"--coverage", filepath.Join(output, "coverage.json"), "--min-inventory-accounting", "1.1"}); err == nil || !strings.Contains(err.Error(), "inventory accounting") {
		t.Fatalf("quality threshold error = %v", err)
	}

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "good.txt"), "payload")
	sum := sha256.Sum256([]byte("payload"))
	manifest := map[string]any{"files": []map[string]any{{"path": "good.txt", "size_bytes": 7, "sha256": hex.EncodeToString(sum[:])}}}
	data, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(root, "manifest.json")
	writeTestFile(t, manifestPath, string(data))
	if failures := verifyExportFiles(root, manifestPath); len(failures) != 0 {
		t.Fatalf("valid manifest failures = %v", failures)
	}
	manifest = map[string]any{"files": []map[string]any{
		{"path": "../escape", "size_bytes": 1, "sha256": strings.Repeat("0", 64)},
		{"path": "missing", "size_bytes": 1, "sha256": strings.Repeat("0", 64)},
		{"path": "good.txt", "size_bytes": 99, "sha256": strings.Repeat("0", 64)},
	}}
	data, _ = json.Marshal(manifest)
	writeTestFile(t, manifestPath, string(data))
	failures := strings.Join(verifyExportFiles(root, manifestPath), "\n")
	for _, expected := range []string{"unsafe export path", "read exported file", "size", "digest"} {
		if !strings.Contains(failures, expected) {
			t.Fatalf("manifest failures missing %q: %s", expected, failures)
		}
	}

	policyPath := filepath.Join(root, "rkc.export-policy.json")
	writeTestFile(t, policyPath, `{"normalized_sources":true,"secret_redaction":false}`)
	if failures := verifySecretRedactionPolicy(root); len(failures) != 1 || !strings.Contains(failures[0], "without secret redaction") {
		t.Fatalf("redaction policy failures = %v", failures)
	}
	writeTestFile(t, policyPath, `{"normalized_sources":true,"secret_redaction":true}`)
	if failures := verifySecretRedactionPolicy(root); len(failures) != 0 {
		t.Fatalf("valid redaction policy failures = %v", failures)
	}

	writeTestFile(t, filepath.Join(output, "docs", "README.md"), "tampered")
	err = runCheck([]string{"--coverage", filepath.Join(output, "coverage.json")})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered export check error = %v", err)
	}
}

func TestDiffCommandValidationAndFormats(t *testing.T) {
	_, before, _ := makeScannedFixture(t)
	_, after, _ := makeScannedFixture(t)
	if err := runDiff([]string{before}); err == nil {
		t.Fatal("diff accepted one directory")
	}
	if err := runDiff([]string{"--format", "yaml", before, after}); err == nil || !strings.Contains(err.Error(), "unknown diff format") {
		t.Fatalf("unknown diff format error = %v", err)
	}
	for _, format := range []string{"json", "text", "markdown", "md"} {
		output, err := captureStdout(t, func() error { return runDiff([]string{"--format", format, before, after}) })
		if err != nil || strings.TrimSpace(output) == "" {
			t.Fatalf("diff %s: output=%q err=%v", format, output, err)
		}
	}
}

func TestFormattingAndSynthesisFileHelpers(t *testing.T) {
	if safeFileKey("A weird/path") == "" || strings.Contains(safeFileKey("A weird/path"), "/") {
		t.Fatalf("unsafe file key %q", safeFileKey("A weird/path"))
	}
	if profileName("ollama", "model/id", "task") == "" {
		t.Fatal("profile name is empty")
	}
	if maxIntCLI(2, 3) != 3 || maxIntCLI(4, 3) != 4 {
		t.Fatal("maxIntCLI is incorrect")
	}
	if escapeCell("a|b") != `a\|b` {
		t.Fatalf("escapeCell = %q", escapeCell("a|b"))
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte("x"))); len(got) != 64 {
		t.Fatal("test digest assumption failed")
	}
}
