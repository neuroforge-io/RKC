package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/runtime"
)

const defaultTraceOutputPath = ".rkc-trace.json"

func runTrace(args []string) error {
	if len(args) == 0 {
		return traceUsage()
	}
	switch args[0] {
	case "capture":
		return runTraceCaptureWithAdmission(args[1:])
	case "verify":
		return runTraceVerify(args[1:])
	case "report":
		return runTraceReport(args[1:])
	case "help", "--help", "-h":
		return traceUsage()
	default:
		return fmt.Errorf("unknown trace subcommand %q; run 'rkc trace help'", args[0])
	}
}

func traceUsage() error {
	_, err := fmt.Fprint(os.Stdout, `Runtime capture assertions: bounded claims from an authorized command.

  rkc trace capture --dir <repository> [--timeout 30m] [--out .rkc-trace.json] [--environment-key NAME ...] -- <command...>
  rkc trace verify --trace <file>
  rkc trace report --dir <atlas> [--json]

Capture runs an authorized command (typically `+"`go test ./...`"+`) in the
repository and records statement-coverage claims, terminal test-event claims,
exit status, duration, and only environment-variable NAMES selected with repeatable
`+"`--environment-key`"+` flags (never values; none by default). The selection
controls trace metadata only; it does not change the command environment. Go
test runs are automatically instrumented with statement coverage and JSON test
events. Trace schema 1.3 binds canonical source SHA-256 identities plus the
repository content and Git commit observed before and after execution.
Endpoint changes are rejected, but the two inventories cannot prove that no
transient (ABA) mutation occurred during the command. Dynamic subtest suffixes
and arbitrary executable names are not persisted verbatim.

Import a trace into a new snapshot:

  rkc scan --trace .rkc-trace.json --no-python --out .rkc --state-dir .rkc-state .

Every current trace import is a confidence-0.5 operator assertion. Same-process
capture authenticates only that this RKC process produced the exact trace
record; it does not authenticate the command-output producer. Consequently,
current traces never establish canonical function execution, test results,
call events, or confidence-1 runtime truth. Import never invents call-edge or
per-test path evidence from aggregate coverage. The trace report keeps
trace-scoped assertions separate from producer-authenticated observations.
`)
	return err
}

// runTraceCaptureWithAdmission places runtime capture inside the same
// fail-closed low-priority envelope and priority-workload policy used by
// scans.
func runTraceCaptureWithAdmission(args []string) error {
	if traceCaptureHelpRequest(args) {
		return runTraceCapture(context.Background(), args)
	}
	ctx, stop := signalContext()
	defer stop()
	return runDirectCommandWithAdmission(ctx, "trace", append([]string{"capture"}, args...), func(ctx context.Context, admissionArgs []string) error {
		if len(admissionArgs) == 0 || admissionArgs[0] != "capture" {
			return errors.New("trace capture admission lost its subcommand")
		}
		return runTraceCapture(ctx, admissionArgs[1:])
	})
}

func traceCaptureHelpRequest(args []string) bool {
	fs := flag.NewFlagSet("trace capture", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var environmentKeys stringList
	fs.String("dir", "", "")
	fs.String("out", "", "")
	fs.Duration("timeout", 0, "")
	fs.Bool("json", false, "")
	fs.Var(&environmentKeys, "environment-key", "")
	return errors.Is(fs.Parse(args), flag.ErrHelp)
}

func runTraceCapture(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trace capture", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "repository directory to capture in")
	out := fs.String("out", defaultTraceOutputPath, "trace output path")
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum capture duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	var environmentKeys stringList
	fs.Var(&environmentKeys, "environment-key", "environment-variable name to record in trace metadata; repeatable, values are never recorded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("trace capture requires a command after --; for example: rkc trace capture --dir . -- go test ./...")
	}
	if strings.TrimSpace(*dir) == "" {
		return errors.New("--dir is required")
	}
	root, err := resolveScipRepositoryRoot(*dir)
	if err != nil {
		return err
	}
	command := fs.Args()
	result, err := runtime.Capture(ctx, runtime.CaptureOptions{
		Repository: root, Command: command, Timeout: *timeout,
		EnvironmentKeys: append([]string(nil), environmentKeys...),
	})
	if err != nil {
		return err
	}
	if err := runtime.Validate(result.Trace); err != nil {
		return fmt.Errorf("captured trace is invalid: %w", err)
	}
	data, err := jsonMarshalIndent(result.Trace)
	if err != nil {
		return fmt.Errorf("encode trace: %w", err)
	}
	if err := writeAtomic(*out, data, 0o600); err != nil {
		return err
	}
	if *jsonOutput {
		summary := map[string]any{
			"trace": *out, "id": result.Trace.ID, "exit_code": result.Trace.ExitCode,
			"duration_ms": result.Trace.DurationMS, "artifacts": len(result.Trace.Artifacts),
			"tests": len(result.Trace.Tests), "truncated_output": result.Truncated,
			"repository_id":                   result.Trace.Repository.RepositoryID,
			"content_digest":                  result.Trace.Repository.ContentDigest,
			"git_commit":                      result.Trace.Repository.GitCommit,
			"evidence_authority":              "operator_assertion",
			"capture_integrity_authenticated": true,
			"producer_authenticated":          false,
		}
		return writeJSONStdout(summary)
	}
	fmt.Printf("Trace captured: %s\n", *out)
	fmt.Printf("ID: %s\n", result.Trace.ID)
	fmt.Printf("Command: %s\n", result.Trace.Command)
	fmt.Printf("Repository: %s; content: %s\n", result.Trace.Repository.RepositoryID, result.Trace.Repository.ContentDigest)
	if result.Trace.Repository.GitCommit != "" {
		fmt.Printf("Git commit: %s\n", result.Trace.Repository.GitCommit)
	}
	fmt.Printf("Exit: %d; duration: %dms\n", result.Trace.ExitCode, result.Trace.DurationMS)
	fmt.Printf("Covered artifacts: %d; tests: %d\n", len(result.Trace.Artifacts), len(result.Trace.Tests))
	fmt.Println("Authority: operator assertion (capture integrity authenticated; command-output producer unauthenticated).")
	if result.Truncated {
		fmt.Println("Note: captured output was truncated at the safety bound.")
	}
	fmt.Printf("Import: rkc scan --trace %s --no-python [options] %s\n", *out, root)
	return nil
}

func runTraceVerify(args []string) error {
	fs := flag.NewFlagSet("trace verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tracePath := fs.String("trace", "", "trace file to validate")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*tracePath) == "" {
		return errors.New("trace verify requires --trace <file>")
	}
	ctx, stop := signalContext()
	defer stop()
	inputs, _, err := runtime.PrepareTraceInputs(ctx, []string{*tracePath})
	if err != nil {
		return fmt.Errorf("invalid trace: %w", err)
	}
	if len(inputs) != 1 {
		return errors.New("no trace was supplied")
	}
	trace, err := runtime.LoadTrace(ctx, inputs[0])
	if err != nil {
		return fmt.Errorf("trace %q is not valid: %w", *tracePath, err)
	}
	if *jsonOutput {
		return writeJSONStdout(trace)
	}
	fmt.Printf("Valid trace: %s\n", *tracePath)
	fmt.Printf("ID: %s\n", trace.ID)
	fmt.Printf("Command: %s\n", trace.Command)
	fmt.Printf("Repository: %s; content: %s\n", trace.Repository.RepositoryID, trace.Repository.ContentDigest)
	if trace.Repository.GitCommit != "" {
		fmt.Printf("Git commit: %s\n", trace.Repository.GitCommit)
	}
	fmt.Printf("Exit: %d; artifacts: %d; tests: %d\n", trace.ExitCode, len(trace.Artifacts), len(trace.Tests))
	fmt.Println("Authority: portable self-hash and source affinity do not authenticate the command-output producer.")
	return nil
}

func runTraceReport(args []string) error {
	fs := flag.NewFlagSet("trace report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir, database, snapshotID, repositoryID := flowDatasetFlags(fs)
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	dataset, err := flowDataset(fs, args, dir, database, snapshotID, repositoryID)
	if err != nil {
		return err
	}
	diff := runtime.BuildDiff(dataset.Bundle)
	if *jsonOutput {
		return writeJSONStdout(diff)
	}
	fmt.Printf("Static/runtime evidence boundary for snapshot %s\n", dataset.Bundle.Snapshot.ID)
	if len(diff.ProducerAuthenticatedTraceIDs) > 0 {
		fmt.Printf("Producer-authenticated traces: %s\n", strings.Join(diff.ProducerAuthenticatedTraceIDs, ", "))
	} else {
		fmt.Println("Producer-authenticated traces: none")
	}
	if len(diff.UnverifiedAssertionIDs) > 0 {
		fmt.Printf("Runtime assertion traces: %s (not counted as observed execution)\n",
			strings.Join(diff.UnverifiedAssertionIDs, ", "))
	}
	if len(diff.CaptureIntegrityAssertionIDs) > 0 {
		fmt.Printf("Capture-integrity assertions: %s (record integrity only; not producer authority)\n",
			strings.Join(diff.CaptureIntegrityAssertionIDs, ", "))
	}
	fmt.Printf("Producer-authenticated test results: %d passed, %d failed, %d skipped\n",
		diff.ProducerAuthenticatedPassed, diff.ProducerAuthenticatedFailed, diff.ProducerAuthenticatedSkipped)
	fmt.Printf("Functions: %d producer-observed, %d execution-asserted, %d not observed in at least one trace-scoped assertion\n",
		diff.ProducerObservedFunctions, diff.AssertedFunctions, diff.FunctionsNotObserved)
	fmt.Printf("Call graph: %d static edges, %d resolved\n", diff.StaticCallEdges, diff.ResolvedCallEdges)
	if diff.CallObservationAvailable {
		fmt.Printf("Observed: %d (%.1f%% of resolved) | Unobserved: %d\n",
			diff.ProducerObservedCallEdges, diff.ProducerCallObservationRatio*100, diff.UndemonstratedCallEdges)
	} else {
		fmt.Printf("Call-event observation: unavailable (%s); %d resolved calls remain undemonstrated\n",
			diff.CallObservationReason, diff.UndemonstratedCallEdges)
	}
	if len(diff.ExecutionAssertedFunctions) > 0 {
		fmt.Printf("Execution-asserted functions (%d; not canonical execution truth):\n", len(diff.ExecutionAssertedFunctions))
		for _, name := range diff.ExecutionAssertedFunctions {
			fmt.Printf("  %s\n", name)
		}
	}
	if len(diff.NotObservedFunctions) > 0 {
		fmt.Printf("Functions not observed in at least one admitted trace assertion (%d; not dead-code claims):\n", len(diff.NotObservedFunctions))
		for _, name := range diff.NotObservedFunctions {
			fmt.Printf("  %s\n", name)
		}
	}
	if len(diff.UndemonstratedCalls) > 0 {
		fmt.Printf("Undemonstrated static call edges (%d):\n", len(diff.UndemonstratedCalls))
		for _, call := range diff.UndemonstratedCalls {
			fmt.Printf("  %s -> %s\n", call.Caller, call.Callee)
		}
	}
	return nil
}

func jsonMarshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
