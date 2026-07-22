// Command rkc-mcp exposes one generated RKC snapshot over JSON-RPC stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuroforge-io/RKC/internal/mcpserver"
	"github.com/neuroforge-io/RKC/internal/server"
	sqlitestore "github.com/neuroforge-io/RKC/internal/storage/sqlite"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

var version = "0.3.0-reference"

// exitProcess is replaced only by the entry-point test. Keeping process exit
// at this outermost boundary lets run flush diagnostics before the production
// command terminates.
var exitProcess = os.Exit

func main() {
	exitProcess(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, input io.Reader, output, diagnostics io.Writer) int {
	fs := flag.NewFlagSet("rkc-mcp", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(diagnostics, "rkc-mcp: positional arguments are not supported")
		return 2
	}
	if *showVersion {
		fmt.Fprintln(output, version)
		return 0
	}
	dirExplicit := false
	fs.Visit(func(item *flag.Flag) { dirExplicit = dirExplicit || item.Name == "dir" })
	dataset, err := loadMCPDataset(ctx, *dir, *database, *snapshotID, *repositoryID, dirExplicit)
	if err != nil {
		fmt.Fprintln(diagnostics, "rkc-mcp:", err)
		return 1
	}
	if err := mcpserver.New(dataset, version).Serve(ctx, input, output); err != nil {
		fmt.Fprintln(diagnostics, "rkc-mcp:", err)
		return 1
	}
	return 0
}

func loadMCPDataset(ctx context.Context, dir, database, snapshotID, repositoryID string, dirExplicit bool) (dataset *server.Dataset, resultErr error) {
	if database == "" {
		if snapshotID != "" || repositoryID != "" {
			return nil, errors.New("--snapshot and --repository require --database")
		}
		return server.Load(dir)
	}
	if dirExplicit {
		return nil, errors.New("--database and --dir are mutually exclusive")
	}
	if (snapshotID == "") == (repositoryID == "") {
		return nil, errors.New("SQLite dataset requires exactly one of --snapshot or --repository")
	}
	if database != strings.TrimSpace(database) {
		return nil, errors.New("SQLite database path has surrounding whitespace")
	}
	absolute, err := filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	durable, err := sqlitestore.Open(ctx, sqlitestore.Options{Path: filepath.Clean(absolute), ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, durable.Close()) }()
	selected := rkcstore.SnapshotID(snapshotID)
	if repositoryID != "" {
		current, err := durable.Current(ctx, rkcstore.RepositoryID(repositoryID))
		if err != nil {
			return nil, err
		}
		selected = rkcstore.SnapshotID(current.ID)
	}
	dataset, err = server.LoadStore(ctx, durable, selected)
	if err != nil {
		return nil, err
	}
	dataset.Root = absolute
	return dataset, nil
}
