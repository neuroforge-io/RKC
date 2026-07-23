package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/builtinplugins"
	"github.com/neuroforge-io/RKC/internal/pipeline"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func runPlan(args []string) error {
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", configPath, "JSON configuration file; omitted uses built-in defaults")
	cacheDir := fs.String("cache-dir", defaultStageCacheDirectory(), "verified incremental stage cache directory")
	noCache := fs.Bool("no-cache", !cfg.Analysis.Incremental, "plan a clean scan without cache reuse")
	maxFile := fs.Int64("max-file-bytes", cfg.Inventory.MaxFileBytes, "largest individual regular file hashed or parsed; 0 disables")
	maxText := fs.Int64("max-text-bytes", cfg.Inventory.MaxTextBytes, "largest text file parsed or normalized")
	maxRepository := fs.Int64("max-repository-bytes", cfg.Inventory.MaxRepositoryBytes, "maximum encountered repository bytes; 0 disables")
	maxFiles := fs.Int("max-files", cfg.Inventory.MaxFiles, "maximum encountered paths; 0 disables")
	python := fs.String("python", cfg.Plugins.PythonAST.Interpreter, "Python interpreter identity for cache planning")
	pluginTimeout := fs.Duration("plugin-timeout", cfg.PluginTimeout(), "per-plugin wall-clock timeout")
	pluginOutput := fs.Int64("plugin-output-bytes", cfg.Plugins.MaximumOutputBytes, "maximum plugin stdout bytes")
	noPlugins := fs.Bool("no-plugins", !cfg.Plugins.Enabled, "disable all language adapters")
	noPython := fs.Bool("no-python", !cfg.Plugins.PythonAST.Enabled, "disable the Python syntax adapter")
	noGo := fs.Bool("no-go", !cfg.Plugins.GoAST.Enabled, "disable the Go syntax adapter")
	noTypeScript := fs.Bool("no-typescript", !cfg.Plugins.TypeScriptSyntax.Enabled, "disable JavaScript and TypeScript syntax")
	noFrameworks := fs.Bool("no-frameworks", !cfg.Frameworks.Enabled, "disable deterministic framework extractors")
	noMarkdown := fs.Bool("no-markdown", !cfg.Frameworks.Markdown, "disable Markdown extraction")
	noOpenAPI := fs.Bool("no-openapi", !cfg.Frameworks.OpenAPIJSON, "disable JSON OpenAPI extraction")
	noJSONSchema := fs.Bool("no-json-schema", !cfg.Frameworks.JSONSchema, "disable JSON Schema extraction")
	noManifests := fs.Bool("no-manifests", !cfg.Frameworks.PackageManifests, "disable package manifests")
	noEnvKeys := fs.Bool("no-env-keys", !cfg.Frameworks.EnvironmentFiles, "disable environment key extraction")
	noSecretScan := fs.Bool("no-secret-scan", !cfg.Security.DetectSecrets, "disable secret-pattern scanning")
	jsonOutput := fs.Bool("json", false, "print machine-readable plan")
	excludes := stringList(append([]string(nil), cfg.Inventory.Exclude...))
	fs.Var(&excludes, "exclude", "repository-relative exclusion; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configFlag != configPath {
		return errors.New("--config must be supplied only once; its values establish flag defaults")
	}
	if fs.NArg() > 1 {
		return errors.New("plan accepts at most one local repository path")
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	var cache *pipeline.StageCache
	if !*noCache {
		resolvedCache, err := safeoutput.ResolveTarget(*cacheDir, root)
		if err != nil {
			return fmt.Errorf("resolve stage cache: %w", err)
		}
		insideRepository, err := pathIsWithin(root, resolvedCache)
		if err != nil {
			return fmt.Errorf("compare repository and stage cache: %w", err)
		}
		if insideRepository {
			return fmt.Errorf("%w: stage cache must remain outside the scanned repository", safeoutput.ErrUnsafeTarget)
		}
		cache, err = pipeline.OpenStageCache(resolvedCache)
		if err != nil {
			return err
		}
	}
	cfg.Inventory.MaxFileBytes = *maxFile
	cfg.Inventory.MaxTextBytes = *maxText
	cfg.Inventory.MaxRepositoryBytes = *maxRepository
	cfg.Inventory.MaxFiles = *maxFiles
	cfg.Inventory.Exclude = append([]string(nil), excludes...)
	cfg.Plugins.Enabled = !*noPlugins
	cfg.Plugins.PythonAST.Enabled = !*noPython
	cfg.Plugins.PythonAST.Interpreter = *python
	cfg.Plugins.GoAST.Enabled = !*noGo
	cfg.Plugins.TypeScriptSyntax.Enabled = !*noTypeScript
	cfg.Frameworks.Enabled = !*noFrameworks
	cfg.Frameworks.Markdown = !*noMarkdown
	cfg.Frameworks.OpenAPIJSON = !*noOpenAPI
	cfg.Frameworks.JSONSchema = !*noJSONSchema
	cfg.Frameworks.PackageManifests = !*noManifests
	cfg.Frameworks.EnvironmentFiles = !*noEnvKeys
	cfg.Security.DetectSecrets = !*noSecretScan
	if err := cfg.Validate(); err != nil {
		return err
	}
	pluginDigest := ""
	if !*noPlugins && !*noPython {
		pluginDigest = builtinplugins.PythonSHA256()
	}
	plan, err := pipeline.Plan(context.Background(), pipeline.Options{
		Root: root, MaxFileBytes: *maxFile, MaxTextBytes: *maxText,
		MaxRepositoryBytes: *maxRepository, MaxFiles: *maxFiles, Excludes: excludes,
		PythonInterpreter: *python, PluginTimeout: *pluginTimeout,
		PluginMaxOutput: *pluginOutput, PluginMaxStderr: 2 * 1024 * 1024,
		PluginMemoryMiB: cfg.Plugins.MemoryLimitMiB, PluginSwapMiB: cfg.Plugins.MemorySwapLimitMiB,
		PluginProcessLimit:    cfg.Plugins.ProcessLimit,
		PluginSandboxRequired: cfg.Plugins.NativeWorkerSandbox == "required",
		PluginDenyNetwork:     !cfg.Plugins.AllowNetwork, PluginDenyProcessSpawn: !cfg.Plugins.AllowProcessSpawn,
		PythonPluginBuiltin: true, PythonPluginSHA256: pluginDigest,
		FailClosedOnPluginError: cfg.Analysis.FailClosedOnPluginError,
		DisablePlugins:          *noPlugins, DisablePythonAST: *noPython,
		DisableGoAST: *noGo, DisableTypeScript: *noTypeScript,
		DisableFrameworks: *noFrameworks, DisableMarkdown: *noMarkdown,
		DisableOpenAPI: *noOpenAPI, DisableJSONSchema: *noJSONSchema,
		DisableManifests: *noManifests, DisableEnvKeys: *noEnvKeys,
		DisableSecretScan: *noSecretScan,
		ToolVersion:       version, ConfigDigest: cfg.Digest(), PolicyDigest: cfg.PolicyDigest(),
		PluginLockDigest: cfg.PluginDigest(), ToolchainDigest: toolchainDigest(*python),
		Cache: cache,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(plan)
	}
	fmt.Printf("Scan plan: %s\n", plan.Root)
	if plan.CacheRoot == "" {
		fmt.Println("Stage cache: disabled")
	} else {
		fmt.Printf("Stage cache: %s\n", plan.CacheRoot)
	}
	fmt.Printf(
		"Stages: %d total; %d execute; %d cache hit; %d disabled\n",
		plan.Summary.Stages,
		plan.Summary.Execute,
		plan.Summary.CacheHit,
		plan.Summary.Disabled,
	)
	for _, stage := range plan.Stages {
		fmt.Printf(
			"- %-21s %-10s %s\n",
			stage.ID,
			stage.Disposition,
			stage.Reason,
		)
	}
	fmt.Println("Planning reads and hashes source for inventory/normalization; it does not execute analyzers or publish output.")
	return nil
}
