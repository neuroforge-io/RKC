package modelassets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveGenerationBindsQualifiedModelAndRuntimeReceipt(t *testing.T) {
	fixture := newResolverFixture(t)
	binding, err := ResolveGeneration(fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if binding.AssetID != "generation" || binding.ModelSHA256 != fixture.modelSHA || binding.RuntimeSHA256 != fixture.executableSHA ||
		binding.RuntimeRevision != strings.Repeat("c", 40) || binding.ModelRevision != strings.Repeat("a", 40) ||
		binding.ModelLicense != "Apache-2.0" || binding.RuntimeLicense != "MIT" || binding.QuantizationBits != 4 ||
		binding.NativeContextTokens != 32768 || binding.LockSHA256 == "" || binding.RuntimeReceiptSHA256 == "" {
		t.Fatalf("unexpected generation binding: %+v", binding)
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fixture.root) || strings.Contains(string(encoded), "executable_path") {
		t.Fatalf("binding JSON exposed local paths: %s", encoded)
	}
}

func TestResolveGenerationRejectsUnqualifiedOrMismatchedArtifacts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*resolverFixture)
		want   string
	}{
		{"no default", func(f *resolverFixture) { f.lock.DefaultGenerationModel = nil }, "no qualified default"},
		{"unqualified", func(f *resolverFixture) { f.lock.Assets[1].Status = "unqualified" }, "not qualified and eligible"},
		{"ineligible", func(f *resolverFixture) { f.lock.Assets[1].DefaultEligible = false }, "not qualified and eligible"},
		{"wrong license", func(f *resolverFixture) { f.lock.Assets[1].LicenseSPDX = "MIT" }, "Apache-2.0"},
		{"wrong model size", func(f *resolverFixture) { f.lock.Assets[1].SizeBytes++ }, "model size"},
		{"wrong receipt lock", func(f *resolverFixture) { f.receipt.LockSHA256 = strings.Repeat("0", 64) }, "does not match"},
		{"missing llama cli", func(f *resolverFixture) { f.receipt.Binaries = f.receipt.Binaries[:1] }, "binary inventory"},
		{"wrong executable", func(f *resolverFixture) {
			other := filepath.Join(f.root, "other-llama-cli")
			f.write(other, []byte("#!/bin/sh\nexit 0\n"), 0o700)
			f.executable = other
		}, "not the receipt-bound executable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolverFixture(t)
			test.mutate(fixture)
			fixture.publishDocuments()
			_, err := ResolveGeneration(fixture.request())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveGeneration error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestStrictDocumentsRejectDuplicateUnknownAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
		`{"a":1} {"b":2}`,
		`[1,2`,
	} {
		if err := rejectDuplicateJSONKeys([]byte(input)); err == nil {
			t.Fatalf("invalid/ambiguous JSON was accepted: %s", input)
		}
	}
	var destination struct {
		A int `json:"a"`
	}
	if err := decodeStrictDocument([]byte(`{"a":1,"unknown":2}`), &destination); err == nil {
		t.Fatal("unknown document field was accepted")
	}
	if err := decodeStrictDocument([]byte(`{"a":1}`), &destination); err != nil || destination.A != 1 {
		t.Fatalf("strict valid document failed: %+v, %v", destination, err)
	}
}

type resolverFixture struct {
	t             *testing.T
	root          string
	lockPath      string
	receiptPath   string
	model         string
	executable    string
	modelSHA      string
	executableSHA string
	lock          lockDocument
	receipt       runtimeReceipt
}

func newResolverFixture(t *testing.T) *resolverFixture {
	t.Helper()
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "build", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &resolverFixture{
		t: t, root: root, lockPath: filepath.Join(root, "models.lock.json"),
		receiptPath: filepath.Join(runtimeRoot, runtimeReceiptName),
		model:       filepath.Join(root, "fixture.gguf"),
		executable:  filepath.Join(runtimeRoot, "build", "bin", "llama-cli"),
	}
	fixture.write(fixture.model, []byte("GGUFtest"), 0o600)
	fixture.write(fixture.executable, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	fixture.modelSHA = fileDigest(t, fixture.model)
	fixture.executableSHA = fileDigest(t, fixture.executable)
	quantization := "Q4_K_M"
	contextTokens := 32768
	qualification := "models/qualification/fixture.json"
	fixture.lock = lockDocument{
		Schema: "../schemas/model-lock.schema.json", SchemaVersion: lockSchemaVersion,
		DefaultGenerationModel: stringPointer("generation"), DefaultEmbeddingModel: nil,
		LlamaCPP: llamaLock{
			Repository: "https://github.com/ggml-org/llama.cpp", Tag: "b1", Commit: strings.Repeat("c", 40), LicenseSPDX: "MIT",
			LicenseURL: "https://github.com/ggml-org/llama.cpp/blob/commit/LICENSE", SourceAssetID: "source", CMake: json.RawMessage(`{}`),
		},
		Assets: []lockAsset{
			{ID: "source", Kind: "source-archive", Status: "runtime-pinned", Repository: "https://github.com/ggml-org/llama.cpp", Revision: strings.Repeat("c", 40), Filename: "source.tar.gz", URL: "https://example.com/source.tar.gz", AllowedHosts: []string{"example.com"}, SHA256: strings.Repeat("b", 64), SizeBytes: 123, LicenseSPDX: "MIT", LicenseURL: "https://example.com/license", Redistribution: "not-bundled-download-on-demand", ExtractionRoot: stringPointer("source")},
			{ID: "generation", Kind: "generation-model", Status: "qualified", DefaultEligible: true, Repository: "https://example.com/model", Revision: strings.Repeat("a", 40), Filename: "fixture.gguf", URL: "https://example.com/fixture.gguf", AllowedHosts: []string{"example.com"}, SHA256: fixture.modelSHA, SizeBytes: 8, LicenseSPDX: "Apache-2.0", LicenseURL: "https://example.com/license", Redistribution: "not-bundled-download-on-demand", Quantization: &quantization, NativeContextTokens: &contextTokens, QualificationSpec: &qualification},
			{ID: "embedding", Kind: "embedding-model", Status: "unqualified", Repository: "https://example.com/embedding", Revision: strings.Repeat("d", 40), Filename: "embedding.gguf", URL: "https://example.com/embedding.gguf", AllowedHosts: []string{"example.com"}, SHA256: strings.Repeat("e", 64), SizeBytes: 8, LicenseSPDX: "Apache-2.0", LicenseURL: "https://example.com/license", Redistribution: "not-bundled-download-on-demand", Quantization: stringPointer("Q8_0"), NativeContextTokens: intPointer(8192), QualificationSpec: stringPointer("models/qualification/fixture.json")},
		},
	}
	fixture.publishDocuments()
	return fixture
}

func (fixture *resolverFixture) publishDocuments() {
	fixture.t.Helper()
	lockBytes, err := json.Marshal(fixture.lock)
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.write(fixture.lockPath, lockBytes, 0o600)
	lockDigest := sha256.Sum256(lockBytes)
	if fixture.receipt.SchemaVersion == "" {
		fixture.receipt = runtimeReceipt{
			SchemaVersion: runtimeSchemaVersion, Runtime: "llama.cpp", Tag: fixture.lock.LlamaCPP.Tag, Commit: fixture.lock.LlamaCPP.Commit,
			SourceSHA256: fixture.lock.Assets[0].SHA256, SourceSizeBytes: fixture.lock.Assets[0].SizeBytes, LockSHA256: hex.EncodeToString(lockDigest[:]),
			Profile: "native", CMake: "cmake version 3.30", ConfigureArgv: []string{"cmake", "-S", "source"}, BuildArgv: []string{"cmake", "--build", "build"},
			Platform: "test", Machine: "test", Python: "3", QualificationStatus: "not-run", DefaultModelStatus: "none",
			Binaries: []runtimeBinary{
				{Path: "build/bin/llama-bench", SHA256: strings.Repeat("1", 64), SizeBytes: 1},
				{Path: "build/bin/llama-cli", SHA256: fixture.executableSHA, SizeBytes: int64(len("#!/bin/sh\nexit 0\n"))},
				{Path: "build/bin/llama-embedding", SHA256: strings.Repeat("2", 64), SizeBytes: 1},
				{Path: "build/bin/llama-server", SHA256: strings.Repeat("3", 64), SizeBytes: 1},
			},
		}
	}
	receiptBytes, err := json.Marshal(fixture.receipt)
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.write(fixture.receiptPath, receiptBytes, 0o600)
}

func (fixture *resolverFixture) request() GenerationRequest {
	return GenerationRequest{LockPath: fixture.lockPath, RuntimeReceiptPath: fixture.receiptPath, ExecutablePath: fixture.executable, ModelPath: fixture.model}
}

func (fixture *resolverFixture) write(path string, data []byte, mode os.FileMode) {
	fixture.t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		fixture.t.Fatal(err)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func stringPointer(value string) *string { return &value }
func intPointer(value int) *int          { return &value }
