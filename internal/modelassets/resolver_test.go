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

func TestResolveEmbeddingBindsQualifiedDefaultAndEmbeddingExecutable(t *testing.T) {
	fixture := newResolverFixture(t)
	embeddingModel := filepath.Join(fixture.root, "embedding.gguf")
	fixture.write(embeddingModel, []byte("GGUFembed"), 0o600)
	asset := &fixture.lock.Assets[2]
	asset.Status = "qualified"
	asset.DefaultEligible = true
	asset.Filename = filepath.Base(embeddingModel)
	asset.SHA256 = fileDigest(t, embeddingModel)
	asset.SizeBytes = int64(len("GGUFembed"))
	fixture.lock.DefaultEmbeddingModel = stringPointer(asset.ID)
	fixture.receipt = runtimeReceipt{}
	fixture.publishDocuments()
	binding, err := ResolveEmbedding(EmbeddingRequest{
		LockPath: fixture.lockPath, RuntimeReceiptPath: fixture.receiptPath,
		ExecutablePath: fixture.embeddingExecutable, ModelPath: embeddingModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.AssetID != asset.ID || binding.RuntimeSHA256 != fixture.embeddingExecutableSHA || binding.ModelSHA256 != asset.SHA256 {
		t.Fatalf("unexpected embedding binding: %+v", binding)
	}
}

func TestResolveGenerationUsesHermeticExecutableAndReceiptDefaults(t *testing.T) {
	fixture := newResolverFixture(t)
	t.Setenv("PATH", filepath.Dir(fixture.executable))
	request := fixture.request()
	request.ExecutablePath = ""
	request.RuntimeReceiptPath = ""
	binding, err := ResolveGeneration(request)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ExecutablePath != fixture.executable || binding.RuntimeReceiptPath != fixture.receiptPath {
		t.Fatalf("default paths = executable %q, receipt %q", binding.ExecutablePath, binding.RuntimeReceiptPath)
	}
}

func TestResolveGenerationRejectsMalformedRequestsAndBindings(t *testing.T) {
	if _, err := ResolveGeneration(GenerationRequest{}); err == nil || !strings.Contains(err.Error(), "lock path") {
		t.Fatalf("missing lock error = %v", err)
	}
	if _, err := ResolveGeneration(GenerationRequest{LockPath: "missing"}); err == nil || !strings.Contains(err.Error(), "model path") {
		t.Fatalf("missing model error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*resolverFixture, *GenerationRequest)
		want   string
	}{
		{"asset whitespace", func(_ *resolverFixture, request *GenerationRequest) { request.AssetID = " generation " }, "surrounding whitespace"},
		{"absent asset", func(_ *resolverFixture, request *GenerationRequest) { request.AssetID = "absent" }, "is absent"},
		{"source revision", func(f *resolverFixture, _ *GenerationRequest) { f.lock.Assets[0].Revision = strings.Repeat("d", 40) }, "source archive"},
		{"source provenance", func(f *resolverFixture, _ *GenerationRequest) { f.lock.Assets[0].SHA256 = "bad" }, "source asset provenance"},
		{"model basename", func(f *resolverFixture, request *GenerationRequest) {
			other := filepath.Join(f.root, "other.gguf")
			f.write(other, []byte("GGUFtest"), 0o600)
			request.ModelPath = other
		}, "basename"},
		{"receipt directory permissions", func(f *resolverFixture, _ *GenerationRequest) {
			if err := os.Chmod(filepath.Dir(f.receiptPath), 0o777); err != nil {
				f.t.Fatal(err)
			}
		}, "private real directory"},
		{"executable receipt size", func(f *resolverFixture, _ *GenerationRequest) { f.receipt.Binaries[1].SizeBytes++ }, "llama-cli size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolverFixture(t)
			request := fixture.request()
			test.mutate(fixture, &request)
			fixture.publishDocuments()
			_, err := ResolveGeneration(request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveGeneration error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolverPolicyValidationRejectsInvalidShapes(t *testing.T) {
	fixture := newResolverFixture(t)
	asset := fixture.lock.Assets[1]
	assetCases := []struct {
		name   string
		mutate func(*lockAsset)
	}{
		{"digest", func(asset *lockAsset) { asset.SHA256 = "bad" }},
		{"filename", func(asset *lockAsset) { asset.Filename = "../fixture.gguf" }},
		{"provenance", func(asset *lockAsset) { asset.Repository = "http://example.com/model" }},
		{"metadata", func(asset *lockAsset) { asset.Quantization = nil }},
		{"qualification", func(asset *lockAsset) { asset.QualificationSpec = stringPointer("../fixture.json") }},
		{"model-only", func(asset *lockAsset) { asset.ExtractionRoot = stringPointer("source") }},
	}
	for _, test := range assetCases {
		t.Run("asset "+test.name, func(t *testing.T) {
			candidate := asset
			test.mutate(&candidate)
			if err := validateGenerationAsset(candidate); err == nil {
				t.Fatalf("invalid generation asset was accepted: %+v", candidate)
			}
		})
	}

	receiptCases := []struct {
		name   string
		mutate func(*runtimeReceipt)
	}{
		{"profile", func(receipt *runtimeReceipt) { receipt.Profile = "gpu" }},
		{"build policy", func(receipt *runtimeReceipt) { receipt.CMake = "" }},
		{"binary record", func(receipt *runtimeReceipt) { receipt.Binaries[0].Path = "../llama-bench" }},
		{"duplicate binary", func(receipt *runtimeReceipt) { receipt.Binaries[1].Path = receipt.Binaries[0].Path }},
	}
	lockBytes, err := os.ReadFile(fixture.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lockDigest := sha256.Sum256(lockBytes)
	for _, test := range receiptCases {
		t.Run("receipt "+test.name, func(t *testing.T) {
			receipt := fixture.receipt
			receipt.Binaries = append([]runtimeBinary(nil), fixture.receipt.Binaries...)
			test.mutate(&receipt)
			data, marshalErr := json.Marshal(receipt)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if err := validateReceiptShape(data, receipt, fixture.lock, hex.EncodeToString(lockDigest[:]), fixture.lock.Assets[0]); err == nil {
				t.Fatalf("invalid runtime receipt was accepted: %+v", receipt)
			}
		})
	}

	if _, _, err := canonicalRegularPath("", false); err == nil {
		t.Fatal("empty artifact path was accepted")
	}
	plain := filepath.Join(fixture.root, "plain")
	fixture.write(plain, []byte("plain"), 0o600)
	if _, _, err := canonicalRegularPath(plain, true); err == nil {
		t.Fatal("non-executable artifact was accepted")
	}
	if _, _, _, _, err := readBoundedRegular(plain, 0); err == nil {
		t.Fatal("non-positive read limit was accepted")
	}
	if _, _, _, _, err := readBoundedRegular(" ", 1); err == nil {
		t.Fatal("blank document path was accepted")
	}
	if _, _, _, _, err := readBoundedRegular(plain, 1); err == nil {
		t.Fatal("oversized document was accepted")
	}
	if quantizationBits("not-quantized") != 0 || quantizationBits("Qx") != 0 {
		t.Fatal("invalid quantization was assigned a bit width")
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
	t                      *testing.T
	root                   string
	lockPath               string
	receiptPath            string
	model                  string
	executable             string
	embeddingExecutable    string
	modelSHA               string
	executableSHA          string
	embeddingExecutableSHA string
	lock                   lockDocument
	receipt                runtimeReceipt
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
		receiptPath:         filepath.Join(runtimeRoot, runtimeReceiptName),
		model:               filepath.Join(root, "fixture.gguf"),
		executable:          filepath.Join(runtimeRoot, "build", "bin", "llama-cli"),
		embeddingExecutable: filepath.Join(runtimeRoot, "build", "bin", "llama-embedding"),
	}
	fixture.write(fixture.model, []byte("GGUFtest"), 0o600)
	fixture.write(fixture.executable, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	fixture.write(fixture.embeddingExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	fixture.modelSHA = fileDigest(t, fixture.model)
	fixture.executableSHA = fileDigest(t, fixture.executable)
	fixture.embeddingExecutableSHA = fileDigest(t, fixture.embeddingExecutable)
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
				{Path: "build/bin/llama-embedding", SHA256: fixture.embeddingExecutableSHA, SizeBytes: int64(len("#!/bin/sh\nexit 0\n"))},
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
