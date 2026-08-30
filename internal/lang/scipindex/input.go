package scipindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MaximumIndexBytes bounds each SCIP file admitted for hashing and import.
	MaximumIndexBytes = int64(512 << 20)
	// MaximumTotalIndexBytes bounds the aggregate prepared SCIP input set.
	MaximumTotalIndexBytes = int64(1 << 30)
	// MaximumIndexCount bounds distinct SCIP files in one scan.
	MaximumIndexCount = 64
)

// Input is an absolute, regular-file SCIP input bound to its exact size and
// SHA-256 digest. Callers must revalidate it with VerifyInputs before import.
type Input struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// PrepareInputs canonicalizes, deduplicates, bounds, and hashes SCIP paths. It
// returns path-sorted inputs and a stable aggregate digest over their content
// digests and sizes; an empty path set returns an empty result and digest.
func PrepareInputs(ctx context.Context, paths []string) ([]Input, string, error) {
	if ctx == nil {
		return nil, "", errors.New("SCIP input context is required")
	}
	if len(paths) == 0 {
		return nil, "", nil
	}
	if len(paths) > MaximumIndexCount {
		return nil, "", fmt.Errorf(
			"%d SCIP indexes requested; maximum is %d",
			len(paths), MaximumIndexCount,
		)
	}
	seen := map[string]struct{}{}
	inputs := make([]Input, 0, len(paths))
	var totalBytes int64
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		path = strings.TrimSpace(path)
		if path == "" || strings.IndexByte(path, 0) >= 0 {
			return nil, "", errors.New("SCIP index path is empty or invalid")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, "", fmt.Errorf("resolve SCIP index: %w", err)
		}
		absolute = filepath.Clean(absolute)
		if _, ok := seen[absolute]; ok {
			return nil, "", fmt.Errorf("duplicate SCIP index path %q", absolute)
		}
		seen[absolute] = struct{}{}
		input, err := inspectAndDigest(ctx, absolute)
		if err != nil {
			return nil, "", err
		}
		totalBytes += input.SizeBytes
		if totalBytes > MaximumTotalIndexBytes {
			return nil, "", fmt.Errorf(
				"SCIP indexes total %d bytes; maximum is %d",
				totalBytes, MaximumTotalIndexBytes,
			)
		}
		inputs = append(inputs, input)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	hasher := sha256.New()
	for _, input := range inputs {
		_, _ = io.WriteString(hasher, input.SHA256)
		_, _ = io.WriteString(hasher, "\x00")
		_, _ = io.WriteString(hasher, fmt.Sprintf("%d", input.SizeBytes))
		_, _ = io.WriteString(hasher, "\n")
	}
	return inputs, hex.EncodeToString(hasher.Sum(nil)), nil
}

// VerifyInputs reopens and rehashes every prepared input, failing if path,
// identity, size, or content changed between admission and extraction.
func VerifyInputs(ctx context.Context, expected []Input) error {
	paths := make([]string, 0, len(expected))
	for _, input := range expected {
		paths = append(paths, input.Path)
	}
	actual, _, err := PrepareInputs(ctx, paths)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("SCIP input set changed")
	}
	expectedByPath := make(map[string]Input, len(expected))
	for _, input := range expected {
		expectedByPath[input.Path] = input
	}
	for _, input := range actual {
		want, ok := expectedByPath[input.Path]
		if !ok || want.SHA256 != input.SHA256 || want.SizeBytes != input.SizeBytes {
			return fmt.Errorf("SCIP index %q changed during the scan", input.Path)
		}
	}
	return nil
}

func inspectAndDigest(ctx context.Context, path string) (Input, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return Input{}, fmt.Errorf("inspect SCIP index %q: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return Input{}, fmt.Errorf("SCIP index %q must be a regular file, not a symlink", path)
	}
	if before.Size() > MaximumIndexBytes {
		return Input{}, fmt.Errorf(
			"SCIP index %q is %d bytes; maximum is %d",
			path, before.Size(), MaximumIndexBytes,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return Input{}, fmt.Errorf("open SCIP index %q: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Input{}, fmt.Errorf("inspect opened SCIP index %q: %w", path, err)
	}
	if !sameFileSnapshot(before, opened) {
		return Input{}, fmt.Errorf("SCIP index %q changed while opening", path)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, &contextReader{ctx: ctx, reader: file}); err != nil {
		return Input{}, fmt.Errorf("hash SCIP index %q: %w", path, err)
	}
	after, err := os.Lstat(path)
	if err != nil || !sameFileSnapshot(before, after) {
		return Input{}, fmt.Errorf("SCIP index %q changed while hashing", path)
	}
	return Input{
		Path: path, SHA256: hex.EncodeToString(hasher.Sum(nil)), SizeBytes: before.Size(),
	}, nil
}

func sameFileSnapshot(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) &&
		before.Mode() == after.Mode() && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime())
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read checks cancellation before delegating a hashing read, preventing a
// canceled preparation pass from continuing through another input chunk.
func (reader *contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}
