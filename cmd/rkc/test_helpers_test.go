package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(main *testing.M) {
	cacheRoot, err := os.MkdirTemp("", "rkc-cli-test-cache-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CACHE_HOME", cacheRoot); err != nil {
		_ = os.RemoveAll(cacheRoot)
		panic(err)
	}
	code := main.Run()
	_ = os.RemoveAll(cacheRoot)
	os.Exit(code)
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	result := make(chan []byte, 1)
	go func() {
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, reader)
		result <- buffer.Bytes()
	}()
	callErr := fn()
	_ = writer.Close()
	os.Stdout = original
	output := <-result
	_ = reader.Close()
	return string(output), callErr
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeScannedFixture(t *testing.T) (repository, output, state string) {
	t.Helper()
	root := t.TempDir()
	repository = filepath.Join(root, "repo")
	writeTestFile(t, filepath.Join(repository, "go.mod"), "module example.test/fixture\n\ngo 1.23\n")
	writeTestFile(t, filepath.Join(repository, "main.go"), `package fixture

// Alpha calls Beta.
func Alpha() string { return Beta() }

// Beta returns a stable value.
func Beta() string { return "beta" }
`)
	writeTestFile(t, filepath.Join(repository, "README.md"), "# Fixture\n\nA compact command test fixture.\n")
	output = filepath.Join(root, "atlas")
	state = filepath.Join(root, "state")
	_, err := captureStdout(t, func() error {
		return runScan([]string{
			"--out", output,
			"--state-dir", state,
			"--no-python",
			"--no-typescript",
			"--no-frameworks",
			"--no-static-site",
			"--no-integrations",
			"--force",
			repository,
		})
	})
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return repository, output, state
}
