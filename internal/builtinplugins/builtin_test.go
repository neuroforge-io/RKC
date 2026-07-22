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
