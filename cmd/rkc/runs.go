package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/scheduler"
)

const (
	defaultRunListLimit = 50
	maximumRunListLimit = 1000
	runDirectoryBatch   = 256
	maximumListBytes    = int64(256 << 20)
	runReplayAttempts   = 3
)

type runSummary struct {
	RunID              string                 `json:"run_id"`
	Path               string                 `json:"path"`
	State              scheduler.JournalState `json:"state"`
	Terminal           bool                   `json:"terminal"`
	Interrupted        bool                   `json:"interrupted"`
	DiscardedTailBytes int64                  `json:"discarded_tail_bytes,omitempty"`
	LastSequence       uint64                 `json:"last_sequence"`
	StartedAt          time.Time              `json:"started_at"`
	FinishedAt         time.Time              `json:"finished_at,omitempty"`
}

type runListReport struct {
	RunsDirectory string       `json:"runs_directory"`
	Healthy       bool         `json:"healthy"`
	Count         int          `json:"count"`
	Runs          []runSummary `json:"runs"`
	Issues        []runIssue   `json:"issues,omitempty"`
}

type runIssue struct {
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error"`
}

type runFile struct {
	name    string
	path    string
	modTime time.Time
	size    int64
}

func defaultRunJournalDirectory() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(root) == "" {
		if err == nil {
			err = errors.New("user cache directory is empty")
		}
		return "", fmt.Errorf("resolve default RKC run journal directory: %w; use --runs-dir", err)
	}
	return filepath.Join(root, "rkc", "runs"), nil
}

func runJournalFlagDefault(args []string) (string, error) {
	if value, present := discoverExplicitFlagValue(args, "runs-dir"); present {
		return value, nil
	}
	return defaultRunJournalDirectory()
}

func discoverExplicitFlagValue(args []string, name string) (string, bool) {
	prefix := "--" + name + "="
	for index, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix), true
		}
		if argument == "--"+name {
			if index+1 < len(args) {
				return args[index+1], true
			}
			return "", true
		}
	}
	return "", false
}

func runRuns(args []string) error {
	if len(args) == 0 {
		return errors.New("runs subcommand is required: list or show")
	}
	switch args[0] {
	case "list":
		return runRunsList(args[1:])
	case "show":
		return runRunsShow(args[1:])
	default:
		return fmt.Errorf("unknown runs subcommand %q; use list or show", args[0])
	}
}

func runRunsList(args []string) error {
	defaultDirectory, defaultErr := runJournalFlagDefault(args)
	flags := flag.NewFlagSet("runs list", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runsDirectory := flags.String("runs-dir", defaultDirectory, "owner-only scheduler run journal directory")
	limit := flags.Int("limit", defaultRunListLimit, "maximum newest runs to inspect and print")
	jsonOutput := flags.Bool("json", false, "print a machine-readable report")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("runs list does not accept positional arguments")
	}
	if *runsDirectory == "" && defaultErr != nil {
		return defaultErr
	}
	if *limit < 1 || *limit > maximumRunListLimit {
		return fmt.Errorf("--limit must be between 1 and %d", maximumRunListLimit)
	}
	root, files, issues, err := listRunFiles(*runsDirectory, *limit)
	if err != nil {
		return err
	}
	report := runListReport{
		RunsDirectory: root,
		Runs:          make([]runSummary, 0, len(files)),
		Issues:        issues,
	}
	var replayedBytes int64
	for _, file := range files {
		if file.size > maximumListBytes-replayedBytes {
			report.Issues = append(report.Issues, runIssue{
				Name: file.name, Path: file.path,
				Error: fmt.Sprintf("aggregate replay limit of %d bytes reached", maximumListBytes),
			})
			continue
		}
		replayed, err := readRunJournalWithRetry(file.path)
		if err != nil {
			report.Issues = append(report.Issues, runIssue{
				Name: file.name, Path: file.path, Error: err.Error(),
			})
			continue
		}
		replayedBytes += file.size
		report.Runs = append(report.Runs, summarizeRun(replayed))
	}
	report.Count = len(report.Runs)
	report.Healthy = len(report.Issues) == 0
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	fmt.Printf("Run journals: %q\n", report.RunsDirectory)
	for _, run := range report.Runs {
		fmt.Printf(
			"%s  %-10s  sequence=%d  terminal=%t  interrupted=%t  started=%s\n",
			run.RunID,
			run.State,
			run.LastSequence,
			run.Terminal,
			run.Interrupted,
			run.StartedAt.Format(time.RFC3339Nano),
		)
	}
	fmt.Printf("%d run(s) shown.\n", report.Count)
	for _, issue := range report.Issues {
		fmt.Printf("WARNING %s: %s\n", issue.Name, issue.Error)
	}
	if !report.Healthy {
		fmt.Printf("%d invalid or unavailable run journal(s) skipped.\n", len(report.Issues))
	}
	return nil
}

func runRunsShow(args []string) error {
	defaultDirectory, defaultErr := runJournalFlagDefault(args)
	flags := flag.NewFlagSet("runs show", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runsDirectory := flags.String("runs-dir", defaultDirectory, "owner-only scheduler run journal directory")
	jsonOutput := flags.Bool("json", false, "print the complete machine-readable replay")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("runs show requires exactly one run ID")
	}
	if *runsDirectory == "" && defaultErr != nil {
		return defaultErr
	}
	runID := flags.Arg(0)
	if err := scheduler.ValidateRunID(runID); err != nil {
		return fmt.Errorf("validate scheduler run ID: %w", err)
	}
	root, err := validateRunDirectory(*runsDirectory)
	if err != nil {
		return err
	}
	path := filepath.Join(root, runID+".jsonl")
	report, err := readRunJournalWithRetry(path)
	if err != nil {
		return fmt.Errorf("strictly replay scheduler run %s: %w", runID, err)
	}
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	summary := summarizeRun(report)
	fmt.Printf("Run: %s\n", summary.RunID)
	fmt.Printf("Journal: %q\n", summary.Path)
	fmt.Printf("State: %s\n", summary.State)
	fmt.Printf("Terminal: %t\n", summary.Terminal)
	fmt.Printf("Interrupted: %t\n", summary.Interrupted)
	if summary.DiscardedTailBytes > 0 {
		fmt.Printf("Discarded tail: %d byte(s)\n", summary.DiscardedTailBytes)
	}
	fmt.Printf("Sequence: %d\n", summary.LastSequence)
	fmt.Printf("Started: %s\n", summary.StartedAt.Format(time.RFC3339Nano))
	if !summary.FinishedAt.IsZero() {
		fmt.Printf("Finished: %s\n", summary.FinishedAt.Format(time.RFC3339Nano))
	}
	fmt.Println("Records:")
	for _, record := range report.Records {
		if record.Kind == scheduler.JournalKindRun {
			fmt.Printf("  %d  run    %-10s  at=%s", record.Sequence, record.State, record.OccurredAt.Format(time.RFC3339Nano))
		} else {
			fmt.Printf(
				"  %d  stage  %-10s  id=%q attempt=%d at=%s",
				record.Sequence,
				record.State,
				record.StageID,
				record.Attempt,
				record.OccurredAt.Format(time.RFC3339Nano),
			)
		}
		if record.Error != "" {
			fmt.Printf(" error=%q", record.Error)
		}
		fmt.Println()
	}
	return nil
}

func validateRunDirectory(directory string) (string, error) {
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("--runs-dir is required")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve run journal directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect run journal directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("run journal path is not a non-symlink directory")
	}
	if err := scheduler.ValidateJournalRootPrivacy(root); err != nil {
		return "", err
	}
	return root, nil
}

func listRunFiles(directory string, limit int) (string, []runFile, []runIssue, error) {
	if strings.TrimSpace(directory) == "" {
		return "", nil, nil, errors.New("--runs-dir is required")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve run journal directory: %w", err)
	}
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return root, []runFile{}, nil, nil
	}
	root, err = validateRunDirectory(root)
	if err != nil {
		return "", nil, nil, err
	}
	directoryFile, err := os.Open(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open run journal directory: %w", err)
	}
	defer directoryFile.Close()
	opened, err := directoryFile.Stat()
	if err != nil || !opened.IsDir() {
		return "", nil, nil, errors.New("run journal directory identity is unavailable")
	}
	current, err := os.Lstat(root)
	if err != nil || !os.SameFile(opened, current) {
		return "", nil, nil, errors.New("run journal directory identity changed")
	}
	files := make([]runFile, 0, limit+runDirectoryBatch)
	var issues []runIssue
	for {
		entries, readErr := directoryFile.ReadDir(runDirectoryBatch)
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(root, name)
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
				!strings.HasSuffix(name, ".jsonl") {
				issues = append(issues, runIssue{
					Name: name, Path: path, Error: "unexpected non-journal directory entry",
				})
				continue
			}
			runID := strings.TrimSuffix(name, ".jsonl")
			if err := scheduler.ValidateRunID(runID); err != nil {
				issues = append(issues, runIssue{
					Name: name, Path: path, Error: err.Error(),
				})
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil {
				issues = append(issues, runIssue{
					Name: name, Path: path, Error: err.Error(),
				})
				continue
			}
			if !entryInfo.Mode().IsRegular() {
				issues = append(issues, runIssue{
					Name: name, Path: path,
					Error: "entry is not a regular file",
				})
				continue
			}
			if err := scheduler.ValidateJournalFilePrivacy(path); err != nil {
				issues = append(issues, runIssue{
					Name: name, Path: path, Error: err.Error(),
				})
				continue
			}
			files = append(files, runFile{
				name: runID, path: path,
				modTime: entryInfo.ModTime().UTC(), size: entryInfo.Size(),
			})
		}
		if len(files) > limit*2 {
			sortRunFiles(files)
			files = files[:limit]
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", nil, nil, fmt.Errorf("list run journal directory: %w", readErr)
		}
	}
	final, err := os.Lstat(root)
	if err != nil || !os.SameFile(opened, final) {
		return "", nil, nil, errors.New("run journal directory changed while listing")
	}
	sortRunFiles(files)
	if len(files) > limit {
		files = files[:limit]
	}
	return root, files, issues, nil
}

func sortRunFiles(files []runFile) {
	sort.Slice(files, func(left, right int) bool {
		if files[left].modTime.Equal(files[right].modTime) {
			return files[left].name < files[right].name
		}
		return files[left].modTime.After(files[right].modTime)
	})
}

func readRunJournalWithRetry(path string) (scheduler.JournalReport, error) {
	var replay scheduler.JournalReport
	var err error
	for attempt := 1; attempt <= runReplayAttempts; attempt++ {
		replay, err = scheduler.ReadFileJournal(path)
		if err == nil {
			return replay, nil
		}
		if attempt < runReplayAttempts {
			time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
		}
	}
	return replay, err
}

func summarizeRun(report scheduler.JournalReport) runSummary {
	summary := runSummary{
		RunID:              report.RunID,
		Path:               report.Path,
		State:              scheduler.JournalState(report.State),
		Terminal:           report.Terminal,
		Interrupted:        report.Interrupted,
		DiscardedTailBytes: report.DiscardedTailBytes,
		LastSequence:       report.LastSequence,
	}
	if len(report.Records) > 0 {
		summary.StartedAt = report.Records[0].OccurredAt
		if report.Terminal {
			summary.FinishedAt = report.Records[len(report.Records)-1].OccurredAt
		}
	}
	return summary
}
