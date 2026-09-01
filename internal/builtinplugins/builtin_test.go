package builtinplugins

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializePythonRejectsInvalidDestinations(t *testing.T) {
	if _, err := MaterializePython(""); err == nil || !strings.Contains(err.Error(), "directory is empty") {
		t.Fatalf("MaterializePython(empty) = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePython(file); err == nil || !strings.Contains(err.Error(), "create builtin plugin directory") {
		t.Fatalf("MaterializePython(file) = %v", err)
	}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "python_extractor.py"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePython(directory); err == nil || !strings.Contains(err.Error(), "write bundled Python extractor") {
		t.Fatalf("MaterializePython(directory target) = %v", err)
	}
}

func TestMaterializePythonReportsWorkingDirectoryResolutionFailure(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	cwd := filepath.Join(base, "cwd")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()
	// Removing the current directory is valid on Unix, but makes filepath.Abs
	// fail when it asks the operating system to resolve the working directory.
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePython("materialized"); err == nil {
		t.Fatal("MaterializePython() succeeded with an unresolvable working directory")
	}
}

func TestMaterializePythonWritesExecutableEmbeddedExtractor(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "plugins")
	path, err := MaterializePython(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Dir(path) != directory {
		t.Fatalf("MaterializePython() path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(pythonExtractor) || !strings.Contains(string(data), "RKC Python AST extractor") {
		t.Fatal("materialized extractor does not match embedded source")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("materialized mode = %v, %v", info.Mode(), err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := MaterializePython(directory)
	if err != nil || again != path {
		t.Fatalf("second MaterializePython() = %q, %v", again, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != string(pythonExtractor) {
		t.Fatalf("second materialization did not restore source: %v", err)
	}
}

func TestMaterializedExtractorMatchesPublishedDigest(t *testing.T) {
	script, err := MaterializePython(filepath.Join(t.TempDir(), "builtin"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != PythonSHA256() {
		t.Fatalf("materialized digest = %s, published digest = %s", got, PythonSHA256())
	}
}

func TestEmbeddedExtractorMatchesPublishedWorker(t *testing.T) {
	published, err := os.ReadFile(filepath.Join("..", "..", "plugins", "python-ast", "extractor.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(published) != string(pythonExtractor) {
		t.Fatal("embedded and published Python extractors differ")
	}
}
