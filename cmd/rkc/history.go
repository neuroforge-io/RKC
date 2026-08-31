package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/history"
	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

const maximumHistorySymbolQueryBytes = 1024

type historyBuildOptions struct {
	dir        string
	out        string
	maxCommits int
	jsonOutput bool
}

type historySymbolOptions struct {
	dir        string
	name       string
	maxCommits int
	jsonOutput bool
}

func runHistory(args []string) error {
	if len(args) == 0 {
		return historyUsage()
	}
	switch args[0] {
	case "build":
		return runHistoryBuild(args[1:])
	case "report":
		return runHistoryReport(args[1:])
	case "symbol":
		return runHistorySymbol(args[1:])
	case "help", "--help", "-h":
		return historyUsage()
	default:
		return fmt.Errorf("unknown history subcommand %q; run 'rkc history help'", args[0])
	}
}

func historyUsage() error {
	_, err := fmt.Fprint(os.Stdout, `Compile a bounded Git observation window into semantic interface deltas.

  rkc history build --dir <repository> [--max-commits 500] [--out .rkc-history.json]
  rkc history report --history <file>
  rkc history symbol --name <NAME> --dir <repository> [--max-commits 500]

Build compares each supported Go or TypeScript file with its exact first-parent
version. It records observed symbol additions, removals, signature changes, and
file moves. A bounded result is explicitly marked when older commits were not
observed; it is not presented as a complete repository lifetime.
`)
	return err
}

func runHistoryBuild(args []string) error {
	options, err := parseHistoryBuild(args)
	if err != nil {
		return err
	}
	operationArgs := append([]string{"build"}, args...)
	return runHistoryWithAdmission(operationArgs, func(ctx context.Context) error {
		return executeHistoryBuild(ctx, options)
	})
}

func parseHistoryBuild(args []string) (historyBuildOptions, error) {
	fs := flag.NewFlagSet("history build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "repository directory to compile")
	out := fs.String("out", ".rkc-history.json", "history output path")
	maxCommits := fs.Int("max-commits", history.DefaultMaxCommits, "maximum commits to observe")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return historyBuildOptions{}, err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*dir) == "" {
		return historyBuildOptions{}, errors.New("history build requires --dir <repository>")
	}
	if strings.TrimSpace(*out) == "" || !terminalSafeHistoryArgument(*out) {
		return historyBuildOptions{}, errors.New("history build requires a valid --out path")
	}
	if !terminalSafeHistoryArgument(*dir) {
		return historyBuildOptions{}, errors.New("history build requires a control-safe repository path")
	}
	if *maxCommits < 1 || *maxCommits > history.MaximumCommits {
		return historyBuildOptions{}, fmt.Errorf("--max-commits must be between 1 and %d", history.MaximumCommits)
	}
	return historyBuildOptions{
		dir: *dir, out: *out, maxCommits: *maxCommits, jsonOutput: *jsonOutput,
	}, nil
}

func executeHistoryBuild(ctx context.Context, options historyBuildOptions) error {
	compiled, err := history.Build(ctx, history.Options{
		Repository: options.dir,
		MaxCommits: options.maxCommits,
	})
	if err != nil {
		return err
	}
	data, err := jsonMarshalIndent(compiled)
	if err != nil {
		return fmt.Errorf("encode history: %w", err)
	}
	if len(data) > history.MaximumCompiledHistoryBytes {
		return fmt.Errorf("compiled history exceeds maximum %d bytes", history.MaximumCompiledHistoryBytes)
	}
	if err := writeAtomic(options.out, data, 0o600); err != nil {
		return err
	}
	if options.jsonOutput {
		return writeJSONStdout(map[string]any{
			"history":           options.out,
			"commit":            compiled.Commit,
			"observed_commits":  len(compiled.Commits),
			"window_truncated":  compiled.WindowTruncated,
			"details_truncated": compiled.DetailsTruncated,
			"symbols":           len(compiled.Symbols),
			"refactors":         len(compiled.Refactors),
		})
	}
	fmt.Printf("History compiled: %s\n", history.EscapeTerminalText(options.out))
	fmt.Printf("Commit: %s\n", history.EscapeTerminalText(compiled.Commit))
	fmt.Printf(
		"Commits observed: %d; symbols observed: %d; refactor candidates: %d\n",
		len(compiled.Commits),
		len(compiled.Symbols),
		len(compiled.Refactors),
	)
	if compiled.WindowTruncated {
		fmt.Printf("Observation window truncated at %d commits.\n", compiled.CommitLimit)
	}
	if compiled.DetailsTruncated {
		fmt.Println("One or more signature histories reached their detail bound.")
	}
	return nil
}

func runHistoryReport(args []string) error {
	fs := flag.NewFlagSet("history report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	historyPath := fs.String("history", "", "compiled history file")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*historyPath) == "" {
		return errors.New("history report requires --history <file>")
	}
	if !terminalSafeHistoryArgument(*historyPath) {
		return errors.New("history report requires a control-safe history path")
	}
	compiled, err := readHistoryFile(*historyPath)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(compiled)
	}
	fmt.Printf(
		"Semantic history observations for %s at %s\n",
		history.EscapeTerminalText(compiled.Repository),
		history.EscapeTerminalText(compiled.Commit),
	)
	fmt.Printf(
		"Observed commits: %d; symbols: %d; refactor candidates: %d\n",
		len(compiled.Commits),
		len(compiled.Symbols),
		len(compiled.Refactors),
	)
	if compiled.WindowTruncated {
		fmt.Printf("Window: newest %d commits only (truncated)\n", compiled.CommitLimit)
	}
	fmt.Println()
	fmt.Println("Observed symbol events:")
	symbols := append([]history.SymbolHistory(nil), compiled.Symbols...)
	sortHistorySymbols(symbols)
	for _, symbol := range symbols {
		fmt.Printf(
			"  %s [%s/%s] observed=%s..%s files=%s\n",
			history.EscapeTerminalText(symbol.QualifiedName),
			history.EscapeTerminalText(symbol.Language),
			history.EscapeTerminalText(symbol.Kind),
			shortHash(symbol.FirstObserved),
			shortHash(symbol.LastObserved),
			history.EscapeTerminalText(strings.Join(symbol.Files, ",")),
		)
	}
	fmt.Println()
	fmt.Println("Commit deltas:")
	for _, commit := range compiled.Commits {
		fmt.Printf(
			"  %s %s (%s): %d files; +%d -%d ~%d symbols\n",
			shortHash(commit.ID),
			history.EscapeTerminalText(commit.Date),
			history.EscapeTerminalText(commit.Subject),
			len(commit.ChangedFiles),
			len(commit.AddedSymbols),
			len(commit.RemovedSymbols),
			len(commit.ChangedSymbols),
		)
	}
	return nil
}

func runHistorySymbol(args []string) error {
	options, err := parseHistorySymbol(args)
	if err != nil {
		return err
	}
	operationArgs := append([]string{"symbol"}, args...)
	return runHistoryWithAdmission(operationArgs, func(ctx context.Context) error {
		return executeHistorySymbol(ctx, options)
	})
}

func parseHistorySymbol(args []string) (historySymbolOptions, error) {
	fs := flag.NewFlagSet("history symbol", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "repository directory to compile")
	name := fs.String("name", "", "symbol name to trace")
	maxCommits := fs.Int("max-commits", history.DefaultMaxCommits, "maximum commits to observe")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return historySymbolOptions{}, err
	}
	trimmedName := strings.TrimSpace(*name)
	if fs.NArg() != 0 || trimmedName == "" || strings.TrimSpace(*dir) == "" {
		return historySymbolOptions{}, errors.New("history symbol requires --name <NAME> and --dir <repository>")
	}
	if len(trimmedName) > maximumHistorySymbolQueryBytes || !terminalSafeHistoryArgument(trimmedName) {
		return historySymbolOptions{}, fmt.Errorf("--name exceeds maximum %d bytes or is invalid", maximumHistorySymbolQueryBytes)
	}
	if !terminalSafeHistoryArgument(*dir) {
		return historySymbolOptions{}, errors.New("history symbol requires a control-safe repository path")
	}
	if *maxCommits < 1 || *maxCommits > history.MaximumCommits {
		return historySymbolOptions{}, fmt.Errorf("--max-commits must be between 1 and %d", history.MaximumCommits)
	}
	return historySymbolOptions{
		dir: *dir, name: trimmedName, maxCommits: *maxCommits, jsonOutput: *jsonOutput,
	}, nil
}

func executeHistorySymbol(ctx context.Context, options historySymbolOptions) error {
	compiled, err := history.Build(ctx, history.Options{
		Repository: options.dir,
		MaxCommits: options.maxCommits,
	})
	if err != nil {
		return err
	}
	var matches []history.SymbolHistory
	for _, symbol := range compiled.Symbols {
		if symbol.Name == options.name {
			matches = append(matches, symbol)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no observed symbol named %q was found in the history window", options.name)
	}
	if options.jsonOutput {
		return writeJSONStdout(map[string]any{
			"window_truncated": compiled.WindowTruncated,
			"matches":          matches,
		})
	}
	if compiled.WindowTruncated {
		fmt.Printf("Observation window is limited to the newest %d commits.\n", compiled.CommitLimit)
	}
	for _, symbol := range matches {
		fmt.Printf(
			"%s [%s/%s]\n",
			history.EscapeTerminalText(symbol.QualifiedName),
			history.EscapeTerminalText(symbol.Language),
			history.EscapeTerminalText(symbol.Kind),
		)
		fmt.Printf("  first observed: %s\n", shortHash(symbol.FirstObserved))
		fmt.Printf("  last observed:  %s\n", shortHash(symbol.LastObserved))
		fmt.Printf("  files:          %s\n", history.EscapeTerminalText(strings.Join(symbol.Files, ", ")))
		fmt.Printf("  event commits:  %d\n", len(symbol.CommitsTouching))
		for _, signature := range symbol.Signatures {
			fmt.Printf(
				"  %s %s\n",
				shortHash(signature.Commit),
				history.EscapeTerminalText(signature.Signature),
			)
		}
		if symbol.SignatureHistoryTruncated {
			fmt.Println("  signature history truncated at the configured detail bound")
		}
	}
	return nil
}

// runHistoryWithAdmission ensures Git walks and syntax extraction cannot run
// above the established low-priority RKC resource envelope. Parsing and report
// reads happen before this point and remain cheap.
func runHistoryWithAdmission(args []string, local func(context.Context) error) error {
	ctx, stop := signalContext()
	defer stop()
	return runHistoryAdmissionUsing(
		ctx,
		args,
		runtime.GOOS,
		os.Getenv(guardedDirectChildEnvironment),
		os.Getenv(guardedOpenChildEnvironment) == "1",
		resourceguard.PrepareCurrentProcessLowPriority,
		func(protectedContext context.Context, work func(context.Context) error) error {
			return runProtectedDirectLocal(
				protectedContext,
				"history",
				args,
				func(workContext context.Context, _ []string) error { return work(workContext) },
			)
		},
		launchGuardedHistory,
		local,
	)
}

func runHistoryAdmissionUsing(
	ctx context.Context,
	args []string,
	platform, guardedChild string,
	guardedOpenChild bool,
	prepare func() error,
	protected func(context.Context, func(context.Context) error) error,
	launch func(context.Context, []string) error,
	local func(context.Context) error,
) error {
	if ctx == nil || prepare == nil || protected == nil || launch == nil || local == nil {
		return errors.New("history resource admission is not configured")
	}
	if platform != "linux" {
		return local(ctx)
	}
	if guardedChild != "" && guardedChild != "history" {
		return fmt.Errorf("protected history child marker names %q", guardedChild)
	}
	if guardedChild == "history" || guardedOpenChild {
		return protected(ctx, local)
	}
	prepareErr := prepare()
	if prepareErr == nil {
		return protected(ctx, local)
	}
	if err := launch(ctx, args); err != nil {
		return errors.Join(
			fmt.Errorf("current process cannot safely host history work: %w", prepareErr),
			err,
		)
	}
	return nil
}

func launchGuardedHistory(ctx context.Context, args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve RKC executable for protected history: %w", err)
	}
	arguments := make([]string, 0, len(args)+1)
	arguments = append(arguments, "history")
	arguments = append(arguments, args...)
	runner, err := resourceguard.NewCommand(ctx, resourceguard.Config{
		Executable:      executable,
		Arguments:       arguments,
		Environment:     guardedDirectEnvironment("history"),
		MaximumRSSBytes: resourceguard.LowPriorityMemoryMaxBytes,
		UnitPrefix:      "rkc-low",
	})
	if err != nil {
		return fmt.Errorf("configure protected history: %w", err)
	}
	if runner == nil {
		return errors.New("configure protected history: guarded command is not configured")
	}
	if _, err := runner.Run(ctx, os.Stdout, os.Stderr); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("protected history: %w", err)
	}
	return nil
}

func readHistoryFile(path string) (history.History, error) {
	ctx := context.Background()
	input, err := history.PrepareInput(ctx, path)
	if err != nil {
		return history.History{}, err
	}
	return history.ReadCompiledFile(ctx, input)
}

func sortHistorySymbols(symbols []history.SymbolHistory) {
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].QualifiedName == symbols[j].QualifiedName {
			return symbols[i].ID < symbols[j].ID
		}
		return symbols[i].QualifiedName < symbols[j].QualifiedName
	})
}

func shortHash(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func terminalSafeHistoryArgument(value string) bool {
	return value != "" && history.EscapeTerminalText(value) == value
}
