package pipeline

import (
	"context"
	"path/filepath"
	"testing"
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
		cold.Summary != (PlanSummary{Stages: 15, Execute: 14, Disabled: 1}) {
		t.Fatalf("cold plan = %+v", cold)
	}
	if _, _, err := Scan(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	warm, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Summary != (PlanSummary{Stages: 15, Execute: 6, CacheHit: 8, Disabled: 1}) {
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
	if selective.Summary != (PlanSummary{Stages: 15, Execute: 9, CacheHit: 5, Disabled: 1}) {
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
		clean.Summary != (PlanSummary{Stages: 15, Execute: 14, Disabled: 1}) {
		t.Fatalf("clean plan = %+v", clean)
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
