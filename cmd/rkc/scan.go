package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/repository-knowledge-compiler/rkc/internal/acquire"
	"github.com/repository-knowledge-compiler/rkc/internal/builtinplugins"
	rkcexport "github.com/repository-knowledge-compiler/rkc/internal/export"
	"github.com/repository-knowledge-compiler/rkc/internal/pipeline"
	"github.com/repository-knowledge-compiler/rkc/internal/snapshot"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

func runScan(args []string) error {
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", configPath, "JSON configuration file; omitted uses built-in defaults")
	out := fs.String("out", ".rkc", "generated output directory")
	stateDir := fs.String("state-dir", cfg.Exports.SnapshotStore, "optional immutable snapshot store directory")
	maxText := fs.Int64("max-text-bytes", cfg.Inventory.MaxTextBytes, "largest text file parsed or normalized")
	maxRepository := fs.Int64("max-repository-bytes", cfg.Inventory.MaxRepositoryBytes, "maximum encountered repository bytes; 0 disables")
	maxFiles := fs.Int("max-files", cfg.Inventory.MaxFiles, "maximum encountered paths; 0 disables")
	python := fs.String("python", cfg.Plugins.PythonAST.Interpreter, "Python interpreter for the AST adapter")
	pythonPlugin := fs.String("python-plugin", cfg.Plugins.PythonAST.Script, "Python extractor path or 'builtin'")
	pluginTimeout := fs.Duration("plugin-timeout", cfg.PluginTimeout(), "per-plugin wall-clock timeout")
	pluginOutput := fs.Int64("plugin-output-bytes", cfg.Plugins.MaximumOutputBytes, "maximum plugin stdout bytes")
	noPlugins := fs.Bool("no-plugins", !cfg.Plugins.Enabled, "disable all language adapters")
	noPython := fs.Bool("no-python", !cfg.Plugins.PythonAST.Enabled, "disable the Python syntax adapter")
	noGo := fs.Bool("no-go", !cfg.Plugins.GoAST.Enabled, "disable the Go syntax adapter")
	noTypeScript := fs.Bool("no-typescript", !cfg.Plugins.TypeScriptSyntax.Enabled, "disable the JavaScript and TypeScript syntax adapter")
	noFrameworks := fs.Bool("no-frameworks", !cfg.Frameworks.Enabled, "disable all deterministic framework and document extractors")
	noMarkdown := fs.Bool("no-markdown", !cfg.Frameworks.Markdown, "disable Markdown document structure extraction")
	noOpenAPI := fs.Bool("no-openapi", !cfg.Frameworks.OpenAPIJSON, "disable JSON OpenAPI extraction")
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
	source := "."
	if fs.NArg() > 1 {
		return errors.New("scan accepts at most one repository path or Git URL")
	}
	if fs.NArg() == 1 {
		source = fs.Arg(0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	outAbs, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if rootAbs == outAbs {
		return errors.New("output directory cannot be the repository root")
	}
	if rel, err := filepath.Rel(rootAbs, outAbs); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		excludes = append(excludes, filepath.ToSlash(rel))
	}
	if *stateDir != "" {
		stateAbs, err := filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
		if rel, err := filepath.Rel(rootAbs, stateAbs); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			excludes = append(excludes, filepath.ToSlash(rel))
		}
		*stateDir = stateAbs
	}
	if _, err := os.Stat(outAbs); err == nil && !*force {
		return fmt.Errorf("output directory exists: %s; use --force to replace it", outAbs)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	pluginPath := strings.TrimSpace(*pythonPlugin)
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
	}
	defer cleanup()

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

	sourceReference := ""
	if acquired.Kind == acquire.KindGit {
		sourceReference = acquired.RedactedSource
	}
	bundle, coverage, err := pipeline.Scan(ctx, pipeline.Options{
		Root: rootAbs, MaxTextBytes: *maxText, MaxRepositoryBytes: *maxRepository, MaxFiles: *maxFiles,
		Excludes: excludes, PythonInterpreter: *python, PythonPlugin: pluginPath, PluginTimeout: *pluginTimeout,
		PluginMaxOutput: *pluginOutput, PluginMaxStderr: 2 * 1024 * 1024,
		DisablePlugins: *noPlugins, DisablePythonAST: *noPython, DisableGoAST: *noGo, DisableTypeScript: *noTypeScript,
		DisableFrameworks: *noFrameworks, DisableMarkdown: *noMarkdown, DisableOpenAPI: *noOpenAPI,
		DisableJSONSchema: *noJSONSchema, DisableManifests: *noManifests, DisableEnvKeys: *noEnvKeys, DisableSecretScan: *noSecretScan,
		ToolVersion: version, SourceReference: sourceReference, ConfigDigest: cfg.Digest(), PolicyDigest: cfg.PolicyDigest(),
		PluginLockDigest: cfg.PluginDigest(), ToolchainDigest: toolchainDigest(*python),
	})
	if err != nil {
		return err
	}

	if err := publishExport(rootAbs, outAbs, *force, bundle, coverage, rkcexport.Options{
		Root: rootAbs, NotebookMaxSize: *notebookPackBytes, IncludeSources: *includeSources,
		DisableStaticSite: *noStaticSite, DisableJSONLGraph: *noJSONLGraph,
		DisableSearchIndex: *noSearchIndex, DisableIntegrations: *noIntegrations,
		UnsafeIncludeSecrets: *unsafeIncludeSecrets,
	}); err != nil {
		return err
	}

	if *stateDir != "" {
		store, err := snapshot.Open(*stateDir)
		if err != nil {
			return err
		}
		stateMetadata := map[string]string{"repository_source": acquired.RedactedSource, "export_root": outAbs}
		if !acquired.Temporary {
			stateMetadata["repository_root"] = rootAbs
		}
		transaction, err := store.Begin(bundle.Snapshot.ID, stateMetadata)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = transaction.Abort("scan command did not commit")
			}
		}()
		if err := transaction.WriteBundle(bundle); err != nil {
			return err
		}
		if err := transaction.WriteCoverage(coverage); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return err
		}
		committed = true
	}

	summary := map[string]any{
		"snapshot_id": bundle.Snapshot.ID, "source": acquired.RedactedSource, "source_kind": acquired.Kind, "output": outAbs, "snapshot_store": *stateDir,
		"artifacts": coverage.ArtifactsInventoried, "text_artifacts": coverage.TextArtifacts,
		"syntax_parsed": coverage.ArtifactsSyntacticallyParsed, "semantic_parsed": coverage.ArtifactsSemanticallyParsed,
		"symbols": coverage.SymbolsTotal, "edges": coverage.EdgesTotal, "unresolved_edges": coverage.UnresolvedEdges,
		"error_diagnostics": coverage.DiagnosticsBySeverity["error"], "deterministic_digest": coverage.DeterministicOutputDigest,
	}
	if *jsonSummary {
		return writeJSONStdout(summary)
	}
	fmt.Printf("Snapshot: %s\n", bundle.Snapshot.ID)
	fmt.Printf("Source: %s (%s)\n", acquired.RedactedSource, acquired.Kind)
	if *keepMaterialized && acquired.Kind == acquire.KindGit {
		fmt.Printf("Materialized repository: %s\n", acquired.MaterializedPath)
	}
	fmt.Printf("Inventory: %d artifacts; %d text; %d syntax-parsed; %d semantic-parsed; %d explicit exclusions\n",
		coverage.ArtifactsInventoried, coverage.TextArtifacts, coverage.ArtifactsSyntacticallyParsed,
		coverage.ArtifactsSemanticallyParsed, coverage.ArtifactsExcluded)
	fmt.Printf("Graph: %d symbols; %d edges; %d unresolved; evidence ratio %.4f\n",
		coverage.SymbolsTotal, coverage.EdgesTotal, coverage.UnresolvedEdges, coverage.SymbolEvidenceRatio)
	fmt.Printf("Output: %s\n", outAbs)
	if *stateDir != "" {
		fmt.Printf("Snapshot store: %s\n", *stateDir)
	}
	fmt.Printf("Browse: rkc serve --dir %s\n", outAbs)
	if *failOnErrors && coverage.DiagnosticsBySeverity["error"] > 0 {
		return fmt.Errorf("scan completed with %d error diagnostic(s)", coverage.DiagnosticsBySeverity["error"])
	}
	return nil
}

func publishExport(root, output string, force bool, bundle rkcmodel.Bundle, coverage rkcmodel.Coverage, options rkcexport.Options) error {
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".rkc-build-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temp)
		}
	}()
	options.Output = temp
	if err := rkcexport.WriteAll(bundle, coverage, options); err != nil {
		return err
	}
	if force {
		if err := os.RemoveAll(output); err != nil {
			return fmt.Errorf("remove old output: %w", err)
		}
	}
	if err := os.Rename(temp, output); err != nil {
		return fmt.Errorf("publish output directory: %w", err)
	}
	committed = true
	return nil
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
