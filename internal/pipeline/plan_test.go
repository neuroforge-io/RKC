package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/history"
	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
	"github.com/neuroforge-io/RKC/internal/runtime"
)

func TestPlanReportsColdWarmAndSelectiveInvalidation(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, ".env"), "PORT=8787\n")
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Fixture\n")
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\nfunc Run() {}\n")
	mustWritePipelineFile(t, filepath.Join(root, "web.ts"), "export const ready = true\n")
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Root: root, ToolVersion: "plan-test", DisablePythonAST: true, Cache: cache,
	}
	cold, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if cold.Root != root || cold.CacheRoot != cache.Root() ||
		cold.Summary != (PlanSummary{Stages: 20, Execute: 16, Disabled: 4}) {
		t.Fatalf("cold plan = %+v", cold)
	}
	if len(cold.EvidenceOpportunities) != 3 ||
		cold.EvidenceOpportunities[0].Status != "not_supplied" ||
		cold.EvidenceOpportunities[1].Status != "not_supplied" ||
		cold.EvidenceOpportunities[2].Status != "not_supplied" {
		t.Fatalf("cold evidence opportunities = %+v", cold.EvidenceOpportunities)
	}
	if got := strings.Join(cold.EvidenceOpportunities[1].Command, " "); !strings.Contains(got, ".rkc-trace.json") {
		t.Fatalf("runtime evidence command can be recursively ingested: %q", got)
	}
	if got := strings.Join(cold.EvidenceOpportunities[2].Command, " "); !strings.Contains(got, ".rkc-history.json") {
		t.Fatalf("history evidence command can be recursively ingested: %q", got)
	}
	if _, _, err := Scan(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	warm, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Summary != (PlanSummary{Stages: 20, Execute: 7, CacheHit: 9, Disabled: 4}) {
		t.Fatalf("warm plan summary = %+v", warm.Summary)
	}
	for _, stage := range warm.Stages {
		if stage.Cacheable && stage.Enabled &&
			(stage.Disposition != "cache-hit" || stage.CacheKey == "") {
			t.Errorf("warm cacheable stage = %+v", stage)
		}
	}

	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Fixture changed\n")
	selective, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if selective.Summary != (PlanSummary{Stages: 20, Execute: 11, CacheHit: 5, Disabled: 4}) {
		t.Fatalf("selective plan summary = %+v", selective.Summary)
	}
	for _, stageID := range []string{"markdown", "manifests", "secret-scan"} {
		stage := plannedStage(t, selective, stageID)
		if stage.Disposition != "execute" ||
			stage.Reason != "stage inputs or relevant configuration changed" {
			t.Errorf("invalidated %s plan = %+v", stageID, stage)
		}
	}
	for _, stageID := range []string{"go-syntax", "typescript-syntax"} {
		if stage := plannedStage(t, selective, stageID); stage.Disposition != "cache-hit" {
			t.Errorf("retained %s plan = %+v", stageID, stage)
		}
	}

	cleanOptions := options
	cleanOptions.Cache = nil
	clean, err := Plan(context.Background(), cleanOptions)
	if err != nil {
		t.Fatal(err)
	}
	if clean.CacheRoot != "" ||
		clean.Summary != (PlanSummary{Stages: 20, Execute: 16, Disabled: 4}) {
		t.Fatalf("clean plan = %+v", clean)
	}
}

func TestEvidenceOpportunitiesReportAdmittedAuthorityWithoutExecuting(t *testing.T) {
	state := &stagedScanState{
		scipInputs:   make([]scipindex.Input, 2),
		traceInputs:  make([]runtime.TraceInput, 1),
		historyInput: history.Input{Path: "history.json"},
	}
	got := evidenceOpportunities(state)
	if len(got) != 3 {
		t.Fatalf("evidence opportunities = %+v", got)
	}
	wantStatuses := []string{"admitted_assertion", "admitted_assertion", "admitted"}
	for index, wantCount := range []int{2, 1, 1} {
		if got[index].Status != wantStatuses[index] || got[index].AdmittedInputs != wantCount ||
			len(got[index].Command) != 0 || got[index].RequiresAuthorization {
			t.Errorf("admitted opportunity %d = %+v", index, got[index])
		}
	}
	if got[0].Authority != "structured_index_assertion" || got[1].Kind != "runtime_capture_assertion" ||
		got[1].Authority != "operator_assertion" {
		t.Fatalf("evidence authorities were overstated: %+v", got)
	}
}

func TestEvidenceOpportunitiesReportMixedCompilerAuthority(t *testing.T) {
	dir := t.TempDir()
	authenticatedPath := filepath.Join(dir, "authenticated.scip")
	externalPath := filepath.Join(dir, "external.scip")
	mustWritePipelineFile(t, authenticatedPath, "authenticated-index")
	mustWritePipelineFile(t, externalPath, "external-index")
	prepared, _, err := scipindex.PrepareInputs(context.Background(), []string{authenticatedPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := scipindex.MarkGeneratedByCurrentProcess(prepared[0]); err != nil {
		t.Fatal(err)
	}
	inputs, _, err := scipindex.PrepareInputs(context.Background(), []string{externalPath, authenticatedPath})
	if err != nil {
		t.Fatal(err)
	}
	got := evidenceOpportunities(&stagedScanState{scipInputs: inputs})[0]
	if got.Status != "admitted_mixed_authority" || got.Authority != "mixed" ||
		got.AdmittedInputs != 2 || !strings.Contains(got.Detail, "1 of 2") {
		t.Fatalf("mixed compiler authority = %+v", got)
	}

	var authenticatedInputs []scipindex.Input
	for _, input := range inputs {
		if input.CompilerAuthenticated() {
			authenticatedInputs = append(authenticatedInputs, input)
		}
	}
	authenticatedOnly := evidenceOpportunities(&stagedScanState{scipInputs: authenticatedInputs})[0]
	if authenticatedOnly.Status != "admitted_compiler_authenticated" || authenticatedOnly.Authority != "compiler" {
		t.Fatalf("authenticated compiler authority = %+v", authenticatedOnly)
	}
}

func TestPlanValidation(t *testing.T) {
	if _, err := Plan(nil, Options{}); err == nil {
		t.Fatal("Plan(nil) succeeded")
	}
	if _, err := Plan(context.Background(), Options{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("Plan(missing) succeeded")
	}
	file := filepath.Join(t.TempDir(), "file")
	mustWritePipelineFile(t, file, "x")
	if _, err := Plan(context.Background(), Options{Root: file}); err == nil {
		t.Fatal("Plan(file) succeeded")
	}
}

func plannedStage(t *testing.T, plan ScanPlan, stageID string) StagePlan {
	t.Helper()
	for _, stage := range plan.Stages {
		if stage.ID == stageID {
			return stage
		}
	}
	t.Fatalf("stage %s missing from plan", stageID)
	return StagePlan{}
}
