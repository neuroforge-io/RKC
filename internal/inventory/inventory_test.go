package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"main.py":        "python",
		"src/app.tsx":    "typescript",
		"Dockerfile":     "dockerfile",
		"CMakeLists.txt": "cmake",
		"README.md":      "markdown",
	}
	for path, want := range cases {
		if got := DetectLanguage(path); got != want {
			t.Fatalf("DetectLanguage(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestScanAccountsForExcludedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("def main():\n    return 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(Options{Root: root, Excludes: []string{".git"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(result.Artifacts))
	}
	var foundExcluded bool
	for _, artifact := range result.Artifacts {
		if artifact.Path == ".git" && artifact.Status == "excluded" {
			foundExcluded = true
		}
	}
	if !foundExcluded {
		t.Fatal("excluded directory was not explicitly recorded")
	}
}
