package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/history"
	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
	"github.com/neuroforge-io/RKC/internal/runtime"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// StagePlan records one effective DAG stage, its cache identity, and the reason
// it will execute, reuse a cache entry, or remain disabled.
type StagePlan struct {
	ID           string                    `json:"id"`
	Version      string                    `json:"version"`
	Dependencies []string                  `json:"dependencies"`
	Enabled      bool                      `json:"enabled"`
	Cacheable    bool                      `json:"cacheable"`
	Resources    scheduler.ResourceRequest `json:"resources"`
	Disposition  string                    `json:"disposition"`
	CacheKey     string                    `json:"cache_key,omitempty"`
	Reason       string                    `json:"reason"`
}

// ScanPlan is the read-only preview of a scan after source inventory,
// normalization, SCIP admission, and cache probing have completed.
type ScanPlan struct {
	Root                  string                `json:"root"`
	CacheRoot             string                `json:"cache_root,omitempty"`
	Stages                []StagePlan           `json:"stages"`
	Summary               PlanSummary           `json:"summary"`
	EvidenceOpportunities []EvidenceOpportunity `json:"evidence_opportunities"`
}

// EvidenceOpportunity makes missing higher-authority inputs explicit without
// acquiring or executing anything during planning. Command is an exact argv
// template for a separately authorized next step, never a shell string.
type EvidenceOpportunity struct {
	Kind                  string   `json:"kind"`
	Authority             string   `json:"authority"`
	Status                string   `json:"status"`
	AdmittedInputs        int      `json:"admitted_inputs"`
	Detail                string   `json:"detail"`
	Command               []string `json:"command,omitempty"`
	RequiresAuthorization bool     `json:"requires_authorization"`
}

// PlanSummary partitions every planned stage by effective disposition.
type PlanSummary struct {
	Stages   int `json:"stages"`
	Execute  int `json:"execute"`
	CacheHit int `json:"cache_hit"`
	Disabled int `json:"disabled"`
}

// Plan inventories and normalizes source as read-only planning inputs, then
// calculates every analyzer cache key without executing an analyzer or
// publishing output.
func Plan(ctx context.Context, opts Options) (ScanPlan, error) {
	if ctx == nil {
		return ScanPlan{}, errors.New("pipeline plan context is required")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return ScanPlan{}, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return ScanPlan{}, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return ScanPlan{}, fmt.Errorf("root is not a directory: %s", root)
	}
	state := &stagedScanState{
		opts:           opts,
		root:           root,
		artifactByPath: map[string]string{},
		fragments:      map[string]rkcmodel.Fragment{},
		parsed:         map[string]struct{}{},
	}
	state.scipInputs, state.scipDigest, err = scipindex.PrepareInputs(ctx, enabledSCIPIndexes(opts))
	if err != nil {
		return ScanPlan{}, fmt.Errorf("prepare SCIP indexes: %w", err)
	}
	state.traceInputs, state.traceDigest, err = runtime.PrepareTraceInputs(ctx, opts.TracePaths)
	if err != nil {
		return ScanPlan{}, fmt.Errorf("validate runtime trace inputs: %w", err)
	}
	for _, input := range state.traceInputs {
		if _, err := runtime.LoadTrace(ctx, input); err != nil {
			return ScanPlan{}, fmt.Errorf("validate runtime trace: %w", err)
		}
	}
	if opts.HistoryPath != "" {
		state.historyInput, err = history.PrepareInput(ctx, opts.HistoryPath)
		if err != nil {
			return ScanPlan{}, fmt.Errorf("prepare history input: %w", err)
		}
		if _, err := history.ReadCompiledFile(ctx, state.historyInput); err != nil {
			return ScanPlan{}, fmt.Errorf("validate history input: %w", err)
		}
	}
	if _, err := state.runInventory(ctx); err != nil {
		return ScanPlan{}, fmt.Errorf("plan inventory: %w", err)
	}
	if _, err := state.runNormalize(ctx); err != nil {
		return ScanPlan{}, fmt.Errorf("plan normalization: %w", err)
	}

	plan := ScanPlan{Root: root}
	if opts.Cache != nil {
		plan.CacheRoot = opts.Cache.Root()
	}
	previousByStage := map[string]bool{}
	if opts.Cache != nil {
		report, err := opts.Cache.Inspect(ctx, false)
		if err != nil {
			return ScanPlan{}, fmt.Errorf("inspect stage cache for plan: %w", err)
		}
		for _, entry := range report.Entries {
			previousByStage[entry.StageID] = true
		}
	}
	for _, stage := range state.stages() {
		stagePlan := StagePlan{
			ID: stage.ID, Version: stage.Version,
			Dependencies: append([]string(nil), stage.Dependencies...),
			Enabled:      stageEnabled(stage.ID, opts),
			Cacheable:    !stage.DisableCache,
			Resources:    stage.Resources,
			Disposition:  "execute",
			Reason:       "stage always executes to preserve current source truth",
		}
		if !stagePlan.Enabled {
			stagePlan.Disposition = "disabled"
			stagePlan.Reason = "disabled by effective scan configuration"
			plan.Summary.Disabled++
		} else if stage.DisableCache {
			plan.Summary.Execute++
		} else if opts.Cache == nil {
			stagePlan.Reason = "stage cache is disabled"
			plan.Summary.Execute++
		} else {
			inputs := scheduler.Inputs{Results: map[string]scheduler.Result{}}
			inputDigests := append([]string(nil), stage.InputDigests...)
			if stage.DynamicInputDigests != nil {
				dynamic, err := stage.DynamicInputDigests(ctx, inputs)
				if err != nil {
					return ScanPlan{}, fmt.Errorf("plan stage %s inputs: %w", stage.ID, err)
				}
				inputDigests = append(inputDigests, dynamic...)
			}
			key, err := scheduler.CacheKey(
				stage.ID,
				stage.Version,
				inputDigests,
				stage.Configuration,
			)
			if err != nil {
				return ScanPlan{}, fmt.Errorf("plan stage %s cache key: %w", stage.ID, err)
			}
			stagePlan.CacheKey = key
			hit, issue, err := opts.Cache.probe(ctx, key, stage.ID)
			if err != nil {
				return ScanPlan{}, fmt.Errorf("probe stage %s cache: %w", stage.ID, err)
			}
			if hit {
				stagePlan.Disposition = "cache-hit"
				stagePlan.Reason = "verified payload matches stage, inputs, and configuration"
				plan.Summary.CacheHit++
			} else {
				stagePlan.Disposition = "execute"
				switch {
				case issue != "":
					stagePlan.Reason = "cached payload rejected: " + issue
				case previousByStage[stage.ID]:
					stagePlan.Reason = "stage inputs or relevant configuration changed"
				default:
					stagePlan.Reason = "no prior cache entry for this stage"
				}
				plan.Summary.Execute++
			}
		}
		plan.Stages = append(plan.Stages, stagePlan)
	}
	plan.Summary.Stages = len(plan.Stages)
	plan.EvidenceOpportunities = evidenceOpportunities(state)
	return plan, nil
}

func evidenceOpportunities(state *stagedScanState) []EvidenceOpportunity {
	result := []EvidenceOpportunity{
		{
			Kind: "compiler_semantics", Authority: "compiler_or_structured_assertion", Status: "not_supplied",
			Detail:  "No SCIP index is admitted; syntax and deterministic framework evidence remain available.",
			Command: []string{"rkc", "scip", "languages"}, RequiresAuthorization: false,
		},
		{
			Kind: "runtime_capture_assertion", Authority: "operator_assertion", Status: "not_supplied",
			Detail:                "No runtime capture assertion is admitted. Current capture can add bounded assertions, not producer-authenticated observations.",
			Command:               []string{"rkc", "trace", "capture", "--dir", ".", "--out", ".rkc-trace.json", "--", "go", "test", "./..."},
			RequiresAuthorization: true,
		},
		{
			Kind: "semantic_history", Authority: "version_control", Status: "not_supplied",
			Detail:                "No compiled semantic history is admitted; current-tree facts remain usable without lifecycle claims.",
			Command:               []string{"rkc", "history", "build", "--dir", ".", "--out", ".rkc-history.json"},
			RequiresAuthorization: true,
		},
	}
	if len(state.scipInputs) > 0 {
		result[0].AdmittedInputs = len(state.scipInputs)
		authenticated := 0
		for _, input := range state.scipInputs {
			if input.CompilerAuthenticated() {
				authenticated++
			}
		}
		switch authenticated {
		case len(state.scipInputs):
			result[0].Authority = "compiler"
			result[0].Status = "admitted_compiler_authenticated"
			result[0].Detail = fmt.Sprintf("All %d validated SCIP inputs retain current-process compiler authentication.", authenticated)
		case 0:
			result[0].Authority = "structured_index_assertion"
			result[0].Status = "admitted_assertion"
			result[0].Detail = fmt.Sprintf("All %d validated SCIP inputs are producer-unverified structured assertions; facts cannot be labelled compiler-resolved.", len(state.scipInputs))
		default:
			result[0].Authority = "mixed"
			result[0].Status = "admitted_mixed_authority"
			result[0].Detail = fmt.Sprintf("%d of %d validated SCIP inputs retain current-process compiler authentication; the remainder are producer-unverified structured assertions.", authenticated, len(state.scipInputs))
		}
		result[0].Command = nil
		result[0].RequiresAuthorization = false
	}
	if len(state.traceInputs) > 0 {
		result[1].Status = "admitted_assertion"
		result[1].AdmittedInputs = len(state.traceInputs)
		result[1].Detail = "Digest-bound, source-affine runtime assertions are validated and included; no current capture authenticates its command-output producer."
		result[1].Command = nil
		result[1].RequiresAuthorization = false
	}
	if state.historyInput.Path != "" {
		result[2].Status = "admitted"
		result[2].AdmittedInputs = 1
		result[2].Detail = "Digest-bound semantic history is validated and included in the planned evidence boundary."
		result[2].Command = nil
		result[2].RequiresAuthorization = false
	}
	return result
}

func stageEnabled(stageID string, opts Options) bool {
	switch stageID {
	case "python-syntax":
		return !opts.DisablePlugins && !opts.DisablePythonAST
	case "go-syntax":
		return !opts.DisablePlugins && !opts.DisableGoAST
	case "typescript-syntax":
		return !opts.DisablePlugins && !opts.DisableTypeScript
	case "scip-semantic":
		return !opts.DisablePlugins && !opts.DisableSCIP && len(opts.SCIPIndexes) > 0
	case "markdown":
		return !opts.DisableFrameworks && !opts.DisableMarkdown
	case "openapi":
		return !opts.DisableFrameworks && !opts.DisableOpenAPI
	case "json-schema":
		return !opts.DisableFrameworks && !opts.DisableJSONSchema
	case "manifests":
		return !opts.DisableFrameworks && !opts.DisableManifests
	case "env-keys":
		return !opts.DisableFrameworks && !opts.DisableEnvKeys
	case "secret-scan":
		return !opts.DisableSecretScan
	case "config-env":
		return !opts.DisableFrameworks
	case "trace-import":
		return len(opts.TracePaths) > 0
	case "history-import":
		return opts.HistoryPath != ""
	default:
		return true
	}
}
