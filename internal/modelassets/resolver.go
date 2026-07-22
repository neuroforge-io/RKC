// Package modelassets resolves production model/runtime bindings from RKC's
// checked-in supply-chain lock and a locally generated llama.cpp build receipt.
package modelassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	lockSchemaVersion    = "1.0.0"
	runtimeSchemaVersion = "1.1.0"
	maximumLockBytes     = int64(4 * 1024 * 1024)
	maximumReceiptBytes  = int64(1024 * 1024)
	maximumLicenseBytes  = int64(1024 * 1024)
	runtimeReceiptName   = "rkc-llama-runtime.json"
	runtimeLicensePath   = "source/LICENSE"
)

var (
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,95}$`)
	filenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)
	tagPattern      = regexp.MustCompile(`^b[0-9]+$`)
)

// ModelRequest identifies local artifacts without trusting caller-provided
// digests or provenance strings. AssetID may be empty only when the lock names a
// qualified default for the requested model kind. RuntimeReceiptPath may be
// empty when the executable has the standard <runtime>/build/bin layout.
type ModelRequest struct {
	LockPath           string
	RuntimeReceiptPath string
	ExecutablePath     string
	ModelPath          string
	AssetID            string
}

// GenerationRequest is the generation-model request contract.
type GenerationRequest = ModelRequest

// EmbeddingRequest is the embedding-model request contract.
type EmbeddingRequest = ModelRequest

// ModelBinding contains only provenance derived from verified lock and
// receipt documents. The provider independently hashes the executable/model
// bytes against these expectations before and after every inference process.
type ModelBinding struct {
	AssetID              string `json:"asset_id"`
	LockSHA256           string `json:"lock_sha256"`
	RuntimeReceiptSHA256 string `json:"runtime_receipt_sha256"`
	RuntimeProfile       string `json:"runtime_profile"`
	RuntimeTag           string `json:"runtime_tag"`
	RuntimeRevision      string `json:"runtime_revision"`
	RuntimeLicense       string `json:"runtime_license"`
	RuntimeSHA256        string `json:"runtime_sha256"`
	ModelRevision        string `json:"model_revision"`
	ModelLicense         string `json:"model_license"`
	ModelSHA256          string `json:"model_sha256"`
	ModelSizeBytes       int64  `json:"model_size_bytes"`
	Quantization         string `json:"quantization"`
	QuantizationBits     int    `json:"quantization_bits"`
	NativeContextTokens  int    `json:"native_context_tokens"`
	QualificationSpec    string `json:"qualification_spec"`
	ExecutablePath       string `json:"-"`
	ModelPath            string `json:"-"`
	RuntimeReceiptPath   string `json:"-"`
	LockPath             string `json:"-"`
}

// GenerationBinding is a verified generation model/runtime binding.
type GenerationBinding = ModelBinding

// EmbeddingBinding is a verified embedding model/runtime binding.
type EmbeddingBinding = ModelBinding

type lockDocument struct {
	Schema                 string      `json:"$schema"`
	SchemaVersion          string      `json:"schema_version"`
	DefaultGenerationModel *string     `json:"default_generation_model"`
	DefaultEmbeddingModel  *string     `json:"default_embedding_model"`
	LlamaCPP               llamaLock   `json:"llama_cpp"`
	Assets                 []lockAsset `json:"assets"`
}

type llamaLock struct {
	Repository    string          `json:"repository"`
	Tag           string          `json:"tag"`
	Commit        string          `json:"commit"`
	LicenseSPDX   string          `json:"license_spdx"`
	LicenseURL    string          `json:"license_url"`
	SourceAssetID string          `json:"source_asset_id"`
	CMake         json.RawMessage `json:"cmake"`
}

type lockAsset struct {
	ID                  string   `json:"id"`
	Kind                string   `json:"kind"`
	Status              string   `json:"status"`
	DefaultEligible     bool     `json:"default_eligible"`
	Repository          string   `json:"repository"`
	Revision            string   `json:"revision"`
	Filename            string   `json:"filename"`
	URL                 string   `json:"url"`
	AllowedHosts        []string `json:"allowed_hosts"`
	SHA256              string   `json:"sha256"`
	SizeBytes           int64    `json:"size_bytes"`
	LicenseSPDX         string   `json:"license_spdx"`
	LicenseURL          string   `json:"license_url"`
	Redistribution      string   `json:"redistribution"`
	Quantization        *string  `json:"quantization"`
	NativeContextTokens *int     `json:"native_context_tokens"`
	QualificationSpec   *string  `json:"qualification_spec"`
	ExtractionRoot      *string  `json:"extraction_root"`
}

type runtimeReceipt struct {
	SchemaVersion       string          `json:"schema_version"`
	Runtime             string          `json:"runtime"`
	Tag                 string          `json:"tag"`
	Commit              string          `json:"commit"`
	SourceSHA256        string          `json:"source_sha256"`
	SourceSizeBytes     int64           `json:"source_size_bytes"`
	LockSHA256          string          `json:"lock_sha256"`
	Profile             string          `json:"profile"`
	CMake               string          `json:"cmake"`
	ConfigureArgv       []string        `json:"configure_argv"`
	BuildArgv           []string        `json:"build_argv"`
	Platform            string          `json:"platform"`
	Machine             string          `json:"machine"`
	Python              string          `json:"python"`
	License             runtimeLicense  `json:"license"`
	Binaries            []runtimeBinary `json:"binaries"`
	QualificationStatus string          `json:"qualification_status"`
	DefaultModelStatus  string          `json:"default_model_status"`
}

type runtimeLicense struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	LicenseSPDX string `json:"license_spdx"`
	LicenseURL  string `json:"license_url"`
}

type runtimeBinary struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// ResolveGeneration fails closed unless a qualified Apache-2.0 model and the
// exact locally built llama-cli binary can be bound to one lock document.
func ResolveGeneration(request GenerationRequest) (GenerationBinding, error) {
	return resolveModelBinding(request, "generation-model")
}

// ResolveEmbedding fails closed unless a qualified Apache-2.0 embedding model
// and the exact locally built llama-embedding binary bind to one lock document.
func ResolveEmbedding(request EmbeddingRequest) (EmbeddingBinding, error) {
	return resolveModelBinding(request, "embedding-model")
}

func resolveModelBinding(request ModelRequest, modelKind string) (ModelBinding, error) {
	if modelKind != "generation-model" && modelKind != "embedding-model" {
		return ModelBinding{}, errors.New("unsupported model binding kind")
	}
	purpose := strings.TrimSuffix(modelKind, "-model")
	executableName := "llama-cli"
	defaultModel := func(lock lockDocument) *string { return lock.DefaultGenerationModel }
	defaultField := "default_generation_model"
	assetFlag := "--model-asset"
	if modelKind == "embedding-model" {
		executableName = "llama-embedding"
		defaultModel = func(lock lockDocument) *string { return lock.DefaultEmbeddingModel }
		defaultField = "default_embedding_model"
		assetFlag = "--embedding-asset"
	}
	if strings.TrimSpace(request.LockPath) == "" {
		return ModelBinding{}, errors.New("model lock path is required")
	}
	if strings.TrimSpace(request.ModelPath) == "" {
		return ModelBinding{}, fmt.Errorf("%s model path is required", purpose)
	}
	if strings.TrimSpace(request.ExecutablePath) == "" {
		request.ExecutablePath = executableName
	}
	lockPath, lockBytes, lockDigest, _, err := readBoundedRegular(request.LockPath, maximumLockBytes)
	if err != nil {
		return GenerationBinding{}, fmt.Errorf("read model lock: %w", err)
	}
	var lock lockDocument
	if err := decodeStrictDocument(lockBytes, &lock); err != nil {
		return GenerationBinding{}, fmt.Errorf("decode model lock: %w", err)
	}
	if err := validateLockShape(lockBytes, lock); err != nil {
		return GenerationBinding{}, err
	}
	assetID := strings.TrimSpace(request.AssetID)
	if request.AssetID != "" && request.AssetID != assetID {
		return GenerationBinding{}, errors.New("model asset ID has surrounding whitespace")
	}
	if assetID == "" {
		selectedDefault := defaultModel(lock)
		if selectedDefault == nil || strings.TrimSpace(*selectedDefault) == "" {
			return GenerationBinding{}, fmt.Errorf("model lock has no qualified %s; %s is required after qualification", defaultField, assetFlag)
		}
		assetID = *selectedDefault
	}
	var selected *lockAsset
	var source *lockAsset
	for index := range lock.Assets {
		asset := &lock.Assets[index]
		if asset.ID == assetID {
			selected = asset
		}
		if asset.ID == lock.LlamaCPP.SourceAssetID {
			source = asset
		}
	}
	if selected == nil {
		return GenerationBinding{}, fmt.Errorf("model asset %q is absent from the lock", assetID)
	}
	if err := validateModelAsset(*selected, modelKind); err != nil {
		return GenerationBinding{}, err
	}
	if source == nil || source.Kind != "source-archive" || source.Revision != lock.LlamaCPP.Commit {
		return GenerationBinding{}, errors.New("model lock does not bind the llama.cpp source archive to its commit")
	}
	if !sha256Pattern.MatchString(source.SHA256) || source.SizeBytes <= 0 || source.LicenseSPDX != "MIT" ||
		source.LicenseURL != lock.LlamaCPP.LicenseURL || source.Status != "runtime-pinned" {
		return GenerationBinding{}, errors.New("llama.cpp source asset provenance is invalid")
	}
	modelPath, modelInfo, err := canonicalRegularPath(request.ModelPath, false)
	if err != nil {
		return GenerationBinding{}, fmt.Errorf("resolve %s model: %w", purpose, err)
	}
	if filepath.Base(modelPath) != selected.Filename {
		return GenerationBinding{}, fmt.Errorf("%s model basename %q does not match locked filename %q", purpose, filepath.Base(modelPath), selected.Filename)
	}
	if modelInfo.Size() != selected.SizeBytes {
		return GenerationBinding{}, fmt.Errorf("%s model size %d does not match locked size %d", purpose, modelInfo.Size(), selected.SizeBytes)
	}
	executablePath, executableInfo, err := resolveExecutable(request.ExecutablePath)
	if err != nil {
		return GenerationBinding{}, err
	}
	receiptPath := strings.TrimSpace(request.RuntimeReceiptPath)
	if receiptPath == "" {
		receiptPath = filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(executablePath))), runtimeReceiptName)
	}
	receiptPath, receiptBytes, receiptDigest, _, err := readBoundedRegular(receiptPath, maximumReceiptBytes)
	if err != nil {
		return GenerationBinding{}, fmt.Errorf("read llama.cpp runtime receipt: %w", err)
	}
	var receipt runtimeReceipt
	if err := decodeStrictDocument(receiptBytes, &receipt); err != nil {
		return GenerationBinding{}, fmt.Errorf("decode llama.cpp runtime receipt: %w", err)
	}
	receiptDirectory, err := os.Lstat(filepath.Dir(receiptPath))
	if err != nil || !receiptDirectory.IsDir() || receiptDirectory.Mode()&os.ModeSymlink != 0 || receiptDirectory.Mode().Perm()&0o022 != 0 {
		return GenerationBinding{}, errors.New("llama.cpp runtime receipt directory must be a private real directory")
	}
	if err := validateReceiptShape(receiptBytes, receipt, lock, lockDigest, *source); err != nil {
		return GenerationBinding{}, err
	}
	runtimeRoot := filepath.Dir(receiptPath)
	if err := verifyRuntimeLicense(runtimeRoot, receipt.License); err != nil {
		return GenerationBinding{}, err
	}
	expectedBinaryName := executableName
	if runtime.GOOS == "windows" {
		expectedBinaryName += ".exe"
	}
	expectedRelative := filepath.ToSlash(filepath.Join("build", "bin", expectedBinaryName))
	var executableReceipt *runtimeBinary
	for index := range receipt.Binaries {
		if receipt.Binaries[index].Path == expectedRelative {
			executableReceipt = &receipt.Binaries[index]
			break
		}
	}
	if executableReceipt == nil {
		return GenerationBinding{}, fmt.Errorf("llama.cpp runtime receipt has no %s binary", executableName)
	}
	expectedExecutable := filepath.Join(runtimeRoot, filepath.FromSlash(executableReceipt.Path))
	expectedExecutable, _, err = canonicalRegularPath(expectedExecutable, true)
	if err != nil {
		return GenerationBinding{}, fmt.Errorf("resolve receipt-bound %s: %w", executableName, err)
	}
	if executablePath != expectedExecutable {
		return GenerationBinding{}, fmt.Errorf("%s %q is not the receipt-bound executable %q", executableName, executablePath, expectedExecutable)
	}
	if executableInfo.Size() != executableReceipt.SizeBytes {
		return GenerationBinding{}, fmt.Errorf("%s size %d does not match receipt size %d", executableName, executableInfo.Size(), executableReceipt.SizeBytes)
	}
	return GenerationBinding{
		AssetID: assetID, LockSHA256: lockDigest, RuntimeReceiptSHA256: receiptDigest,
		RuntimeProfile: receipt.Profile, RuntimeTag: receipt.Tag, RuntimeRevision: receipt.Commit,
		RuntimeLicense: lock.LlamaCPP.LicenseSPDX, RuntimeSHA256: executableReceipt.SHA256,
		ModelRevision: selected.Revision, ModelLicense: selected.LicenseSPDX, ModelSHA256: selected.SHA256,
		ModelSizeBytes: selected.SizeBytes, Quantization: *selected.Quantization,
		QuantizationBits: quantizationBits(*selected.Quantization), NativeContextTokens: *selected.NativeContextTokens,
		QualificationSpec: *selected.QualificationSpec, ExecutablePath: executablePath, ModelPath: modelPath,
		RuntimeReceiptPath: receiptPath, LockPath: lockPath,
	}, nil
}

func validateLockShape(data []byte, lock lockDocument) error {
	if err := requireJSONObjectKeys(data, []string{"$schema", "schema_version", "default_generation_model", "default_embedding_model", "llama_cpp", "assets"}); err != nil {
		return fmt.Errorf("model lock shape: %w", err)
	}
	if lock.Schema != "../schemas/model-lock.schema.json" || lock.SchemaVersion != lockSchemaVersion {
		return errors.New("model lock schema identity is unsupported")
	}
	if lock.LlamaCPP.Repository != "https://github.com/ggml-org/llama.cpp" || lock.LlamaCPP.LicenseSPDX != "MIT" ||
		!revisionPattern.MatchString(lock.LlamaCPP.Commit) || !validHTTPS(lock.LlamaCPP.LicenseURL) ||
		!tagPattern.MatchString(lock.LlamaCPP.Tag) || len(lock.LlamaCPP.CMake) == 0 || string(lock.LlamaCPP.CMake) == "null" {
		return errors.New("model lock llama_cpp provenance is invalid")
	}
	if !idPattern.MatchString(lock.LlamaCPP.SourceAssetID) || len(lock.Assets) < 3 || len(lock.Assets) > 32 {
		return errors.New("model lock asset inventory is invalid")
	}
	seen := map[string]struct{}{}
	for _, asset := range lock.Assets {
		if !idPattern.MatchString(asset.ID) {
			return fmt.Errorf("model lock asset id %q is invalid", asset.ID)
		}
		if _, duplicate := seen[asset.ID]; duplicate {
			return fmt.Errorf("model lock repeats asset %q", asset.ID)
		}
		seen[asset.ID] = struct{}{}
	}
	for _, defaultAsset := range []struct {
		id   *string
		kind string
	}{{lock.DefaultGenerationModel, "generation-model"}, {lock.DefaultEmbeddingModel, "embedding-model"}} {
		if defaultAsset.id == nil {
			continue
		}
		if !idPattern.MatchString(*defaultAsset.id) {
			return errors.New("model lock contains an invalid default asset ID")
		}
		matched := false
		for _, asset := range lock.Assets {
			if asset.ID == *defaultAsset.id && asset.Kind == defaultAsset.kind && asset.Status == "qualified" && asset.DefaultEligible {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("model lock default %q is not qualified and eligible", *defaultAsset.id)
		}
	}
	return nil
}

func validateGenerationAsset(asset lockAsset) error {
	return validateModelAsset(asset, "generation-model")
}

func validateEmbeddingAsset(asset lockAsset) error {
	return validateModelAsset(asset, "embedding-model")
}

func validateModelAsset(asset lockAsset, expectedKind string) error {
	purpose := strings.TrimSuffix(expectedKind, "-model")
	if asset.Kind != expectedKind || asset.Status != "qualified" || !asset.DefaultEligible {
		return fmt.Errorf("model asset %q is not a qualified, default-eligible %s model", asset.ID, purpose)
	}
	if !revisionPattern.MatchString(asset.Revision) || !sha256Pattern.MatchString(asset.SHA256) || asset.SizeBytes <= 4 || asset.SizeBytes > 8*1024*1024*1024 {
		return fmt.Errorf("model asset %q has invalid revision, digest, or size", asset.ID)
	}
	if !filenamePattern.MatchString(asset.Filename) || filepath.Base(asset.Filename) != asset.Filename {
		return fmt.Errorf("model asset %q has an unsafe filename", asset.ID)
	}
	if asset.LicenseSPDX != "Apache-2.0" || !validHTTPS(asset.LicenseURL) || !validHTTPS(asset.Repository) || !validHTTPS(asset.URL) || asset.Redistribution != "not-bundled-download-on-demand" {
		return fmt.Errorf("model asset %q is not bound to approved Apache-2.0 provenance", asset.ID)
	}
	if asset.Quantization == nil || strings.TrimSpace(*asset.Quantization) == "" || asset.NativeContextTokens == nil || *asset.NativeContextTokens <= 0 || *asset.NativeContextTokens > 1048576 || asset.QualificationSpec == nil {
		return fmt.Errorf("model asset %q lacks quantization, context, or qualification metadata", asset.ID)
	}
	qualification := filepath.ToSlash(filepath.Clean(*asset.QualificationSpec))
	if qualification != *asset.QualificationSpec || !strings.HasPrefix(qualification, "models/qualification/") || filepath.Ext(qualification) != ".json" {
		return fmt.Errorf("model asset %q has an unsafe qualification specification", asset.ID)
	}
	if quantizationBits(*asset.Quantization) <= 0 || asset.ExtractionRoot != nil {
		return fmt.Errorf("model asset %q has invalid model-only metadata", asset.ID)
	}
	return nil
}

func validateReceiptShape(data []byte, receipt runtimeReceipt, lock lockDocument, lockDigest string, source lockAsset) error {
	expectedKeys := []string{
		"schema_version", "runtime", "tag", "commit", "source_sha256", "source_size_bytes", "lock_sha256", "profile", "cmake",
		"configure_argv", "build_argv", "platform", "machine", "python", "license", "binaries", "qualification_status", "default_model_status",
	}
	if err := requireJSONObjectKeys(data, expectedKeys); err != nil {
		return fmt.Errorf("llama.cpp runtime receipt shape: %w", err)
	}
	if receipt.SchemaVersion != runtimeSchemaVersion || receipt.Runtime != "llama.cpp" || receipt.Tag != lock.LlamaCPP.Tag ||
		receipt.Commit != lock.LlamaCPP.Commit || receipt.SourceSHA256 != source.SHA256 || receipt.SourceSizeBytes != source.SizeBytes ||
		receipt.LockSHA256 != lockDigest {
		return errors.New("llama.cpp runtime receipt does not match the model lock")
	}
	if receipt.Profile != "portable" && receipt.Profile != "native" {
		return errors.New("llama.cpp runtime receipt profile is invalid")
	}
	if receipt.License.Path != runtimeLicensePath || !sha256Pattern.MatchString(receipt.License.SHA256) ||
		receipt.License.SizeBytes <= 0 || receipt.License.SizeBytes > maximumLicenseBytes ||
		receipt.License.LicenseSPDX != lock.LlamaCPP.LicenseSPDX || receipt.License.LicenseSPDX != source.LicenseSPDX ||
		receipt.License.LicenseURL != lock.LlamaCPP.LicenseURL || receipt.License.LicenseURL != source.LicenseURL {
		return errors.New("llama.cpp runtime receipt has invalid license binding")
	}
	if strings.TrimSpace(receipt.CMake) == "" || len(receipt.ConfigureArgv) == 0 || len(receipt.BuildArgv) == 0 ||
		strings.TrimSpace(receipt.Platform) == "" || strings.TrimSpace(receipt.Machine) == "" || strings.TrimSpace(receipt.Python) == "" ||
		receipt.QualificationStatus != "not-run" || receipt.DefaultModelStatus != "none" {
		return errors.New("llama.cpp runtime receipt build policy is incomplete or unsupported")
	}
	expectedPaths := []string{"build/bin/llama-bench", "build/bin/llama-cli", "build/bin/llama-embedding", "build/bin/llama-server"}
	if runtime.GOOS == "windows" {
		for index := range expectedPaths {
			expectedPaths[index] += ".exe"
		}
	}
	observed := make([]string, 0, len(receipt.Binaries))
	seen := map[string]struct{}{}
	for _, binary := range receipt.Binaries {
		if filepath.ToSlash(filepath.Clean(filepath.FromSlash(binary.Path))) != binary.Path || strings.HasPrefix(binary.Path, "/") || strings.Contains(binary.Path, "..") ||
			!sha256Pattern.MatchString(binary.SHA256) || binary.SizeBytes <= 0 {
			return errors.New("llama.cpp runtime receipt contains an invalid binary record")
		}
		if _, duplicate := seen[binary.Path]; duplicate {
			return errors.New("llama.cpp runtime receipt repeats a binary path")
		}
		seen[binary.Path] = struct{}{}
		observed = append(observed, binary.Path)
	}
	sort.Strings(observed)
	sort.Strings(expectedPaths)
	if strings.Join(observed, "\x00") != strings.Join(expectedPaths, "\x00") {
		return fmt.Errorf("llama.cpp runtime receipt binary inventory differs: got %v", observed)
	}
	return nil
}

func verifyRuntimeLicense(runtimeRoot string, license runtimeLicense) error {
	path := filepath.Join(runtimeRoot, filepath.FromSlash(runtimeLicensePath))
	_, _, digest, info, err := readBoundedRegular(path, maximumLicenseBytes)
	if err != nil {
		return fmt.Errorf("verify llama.cpp runtime license: %w", err)
	}
	if digest != license.SHA256 || info.Size() != license.SizeBytes {
		return errors.New("llama.cpp runtime license does not match its receipt")
	}
	return nil
}

func resolveExecutable(path string) (string, os.FileInfo, error) {
	resolved := path
	if filepath.Base(path) == path {
		var err error
		resolved, err = exec.LookPath(path)
		if err != nil {
			return "", nil, fmt.Errorf("resolve llama.cpp executable: %w", err)
		}
	}
	return canonicalRegularPath(resolved, true)
}

func canonicalRegularPath(path string, executable bool) (string, os.FileInfo, error) {
	if path == "" || path != strings.TrimSpace(path) {
		return "", nil, errors.New("artifact path is empty or has surrounding whitespace")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("artifact is not a canonical regular file")
	}
	if executable && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", nil, errors.New("artifact is not executable")
	}
	return filepath.Clean(canonical), info, nil
}

func readBoundedRegular(path string, maximum int64) (string, []byte, string, os.FileInfo, error) {
	if maximum <= 0 {
		return "", nil, "", nil, errors.New("read limit must be positive")
	}
	if path == "" || path != strings.TrimSpace(path) {
		return "", nil, "", nil, errors.New("document path is empty or has surrounding whitespace")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, "", nil, err
	}
	absolute = filepath.Clean(absolute)
	pathnameBefore, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, "", nil, err
	}
	if pathnameBefore.Mode()&os.ModeSymlink != 0 || !pathnameBefore.Mode().IsRegular() || pathnameBefore.Size() > maximum {
		return "", nil, "", nil, errors.New("document must be a bounded real regular file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return "", nil, "", nil, err
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil || !os.SameFile(pathnameBefore, openedBefore) {
		return "", nil, "", nil, errors.New("document identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return "", nil, "", nil, errors.New("document exceeded its read limit")
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return "", nil, "", nil, err
	}
	pathnameAfter, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(openedBefore, openedAfter) || !os.SameFile(openedAfter, pathnameAfter) || openedAfter.Size() != int64(len(data)) {
		return "", nil, "", nil, errors.New("document changed while reading")
	}
	digest := sha256.Sum256(data)
	return absolute, data, hex.EncodeToString(digest[:]), openedAfter, nil
}

func decodeStrictDocument(data []byte, destination any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("document contains trailing JSON content")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("document contains multiple JSON values")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("JSON object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("JSON array is not closed")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireJSONObjectKeys(data []byte, expected []string) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	expected = append([]string(nil), expected...)
	sort.Strings(expected)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf("keys differ: got %v, expected %v", actual, expected)
	}
	return nil
}

func validHTTPS(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func quantizationBits(value string) int {
	upper := strings.ToUpper(value)
	if !strings.HasPrefix(upper, "Q") {
		return 0
	}
	suffix := strings.TrimPrefix(upper, "Q")
	end := 0
	for end < len(suffix) && suffix[end] >= '0' && suffix[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	bits, _ := strconv.Atoi(suffix[:end])
	return bits
}
