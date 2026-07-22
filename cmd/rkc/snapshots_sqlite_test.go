package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/neuroforge-io/RKC/internal/storage/sqlite"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const (
	sqliteSnapshotRepositoryA = "repository-a"
	sqliteSnapshotRepositoryB = "repository-b"
	sqliteSnapshotA1          = "snapshot-a-1"
	sqliteSnapshotA2          = "snapshot-a-2"
	sqliteSnapshotB1          = "snapshot-b-1"
)

type sqliteSnapshotListResponse struct {
	Current    string              `json:"current"`
	Items      []rkcmodel.Snapshot `json:"items"`
	NextCursor rkcstore.Cursor     `json:"next_cursor"`
}

func TestSQLiteSnapshotsPaginationAndSelectors(t *testing.T) {
	databasePath := createSQLiteSnapshotsFixture(t)

	firstOutput, err := captureStdout(t, func() error {
		return runSnapshots([]string{
			"list", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
			"--limit", "1", "--json",
		})
	})
	if err != nil {
		t.Fatalf("first SQLite snapshot page: %v", err)
	}
	var first sqliteSnapshotListResponse
	if err := json.Unmarshal([]byte(firstOutput), &first); err != nil {
		t.Fatalf("decode first SQLite snapshot page: %v", err)
	}
	if first.Current != sqliteSnapshotA2 || len(first.Items) != 1 || first.Items[0].ID != sqliteSnapshotA2 || first.NextCursor == "" {
		t.Fatalf("first SQLite snapshot page = %+v", first)
	}

	secondOutput, err := captureStdout(t, func() error {
		return runSnapshots([]string{
			"list", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
			"--limit", "1", "--cursor", string(first.NextCursor), "--json",
		})
	})
	if err != nil {
		t.Fatalf("second SQLite snapshot page: %v", err)
	}
	var second sqliteSnapshotListResponse
	if err := json.Unmarshal([]byte(secondOutput), &second); err != nil {
		t.Fatalf("decode second SQLite snapshot page: %v", err)
	}
	if second.Current != sqliteSnapshotA2 || len(second.Items) != 1 || second.Items[0].ID != sqliteSnapshotA1 || second.NextCursor != "" {
		t.Fatalf("second SQLite snapshot page = %+v", second)
	}

	for _, args := range [][]string{
		{"list", "--database", databasePath, "--limit", "0"},
		{"list", "--database", databasePath, "--limit", "201"},
		{"list", "--state-dir", filepath.Join(t.TempDir(), "state"), "--limit", "1"},
		{"show", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA, "--current", sqliteSnapshotA1},
	} {
		if err := runSnapshots(args); err == nil {
			t.Fatalf("selector unexpectedly accepted: %v", args)
		}
	}
	if err := runSnapshots([]string{
		"list", "--database", databasePath, "--repository", sqliteSnapshotRepositoryB,
		"--limit", "1", "--cursor", string(first.NextCursor),
	}); err == nil {
		t.Fatal("repository-scoped cursor was accepted for another repository")
	}
	if err := runSnapshots([]string{
		"show", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
		sqliteSnapshotB1,
	}); err == nil || !strings.Contains(err.Error(), "belongs to repository") {
		t.Fatalf("cross-repository show error = %v", err)
	}
}

func TestSQLiteSnapshotsShowExportAndRecover(t *testing.T) {
	databasePath := createSQLiteSnapshotsFixture(t)

	showOutput, err := captureStdout(t, func() error {
		return runSnapshots([]string{
			"show", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
			"--json", sqliteSnapshotA1,
		})
	})
	if err != nil {
		t.Fatalf("show SQLite snapshot: %v", err)
	}
	var shown struct {
		Snapshot rkcmodel.Snapshot `json:"snapshot"`
		Coverage rkcmodel.Coverage `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(showOutput), &shown); err != nil {
		t.Fatalf("decode shown SQLite snapshot: %v", err)
	}
	if shown.Snapshot.ID != sqliteSnapshotA1 || shown.Snapshot.RepositoryID != sqliteSnapshotRepositoryA || shown.Coverage.SnapshotID != sqliteSnapshotA1 {
		t.Fatalf("shown SQLite snapshot = %+v", shown)
	}
	currentOutput, err := captureStdout(t, func() error {
		return runSnapshots([]string{
			"show", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
			"--current", "--json",
		})
	})
	if err != nil {
		t.Fatalf("show current SQLite snapshot: %v", err)
	}
	var current struct {
		Snapshot rkcmodel.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(currentOutput), &current); err != nil {
		t.Fatalf("decode current SQLite snapshot: %v", err)
	}
	if current.Snapshot.ID != sqliteSnapshotA2 {
		t.Fatalf("current SQLite snapshot = %q, want %q", current.Snapshot.ID, sqliteSnapshotA2)
	}

	if err := runSnapshots([]string{
		"export", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
		"--out", filepath.Join(t.TempDir(), "wrong-export"), sqliteSnapshotB1,
	}); err == nil || !strings.Contains(err.Error(), "belongs to repository") {
		t.Fatalf("cross-repository export error = %v", err)
	}
	exportRoot := filepath.Join(t.TempDir(), "export")
	if _, err := captureStdout(t, func() error {
		return runSnapshots([]string{
			"export", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
			"--out", exportRoot, sqliteSnapshotA1,
		})
	}); err != nil {
		t.Fatalf("export SQLite snapshot: %v", err)
	}
	exportedBytes, err := os.ReadFile(filepath.Join(exportRoot, "bundle.json"))
	if err != nil {
		t.Fatalf("read exported SQLite bundle: %v", err)
	}
	var exported rkcmodel.Bundle
	if err := json.Unmarshal(exportedBytes, &exported); err != nil {
		t.Fatalf("decode exported SQLite bundle: %v", err)
	}
	if exported.Snapshot.ID != sqliteSnapshotA1 || exported.Snapshot.RepositoryID != sqliteSnapshotRepositoryA {
		t.Fatalf("exported SQLite bundle identity = %+v", exported.Snapshot)
	}

	buildID := leaveSQLiteBuildOpen(t, databasePath)
	recoverOutput, err := captureStdout(t, func() error {
		return runSnapshots([]string{"recover", "--database", databasePath, "--json"})
	})
	if err != nil {
		t.Fatalf("recover SQLite snapshots: %v", err)
	}
	var recovered rkcstore.RecoveryResult
	if err := json.Unmarshal([]byte(recoverOutput), &recovered); err != nil {
		t.Fatalf("decode SQLite recovery result: %v", err)
	}
	if len(recovered.AbortedBuilds) != 1 || recovered.AbortedBuilds[0] != buildID {
		t.Fatalf("SQLite recovery result = %+v, want %q", recovered, buildID)
	}
	if err := runSnapshots([]string{
		"recover", "--database", databasePath, "--older-than", "1s",
	}); err == nil {
		t.Fatal("SQLite recovery accepted legacy --older-than")
	}
	for _, command := range []string{"list", "recover"} {
		err := runSnapshots([]string{command, "--database", databasePath, "unexpected"})
		if err == nil || !strings.Contains(err.Error(), "does not accept positional arguments") {
			t.Fatalf("snapshots %s positional argument error = %v", command, err)
		}
	}

	for name, output := range map[string]string{
		"database itself":   databasePath,
		"inside database":   filepath.Join(databasePath, "nested"),
		"contains database": filepath.Dir(databasePath),
	} {
		t.Run("reject export output "+name, func(t *testing.T) {
			err := runSnapshots([]string{
				"export", "--database", databasePath, "--repository", sqliteSnapshotRepositoryA,
				"--out", output, "--force", sqliteSnapshotA1,
			})
			if err == nil || !strings.Contains(err.Error(), "SQLite export output and database must be disjoint") {
				t.Fatalf("unsafe SQLite export error = %v", err)
			}
		})
	}
}

func createSQLiteSnapshotsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "snapshots.sqlite")
	database, err := sqlitestore.Open(context.Background(), sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	commitSQLiteSnapshotFixture(t, database, sqliteSnapshotFixtureBundle(
		sqliteSnapshotA1, sqliteSnapshotRepositoryA, "", time.Unix(100, 0).UTC(),
	))
	commitSQLiteSnapshotFixture(t, database, sqliteSnapshotFixtureBundle(
		sqliteSnapshotB1, sqliteSnapshotRepositoryB, "", time.Unix(200, 0).UTC(),
	))
	commitSQLiteSnapshotFixture(t, database, sqliteSnapshotFixtureBundle(
		sqliteSnapshotA2, sqliteSnapshotRepositoryA, sqliteSnapshotA1, time.Unix(300, 0).UTC(),
	))
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func commitSQLiteSnapshotFixture(t *testing.T, database *sqlitestore.Database, bundle rkcmodel.Bundle) {
	t.Helper()
	ctx := context.Background()
	build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
		RepositoryID:     rkcstore.RepositoryID(bundle.Snapshot.RepositoryID),
		ParentSnapshotID: rkcstore.SnapshotID(bundle.Snapshot.ParentSnapshotID),
		ExpectedSchema:   bundle.Snapshot.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rkcstore.StageBundle(ctx, database, build, bundle); err != nil {
		t.Fatal(err)
	}
	if err := database.Commit(ctx, build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}
}

func sqliteSnapshotFixtureBundle(id, repository, parent string, created time.Time) rkcmodel.Bundle {
	artifactID := "artifact-" + id
	evidenceID := "evidence-" + id
	nodeID := "node-" + id
	artifact := rkcmodel.Artifact{
		ID: artifactID, Path: "main.go", Kind: "file", Language: "go", Status: "parsed", Text: true,
	}
	evidence := rkcmodel.Evidence{
		ID: evidenceID, Kind: "declared", Method: "snapshot-cli-test", Confidence: 1,
		Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: artifact.Path, StartLine: 1, EndLine: 1},
	}
	node := rkcmodel.Node{
		ID: nodeID, Kind: "function", Name: "Alpha", Language: "go", Visibility: "public",
		ArtifactID: artifactID, EvidenceIDs: []string{evidenceID},
	}
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion, ID: id, RepositoryID: repository,
			ParentSnapshotID: parent, CreatedAt: created, Status: "committed",
			RootName: repository, RootPath: "/private/repository",
			ContentDigest: strings.Repeat("a", 64),
			Tool:          rkcmodel.ToolInfo{Name: "snapshot-cli-test", Version: "1"},
		},
		Artifacts: []rkcmodel.Artifact{artifact},
		Nodes:     []rkcmodel.Node{node},
		Evidence:  []rkcmodel.Evidence{evidence},
	}
}

func leaveSQLiteBuildOpen(t *testing.T, path string) rkcstore.BuildID {
	t.Helper()
	ctx := context.Background()
	database, err := sqlitestore.Open(ctx, sqlitestore.Options{Path: path, RequireExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
		RepositoryID:     sqliteSnapshotRepositoryA,
		ParentSnapshotID: sqliteSnapshotA2,
		ExpectedSchema:   rkcmodel.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return build
}
