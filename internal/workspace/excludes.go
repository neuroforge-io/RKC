package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const MaximumExcludePatterns = 128

// ValidateExcludePatterns accepts slash-separated Go path.Match patterns,
// extended with ** as a whole segment matching zero or more path segments.
// Patterns are rooted at the registered source, never at the host filesystem.
func ValidateExcludePatterns(patterns []string) error {
	if len(patterns) > MaximumExcludePatterns {
		return errors.New("too many workspace exclusion patterns")
	}
	seen := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		if !safeText(pattern, 1024) || strings.ContainsAny(pattern, `\:`) || strings.HasPrefix(pattern, "/") || path.Clean(pattern) != pattern || seen[pattern] {
			return errors.New("workspace exclusion patterns must be unique canonical relative slash patterns")
		}
		seen[pattern] = true
		for _, segment := range strings.Split(pattern, "/") {
			if segment == "." || segment == ".." || segment == "" || (segment != "**" && strings.Contains(segment, "**")) {
				return errors.New("workspace exclusion patterns require ** to occupy a whole path segment")
			}
			if _, err := path.Match(segment, ""); err != nil {
				return errors.New("invalid workspace exclusion pattern")
			}
		}
	}
	return nil
}

// ResolveExclusions expands an operator's explicit patterns using only bounded
// filesystem metadata. It never opens source files, follows symlinks, or reads
// ignore rules supplied by the source. Matching directories are pruned and
// represented by one exact exclusion so inventory can account for them.
func ResolveExclusions(ctx context.Context, root string, exact, patterns []string, maxPaths int) ([]string, error) {
	if ctx == nil || maxPaths < 1 || maxPaths > 500000 {
		return nil, errors.New("workspace exclusion resolution requires a context and bounded path limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateExcludePatterns(patterns); err != nil {
		return nil, err
	}
	if err := rejectSymlinks(root); err != nil {
		return nil, err
	}
	resolved := make(map[string]bool, len(exact))
	for _, value := range exact {
		resolved[value] = true
	}
	if len(patterns) != 0 {
		segments := make([][]string, len(patterns))
		for i, pattern := range patterns {
			segments[i] = strings.Split(pattern, "/")
		}
		visited := 0
		err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, current)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if relative == "." {
				return nil
			}
			visited++
			if visited > maxPaths {
				return fmt.Errorf("workspace exclusion path limit exceeded: %d", maxPaths)
			}
			matched := resolved[relative]
			if !matched {
				parts := strings.Split(relative, "/")
				for _, pattern := range segments {
					if matchExcludePattern(pattern, parts) {
						matched = true
						break
					}
				}
			}
			if matched {
				resolved[relative] = true
				if entry.IsDir() {
					return fs.SkipDir
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(resolved))
	for value := range resolved {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func matchExcludePattern(pattern, parts []string) bool {
	// Dynamic programming prevents adjacent recursive wildcards from creating
	// exponential work on deeply nested source paths.
	previous := make([]bool, len(parts)+1)
	previous[0] = true
	for _, segment := range pattern {
		next := make([]bool, len(parts)+1)
		if segment == "**" {
			next[0] = previous[0]
			for j := 1; j <= len(parts); j++ {
				next[j] = previous[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				if previous[j-1] {
					next[j], _ = path.Match(segment, parts[j-1])
				}
			}
		}
		previous = next
	}
	return previous[len(parts)]
}
