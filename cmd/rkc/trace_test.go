package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/runtime"
)

func traceRepositoryFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/trace\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `package trace

func Alpha() string { return "alpha" }

func Beta() string { return Alpha() }

func Gamma() string { return "never called" }
`
	if err := os.WriteFile(filepath.Join(root, "trace.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	testSource := `package trace

import "testing"

func TestBeta(t *testing.T) {
	if Beta() != "alpha" {
		t.Fatal("bad")
	}
}
`
	if err := os.WriteFile(filepath.Join(root, "trace_test.go"), []byte(testSource), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestTraceCaptureCommand(t *testing.T) {
	root := traceRepositoryFixture(t)
	out := filepath.Join(t.TempDir(), "trace.json")
	if err := runTraceCapture(context.Background(), []string{"--dir", root, "--out", out, "--timeout", "2m", "--", "go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var trace runtime.Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.ID == "" || len(trace.Artifacts) == 0 || len(trace.Tests) != 1 {
		t.Fatalf("trace = %+v", trace)
	}
	if err := runTrace([]string{"verify", "--trace", out}); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"verify", "--trace", out, "--json"}); err != nil {
		t.Fatal(err)
	}
}

func TestTraceCommandValidation(t *testing.T) {
	if err := runTrace(nil); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown trace subcommand") {
		t.Fatalf("unknown trace subcommand = %v", err)
	}
	if err := runTraceCapture(context.Background(), []string{"--dir", "."}); err == nil {
		t.Fatal("capture without command succeeded")
	}
	if err := runTrace([]string{"verify"}); err == nil {
		t.Fatal("verify without --trace succeeded")
	}
	if err := runTrace([]string{"verify", "--trace", filepath.Join(t.TempDir(), "absent.json")}); err == nil {
		t.Fatal("verify of a missing trace succeeded")
	}
}

func TestTraceCaptureAndImportEndToEnd(t *testing.T) {
	root := traceRepositoryFixture(t)
	out := filepath.Join(t.TempDir(), "trace.json")
	if err := runTraceCapture(context.Background(), []string{"--dir", root, "--out", out, "--timeout", "2m", "--", "go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	atlas := filepath.Join(t.TempDir(), "atlas")
	state := filepath.Join(t.TempDir(), "state")
	if err := runScanContext(context.Background(), []string{
		"--out", atlas, "--state-dir", state,
		"--trace", out,
		"--no-python", "--no-typescript", "--no-frameworks",
		"--no-static-site", "--no-integrations", "--force",
		root,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(atlas, "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var coverage coverageShape
	if err := json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	if coverage.RuntimeTraces != 1 || coverage.RuntimeAssertionTraces != 1 ||
		coverage.RuntimeCaptureIntegrityAssertions != 1 || coverage.RuntimeProducerAuthenticatedTraces != 0 ||
		coverage.RuntimeProducerAuthenticatedTests != 0 || coverage.RuntimeFunctionsExecutionAsserted < 1 {
		t.Fatalf("runtime assertion boundary missing: %+v", coverage)
	}
	// The report must keep statement-coverage assertions separate from
	// producer-authenticated execution and call-event observations.
	if err := runTrace([]string{"report", "--dir", atlas}); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"report", "--dir", atlas, "--json"}); err != nil {
		t.Fatal(err)
	}
}

type coverageShape struct {
	RuntimeTraces                           int     `json:"runtime_traces"`
	RuntimeProducerAuthenticatedTraces      int     `json:"runtime_producer_authenticated_traces"`
	RuntimeAssertionTraces                  int     `json:"runtime_assertion_traces"`
	RuntimeCaptureIntegrityAssertions       int     `json:"runtime_capture_integrity_assertions"`
	RuntimeProducerAuthenticatedTests       int     `json:"runtime_producer_authenticated_tests"`
	RuntimeProducerObservedCallEdges        int     `json:"runtime_producer_observed_call_edges"`
	RuntimeFunctionsExecutionAsserted       int     `json:"runtime_functions_execution_asserted"`
	RuntimeFunctionsNotObserved             int     `json:"runtime_functions_not_observed"`
	RuntimeCallObservationAvailable         bool    `json:"runtime_call_observation_available"`
	RuntimeProducerCallEdgeObservationRatio float64 `json:"runtime_producer_call_edge_observation_ratio"`
	FlowCFGBlocks                           int     `json:"flow_cfg_blocks"`
}

func TestTraceReportValidation(t *testing.T) {
	root := traceRepositoryFixture(t)
	atlas := filepath.Join(t.TempDir(), "atlas")
	state := filepath.Join(t.TempDir(), "state")
	if err := runScanContext(context.Background(), []string{
		"--out", atlas, "--state-dir", state,
		"--no-python", "--no-typescript", "--no-frameworks",
		"--no-static-site", "--no-integrations", "--force",
		root,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"report", "--dir", atlas}); err != nil {
		t.Fatal(err)
	}
	if err := runTrace([]string{"report", "--dir", filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("report of a missing atlas succeeded")
	}
	if err := runTrace([]string{"report", "--dir", atlas, "extra"}); err == nil {
		t.Fatal("positional arguments were accepted")
	}
	if err := runTraceCapture(context.Background(), []string{"--dir", filepath.Join(t.TempDir(), "absent"), "--", "true"}); err == nil {
		t.Fatal("capture in a missing directory succeeded")
	}
}

func TestTraceCaptureTimeout(t *testing.T) {
	root := traceRepositoryFixture(t)
	started := time.Now()
	err := runTraceCapture(context.Background(), []string{"--dir", root, "--out", filepath.Join(t.TempDir(), "t.json"), "--timeout", "1s", "--", "sleep", "5"})
	if err == nil {
		t.Fatal("slow capture did not time out")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestTraceCaptureAdmissionFailsClosed(t *testing.T) {
	// A same-command child is valid when this test already runs inside the
	// required low-priority envelope. A cross-command marker is unconditionally
	// invalid and therefore exercises fail-closed admission deterministically.
	t.Setenv("RKC_GUARDED_DIRECT_CHILD", "scan")
	if err := runTraceCaptureWithAdmission([]string{"--dir", ".", "--", "true"}); err == nil ||
		!strings.Contains(err.Error(), "cross-command admission") {
		t.Fatalf("cross-command guarded-child admission = %v", err)
	}
}

func TestTraceCaptureEnvironmentKeysAreExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	const selected = "RKC_TRACE_TEST_SELECTED"
	const second = "RKC_TRACE_TEST_SECOND"
	const secretValue = "must-not-appear-in-trace-782e11"
	t.Setenv(selected, secretValue)
	t.Setenv(second, "another-private-value")

	defaultOutput := filepath.Join(t.TempDir(), "default.json")
	if err := runTraceCapture(context.Background(), []string{
		"--dir", root, "--out", defaultOutput, "--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	defaultTrace := readTraceFixture(t, defaultOutput)
	if len(defaultTrace.EnvironmentKeys) != 0 {
		t.Fatalf("default capture enumerated host environment keys: %v", defaultTrace.EnvironmentKeys)
	}

	selectedOutput := filepath.Join(t.TempDir(), "selected.json")
	if err := runTraceCapture(context.Background(), []string{
		"--dir", root, "--out", selectedOutput,
		"--environment-key", selected,
		"--environment-key", second,
		"--environment-key", selected,
		"--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	selectedTrace := readTraceFixture(t, selectedOutput)
	if got := strings.Join(selectedTrace.EnvironmentKeys, ","); got != second+","+selected {
		t.Fatalf("selected environment keys = %q", got)
	}
	encoded, err := json.Marshal(selectedTrace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretValue) || strings.Contains(string(encoded), "another-private-value") {
		t.Fatalf("trace disclosed an environment value: %s", encoded)
	}
	if _, err := validateDirectCommandAdmission("trace", []string{
		"capture", "--dir", root, "--environment-key", selected, "--", "true",
	}); err != nil {
		t.Fatalf("environment-key admission grammar = %v", err)
	}
	if err := runTraceCapture(context.Background(), []string{
		"--dir", root, "--environment-key", "NOT-VALID", "--", "true",
	}); err == nil || !strings.Contains(err.Error(), "environment key") {
		t.Fatalf("invalid environment key = %v", err)
	}
}

func TestTraceCaptureDefaultOutputIsHiddenGeneratedEvidence(t *testing.T) {
	working := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := runTraceCapture(context.Background(), []string{
		"--dir", working, "--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(working, defaultTraceOutputPath)
	trace := readTraceFixture(t, generated)
	if trace.ID == "" {
		t.Fatal("default trace output is incomplete")
	}
	if _, err := os.Stat(filepath.Join(working, "trace.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy visible default trace.json exists: %v", err)
	}
}

func readTraceFixture(t *testing.T, path string) runtime.Trace {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace runtime.Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatal(err)
	}
	return trace
}
