package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	rkcplugin "github.com/neuroforge-io/RKC/internal/plugin"
)

const (
	configurationSchemaVersion = "0.2.0"
	modelMaximumRSSMiB         = int64(2560)
)

type Configuration struct {
	SchemaURI     string              `json:"$schema,omitempty"`
	SchemaVersion string              `json:"schema_version"`
	Workspace     WorkspaceConfig     `json:"workspace"`
	Inventory     InventoryConfig     `json:"inventory"`
	Analysis      AnalysisConfig      `json:"analysis"`
	Plugins       PluginsConfig       `json:"plugins"`
	Frameworks    FrameworksConfig    `json:"frameworks"`
	Security      SecurityConfig      `json:"security"`
	Documentation DocumentationConfig `json:"documentation"`
	Model         ModelConfig         `json:"model"`
	Search        SearchConfig        `json:"search"`
	Exports       ExportsConfig       `json:"exports"`
	QualityGates  QualityGatesConfig  `json:"quality_gates"`
}

type WorkspaceConfig struct {
	Name        string `json:"name,omitempty"`
	PrivacyMode string `json:"privacy_mode"`
	Output      string `json:"output"`
}

type InventoryConfig struct {
	MaxFileBytes              int64    `json:"max_file_bytes"`
	MaxTextBytes              int64    `json:"max_text_bytes"`
	MaxRepositoryBytes        int64    `json:"max_repository_bytes"`
	MaxFiles                  int      `json:"max_files"`
	FollowSymlinks            bool     `json:"follow_symlinks"`
	Exclude                   []string `json:"exclude"`
	ClassifyVendorDirectories bool     `json:"classify_vendor_directories"`
	ClassifyGeneratedFiles    bool     `json:"classify_generated_files"`
}

type AnalysisConfig struct {
	Tiers                   []string `json:"tiers"`
	FailClosedOnPluginError bool     `json:"fail_closed_on_plugin_error"`
	Incremental             bool     `json:"incremental"`
	RuntimeEvidence         bool     `json:"runtime_evidence"`
}

type AnalyzerToggleConfig struct {
	Enabled bool `json:"enabled"`
}

type PythonASTConfig struct {
	Enabled        bool   `json:"enabled"`
	Interpreter    string `json:"interpreter"`
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type PluginsConfig struct {
	Enabled             bool                 `json:"enabled"`
	Directories         []string             `json:"directories"`
	AllowNetwork        bool                 `json:"allow_network"`
	AllowProcessSpawn   bool                 `json:"allow_process_spawn"`
	NativeWorkerSandbox string               `json:"native_worker_sandbox"`
	TimeoutSeconds      int                  `json:"timeout_seconds"`
	MemoryLimitMiB      int64                `json:"memory_limit_mib"`
	MemorySwapLimitMiB  int64                `json:"memory_swap_limit_mib"`
	ProcessLimit        int                  `json:"process_limit"`
	LegacyMemoryLimitMB int64                `json:"memory_limit_mb,omitempty"`
	MaximumOutputBytes  int64                `json:"maximum_output_bytes"`
	PythonAST           PythonASTConfig      `json:"python_ast"`
	GoAST               AnalyzerToggleConfig `json:"go_ast"`
	TypeScriptSyntax    AnalyzerToggleConfig `json:"typescript_syntax"`
}

type FrameworksConfig struct {
	Enabled          bool `json:"enabled"`
	Markdown         bool `json:"markdown"`
	OpenAPIJSON      bool `json:"openapi_json"`
	JSONSchema       bool `json:"json_schema"`
	PackageManifests bool `json:"package_manifests"`
	EnvironmentFiles bool `json:"environment_files"`
}

type SecurityConfig struct {
	DetectSecrets bool `json:"detect_secrets"`
	RedactExports bool `json:"redact_exports"`
}

type DocumentationConfig struct {
	DeterministicTemplates        bool `json:"deterministic_templates"`
	ModelSynthesis                bool `json:"model_synthesis"`
	RequireEvidenceForEveryClaim  bool `json:"require_evidence_for_every_claim"`
	RejectUnknownSymbolReferences bool `json:"reject_unknown_symbol_references"`
}

type ModelConfig struct {
	Provider            string  `json:"provider"`
	ModelName           string  `json:"model,omitempty"`
	ModelPath           string  `json:"model_path,omitempty"`
	ContextTokens       int     `json:"context_tokens"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
	Temperature         float64 `json:"temperature"`
	MaxRSSMiB           int64   `json:"max_rss_mib"`
	LegacyPeakMemoryMiB int64   `json:"peak_memory_limit_mb,omitempty"`
}

type SearchConfig struct {
	Enabled            bool   `json:"enabled"`
	Lexical            bool   `json:"lexical"`
	Embeddings         bool   `json:"embeddings"`
	EmbeddingModel     string `json:"embedding_model,omitempty"`
	GraphExpansionHops int    `json:"graph_expansion_hops"`
}

type ExportsConfig struct {
	NormalizedSources bool   `json:"normalized_sources"`
	JSONLGraph        bool   `json:"jsonl_graph"`
	StaticSite        bool   `json:"static_site"`
	SearchIndex       bool   `json:"search_index"`
	Integrations      bool   `json:"integrations"`
	NotebookPackBytes int    `json:"notebook_pack_bytes"`
	SnapshotStore     string `json:"snapshot_store,omitempty"`
	// Compatibility for the 0.1 configuration.
	JSONL      *bool `json:"jsonl,omitempty"`
	SARIF      *bool `json:"sarif,omitempty"`
	Markdown   *bool `json:"markdown,omitempty"`
	SQLite     *bool `json:"sqlite,omitempty"`
	NotebookLM *bool `json:"notebooklm,omitempty"`
	MCP        *bool `json:"mcp,omitempty"`
}

type QualityGatesConfig struct {
	MinInventoryAccounting   float64 `json:"min_inventory_accounting"`
	MinSymbolEvidence        float64 `json:"min_symbol_evidence"`
	MinEdgeResolution        float64 `json:"min_edge_resolution"`
	MinClaimCitation         float64 `json:"min_claim_citation"`
	MaxErrorDiagnostics      int     `json:"max_error_diagnostics"`
	MaxUnresolvedEdges       int     `json:"max_unresolved_edges"`
	MaxHighConfidenceSecrets int     `json:"max_high_confidence_secrets"`
	RequireDeterminism       bool    `json:"require_determinism"`
	// Compatibility values from 0.1.
	InventoryAccounting       *float64 `json:"inventory_accounting,omitempty"`
	PublicSymbolDocumentation *float64 `json:"public_symbol_documentation,omitempty"`
	UncitedGeneratedClaims    *int     `json:"uncited_generated_claims,omitempty"`
	DanglingEdges             *int     `json:"dangling_edges,omitempty"`
}

func defaultConfiguration() Configuration {
	return Configuration{
		SchemaURI: "../schemas/config.schema.json", SchemaVersion: configurationSchemaVersion,
		Workspace:     WorkspaceConfig{PrivacyMode: "paths-relative", Output: ".rkc"},
		Inventory:     InventoryConfig{MaxFileBytes: 1 << 30, MaxTextBytes: 2 << 20, MaxRepositoryBytes: 20 << 30, MaxFiles: 500000, Exclude: defaultInventoryExclusions(), ClassifyVendorDirectories: true, ClassifyGeneratedFiles: true},
		Analysis:      AnalysisConfig{Tiers: []string{"inventory", "syntax", "framework"}, FailClosedOnPluginError: true, Incremental: true},
		Plugins:       PluginsConfig{Enabled: true, Directories: []string{"./plugins"}, NativeWorkerSandbox: "required", TimeoutSeconds: 60, MemoryLimitMiB: 1024, MemorySwapLimitMiB: 128, ProcessLimit: 1, MaximumOutputBytes: 64 << 20, PythonAST: PythonASTConfig{Enabled: true, Interpreter: "python3", Script: "builtin", TimeoutSeconds: 60}, GoAST: AnalyzerToggleConfig{Enabled: true}, TypeScriptSyntax: AnalyzerToggleConfig{Enabled: true}},
		Frameworks:    FrameworksConfig{Enabled: true, Markdown: true, OpenAPIJSON: true, JSONSchema: true, PackageManifests: true, EnvironmentFiles: true},
		Security:      SecurityConfig{DetectSecrets: true, RedactExports: true},
		Documentation: DocumentationConfig{DeterministicTemplates: true, RequireEvidenceForEveryClaim: true, RejectUnknownSymbolReferences: true},
		Model:         ModelConfig{Provider: "disabled", ContextTokens: 4096, MaxOutputTokens: 768, Temperature: 0, MaxRSSMiB: 2560},
		Search:        SearchConfig{Enabled: true, Lexical: true, Embeddings: false, GraphExpansionHops: 2},
		Exports:       ExportsConfig{NormalizedSources: true, JSONLGraph: true, StaticSite: true, SearchIndex: true, Integrations: true, NotebookPackBytes: 1000000},
		QualityGates:  QualityGatesConfig{MinInventoryAccounting: 1, MinSymbolEvidence: 1, MinEdgeResolution: 0, MinClaimCitation: 1, MaxErrorDiagnostics: 0, MaxUnresolvedEdges: -1, MaxHighConfidenceSecrets: 0, RequireDeterminism: true},
	}
}

// defaultInventoryExclusions lists only exact repository-relative paths. The
// inventory engine treats each value as that path plus its descendants; it does
// not interpret Git ignore files or glob metacharacters.
func defaultInventoryExclusions() []string {
	return []string{
		".cache",
		".coverage",
		".git",
		".mypy_cache",
		".pytest_cache",
		".rkc",
		".rkc-coverage",
		".rkc-downloads",
		".rkc-models",
		".rkc-runtime",
		".rkc-state",
		".rkc.rkc-derived",
		".ruff_cache",
		".venv",
		"__pycache__",
		"bin",
		"coverage",
		"coverage.out",
		"coverage.xml",
		"dist",
		"htmlcov",
		"venv",
	}
}

func loadConfiguration(path string) (Configuration, error) {
	cfg := defaultConfiguration()
	if strings.TrimSpace(path) == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Configuration{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Configuration{}, fmt.Errorf("decode configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Configuration{}, errors.New("configuration contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return Configuration{}, err
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Configuration{}, err
	}
	return cfg, nil
}

func (cfg *Configuration) normalize() {
	if cfg.SchemaVersion == "" {
		cfg.SchemaVersion = configurationSchemaVersion
	}
	if cfg.SchemaVersion == "0.1.0" {
		cfg.SchemaVersion = configurationSchemaVersion
	}
	if cfg.Plugins.MemoryLimitMiB == 0 {
		cfg.Plugins.MemoryLimitMiB = cfg.Plugins.LegacyMemoryLimitMB
	}
	if cfg.Model.MaxRSSMiB == 0 {
		cfg.Model.MaxRSSMiB = cfg.Model.LegacyPeakMemoryMiB
	}
	if cfg.Exports.JSONL != nil {
		cfg.Exports.JSONLGraph = *cfg.Exports.JSONL
	}
	if cfg.Exports.SARIF != nil {
		cfg.Exports.Integrations = *cfg.Exports.SARIF
	}
	if cfg.QualityGates.InventoryAccounting != nil {
		cfg.QualityGates.MinInventoryAccounting = *cfg.QualityGates.InventoryAccounting
	}
	if cfg.QualityGates.DanglingEdges != nil && *cfg.QualityGates.DanglingEdges == 0 {
		cfg.QualityGates.MaxUnresolvedEdges = 0
	}
	if cfg.Model.Provider == "llama.cpp" && cfg.Model.ModelPath == "" {
		cfg.Model.ModelPath = cfg.Model.ModelName
	}
	cfg.Inventory.Exclude = uniqueSortedStrings(cfg.Inventory.Exclude)
	cfg.Analysis.Tiers = uniqueSortedStrings(cfg.Analysis.Tiers)
	cfg.Plugins.Directories = uniqueSortedStrings(cfg.Plugins.Directories)
}

func (cfg Configuration) Validate() error {
	var failures []string
	if cfg.SchemaVersion != configurationSchemaVersion {
		failures = append(failures, "schema_version must be "+configurationSchemaVersion)
	}
	if cfg.Inventory.MaxFileBytes < 0 || cfg.Inventory.MaxTextBytes < 0 || cfg.Inventory.MaxRepositoryBytes < 0 || cfg.Inventory.MaxFiles < 0 {
		failures = append(failures, "inventory limits must be non-negative")
	}
	if cfg.Inventory.MaxFileBytes > 0 && cfg.Inventory.MaxTextBytes > cfg.Inventory.MaxFileBytes {
		failures = append(failures, "max_text_bytes cannot exceed max_file_bytes")
	}
	if cfg.Plugins.TimeoutSeconds <= 0 {
		failures = append(failures, "plugins.timeout_seconds must be positive")
	}
	if cfg.Plugins.MaximumOutputBytes < 1024 {
		failures = append(failures, "plugins.maximum_output_bytes must be at least 1024")
	}
	if cfg.Plugins.MemoryLimitMiB < 16 {
		failures = append(failures, "plugins.memory_limit_mib must be at least 16")
	} else if cfg.Plugins.MemoryLimitMiB > rkcplugin.MaximumMemoryMiB {
		failures = append(failures, "plugins.memory_limit_mib exceeds the enforced safety ceiling")
	}
	if cfg.Plugins.MemorySwapLimitMiB < 0 || cfg.Plugins.MemorySwapLimitMiB > rkcplugin.MaximumSwapMiB || cfg.Plugins.MemorySwapLimitMiB > cfg.Plugins.MemoryLimitMiB {
		failures = append(failures, "plugins.memory_swap_limit_mib is outside the enforced safety policy")
	}
	if cfg.Plugins.ProcessLimit != rkcplugin.MaximumProcesses {
		failures = append(failures, "plugins.process_limit must be 1 while process spawning is disabled")
	}
	if cfg.Plugins.PythonAST.Interpreter == "" {
		failures = append(failures, "plugins.python_ast.interpreter is required")
	}
	switch cfg.Workspace.PrivacyMode {
	case "full", "paths-relative", "redacted":
	default:
		failures = append(failures, "workspace.privacy_mode is invalid")
	}
	switch cfg.Plugins.NativeWorkerSandbox {
	case "required", "preferred", "disabled":
	default:
		failures = append(failures, "plugins.native_worker_sandbox is invalid")
	}
	if cfg.Plugins.Enabled && cfg.Plugins.NativeWorkerSandbox != "required" {
		failures = append(failures, "plugins.native_worker_sandbox must be required while plugins are enabled")
	}
	if cfg.Plugins.AllowNetwork {
		failures = append(failures, "plugins.allow_network is unsupported; plugin network access must remain disabled")
	}
	if cfg.Plugins.AllowProcessSpawn {
		failures = append(failures, "plugins.allow_process_spawn is unsupported; plugin process spawning must remain disabled")
	}
	if script := strings.TrimSpace(cfg.Plugins.PythonAST.Script); script != "" && script != "builtin" {
		failures = append(failures, "plugins.python_ast.script must be builtin; external Python plugins are disabled")
	}
	switch cfg.Model.Provider {
	case "disabled", "llama.cpp", "":
	default:
		failures = append(failures, "model.provider must be disabled or llama.cpp")
	}
	if cfg.Model.ContextTokens < 512 {
		failures = append(failures, "model.context_tokens must be at least 512")
	} else if cfg.Model.ContextTokens > 262144 {
		failures = append(failures, "model.context_tokens must not exceed 262144")
	}
	if cfg.Model.MaxOutputTokens <= 0 {
		failures = append(failures, "model.max_output_tokens must be positive")
	} else if cfg.Model.MaxOutputTokens > cfg.Model.ContextTokens {
		failures = append(failures, "model.max_output_tokens cannot exceed model.context_tokens")
	}
	if cfg.Model.MaxRSSMiB < 256 {
		failures = append(failures, "model.max_rss_mib must be at least 256")
	} else if cfg.Model.MaxRSSMiB > modelMaximumRSSMiB {
		failures = append(failures, "model.max_rss_mib must not exceed the 2560 MiB safety ceiling")
	}
	if cfg.Search.Embeddings {
		failures = append(failures, "search.embeddings is not implemented in this reference build")
	}
	for name, value := range map[string]float64{"min_inventory_accounting": cfg.QualityGates.MinInventoryAccounting, "min_symbol_evidence": cfg.QualityGates.MinSymbolEvidence, "min_edge_resolution": cfg.QualityGates.MinEdgeResolution, "min_claim_citation": cfg.QualityGates.MinClaimCitation} {
		if value < 0 || value > 1 {
			failures = append(failures, name+" must be between 0 and 1")
		}
	}
	if cfg.Exports.NotebookPackBytes < 65536 {
		failures = append(failures, "exports.notebook_pack_bytes must be at least 65536")
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (cfg Configuration) PluginTimeout() time.Duration {
	seconds := cfg.Plugins.TimeoutSeconds
	if cfg.Plugins.PythonAST.TimeoutSeconds > 0 {
		seconds = cfg.Plugins.PythonAST.TimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (cfg Configuration) Digest() string {
	normalized := cfg
	normalized.normalize()
	normalized.Workspace.Output = ""
	normalized.Exports.SnapshotStore = ""
	// Model execution is a derived, post-scan operation. Local model paths and
	// inference settings must not change the canonical repository snapshot.
	normalized.Model = ModelConfig{}
	normalized.Documentation.ModelSynthesis = false
	return digestJSON(normalized)
}
func (cfg Configuration) PolicyDigest() string {
	return digestJSON(struct {
		Inventory InventoryConfig    `json:"inventory"`
		Security  SecurityConfig     `json:"security"`
		Quality   QualityGatesConfig `json:"quality_gates"`
	}{cfg.Inventory, cfg.Security, cfg.QualityGates})
}
func (cfg Configuration) PluginDigest() string {
	return digestJSON(struct {
		Plugins    PluginsConfig    `json:"plugins"`
		Frameworks FrameworksConfig `json:"frameworks"`
	}{cfg.Plugins, cfg.Frameworks})
}

func digestJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func uniqueSortedStrings(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
