package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestQuickstartBuildsAndVerifiesPortableAtlas(t *testing.T) {
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte(`package example

// Hello returns a stable greeting.
func Hello() string { return "hello" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	atlas := filepath.Join(repository, "knowledge")
	state := filepath.Join(repository, "state")
	output, err := captureStdout(t, func() error {
		return runQuickstartContext(context.Background(), []string{
			"--clean",
			"--out", atlas,
			"--state-dir", state,
			repository,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(atlas, "bundle.json"),
		filepath.Join(atlas, "coverage.json"),
		filepath.Join(atlas, "search", "index.json"),
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("quickstart output %s = %v, %v", path, info, err)
		}
	}
	if !strings.Contains(output, "RKC atlas is ready:") ||
		!strings.Contains(output, "Quality gate passed") ||
		!strings.Contains(output, filepath.ToSlash(filepath.Join(atlas, "notebooklm", "UPLOAD.md"))) {
		t.Fatalf("quickstart output = %q", output)
	}
	if strings.Contains(output, "<atlas>") || strings.Contains(output, "<question>") ||
		!strings.Contains(output, quoteCommandPath(atlas, runtime.GOOS)) {
		t.Fatalf("quickstart did not print copy-ready atlas commands: %q", output)
	}
}

func TestQuoteCommandPath(t *testing.T) {
	if got, want := quoteCommandPath("/tmp/user's atlas", "linux"), `'/tmp/user'"'"'s atlas'`; got != want {
		t.Fatalf("POSIX path = %q, want %q", got, want)
	}
	if got, want := quoteCommandPath(`C:\Users\O'Brien\atlas`, "windows"), `'C:\Users\O''Brien\atlas'`; got != want {
		t.Fatalf("PowerShell path = %q, want %q", got, want)
	}
}

func TestQuickstartInputContracts(t *testing.T) {
	if err := runQuickstartContext(context.Background(), []string{"--unknown"}); err == nil {
		t.Fatal("unknown quickstart option succeeded")
	}
	if err := runQuickstartContext(context.Background(), []string{"one", "two"}); err == nil ||
		!strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two repositories = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := runQuickstartContext(context.Background(), []string{missing}); err == nil ||
		!strings.Contains(err.Error(), "inspect quickstart repository") {
		t.Fatalf("missing repository = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runQuickstartContext(context.Background(), []string{file}); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file repository = %v", err)
	}
	if err := dispatch([]string{"quickstart", "--help"}); err != nil &&
		!strings.Contains(err.Error(), "help requested") {
		t.Fatalf("quickstart help = %v", err)
	}
}

func TestQuickstartOptionalAndFailurePaths(t *testing.T) {
	t.Run("configured Python and incremental profile", func(t *testing.T) {
		repository := t.TempDir()
		t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
		if err := os.WriteFile(
			filepath.Join(repository, "README.md"),
			[]byte("# Configured example\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(t.TempDir(), "rkc.json")
		if err := writeInitConfiguration(config, mustJSON(t, defaultConfiguration()), false); err != nil {
			t.Fatal(err)
		}
		if err := runQuickstartContext(context.Background(), []string{
			"--config", config,
			"--python",
			"--force=false",
			repository,
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("scan rejection", func(t *testing.T) {
		repository := t.TempDir()
		shared := filepath.Join(repository, "shared")
		err := runQuickstartContext(context.Background(), []string{
			"--clean",
			"--out", shared,
			"--state-dir", shared,
			repository,
		})
		if err == nil || !strings.Contains(err.Error(), "quickstart scan") {
			t.Fatalf("unsafe output/state = %v", err)
		}
	})

	t.Run("quality rejection", func(t *testing.T) {
		repository := t.TempDir()
		if err := os.WriteFile(
			filepath.Join(repository, ".env"),
			[]byte("GITHUB_TOKEN="+"gh"+"p_"+strings.Repeat("a", 40)+"\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		err := runQuickstartContext(context.Background(), []string{
			"--clean",
			repository,
		})
		if err == nil || !strings.Contains(err.Error(), "quickstart verification") {
			t.Fatalf("secret-bearing repository = %v", err)
		}
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}
