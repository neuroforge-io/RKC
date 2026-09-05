package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/neuroforge-io/RKC/internal/server"
)

func runContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	dir := fs.String("dir", ".rkc", "compiled atlas directory")
	database := fs.String("database", "", "SQLite store; mutually exclusive with --dir")
	snapshot := fs.String("snapshot", "", "SQLite snapshot ID")
	repository := fs.String("repository", "", "SQLite repository ID")
	limit := fs.Int("limit", 12, "maximum results, 1 to 50")
	maxBytes := fs.Int("max-bytes", 32768, "compact JSON item-array byte budget, 1024 to 262144")
	format := fs.String("format", "json", "json or markdown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("context requires a search query; example: rkc context --dir .rkc authentication")
	}
	if *format != "json" && *format != "markdown" {
		return errors.New("format must be json or markdown")
	}
	ctx := context.Background()
	dataset, err := loadSelectedDataset(ctx, *dir, *database, *snapshot, *repository, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	packet, err := dataset.BuildContext(ctx, strings.Join(fs.Args(), " "), *limit, *maxBytes)
	if err != nil {
		return err
	}
	if *format == "markdown" {
		_, err := fmt.Fprint(os.Stdout, server.ContextMarkdown(packet))
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(packet)
}
