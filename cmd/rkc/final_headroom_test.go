package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type finalHeadroomCancelContext struct {
	context.Context
	cancelAt int
	calls    int
}

func (ctx *finalHeadroomCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *finalHeadroomCancelContext) Done() <-chan struct{}       { return nil }
func (ctx *finalHeadroomCancelContext) Value(key any) any           { return ctx.Context.Value(key) }
func (ctx *finalHeadroomCancelContext) Err() error {
	ctx.calls++
	if ctx.calls >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestFinalHeadroomCheckCanonicalAndFilesystemFailures(t *testing.T) {
	_, output, _ := makeScannedFixture(t)
	coveragePath := filepath.Join(output, "coverage.json")
	bundlePath := filepath.Join(output, "bundle.json")
	bundleData, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	var bundle rkcmodel.Bundle
	if err := json.Unmarshal(bundleData, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.Snapshot.ID = "rkc:snapshot:wrong-identity"
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
		ID:   "rkc:node:invalid-vocabulary",
		Kind: "not-a-canonical-node-kind",
		Name: "InvalidVocabulary",
	})
	changedData, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	changedBundle := filepath.Join(t.TempDir(), "changed-bundle.json")
	writeTestFile(t, changedBundle, string(changedData))
	err = runCheck([]string{
		"--coverage", coveragePath,
		"--bundle", changedBundle,
		"--verify-files=false",
		"--require-secret-redaction=false",
	})
	if err == nil || !strings.Contains(err.Error(), "bundle digest") || !strings.Contains(err.Error(), "bundle snapshot") {
		t.Fatalf("canonical bundle mismatch = %v", err)
	}

	bundleDirectory := t.TempDir()
	err = runCheck([]string{
		"--coverage", coveragePath,
		"--bundle", bundleDirectory,
		"--verify-files=false",
		"--require-secret-redaction=false",
	})
	if err == nil || !strings.Contains(err.Error(), "read bundle") {
		t.Fatalf("directory bundle = %v", err)
	}

	manifestDirectory := t.TempDir()
	err = runCheck([]string{
		"--coverage", coveragePath,
		"--verify-bundle=false",
		"--export-manifest", manifestDirectory,
		"--require-secret-redaction=false",
	})
	if err == nil || !strings.Contains(err.Error(), "read export manifest") {
		t.Fatalf("directory export manifest = %v", err)
	}

	if runtime.GOOS != "windows" {
		loopRoot := t.TempDir()
		bundleLoop := filepath.Join(loopRoot, "bundle-loop")
		if err := os.Symlink("bundle-loop", bundleLoop); err != nil {
			t.Fatal(err)
		}
		err = runCheck([]string{
			"--coverage", coveragePath,
			"--bundle", bundleLoop,
			"--verify-files=false",
			"--require-secret-redaction=false",
		})
		if err == nil || !strings.Contains(err.Error(), "stat bundle") {
			t.Fatalf("bundle symlink loop = %v", err)
		}

		manifestLoop := filepath.Join(loopRoot, "manifest-loop")
		if err := os.Symlink("manifest-loop", manifestLoop); err != nil {
			t.Fatal(err)
		}
		err = runCheck([]string{
			"--coverage", coveragePath,
			"--verify-bundle=false",
			"--export-manifest", manifestLoop,
			"--require-secret-redaction=false",
		})
		if err == nil || !strings.Contains(err.Error(), "stat export manifest") {
			t.Fatalf("manifest symlink loop = %v", err)
		}
	}
}

func TestFinalHeadroomSemanticDiffFailurePolicies(t *testing.T) {
	repository, before, _ := makeScannedFixture(t)
	root := filepath.Dir(repository)
	writeTestFile(t, filepath.Join(repository, "main.go"), `package fixture

// Alpha calls Beta.
func Alpha() string { return Beta() }

// Beta returns a changed value.
func Beta() string { return "changed" }
`)
	riskOutput := filepath.Join(root, "risk-atlas")
	if _, err := captureStdout(t, func() error {
		return runScan([]string{
			"--out", riskOutput, "--state-dir", "",
			"--no-python", "--no-typescript", "--no-frameworks",
			"--no-static-site", "--no-integrations", repository,
		})
	}); err != nil {
		t.Fatalf("scan risk fixture: %v", err)
	}
	err := runDiff([]string{"--fail-on-risk", before, riskOutput})
	if err == nil || !strings.Contains(err.Error(), "risk change") {
		t.Fatalf("risk diff policy = %v", err)
	}

	writeTestFile(t, filepath.Join(repository, "main.go"), `package fixture

// Alpha no longer calls the removed public Beta symbol.
func Alpha() string { return "alpha" }
`)
	breakingOutput := filepath.Join(root, "breaking-atlas")
	if _, err := captureStdout(t, func() error {
		return runScan([]string{
			"--out", breakingOutput, "--state-dir", "",
			"--no-python", "--no-typescript", "--no-frameworks",
			"--no-static-site", "--no-integrations", repository,
		})
	}); err != nil {
		t.Fatalf("scan breaking fixture: %v", err)
	}
	err = runDiff([]string{"--fail-on-breaking", before, breakingOutput})
	if err == nil || !strings.Contains(err.Error(), "breaking change") {
		t.Fatalf("breaking diff policy = %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing-atlas")
	if err := runDiff([]string{missing, before}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing before dataset = %v", err)
	}
	if err := runDiff([]string{before, missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing after dataset = %v", err)
	}
}

func TestFinalHeadroomRemoteScanAndConfigurationBoundaries(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repository, "go.mod"), "module example.test/remote\n\ngo 1.23\n")
	writeTestFile(t, filepath.Join(repository, "remote.go"), "package remote\n\nfunc Public() string { return \"remote\" }\n")
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "rkc-tests@example.test"},
		{"config", "user.name", "RKC Tests"},
		{"add", "."},
		{"commit", "-qm", "fixture"},
	} {
		command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	output := filepath.Join(t.TempDir(), "remote-atlas")
	acquireTemp := t.TempDir()
	stdout, err := captureStdout(t, func() error {
		return runScan([]string{
			"--out", output,
			"--state-dir", "",
			"--allow-file-url",
			"--keep-materialized",
			"--acquire-temp", acquireTemp,
			"--no-plugins",
			"--no-frameworks",
			"--no-static-site",
			"--no-jsonl-graph",
			"--no-search-index",
			"--no-integrations",
			"file://" + filepath.ToSlash(repository),
		})
	})
	if err != nil || !strings.Contains(stdout, "Materialized repository:") || !strings.Contains(stdout, "Source: file://") {
		t.Fatalf("remote scan output = %q, %v", stdout, err)
	}

	for name, arguments := range map[string][]string{
		"invalid configuration": {
			"--out", filepath.Join(t.TempDir(), "invalid-config"),
			"--state-dir", "", "--no-plugins", "--no-frameworks",
			"--max-file-bytes", "-1", repository,
		},
		"whitespace database": {
			"--out", filepath.Join(t.TempDir(), "whitespace-database"),
			"--state-dir", "", "--database", " ",
			"--no-plugins", "--no-frameworks", repository,
		},
		"database inside repository": {
			"--out", filepath.Join(t.TempDir(), "inside-database"),
			"--state-dir", "", "--database", filepath.Join(repository, "rkc.sqlite"),
			"--no-plugins", "--no-frameworks", repository,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runScanContext(t.Context(), arguments); err == nil {
				t.Fatalf("scan accepted %s", name)
			}
		})
	}
}

func TestFinalHeadroomSynthesisFilesystemAndIdentityFailures(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "store.sqlite")
	writeTestFile(t, database, "database identity")
	resolved, protected, err := synthesisDatasetIdentity(database)
	if err != nil || resolved != database || protected != "" {
		t.Fatalf("regular dataset identity = %q, %q, %v", resolved, protected, err)
	}

	if runtime.GOOS != "windows" {
		fifo := filepath.Join(root, "dataset.fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := synthesisDatasetIdentity(fifo); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
			t.Fatalf("FIFO dataset identity = %v", err)
		}
	}

	dataset := filepath.Join(root, "atlas")
	if err := os.Mkdir(dataset, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSynthesisOutput(root, dataset, "", "profile"); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("output containing dataset = %v", err)
	}

	writer := bufio.NewWriterSize(commandFailWriter{}, 1)
	if err := writeJSONLine(writer, map[string]string{"value": "large enough to bypass the buffer"}); err == nil {
		t.Fatal("writeJSONLine accepted an immediate writer failure")
	}

	pretty := filepath.Join(root, "pretty.json")
	if err := os.Mkdir(pretty+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrettyJSONFile(pretty, map[string]string{"x": "y"}); err == nil {
		t.Fatal("writePrettyJSONFile replaced its staging directory")
	}
	if err := os.Remove(pretty + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pretty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrettyJSONFile(pretty, map[string]string{"x": "y"}); err == nil {
		t.Fatal("writePrettyJSONFile renamed over a directory")
	}

	parentFile := filepath.Join(root, "markdown-parent")
	writeTestFile(t, parentFile, "not a directory")
	if err := writeSynthesisMarkdown(
		filepath.Join(parentFile, "summary.md"),
		rkcmodel.Node{},
		modelruntime.EvidencePacket{},
		modelruntime.ClaimValidation{},
		modelruntime.ModelDescriptor{},
	); err == nil {
		t.Fatal("writeSynthesisMarkdown accepted a regular-file parent")
	}
	markdownDirectory := filepath.Join(root, "summary.md")
	if err := os.Mkdir(markdownDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSynthesisMarkdown(
		markdownDirectory,
		rkcmodel.Node{},
		modelruntime.EvidencePacket{},
		modelruntime.ClaimValidation{},
		modelruntime.ModelDescriptor{},
	); err == nil {
		t.Fatal("writeSynthesisMarkdown replaced a directory")
	}

	brokenRoot := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Symlink("missing-target", filepath.Join(brokenRoot, "broken")); err != nil {
			t.Fatal(err)
		}
		if _, err := inventorySynthesisFiles(brokenRoot); err == nil {
			t.Fatal("inventorySynthesisFiles accepted a broken symlink")
		}
	}
}

func TestFinalHeadroomSynthesisCancellationCheckpointsDoNotPublish(t *testing.T) {
	repository, atlas, _ := makeScannedFixture(t)
	for _, cancelAt := range []int{2, 3, 4, 5} {
		t.Run(string(rune('0'+cancelAt)), func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "derived")
			ctx := &finalHeadroomCancelContext{
				Context:  context.Background(),
				cancelAt: cancelAt,
			}
			err := runSynthesizeContext(ctx, []string{
				"--dir", atlas,
				"--repo-root", repository,
				"--out", target,
				"--packet-only",
				"--query", "Alpha",
				"--limit", "1",
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel checkpoint %d = %v", cancelAt, err)
			}
			if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
				t.Fatalf("cancel checkpoint %d published output: %v", cancelAt, statErr)
			}
		})
	}
}

func TestFinalHeadroomCommandPublicationAndWarningBoundaries(t *testing.T) {
	root := t.TempDir()
	if err := runInit([]string{"--definitely-invalid"}); err == nil {
		t.Fatal("init accepted an unknown flag")
	}
	parentFile := filepath.Join(root, "configuration-parent")
	writeTestFile(t, parentFile, "not a directory")
	if err := runInit([]string{"--path", filepath.Join(parentFile, "rkc.json")}); err == nil {
		t.Fatal("init accepted a regular-file parent")
	}
	directoryTarget := filepath.Join(root, "configuration-directory")
	if err := os.Mkdir(directoryTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"--path", directoryTarget, "--force"}); err == nil {
		t.Fatal("init replaced a directory")
	}
	if runtime.GOOS != "windows" {
		loop := filepath.Join(root, "configuration-loop")
		if err := os.Symlink("configuration-loop", loop); err != nil {
			t.Fatal(err)
		}
		if err := runInit([]string{"--path", loop}); err == nil {
			t.Fatal("init accepted a symlink loop")
		}
	}

	config := defaultConfiguration()
	config.Plugins.PythonAST.Interpreter = "rkc-python-that-cannot-exist"
	configData, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "doctor.json")
	writeTestFile(t, configPath, string(configData))
	stdout, err := captureStdout(t, func() error {
		return runDoctor([]string{"--config", configPath, "--repository", root, "--json"})
	})
	if err != nil || !strings.Contains(stdout, `"warnings": 1`) || !strings.Contains(stdout, "rkc-python-that-cannot-exist") {
		t.Fatalf("doctor warning report = %q, %v", stdout, err)
	}

	_, atlas, _ := makeScannedFixture(t)
	readyFile := filepath.Join(root, "missing-ready-parent", "ready.json")
	err = runServe([]string{
		"--dir", atlas,
		"--addr", "127.0.0.1:0",
		"--ready-file", readyFile,
	})
	if err == nil || !strings.Contains(err.Error(), "create readiness staging file") {
		t.Fatalf("serve missing readiness parent = %v", err)
	}

	if got := keys(map[string]struct{}{"z": {}, "a": {}, "m": {}}); strings.Join(got, ",") != "a,m,z" {
		t.Fatalf("sorted presentation keys = %v", got)
	}
}

func TestFinalHeadroomDeletedWorkingDirectoryFailures(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	manifestPath := filepath.Join(outside, "manifest.json")
	writeTestFile(t, manifestPath, `{"files":[]}`)
	dead := filepath.Join(outside, "deleted-working-directory")
	if err := os.Mkdir(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dead); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()
	if err := os.Remove(dead); err != nil {
		t.Fatal(err)
	}

	if _, _, err := discoverPlugins("plugins"); err == nil {
		t.Fatal("plugin discovery resolved a deleted working directory")
	}
	if failures := verifyExportFiles(".", manifestPath); len(failures) != 1 || !strings.Contains(failures[0], "resolve export root") {
		t.Fatalf("deleted-cwd export failures = %v", failures)
	}
	if err := publishServeReadyFile("ready.json", serveReadyReceipt{}); err == nil || !strings.Contains(err.Error(), "resolve readiness file") {
		t.Fatalf("deleted-cwd readiness error = %v", err)
	}
	if _, err := canonicalSQLitePath("store.sqlite"); err == nil || !strings.Contains(err.Error(), "resolve SQLite database") {
		t.Fatalf("deleted-cwd SQLite path = %v", err)
	}
	if _, err := resolveSQLiteExportOutput("export", filepath.Join(outside, "store.sqlite")); err == nil || !strings.Contains(err.Error(), "resolve SQLite export") {
		t.Fatalf("deleted-cwd SQLite export = %v", err)
	}
	stdout, err := captureStdout(t, func() error { return runDoctor([]string{"--repository", ".", "--json"}) })
	if err == nil || !strings.Contains(err.Error(), "fatal problem") || !strings.Contains(stdout, `"repository"`) {
		t.Fatalf("deleted-cwd doctor = %q, %v", stdout, err)
	}
}
