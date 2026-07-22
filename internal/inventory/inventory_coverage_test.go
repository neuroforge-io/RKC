package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestInventoryCoverageFileLimitsSpecialObjectsAndFileExclusions(t *testing.T) {
	root := t.TempDir()
	payload := []byte("larger than the direct file limit")
	largePath := filepath.Join(root, "large.txt")
	mustWriteInventoryFile(t, largePath, payload)
	artifact, diagnostic := inspectFile(largePath, "large.txt", int64(len(payload)), 4, 1024)
	if diagnostic != nil || artifact.Status != "oversized" || artifact.ExclusionReason != "file_exceeds_limit:4" {
		t.Fatalf("inspectFile(file limit) = %+v, %+v", artifact, diagnostic)
	}
	if likelyText([]byte{0xff, 1, 'x'}) {
		t.Fatal("likelyText classified control-heavy invalid UTF-8 as text")
	}

	excludedPath := filepath.Join(root, "excluded.txt")
	mustWriteInventoryFile(t, excludedPath, []byte("excluded"))
	socketPath := filepath.Join(root, "special.sock")
	listener, listenErr := net.Listen("unix", socketPath)
	if listenErr == nil {
		t.Cleanup(func() { _ = listener.Close() })
	}

	result, err := Scan(Options{Root: root, MaxFileBytes: 1024, Excludes: []string{"excluded.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(result.Artifacts))
	for _, item := range result.Artifacts {
		statuses[item.Path] = item.Status
	}
	if statuses["excluded.txt"] != "excluded" {
		t.Fatalf("excluded file status = %q", statuses["excluded.txt"])
	}
	if listenErr == nil && statuses["special.sock"] != "excluded" {
		t.Fatalf("special object status = %q", statuses["special.sock"])
	}
}

func TestScanClassifiesFilesAndEnforcesLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "empty.txt"), nil)
	mustWriteInventoryFile(t, filepath.Join(root, "source.go"), []byte("package fixture\n\nfunc Answer() int { return 42 }\n"))
	mustWriteInventoryFile(t, filepath.Join(root, "binary.dat"), []byte{'a', 0, 'b'})
	mustWriteInventoryFile(t, filepath.Join(root, "invalid.txt"), []byte{0xff, 0xfe, 'x'})
	mustWriteInventoryFile(t, filepath.Join(root, "large.md"), []byte(strings.Repeat("x", 64)))
	if err := os.Symlink("source.go", filepath.Join(root, "source-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteInventoryFile(t, filepath.Join(root, "vendor", "ignored.go"), []byte("package ignored\n"))

	result, err := Scan(Options{Root: root, MaxFileBytes: 128, MaxTextBytes: 32, Excludes: []string{"./vendor/", "vendor"}})
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]string, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		byPath[artifact.Path] = artifact.Status
	}
	wants := map[string]string{
		"binary.dat":  "binary",
		"empty.txt":   "text",
		"invalid.txt": "binary",
		"large.md":    "oversized",
		"source-link": "recorded",
		"source.go":   "oversized",
		"vendor":      "excluded",
	}
	for path, want := range wants {
		if got := byPath[path]; got != want {
			t.Errorf("status for %s = %q, want %q (all=%v)", path, got, want, byPath)
		}
	}
	if result.Digest == "" {
		t.Fatal("empty inventory digest")
	}

	if _, err := Scan(Options{Root: root, MaxFiles: 1}); err == nil || !strings.Contains(err.Error(), "path limit") {
		t.Fatalf("expected path limit error, got %v", err)
	}
	if _, err := Scan(Options{Root: root, MaxRepositoryBytes: 1}); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("expected byte limit error, got %v", err)
	}
}

func TestGeneratedOutputsAreExcludedAndInvariant(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "main.go"), []byte("package main\n"))
	generated := filepath.Join(root, "atlas")
	tx, err := safeoutput.Begin(generated, root, false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInventoryFile(t, filepath.Join(tx.Staging, "generation.txt"), []byte("first\n"))
	writeInventoryAtlasManifest(t, tx.Staging, "snapshot-one", "generation.txt")
	if err := tx.Commit("snapshot-one"); err != nil {
		t.Fatal(err)
	}

	first, err := Scan(Options{Root: root, Excludes: []string{"atlas"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInventoryFile(t, filepath.Join(generated, "generation.txt"), []byte("second output must never recurse\n"))
	mustWriteInventoryFile(t, filepath.Join(generated, "nested", "self.md"), []byte("# generated\n"))
	second, err := Scan(Options{Root: root, Excludes: []string{"atlas"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("generated output changed inventory digest: %s != %s", first.Digest, second.Digest)
	}
	if len(first.Artifacts) != 2 || first.Artifacts[0].Path != "atlas" || first.Artifacts[0].Generated || first.Artifacts[0].ExclusionReason != "policy_exclude:atlas" {
		t.Fatalf("generated tree was not represented exactly once: %+v", first.Artifacts)
	}
	if _, err := Scan(Options{Root: generated}); err == nil || !strings.Contains(err.Error(), "RKC-generated") {
		t.Fatalf("expected generated-root refusal, got %v", err)
	}
}

func TestReservedMarkerPresenceFailsClosed(t *testing.T) {
	t.Parallel()

	markers := []struct {
		name string
		body string
	}{
		{name: "malformed", body: "{"},
		{name: "version-skewed", body: `{"schema_version":"99.0","producer":"rkc","kind":"atlas"}`},
		{name: "forged-valid", body: `{"schema_version":"1.0","producer":"rkc","kind":"atlas","snapshot_id":"forged"}`},
	}
	for _, test := range markers {
		t.Run("root-"+test.name, func(t *testing.T) {
			root := t.TempDir()
			mustWriteInventoryFile(t, filepath.Join(root, safeoutput.MarkerName), []byte(test.body))
			mustWriteInventoryFile(t, filepath.Join(root, "generated.md"), []byte("must not be processed\n"))
			if _, err := Scan(Options{Root: root}); err == nil || !strings.Contains(err.Error(), "RKC-generated") {
				t.Fatalf("Scan(marker-bearing root) = %v, want fail-closed refusal", err)
			}
		})
	}

	for _, test := range markers {
		t.Run("subtree-"+test.name, func(t *testing.T) {
			root := t.TempDir()
			mustWriteInventoryFile(t, filepath.Join(root, "main.go"), []byte("package main\n"))
			generated := filepath.Join(root, "generated")
			mustWriteInventoryFile(t, filepath.Join(generated, safeoutput.MarkerName), []byte(test.body))
			mustWriteInventoryFile(t, filepath.Join(generated, "recursive.md"), []byte("first generated output\n"))
			if _, err := Scan(Options{Root: root}); err == nil || !strings.Contains(err.Error(), "untrusted repository subtree") {
				t.Fatalf("Scan(nested marker) = %v, want fail-closed refusal", err)
			}
			excluded, err := Scan(Options{Root: root, Excludes: []string{"generated"}})
			if err != nil {
				t.Fatalf("Scan(explicitly excluded generated output) = %v", err)
			}
			if len(excluded.Artifacts) != 2 || excluded.Artifacts[0].Path != "generated" || excluded.Artifacts[0].ExclusionReason != "policy_exclude:generated" {
				t.Fatalf("explicit exclusion was not represented exactly once: %+v", excluded.Artifacts)
			}
		})
	}
}

func TestInventoryHelpersAndInvalidRoots(t *testing.T) {
	t.Parallel()
	if _, err := Scan(Options{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("expected missing-root error")
	}
	file := filepath.Join(t.TempDir(), "file")
	mustWriteInventoryFile(t, file, []byte("x"))
	if _, err := Scan(Options{Root: file}); err == nil {
		t.Fatal("expected non-directory error")
	}
	if !likelyText(nil) || likelyText([]byte{'x', 0}) || countLines(nil) != 0 || countLines([]byte("a\nb\n")) != 2 {
		t.Fatal("text helper classification mismatch")
	}
	if reason, ok := exclusionReason("vendor/x.go", map[string]struct{}{"vendor": {}}); !ok || reason != "policy_exclude:vendor" {
		t.Fatalf("unexpected exclusion: %q %v", reason, ok)
	}
	if reason, ok := exclusionReason("vendored/x.go", map[string]struct{}{"vendor": {}}); ok || reason != "" {
		t.Fatalf("unexpected prefix exclusion: %q %v", reason, ok)
	}
	if got := detectMediaType("unknown.rkc-no-such-extension"); got != "application/octet-stream" {
		t.Fatalf("fallback media type = %q", got)
	}
	if got := DetectLanguage("unknown.rkc-no-such-extension"); got != "" {
		t.Fatalf("unknown language = %q", got)
	}
	d := diagnostic("warning", "RKC-TEST", "message", "path", "stage")
	if d.ID == "" || d.Source == nil || d.Source.Path != "path" {
		t.Fatalf("bad diagnostic: %+v", d)
	}
}

func mustWriteInventoryFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeInventoryAtlasManifest(t *testing.T, root, snapshotID, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	manifest := struct {
		SchemaVersion string `json:"schema_version"`
		SnapshotID    string `json:"snapshot_id"`
		Files         []struct {
			Path      string `json:"path"`
			SizeBytes int64  `json:"size_bytes"`
			SHA256    string `json:"sha256"`
		} `json:"files"`
	}{SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: snapshotID}
	manifest.Files = append(manifest.Files, struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}{Path: name, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInventoryFile(t, filepath.Join(root, "rkc-export-manifest.json"), append(encoded, '\n'))
}
