package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaximumTraceInputBytes bounds the aggregate memory exposed by one import.
const MaximumTraceInputBytes = 256 << 20

// TraceInput is an absolute, regular-file trace bound to its exact size and
// SHA-256 digest.
type TraceInput struct {
	Path                          string `json:"path"`
	SHA256                        string `json:"sha256"`
	SizeBytes                     int64  `json:"size_bytes"`
	captureIntegrityAuthenticated bool
	integrityAuthenticatedTraceID string
}

// PrepareTraceInputs canonicalizes, deduplicates, bounds, and hashes trace
// paths. It returns path-sorted inputs and a stable aggregate digest; an
// empty path set returns an empty result and digest.
func PrepareTraceInputs(ctx context.Context, paths []string) ([]TraceInput, string, error) {
	if ctx == nil {
		return nil, "", errors.New("trace input context is required")
	}
	if len(paths) == 0 {
		return nil, "", nil
	}
	if len(paths) > 64 {
		return nil, "", fmt.Errorf("%d traces requested; maximum is 64", len(paths))
	}
	seen := map[string]struct{}{}
	seenDigests := map[string]struct{}{}
	inputs := make([]TraceInput, 0, len(paths))
	var totalBytes int64
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		path = strings.TrimSpace(path)
		if path == "" || strings.IndexByte(path, 0) >= 0 {
			return nil, "", errors.New("trace path is empty or invalid")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, "", fmt.Errorf("resolve trace path: %w", err)
		}
		if _, duplicate := seen[absolute]; duplicate {
			continue
		}
		seen[absolute] = struct{}{}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, "", fmt.Errorf("inspect trace %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("trace %q must be a real regular file, not a symlink", path)
		}
		if info.Size() > MaximumTraceBytes {
			return nil, "", fmt.Errorf("trace %q is %d bytes; maximum is %d", path, info.Size(), MaximumTraceBytes)
		}
		digest, observedSize := fileDigest(absolute)
		if digest == "" {
			return nil, "", fmt.Errorf("hash trace %q", path)
		}
		after, err := os.Lstat(absolute)
		if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || after.Size() != info.Size() || observedSize != info.Size() {
			return nil, "", fmt.Errorf("trace %q changed while it was prepared", path)
		}
		if _, duplicate := seenDigests[digest]; duplicate {
			continue
		}
		seenDigests[digest] = struct{}{}
		input := TraceInput{Path: absolute, SHA256: digest, SizeBytes: info.Size()}
		totalBytes += info.Size()
		if totalBytes > MaximumTraceInputBytes {
			return nil, "", fmt.Errorf("trace inputs total %d bytes; maximum is %d", totalBytes, MaximumTraceInputBytes)
		}
		trace, err := LoadTrace(ctx, input)
		if err != nil {
			return nil, "", err
		}
		input.integrityAuthenticatedTraceID = trace.ID
		input.captureIntegrityAuthenticated = registeredCurrentProcessCaptureIntegrity(trace.ID)
		inputs = append(inputs, input)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	return inputs, AggregateDigest(inputs), nil
}

// LoadTrace reads, decodes, and validates one prepared trace input. The file
// must still match its prepared digest at read time.
func LoadTrace(ctx context.Context, input TraceInput) (Trace, error) {
	if ctx == nil {
		return Trace{}, errors.New("trace load context is required")
	}
	before, err := os.Lstat(input.Path)
	if err != nil {
		return Trace{}, fmt.Errorf("inspect trace %q: %w", input.Path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() != input.SizeBytes {
		return Trace{}, fmt.Errorf("trace %q no longer matches its prepared input", input.Path)
	}
	data, err := os.ReadFile(input.Path)
	if err != nil {
		return Trace{}, fmt.Errorf("read trace %q: %w", input.Path, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != input.SHA256 {
		return Trace{}, fmt.Errorf("trace %q digest changed", input.Path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var trace Trace
	if err := decoder.Decode(&trace); err != nil {
		return Trace{}, fmt.Errorf("decode trace %q: %w", input.Path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Trace{}, fmt.Errorf("decode trace %q: trailing JSON content", input.Path)
	}
	if err := Validate(trace); err != nil {
		return Trace{}, fmt.Errorf("trace %q is invalid: %w", input.Path, err)
	}
	if input.integrityAuthenticatedTraceID != "" && input.integrityAuthenticatedTraceID != trace.ID {
		return Trace{}, fmt.Errorf("trace %q capture-integrity identity changed", input.Path)
	}
	trace.captureIntegrityBound = true
	trace.captureIntegrityAuthenticated = input.captureIntegrityAuthenticated
	trace.integrityAuthenticatedID = trace.ID
	return trace, nil
}

// AggregateDigest returns the stable digest over prepared inputs.
func AggregateDigest(inputs []TraceInput) string {
	if len(inputs) == 0 {
		return ""
	}
	digests := make([]string, 0, len(inputs))
	for _, input := range inputs {
		integrity := "portable-record"
		if input.captureIntegrityAuthenticated {
			integrity = "current-process-capture-integrity"
		}
		digests = append(digests, input.SHA256+"\x00"+integrity)
	}
	sort.Strings(digests)
	aggregate := sha256.Sum256([]byte(strings.Join(digests, "\n") + "\n"))
	return hex.EncodeToString(aggregate[:])
}
