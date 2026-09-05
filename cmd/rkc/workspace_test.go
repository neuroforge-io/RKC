package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/workspace"
)

func TestWorkspaceCLICompilesRefreshesAndRetainsLastGood(t *testing.T) {
	parent := workspaceTestTempDir(t)
	source := filepath.Join(parent, "source")
	root := filepath.Join(parent, "workspace")
	writeTestFile(t, filepath.Join(source, "main.go"), "package example\n\n// Value returns a number.\nfunc Value() int { return 1 }\n")
	writeTestFile(t, filepath.Join(source, "README.md"), "# Example\n\nA public synthetic fixture.\n")
	_, err := captureStdout(t, func() error { return runWorkspaceAdd([]string{"--workspace", root, "--id", "sample", source}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	syncSource := func() {
		t.Helper()
		_, err := captureStdout(t, func() error { return runWorkspaceSyncContext(t.Context(), []string{"sync", "--workspace", root}) })
		if err != nil {
			t.Fatal(err)
		}
	}
	syncSource()
	first, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	initial := first.Sources[0].Active
	if initial == nil || first.Sources[0].Freshness.Status != "current" {
		t.Fatal("initial atlas was not activated")
	}
	lease, err := workspace.AcquireActive(*initial)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	syncSource()
	same, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if same.Sources[0].Active.Generation != initial.Generation {
		t.Fatal("unchanged source rebuilt")
	}
	writeTestFile(t, filepath.Join(source, "untracked.md"), "# New document\n\nUncommitted files are admitted.\n")
	syncSource()
	second, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Sources[0].Active.Generation == initial.Generation || second.Sources[0].Previous.Generation != initial.Generation {
		t.Fatal("untracked file did not advance generation")
	}
	writeTestFile(t, filepath.Join(source, "README.md"), "# Changed example\n\nNew content.\n")
	syncSource()
	third, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(initial.AtlasPath); err != nil {
		t.Fatal("pinned old generation removed", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	syncSource()
	if _, err := os.Stat(initial.AtlasPath); !os.IsNotExist(err) {
		t.Fatal("unpinned obsolete generation was not pruned", err)
	}
	writeTestFile(t, filepath.Join(source, "main.go"), "package example\nfunc invalid( {\n")
	_, err = captureStdout(t, func() error { return runWorkspaceSyncContext(t.Context(), []string{"sync", "--workspace", root}) })
	if err == nil {
		t.Fatal("broken syntax accepted")
	}
	failed, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Sources[0].Active.Generation != third.Sources[0].Active.Generation || failed.Sources[0].Freshness.Status != "error" {
		t.Fatal("failed refresh replaced active source")
	}
	output, err := captureStdout(t, func() error { return runWorkspaceList([]string{"--workspace", root}) })
	if err != nil || !strings.Contains(output, "sample\terror") {
		t.Fatal(output, err)
	}
	output, err = captureStdout(t, func() error { return runWorkspaceList([]string{"--workspace", root, "--json"}) })
	if err != nil || !strings.Contains(output, `"schema_version":"rkc-workspace/v1"`) {
		t.Fatal(output, err)
	}
}

func TestWorkspaceFingerprintSeesContentsAndAdmissionChanges(t *testing.T) {
	root := workspaceTestTempDir(t)
	path := filepath.Join(root, "sample.md")
	writeTestFile(t, path, "first")
	source := workspace.Source{ID: "sample", Label: "Sample", Kind: "local", LocalPath: root, Limits: workspace.DefaultLimits(), Excludes: inventory.DefaultExclusions()}
	observe := func() string {
		t.Helper()
		_, digest, err := workspaceFingerprint(t.Context(), source, root)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	first := observe()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if observe() != first {
		t.Fatal("mtime-only change rebuilt content")
	}
	writeTestFile(t, filepath.Join(root, ".git", "ignored"), "metadata")
	second := observe()
	writeTestFile(t, filepath.Join(root, ".git", "ignored"), "different metadata")
	if observe() != second {
		t.Fatal("excluded bytes changed fingerprint")
	}
	writeTestFile(t, path, "second")
	if observe() == second {
		t.Fatal("same-size working-tree edit missed")
	}
	before := observe()
	writeTestFile(t, filepath.Join(root, "new.md"), "untracked")
	if observe() == before {
		t.Fatal("untracked addition missed")
	}
	before = observe()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if observe() == before {
		t.Fatal("deletion missed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := workspaceFingerprint(ctx, source, root); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkspaceRejectsUnsafeCLIAndAdmission(t *testing.T) {
	root := workspaceTestTempDir(t)
	for _, args := range [][]string{
		{"--workspace", root, "--id", "bad", "--remote", strings.Join([]string{"https://user", "credential@example.test/repo"}, ":")},
		{"--workspace", root, "--id", "bad", "--ref", "main", root},
		{"--workspace", root, "--id", "bad", root},
		{"--workspace", filepath.Join(root, "nested"), "--id", "bad", root},
		{"--id", "bad", root},
	} {
		if err := runWorkspaceAdd(args); err == nil {
			t.Fatal("unsafe registration accepted")
		}
	}
	for _, args := range [][]string{{"watch", "--workspace", root}, {"sync", "--unsafe-disable-guard"}, {"sync", "--workspace"}} {
		if _, err := validateDirectCommandAdmission("workspace", args); err == nil {
			t.Fatal("unsafe admission accepted", args)
		}
	}
	if help, err := validateDirectCommandAdmission("workspace", []string{"sync", "--help"}); err != nil || !help {
		t.Fatal(help, err)
	}
	if help, err := validateDirectCommandAdmission("workspace", []string{"sync", "--workspace", root, "--timeout", "1m", "sample"}); err != nil || help {
		t.Fatal(help, err)
	}
	for _, args := range [][]string{{"sync", "--workspace", root, "--timeout", "0s"}, {"sync", "--workspace", root, "a", "b"}, {"watch"}} {
		if err := runWorkspaceSyncContext(t.Context(), args); err == nil {
			t.Fatal("invalid sync accepted")
		}
	}
	for _, args := range [][]string{{"--workspace", root, "--interval", "1s"}, {"--workspace", root, "--timeout", "2h"}, {"--workspace", root, "a", "b"}} {
		if err := runWorkspaceWatch(t.Context(), args); err == nil {
			t.Fatal("invalid watch accepted")
		}
	}
	if err := runWorkspaceList([]string{"--workspace", root, "extra"}); err == nil {
		t.Fatal("list accepted arguments")
	}
}

func TestWorkspaceWatchRetriesSequentiallyAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passes, reports := 0, 0
	err := watchWorkspace(ctx, time.Millisecond, func(context.Context) error {
		passes++
		if passes == 1 {
			return errors.New("retry")
		}
		cancel()
		return nil
	}, func(error) { reports++ })
	if err != nil || passes != 2 || reports != 1 {
		t.Fatal(passes, reports, err)
	}
	if err := watchWorkspace(ctx, time.Hour, func(context.Context) error { t.Fatal("canceled watch ran"); return nil }, func(error) {}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRejectsSourceChangeDuringCompilation(t *testing.T) {
	parent := workspaceTestTempDir(t)
	root := filepath.Join(parent, "workspace")
	sourcePath := filepath.Join(parent, "source")
	writeTestFile(t, filepath.Join(sourcePath, "README.md"), "# Before\n")
	store, err := workspace.Open(filepath.Join(root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := workspace.Source{ID: "sample", Label: "Sample", Kind: "local", LocalPath: sourcePath, Limits: workspace.DefaultLimits(), Excludes: inventory.DefaultExclusions()}
	_, err = captureStdout(t, func() error {
		_, err := compileWorkspaceSourceUsing(t.Context(), source, filepath.Join(root, "generations"), func(ctx context.Context, args []string) error {
			if err := runScanContext(ctx, args); err != nil {
				return err
			}
			writeTestFile(t, filepath.Join(sourcePath, "README.md"), "# After\n")
			return nil
		})
		return err
	})
	var failure *workspace.RefreshError
	if !errors.As(err, &failure) || failure.Code != "source_changed" {
		t.Fatal("changed source accepted", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "generations"))
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected generation left published files", entries, err)
	}
}

func TestWorkspacePatternsExcludeNewLargeArtifactsBeforeInventory(t *testing.T) {
	root := workspaceTestTempDir(t)
	writeTestFile(t, filepath.Join(root, "reports", "useful.md"), "# Useful report\n")
	source := workspace.Source{ID: "sample", Label: "Sample", Kind: "local", LocalPath: root, Limits: workspace.DefaultLimits(), Excludes: inventory.DefaultExclusions(), ExcludePatterns: []string{"**/*.pt"}}
	source.Limits.MaxRepositoryBytes = 1024
	_, before, err := workspaceFingerprint(t.Context(), source, root)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(root, "new.pt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	file.Close()
	result, after, excludes, err := workspaceObservation(t.Context(), source, root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("new exclusion metadata missed")
	}
	found := false
	for _, artifact := range result.Artifacts {
		if artifact.Path == "new.pt" && artifact.Status == "excluded" {
			found = true
		}
	}
	if !found || len(excludes) == 0 {
		t.Fatal("large artifact silently dropped instead of accounted as exclusion")
	}
}

func workspaceTestTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
