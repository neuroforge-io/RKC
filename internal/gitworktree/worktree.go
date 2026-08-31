// Package gitworktree verifies that Git metadata belongs to the exact
// directory RKC was asked to inspect. Git normally searches parent
// directories for metadata; that discovery is unsafe for arbitrary-folder
// scans because a plain directory beneath another checkout is not itself a
// repository.
package gitworktree

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// MaximumTopLevelBytes bounds a Git-reported work-tree pathname.
const MaximumTopLevelBytes = 4096

// AffinityEnvironmentIsPaired reports whether ambient Git repository
// affinity is either absent or explicitly supplies both sides of an external
// work tree. An unpaired GIT_DIR can otherwise make an arbitrary directory
// appear to be the work tree of an unrelated repository.
func AffinityEnvironmentIsPaired() bool {
	gitDirectory := os.Getenv("GIT_DIR")
	gitWorkTree := os.Getenv("GIT_WORK_TREE")
	return (gitDirectory == "") == (gitWorkTree == "")
}

// ParseTopLevelOutput parses `git rev-parse --show-toplevel` without trimming
// valid leading or trailing spaces from a directory name. Embedded line breaks
// and NULs are rejected because they are not safe as command-output records.
func ParseTopLevelOutput(output []byte) (string, bool) {
	if len(output) == 0 || len(output) > MaximumTopLevelBytes+2 {
		return "", false
	}
	if output[len(output)-1] == '\n' {
		output = output[:len(output)-1]
		if len(output) > 0 && output[len(output)-1] == '\r' {
			output = output[:len(output)-1]
		}
	}
	if len(output) == 0 || len(output) > MaximumTopLevelBytes ||
		bytes.ContainsAny(output, "\x00\r\n") {
		return "", false
	}
	return string(output), true
}

// IsExactRoot reports whether reportedTopLevel identifies the same directory
// as requestedRoot. Filesystem identity is used rather than string equality so
// equivalent path spellings and platform-specific case rules are respected.
func IsExactRoot(requestedRoot, reportedTopLevel string) bool {
	if requestedRoot == "" || reportedTopLevel == "" ||
		len(reportedTopLevel) > MaximumTopLevelBytes ||
		strings.IndexByte(reportedTopLevel, 0) >= 0 ||
		!filepath.IsAbs(requestedRoot) || !filepath.IsAbs(reportedTopLevel) {
		return false
	}
	requested, requestedErr := os.Stat(requestedRoot)
	reported, reportedErr := os.Stat(reportedTopLevel)
	return requestedErr == nil && reportedErr == nil &&
		requested.IsDir() && reported.IsDir() && os.SameFile(requested, reported)
}
