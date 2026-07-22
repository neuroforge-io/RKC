// Command rkc-mcp exposes one generated RKC snapshot over JSON-RPC stdio.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/repository-knowledge-compiler/rkc/internal/mcpserver"
	"github.com/repository-knowledge-compiler/rkc/internal/server"
)

var version = "0.3.0-reference"

func main() {
	fs := flag.NewFlagSet("rkc-mcp", flag.ExitOnError)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	showVersion := fs.Bool("version", false, "print version")
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version)
		return
	}
	dataset, err := server.Load(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rkc-mcp:", err)
		os.Exit(1)
	}
	if err := mcpserver.New(dataset, version).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rkc-mcp:", err)
		os.Exit(1)
	}
}
