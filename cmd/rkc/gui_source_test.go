package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/githubsource"
	"github.com/neuroforge-io/RKC/internal/server"
)

func TestGUIArchiveKeepsReceiptWithoutInventingGitMetadata(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Account guide\nLogin opens the dashboard.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	checkout := githubsource.Checkout{Root: root, Repository: githubsource.Repository{FullName: "example/accounts", HTMLURL: "https://github.com/example/accounts"}, CommitSHA: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64)}
	if err := runGUIArchive(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	dataset, err := server.Load(filepath.Join(root, ".rkc"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := dataset.Manifest
	if snapshot.Git.Commit != "" || !snapshot.Git.Unavailable || snapshot.Git.Origin != checkout.Repository.HTMLURL || snapshot.Metadata["source_revision"] != checkout.CommitSHA || snapshot.Metadata["source_archive_sha256"] != checkout.ArchiveSHA256 || snapshot.RootPath != "" {
		t.Fatalf("archive provenance: %+v", snapshot)
	}
	bundle := dataset.Bundle
	if _, err := enforceWorkspacePrivacy(&bundle, "redacted"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"source_reference", "source_provider", "source_revision", "source_archive_sha256", "repository_origin"} {
		if bundle.Snapshot.Metadata[key] != "" {
			t.Fatalf("redacted archive exposed %s", key)
		}
	}
}
