package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/neuroforge-io/RKC/internal/acquire"
	"github.com/neuroforge-io/RKC/internal/builtinplugins"
	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/pipeline"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/internal/snapshot"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func runScan(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDirectCommandWithAdmission(ctx, "scan", args, runScanContext)
}

func runScanContext(ctx context.Context, args []string) (resultErr error) {
	if err := scanCancellation(ctx); err != nil {
		return err
	}
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	defaultRunsDirectory, defaultRunsErr := runJournalFlagDefault(args)

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", configPath, "JSON configuration file; omitted uses built-in defaults")
	out := fs.String("out", cfg.Workspace.Output, "generated output directory")
	stateDir := fs.String("state-dir", cfg.Exports.SnapshotStore, "optional immutable snapshot store directory")
	databasePath := fs.String("database", "", "optional durable SQLite store; must remain outside the scanned repository")
	cacheDir := fs.String("cache-dir", defaultStageCacheDirectory(), "verified incremental stage cache directory outside the scanned repository")
	runsDir := fs.String("runs-dir", defaultRunsDirectory, "owner-only scheduler run journal directory outside the scanned repository")
	noCache := fs.Bool("no-cache", !cfg.Analysis.Incremental, "disable stage cache reads and writes for this clean scan")
	stageWorkers := fs.Int("stage-workers", 4, "maximum concurrently admitted scan stages")
	stageMemory := fs.Int64("stage-memory-mib", 2048, "total scheduler memory-admission budget in MiB")
	maxFile := fs.Int64("max-file-bytes", cfg.Inventory.MaxFileBytes, "largest individual regular file hashed or parsed; 0 disables")
	maxText := fs.Int64("max-text-bytes", cfg.Inventory.MaxTextBytes, "largest text file parsed or normalized")
	maxRepository := fs.Int64("max-repository-bytes", cfg.Inventory.MaxRepositoryBytes, "maximum encountered repository bytes; 0 disables")
	maxFiles := fs.Int("max-files", cfg.Inventory.MaxFiles, "maximum encountered paths; 0 disables")
	python := fs.String("python", cfg.Plugins.PythonAST.Interpreter, "Python interpreter for the AST adapter")
	pythonPlugin := fs.String("python-plugin", cfg.Plugins.PythonAST.Script, "Python extractor path or 'builtin'")
	pluginTimeout := fs.Duration("plugin-timeout", cfg.PluginTimeout(), "per-plugin wall-clock timeout")
	pluginOutput := fs.Int64("plugin-output-bytes", cfg.Plugins.MaximumOutputBytes, "maximum plugin stdout bytes")
	noPlugins := fs.Bool("no-plugins", !cfg.Plugins.Enabled, "disable all language adapters; direct scan requires this or --no-python")
	noPython := fs.Bool("no-python", !cfg.Plugins.PythonAST.Enabled, "disable the Python syntax adapter; direct scan requires this or --no-plugins")
	noGo := fs.Bool("no-go", !cfg.Plugins.GoAST.Enabled, "disable the Go syntax adapter")
	noTypeScript := fs.Bool("no-typescript", !cfg.Plugins.TypeScriptSyntax.Enabled, "disable the JavaScript and TypeScript syntax adapter")
	scipIndexes := stringList{}
	fs.Var(&scipIndexes, "scip-index", "SCIP index to import; external files remain producer-unverified; repeatable")
	scipGenerate := stringList{}
	fs.Var(&scipGenerate, "scip-generate", "generate a compiler-grade SCIP index for this language before scanning; repeatable")
	scipTool := fs.String("scip-tool", "", "indexer binary override used by --scip-generate")
	scipLock := fs.String("scip-lock", defaultScipIndexerLockPath(), "operator-owned absolute indexer pin lock used by --scip-generate")
	scipNoPinCheck := fs.Bool("scip-no-pin-check", false, "explicitly allow an unpinned or digest-mismatched SCIP indexer")
	historyPath := fs.String("history", "", "compiled history file to import (lifecycle and supersedes evidence)")
	tracePaths := stringList{}
	fs.Var(&tracePaths, "trace", "runtime trace file to import; repeatable")
	noFrameworks := fs.Bool("no-frameworks", !cfg.Frameworks.Enabled, "disable all deterministic framework and document extractors")
	noMarkdown := fs.Bool("no-markdown", !cfg.Frameworks.Markdown, "disable Markdown document structure extraction")
	noOpenAPI := fs.Bool("no-openapi", !cfg.Frameworks.OpenAPIJSON, "disable OpenAPI JSON/YAML extraction")
	noJSONSchema := fs.Bool("no-json-schema", !cfg.Frameworks.JSONSchema, "disable JSON Schema extraction")
	noManifests := fs.Bool("no-manifests", !cfg.Frameworks.PackageManifests, "disable package and build manifest extraction")
	noEnvKeys := fs.Bool("no-env-keys", !cfg.Frameworks.EnvironmentFiles, "disable environment template key extraction")
	noSecretScan := fs.Bool("no-secret-scan", !cfg.Security.DetectSecrets, "disable deterministic credential-pattern scanning")
	unsafeIncludeSecrets := fs.Bool("unsafe-include-secret-values", !cfg.Security.RedactExports, "write probable secret values into normalized source exports; unsafe and never the default")
	includeSources := fs.Bool("include-sources", cfg.Exports.NormalizedSources, "write normalized Markdown source envelopes")
	noStaticSite := fs.Bool("no-static-site", !cfg.Exports.StaticSite, "omit the generated static browser")
	noJSONLGraph := fs.Bool("no-jsonl-graph", !cfg.Exports.JSONLGraph, "omit per-record JSONL graph exports; bundle.json remains canonical")
	noSearchIndex := fs.Bool("no-search-index", !cfg.Exports.SearchIndex || !cfg.Search.Enabled, "omit the persisted lexical search index")
	noIntegrations := fs.Bool("no-integrations", !cfg.Exports.Integrations, "omit SARIF, GraphML, Mermaid, and CSV integration exports")
	notebookPackBytes := fs.Int("notebook-pack-bytes", cfg.Exports.NotebookPackBytes, "target maximum NotebookLM pack bytes")
	force := fs.Bool("force", false, "replace an existing generated output directory")
	jsonSummary := fs.Bool("json", false, "print machine-readable summary")
	failOnErrors := fs.Bool("fail-on-errors", false, "fail after publishing when error diagnostics exist")
	gitExecutable := fs.String("git", "git", "Git executable used for remote repository acquisition")
	gitRef := fs.String("ref", "", "remote Git branch, tag, or commit to materialize")
	cloneDepth := fs.Int("clone-depth", 1, "remote Git fetch depth; 0 requests full history")
	submodules := fs.Bool("submodules", false, "initialize remote repository submodules")
	gitTimeout := fs.Duration("git-timeout", 10*time.Minute, "remote acquisition timeout")
	acquireTemp := fs.String("acquire-temp", "", "parent directory for temporary remote materializations")
	keepMaterialized := fs.Bool("keep-materialized", false, "retain a remotely acquired working tree after the scan")
	allowFileURL := fs.Bool("allow-file-url", false, "allow file:// Git URLs; intended for controlled local automation")
	excludes := stringList(append([]string(nil), cfg.Inventory.Exclude...))
	fs.Var(&excludes, "exclude", "repository-relative exclusion; repeatable and explicitly inventoried")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configFlag != configPath {
		return errors.New("--config must be supplied only once; its values establish flag defaults")
	}
	if *runsDir == "" && defaultRunsErr != nil {
		return defaultRunsErr
	}
	if *stageWorkers <= 0 || *stageWorkers > 64 {
		return errors.New("--stage-workers must be between 1 and 64")
	}
	if *stageMemory < 128 {
		return errors.New("--stage-memory-mib must be at least 128")
	}
	if *databasePath != "" && *stateDir != "" {
		return errors.New("--database and --state-dir are mutually exclusive")
	}
	source := "."
	if fs.NArg() > 1 {
		return errors.New("scan accepts at most one repository path or Git URL")
	}
	if fs.NArg() == 1 {
		source = fs.Arg(0)
	}
	acquired, err := acquire.Open(ctx, source, acquire.Options{
		GitExecutable: *gitExecutable, Ref: *gitRef, Depth: *cloneDepth, Submodules: *submodules,
		Timeout: *gitTimeout, TemporaryRoot: *acquireTemp, KeepMaterialized: *keepMaterialized,
		AllowFileURLs: *allowFileURL, MaximumLogBytes: 2 * 1024 * 1024,
	})
	if err != nil {
		return err
	}
	defer func() { _ = acquired.Cleanup() }()
	rootAbs := acquired.Root
	outAbs, err := safeoutput.ResolveTarget(*out, rootAbs)
	if err != nil {
		return err
	}
	if len(scipGenerate) > 0 {
		generated, err := generateSCIPIndexes(ctx, scipGenerate, rootAbs, outAbs, *scipTool, *scipLock, *scipNoPinCheck)
		if err != nil {
			return err
		}
		scipIndexes = append(scipIndexes, generated...)
	}
	if rel, err := filepath.Rel(rootAbs, outAbs); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		excludes = append(excludes, filepath.ToSlash(rel))
	}
	if len(scipGenerate) > 0 {
		if rel, err := filepath.Rel(rootAbs, generatedSCIPDirectory(outAbs)); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			excludes = append(excludes, filepath.ToSlash(rel))
		}
	}
	if *stateDir != "" {
		stateAbs, err := safeoutput.ResolveTarget(*stateDir, rootAbs)
		if err != nil {
			return err
		}
		outInsideState, err := pathIsWithin(stateAbs, outAbs)
		if err != nil {
			return fmt.Errorf("compare output and snapshot store: %w", err)
		}
		stateInsideOut, err := pathIsWithin(outAbs, stateAbs)
		if err != nil {
			return fmt.Errorf("compare output and snapshot store: %w", err)
		}
		if outInsideState || stateInsideOut {
			return fmt.Errorf("%w: output and snapshot store must be disjoint directories", safeoutput.ErrUnsafeTarget)
		}
		if rel, err := filepath.Rel(rootAbs, stateAbs); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			excludes = append(excludes, filepath.ToSlash(rel))
		}
		*stateDir = stateAbs
	}
	resolvedDatabase := ""
	if *databasePath != "" {
		resolvedDatabase, err = canonicalSQLitePath(*databasePath)
		if err != nil {
			return err
		}
		insideRepository, err := pathIsWithin(rootAbs, resolvedDatabase)
		if err != nil {
			return fmt.Errorf("compare repository and SQLite database: %w", err)
		}
		insideOutput, err := pathIsWithin(outAbs, resolvedDatabase)
		if err != nil {
			return fmt.Errorf("compare output and SQLite database: %w", err)
		}
		if insideRepository || insideOutput {
			return fmt.Errorf("%w: SQLite database must remain outside the scanned repository and generated output", safeoutput.ErrUnsafeTarget)
		}
	}
	resolvedRuns, err := safeoutput.ResolveTarget(*runsDir, rootAbs)
	if err != nil {
		return fmt.Errorf("resolve run journal directory: %w", err)
	}
	runsInsideRepository, err := pathIsWithin(rootAbs, resolvedRuns)
	if err != nil {
		return fmt.Errorf("compare repository and run journal directory: %w", err)
	}
	runsInsideOutput, err := pathIsWithin(outAbs, resolvedRuns)
	if err != nil {
		return fmt.Errorf("compare output and run journal directory: %w", err)
	}
	outputInsideRuns, err := pathIsWithin(resolvedRuns, outAbs)
	if err != nil {
		return fmt.Errorf("compare run journal directory and output: %w", err)
	}
	if runsInsideRepository || runsInsideOutput || outputInsideRuns {
		return fmt.Errorf(
			"%w: run journals must remain outside the scanned repository and generated output",
			safeoutput.ErrUnsafeTarget,
		)
	}
	if *stateDir != "" {
		runsInsideState, err := pathIsWithin(*stateDir, resolvedRuns)
		if err != nil {
			return fmt.Errorf("compare snapshot store and run journal directory: %w", err)
		}
		stateInsideRuns, err := pathIsWithin(resolvedRuns, *stateDir)
		if err != nil {
			return fmt.Errorf("compare run journal directory and snapshot store: %w", err)
		}
		if runsInsideState || stateInsideRuns {
			return fmt.Errorf(
				"%w: run journal and snapshot-store directories must be disjoint",
				safeoutput.ErrUnsafeTarget,
			)
		}
	}
	if resolvedDatabase != "" {
		databaseInsideRuns, err := pathIsWithin(resolvedRuns, resolvedDatabase)
		if err != nil {
			return fmt.Errorf("compare SQLite database and run journal directory: %w", err)
		}
		if databaseInsideRuns {
			return fmt.Errorf(
				"%w: SQLite database cannot be stored inside the run journal directory",
				safeoutput.ErrUnsafeTarget,
			)
		}
	}
	var stageCache *pipeline.StageCache
	resolvedCache := ""
	if !*noCache {
		cacheTarget := *cacheDir
		resolvedCache, err = safeoutput.ResolveTarget(cacheTarget, rootAbs)
		if err != nil {
			return fmt.Errorf("resolve stage cache: %w", err)
		}
		cacheInsideRepository, err := pathIsWithin(rootAbs, resolvedCache)
		if err != nil {
			return fmt.Errorf("compare repository and stage cache: %w", err)
		}
		if cacheInsideRepository {
			return fmt.Errorf("%w: stage cache must remain outside the scanned repository", safeoutput.ErrUnsafeTarget)
		}
		cacheInsideOutput, err := pathIsWithin(outAbs, resolvedCache)
		if err != nil {
			return fmt.Errorf("compare output and stage cache: %w", err)
		}
		outputInsideCache, err := pathIsWithin(resolvedCache, outAbs)
		if err != nil {
			return fmt.Errorf("compare stage cache and output: %w", err)
		}
		if cacheInsideOutput || outputInsideCache {
			return fmt.Errorf("%w: output and stage cache must be disjoint directories", safeoutput.ErrUnsafeTarget)
		}
		runsInsideCache, err := pathIsWithin(resolvedCache, resolvedRuns)
		if err != nil {
			return fmt.Errorf("compare stage cache and run journal directory: %w", err)
		}
		cacheInsideRuns, err := pathIsWithin(resolvedRuns, resolvedCache)
		if err != nil {
			return fmt.Errorf("compare run journal directory and stage cache: %w", err)
		}
		if runsInsideCache || cacheInsideRuns {
			return fmt.Errorf(
				"%w: stage cache and run journal directories must be disjoint",
				safeoutput.ErrUnsafeTarget,
			)
		}
		if resolvedDatabase != "" {
			databaseInsideCache, err := pathIsWithin(resolvedCache, resolvedDatabase)
			if err != nil {
				return fmt.Errorf("compare SQLite database and stage cache: %w", err)
			}
			if databaseInsideCache {
				return fmt.Errorf("%w: SQLite database cannot be stored inside the stage cache", safeoutput.ErrUnsafeTarget)
			}
		}
		stageCache, err = pipeline.OpenStageCache(resolvedCache)
		if err != nil {
			return err
		}
	}
	pluginPath := strings.TrimSpace(*pythonPlugin)
	pythonPluginBuiltin := false
	pythonPluginSHA256 := ""
	cleanup := func() {}
	if !*noPlugins && !*noPython && (pluginPath == "" || pluginPath == "builtin") {
		temp, err := os.MkdirTemp("", "rkc-python-plugin-")
		if err != nil {
			return err
		}
		cleanup = func() { _ = os.RemoveAll(temp) }
		pluginPath, err = builtinplugins.MaterializePython(temp)
		if err != nil {
			cleanup()
			return err
		}
		pythonPluginBuiltin = true
		pythonPluginSHA256 = builtinplugins.PythonSHA256()
	}
	defer cleanup()

	cfg.Inventory.MaxFileBytes = *maxFile
	cfg.Inventory.MaxTextBytes = *maxText
	cfg.Inventory.MaxRepositoryBytes = *maxRepository
	cfg.Inventory.MaxFiles = *maxFiles
	cfg.Inventory.Exclude = append([]string(nil), excludes...)
	cfg.Plugins.Enabled = !*noPlugins
	cfg.Plugins.PythonAST.Enabled = !*noPython
	cfg.Plugins.PythonAST.Interpreter = *python
	cfg.Plugins.PythonAST.Script = firstNonBlank(*pythonPlugin, "builtin")
	cfg.Plugins.GoAST.Enabled = !*noGo
	cfg.Plugins.TypeScriptSyntax.Enabled = !*noTypeScript
	cfg.Frameworks.Enabled = !*noFrameworks
	cfg.Frameworks.Markdown = !*noMarkdown
	cfg.Frameworks.OpenAPIJSON = !*noOpenAPI
	cfg.Frameworks.JSONSchema = !*noJSONSchema
	cfg.Frameworks.PackageManifests = !*noManifests
	cfg.Frameworks.EnvironmentFiles = !*noEnvKeys
	cfg.Security.DetectSecrets = !*noSecretScan
	cfg.Security.RedactExports = !*unsafeIncludeSecrets
	cfg.Exports.NormalizedSources = *includeSources
	cfg.Exports.StaticSite = !*noStaticSite
	cfg.Exports.JSONLGraph = !*noJSONLGraph
	cfg.Exports.SearchIndex = !*noSearchIndex
	cfg.Exports.Integrations = !*noIntegrations
	cfg.Search.Enabled = !*noSearchIndex
	cfg.Exports.NotebookPackBytes = *notebookPackBytes
	cfg.Exports.SnapshotStore = *stateDir
	if err := cfg.Validate(); err != nil {
		return err
	}

	repositoryOrigin := acquired.Origin
	var stageEventMu sync.Mutex
	var stageEvents []scheduler.Event
	runID, err := scheduler.NewRunID()
	if err != nil {
		return err
	}
	runJournal, err := scheduler.OpenFileJournal(resolvedRuns, runID)
	if err != nil {
		return fmt.Errorf("open scheduler run journal: %w", err)
	}
	runJournalStarted := time.Now()
	runJournalFinalized := false
	defer func() {
		if runJournalFinalized {
			return
		}
		cause := resultErr
		if cause == nil {
			cause = errors.New("scan exited before successful command finalization")
		}
		_, finalizeErr := finalizeScanRunJournal(
			runJournal,
			cause,
			time.Since(runJournalStarted),
		)
		runJournalFinalized = true
		if finalizeErr != nil {
			resultErr = errors.Join(
				resultErr,
				fmt.Errorf("finalize failed scan journal: %w", finalizeErr),
			)
		}
	}()
	runJournalPath := runJournal.Path()
	bundle, coverage, scanErr := pipeline.Scan(ctx, pipeline.Options{
		Root: rootAbs, MaxFileBytes: *maxFile, MaxTextBytes: *maxText, MaxRepositoryBytes: *maxRepository, MaxFiles: *maxFiles,
		Excludes: excludes, SCIPIndexes: append([]string(nil), scipIndexes...),
		TracePaths:        append([]string(nil), tracePaths...),
		HistoryPath:       *historyPath,
		PythonInterpreter: *python, PythonPlugin: pluginPath, PluginTimeout: *pluginTimeout,
		PluginMaxOutput: *pluginOutput, PluginMaxStderr: 2 * 1024 * 1024,
		PluginMemoryMiB: cfg.Plugins.MemoryLimitMiB, PluginSwapMiB: cfg.Plugins.MemorySwapLimitMiB,
		PluginProcessLimit: cfg.Plugins.ProcessLimit, PluginSandboxRequired: cfg.Plugins.NativeWorkerSandbox == "required",
		PluginDenyNetwork: !cfg.Plugins.AllowNetwork, PluginDenyProcessSpawn: !cfg.Plugins.AllowProcessSpawn,
		PythonPluginBuiltin: pythonPluginBuiltin, PythonPluginSHA256: pythonPluginSHA256,
		FailClosedOnPluginError: cfg.Analysis.FailClosedOnPluginError,
		DisablePlugins:          *noPlugins, DisablePythonAST: *noPython, DisableGoAST: *noGo, DisableTypeScript: *noTypeScript,
		DisableFrameworks: *noFrameworks, DisableMarkdown: *noMarkdown, DisableOpenAPI: *noOpenAPI,
		DisableJSONSchema: *noJSONSchema, DisableManifests: *noManifests, DisableEnvKeys: *noEnvKeys, DisableSecretScan: *noSecretScan,
		ToolVersion: version, Origin: repositoryOrigin, ConfigDigest: cfg.Digest(), PolicyDigest: cfg.PolicyDigest(),
		PluginLockDigest: cfg.PluginDigest(), ToolchainDigest: toolchainDigest(*python),
		Cache:                  stageCache,
		StageWorkers:           *stageWorkers,
		RunID:                  runID,
		Journal:                runJournal,
		DeferJournalCompletion: true,
		ResourceBudget: scheduler.ResourceBudget{
			MemoryMiB: *stageMemory,
			CPU:       *stageWorkers,
			Processes: 8,
			OpenFiles: 512,
		},
		OnStageEvent: func(event scheduler.Event) {
			stageEventMu.Lock()
			stageEvents = append(stageEvents, event)
			stageEventMu.Unlock()
		},
	})
	if scanErr != nil {
		return fmt.Errorf(
			"scan run %s (journal %s): %w",
			runID,
			runJournalPath,
			scanErr,
		)
	}
	if err := scanCancellation(ctx); err != nil {
		return err
	}
	if *failOnErrors && coverage.DiagnosticsBySeverity["error"] > 0 {
		return fmt.Errorf("scan rejected before publication with %d error diagnostic(s)", coverage.DiagnosticsBySeverity["error"])
	}
	displayRepositoryOrigin := bundle.Snapshot.Git.Origin
	coverage, err = enforceWorkspacePrivacyWithCoverage(&bundle, cfg.Workspace.PrivacyMode, coverage)
	if err != nil {
		return err
	}
	repositoryOrigin = bundle.Snapshot.Git.Origin

	var sqlitePending *sqlitePublication
	sqliteNoop := false
	snapshotStoreNoop := false
	if resolvedDatabase != "" {
		stateMetadata := scanPublicationMetadata(
			cfg.Workspace.PrivacyMode,
			"atlas_target",
			outAbs,
			repositoryOrigin,
			rootAbs,
			acquired.Temporary,
		)
		sqlitePending, err = prepareSQLiteBundle(ctx, resolvedDatabase, bundle, stateMetadata)
		if err != nil {
			return fmt.Errorf("stage SQLite snapshot before atlas publication: %w", err)
		}
		defer func() { _ = sqlitePending.Close(errors.New("scan exited before SQLite cleanup")) }()
		bundle, coverage = sqlitePending.Bundle, sqlitePending.Coverage
		resolvedDatabase, sqliteNoop = sqlitePending.Path, sqlitePending.Noop
	}

	publication, err := prepareExport(rootAbs, outAbs, *force, bundle, coverage, rkcexport.Options{
		Root: rootAbs, NotebookMaxSize: *notebookPackBytes, IncludeSources: *includeSources,
		DisableStaticSite: *noStaticSite, DisableJSONLGraph: *noJSONLGraph,
		DisableSearchIndex: *noSearchIndex, DisableIntegrations: *noIntegrations,
		UnsafeIncludeSecrets: *unsafeIncludeSecrets,
	})
	if err != nil {
		if sqlitePending != nil {
			return errors.Join(err, sqlitePending.Close(err))
		}
		return err
	}
	defer func() { _ = publication.Abort() }()

	if err := scanCancellation(ctx); err != nil {
		if sqlitePending != nil {
			return errors.Join(err, sqlitePending.Close(err))
		}
		return err
	}
	if sqlitePending != nil {
		if err := sqlitePending.Commit(ctx); err != nil {
			return errors.Join(err, sqlitePending.Close(err))
		}
	}
	if err := publication.Commit(bundle.Snapshot.ID); err != nil {
		if sqlitePending != nil {
			return errors.Join(
				fmt.Errorf("SQLite snapshot %s is durable at %s but atlas publication failed: %w", bundle.Snapshot.ID, resolvedDatabase, err),
				sqlitePending.Close(err),
			)
		}
		return err
	}
	if sqlitePending != nil {
		if err := sqlitePending.Close(nil); err != nil {
			return fmt.Errorf("atlas and SQLite snapshot are durable but SQLite close failed: %w", err)
		}
	}

	// Publish the self-contained atlas before advancing the optional snapshot
	// store. A failed atlas rename must never leave CURRENT pointing at state
	// whose declared export was not installed. If the later store commit fails,
	// the already-published atlas remains complete and independently usable.
	if *stateDir != "" {
		store, err := snapshot.Open(*stateDir)
		if err != nil {
			return fmt.Errorf("atlas published at %s but snapshot store could not be opened: %w", outAbs, err)
		}
		stateMetadata := scanPublicationMetadata(
			cfg.Workspace.PrivacyMode,
			"export_root",
			outAbs,
			repositoryOrigin,
			rootAbs,
			acquired.Temporary,
		)
		transaction, err := store.Begin(bundle.Snapshot.ID, stateMetadata)
		if errors.Is(err, snapshot.ErrSnapshotExists) {
			storedBundle, storedCoverage, _, loadErr := store.Load(bundle.Snapshot.ID)
			if loadErr != nil {
				return fmt.Errorf("atlas published at %s but existing snapshot could not be verified: %w", outAbs, loadErr)
			}
			storedJSON, storedErr := rkcmodel.CanonicalJSON(storedBundle)
			currentJSON, currentErr := rkcmodel.CanonicalJSON(bundle)
			if storedErr != nil || currentErr != nil || !bytes.Equal(storedJSON, currentJSON) || !reflect.DeepEqual(storedCoverage, coverage) {
				return fmt.Errorf("atlas published at %s but snapshot ID %s is already bound to different canonical content", outAbs, bundle.Snapshot.ID)
			}
			if err := store.SetCurrent(bundle.Snapshot.ID); err != nil {
				return fmt.Errorf("atlas published at %s but existing snapshot could not be selected: %w", outAbs, err)
			}
			snapshotStoreNoop = true
			err = nil
		}
		if err != nil {
			return fmt.Errorf("atlas published at %s but snapshot transaction could not start: %w", outAbs, err)
		}
		if transaction != nil {
			committed := false
			defer func() {
				if !committed {
					_ = transaction.Abort("atlas published but snapshot store did not commit")
				}
			}()
			if err := transaction.WriteBundle(bundle); err != nil {
				return fmt.Errorf("atlas published at %s but snapshot bundle write failed: %w", outAbs, err)
			}
			if err := transaction.WriteCoverage(coverage); err != nil {
				return fmt.Errorf("atlas published at %s but snapshot coverage write failed: %w", outAbs, err)
			}
			if err := transaction.Commit(); err != nil {
				return fmt.Errorf("atlas published at %s but snapshot store commit failed: %w", outAbs, err)
			}
			committed = true
		}
	}

	runReport, journalErr := finalizeScanRunJournal(
		runJournal,
		nil,
		time.Since(runJournalStarted),
	)
	runJournalFinalized = true
	if journalErr != nil {
		return fmt.Errorf(
			"scan run %s journal %s did not finalize and replay cleanly: %w",
			runID,
			runJournalPath,
			journalErr,
		)
	}
	if !runReport.Terminal || runReport.Interrupted ||
		scheduler.JournalState(runReport.State) != scheduler.JournalStateSucceeded {
		return fmt.Errorf(
			"scan run %s journal %s has unexpected terminal state %q",
			runID,
			runJournalPath,
			runReport.State,
		)
	}

	stageEventMu.Lock()
	cacheHits := 0
	for _, event := range stageEvents {
		if event.State == "cached" {
			cacheHits++
		}
	}
	stageEventMu.Unlock()
	displaySource := displayRepositoryOrigin
	if displaySource == "" {
		displaySource = acquired.Origin
	}
	if displaySource == "" {
		displaySource = rootAbs
	}
	summary := map[string]any{
		"snapshot_id": bundle.Snapshot.ID, "source": displaySource, "source_kind": acquired.Kind, "output": outAbs, "snapshot_store": *stateDir, "snapshot_store_noop": snapshotStoreNoop,
		"database": resolvedDatabase, "database_noop": sqliteNoop, "cache": resolvedCache, "cache_hits": cacheHits,
		"run_id": runID, "run_journal": runJournalPath,
		"artifacts": coverage.ArtifactsInventoried, "text_artifacts": coverage.TextArtifacts,
		"syntax_parsed": coverage.ArtifactsSyntacticallyParsed, "semantic_parsed": coverage.ArtifactsSemanticallyParsed,
		"symbols": coverage.SymbolsTotal, "edges": coverage.EdgesTotal, "unresolved_edges": coverage.UnresolvedEdges,
		"error_diagnostics": coverage.DiagnosticsBySeverity["error"], "deterministic_digest": coverage.DeterministicOutputDigest,
	}
	if *jsonSummary {
		return writeJSONStdout(summary)
	}
	fmt.Printf("Snapshot: %s\n", bundle.Snapshot.ID)
	fmt.Printf("Source: %s (%s)\n", displaySource, acquired.Kind)
	if *keepMaterialized && acquired.Kind == acquire.KindGit {
		fmt.Printf("Materialized repository: %s\n", acquired.MaterializedPath)
	}
	fmt.Printf("Inventory: %d artifacts; %d text; %d syntax-parsed; %d semantic-parsed; %d explicit exclusions\n",
		coverage.ArtifactsInventoried, coverage.TextArtifacts, coverage.ArtifactsSyntacticallyParsed,
		coverage.ArtifactsSemanticallyParsed, coverage.ArtifactsExcluded)
	fmt.Printf("Graph: %d symbols; %d edges; %d unresolved; evidence ratio %.4f\n",
		coverage.SymbolsTotal, coverage.EdgesTotal, coverage.UnresolvedEdges, coverage.SymbolEvidenceRatio)
	fmt.Printf("Output: %s\n", outAbs)
	fmt.Printf("Run journal: %s (%s)\n", runJournalPath, runID)
	if *stateDir != "" {
		fmt.Printf("Snapshot store: %s (reused=%t)\n", *stateDir, snapshotStoreNoop)
	}
	if resolvedDatabase != "" {
		fmt.Printf("SQLite store: %s (idempotent=%t)\n", resolvedDatabase, sqliteNoop)
	}
	if resolvedCache != "" {
		fmt.Printf("Stage cache: %s (%d hit(s))\n", resolvedCache, cacheHits)
	} else {
		fmt.Println("Stage cache: disabled (clean scan)")
	}
	fmt.Printf("Browse: rkc serve --dir %s\n", outAbs)
	return nil
}

// enforceWorkspacePrivacy applies the configured publication boundary after
// analysis and before any atlas or durable store observes the bundle. Relative
// artifact and evidence paths are intentionally retained in every mode: they
// are the portable citation contract used by humans, agents, and integrations.
func enforceWorkspacePrivacy(bundle *rkcmodel.Bundle, mode string) (rkcmodel.Coverage, error) {
	return enforceWorkspacePrivacyWithCoverage(bundle, mode, rkcmodel.Coverage{})
}

func enforceWorkspacePrivacyWithCoverage(bundle *rkcmodel.Bundle, mode string, _ rkcmodel.Coverage) (rkcmodel.Coverage, error) {
	if bundle == nil {
		return rkcmodel.Coverage{}, errors.New("workspace privacy transformation requires a bundle")
	}
	switch mode {
	case "full":
		// Full mode deliberately retains machine-local operational provenance.
	case "paths-relative":
		bundle.Snapshot.RootPath = ""
	case "redacted":
		bundle.Snapshot.RootPath = ""
		origin := bundle.Snapshot.Git.Origin
		bundle.Snapshot.Git.Origin = ""
		if bundle.Snapshot.Metadata != nil {
			for key, value := range bundle.Snapshot.Metadata {
				if key == "source_reference" || key == "repository_origin" || origin != "" && value == origin {
					delete(bundle.Snapshot.Metadata, key)
				}
			}
		}
		for index := range bundle.Nodes {
			node := &bundle.Nodes[index]
			if node.Kind != "repository" || node.ID != bundle.Snapshot.RepositoryID {
				continue
			}
			node.QualifiedName = ""
			if node.Attributes != nil {
				for key, value := range node.Attributes {
					text, isText := value.(string)
					if key == "git_origin" || key == "repository_origin" || key == "source_reference" ||
						origin != "" && isText && text == origin {
						delete(node.Attributes, key)
					}
				}
			}
		}
	default:
		return rkcmodel.Coverage{}, errors.New("workspace privacy mode is invalid")
	}

	rkcmodel.SortBundle(bundle)
	report := rkcmodel.ValidateBundle(*bundle, rkcmodel.ValidationOptions{
		StrictVocabulary: true,
		RequireEvidence:  true,
	})
	if report.HasErrors() {
		return rkcmodel.Coverage{}, errors.New("workspace privacy transformation produced an invalid canonical bundle")
	}
	return rkcmodel.BuildCoverage(*bundle), nil
}

// scanPublicationMetadata keeps operational absolute paths only when the user
// explicitly selects full privacy mode. The same constructor feeds both
// SQLite and filesystem snapshot publication so their privacy behavior cannot
// silently drift apart.
func scanPublicationMetadata(
	mode string,
	targetKey string,
	target string,
	repositoryOrigin string,
	repositoryRoot string,
	temporaryRepository bool,
) map[string]string {
	metadata := map[string]string{}
	if mode == "full" {
		if targetKey != "" && target != "" {
			metadata[targetKey] = target
		}
		if repositoryRoot != "" && !temporaryRepository {
			metadata["repository_root"] = repositoryRoot
		}
	}
	if mode != "redacted" && repositoryOrigin != "" {
		metadata["repository_origin"] = repositoryOrigin
	}
	return metadata
}

func scanCancellation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scan cancelled; staged output was not published: %w", err)
	}
	return nil
}

func finalizeScanRunJournal(
	journal *scheduler.FileJournal,
	runErr error,
	duration time.Duration,
) (scheduler.JournalReport, error) {
	if journal == nil {
		return scheduler.JournalReport{}, errors.New("scheduler run journal is nil")
	}
	path := journal.Path()
	state := scheduler.JournalStateSucceeded
	if runErr != nil {
		state = scheduler.JournalStateFailed
		if errors.Is(runErr, context.Canceled) ||
			errors.Is(runErr, context.DeadlineExceeded) {
			state = scheduler.JournalStateCancelled
		}
	}
	record := scheduler.JournalRecord{
		RunID:    journal.RunID(),
		Kind:     scheduler.JournalKindRun,
		State:    state,
		Duration: duration,
	}
	if runErr != nil {
		record.Error = runErr.Error()
	}
	appendContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	appendErr := journal.Append(appendContext, record)
	cancel()
	closeErr := journal.Close()
	report, replayErr := scheduler.ReadFileJournal(path)
	if appendErr != nil || closeErr != nil || replayErr != nil {
		return report, errors.Join(appendErr, closeErr, replayErr)
	}
	return report, nil
}

func publishExport(root, output string, force bool, bundle rkcmodel.Bundle, coverage rkcmodel.Coverage, options rkcexport.Options) error {
	publication, err := prepareExport(root, output, force, bundle, coverage, options)
	if err != nil {
		return err
	}
	defer func() { _ = publication.Abort() }()
	return publication.Commit(bundle.Snapshot.ID)
}

func prepareExport(root, output string, force bool, bundle rkcmodel.Bundle, coverage rkcmodel.Coverage, options rkcexport.Options) (*safeoutput.Transaction, error) {
	publication, err := safeoutput.Begin(output, root, force, "atlas")
	if err != nil {
		return nil, err
	}
	options.Output = publication.Staging
	if err := rkcexport.WriteAll(bundle, coverage, options); err != nil {
		_ = publication.Abort()
		return nil, err
	}
	return publication, nil
}

func discoverFlagValue(args []string, name string) string {
	prefix := "--" + name + "="
	for index, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
		if arg == "--"+name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func toolchainDigest(python string) string {
	data, _ := json.Marshal(map[string]string{"python": python, "rkc": version})
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}
