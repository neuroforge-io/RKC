package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/inventory"
)

func TestCapturePreservesInventoryAndIsolatesLaterEdits(t *testing.T) {
	parent := workspaceTempDir(t)
	source := filepath.Join(parent, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "excluded"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# Before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(source, "large.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(8 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	limits := DefaultLimits()
	limits.MaxFileBytes = 1024
	limits.MaxTextBytes = 1024
	opts := inventory.Options{Root: source, MaxFiles: limits.MaxFiles, MaxFileBytes: limits.MaxFileBytes, MaxTextBytes: limits.MaxTextBytes, MaxRepositoryBytes: limits.MaxRepositoryBytes, Excludes: []string{"excluded"}}
	before, err := inventory.ScanContext(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(parent, "captured")
	if err := Capture(t.Context(), source, captured, before, limits); err != nil {
		t.Fatal(err)
	}
	opts.Root = captured
	after, err := inventory.ScanContext(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != before.Digest {
		t.Fatal("capture changed canonical inventory")
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# After\n"), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(captured, "README.md"))
	if err != nil || string(content) != "# Before\n" {
		t.Fatal("live edit changed captured source", err)
	}
	if err := Capture(t.Context(), source, filepath.Join(parent, "invalid"), before, limits); err == nil {
		t.Fatal("accepted source that changed before capture")
	}
	if _, err := os.Stat(filepath.Join(parent, "invalid")); !os.IsNotExist(err) {
		t.Fatal("failed capture left files behind")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Capture(ctx, source, filepath.Join(parent, "canceled"), before, limits); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestWorkspacePublishesVerifiedCaptureAsStaleWhenSourceAdvances(t *testing.T) {
	store := fixtureStore(t)
	if err := store.Add(fixtureSource(t)); err != nil {
		t.Fatal(err)
	}
	active := fixtureActive(store, "a")
	active.SourceAdvanced = true
	if err := store.Refresh(t.Context(), "sample", func(context.Context, Source, string) (*Active, error) { return active, nil }); err != nil {
		t.Fatal(err)
	}
	got := store.Registry.Sources[0]
	if got.Active == nil || got.Freshness.Status != "stale" || got.Freshness.ErrorCode != "source_changed" {
		t.Fatal("captured snapshot was mislabeled current")
	}
	if err := store.Refresh(t.Context(), "sample", func(context.Context, Source, string) (*Active, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if store.Registry.Sources[0].Freshness.Status != "current" {
		t.Fatal("matching later source check did not become current")
	}
}
