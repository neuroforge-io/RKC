// Package sourcepath provides race-aware, portable access to inventoried
// repository files. Repository paths are untrusted input even when they were
// produced by an earlier inventory stage.
package sourcepath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OpenRegular opens path as a regular file beneath root. It accepts only the
// canonical forward-slash paths used by repository inventories and rejects
// symlinks in both the root and every source component.
//
// Portable filesystem APIs cannot make a multi-component walk fully atomic.
// OpenRegular therefore snapshots every component, opens the final file, and
// then verifies that the complete path identity stayed unchanged. Callers that
// need a transaction across multiple reads must perform their own post-read
// identity or content-digest verification as well.
func OpenRegular(root, path string) (*os.File, error) {
	resolvedRoot, rootInfo, err := inspectRoot(root)
	if err != nil {
		return nil, err
	}
	relative, parts, err := canonicalRelative(path)
	if err != nil {
		return nil, err
	}

	states := make([]componentState, 0, len(parts))
	current := resolvedRoot
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, inspectErr := os.Lstat(current)
		if inspectErr != nil {
			return nil, sourceError("inspect", relative, inspectErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("source path %q contains a symlink", relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("source path %q contains a non-directory component", relative)
		}
		states = append(states, componentState{path: current, info: info})
	}
	if !states[len(states)-1].info.Mode().IsRegular() {
		return nil, fmt.Errorf("source path %q is not a regular file", relative)
	}

	input, err := os.Open(current)
	if err != nil {
		return nil, sourceError("open", relative, err)
	}
	openedInfo, err := input.Stat()
	if err != nil {
		_ = input.Close()
		return nil, sourceError("inspect opened", relative, err)
	}
	if !openedInfo.Mode().IsRegular() || !sameSnapshot(states[len(states)-1].info, openedInfo) {
		_ = input.Close()
		return nil, fmt.Errorf("source path %q changed while opening", relative)
	}
	if err := verifyUnchanged(resolvedRoot, rootInfo, states, relative); err != nil {
		_ = input.Close()
		return nil, err
	}
	return input, nil
}

// ReadFile reads a regular repository file using OpenRegular's containment and
// identity checks.
func ReadFile(root, path string) ([]byte, error) {
	input, err := OpenRegular(root, path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, sourceError("read", path, err)
	}
	return data, nil
}

// ResolveRelative returns a canonical repository-relative path, or an error
// when joining base and target would escape the repository namespace. It does
// not access the filesystem.
func ResolveRelative(base, target string) (string, error) {
	if target == "" || strings.IndexByte(target, 0) >= 0 {
		return "", errors.New("repository-relative path is empty or invalid")
	}
	nativeTarget := filepath.FromSlash(target)
	if filepath.IsAbs(nativeTarget) || filepath.VolumeName(nativeTarget) != "" {
		return "", fmt.Errorf("repository-relative path %q is absolute", target)
	}
	joined := filepath.Clean(filepath.Join(filepath.FromSlash(base), nativeTarget))
	resolved := filepath.ToSlash(joined)
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") || filepath.IsAbs(joined) {
		return "", fmt.Errorf("repository-relative path %q escapes the repository", target)
	}
	return strings.TrimPrefix(resolved, "./"), nil
}

type componentState struct {
	path string
	info os.FileInfo
}

func inspectRoot(root string) (string, os.FileInfo, error) {
	if strings.TrimSpace(root) == "" || strings.IndexByte(root, 0) >= 0 {
		return "", nil, errors.New("repository root is empty or invalid")
	}
	resolved, err := filepath.Abs(root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", nil, rootError("inspect", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.New("repository root must be a real directory, not a symlink")
	}
	return resolved, info, nil
}

func canonicalRelative(path string) (string, []string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", nil, errors.New("source path is empty or invalid")
	}
	native := filepath.FromSlash(path)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", nil, fmt.Errorf("source path %q is not repository-relative", path)
	}
	clean := filepath.Clean(native)
	if clean == "." || filepath.ToSlash(clean) != path {
		return "", nil, fmt.Errorf("source path %q is not canonical", path)
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", nil, fmt.Errorf("source path %q contains an unsafe component", path)
		}
	}
	return path, parts, nil
}

func verifyUnchanged(root string, rootInfo os.FileInfo, states []componentState, relative string) error {
	currentRoot, err := os.Lstat(root)
	if err != nil {
		return rootError("reinspect", err)
	}
	if currentRoot.Mode()&os.ModeSymlink != 0 || !currentRoot.IsDir() || !sameSnapshot(rootInfo, currentRoot) {
		return errors.New("repository root changed while opening source")
	}
	for _, state := range states {
		current, err := os.Lstat(state.path)
		if err != nil {
			return sourceError("reinspect", relative, err)
		}
		if current.Mode()&os.ModeSymlink != 0 || !sameSnapshot(state.info, current) {
			return fmt.Errorf("source path %q changed while opening", relative)
		}
	}
	return nil
}

// sameSnapshot supplements filesystem identity with stable metadata. Some
// filesystems can immediately reuse an inode after unlink, which makes
// os.SameFile alone insufficient for detecting a remove-and-recreate race.
func sameSnapshot(before, after os.FileInfo) bool {
	return os.SameFile(before, after) &&
		before.Mode() == after.Mode() &&
		before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime())
}

func sourceError(operation, relative string, err error) error {
	return fmt.Errorf("%s source path %q: %w", operation, relative, underlyingPathError(err))
}

func rootError(operation string, err error) error {
	return fmt.Errorf("%s repository root: %w", operation, underlyingPathError(err))
}

func underlyingPathError(err error) error {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return pathError.Err
	}
	return err
}
