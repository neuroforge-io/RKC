package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// runQuickstart provides one safe first-run command without weakening any scan
// or quality-gate contract. The portable profile is the default; Python remains
// an explicit opt-in because its fail-closed sandbox is Linux-specific.
func runQuickstart(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runQuickstartContext(ctx, args)
}

func runQuickstartContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	config := fs.String("config", "", "optional RKC JSON configuration")
	output := fs.String("out", "", "atlas directory (default <repository>/.rkc)")
	state := fs.String("state-dir", "", "snapshot directory (default <repository>/.rkc-state)")
	enablePython := fs.Bool("python", false, "enable the sandboxed Python adapter after doctor passes")
	clean := fs.Bool("clean", false, "disable incremental stage-cache reuse")
	force := fs.Bool("force", true, "replace an existing RKC-owned atlas")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("quickstart accepts at most one repository path")
	}
	repository := "."
	if fs.NArg() == 1 {
		repository = fs.Arg(0)
	}
	root, err := filepath.Abs(repository)
	if err != nil {
		return fmt.Errorf("resolve quickstart repository: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect quickstart repository: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("quickstart repository is not a directory: %s", root)
	}
	if *output == "" {
		*output = filepath.Join(root, ".rkc")
	}
	if *state == "" {
		*state = filepath.Join(root, ".rkc-state")
	}
	atlas, err := filepath.Abs(*output)
	if err != nil {
		return fmt.Errorf("resolve quickstart output: %w", err)
	}
	snapshotState, err := filepath.Abs(*state)
	if err != nil {
		return fmt.Errorf("resolve quickstart state: %w", err)
	}

	scanArguments := []string{
		"--out", atlas,
		"--state-dir", snapshotState,
	}
	if *config != "" {
		scanArguments = append(scanArguments, "--config", *config)
	}
	if !*enablePython {
		scanArguments = append(scanArguments, "--no-python")
	}
	if *clean {
		scanArguments = append(scanArguments, "--no-cache")
	}
	if *force {
		scanArguments = append(scanArguments, "--force")
	}
	scanArguments = append(scanArguments, root)
	if err := runScanContext(ctx, scanArguments); err != nil {
		return fmt.Errorf("quickstart scan: %w", err)
	}

	checkArguments := []string{
		"--coverage", filepath.Join(atlas, "coverage.json"),
		"--bundle", filepath.Join(atlas, "bundle.json"),
		"--min-inventory-accounting", "1",
		"--min-symbol-evidence", "1",
		"--min-edge-resolution", "0",
		"--min-claim-citation", "1",
		"--max-errors", "0",
		"--max-high-confidence-secrets", "0",
	}
	if *config != "" {
		checkArguments = append(checkArguments, "--config", *config)
	}
	if err := runCheck(checkArguments); err != nil {
		return fmt.Errorf("quickstart verification: %w", err)
	}

	fmt.Printf("RKC atlas is ready: %s\n", atlas)
	fmt.Println("Search it: rkc query --dir <atlas> <terms>")
	fmt.Println("Explore it: rkc serve --dir <atlas>")
	fmt.Println("Build an evidence packet: rkc synthesize --dir <atlas> --query <question> --packet-only")
	fmt.Println("Ask with citations after model setup: rkc answer --dir <atlas> <question>")
	return nil
}
