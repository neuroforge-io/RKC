package builtinplugins

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed python_extractor.py
var pythonExtractor []byte

// PythonSHA256 returns the digest of the exact extractor bytes embedded in the
// RKC binary. The plugin host requires this digest before it will execute the
// materialized adapter, so an arbitrary --python-plugin path cannot be
// mistaken for the vetted built-in worker.
func PythonSHA256() string {
	digest := sha256.Sum256(pythonExtractor)
	return hex.EncodeToString(digest[:])
}

// MaterializePython writes the bundled Python AST extractor to dir and returns
// its absolute path. The caller owns cleanup of dir.
func MaterializePython(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("builtin plugin directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create builtin plugin directory: %w", err)
	}
	path := filepath.Join(dir, "python_extractor.py")
	// The interpreter only needs read access. Keeping the materialized worker
	// owner-only and non-executable narrows the interval between digest
	// verification and the sandboxed interpreter opening it.
	if err := os.WriteFile(path, pythonExtractor, 0o400); err != nil {
		return "", fmt.Errorf("write bundled Python extractor: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
