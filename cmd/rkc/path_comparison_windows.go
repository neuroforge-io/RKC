//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuroforge-io/RKC/internal/privatepath"
	"golang.org/x/sys/windows"
)

func pathComparisonNames(parent, candidate string) (string, string, error) {
	// Preserve the existing relative-path contract. Admission callers supply
	// absolute paths after resolving their source/output directories.
	if !filepath.IsAbs(parent) || !filepath.IsAbs(candidate) {
		return parent, candidate, nil
	}
	resolvedParent, err := windowsComparisonName(parent)
	if err != nil {
		return "", "", fmt.Errorf("resolve comparison parent: %w", err)
	}
	resolvedCandidate, err := windowsComparisonName(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolve comparison candidate: %w", err)
	}
	return resolvedParent, resolvedCandidate, nil
}

// windowsComparisonName resolves the nearest existing ancestor to its normalized
// NT device name. Drive letters can differ while pointing into the same tree
// (SUBST), so treating a filepath.Rel volume error as disjointness is unsafe.
// These names are used only for comparison, never for filesystem operations.
// https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-getfinalpathnamebyhandlew
func windowsComparisonName(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		encoded, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return "", err
		}
		// Follow aliases to the actual object. The callers independently admit
		// source/output links; a lexical alias must not hide their overlap.
		handle, err := windows.CreateFile(encoded, 0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err == nil {
			file := os.NewFile(uintptr(handle), current)
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return "", err
			}
			if len(missing) != 0 && !info.IsDir() {
				return "", errors.New("comparison ancestor is not a directory")
			}
			var name [32768]uint16
			// Network shares can alias a local directory (or another share),
			// despite having unrelated NT names. Preserve fail-closed admission
			// unless this same handle proves a local volume GUID is available.
			guidLength, err := windows.GetFinalPathNameByHandle(handle, &name[0], uint32(len(name)), 1) // VOLUME_NAME_GUID
			if err != nil || guidLength == 0 || guidLength >= uint32(len(name)) ||
				!strings.HasPrefix(windows.UTF16ToString(name[:guidLength]), `\\?\Volume{`) {
				return "", errors.New("Windows path containment requires local volumes; network-backed paths cannot prove disjointness")
			}
			length, err := windows.GetFinalPathNameByHandle(handle, &name[0], uint32(len(name)), 2) // VOLUME_NAME_NT, FILE_NAME_NORMALIZED
			if err != nil {
				return "", err
			}
			if length == 0 || length >= uint32(len(name)) {
				return "", errors.New("comparison name exceeds the Windows path bound")
			}
			resolved := filepath.Clean(windows.UTF16ToString(name[:length]))
			if !strings.HasPrefix(resolved, `\Device\`) {
				return "", errors.New("comparison name is not a normalized NT device path")
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return "", &os.PathError{Op: "resolve comparison path", Path: current, Err: err}
		}
		// A dangling link exists even though opening its referent failed. It
		// must not become a prospective ordinary directory in this comparison.
		if _, inspectErr := privatepath.Lstat(current); inspectErr == nil {
			return "", errors.New("comparison path exists but its target cannot be resolved")
		} else if !errors.Is(inspectErr, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(inspectErr, windows.ERROR_PATH_NOT_FOUND) {
			return "", inspectErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", &os.PathError{Op: "resolve comparison root", Path: current, Err: err}
		}
		component := filepath.Base(current)
		if strings.TrimRight(component, ". ") != component || strings.Contains(component, ":") {
			return "", errors.New("prospective comparison path contains an ambiguous Windows name")
		}
		missing = append(missing, component)
		current = parent
	}
}
