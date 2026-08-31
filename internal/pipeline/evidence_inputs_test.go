package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/neuroforge-io/RKC/internal/configenv"
	"github.com/neuroforge-io/RKC/internal/flow"
	"github.com/neuroforge-io/RKC/internal/framework/envkeys"
	"github.com/neuroforge-io/RKC/internal/framework/manifest"
	"github.com/neuroforge-io/RKC/internal/history"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/runtime"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestImportedEvidenceBindsSnapshotIdentityAndSequentialOracle(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\n\nfunc Run() {}\n")
	compiledHistory := pipelineHistoryFixture(t, root)
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	historyPath := filepath.Join(t.TempDir(), "history.json")
	writePipelineJSON(t, tracePath, pipelineTrace(t, root, "TRACE_ONE"))
	writePipelineJSON(t, historyPath, compiledHistory)
	opts := Options{
		Root: root, ToolVersion: "evidence-identity-test",
		DisablePythonAST: true, DisableTypeScript: true,
		DisableFrameworks: true, DisableSecretScan: true,
		TracePaths: []string{tracePath}, HistoryPath: historyPath,
	}

	first, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	firstTraceDigest := first.Snapshot.Metadata["trace_input_digest"]
	firstHistoryDigest := first.Snapshot.Metadata["history_input_digest"]
	if len(firstTraceDigest) != 64 || len(firstHistoryDigest) != 64 {
		t.Fatalf("input metadata is not digest-bound: %+v", first.Snapshot.Metadata)
	}

	writePipelineJSON(t, tracePath, pipelineTrace(t, root, "TRACE_TWO"))
	second, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Snapshot.ID == first.Snapshot.ID ||
		second.Snapshot.Metadata["trace_input_digest"] == firstTraceDigest {
		t.Fatalf("trace change did not change snapshot identity: %q", second.Snapshot.ID)
	}
	if second.Snapshot.Metadata["history_input_digest"] != firstHistoryDigest {
		t.Fatal("unchanged history digest drifted after a trace-only change")
	}

	compiledHistory.DetailsTruncated = true
	writePipelineJSON(t, historyPath, compiledHistory)
	third, thirdCoverage, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if third.Snapshot.ID == second.Snapshot.ID ||
		third.Snapshot.Metadata["history_input_digest"] == firstHistoryDigest {
		t.Fatalf("history change did not change snapshot identity: %q", third.Snapshot.ID)
	}

	oracle, oracleCoverage, err := scanSequential(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	stagedJSON, err := rkcmodel.CanonicalJSON(third)
	if err != nil {
		t.Fatal(err)
	}
	oracleJSON, err := rkcmodel.CanonicalJSON(oracle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stagedJSON, oracleJSON) ||
		thirdCoverage.DeterministicOutputDigest != oracleCoverage.DeterministicOutputDigest {
		t.Fatal("staged and sequential scans disagree when trace and history inputs are enabled")
	}
}

func TestPlanValidatesAndReportsEvidenceInputs(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\n")
	compiledHistory := pipelineHistoryFixture(t, root)
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	historyPath := filepath.Join(t.TempDir(), "history.json")
	writePipelineJSON(t, tracePath, pipelineTrace(t, root, "PLAN_TRACE"))
	writePipelineJSON(t, historyPath, compiledHistory)
	opts := Options{
		Root: root, DisablePythonAST: true,
		TracePaths: []string{tracePath}, HistoryPath: historyPath,
	}

	plan, err := Plan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, stageID := range []string{"trace-import", "history-import"} {
		stage := plannedStage(t, plan, stageID)
		if !stage.Enabled || stage.Disposition != "execute" || stage.Reason == "" {
			t.Fatalf("planned %s stage = %+v", stageID, stage)
		}
	}

	mustWritePipelineFile(t, tracePath, "{}\n")
	if _, err := Plan(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "validate runtime trace") {
		t.Fatalf("Plan(invalid trace) = %v", err)
	}
	writePipelineJSON(t, tracePath, pipelineTrace(t, root, "PLAN_TRACE"))
	mustWritePipelineFile(t, historyPath, "{}\n")
	if _, err := Plan(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "validate history input") {
		t.Fatalf("Plan(invalid history) = %v", err)
	}
}

func pipelineHistoryFixture(t *testing.T, root string) history.History {
	t.Helper()
	commands := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "rkc@example.invalid"},
		{"config", "user.name", "RKC fixture"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-q", "-m", "fixture"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", arguments...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	compiled, err := history.Build(context.Background(), history.Options{Repository: root})
	if err != nil {
		t.Fatalf("build history fixture: %v", err)
	}
	return compiled
}

func TestFinalSourceRevalidationCoversPostMergeAnalyzers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	mustWritePipelineFile(t, path, "package fixture\n\nfunc Original() {}\n")
	var once sync.Once
	var mutationErr error
	opts := Options{
		Root: root, ToolVersion: "final-revalidation-test", DisablePythonAST: true,
		OnStageEvent: func(event scheduler.Event) {
			if event.StageID == "config-env" && event.State == "complete" {
				once.Do(func() {
					mutationErr = os.WriteFile(path, []byte("package fixture\n\nfunc Changed() {}\n"), 0o600)
				})
			}
		},
	}
	if _, _, err := Scan(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "source changed after adapters") {
		t.Fatalf("Scan(source changed after config-env) = %v", err)
	}
	if mutationErr != nil {
		t.Fatalf("mutate source fixture: %v", mutationErr)
	}
}

func TestConfigEnvironmentFactsAreMergedAndCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "feature.go"),
		"//go:build linux && rkc_feature\n\npackage fixture\n")
	opts := Options{
		Root: root, ToolVersion: "config-env-integration-test",
		DisablePythonAST: true, DisableTypeScript: true,
	}

	bundle, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	foundBuildTarget := false
	for _, node := range bundle.Nodes {
		if node.Kind == "build_target" && node.Attributes["constraint"] == "linux && rkc_feature" {
			foundBuildTarget = true
			break
		}
	}
	if !foundBuildTarget {
		t.Fatal("config-env build target was not merged into the canonical bundle")
	}
	oracle, _, err := scanSequential(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	stagedJSON, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	oracleJSON, err := rkcmodel.CanonicalJSON(oracle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stagedJSON, oracleJSON) {
		t.Fatal("staged and sequential config-env output disagree")
	}

	disabled := opts
	disabled.DisableFrameworks = true
	disabledBundle, _, err := Scan(context.Background(), disabled)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range disabledBundle.Nodes {
		if node.Kind == "build_target" {
			t.Fatalf("disabled config-env emitted build target %s", node.ID)
		}
	}
	plan, err := Plan(context.Background(), disabled)
	if err != nil {
		t.Fatal(err)
	}
	stage := plannedStage(t, plan, "config-env")
	if stage.Enabled || stage.Disposition != "disabled" {
		t.Fatalf("disabled config-env plan = %+v", stage)
	}
}

func TestAnalyzerStageConfigurationsBindProducerVersions(t *testing.T) {
	expected := map[string][2]string{
		"env-keys":   {envkeys.PluginID, envkeys.PluginVersion},
		"manifests":  {manifest.PluginID, manifest.PluginVersion},
		"value-flow": {flow.PluginID, flow.PluginVersion},
		"config-env": {configenv.PluginID, configenv.PluginVersion},
	}
	stages := (&stagedScanState{opts: Options{}}).stages()
	for stageID, producer := range expected {
		var configuration map[string]any
		for _, stage := range stages {
			if stage.ID != stageID {
				continue
			}
			configuration, _ = stage.Configuration.(map[string]any)
			break
		}
		if settings, ok := configuration["settings"].(map[string]any); ok {
			configuration = settings
		}
		if configuration["plugin_id"] != producer[0] ||
			configuration["plugin_version"] != producer[1] {
			t.Fatalf("%s cache configuration does not bind its producer: %+v", stageID, configuration)
		}
	}
}

func TestConfigEnvironmentStageDoesNotCacheAnalyzerFailures(t *testing.T) {
	state := &stagedScanState{opts: Options{}}
	result, err := state.runConfigEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.DoNotCache {
		t.Fatal("config-env analyzer failure was eligible for persistent cache reuse")
	}
	if len(state.bundle.Diagnostics) != 1 || state.bundle.Diagnostics[0].Code != "RKC-CFG-3003" {
		t.Fatalf("config-env analyzer failure diagnostic = %+v", state.bundle.Diagnostics)
	}
}

func writePipelineJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	mustWritePipelineFile(t, path, string(data)+"\n")
}

func pipelineTrace(t *testing.T, root, environmentKey string) runtime.Trace {
	t.Helper()
	result, err := inventory.Scan(inventory.Options{
		Root: root, Excludes: inventory.DefaultExclusions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = root
	commitOutput, err := command.Output()
	if err != nil {
		t.Fatalf("resolve history fixture head: %v", err)
	}
	contentHash := sha256.New()
	artifactCount := 0
	for _, artifact := range result.Artifacts {
		if artifact.Path == ".rkc-history.json" || artifact.Path == ".rkc-trace.json" {
			continue
		}
		fmt.Fprintf(
			contentHash,
			"%s\x00%s\x00%d\x00%s\x00%s\n",
			artifact.Path,
			artifact.Kind,
			artifact.SizeBytes,
			artifact.SHA256,
			artifact.Target,
		)
		artifactCount++
	}
	trace := runtime.Trace{
		SchemaVersion:    runtime.SchemaVersion,
		Command:          "go",
		CommandSHA256:    strings.Repeat("1", 64),
		WorkingDirectory: ".",
		EnvironmentKeys:  []string{environmentKey},
		Repository: runtime.TraceRepository{
			RepositoryID:  rkcmodel.StableID("repository", filepath.Base(root)),
			ContentDigest: hex.EncodeToString(contentHash.Sum(nil)), ArtifactCount: artifactCount,
			GitCommit: strings.TrimSpace(string(commitOutput)),
		},
	}
	trace.ID = runtime.IDFor(trace)
	return trace
}
