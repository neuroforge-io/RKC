package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/snapshot"
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
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
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
	olderThan := fs.Duration("older-than", 0, "remove unlocked abandoned builds older than this; zero removes unlocked builds of any age")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
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
