package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareTraceInputsValidation(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "trace.json")
	encoded, err := json.Marshal(sealTrace(Trace{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareTraceInputs(nil, []string{valid}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, _, err := PrepareTraceInputs(context.Background(), []string{""}); err == nil {
		t.Fatal("empty path was accepted")
	}
	if _, _, err := PrepareTraceInputs(context.Background(), []string{filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("absent path was accepted")
	}
	symlink := filepath.Join(dir, "linked")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareTraceInputs(context.Background(), []string{symlink}); err == nil {
		t.Fatal("symlink was accepted")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, MaximumTraceBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareTraceInputs(context.Background(), []string{big}); err == nil {
		t.Fatal("oversized trace was accepted")
	}
	var many []string
	for index := 0; index < 65; index++ {
		many = append(many, valid)
	}
	if _, _, err := PrepareTraceInputs(context.Background(), many); err == nil {
		t.Fatal("too many traces were accepted")
	}
	// Duplicates are deduplicated deterministically.
	inputs, digest, err := PrepareTraceInputs(context.Background(), []string{valid, valid})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("duplicate inputs = %d", len(inputs))
	}
	if digest != AggregateDigest(inputs) {
		t.Fatal("aggregate digest disagrees")
	}
	copyPath := filepath.Join(dir, "same-content.json")
	if err := os.WriteFile(copyPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	inputs, _, err = PrepareTraceInputs(context.Background(), []string{valid, copyPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("content-identical traces = %d, want 1", len(inputs))
	}
}

func TestLoadTraceValidation(t *testing.T) {
	dir := t.TempDir()
	encoded, err := json.Marshal(sealTrace(Trace{}))
	if err != nil {
		t.Fatal(err)
	}
	content := string(encoded)
	path := filepath.Join(dir, "trace.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	input := TraceInput{Path: path, SHA256: strings.Repeat("c", 64), SizeBytes: int64(len(content))}
	if _, err := LoadTrace(context.Background(), input); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	input.SHA256 = sha256OfFile(t, path)
	if _, err := LoadTrace(context.Background(), input); err != nil {
		t.Fatalf("valid trace rejected: %v", err)
	}
	input.SHA256 = ""
	if _, err := LoadTrace(context.Background(), input); err == nil {
		t.Fatal("missing digest was accepted")
	}
	if _, err := LoadTrace(nil, input); err == nil {
		t.Fatal("nil context was accepted")
	}
	changed := filepath.Join(dir, "changed.json")
	if err := os.WriteFile(changed, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte(content+" "), 0o600); err != nil {
		t.Fatal(err)
	}
	input2 := TraceInput{Path: changed, SHA256: strings.Repeat("d", 64), SizeBytes: int64(len(content))}
	if _, err := LoadTrace(context.Background(), input2); err == nil {
		t.Fatal("size mismatch was accepted")
	}
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256OfFile(t, garbage)
	if _, err := LoadTrace(context.Background(), TraceInput{Path: garbage, SHA256: sum, SizeBytes: int64(len("not json"))}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestTraceInputDigestSeparatesCaptureIntegrity(t *testing.T) {
	dir := t.TempDir()
	trace := sealTrace(Trace{})
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trace.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	unverified, unverifiedDigest, err := PrepareTraceInputs(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if unverified[0].captureIntegrityAuthenticated {
		t.Fatal("external trace unexpectedly gained same-process capture integrity")
	}
	markCurrentProcessCaptureIntegrity(trace)
	authenticated, authenticatedDigest, err := PrepareTraceInputs(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated[0].captureIntegrityAuthenticated {
		t.Fatal("current-process capture-integrity marker was not restored")
	}
	if authenticatedDigest == unverifiedDigest {
		t.Fatal("trace cache identity ignored capture-integrity authentication")
	}
	loaded, err := LoadTrace(context.Background(), authenticated[0])
	if err != nil {
		t.Fatal(err)
	}
	if !authenticatedCaptureIntegrity(loaded) {
		t.Fatal("prepared trace lost its bound capture integrity")
	}
	loaded.DurationMS++
	loaded.ID = IDFor(loaded)
	if authenticatedCaptureIntegrity(loaded) {
		t.Fatal("mutated trace retained capture-integrity authentication")
	}
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
