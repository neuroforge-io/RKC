package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigurationIsValidAndDeterministic(t *testing.T) {
	const (
		publishedSchemaURI    = "https://raw.githubusercontent.com/neuroforge-io/RKC/d5daad7bb8c39438d28fe40f761e7c6938cc2e79/schemas/config.schema.json"
		publishedSchemaSHA256 = "4dbd0f6f7748e9d32b258cc167f9e3cc154d79713f274cbcb4291b536247df6b"
	)
	if configurationSchemaURI != publishedSchemaURI {
		t.Fatalf("configuration schema URI = %q, want published main schema %q", configurationSchemaURI, publishedSchemaURI)
	}
	schemaBytes, err := os.ReadFile(filepath.Join("..", "..", "schemas", "config.schema.json"))
	if err != nil {
		t.Fatalf("read maintained configuration schema: %v", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(schemaBytes)); got != publishedSchemaSHA256 {
		t.Fatalf("maintained configuration schema SHA-256 = %s, want published bytes %s", got, publishedSchemaSHA256)
	}
	var schemaContract struct {
		Properties struct {
			Model struct {
				Properties struct {
					MaximumRSS struct {
						Maximum int64 `json:"maximum"`
					} `json:"max_rss_mib"`
				} `json:"properties"`
			} `json:"model"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaBytes, &schemaContract); err != nil {
		t.Fatalf("decode maintained configuration schema: %v", err)
	}
	if maximum := schemaContract.Properties.Model.Properties.MaximumRSS.Maximum; maximum != modelMaximumRSSMiB {
		t.Fatalf("published schema model.max_rss_mib maximum = %d, want runtime ceiling %d", maximum, modelMaximumRSSMiB)
	}
	cfg := defaultConfiguration()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default configuration is invalid: %v", err)
	}
	if cfg.SchemaURI != configurationSchemaURI {
		t.Fatalf("default schema URI = %q, want canonical %q", cfg.SchemaURI, configurationSchemaURI)
	}
	if modelMaximumRSSMiB != 4608 {
		t.Fatalf("modelMaximumRSSMiB = %d, want 4608", modelMaximumRSSMiB)
	}
	if cfg.Model.MaxRSSMiB != modelMaximumRSSMiB {
		t.Fatalf("default model RSS ceiling = %d MiB, want 4608 MiB", cfg.Model.MaxRSSMiB)
	}
	if cfg.Digest() == "" || cfg.PolicyDigest() == "" || cfg.PluginDigest() == "" {
		t.Fatal("configuration digests must be populated")
	}

	clone := cfg
	clone.Workspace.Output = "/tmp/another-output"
	clone.Model.Provider = "llama.cpp"
	clone.Model.ModelPath = "/models/another-model.gguf"
	clone.Model.ContextTokens = 32768
	clone.Documentation.ModelSynthesis = true
	clone.Exports.SnapshotStore = "/tmp/another-state"
	clone.Analysis.Incremental = !cfg.Analysis.Incremental
	if got, want := clone.Digest(), cfg.Digest(); got != want {
		t.Fatalf("operational paths changed portable digest: got %s want %s", got, want)
	}
	clone.Security.DetectSecrets = false
	if clone.PolicyDigest() == cfg.PolicyDigest() {
		t.Fatal("security policy change did not change policy digest")
	}
}

func TestDefaultInventoryExclusionsAreExplicitAndIsolated(t *testing.T) {
	want := strings.Join([]string{
		".cache", ".coverage", ".git", ".mypy_cache", ".pytest_cache",
		".rkc", ".rkc-coverage", ".rkc-downloads", ".rkc-history.json",
		".rkc-models", ".rkc-runtime", ".rkc-scip",
		".rkc-state", ".rkc-trace.json", ".rkc.rkc-derived", ".ruff_cache", ".venv",
		"__pycache__", "bin", "coverage", "coverage.out", "coverage.xml",
		"dist", "htmlcov", "venv",
	}, ",")
	first := defaultConfiguration()
	if got := strings.Join(first.Inventory.Exclude, ","); got != want {
		t.Fatalf("default inventory exclusions = %q, want %q", got, want)
	}
	for _, path := range first.Inventory.Exclude {
		if strings.ContainsAny(path, "*?[") {
			t.Fatalf("default exclusion %q falsely implies glob semantics", path)
		}
	}
	first.Inventory.Exclude[0] = "mutated"
	second := defaultConfiguration()
	if second.Inventory.Exclude[0] != ".cache" {
		t.Fatal("default inventory exclusion slices share mutable backing storage")
	}
}

func TestConfigurationNormalizeLegacyAndCollections(t *testing.T) {
	legacyFalse := false
	legacyTrue := true
	zero := 0
	accounting := 0.75
	cfg := defaultConfiguration()
	cfg.SchemaVersion = "0.1.0"
	cfg.Plugins.MemoryLimitMiB = 0
	cfg.Plugins.LegacyMemoryLimitMB = 512
	cfg.Model.MaxRSSMiB = 0
	cfg.Model.LegacyPeakMemoryMiB = 1024
	cfg.Model.Provider = "llama.cpp"
	cfg.Model.ModelName = "model.gguf"
	cfg.Exports.JSONL = &legacyFalse
	cfg.Exports.SARIF = &legacyTrue
	cfg.QualityGates.InventoryAccounting = &accounting
	cfg.QualityGates.DanglingEdges = &zero
	cfg.Inventory.Exclude = []string{" z ", "a", "a", ""}
	cfg.Analysis.Tiers = []string{"syntax", " inventory ", "syntax"}
	cfg.Plugins.Directories = []string{"b", "a", "b"}

	cfg.normalize()
	if cfg.SchemaVersion != configurationSchemaVersion || cfg.Plugins.MemoryLimitMiB != 512 || cfg.Model.MaxRSSMiB != 1024 {
		t.Fatalf("legacy values not normalized: %+v", cfg)
	}
	if cfg.Model.ModelPath != "model.gguf" || cfg.Exports.JSONLGraph || !cfg.Exports.Integrations {
		t.Fatalf("legacy model/export values not normalized: %+v", cfg)
	}
	if cfg.QualityGates.MinInventoryAccounting != accounting || cfg.QualityGates.MaxUnresolvedEdges != 0 {
		t.Fatalf("legacy quality values not normalized: %+v", cfg.QualityGates)
	}
	if got := strings.Join(cfg.Inventory.Exclude, ","); got != "a,z" {
		t.Fatalf("exclude normalization = %q", got)
	}
	if got := strings.Join(cfg.Analysis.Tiers, ","); got != "inventory,syntax" {
		t.Fatalf("tier normalization = %q", got)
	}
	if got := strings.Join(cfg.Plugins.Directories, ","); got != "a,b" {
		t.Fatalf("directory normalization = %q", got)
	}
}

func TestLoadConfigurationStrictJSONAndDefaults(t *testing.T) {
	if cfg, err := loadConfiguration(""); err != nil || cfg.SchemaVersion != configurationSchemaVersion {
		t.Fatalf("load defaults: cfg=%+v err=%v", cfg, err)
	}

	root := t.TempDir()
	valid := filepath.Join(root, "valid.json")
	cfg := defaultConfiguration()
	cfg.Workspace.Name = "fixture"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfiguration(valid)
	if err != nil || loaded.Workspace.Name != "fixture" {
		t.Fatalf("load valid: cfg=%+v err=%v", loaded, err)
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown", `{"unknown":true}`, "unknown field"},
		{"removed-git-ignore-toggle", `{"inventory":{"include_git_ignored":false}}`, "unknown field"},
		{"trailing", string(data) + ` {}`, "multiple JSON values"},
		{"malformed-trailing", string(data) + ` [`, "unexpected EOF"},
		{"malformed", `{`, "decode configuration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name+".json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfiguration(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestConfigurationValidationRejectsEveryInvalidClass(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Configuration)
		want string
	}{
		{"schema", func(c *Configuration) { c.SchemaVersion = "9" }, "schema_version"},
		{"negative-inventory", func(c *Configuration) { c.Inventory.MaxFiles = -1 }, "inventory limits"},
		{"text-over-file", func(c *Configuration) { c.Inventory.MaxFileBytes = 10; c.Inventory.MaxTextBytes = 11 }, "max_text_bytes"},
		{"plugin-timeout", func(c *Configuration) { c.Plugins.TimeoutSeconds = 0 }, "timeout_seconds"},
		{"plugin-output", func(c *Configuration) { c.Plugins.MaximumOutputBytes = 10 }, "maximum_output_bytes"},
		{"plugin-memory", func(c *Configuration) { c.Plugins.MemoryLimitMiB = 1 }, "memory_limit_mib"},
		{"plugin-memory-ceiling", func(c *Configuration) { c.Plugins.MemoryLimitMiB = 2049 }, "safety ceiling"},
		{"plugin-swap", func(c *Configuration) { c.Plugins.MemorySwapLimitMiB = 513 }, "memory_swap_limit_mib"},
		{"plugin-processes", func(c *Configuration) { c.Plugins.ProcessLimit = 0 }, "process_limit"},
		{"plugin-network", func(c *Configuration) { c.Plugins.AllowNetwork = true }, "allow_network"},
		{"plugin-spawn", func(c *Configuration) { c.Plugins.AllowProcessSpawn = true }, "allow_process_spawn"},
		{"plugin-external", func(c *Configuration) { c.Plugins.PythonAST.Script = "worker.py" }, "must be builtin"},
		{"python", func(c *Configuration) { c.Plugins.PythonAST.Interpreter = "" }, "interpreter"},
		{"privacy", func(c *Configuration) { c.Workspace.PrivacyMode = "public" }, "privacy_mode"},
		{"sandbox", func(c *Configuration) { c.Plugins.NativeWorkerSandbox = "sometimes" }, "native_worker_sandbox"},
		{"sandbox-preferred", func(c *Configuration) { c.Plugins.NativeWorkerSandbox = "preferred" }, "must be required"},
		{"provider", func(c *Configuration) { c.Model.Provider = "cloud-magic" }, "model.provider"},
		{"context", func(c *Configuration) { c.Model.ContextTokens = 511 }, "context_tokens"},
		{"context-ceiling", func(c *Configuration) { c.Model.ContextTokens = 262145 }, "context_tokens"},
		{"output", func(c *Configuration) { c.Model.MaxOutputTokens = 0 }, "max_output_tokens"},
		{"rss", func(c *Configuration) { c.Model.MaxRSSMiB = 255 }, "max_rss_mib"},
		{"rss-ceiling", func(c *Configuration) { c.Model.MaxRSSMiB = modelMaximumRSSMiB + 1 }, "max_rss_mib"},
		{"embeddings", func(c *Configuration) { c.Search.Embeddings = true }, "search.embeddings"},
		{"ratio-low", func(c *Configuration) { c.QualityGates.MinClaimCitation = -0.1 }, "min_claim_citation"},
		{"ratio-high", func(c *Configuration) { c.QualityGates.MinEdgeResolution = 1.1 }, "min_edge_resolution"},
		{"notebook", func(c *Configuration) { c.Exports.NotebookPackBytes = 65535 }, "notebook_pack_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultConfiguration()
			test.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestConfigurationHelpers(t *testing.T) {
	cfg := defaultConfiguration()
	cfg.SchemaVersion = ""
	cfg.normalize()
	if cfg.SchemaVersion != configurationSchemaVersion {
		t.Fatalf("normalized schema version = %q", cfg.SchemaVersion)
	}
	cfg.Plugins.TimeoutSeconds = 3
	cfg.Plugins.PythonAST.TimeoutSeconds = 7
	if got := cfg.PluginTimeout().Seconds(); got != 7 {
		t.Fatalf("plugin timeout = %v", got)
	}
	cfg.Plugins.PythonAST.TimeoutSeconds = 0
	if got := cfg.PluginTimeout().Seconds(); got != 3 {
		t.Fatalf("fallback plugin timeout = %v", got)
	}
	if got := uniqueSortedStrings([]string{" b ", "a", "b", ""}); strings.Join(got, ",") != "a,b" {
		t.Fatalf("uniqueSortedStrings = %v", got)
	}

	unsafe := defaultConfiguration()
	unsafe.Model.MaxRSSMiB = modelMaximumRSSMiB + 1
	data, err := json.Marshal(unsafe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unsafe.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(path); err == nil || !strings.Contains(err.Error(), "max_rss_mib") {
		t.Fatalf("unsafe loaded configuration = %v", err)
	}
}
