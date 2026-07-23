package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/pipeline"
)

func defaultStageCacheDirectory() string {
	if root, err := os.UserCacheDir(); err == nil && strings.TrimSpace(root) != "" {
		return filepath.Join(root, "rkc", "stages")
	}
	return filepath.Join(".rkc-state", "cache")
}

func runCache(args []string) error {
	if len(args) == 0 {
		return errors.New("cache subcommand is required: inspect, verify, or prune")
	}
	switch args[0] {
	case "inspect":
		return runCacheInspect(args[1:])
	case "verify":
		return runCacheVerify(args[1:])
	case "prune":
		return runCachePrune(args[1:])
	default:
		return fmt.Errorf("unknown cache subcommand %q; use inspect, verify, or prune", args[0])
	}
}

func runCacheInspect(args []string) error {
	fs := flag.NewFlagSet("cache inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cacheDir := fs.String("cache-dir", defaultStageCacheDirectory(), "stage cache directory")
	verify := fs.Bool("verify", false, "hash and decode every referenced payload")
	limit := fs.Int("limit", 100, "maximum entries to print in human-readable output; 0 prints all")
	jsonOutput := fs.Bool("json", false, "print machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("cache inspect does not accept positional arguments")
	}
	if *limit < 0 {
		return errors.New("--limit must be non-negative")
	}
	cache, err := openCLIStageCache(*cacheDir)
	if err != nil {
		return err
	}
	report, err := cache.Inspect(context.Background(), *verify)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	printCacheReport(report, *limit)
	return nil
}

func runCacheVerify(args []string) error {
	fs := flag.NewFlagSet("cache verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cacheDir := fs.String("cache-dir", defaultStageCacheDirectory(), "stage cache directory")
	jsonOutput := fs.Bool("json", false, "print machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("cache verify does not accept positional arguments")
	}
	cache, err := openCLIStageCache(*cacheDir)
	if err != nil {
		return err
	}
	report, err := cache.Inspect(context.Background(), true)
	if err != nil {
		return err
	}
	if *jsonOutput {
		if err := writeJSONStdout(report); err != nil {
			return err
		}
	} else {
		printCacheReport(report, 100)
	}
	if !report.Healthy {
		return fmt.Errorf("stage cache verification failed with %d invalid entry or entries", report.InvalidEntries)
	}
	return nil
}

func runCachePrune(args []string) error {
	fs := flag.NewFlagSet("cache prune", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cacheDir := fs.String("cache-dir", defaultStageCacheDirectory(), "stage cache directory")
	olderThan := fs.Duration("older-than", 30*24*time.Hour, "prune entries unused for at least this duration")
	all := fs.Bool("all", false, "prune every entry and payload")
	dryRun := fs.Bool("dry-run", false, "report what would be removed without changing the cache")
	yes := fs.Bool("yes", false, "confirm destructive --all pruning")
	jsonOutput := fs.Bool("json", false, "print machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("cache prune does not accept positional arguments")
	}
	if *olderThan <= 0 && !*all {
		return errors.New("--older-than must be positive unless --all is used")
	}
	if *all && !*dryRun && !*yes {
		return errors.New("cache prune --all requires --yes, or use --dry-run to preview")
	}
	cache, err := openCLIStageCache(*cacheDir)
	if err != nil {
		return err
	}
	report, err := cache.Prune(context.Background(), pipeline.StageCachePruneOptions{
		OlderThan: *olderThan,
		All:       *all,
		DryRun:    *dryRun,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	action := "Removed"
	if report.DryRun {
		action = "Would remove"
	}
	fmt.Printf(
		"%s %d cache entry or entries (%s metadata) and %d payload object(s) (%s).\n",
		action,
		report.EntriesSelected,
		formatByteCount(report.MetadataBytes),
		report.ObjectsSelected,
		formatByteCount(report.PayloadBytes),
	)
	fmt.Printf("Cache: %s\n", report.Root)
	fmt.Printf("Entries remaining: %d\n", report.EntriesRemaining)
	return nil
}

func openCLIStageCache(path string) (*pipeline.StageCache, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("--cache-dir is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve cache directory: %w", err)
	}
	return pipeline.OpenStageCache(absolute)
}

func printCacheReport(report pipeline.StageCacheReport, limit int) {
	fmt.Printf("Stage cache: %s\n", report.Root)
	fmt.Printf(
		"Health: %t; entries: %d valid, %d invalid; payloads: %d (%s); orphaned: %d\n",
		report.Healthy,
		report.ValidEntries,
		report.InvalidEntries,
		report.ObjectCount,
		formatByteCount(report.PayloadBytes),
		report.OrphanObjects,
	)
	shown := len(report.Entries)
	if limit > 0 && shown > limit {
		shown = limit
	}
	for _, entry := range report.Entries[:shown] {
		status := "valid"
		if !entry.Valid {
			status = "invalid: " + entry.Issue
		}
		fmt.Printf(
			"- %s  %s  %s  %s\n",
			entry.StageID,
			shortDigest(entry.CacheKey),
			formatByteCount(entry.PayloadBytes),
			status,
		)
	}
	if shown < len(report.Entries) {
		fmt.Printf("… %d more entrie(s); use --limit 0 or --json to show all.\n", len(report.Entries)-shown)
	}
}

func shortDigest(value string) string {
	value = strings.TrimPrefix(value, "stage:")
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func formatByteCount(value int64) string {
	const (
		kib = int64(1024)
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case value >= gib:
		return fmt.Sprintf("%.2f GiB", float64(value)/float64(gib))
	case value >= mib:
		return fmt.Sprintf("%.2f MiB", float64(value)/float64(mib))
	case value >= kib:
		return fmt.Sprintf("%.2f KiB", float64(value)/float64(kib))
	default:
		return fmt.Sprintf("%d B", value)
	}
}
