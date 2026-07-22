package builtinplugins

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed python_extractor.py
var pythonExtractor []byte

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
	if err := os.WriteFile(path, pythonExtractor, 0o700); err != nil {
		return "", fmt.Errorf("write bundled Python extractor: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
