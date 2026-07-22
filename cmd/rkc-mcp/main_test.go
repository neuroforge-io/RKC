package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func writeTestDataset(t *testing.T, root string) string {
	t.Helper()
	dataset := filepath.Join(root, "atlas")
	transaction, err := safeoutput.Begin(dataset, root, false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transaction.Abort() })
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{
		SchemaVersion: rkcmodel.SchemaVersion,
		ID:            "rkc:snapshot:test",
		RootName:      "fixture",
		ContentDigest: strings.Repeat("a", 64),
		Status:        "committed",
		Tool:          rkcmodel.ToolInfo{Name: "rkc", Version: "test"},
	}}
	if err := rkcexport.WriteAll(bundle, rkcmodel.BuildCoverage(bundle), rkcexport.Options{
		Root:                root,
		Output:              transaction.Staging,
		DisableStaticSite:   true,
		DisableJSONLGraph:   true,
		DisableSearchIndex:  true,
		DisableIntegrations: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(bundle.Snapshot.ID); err != nil {
		t.Fatal(err)
	}
	return dataset
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestRunCoversFlagsLoadingIOAndCancellation(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &output, &diagnostics); code != 0 {
		t.Fatalf("version exit code = %d, diagnostics = %q", code, diagnostics.String())
	}
	if got := strings.TrimSpace(output.String()); got != version {
		t.Fatalf("version output = %q", got)
	}

	output.Reset()
	diagnostics.Reset()
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &output, &diagnostics); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(diagnostics.String(), "Usage of rkc-mcp") {
		t.Fatalf("help output = %q", diagnostics.String())
	}

	diagnostics.Reset()
	if code := run(context.Background(), []string{"--definitely-invalid"}, strings.NewReader(""), &output, &diagnostics); code != 2 {
		t.Fatalf("invalid flag exit code = %d", code)
	}
	if !strings.Contains(diagnostics.String(), "flag provided but not defined") {
		t.Fatalf("invalid flag diagnostics = %q", diagnostics.String())
	}

	diagnostics.Reset()
	missing := filepath.Join(t.TempDir(), "missing")
	if code := run(context.Background(), []string{"--dir", missing}, strings.NewReader(""), &output, &diagnostics); code != 1 {
		t.Fatalf("missing dataset exit code = %d", code)
	}
	if !strings.Contains(diagnostics.String(), "rkc-mcp:") {
		t.Fatalf("missing dataset diagnostics = %q", diagnostics.String())
	}

	root := t.TempDir()
	dataset := writeTestDataset(t, root)
	diagnostics.Reset()
	if code := run(context.Background(), []string{"--dir", dataset}, strings.NewReader(""), &output, &diagnostics); code != 0 {
		t.Fatalf("EOF exit code = %d, diagnostics = %q", code, diagnostics.String())
	}

	readFailure := errors.New("deterministic read failure")
	diagnostics.Reset()
	if code := run(context.Background(), []string{"--dir", dataset}, failingReader{err: readFailure}, io.Discard, &diagnostics); code != 1 {
		t.Fatalf("read failure exit code = %d", code)
	}
	if !strings.Contains(diagnostics.String(), readFailure.Error()) {
		t.Fatalf("read failure diagnostics = %q", diagnostics.String())
	}

	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	writeFailure := errors.New("deterministic write failure")
	diagnostics.Reset()
	if code := run(context.Background(), []string{"--dir", dataset}, strings.NewReader(request), failingWriter{err: writeFailure}, &diagnostics); code != 1 {
		t.Fatalf("write failure exit code = %d", code)
	}
	if !strings.Contains(diagnostics.String(), writeFailure.Error()) {
		t.Fatalf("write failure diagnostics = %q", diagnostics.String())
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	output.Reset()
	diagnostics.Reset()
	if code := run(cancelled, []string{"--dir", dataset}, strings.NewReader(request), &output, &diagnostics); code != 0 {
		t.Fatalf("cancelled request exit code = %d, diagnostics = %q", code, diagnostics.String())
	}
	if !strings.Contains(output.String(), "request cancelled") {
		t.Fatalf("cancelled response = %q", output.String())
	}
}

func TestMainDelegatesItsExitCode(t *testing.T) {
	previousArgs := os.Args
	previousExit := exitProcess
	t.Cleanup(func() {
		os.Args = previousArgs
		exitProcess = previousExit
	})
	os.Args = []string{"rkc-mcp", "--version"}
	exitCode := -1
	exitProcess = func(code int) { exitCode = code }
	main()
	if exitCode != 0 {
		t.Fatalf("main exit code = %d", exitCode)
	}
}

func TestExecutableVersionFailureAndStdioLifecycle(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "rkc-mcp")
	// Use the driver from the same GOROOT that compiled this test. Host PATHs
	// can legitimately contain several Go installations; mixing their drivers
	// and compile tools creates invalid, non-reproducible binaries.
	command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build rkc-mcp: %v\n%s", err, output)
	}

	command = exec.Command(binary, "--version")
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != version {
		t.Fatalf("version: output=%q err=%v", output, err)
	}

	command = exec.Command(binary, "--dir", filepath.Join(root, "missing"))
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "rkc-mcp:") {
		t.Fatalf("missing dataset: output=%q err=%v", output, err)
	}

	dataset := writeTestDataset(t, root)
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	command = exec.Command(binary, "--dir", dataset)
	command.Stdin = strings.NewReader(input)
	output, err = command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"name":"rkc-mcp"`) || !strings.Contains(string(output), `"id":1`) {
		t.Fatalf("stdio lifecycle: output=%q err=%v", output, err)
	}
}
