package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

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

type ScanPlan struct {
	Root      string      `json:"root"`
	CacheRoot string      `json:"cache_root,omitempty"`
	Stages    []StagePlan `json:"stages"`
	Summary   PlanSummary `json:"summary"`
}

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
	return plan, nil
}

func stageEnabled(stageID string, opts Options) bool {
	switch stageID {
	case "python-syntax":
		return !opts.DisablePlugins && !opts.DisablePythonAST
	case "go-syntax":
		return !opts.DisablePlugins && !opts.DisableGoAST
	case "typescript-syntax":
		return !opts.DisablePlugins && !opts.DisableTypeScript
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
	default:
		return true
	}
}
