package scipindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
)

const (
	// ManifestName is the bounded sidecar used for RKC-published SCIP indexes.
	ManifestName = "rkc.scip-manifest.json"
	// ManifestSchemaVersion identifies manifests containing source-affinity receipts.
	ManifestSchemaVersion = "1.1"
	// SourceBindingSchemaVersion identifies the canonical source-set digest contract.
	SourceBindingSchemaVersion = "1.0"
	// MaximumManifestBytes bounds automatic sidecar discovery.
	MaximumManifestBytes = int64(1 << 20)
	// MaximumBindingSourceBytes bounds one source hashed during receipt creation.
	MaximumBindingSourceBytes = int64(256 << 20)
	// MaximumTotalBindingSourceBytes bounds all sources hashed for one index receipt.
	MaximumTotalBindingSourceBytes = int64(8 << 30)
)

// SourceBinding cryptographically binds one exact SCIP index to the current
// repository sources named by its documents. It contains no repository path.
type SourceBinding struct {
	SchemaVersion     string `json:"schema_version"`
	IndexSHA256       string `json:"index_sha256"`
	IndexSizeBytes    int64  `json:"index_size_bytes"`
	SourceSHA256      string `json:"source_sha256"`
	DocumentCount     int    `json:"document_count"`
	ProjectRootSHA256 string `json:"project_root_sha256"`
}

// ManifestIndex is one generated index and its repository-source receipt.
// Paths are portable base names; absolute repository and tool paths are never
// persisted.
type ManifestIndex struct {
	Language                 string         `json:"language"`
	Tool                     string         `json:"tool,omitempty"`
	ToolVersion              string         `json:"tool_version,omitempty"`
	ExecutableSHA256         string         `json:"indexer_executable_sha256,omitempty"`
	Path                     string         `json:"path"`
	SHA256                   string         `json:"sha256"`
	SizeBytes                int64          `json:"size_bytes"`
	Documents                int            `json:"documents"`
	Symbols                  int            `json:"symbols"`
	Occurrences              int            `json:"occurrences"`
	ExternalDocumentsSkipped int            `json:"external_documents_skipped"`
	SourceBinding            *SourceBinding `json:"source_binding,omitempty"`
}

// Manifest is the portable generated-index sidecar automatically discovered
// by PrepareInputs.
type Manifest struct {
	SchemaVersion string          `json:"schema_version"`
	Indexes       []ManifestIndex `json:"indexes"`
}

type sourceIdentity struct {
	path      string
	sha256    string
	sizeBytes int64
}

// BuildSourceBinding parses an already prepared index and hashes every source
// document through the repository-safe path boundary. The returned receipt is
// portable because it commits to canonical relative paths and content, never
// an absolute host path.
func BuildSourceBinding(ctx context.Context, root string, input Input) (SourceBinding, error) {
	if ctx == nil {
		return SourceBinding{}, errors.New("SCIP source-binding context is required")
	}
	inspection, err := Inspect(ctx, input)
	if err != nil {
		return SourceBinding{}, err
	}
	projectDigest, err := projectRootDigest(inspection.ProjectRoot)
	if err != nil {
		return SourceBinding{}, err
	}
	documents, err := readBindingDocuments(ctx, input)
	if err != nil {
		return SourceBinding{}, err
	}
	identities := make([]sourceIdentity, 0, len(documents))
	seen := make(map[string]struct{}, len(documents))
	var totalBytes int64
	for _, document := range documents {
		if _, duplicate := seen[document.path]; duplicate {
			return SourceBinding{}, fmt.Errorf("SCIP index contains duplicate document %q", document.path)
		}
		seen[document.path] = struct{}{}
		identity, err := hashRepositorySource(ctx, root, document.path)
		if err != nil {
			return SourceBinding{}, fmt.Errorf("bind SCIP document %q: %w", document.path, err)
		}
		totalBytes += identity.sizeBytes
		if totalBytes > MaximumTotalBindingSourceBytes {
			return SourceBinding{}, fmt.Errorf(
				"SCIP source binding exceeds the %d-byte aggregate limit",
				MaximumTotalBindingSourceBytes,
			)
		}
		if document.textPresent {
			embedded := sha256.Sum256([]byte(document.text))
			if int64(len(document.text)) != identity.sizeBytes ||
				hex.EncodeToString(embedded[:]) != identity.sha256 {
				return SourceBinding{}, fmt.Errorf(
					"SCIP document %q embedded text does not match the repository source",
					document.path,
				)
			}
		}
		identities = append(identities, identity)
	}
	return SourceBinding{
		SchemaVersion: SourceBindingSchemaVersion,
		IndexSHA256:   input.SHA256, IndexSizeBytes: input.SizeBytes,
		SourceSHA256: sourceIdentityDigest(identities), DocumentCount: len(identities),
		ProjectRootSHA256: projectDigest,
	}, nil
}

func readBindingDocuments(ctx context.Context, input Input) ([]document, error) {
	before, err := os.Lstat(input.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect SCIP index: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() != input.SizeBytes {
		return nil, errors.New("SCIP index no longer matches its prepared input")
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return nil, fmt.Errorf("open SCIP index: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, opened) {
		return nil, errors.New("SCIP index changed while opening")
	}
	hasher := sha256.New()
	reader := newWireReader(
		io.TeeReader(&contextReader{ctx: ctx, reader: file}, hasher),
		input.SizeBytes,
	)
	documents := make([]document, 0)
	metadataSeen := false
	for {
		field, wire, done, err := reader.next()
		if err != nil {
			return nil, fmt.Errorf("decode SCIP source binding: %w", err)
		}
		if done {
			break
		}
		switch field {
		case 1:
			if metadataSeen || wire != 2 {
				return nil, errors.New("decode SCIP source binding: invalid metadata")
			}
			if _, err := reader.bytes(maximumMessageBytes); err != nil {
				return nil, fmt.Errorf("decode SCIP source-binding metadata: %w", err)
			}
			metadataSeen = true
		case 2:
			if !metadataSeen || wire != 2 {
				return nil, errors.New("decode SCIP source binding: invalid document ordering")
			}
			message, err := reader.bytes(maximumDocumentBytes)
			if err != nil {
				return nil, fmt.Errorf("decode SCIP source-binding document: %w", err)
			}
			value, err := parseDocument(message)
			if err != nil {
				return nil, fmt.Errorf("decode SCIP source-binding document: %w", err)
			}
			canonical, contained := classifyDocumentPath(value.path)
			if !canonical || !contained {
				return nil, errors.New("SCIP source-binding document path is not canonical")
			}
			documents = append(documents, value)
			if len(documents) > maximumDocuments {
				return nil, fmt.Errorf("SCIP inputs exceed the %d-document limit", maximumDocuments)
			}
		default:
			if err := reader.skip(wire); err != nil {
				return nil, fmt.Errorf("skip SCIP source-binding field: %w", err)
			}
		}
	}
	if !metadataSeen {
		return nil, errors.New("decode SCIP source binding: metadata is missing")
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != input.SHA256 {
		return nil, errors.New("SCIP index changed while building its source binding")
	}
	after, err := os.Lstat(input.Path)
	if err != nil || !sameFileSnapshot(before, after) {
		return nil, errors.New("SCIP index changed while building its source binding")
	}
	return documents, nil
}

func hashRepositorySource(ctx context.Context, root, path string) (sourceIdentity, error) {
	file, err := sourcepath.OpenRegular(root, path)
	if err != nil {
		return sourceIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return sourceIdentity{}, fmt.Errorf("inspect source: %w", err)
	}
	if info.Size() > MaximumBindingSourceBytes {
		return sourceIdentity{}, fmt.Errorf(
			"source is %d bytes; maximum for a SCIP receipt is %d",
			info.Size(), MaximumBindingSourceBytes,
		)
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		return sourceIdentity{}, fmt.Errorf("hash source: %w", err)
	}
	if written != info.Size() {
		return sourceIdentity{}, errors.New("source size changed while hashing")
	}
	after, err := file.Stat()
	if err != nil || !sameFileSnapshot(info, after) {
		return sourceIdentity{}, errors.New("source changed while hashing")
	}
	return sourceIdentity{
		path: path, sha256: hex.EncodeToString(hasher.Sum(nil)), sizeBytes: written,
	}, nil
}

func sourceIdentityDigest(values []sourceIdentity) string {
	values = append([]sourceIdentity(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i].path < values[j].path })
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, "rkc-scip-source-binding-v1\n")
	for _, value := range values {
		_, _ = io.WriteString(hasher, value.path)
		_, _ = io.WriteString(hasher, "\x00")
		_, _ = io.WriteString(hasher, strings.ToLower(value.sha256))
		_, _ = io.WriteString(hasher, "\x00")
		_, _ = io.WriteString(hasher, strconv.FormatInt(value.sizeBytes, 10))
		_, _ = io.WriteString(hasher, "\n")
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func projectRootDigest(value string) (string, error) {
	if value == "" || len(value) > 4096 {
		return "", errors.New("SCIP project_root metadata is missing or too long")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", errors.New("SCIP project_root metadata contains control characters")
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Scheme == "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("SCIP project_root metadata must be an absolute URI")
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:]), nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateSourceBinding(value SourceBinding, input Input) error {
	if value.SchemaVersion != SourceBindingSchemaVersion {
		return fmt.Errorf("unsupported source-binding schema_version %q", value.SchemaVersion)
	}
	if !validDigest(value.IndexSHA256) || !validDigest(value.SourceSHA256) ||
		!validDigest(value.ProjectRootSHA256) {
		return errors.New("source binding contains an invalid SHA-256 digest")
	}
	if value.IndexSHA256 != input.SHA256 || value.IndexSizeBytes != input.SizeBytes {
		return errors.New("source binding does not match the prepared SCIP index")
	}
	if value.DocumentCount < 0 || value.DocumentCount > maximumDocuments {
		return errors.New("source binding document_count is outside the supported bound")
	}
	return nil
}

func loadManifestSourceBinding(input Input) (*SourceBinding, error) {
	manifestPath := filepath.Join(filepath.Dir(input.Path), ManifestName)
	data, err := readBoundedManifest(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read SCIP source-binding manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode SCIP source-binding manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("decode SCIP source-binding manifest: trailing JSON content")
	}
	if manifest.SchemaVersion != ManifestSchemaVersion && manifest.SchemaVersion != "1.0" {
		return nil, fmt.Errorf("decode SCIP source-binding manifest: unsupported schema_version %q", manifest.SchemaVersion)
	}
	if len(manifest.Indexes) > MaximumIndexCount {
		return nil, errors.New("decode SCIP source-binding manifest: index count exceeds the supported bound")
	}
	base := filepath.Base(input.Path)
	seen := map[string]struct{}{}
	var matched *SourceBinding
	for _, entry := range manifest.Indexes {
		if entry.Path == "" || len(entry.Path) > 255 || strings.ContainsAny(entry.Path, "/\\\x00\r\n") || filepath.Base(entry.Path) != entry.Path {
			return nil, errors.New("decode SCIP source-binding manifest: invalid portable index path")
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return nil, errors.New("decode SCIP source-binding manifest: duplicate index path")
		}
		seen[entry.Path] = struct{}{}
		if !validDigest(entry.SHA256) || entry.SizeBytes < 0 || entry.Documents < 0 ||
			entry.Symbols < 0 || entry.Occurrences < 0 || entry.ExternalDocumentsSkipped < 0 {
			return nil, errors.New("decode SCIP source-binding manifest: invalid index record")
		}
		if entry.ExecutableSHA256 != "" && !validDigest(entry.ExecutableSHA256) {
			return nil, errors.New("decode SCIP source-binding manifest: invalid indexer executable digest")
		}
		if entry.Path != base {
			continue
		}
		if entry.SHA256 != input.SHA256 || entry.SizeBytes != input.SizeBytes {
			return nil, errors.New("SCIP source-binding manifest describes a stale or foreign index")
		}
		if entry.SourceBinding == nil {
			return nil, errors.New("SCIP manifest entry has no source-affinity receipt; regenerate the index with 'rkc scip generate'")
		}
		if err := validateSourceBinding(*entry.SourceBinding, input); err != nil {
			return nil, fmt.Errorf("SCIP source-binding manifest: %w", err)
		}
		copy := *entry.SourceBinding
		matched = &copy
	}
	if matched == nil {
		return nil, errors.New("SCIP source-binding manifest does not contain the selected index")
	}
	return matched, nil
}

func readBoundedManifest(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("manifest must be a real regular file, not a symlink")
	}
	if before.Size() > MaximumManifestBytes {
		return nil, fmt.Errorf("manifest exceeds the %d-byte bound", MaximumManifestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, opened) {
		_ = file.Close()
		return nil, errors.New("manifest changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaximumManifestBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(data)) > MaximumManifestBytes {
		return nil, fmt.Errorf("manifest exceeds the %d-byte bound", MaximumManifestBytes)
	}
	after, err := os.Lstat(path)
	if err != nil || !sameFileSnapshot(before, after) {
		return nil, errors.New("manifest changed while reading")
	}
	return data, nil
}
