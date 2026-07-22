// Command rkc-mcp exposes one generated RKC snapshot over JSON-RPC stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/neuroforge-io/RKC/internal/mcpserver"
	"github.com/neuroforge-io/RKC/internal/server"
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
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(output, version)
		return 0
	}
	dataset, err := server.Load(*dir)
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
