package pipeline

import (
	"context"
	"errors"
	"fmt"
	"github.com/neuroforge-io/RKC/internal/configenv"
	"github.com/neuroforge-io/RKC/internal/flow"
	"github.com/neuroforge-io/RKC/internal/history"
	"github.com/neuroforge-io/RKC/internal/runtime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/framework/envkeys"
	"github.com/neuroforge-io/RKC/internal/framework/jsonschema"
	"github.com/neuroforge-io/RKC/internal/framework/manifest"
	"github.com/neuroforge-io/RKC/internal/framework/openapi"
	"github.com/neuroforge-io/RKC/internal/framework/secretpack"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/lang/goast"
	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
	"github.com/neuroforge-io/RKC/internal/lang/tssyntax"
	"github.com/neuroforge-io/RKC/internal/plugin"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const pipelineStageVersion = "1.0.0"

var analysisStageIDs = []string{
	"env-keys",
	"go-syntax",
	"json-schema",
	"manifests",
	"markdown",
	"openapi",
	"python-syntax",
	"scip-semantic",
	"secret-scan",
	"typescript-syntax",
}

var fragmentMergeOrder = []string{
	"python-syntax",
	"go-syntax",
	"typescript-syntax",
	"scip-semantic",
	"markdown",
	"openapi",
	"json-schema",
	"manifests",
	"env-keys",
	"secret-scan",
}

type stagedScanState struct {
	mu sync.Mutex

	opts Options
	root string

	inventory        inventory.Result
	bundle           rkcmodel.Bundle
	coverage         rkcmodel.Coverage
	files            []pluginapi.FileRef
	artifactByPath   map[string]string
	fragments        map[string]rkcmodel.Fragment
	parsed           map[string]struct{}
	secretLiterals   []string
	sourceIdentities map[string]sourceFileIdentity
	scipInputs       []scipindex.Input
	scipDigest       string
	traceInputs      []runtime.TraceInput
	traceDigest      string
	historyInput     history.Input
}

// Scan executes the active compiler as an explicit deterministic DAG. Stage
// outputs are intentionally digest-addressed even before persistent payload
// caching is enabled, so cache integration cannot later change stage identity.
func Scan(ctx context.Context, opts Options) (rkcmodel.Bundle, rkcmodel.Coverage, error) {
	if ctx == nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, errors.New("pipeline scan context is required")
	}
	if err := validateArchiveOptions(&opts); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, err
	}
	if (opts.RunID == "") != (opts.Journal == nil) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{},
			errors.New("pipeline run ID and journal must be supplied together")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("root is not a directory: %s", root)
	}
	opts.Excludes = effectivePipelineExcludes(opts.Excludes)
	canonicalOrigin, err := publicSuppliedOrigin(opts.Origin)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, err
	}
	opts.Origin = canonicalOrigin

	state := &stagedScanState{
		opts:           opts,
		root:           root,
		artifactByPath: map[string]string{},
		fragments:      map[string]rkcmodel.Fragment{},
		parsed:         map[string]struct{}{},
	}
	state.scipInputs, state.scipDigest, err = scipindex.PrepareInputs(ctx, enabledSCIPIndexes(opts))
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("prepare SCIP indexes: %w", err)
	}
	state.traceInputs, state.traceDigest, err = runtime.PrepareTraceInputs(ctx, opts.TracePaths)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("prepare runtime traces: %w", err)
	}
	if opts.HistoryPath != "" {
		state.historyInput, err = history.PrepareInput(ctx, opts.HistoryPath)
		if err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("prepare history input: %w", err)
		}
	}
	var cache scheduler.Cache
	if opts.Cache != nil {
		cache = opts.Cache
	}
	workers := opts.StageWorkers
	if workers <= 0 {
		workers = 4
	}
	budget := opts.ResourceBudget
	if budget == (scheduler.ResourceBudget{}) {
		budget = scheduler.ResourceBudget{
			MemoryMiB: 2048,
			CPU:       workers,
			Processes: 8,
			OpenFiles: 512,
		}
	}
	report, err := scheduler.Execute(ctx, state.stages(), scheduler.Options{
		Workers:                workers,
		Budget:                 budget,
		Cache:                  cache,
		RunID:                  opts.RunID,
		Journal:                opts.Journal,
		DeferJournalCompletion: opts.DeferJournalCompletion,
		OnEvent:                opts.OnStageEvent,
	})
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("execute scan DAG: %w", err)
	}
	if len(report.Results) != 20 {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf(
			"execute scan DAG: completed %d stages, want 20", len(report.Results),
		)
	}
	if state.bundle.Snapshot.ID == "" || state.coverage.SnapshotID != state.bundle.Snapshot.ID {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, errors.New("execute scan DAG: final coverage is not bound to the compiled snapshot")
	}
	return state.bundle, state.coverage, nil
}

func (state *stagedScanState) stages() []scheduler.Stage {
	inventoryConfig := map[string]any{
		"max_file_bytes":       state.opts.MaxFileBytes,
		"max_text_bytes":       state.opts.MaxTextBytes,
		"max_repository_bytes": state.opts.MaxRepositoryBytes,
		"max_files":            state.opts.MaxFiles,
		"excludes":             uniqueSorted(state.opts.Excludes),
		"config_digest":        state.opts.ConfigDigest,
		"policy_digest":        state.opts.PolicyDigest,
	}
	stages := []scheduler.Stage{
		state.stage("inventory", nil, inventoryConfig, state.runInventory),
		state.stage("normalize", []string{"inventory"}, map[string]any{
			"redact_secrets": !state.opts.DisableSecretScan,
		}, state.runNormalize),
		state.analysisStage("env-keys", []string{"normalize"}, map[string]any{
			"enabled":        !state.opts.DisableFrameworks && !state.opts.DisableEnvKeys,
			"plugin_id":      envkeys.PluginID,
			"plugin_version": envkeys.PluginVersion,
		}, func(file pluginapi.FileRef) bool {
			return envkeys.IsCandidate(file.Path)
		}, state.runEnvKeys),
		state.analysisStage("go-syntax", []string{"normalize"}, map[string]any{
			"enabled":            !state.opts.DisablePlugins && !state.opts.DisableGoAST,
			"tool":               state.opts.ToolVersion,
			"toolchain_digest":   state.opts.ToolchainDigest,
			"plugin_lock_digest": state.opts.PluginLockDigest,
		}, isGoCacheInput, state.runGoSyntax),
		state.analysisStage("json-schema", []string{"normalize"}, map[string]any{
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableJSONSchema,
		}, func(file pluginapi.FileRef) bool {
			return file.Language == "json"
		}, state.runJSONSchema),
		state.analysisStage("manifests", []string{"normalize"}, map[string]any{
			"enabled":        !state.opts.DisableFrameworks && !state.opts.DisableManifests,
			"plugin_id":      manifest.PluginID,
			"plugin_version": manifest.PluginVersion,
		}, nil, state.runManifests),
		state.analysisStage("markdown", []string{"normalize"}, map[string]any{
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableMarkdown,
		}, nil, state.runMarkdown),
		state.analysisStage("openapi", []string{"normalize"}, map[string]any{
			"enabled":        !state.opts.DisableFrameworks && !state.opts.DisableOpenAPI,
			"plugin_id":      openapi.PluginID,
			"plugin_version": openapi.PluginVersion,
		}, isOpenAPICacheInput, state.runOpenAPI),
		state.analysisStage("python-syntax", []string{"normalize"}, map[string]any{
			"enabled":              !state.opts.DisablePlugins && !state.opts.DisablePythonAST,
			"plugin_sha256":        state.opts.PythonPluginSHA256,
			"plugin_lock_digest":   state.opts.PluginLockDigest,
			"toolchain_digest":     state.opts.ToolchainDigest,
			"timeout_nanoseconds":  state.opts.PluginTimeout.Nanoseconds(),
			"maximum_output_bytes": state.opts.PluginMaxOutput,
			"maximum_stderr_bytes": state.opts.PluginMaxStderr,
			"memory_mib":           state.opts.PluginMemoryMiB,
			"swap_mib":             state.opts.PluginSwapMiB,
			"processes":            state.opts.PluginProcessLimit,
			"sandbox_required":     state.opts.PluginSandboxRequired,
			"deny_network":         state.opts.PluginDenyNetwork,
			"deny_process_spawn":   state.opts.PluginDenyProcessSpawn,
		}, func(file pluginapi.FileRef) bool {
			return file.Language == "python"
		}, state.runPythonSyntax),
		state.analysisStage("scip-semantic", []string{"normalize"}, map[string]any{
			"enabled":      !state.opts.DisablePlugins && !state.opts.DisableSCIP && len(state.scipInputs) > 0,
			"input_digest": state.scipDigest,
			"input_count":  len(state.scipInputs),
			"plugin_id":    scipindex.PluginID,
			"version":      scipindex.PluginVersion,
		}, nil, state.runSCIPSemantic),
		state.analysisStage("secret-scan", []string{"normalize"}, map[string]any{
			"enabled":       !state.opts.DisableSecretScan,
			"policy_digest": state.opts.PolicyDigest,
		}, nil, state.runSecretScan),
		state.analysisStage("typescript-syntax", []string{"normalize"}, map[string]any{
			"enabled":            !state.opts.DisablePlugins && !state.opts.DisableTypeScript,
			"tool":               state.opts.ToolVersion,
			"toolchain_digest":   state.opts.ToolchainDigest,
			"plugin_lock_digest": state.opts.PluginLockDigest,
		}, isTypeScriptCacheInput, state.runTypeScriptSyntax),
	}
	stages = append(stages,
		state.stage("merge", append([]string(nil), analysisStageIDs...), nil, state.runMerge),
		state.stage("resolve", []string{"merge"}, nil, state.runResolve),
		state.stage("value-flow", []string{"resolve"}, map[string]any{
			"enabled":        true,
			"plugin_id":      flow.PluginID,
			"plugin_version": flow.PluginVersion,
		}, state.runValueFlow),
		state.postMergeAnalysisStage("config-env", []string{"value-flow"}, map[string]any{
			"enabled":        !state.opts.DisableFrameworks,
			"plugin_id":      configenv.PluginID,
			"plugin_version": configenv.PluginVersion,
		}, nil, state.runConfigEnv),
		state.stage("trace-import", []string{"config-env"}, map[string]any{
			"enabled":      len(state.traceInputs) > 0,
			"input_digest": state.traceDigest,
			"input_count":  len(state.traceInputs),
			"plugin_id":    runtime.PluginID,
			"version":      runtime.PluginVersion,
		}, state.runTraceImport),
		state.stage("history-import", []string{"trace-import"}, map[string]any{
			"enabled":      state.historyInput.Path != "",
			"input_digest": state.historyInput.SHA256,
			"plugin_id":    history.PluginID,
			"version":      history.PluginVersion,
		}, state.runHistoryImport),
		state.stage("validate", []string{"history-import"}, map[string]any{
			"schema_version":    rkcmodel.SchemaVersion,
			"strict_vocabulary": true,
			"require_evidence":  true,
		}, state.runValidate),
		state.stage("coverage", []string{"validate"}, nil, state.runCoverage),
	)
	for index := range stages {
		stages[index].Resources = state.stageResources(stages[index].ID)
	}
	return stages
}

func (state *stagedScanState) stageResources(stageID string) scheduler.ResourceRequest {
	if !stageEnabled(stageID, state.opts) {
		return scheduler.ResourceRequest{
			MemoryMiB: 16, CPU: 1, OpenFiles: 4, IOClass: "normal",
		}
	}
	switch stageID {
	case "inventory":
		return scheduler.ResourceRequest{
			MemoryMiB: 256, CPU: 1, OpenFiles: 128, IOClass: "bulk",
		}
	case "normalize", "secret-scan":
		return scheduler.ResourceRequest{
			MemoryMiB: 256, CPU: 1, OpenFiles: 64, IOClass: "bulk",
		}
	case "python-syntax":
		memory := state.opts.PluginMemoryMiB + 128
		if memory < 256 {
			memory = 256
		}
		processes := state.opts.PluginProcessLimit + 1
		if processes < 2 {
			processes = 2
		}
		return scheduler.ResourceRequest{
			MemoryMiB: memory, CPU: 1, Processes: processes,
			OpenFiles: 64, IOClass: "normal",
		}
	case "go-syntax", "typescript-syntax", "scip-semantic":
		return scheduler.ResourceRequest{
			MemoryMiB: 512, CPU: 1, OpenFiles: 128, IOClass: "normal",
		}
	case "merge", "resolve", "validate":
		return scheduler.ResourceRequest{
			MemoryMiB: 512, CPU: 1, OpenFiles: 32, IOClass: "normal",
		}
	case "value-flow":
		// The flow pass retains the resolved canonical bundle while building a
		// separately bounded fact/evidence fragment. Its 256 MiB fragment byte
		// ceiling is therefore not the complete working-set declaration. A
		// 512 MiB admission preserves the supported low-memory scan profile.
		return scheduler.ResourceRequest{
			MemoryMiB: 512, CPU: 1, Processes: 1, OpenFiles: 128, IOClass: "normal",
		}
	case "coverage":
		return scheduler.ResourceRequest{
			MemoryMiB: 128, CPU: 1, OpenFiles: 8, IOClass: "latency",
		}
	default:
		return scheduler.ResourceRequest{
			MemoryMiB: 128, CPU: 1, OpenFiles: 32, IOClass: "normal",
		}
	}
}

func (state *stagedScanState) stage(
	id string,
	dependencies []string,
	configuration any,
	run func(context.Context) (scheduler.Result, error),
) scheduler.Stage {
	return scheduler.Stage{
		ID:            id,
		Version:       pipelineStageVersion,
		Dependencies:  dependencies,
		Configuration: configuration,
		DisableCache:  true,
		Run: func(ctx context.Context, _ scheduler.Inputs) (scheduler.Result, error) {
			if err := ctx.Err(); err != nil {
				return scheduler.Result{}, err
			}
			return run(ctx)
		},
	}
}

func (state *stagedScanState) analysisStage(
	id string,
	dependencies []string,
	configuration any,
	cacheInput func(pluginapi.FileRef) bool,
	run func(context.Context) (scheduler.Result, error),
) scheduler.Stage {
	stage := state.stage(id, dependencies, map[string]any{
		"schema_version": rkcmodel.SchemaVersion,
		"settings":       configuration,
	}, run)
	stage.DisableCache = false
	stage.IgnoreDependencyDigests = true
	stage.DynamicInputDigests = func(
		ctx context.Context,
		_ scheduler.Inputs,
	) ([]string, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files := state.files
		if cacheInput != nil {
			files = filterFiles(files, cacheInput)
		}
		return []string{rkcmodel.DigestJSON(fileDigestInputs(files))}, nil
	}
	stage.Restore = func(
		ctx context.Context,
		_ scheduler.Inputs,
		result scheduler.Result,
	) error {
		return state.restoreFragment(ctx, id, result)
	}
	return stage
}

// postMergeAnalysisStage caches a fragment produced after the main merge and
// applies restored payloads immediately. A normal analysis stage only records
// its fragment for runMerge; using that contract after runMerge would silently
// omit cache hits and fresh results from the canonical bundle.
func (state *stagedScanState) postMergeAnalysisStage(
	id string,
	dependencies []string,
	configuration any,
	cacheInput func(pluginapi.FileRef) bool,
	run func(context.Context) (scheduler.Result, error),
) scheduler.Stage {
	stage := state.analysisStage(id, dependencies, configuration, cacheInput, run)
	restore := stage.Restore
	stage.Restore = func(ctx context.Context, inputs scheduler.Inputs, result scheduler.Result) error {
		if err := restore(ctx, inputs, result); err != nil {
			return err
		}
		return state.mergeRecordedFragment(id)
	}
	return stage
}

func (state *stagedScanState) mergeRecordedFragment(stageID string) error {
	state.mu.Lock()
	fragment, ok := state.fragments[stageID]
	state.mu.Unlock()
	if !ok {
		return fmt.Errorf("post-merge stage %s produced no fragment", stageID)
	}
	mergeFragment(&state.bundle, fragment)
	dedupeBundle(&state.bundle)
	return nil
}

func isGoCacheInput(file pluginapi.FileRef) bool {
	path := filepath.ToSlash(file.Path)
	base := filepath.Base(path)
	return file.Language == "go" || base == "go.mod" || base == "go.sum" ||
		base == "go.work" || base == "go.work.sum"
}

func isTypeScriptCacheInput(file pluginapi.FileRef) bool {
	path := filepath.ToSlash(file.Path)
	base := filepath.Base(path)
	return file.Language == "typescript" || file.Language == "javascript" ||
		base == "package.json" || base == "package-lock.json" ||
		base == "pnpm-lock.yaml" || base == "yarn.lock" ||
		strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json") ||
		strings.HasPrefix(base, "jsconfig") && strings.HasSuffix(base, ".json")
}

func (state *stagedScanState) runInventory(ctx context.Context) (scheduler.Result, error) {
	inv, err := inventory.ScanContext(ctx, inventory.Options{
		Root: state.root, MaxFileBytes: state.opts.MaxFileBytes, MaxTextBytes: state.opts.MaxTextBytes,
		MaxRepositoryBytes: state.opts.MaxRepositoryBytes, MaxFiles: state.opts.MaxFiles,
		Excludes: state.opts.Excludes,
	})
	if err != nil {
		return scheduler.Result{}, err
	}
	state.inventory = inv
	gitInfo, err := inspectGitForScan(ctx, state.root, state.opts.SkipGitInspection)
	if err != nil {
		return scheduler.Result{}, err
	}
	origin, err := reconcileRepositoryOrigin(state.opts.Origin, gitInfo.Origin)
	if err != nil {
		return scheduler.Result{}, err
	}
	gitInfo.Origin = origin
	rootName := filepath.Base(state.root)
	repositoryIdentity := firstNonEmpty(origin, rootName)
	repositoryID := rkcmodel.StableID("repository", repositoryIdentity)
	snapshotID := stableSnapshotID(
		repositoryIdentity,
		gitInfo.Commit,
		inv.Digest,
		state.scipDigest,
		state.traceDigest,
		state.historyInput.SHA256,
		state.opts,
	)
	if gitInfo.Dirty {
		gitInfo.WorkingTreeDigest = inv.Digest
	}
	metadata := map[string]string{"scip_input_digest": state.scipDigest}
	addArchiveMetadata(metadata, state.opts.ArchiveProvenance)
	if state.traceDigest != "" {
		metadata["trace_input_digest"] = state.traceDigest
	}
	if state.historyInput.SHA256 != "" {
		metadata["history_input_digest"] = state.historyInput.SHA256
	}
	if origin != "" {
		metadata["source_reference"] = origin
	}
	state.bundle = rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{
		SchemaVersion:    rkcmodel.SchemaVersion,
		ID:               snapshotID,
		RepositoryID:     repositoryID,
		CreatedAt:        time.Now().UTC(),
		Status:           "committed",
		RootName:         rootName,
		RootPath:         state.root,
		ContentDigest:    inv.Digest,
		ConfigDigest:     state.opts.ConfigDigest,
		PolicyDigest:     state.opts.PolicyDigest,
		PluginLockDigest: state.opts.PluginLockDigest,
		ToolchainDigest:  state.opts.ToolchainDigest,
		Git:              gitInfo,
		Tool: rkcmodel.ToolInfo{
			Name: "rkc", Version: firstNonEmpty(state.opts.ToolVersion, "development"),
		},
		Policy: map[string]any{
			"max_file_bytes":       state.opts.MaxFileBytes,
			"max_text_bytes":       state.opts.MaxTextBytes,
			"max_repository_bytes": state.opts.MaxRepositoryBytes,
			"max_files":            state.opts.MaxFiles,
			"excludes":             uniqueSorted(state.opts.Excludes),
			"plugins":              !state.opts.DisablePlugins,
			"frameworks":           !state.opts.DisableFrameworks,
			"secret_scan":          !state.opts.DisableSecretScan,
			"scip_semantic":        len(state.scipInputs) > 0,
		},
		Metadata: metadata,
	}, Artifacts: inv.Artifacts, Diagnostics: inv.Diagnostics}
	state.bundle.Nodes = append(state.bundle.Nodes, rkcmodel.Node{
		ID: repositoryID, LogicalID: repositoryID, Kind: "repository",
		Name: rootName, QualifiedName: repositoryIdentity, Visibility: "repository",
		Attributes: map[string]any{
			"snapshot_id": snapshotID, "git_commit": gitInfo.Commit, "git_origin": gitInfo.Origin,
		},
	})
	for _, artifact := range state.bundle.Artifacts {
		state.bundle.Nodes = append(state.bundle.Nodes, artifactNode(artifact))
		state.bundle.Edges = append(state.bundle.Edges, rkcmodel.Edge{
			ID:   rkcmodel.StableID("edge", "contains", repositoryID, artifact.ID),
			Kind: "contains", From: repositoryID, To: artifact.ID,
			Resolution: rkcmodel.ResolutionDeclared, Confidence: 1, Producer: "rkc.inventory",
		})
		state.artifactByPath[artifact.Path] = artifact.ID
		if artifact.Text && artifact.Status == "text" {
			state.files = append(state.files, pluginapi.FileRef{
				ArtifactID: artifact.ID, Path: artifact.Path, Language: artifact.Language,
				MediaType: artifact.MediaType, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes,
				Materialized: filepath.Join(state.root, filepath.FromSlash(artifact.Path)),
			})
		}
	}
	sort.Slice(state.files, func(i, j int) bool { return state.files[i].Path < state.files[j].Path })
	return scheduler.Result{
		ObjectDigest: inv.Digest,
		Metadata: map[string]any{
			"artifacts": len(inv.Artifacts), "diagnostics": len(inv.Diagnostics),
		},
	}, nil
}

func (state *stagedScanState) runNormalize(context.Context) (scheduler.Result, error) {
	values, identities, err := collectSensitiveLiteralsAndIdentity(state.root, state.files)
	if err != nil {
		return scheduler.Result{}, err
	}
	state.secretLiterals = values
	state.sourceIdentities = identities
	return state.valueResult(map[string]any{
		"inventory_digest": state.inventory.Digest,
		"files":            fileDigestInputs(state.files),
		"secret_literals":  values,
	}), nil
}

func (state *stagedScanState) runPythonSyntax(ctx context.Context) (scheduler.Result, error) {
	if state.opts.DisablePlugins || state.opts.DisablePythonAST {
		return state.disabledResult("python-syntax"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool { return file.Language == "python" })
	if len(files) == 0 {
		return state.recordFragment("python-syntax", rkcmodel.Fragment{}, nil, false)
	}
	legacy := make([]plugin.FileRef, 0, len(files))
	for _, file := range files {
		legacy = append(legacy, plugin.FileRef{
			ID: file.ArtifactID, Path: file.Path, Language: file.Language,
			SHA256: file.SHA256, SizeBytes: file.SizeBytes,
		})
	}
	fragment, runErr := plugin.RunPython(ctx, plugin.Request{
		SchemaVersion: rkcmodel.SchemaVersion,
		SnapshotID:    state.bundle.Snapshot.ID,
		Root:          state.root,
		Files:         legacy,
	}, plugin.PythonOptions{
		Interpreter: state.opts.PythonInterpreter, Script: state.opts.PythonPlugin,
		Timeout: state.opts.PluginTimeout, MaxOutputBytes: state.opts.PluginMaxOutput,
		MaxStderrBytes: state.opts.PluginMaxStderr, MemoryLimitMiB: state.opts.PluginMemoryMiB,
		SwapLimitMiB: state.opts.PluginSwapMiB, ProcessLimit: state.opts.PluginProcessLimit,
		RequireSandbox: state.opts.PluginSandboxRequired, DenyNetwork: state.opts.PluginDenyNetwork,
		DenyProcessSpawn: state.opts.PluginDenyProcessSpawn, Builtin: state.opts.PythonPluginBuiltin,
		ExpectedScriptSHA256: state.opts.PythonPluginSHA256,
	})
	if runErr != nil {
		diagnostic := adapterError("RKC-PY-2001", "rkc.python-ast", runErr)
		if state.opts.FailClosedOnPluginError {
			return scheduler.Result{}, fmt.Errorf("Python adapter failed closed: %w", runErr)
		}
		return state.diagnosticResult("python-syntax", diagnostic), nil
	}
	return state.recordFragment("python-syntax", fragment, files, true)
}

func (state *stagedScanState) runGoSyntax(context.Context) (scheduler.Result, error) {
	if state.opts.DisablePlugins || state.opts.DisableGoAST {
		return state.disabledResult("go-syntax"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool { return file.Language == "go" })
	fragment, err := goast.Extract(goast.Options{
		Root: state.root, SnapshotID: state.bundle.Snapshot.ID, Files: files,
	})
	if err != nil {
		diagnostic := adapterError("RKC-GO-2001", goast.PluginID, err)
		return state.diagnosticResult("go-syntax", diagnostic), nil
	}
	return state.recordFragment("go-syntax", fragment, files, true)
}

func (state *stagedScanState) runTypeScriptSyntax(context.Context) (scheduler.Result, error) {
	if state.opts.DisablePlugins || state.opts.DisableTypeScript {
		return state.disabledResult("typescript-syntax"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool {
		return file.Language == "typescript" || file.Language == "javascript"
	})
	fragment, err := tssyntax.Extract(tssyntax.Options{
		Root: state.root, SnapshotID: state.bundle.Snapshot.ID, Files: files,
	})
	if err != nil {
		diagnostic := adapterError("RKC-TS-2001", tssyntax.PluginID, err)
		return state.diagnosticResult("typescript-syntax", diagnostic), nil
	}
	return state.recordFragment("typescript-syntax", fragment, files, true)
}

func (state *stagedScanState) runSCIPSemantic(ctx context.Context) (scheduler.Result, error) {
	if state.opts.DisablePlugins || state.opts.DisableSCIP || len(state.scipInputs) == 0 {
		return state.disabledResult("scip-semantic"), nil
	}
	fragment, err := scipindex.Extract(ctx, scipindex.Options{
		Root: state.root, Inputs: state.scipInputs,
		Files: state.files, Artifacts: state.bundle.Artifacts,
	})
	if err != nil {
		return scheduler.Result{}, fmt.Errorf("SCIP semantic adapter failed closed: %w", err)
	}
	return state.recordFragment("scip-semantic", fragment, nil, false)
}

func (state *stagedScanState) runMarkdown(context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks || state.opts.DisableMarkdown {
		return state.disabledResult("markdown"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool {
		return file.Language == "markdown" || file.Language == "mdx"
	})
	fragment, err := docparse.Extract(docparse.Options{
		Root: state.root, SnapshotID: state.bundle.Snapshot.ID,
		Files: files, Artifacts: state.artifactByPath,
	})
	return state.handleFragmentResult("markdown", files, fragment, err, "RKC-DOC-2001", docparse.PluginID, true)
}

func (state *stagedScanState) runOpenAPI(context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks || state.opts.DisableOpenAPI {
		return state.disabledResult("openapi"), nil
	}
	files := filterFiles(state.files, isOpenAPICacheInput)
	fragment, err := openapi.Extract(openapi.Options{Root: state.root, Files: files})
	return state.handleFragmentResult("openapi", files, fragment, err, "RKC-API-2001", openapi.PluginID, false)
}

func isOpenAPICacheInput(file pluginapi.FileRef) bool {
	return file.Language == "json" || file.Language == "yaml"
}

func (state *stagedScanState) runJSONSchema(context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks || state.opts.DisableJSONSchema {
		return state.disabledResult("json-schema"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool { return file.Language == "json" })
	fragment, err := jsonschema.Extract(jsonschema.Options{Root: state.root, Files: files})
	return state.handleFragmentResult("json-schema", files, fragment, err, "RKC-SCH-2001", jsonschema.PluginID, false)
}

func (state *stagedScanState) runManifests(context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks || state.opts.DisableManifests {
		return state.disabledResult("manifests"), nil
	}
	fragment, err := manifest.Extract(manifest.Options{Root: state.root, Files: state.files})
	return state.handleFragmentResult("manifests", state.files, fragment, err, "RKC-MAN-2001", manifest.PluginID, false)
}

func (state *stagedScanState) runEnvKeys(context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks || state.opts.DisableEnvKeys {
		return state.disabledResult("env-keys"), nil
	}
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool { return envkeys.IsCandidate(file.Path) })
	fragment, err := envkeys.Extract(envkeys.Options{Root: state.root, Files: files})
	return state.handleFragmentResult("env-keys", files, fragment, err, "RKC-CFG-2001", envkeys.PluginID, true)
}

func (state *stagedScanState) runSecretScan(context.Context) (scheduler.Result, error) {
	if state.opts.DisableSecretScan {
		return state.disabledResult("secret-scan"), nil
	}
	fragment, err := secretpack.Extract(secretpack.Options{Root: state.root, Files: state.files})
	return state.handleFragmentResult("secret-scan", state.files, fragment, err, "RKC-SEC-2001", secretpack.PluginID, false)
}

func (state *stagedScanState) handleFragmentResult(
	stage string,
	files []pluginapi.FileRef,
	fragment rkcmodel.Fragment,
	err error,
	code string,
	pluginID string,
	markSyntax bool,
) (scheduler.Result, error) {
	if err != nil {
		diagnostic := adapterError(code, pluginID, err)
		return state.diagnosticResult(stage, diagnostic), nil
	}
	return state.recordFragment(stage, fragment, files, markSyntax)
}

func (state *stagedScanState) runMerge(ctx context.Context) (scheduler.Result, error) {
	if err := scipindex.VerifyInputs(ctx, state.scipInputs); err != nil {
		return scheduler.Result{}, fmt.Errorf("reverify SCIP indexes: %w", err)
	}
	for _, stageID := range fragmentMergeOrder {
		if fragment, ok := state.fragments[stageID]; ok {
			mergeFragment(&state.bundle, fragment)
		}
	}
	for i := range state.bundle.Artifacts {
		if _, ok := state.parsed[state.bundle.Artifacts[i].ID]; ok &&
			state.bundle.Artifacts[i].Status == "text" {
			state.bundle.Artifacts[i].Status = "syntax_parsed"
		}
	}
	updateArtifactNodes(&state.bundle)
	dedupeBundle(&state.bundle)
	return state.bundleResult("merge"), nil
}

func (state *stagedScanState) runResolve(context.Context) (scheduler.Result, error) {
	resolveHeuristicEdges(&state.bundle)
	dedupeBundle(&state.bundle)
	secrets.SanitizeBundle(&state.bundle, state.secretLiterals)
	return state.bundleResult("resolve"), nil
}

// runTraceImport applies validated runtime traces to the bundle: executed
// spans, observed call edges, runtime evidence, and separate per-test results.
// Aggregate coverage cannot truthfully attribute a call path to one test.
func (state *stagedScanState) runTraceImport(ctx context.Context) (scheduler.Result, error) {
	if len(state.traceInputs) == 0 {
		return state.disabledResult("trace-import"), nil
	}
	traces := make([]runtime.Trace, 0, len(state.traceInputs))
	for _, input := range state.traceInputs {
		trace, err := runtime.LoadTrace(ctx, input)
		if err != nil {
			return scheduler.Result{}, err
		}
		traces = append(traces, trace)
	}
	for _, trace := range traces {
		if _, err := runtime.Import(ctx, &state.bundle, trace); err != nil {
			return scheduler.Result{}, fmt.Errorf("import trace %s: %w", trace.ID, err)
		}
	}
	dedupeBundle(&state.bundle)
	return state.bundleResult("trace-import"), nil
}

// runHistoryImport applies a compiled history to the bundle: symbol lifecycle
// attributes and conservative supersedes edges for rename refactors.
func (state *stagedScanState) runHistoryImport(ctx context.Context) (scheduler.Result, error) {
	if state.historyInput.Path == "" {
		return state.disabledResult("history-import"), nil
	}
	compiled, err := history.ReadCompiledFile(ctx, state.historyInput)
	if err != nil {
		return scheduler.Result{}, err
	}
	if _, err := history.Import(ctx, &state.bundle, compiled); err != nil {
		return scheduler.Result{}, fmt.Errorf("import history %s: %w", state.historyInput.SHA256, err)
	}
	dedupeBundle(&state.bundle)
	return state.bundleResult("history-import"), nil
}

// runConfigEnv compiles build configuration and environment contracts into
// the graph: Go build tags, CI workflows, Terraform declarations, and
// environment-variable declarations.
func (state *stagedScanState) runConfigEnv(ctx context.Context) (scheduler.Result, error) {
	if state.opts.DisableFrameworks {
		return state.disabledResult("config-env"), nil
	}
	fragment, err := configenv.Extract(ctx, configenv.Options{Root: state.root, Files: state.files})
	if err != nil {
		fragment = rkcmodel.Fragment{Diagnostics: []rkcmodel.Diagnostic{
			adapterError("RKC-CFG-3003", configenv.PluginID, err),
		}}
		mergeFragment(&state.bundle, fragment)
		dedupeBundle(&state.bundle)
		result := state.bundleResult("config-env")
		result.DoNotCache = true
		return result, nil
	}
	result, err := state.recordFragment("config-env", fragment, state.files, false)
	if err != nil {
		return scheduler.Result{}, err
	}
	if err := state.mergeRecordedFragment("config-env"); err != nil {
		return scheduler.Result{}, err
	}
	return result, nil
}

// runValueFlow compiles the bounded interprocedural control-flow and
// value-flow graphs over the resolved bundle. It is deterministic and bounded;
// a diagnostic reports truncation instead of unbounded work.
func (state *stagedScanState) runValueFlow(ctx context.Context) (scheduler.Result, error) {
	fragment, stats, err := flow.Analyze(ctx, flow.Options{
		Root: state.root, Files: state.files, Artifacts: state.bundle.Artifacts, Bundle: state.bundle,
	})
	if err != nil {
		return scheduler.Result{}, err
	}
	mergeFragment(&state.bundle, fragment)
	dedupeBundle(&state.bundle)
	_ = stats
	return state.bundleResult("value-flow"), nil
}

func (state *stagedScanState) runValidate(ctx context.Context) (scheduler.Result, error) {
	if err := reverifyInventoriedSources(state.root, state.files, state.sourceIdentities); err != nil {
		return scheduler.Result{}, err
	}
	report := rkcmodel.ValidateBundle(state.bundle, rkcmodel.ValidationOptions{
		StrictVocabulary: true, RequireEvidence: true,
	})
	state.bundle.Diagnostics = append(state.bundle.Diagnostics, report.Diagnostics...)
	dedupeBundle(&state.bundle)
	rkcmodel.SortBundle(&state.bundle)
	if report.HasErrors() {
		errorCount := 0
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Severity == "error" || diagnostic.Severity == "fatal" {
				errorCount++
			}
		}
		return scheduler.Result{}, fmt.Errorf(
			"canonical bundle validation failed with %d error diagnostic(s) (codes: %s)",
			errorCount,
			validationErrorCodeSummary(report.Diagnostics, 8),
		)
	}
	return state.bundleResult("validate"), nil
}

func validationErrorCodeSummary(diagnostics []rkcmodel.Diagnostic, limit int) string {
	counts := map[string]int{}
	total := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity != "error" && diagnostic.Severity != "fatal" {
			continue
		}
		code := strings.TrimSpace(diagnostic.Code)
		if code == "" {
			code = "unknown"
		}
		counts[code]++
		total++
	}
	type codeCount struct {
		code  string
		count int
	}
	ordered := make([]codeCount, 0, len(counts))
	for code, count := range counts {
		ordered = append(ordered, codeCount{code: code, count: count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].code < ordered[j].code
	})
	if limit <= 0 || limit > len(ordered) {
		limit = len(ordered)
	}
	parts := make([]string, 0, limit+1)
	included := 0
	for _, item := range ordered[:limit] {
		parts = append(parts, fmt.Sprintf("%s=%d", item.code, item.count))
		included += item.count
	}
	if included < total {
		parts = append(parts, fmt.Sprintf("other=%d", total-included))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func (state *stagedScanState) runCoverage(context.Context) (scheduler.Result, error) {
	state.coverage = rkcmodel.BuildCoverage(state.bundle)
	return scheduler.Result{
		ObjectDigest: rkcmodel.DigestJSON(state.coverage),
		Metadata: map[string]any{
			"snapshot_id": state.coverage.SnapshotID,
			"artifacts":   state.coverage.ArtifactsInventoried,
			"nodes":       state.coverage.NodesTotal,
			"edges":       state.coverage.EdgesTotal,
		},
	}, nil
}

func (state *stagedScanState) disabledResult(stage string) scheduler.Result {
	result := state.valueResult(map[string]any{"stage": stage, "status": "disabled"})
	result.DoNotCache = true
	return result
}

func (state *stagedScanState) diagnosticResult(
	stage string,
	diagnostic rkcmodel.Diagnostic,
) scheduler.Result {
	state.mu.Lock()
	state.fragments[stage] = rkcmodel.Fragment{
		Diagnostics: []rkcmodel.Diagnostic{diagnostic},
	}
	state.mu.Unlock()
	result := state.valueResult(diagnostic)
	result.DoNotCache = true
	return result
}

func (state *stagedScanState) bundleResult(stage string) scheduler.Result {
	return scheduler.Result{
		ObjectDigest: rkcmodel.CanonicalDigest(state.bundle),
		Metadata: map[string]any{
			"stage": stage, "artifacts": len(state.bundle.Artifacts),
			"nodes": len(state.bundle.Nodes), "edges": len(state.bundle.Edges),
		},
	}
}

func (state *stagedScanState) valueResult(value any) scheduler.Result {
	return scheduler.Result{ObjectDigest: rkcmodel.DigestJSON(value)}
}

func fileDigestInputs(files []pluginapi.FileRef) []map[string]any {
	output := make([]map[string]any, 0, len(files))
	for _, file := range files {
		output = append(output, map[string]any{
			"artifact_id": file.ArtifactID,
			"path":        file.Path,
			"language":    file.Language,
			"media_type":  file.MediaType,
			"sha256":      file.SHA256,
			"size_bytes":  file.SizeBytes,
		})
	}
	return output
}
