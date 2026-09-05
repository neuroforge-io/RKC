package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestWorkspaceGenerationReadersProtectRetention(t *testing.T) {
	store := fixtureStore(t)
	parent := filepath.Join(store.root, "generations")
	paths := make([]string, 3)
	for i := range paths {
		path, err := CreateGeneration(parent, "sample")
		if err != nil {
			t.Fatal(err)
		}
		paths[i] = path
		if err := os.Mkdir(filepath.Join(path, "atlas"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	active := func(index int) *Active {
		a := fixtureActive(store, "a")
		a.Generation = filepath.Base(paths[index])
		a.AtlasPath = filepath.Join(paths[index], "atlas")
		return a
	}
	source := fixtureSource(t)
	source.Active = active(2)
	source.Previous = active(1)
	source.Freshness.Status = "current"
	store.Registry.Sources = []Source{source}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireActive(*active(0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireActive(*active(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed pinned or retained generation", err)
		}
	}
	first.Close()
	second.Close()
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("obsolete generation not removed", err)
	}
	for _, path := range paths[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("retained generation missing", err)
		}
	}
	if _, err := AcquireActive(*active(0)); err == nil {
		t.Fatal("removed generation acquired")
	}
	store.Close()
	if err := store.Prune(); err == nil {
		t.Fatal("closed writer pruned")
	}
}

func TestWorkspaceGenerationOwnershipAndLeaseFailures(t *testing.T) {
	store := fixtureStore(t)
	parent := filepath.Join(store.root, "generations")
	if _, err := CreateGeneration(parent, "../bad"); err == nil {
		t.Fatal("unsafe source alias accepted")
	}
	if _, err := CreateGeneration(filepath.Join(parent, "missing"), "sample"); err == nil {
		t.Fatal("missing parent accepted")
	}
	path, err := CreateGeneration(parent, "sample")
	if err != nil {
		t.Fatal(err)
	}
	active := Active{AtlasPath: filepath.Join(path, "atlas"), Generation: filepath.Base(path)}
	bad := active
	bad.Generation = "other"
	if _, err := AcquireActive(bad); err == nil {
		t.Fatal("mismatched active accepted")
	}
	if _, err := acquireGeneration(filepath.Join(parent, "notcanonical"), false); err == nil {
		t.Fatal("unowned generation accepted")
	}
	marker := filepath.Join(path, "generation.txt")
	if err := os.WriteFile(marker, []byte("unowned"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireActive(active); err == nil {
		t.Fatal("invalid marker accepted")
	}
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unowned directory removed")
	}
	if err := os.WriteFile(marker, []byte(SchemaVersion+"\n"+filepath.Base(path)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "readers.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireActive(active); err == nil {
		t.Fatal("missing lease accepted")
	}
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unprovable directory removed")
	}
	identity, err := privatepath.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := privatepath.CheckDir(path, identity); err != nil {
		t.Fatal(err)
	}
}
