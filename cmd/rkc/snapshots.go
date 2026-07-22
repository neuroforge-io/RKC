package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/snapshot"
	sqlitestore "github.com/neuroforge-io/RKC/internal/storage/sqlite"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func runSnapshots(args []string) error {
	if len(args) == 0 {
		return errors.New("snapshots requires list, show, export, recover, or set-current")
	}
	switch args[0] {
	case "list":
		return runSnapshotsList(args[1:])
	case "show":
		return runSnapshotsShow(args[1:])
	case "export":
		return runSnapshotsExport(args[1:])
	case "recover":
		return runSnapshotsRecover(args[1:])
	case "set-current":
		return runSnapshotsSetCurrent(args[1:])
	default:
		return fmt.Errorf("unknown snapshots command %q", args[0])
	}
}

func runSnapshotsSetCurrent(args []string) error {
	fs := flag.NewFlagSet("snapshots set-current", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", ".rkc-state", "snapshot store directory")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("set-current requires exactly one snapshot ID")
	}
	store, err := snapshot.Open(*stateDir)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := store.SetCurrent(id); err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"current": id})
	}
	fmt.Printf("Current snapshot: %s\n", id)
	return nil
}

func runSnapshotsList(args []string) error {
	fs := flag.NewFlagSet("snapshots list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", ".rkc-state", "snapshot store directory")
	databasePath := fs.String("database", "", "durable SQLite store (mutually exclusive with --state-dir)")
	repositoryID := fs.String("repository", "", "optional SQLite repository filter")
	limit := fs.Int("limit", rkcstore.DefaultPageSize, "maximum SQLite snapshots returned in one bounded page")
	cursor := fs.String("cursor", "", "opaque SQLite continuation cursor from a previous page")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("list does not accept positional arguments")
	}
	if *databasePath != "" {
		if flagWasSet(fs, "state-dir") {
			return errors.New("--database and --state-dir are mutually exclusive")
		}
		if *limit < 1 || *limit > rkcstore.MaxPageSize {
			return fmt.Errorf("--limit must be between 1 and %d", rkcstore.MaxPageSize)
		}
		return snapshotsListSQLite(
			context.Background(),
			*databasePath,
			*repositoryID,
			*limit,
			rkcstore.Cursor(*cursor),
			*jsonOutput,
		)
	}
	if *repositoryID != "" {
		return errors.New("--repository requires --database")
	}
	if flagWasSet(fs, "limit") || flagWasSet(fs, "cursor") {
		return errors.New("--limit and --cursor require --database")
	}
	store, err := snapshot.Open(*stateDir)
	if err != nil {
		return err
	}
	records, err := store.List()
	if err != nil {
		return err
	}
	current, _ := store.CurrentID()
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"current": current, "items": records})
	}
	fmt.Printf("Snapshot store: %s\n", store.Root())
	for _, record := range records {
		marker := " "
		if record.SnapshotID == current {
			marker = "*"
		}
		fmt.Printf("%s %-34s %-10s committed=%s bundle=%s\n", marker, record.SnapshotID, record.Status, record.CommittedAt.Format(time.RFC3339), record.BundleDigest)
	}
	return nil
}

func runSnapshotsShow(args []string) error {
	fs := flag.NewFlagSet("snapshots show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", ".rkc-state", "snapshot store directory")
	databasePath := fs.String("database", "", "durable SQLite store (mutually exclusive with --state-dir)")
	repositoryID := fs.String("repository", "", "SQLite repository ID used with --current")
	current := fs.Bool("current", false, "show the current snapshot")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := ""
	if fs.NArg() > 1 {
		return errors.New("show accepts at most one snapshot ID")
	}
	if fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if *current && id != "" {
		return errors.New("--current and an explicit snapshot ID are mutually exclusive")
	}
	if *databasePath != "" {
		if flagWasSet(fs, "state-dir") {
			return errors.New("--database and --state-dir are mutually exclusive")
		}
		return snapshotsShowSQLite(context.Background(), *databasePath, *repositoryID, id, *current, *jsonOutput)
	}
	if *repositoryID != "" {
		return errors.New("--repository requires --database")
	}
	store, err := snapshot.Open(*stateDir)
	if err != nil {
		return err
	}
	if *current || id == "" {
		id, err = store.CurrentID()
		if err != nil {
			return err
		}
	}
	bundle, coverage, record, err := store.Load(id)
	if err != nil {
		return err
	}
	response := map[string]any{"record": record, "snapshot": bundle.Snapshot, "coverage": coverage}
	if *jsonOutput {
		return writeJSONStdout(response)
	}
	fmt.Printf("Snapshot: %s\n", record.SnapshotID)
	fmt.Printf("Status: %s\n", record.Status)
	fmt.Printf("Committed: %s\n", record.CommittedAt.Format(time.RFC3339))
	fmt.Printf("Repository: %s\n", bundle.Snapshot.RepositoryID)
	fmt.Printf("Digest: %s\n", coverage.DeterministicOutputDigest)
	fmt.Printf("Artifacts: %d | Symbols: %d | Edges: %d | Unresolved: %d\n", coverage.ArtifactsInventoried, coverage.SymbolsTotal, coverage.EdgesTotal, coverage.UnresolvedEdges)
	return nil
}

func runSnapshotsExport(args []string) error {
	fs := flag.NewFlagSet("snapshots export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", ".rkc-state", "snapshot store directory")
	databasePath := fs.String("database", "", "durable SQLite store (mutually exclusive with --state-dir)")
	repositoryID := fs.String("repository", "", "SQLite repository ID used when snapshot ID is omitted")
	out := fs.String("out", "", "output directory")
	force := fs.Bool("force", false, "replace output")
	includeSources := fs.Bool("include-sources", false, "re-read and normalize source files when repository_root metadata is available")
	notebookBytes := fs.Int("notebook-pack-bytes", 1_000_000, "NotebookLM target pack bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("export accepts at most one snapshot ID")
	}
	if *databasePath != "" {
		if flagWasSet(fs, "state-dir") {
			return errors.New("--database and --state-dir are mutually exclusive")
		}
		id := ""
		if fs.NArg() == 1 {
			id = fs.Arg(0)
		}
		return snapshotsExportSQLite(context.Background(), *databasePath, *repositoryID, id, *out, *force, *includeSources, *notebookBytes)
	}
	if *repositoryID != "" {
		return errors.New("--repository requires --database")
	}
	store, err := snapshot.Open(*stateDir)
	if err != nil {
		return err
	}
	id := ""
	if fs.NArg() == 1 {
		id = fs.Arg(0)
	} else {
		id, err = store.CurrentID()
		if err != nil {
			return err
		}
	}
	bundle, coverage, record, err := store.Load(id)
	if err != nil {
		return err
	}
	output := *out
	if output == "" {
		output = filepath.Join(store.Root(), "exports", id)
	}
	root := record.Metadata["repository_root"]
	if root == "" {
		*includeSources = false
	}
	if err := publishExport(root, output, *force, bundle, coverage, rkcexport.Options{Root: root, NotebookMaxSize: *notebookBytes, IncludeSources: *includeSources}); err != nil {
		return err
	}
	fmt.Printf("Exported %s to %s\n", id, output)
	return nil
}

func runSnapshotsRecover(args []string) error {
	fs := flag.NewFlagSet("snapshots recover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", ".rkc-state", "snapshot store directory")
	databasePath := fs.String("database", "", "durable SQLite store (mutually exclusive with --state-dir)")
	olderThan := fs.Duration("older-than", 0, "remove unlocked abandoned builds older than this; zero removes unlocked builds of any age")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("recover does not accept positional arguments")
	}
	if *databasePath != "" {
		if flagWasSet(fs, "state-dir") {
			return errors.New("--database and --state-dir are mutually exclusive")
		}
		if flagWasSet(fs, "older-than") {
			return errors.New("--older-than is not used by lease-protected SQLite recovery")
		}
		return snapshotsRecoverSQLite(context.Background(), *databasePath, *jsonOutput)
	}
	store, err := snapshot.Open(*stateDir)
	if err != nil {
		return err
	}
	removed, err := store.Recover(*olderThan)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"removed": removed})
	}
	fmt.Printf("Removed %d abandoned build(s).\n", len(removed))
	return nil
}

func snapshotsListSQLite(
	ctx context.Context,
	path, repositoryID string,
	limit int,
	cursor rkcstore.Cursor,
	jsonOutput bool,
) (resultErr error) {
	database, _, err := openSnapshotsSQLite(ctx, path, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	page, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
		RepositoryID: rkcstore.RepositoryID(repositoryID),
		PageRequest:  rkcstore.PageRequest{Limit: limit, Cursor: cursor},
	})
	if err != nil {
		return err
	}
	current := ""
	if repositoryID != "" {
		if snapshot, currentErr := database.Current(ctx, rkcstore.RepositoryID(repositoryID)); currentErr == nil {
			current = snapshot.ID
		} else if !errors.Is(currentErr, rkcstore.ErrSnapshotNotFound) {
			return currentErr
		}
	}
	if jsonOutput {
		return writeJSONStdout(map[string]any{"current": current, "items": page.Items, "next_cursor": page.Next})
	}
	for _, snapshot := range page.Items {
		marker := " "
		if snapshot.ID == current {
			marker = "*"
		}
		fmt.Printf("%s %-34s repository=%s parent=%s\n", marker, snapshot.ID, snapshot.RepositoryID, snapshot.ParentSnapshotID)
	}
	if page.Next != "" {
		fmt.Printf("Next cursor: %s\n", page.Next)
	}
	return nil
}

func snapshotsShowSQLite(ctx context.Context, path, repositoryID, id string, current, jsonOutput bool) (resultErr error) {
	if current && id != "" {
		return errors.New("--current and an explicit snapshot ID are mutually exclusive")
	}
	database, _, err := openSnapshotsSQLite(ctx, path, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	if current || id == "" {
		if repositoryID == "" {
			return errors.New("SQLite current snapshot selection requires --repository")
		}
		snapshot, err := database.Current(ctx, rkcstore.RepositoryID(repositoryID))
		if err != nil {
			return err
		}
		id = snapshot.ID
	}
	bundle, err := database.Bundle(ctx, rkcstore.SnapshotID(id))
	if err != nil {
		return err
	}
	if repositoryID != "" && bundle.Snapshot.RepositoryID != repositoryID {
		return fmt.Errorf(
			"snapshot %q belongs to repository %q, not requested repository %q",
			id,
			bundle.Snapshot.RepositoryID,
			repositoryID,
		)
	}
	coverage, err := database.Coverage(ctx, rkcstore.SnapshotID(id))
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSONStdout(map[string]any{"snapshot": bundle.Snapshot, "coverage": coverage})
	}
	fmt.Printf("Snapshot: %s\nRepository: %s\nDigest: %s\n", id, bundle.Snapshot.RepositoryID, coverage.DeterministicOutputDigest)
	fmt.Printf("Artifacts: %d | Symbols: %d | Edges: %d | Unresolved: %d\n", coverage.ArtifactsInventoried, coverage.SymbolsTotal, coverage.EdgesTotal, coverage.UnresolvedEdges)
	return nil
}

func snapshotsExportSQLite(ctx context.Context, path, repositoryID, id, output string, force, includeSources bool, notebookBytes int) (resultErr error) {
	database, absolute, err := openSnapshotsSQLite(ctx, path, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	if id == "" {
		if repositoryID == "" {
			return errors.New("SQLite export without a snapshot ID requires --repository")
		}
		current, err := database.Current(ctx, rkcstore.RepositoryID(repositoryID))
		if err != nil {
			return err
		}
		id = current.ID
	}
	bundle, err := database.Bundle(ctx, rkcstore.SnapshotID(id))
	if err != nil {
		return err
	}
	if repositoryID != "" && bundle.Snapshot.RepositoryID != repositoryID {
		return fmt.Errorf(
			"snapshot %q belongs to repository %q, not requested repository %q",
			id,
			bundle.Snapshot.RepositoryID,
			repositoryID,
		)
	}
	coverage, err := database.Coverage(ctx, rkcstore.SnapshotID(id))
	if err != nil {
		return err
	}
	if output == "" {
		output = filepath.Join(filepath.Dir(absolute), "exports", id)
	}
	if includeSources {
		return errors.New("SQLite canonical snapshots do not retain a trusted repository root; omit --include-sources")
	}
	output, err = resolveSQLiteExportOutput(output, absolute)
	if err != nil {
		return err
	}
	if err := publishExport("", output, force, bundle, coverage, rkcexport.Options{NotebookMaxSize: notebookBytes}); err != nil {
		return err
	}
	fmt.Printf("Exported %s to %s\n", id, output)
	return nil
}

func resolveSQLiteExportOutput(output, databasePath string) (string, error) {
	absolute, err := filepath.Abs(output)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite export output: %w", err)
	}
	overlaps, err := sqliteExportPathsOverlap(filepath.Clean(absolute), databasePath)
	if err != nil {
		return "", err
	}
	if overlaps {
		return "", fmt.Errorf("%w: SQLite export output and database must be disjoint", safeoutput.ErrUnsafeTarget)
	}
	resolved, err := safeoutput.ResolveTarget(output, "")
	if err != nil {
		return "", err
	}
	overlaps, err = sqliteExportPathsOverlap(resolved, databasePath)
	if err != nil {
		return "", err
	}
	if overlaps {
		return "", fmt.Errorf("%w: SQLite export output and database must be disjoint", safeoutput.ErrUnsafeTarget)
	}
	return resolved, nil
}

func sqliteExportPathsOverlap(output, databasePath string) (bool, error) {
	outputInsideDatabase, err := pathIsWithin(databasePath, output)
	if err != nil {
		return false, fmt.Errorf("compare SQLite export output and database: %w", err)
	}
	databaseInsideOutput, err := pathIsWithin(output, databasePath)
	if err != nil {
		return false, fmt.Errorf("compare SQLite export output and database: %w", err)
	}
	return outputInsideDatabase || databaseInsideOutput, nil
}

func snapshotsRecoverSQLite(ctx context.Context, path string, jsonOutput bool) (resultErr error) {
	database, _, err := openSnapshotsSQLite(ctx, path, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	result, err := database.Recover(ctx)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSONStdout(result)
	}
	fmt.Printf("Recovered %d incomplete SQLite build(s).\n", len(result.AbortedBuilds))
	return nil
}

func openSnapshotsSQLite(ctx context.Context, path string, readOnly bool) (*sqlitestore.Database, string, error) {
	absolute, err := canonicalSQLitePath(path)
	if err != nil {
		return nil, "", err
	}
	database, err := sqlitestore.Open(ctx, sqlitestore.Options{Path: absolute, ReadOnly: readOnly, RequireExisting: true})
	return database, absolute, err
}
