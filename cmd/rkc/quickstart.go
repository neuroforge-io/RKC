package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// runQuickstart provides one safe first-run command without weakening any scan
// or quality-gate contract. The portable profile is the default; direct Python
// remains disabled until its child unit and parent scan share a proven ceiling.
func runQuickstart(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDirectCommandWithAdmission(ctx, "quickstart", args, runQuickstartContext)
}

func runQuickstartContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	config := fs.String("config", "", "optional RKC JSON configuration")
	output := fs.String("out", "", "atlas directory (default <repository>/.rkc)")
	state := fs.String("state-dir", "", "snapshot directory (default <repository>/.rkc-state)")
	noGitMetadata := fs.Bool("no-git-metadata", false, "skip the Git metadata helper and record Git provenance as unavailable")
	enablePython := fs.Bool("python", false, "request the Python adapter (disabled for direct quickstart until aggregate ceilings are proved)")
	clean := fs.Bool("clean", false, "disable incremental stage-cache reuse")
	force := fs.Bool("force", true, "replace an existing RKC-owned atlas")
	scipIndexes := stringList{}
	fs.Var(&scipIndexes, "scip-index", "SCIP index to import; external files remain producer-unverified; repeatable")
	scipGenerate := stringList{}
	fs.Var(&scipGenerate, "scip-generate", "generate a compiler-grade SCIP index for this language before scanning; repeatable")
	scipTool := fs.String("scip-tool", "", "indexer binary override used by --scip-generate")
	defaultScipLock := defaultScipIndexerLockPath()
	scipLock := fs.String("scip-lock", defaultScipLock, "operator-owned absolute indexer pin lock used by --scip-generate")
	scipNoPinCheck := fs.Bool("scip-no-pin-check", false, "explicitly allow an unpinned or digest-mismatched SCIP indexer")
	tracePaths := stringList{}
	fs.Var(&tracePaths, "trace", "runtime trace file to import; repeatable")
	historyPath := fs.String("history", "", "compiled semantic history file to import")
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
	if *noGitMetadata {
		scanArguments = append(scanArguments, "--no-git-metadata")
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
	for _, index := range scipIndexes {
		scanArguments = append(scanArguments, "--scip-index", index)
	}
	for _, language := range scipGenerate {
		scanArguments = append(scanArguments, "--scip-generate", language)
	}
	if *scipTool != "" {
		scanArguments = append(scanArguments, "--scip-tool", *scipTool)
	}
	if *scipLock != defaultScipLock {
		scanArguments = append(scanArguments, "--scip-lock", *scipLock)
	}
	if *scipNoPinCheck {
		scanArguments = append(scanArguments, "--scip-no-pin-check")
	}
	for _, tracePath := range tracePaths {
		scanArguments = append(scanArguments, "--trace", tracePath)
	}
	if *historyPath != "" {
		scanArguments = append(scanArguments, "--history", *historyPath)
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

	atlasArgument := quoteCommandPath(atlas, runtime.GOOS)
	fmt.Printf("RKC atlas is ready: %s\n", atlas)
	fmt.Printf("Search it: rkc query --dir %s \"your terms\"\n", atlasArgument)
	fmt.Printf("Explore it: rkc serve --dir %s\n", atlasArgument)
	fmt.Printf("Upload the wiki pack: %s/notebooklm/UPLOAD.md\n", filepath.ToSlash(atlas))
	fmt.Printf("Build an evidence packet: rkc synthesize --dir %s --query \"your question\" --packet-only\n", atlasArgument)
	fmt.Printf("Ask with citations after model setup: rkc answer --dir %s \"your question\"\n", atlasArgument)
	return nil
}

// quoteCommandPath returns one copy-ready path argument for the host's normal
// interactive shell. Windows documentation and first-run output target
// PowerShell; other supported hosts use POSIX shell quoting.
func quoteCommandPath(value, platform string) string {
	if platform == "windows" {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
