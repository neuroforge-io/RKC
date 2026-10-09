// Package main contains the rkc-mcp JSON-RPC adapters.
//
// Command rkc-mcp exposes generated RKC snapshots over stdio or local HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	workspace := fs.String("workspace", "", "private workspace registry; automatically follows verified active snapshots")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	transport := fs.String("transport", "stdio", "MCP transport: stdio or credential-free loopback http")
	listen := fs.String("listen", "127.0.0.1:0", "HTTP listen address: exact 127.0.0.1:port or [::1]:port; 0 assigns a port")
	httpTimeout := fs.Duration("http-request-timeout", 10*time.Second, "HTTP request deadline, positive and at most 30s")
	httpConcurrent := fs.Int("http-concurrency", 2, "maximum HTTP requests executing concurrently, 1 through 8")
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
	dirExplicit, workspaceExplicit := false, false
	flagsSet := map[string]bool{}
	fs.Visit(func(item *flag.Flag) {
		flagsSet[item.Name] = true
		dirExplicit = dirExplicit || item.Name == "dir"
		workspaceExplicit = workspaceExplicit || item.Name == "workspace"
	})
	if *transport != "stdio" && *transport != "http" {
		fmt.Fprintln(diagnostics, "rkc-mcp: transport must be stdio or http")
		return 1
	}
	if *transport == "stdio" && (flagsSet["listen"] || flagsSet["http-request-timeout"] || flagsSet["http-concurrency"]) {
		fmt.Fprintln(diagnostics, "rkc-mcp: HTTP options require --transport http")
		return 1
	}
	if *transport == "http" {
		if !dirExplicit || strings.TrimSpace(*dir) == "" || *dir != strings.TrimSpace(*dir) ||
			workspaceExplicit || flagsSet["database"] || flagsSet["snapshot"] || flagsSet["repository"] {
			fmt.Fprintln(diagnostics, "rkc-mcp: HTTP requires explicit --dir and cannot use workspace or SQLite selectors")
			return 1
		}
		options := mcpserver.HTTPOptions{Listen: *listen, RequestTimeout: *httpTimeout, MaximumConcurrent: *httpConcurrent}
		if err := mcpserver.ValidateHTTPOptions(options); err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		httpContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		dataset, err := loadMCPDataset(httpContext, *dir, "", "", "", true)
		if err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		adapter, err := mcpserver.New(dataset, version).ListenHTTP(options)
		if err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		fmt.Fprintf(diagnostics, "rkc-mcp: listening on %s (snapshot %s; credential-free local only)\n", adapter.Endpoint(), dataset.Manifest.ID)
		if err := adapter.Serve(httpContext); err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		return 0
	}
	if workspaceExplicit {
		if strings.TrimSpace(*workspace) == "" || *workspace != strings.TrimSpace(*workspace) {
			fmt.Fprintln(diagnostics, "rkc-mcp: --workspace requires a registry path without surrounding whitespace")
			return 1
		}
		if dirExplicit || *database != "" || *snapshotID != "" || *repositoryID != "" {
			fmt.Fprintln(diagnostics, "rkc-mcp: --workspace is mutually exclusive with --dir, --database, --snapshot, and --repository")
			return 1
		}
		adapter, err := mcpserver.NewWorkspace(*workspace, version)
		if err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		if err := adapter.Serve(ctx, input, output); err != nil {
			fmt.Fprintln(diagnostics, "rkc-mcp:", err)
			return 1
		}
		return 0
	}
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
