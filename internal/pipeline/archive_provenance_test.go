package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func archiveFixtureOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Source fixture\n\nThis public fixture documents the Example function.\n")
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package example\n\nfunc Example() bool { return true }\n")
	return Options{
		Root: root, StageWorkers: 1, ToolVersion: "test", DisablePythonAST: true,
		SkipGitInspection: true, Origin: "https://github.com/example/source",
		ArchiveProvenance: &ArchiveProvenance{Provider: "github", Revision: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64)},
	}
}

func TestArchiveProvenanceMatchesStagedAndSequentialWithoutGit(t *testing.T) {
	options := archiveFixtureOptions(t)
	// Every external command lookup fails; both compiler paths must remain
	// deterministic in-process operations for this archive acquisition profile.
	t.Setenv("PATH", t.TempDir())
	staged, coverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	sequential, oracleCoverage, err := scanSequential(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	stagedJSON, err := rkcmodel.CanonicalJSON(staged)
	if err != nil {
		t.Fatal(err)
	}
	sequentialJSON, err := rkcmodel.CanonicalJSON(sequential)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stagedJSON, sequentialJSON) || !reflect.DeepEqual(coverage, oracleCoverage) {
		t.Fatal("archive compiler paths disagree")
	}
	snapshot := staged.Snapshot
	if !snapshot.Git.Unavailable || snapshot.Git.Commit != "" || snapshot.Git.Branch != "" || snapshot.Git.Dirty || snapshot.Git.WorkingTreeDigest != "" {
		t.Fatalf("archive claimed verified Git working-tree metadata: %+v", snapshot.Git)
	}
	assertCanonicalOriginBundle(t, staged, options.Origin)
	if snapshot.Metadata["source_provider"] != "github" || snapshot.Metadata["source_revision"] != options.ArchiveProvenance.Revision || snapshot.Metadata["source_archive_sha256"] != options.ArchiveProvenance.ArchiveSHA256 {
		t.Fatal("archive provenance receipt missing from snapshot")
	}
	for _, node := range staged.Nodes {
		if node.Kind == "repository" && node.Attributes["git_commit"] != "" {
			t.Fatal("repository node claimed a Git commit")
		}
	}
	options.ArchiveProvenance.Revision = strings.Repeat("c", 40)
	next, _, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if next.Snapshot.RepositoryID != snapshot.RepositoryID || next.Snapshot.ID == snapshot.ID || next.Snapshot.ContentDigest != snapshot.ContentDigest {
		t.Fatal("revision change did not preserve source identity and distinguish snapshot provenance")
	}
}

func TestArchiveProvenanceIdentityPreservesNilHistory(t *testing.T) {
	options := Options{ConfigDigest: "config", PolicyDigest: "policy", PluginLockDigest: "plugins", ToolchainDigest: "toolchain"}
	expected := rkcmodel.StableID("snapshot", "repository", "commit", "inventory", "scip", "trace-input", "trace", "history-input", "history", "config", "policy", "plugins", "toolchain", rkcmodel.SchemaVersion)
	legacy := stableSnapshotID("repository", "commit", "inventory", "scip", "trace", "history", options)
	if legacy != expected {
		t.Fatal("nil archive provenance changed existing snapshot identity")
	}
	options.ArchiveProvenance = &ArchiveProvenance{Provider: "github", Revision: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64)}
	first := stableSnapshotID("repository", "commit", "inventory", "scip", "trace", "history", options)
	options.ArchiveProvenance.ArchiveSHA256 = strings.Repeat("c", 64)
	second := stableSnapshotID("repository", "commit", "inventory", "scip", "trace", "history", options)
	if first == legacy || second == first {
		t.Fatal("archive receipt was omitted from snapshot identity")
	}
}

func TestArchiveProvenanceRejectsInvalidOrCredentialedReceiptsBeforeReadingSource(t *testing.T) {
	mutations := map[string]func(*Options){
		"git enabled":                func(o *Options) { o.SkipGitInspection = false },
		"provider":                   func(o *Options) { o.ArchiveProvenance.Provider = "other" },
		"branch instead of revision": func(o *Options) { o.ArchiveProvenance.Revision = "main" },
		"uppercase revision":         func(o *Options) { o.ArchiveProvenance.Revision = strings.Repeat("A", 40) },
		"uppercase digest":           func(o *Options) { o.ArchiveProvenance.ArchiveSHA256 = strings.Repeat("B", 64) },
		"missing digest":             func(o *Options) { o.ArchiveProvenance.ArchiveSHA256 = "" },
		"missing origin":             func(o *Options) { o.Origin = "" },
		"foreign origin":             func(o *Options) { o.Origin = "https://example.com/owner/repo" },
		"credential origin": func(o *Options) {
			o.Origin = strings.Join([]string{"https", ":/", "/user:", "PRIVATE_SECRET", "@github.com/example/source"}, "")
		},
		"query origin":    func(o *Options) { o.Origin += "?token=PRIVATE_SECRET" },
		"fragment origin": func(o *Options) { o.Origin += "#PRIVATE_SECRET" },
		"branch URL":      func(o *Options) { o.Origin += "/tree/main" },
		"port URL":        func(o *Options) { o.Origin = "https://github.com:443/example/source" },
		"ssh URL":         func(o *Options) { o.Origin = "ssh://github.com/example/source" },
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			options := Options{Root: filepath.Join(t.TempDir(), "does-not-exist"), SkipGitInspection: true, Origin: "https://github.com/example/source", ArchiveProvenance: &ArchiveProvenance{Provider: "github", Revision: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64)}}
			mutation(&options)
			for _, scan := range []func(context.Context, Options) (rkcmodel.Bundle, rkcmodel.Coverage, error){Scan, scanSequential} {
				bundle, _, err := scan(context.Background(), options)
				if err == nil || !strings.Contains(err.Error(), "archive provenance") || strings.Contains(err.Error(), "PRIVATE_SECRET") || bundle.Snapshot.ID != "" {
					t.Fatalf("invalid receipt or input disclosure: %v", err)
				}
			}
		})
	}
}

func TestArchiveReceiptIsCopiedAndSupportsSHA256Revisions(t *testing.T) {
	options := archiveFixtureOptions(t)
	options.ArchiveProvenance.Revision = strings.Repeat("a", 64)
	callerReceipt := options.ArchiveProvenance
	if err := validateArchiveOptions(&options); err != nil {
		t.Fatal(err)
	}
	callerReceipt.Revision = "changed"
	if options.ArchiveProvenance.Revision != strings.Repeat("a", 64) {
		t.Fatal("pipeline retained caller receipt pointer")
	}
	options.ArchiveProvenance = nil
	bundle, _, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(bundle.Snapshot.Metadata)
	for _, key := range []string{"source_provider", "source_revision", "source_archive_sha256"} {
		if bytes.Contains(data, []byte(key)) {
			t.Fatal("nil receipt emitted archive claims")
		}
	}
	if _, err := os.Stat(options.Root); err != nil {
		t.Fatal(err)
	}
}
