package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/githubsource"
)

func TestGUIArchiveCleanupDoesNotRetainSourceTextInSharedCache(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("PATH", t.TempDir())
	const marker = "RetainedSourceAuditSentinelVividPurpleAntelope"
	paragraph := marker + " documents a source paragraph that must leave with its canceled job."
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Source audit\n\n"+paragraph+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := githubsource.Checkout{
		Root:       root,
		Repository: githubsource.Repository{FullName: "example/source", HTMLURL: "https://github.com/example/source"},
		CommitSHA:  strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64),
	}
	if err := runGUIArchive(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	// Cancellation may arrive after successful compilation but before atlas
	// activation. The server then removes the acquisition directory. Operational
	// scheduler receipts can remain, but source paragraphs must not remain in a
	// shared stage payload cache outside that removed directory.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), marker) {
			t.Errorf("source text remains outside the removed job directory: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(cache, "rkc", "stages")); err == nil && len(entries) != 0 {
		t.Fatal("GitHub compilation unexpectedly populated the shared stage cache")
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
