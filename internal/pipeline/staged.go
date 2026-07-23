package pipeline

import (
	"context"
	"errors"
	"fmt"
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
	"secret-scan",
	"typescript-syntax",
}

var fragmentMergeOrder = []string{
	"python-syntax",
	"go-syntax",
	"typescript-syntax",
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
}

// Scan executes the active compiler as an explicit deterministic DAG. Stage
// outputs are intentionally digest-addressed even before persistent payload
// caching is enabled, so cache integration cannot later change stage identity.
func Scan(ctx context.Context, opts Options) (rkcmodel.Bundle, rkcmodel.Coverage, error) {
	if ctx == nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, errors.New("pipeline scan context is required")
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

	state := &stagedScanState{
		opts:           opts,
		root:           root,
		artifactByPath: map[string]string{},
		fragments:      map[string]rkcmodel.Fragment{},
		parsed:         map[string]struct{}{},
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
		Workers: workers,
		Budget:  budget,
		Cache:   cache,
		OnEvent: opts.OnStageEvent,
	})
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("execute scan DAG: %w", err)
	}
	if len(report.Results) != 15 {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf(
			"execute scan DAG: completed %d stages, want 15", len(report.Results),
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
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableEnvKeys,
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
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableManifests,
		}, nil, state.runManifests),
		state.analysisStage("markdown", []string{"normalize"}, map[string]any{
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableMarkdown,
		}, nil, state.runMarkdown),
		state.analysisStage("openapi", []string{"normalize"}, map[string]any{
			"enabled": !state.opts.DisableFrameworks && !state.opts.DisableOpenAPI,
		}, func(file pluginapi.FileRef) bool {
			return file.Language == "json"
		}, state.runOpenAPI),
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
		state.stage("validate", []string{"resolve"}, map[string]any{
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
	case "go-syntax", "typescript-syntax":
		return scheduler.ResourceRequest{
			MemoryMiB: 512, CPU: 1, OpenFiles: 128, IOClass: "normal",
		}
	case "merge", "resolve", "validate":
		return scheduler.ResourceRequest{
			MemoryMiB: 512, CPU: 1, OpenFiles: 32, IOClass: "normal",
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
	inv, err := inventory.Scan(inventory.Options{
		Root: state.root, MaxFileBytes: state.opts.MaxFileBytes, MaxTextBytes: state.opts.MaxTextBytes,
		MaxRepositoryBytes: state.opts.MaxRepositoryBytes, MaxFiles: state.opts.MaxFiles,
		Excludes: state.opts.Excludes,
	})
	if err != nil {
		return scheduler.Result{}, err
	}
	state.inventory = inv
	gitInfo := inspectGit(ctx, state.root)
	rootName := filepath.Base(state.root)
	repositoryIdentity := firstNonEmpty(state.opts.SourceReference, gitInfo.Origin, rootName)
	repositoryID := rkcmodel.StableID("repository", repositoryIdentity)
	snapshotID := rkcmodel.StableID(
		"snapshot", repositoryIdentity, gitInfo.Commit, inv.Digest,
		state.opts.ConfigDigest, state.opts.PolicyDigest, state.opts.PluginLockDigest,
		state.opts.ToolchainDigest, rkcmodel.SchemaVersion,
	)
	if gitInfo.Dirty {
		gitInfo.WorkingTreeDigest = inv.Digest
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
		},
		Metadata: map[string]string{"source_reference": state.opts.SourceReference},
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
			ID: file.ArtifactID, Path: file.Path, Language: file.Language, SHA256: file.SHA256,
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
	files := filterFiles(state.files, func(file pluginapi.FileRef) bool { return file.Language == "json" })
	fragment, err := openapi.Extract(openapi.Options{Root: state.root, Files: files})
	return state.handleFragmentResult("openapi", files, fragment, err, "RKC-API-2001", openapi.PluginID, false)
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

func (state *stagedScanState) runMerge(context.Context) (scheduler.Result, error) {
	if err := reverifyInventoriedSources(state.root, state.files, state.sourceIdentities); err != nil {
		return scheduler.Result{}, err
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

func (state *stagedScanState) runValidate(context.Context) (scheduler.Result, error) {
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
			"canonical bundle validation failed with %d error diagnostic(s)", errorCount,
		)
	}
	return state.bundleResult("validate"), nil
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
