package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltInScanOmitsGitProcessAndMarksUnavailable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Example\nA local knowledge folder.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// No executable can be resolved. This must remain a useful deterministic scan.
	t.Setenv("PATH", t.TempDir())
	opts := Options{Root: root, SkipGitInspection: true, DisablePythonAST: true}
	bundle, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Snapshot.Git.Unavailable || bundle.Snapshot.Git.Commit != "" || len(bundle.Artifacts) != 1 {
		t.Fatalf("built-in provenance: %+v", bundle.Snapshot.Git)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspectGitForScan(ctx, root, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
