// Package server serves an immutable RKC snapshot through the local read API
// and responsive static web interface.
package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/search"
)

const (
	// IntegrityVerified identifies an export whose ownership marker, manifest,
	// canonical files, sizes, and digests have all been verified.
	IntegrityVerified = "verified"
	// IntegrityVerifiedLegacyUnmarked identifies a verified legacy export that
	// predates the current ownership marker but retains its integrity manifest.
	IntegrityVerifiedLegacyUnmarked = "verified_legacy_unmarked"
	// IntegrityLegacyUnverified identifies the bounded compatibility layout that
	// lacks a modern export manifest and therefore cannot claim verified integrity.
	IntegrityLegacyUnverified       = "legacy_unverified"
	exportManifestName              = "rkc-export-manifest.json"
	maximumOwnershipMarkerFileSize  = 64 * 1024
	maximumExportManifestFileSize   = 16 * 1024 * 1024
	maximumCanonicalDatasetFileSize = 512 * 1024 * 1024
	maximumSearchIndexFileSize      = search.MaximumPersistedIndexBytes
	maximumDatasetFileCount         = 500000
	maximumStaticSiteFileSize       = 512 * 1024 * 1024
	maximumStaticSiteTotalBytes     = 512 * 1024 * 1024
	maximumStaticSiteFileCount      = 4096
	maximumLegacyManifestFileSize   = 1 * 1024 * 1024
	maximumLegacyMetadataFileSize   = 16 * 1024 * 1024
	maximumLegacyJSONLFileSize      = 64 * 1024 * 1024
	maximumLegacyJSONLTotalBytes    = 192 * 1024 * 1024
	maximumLegacyJSONLLineBytes     = 4 * 1024 * 1024
	maximumLegacyRecordsPerFile     = 250000
	maximumLegacyRecordsTotal       = 500000
	snapshotGenerationHeader        = "X-RKC-Snapshot-ID"
)

type staticSiteAsset struct {
	data   []byte
	digest string
}

// Dataset is one immutable, integrity-classified atlas plus its derived lookup,
// graph, search, and browser indexes. Maps and indexes are constructed by Load
// and must be treated as read-only by concurrent handlers.
type Dataset struct {
	Root         string
	Manifest     model.Snapshot
	Coverage     model.Coverage
	Bundle       model.Bundle
	NodeByID     map[string]model.Node
	ArtifactByID map[string]model.Artifact
	EvidenceByID map[string]model.Evidence
	Graph        *graphindex.Index
	Search       *search.Index
	Integrity    string
	LoadedAt     time.Time
	staticSite   map[string]staticSiteAsset
	// staticSiteTrusted is true only when the current RKC binary generated the
	// browser assets in memory. A checksum-consistent imported atlas is valid
	// read-only data, but it is not trusted code for a privileged workbench
	// origin.
	staticSiteTrusted bool
	pagination        *paginationState
}

// Load verifies and decodes an exported atlas, validates canonical bundle and
// coverage bindings, and builds read-only indexes. Modern canonical layouts
// without an export manifest fail closed; bounded legacy layouts are labeled.
func Load(outputRoot string) (*Dataset, error) {
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return nil, err
	}
	if filepath.Base(root) == "site" {
		parent := filepath.Dir(root)
		if _, err := os.Stat(filepath.Join(parent, "rkc.manifest.json")); err == nil {
			root = parent
		}
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	integrity, err := inspectDatasetIntegrity(root)
	if err != nil {
		return nil, fmt.Errorf("verify dataset integrity: %w", err)
	}

	var bundle model.Bundle
	legacyLayout := false
	if err := integrity.readJSON(root, "bundle.json", &bundle); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load bundle: %w", err)
		}
		legacyLayout = true
		bundle, err = loadLegacyBundle(root)
		if err != nil {
			return nil, err
		}
	}
	var manifest model.Snapshot
	if err := integrity.readJSON(root, "rkc.manifest.json", &manifest); err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	if !reflect.DeepEqual(bundle.Snapshot, manifest) {
		return nil, fmt.Errorf("bundle snapshot does not match rkc.manifest.json (bundle=%q manifest=%q)", bundle.Snapshot.ID, manifest.ID)
	}
	if !legacyLayout && integrity.status == IntegrityLegacyUnverified {
		return nil, errors.New("canonical dataset is missing its export manifest")
	}
	if !model.IsCanonicalDecodedBundle(bundle) {
		return nil, errors.New("bundle.json is not in canonical form")
	}
	validation := model.ValidateBundle(bundle, model.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if validation.HasErrors() {
		return nil, fmt.Errorf("validate bundle: %s", validationErrors(validation))
	}
	if err := search.ValidateBundleObjectIDs(bundle); err != nil {
		return nil, fmt.Errorf("validate search identities: %w", err)
	}
	if integrity.manifest != nil && integrity.manifest.SnapshotID != manifest.ID {
		return nil, fmt.Errorf("export manifest snapshot %q does not match dataset snapshot %q", integrity.manifest.SnapshotID, manifest.ID)
	}

	expectedCoverage := model.BuildCoverage(bundle)
	var coverage model.Coverage
	if err := integrity.readJSON(root, "coverage.json", &coverage); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load coverage: %w", err)
		}
		coverage = expectedCoverage
	} else if !reflect.DeepEqual(coverage, expectedCoverage) {
		return nil, errors.New("coverage.json does not match the canonical bundle")
	}

	// A legacy or metadata-only index is rebuilt from canonical truth. A modern
	// repository-text index is used only after its manifest hash, snapshot
	// binding, object identities, body receipts, postings, and accounting are
	// independently validated against the canonical bundle.
	var searchIndex *search.Index
	if integrity.manifest != nil {
		var persisted search.Index
		err := integrity.readVerifiedDerivedJSON(root, "search/index.json", maximumSearchIndexFileSize, &persisted)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load repository text search index: %w", err)
		}
		if err == nil && persisted.CorpusVersion == search.RepositoryTextCorpusVersion {
			if err := search.ValidateBundleIndex(&persisted, bundle, true); err != nil {
				return nil, fmt.Errorf("validate repository text search index: %w", err)
			}
			searchIndex = &persisted
		}
	}
	if searchIndex == nil {
		searchIndex = search.BuildFromBundle(bundle)
	}
	// Persisted site/* files are a portable export projection, not authenticated
	// publisher code. A live server always regenerates its executable application
	// shell from the current binary. Full atlas data stays behind the bounded API
	// instead of being serialized and retained a second time in process memory.
	browserAssets, err := rkcexport.SiteAssets()
	if err != nil {
		return nil, fmt.Errorf("load current browser: %w", err)
	}
	staticSite, err := captureGeneratedBrowser(browserAssets)
	if err != nil {
		return nil, fmt.Errorf("load current browser: %w", err)
	}
	dataset := &Dataset{
		Root: root, Manifest: manifest, Coverage: coverage, Bundle: bundle,
		NodeByID: make(map[string]model.Node, len(bundle.Nodes)), ArtifactByID: make(map[string]model.Artifact, len(bundle.Artifacts)),
		EvidenceByID: make(map[string]model.Evidence, len(bundle.Evidence)), Graph: graphindex.Build(bundle.Nodes, bundle.Edges),
		Search: searchIndex, Integrity: integrity.status, LoadedAt: time.Now().UTC(),
		staticSite: staticSite, staticSiteTrusted: true,
	}
	dataset.pagination = newPaginationState(dataset.Search)
	for _, node := range bundle.Nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, artifact := range bundle.Artifacts {
		dataset.ArtifactByID[artifact.ID] = artifact
	}
	for _, evidence := range bundle.Evidence {
		dataset.EvidenceByID[evidence.ID] = evidence
	}
	return dataset, nil
}

type datasetExportFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Canonical bool   `json:"canonical"`
}

type datasetExportManifest struct {
	SchemaVersion        string              `json:"schema_version"`
	SnapshotID           string              `json:"snapshot_id"`
	Files                []datasetExportFile `json:"files"`
	TotalBytes           int64               `json:"total_bytes"`
	CanonicalBytes       int64               `json:"canonical_bytes"`
	CanonicalFilesDigest string              `json:"canonical_files_digest"`
}

type datasetIntegrity struct {
	status   string
	manifest *datasetExportManifest
	files    map[string][]byte
}

func (integrity datasetIntegrity) readJSON(root, name string, target any) error {
	if integrity.manifest != nil {
		data, ok := integrity.files[filepath.ToSlash(name)]
		if !ok {
			return os.ErrNotExist
		}
		return decodeStrictJSON(data, target)
	}
	limit := int64(maximumLegacyMetadataFileSize)
	switch filepath.ToSlash(name) {
	case "bundle.json":
		limit = maximumCanonicalDatasetFileSize
	case "rkc.manifest.json":
		limit = maximumLegacyManifestFileSize
	}
	return readBoundedDatasetJSON(root, name, limit, target)
}

func (integrity datasetIntegrity) readVerifiedDerivedJSON(root, name string, maximumBytes int64, target any) error {
	if integrity.manifest == nil {
		return os.ErrNotExist
	}
	canonical, err := canonicalDatasetPath(filepath.ToSlash(name))
	if err != nil {
		return err
	}
	var expected *datasetExportFile
	for index := range integrity.manifest.Files {
		if integrity.manifest.Files[index].Path == canonical {
			expected = &integrity.manifest.Files[index]
			break
		}
	}
	if expected == nil {
		return os.ErrNotExist
	}
	if maximumBytes <= 0 || expected.Size > maximumBytes {
		return fmt.Errorf("derived file %q exceeds %d bytes", canonical, maximumBytes)
	}
	path := filepath.Join(root, filepath.FromSlash(canonical))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !datasetPathWithin(root, resolved) {
		return errors.New("derived file escapes the dataset root")
	}
	size, digest, err := decodeAndHashRegularJSON(resolved, maximumBytes, target)
	if err != nil {
		return err
	}
	if size != expected.Size || digest != expected.SHA256 {
		return errors.New("derived file changed after export verification")
	}
	return nil
}

func datasetPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func inspectDatasetIntegrity(root string) (datasetIntegrity, error) {
	integrity := datasetIntegrity{status: IntegrityLegacyUnverified}
	markerPath := filepath.Join(root, safeoutput.MarkerName)
	markerExists, err := pathExists(markerPath)
	if err != nil {
		return integrity, fmt.Errorf("inspect ownership marker: %w", err)
	}
	var marker safeoutput.Marker
	if markerExists {
		info, err := os.Lstat(markerPath)
		if err != nil {
			return integrity, fmt.Errorf("inspect ownership marker: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > maximumOwnershipMarkerFileSize {
			return integrity, errors.New("ownership marker is not a bounded regular file")
		}
		marker, err = safeoutput.ReadMarker(root)
		if err != nil {
			return integrity, fmt.Errorf("read ownership marker: %w", err)
		}
		if marker.Kind != "atlas" || strings.TrimSpace(marker.SnapshotID) == "" {
			return integrity, fmt.Errorf("ownership marker does not identify a committed atlas snapshot")
		}
	}

	exportPath := filepath.Join(root, exportManifestName)
	exportExists, err := pathExists(exportPath)
	if err != nil {
		return integrity, fmt.Errorf("inspect export manifest: %w", err)
	}
	if !exportExists {
		if markerExists {
			return integrity, errors.New("marked atlas is missing rkc-export-manifest.json")
		}
		return integrity, nil
	}

	manifest, files, err := verifyDatasetExportManifest(root, exportPath)
	if err != nil {
		return integrity, err
	}
	if markerExists && marker.SnapshotID != manifest.SnapshotID {
		return integrity, fmt.Errorf("ownership marker snapshot %q does not match export manifest snapshot %q", marker.SnapshotID, manifest.SnapshotID)
	}
	integrity.manifest = &manifest
	integrity.files = files
	if markerExists {
		integrity.status = IntegrityVerified
	} else {
		// Releases produced before atomic ownership markers remain readable, but
		// the health endpoint makes that compatibility mode explicit.
		integrity.status = IntegrityVerifiedLegacyUnmarked
	}
	return integrity, nil
}

func verifyDatasetExportManifest(root, manifestPath string) (datasetExportManifest, map[string][]byte, error) {
	var manifest datasetExportManifest
	manifestData, _, _, err := readAndHashRegularFile(manifestPath, maximumExportManifestFileSize)
	if err != nil {
		return manifest, nil, fmt.Errorf("read export manifest: %w", err)
	}
	if err := decodeStrictJSON(manifestData, &manifest); err != nil {
		return manifest, nil, fmt.Errorf("decode export manifest: %w", err)
	}
	if manifest.SchemaVersion != model.SchemaVersion || strings.TrimSpace(manifest.SnapshotID) == "" {
		return manifest, nil, errors.New("export manifest has unsupported schema or empty snapshot id")
	}
	if len(manifest.Files) > maximumDatasetFileCount {
		return manifest, nil, fmt.Errorf("export manifest file count exceeds %d", maximumDatasetFileCount)
	}

	actualFiles, err := walkDatasetFiles(root)
	if err != nil {
		return manifest, nil, err
	}
	captured := map[string][]byte{}
	seen := make(map[string]struct{}, len(manifest.Files))
	hashBuffer := make([]byte, 32*1024)
	var totalBytes, canonicalBytes, staticSiteBytes int64
	staticSiteFiles := 0
	previous := ""
	for _, record := range manifest.Files {
		clean, err := canonicalDatasetPath(record.Path)
		if err != nil {
			return manifest, nil, fmt.Errorf("unsafe export path %q: %w", record.Path, err)
		}
		if previous != "" && previous >= clean {
			return manifest, nil, errors.New("export manifest file records are duplicated or not canonically sorted")
		}
		previous = clean
		path, ok := actualFiles[clean]
		if !ok {
			return manifest, nil, fmt.Errorf("export manifest references missing file %q", clean)
		}
		if record.Size < 0 || len(record.SHA256) != sha256.Size*2 || record.SHA256 != strings.ToLower(record.SHA256) {
			return manifest, nil, fmt.Errorf("export manifest has invalid metadata for %q", clean)
		}
		if _, err := hex.DecodeString(record.SHA256); err != nil {
			return manifest, nil, fmt.Errorf("export manifest has invalid digest for %q", clean)
		}
		captureCanonical := clean == "bundle.json" || clean == "rkc.manifest.json" || clean == "coverage.json"
		staticProjection := strings.HasPrefix(clean, "site/")
		if captureCanonical && !record.Canonical {
			return manifest, nil, fmt.Errorf("required file %q is not marked canonical", clean)
		}
		captureLimit := int64(0)
		if captureCanonical {
			captureLimit = maximumCanonicalDatasetFileSize
		}
		if staticProjection {
			staticSiteFiles++
			if staticSiteFiles > maximumStaticSiteFileCount {
				return manifest, nil, fmt.Errorf("static site file count exceeds %d", maximumStaticSiteFileCount)
			}
			if record.Size > maximumStaticSiteFileSize {
				return manifest, nil, fmt.Errorf("static site file %q exceeds %d bytes", clean, maximumStaticSiteFileSize)
			}
			if record.Size > maximumStaticSiteTotalBytes-staticSiteBytes {
				return manifest, nil, fmt.Errorf("static site total exceeds %d bytes", maximumStaticSiteTotalBytes)
			}
			staticSiteBytes += record.Size
		}
		data, size, digest, err := readAndHashRegularFileWithBuffer(path, captureLimit, hashBuffer)
		if err != nil {
			return manifest, nil, fmt.Errorf("verify exported file %q: %w", clean, err)
		}
		if size != record.Size || digest != record.SHA256 {
			return manifest, nil, fmt.Errorf("exported file %q does not match its size or sha256", clean)
		}
		if captureCanonical {
			captured[clean] = data
		}
		seen[clean] = struct{}{}
		if size > maxSignedInt64-totalBytes {
			return manifest, nil, errors.New("export manifest total byte count overflows")
		}
		totalBytes += size
		if record.Canonical {
			if size > maxSignedInt64-canonicalBytes {
				return manifest, nil, errors.New("export manifest canonical byte count overflows")
			}
			canonicalBytes += size
		}
	}
	if len(seen) != len(actualFiles) {
		for name := range actualFiles {
			if _, ok := seen[name]; !ok {
				return manifest, nil, fmt.Errorf("exported file %q is absent from rkc-export-manifest.json", name)
			}
		}
	}
	for _, required := range []string{"bundle.json", "rkc.manifest.json", "coverage.json"} {
		if _, ok := captured[required]; !ok {
			return manifest, nil, fmt.Errorf("export manifest is missing required canonical file %q", required)
		}
	}
	if totalBytes != manifest.TotalBytes || canonicalBytes != manifest.CanonicalBytes {
		return manifest, nil, errors.New("export manifest byte totals do not match its file records")
	}
	canonicalRecords := make([]datasetExportFile, 0, len(manifest.Files))
	for _, record := range manifest.Files {
		if record.Canonical {
			canonicalRecords = append(canonicalRecords, record)
		}
	}
	canonicalJSON, err := json.Marshal(canonicalRecords)
	if err != nil {
		return manifest, nil, fmt.Errorf("encode canonical export records: %w", err)
	}
	canonicalSum := sha256.Sum256(canonicalJSON)
	if digest := hex.EncodeToString(canonicalSum[:]); digest != manifest.CanonicalFilesDigest {
		return manifest, nil, errors.New("canonical export manifest digest does not match its file records")
	}
	return manifest, captured, nil
}

func walkDatasetFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == exportManifestName || relative == safeoutput.MarkerName {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("export contains non-regular file %q", relative)
		}
		if len(files) >= maximumDatasetFileCount {
			return fmt.Errorf("export file count exceeds %d", maximumDatasetFileCount)
		}
		files[relative] = path
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inventory exported files: %w", err)
	}
	return files, nil
}

func canonicalDatasetPath(value string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(value))
	if value == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes dataset root")
	}
	canonical := filepath.ToSlash(clean)
	if canonical != value || canonical == exportManifestName || canonical == safeoutput.MarkerName {
		return "", errors.New("path is not a canonical exported file path")
	}
	return canonical, nil
}

func readAndHashRegularFile(path string, captureLimit int64) ([]byte, int64, string, error) {
	return readAndHashRegularFileWithBuffer(path, captureLimit, nil)
}

func readAndHashRegularFileWithBuffer(path string, captureLimit int64, copyBuffer []byte) ([]byte, int64, string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, "", err
	}
	if !before.Mode().IsRegular() {
		return nil, 0, "", errors.New("file is not regular")
	}
	if captureLimit > 0 && before.Size() > captureLimit {
		return nil, 0, "", fmt.Errorf("file exceeds capture limit %d", captureLimit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, 0, "", errors.New("file identity changed while opening")
	}
	if captureLimit > 0 && opened.Size() > captureLimit {
		return nil, 0, "", fmt.Errorf("file exceeds capture limit %d", captureLimit)
	}
	hash := sha256.New()
	var buffer bytes.Buffer
	writer := io.Writer(hash)
	reader := io.Reader(file)
	if captureLimit > 0 {
		// The size is bounded before this allocation. Reserving it once avoids
		// repeatedly copying large canonical bundles while their integrity is
		// checked. The 512 MiB canonical ceiling also fits on 32-bit targets.
		buffer.Grow(int(opened.Size()))
		writer = io.MultiWriter(hash, &buffer)
		reader = io.LimitReader(file, captureLimit+1)
	}
	if len(copyBuffer) == 0 {
		copyBuffer = make([]byte, 32*1024)
	}
	// Hide os.File.WriteTo so io.CopyBuffer uses the caller's reusable bounded
	// buffer instead of allocating one scratch region for every exported file.
	reader = struct{ io.Reader }{reader}
	size, err := io.CopyBuffer(writer, reader, copyBuffer)
	if err != nil {
		return nil, 0, "", err
	}
	if captureLimit > 0 && size > captureLimit {
		return nil, 0, "", fmt.Errorf("file exceeds capture limit %d", captureLimit)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != size {
		return nil, 0, "", errors.New("file changed while reading")
	}
	return buffer.Bytes(), size, hex.EncodeToString(hash.Sum(nil)), nil
}

// decodeAndHashRegularJSON verifies file identity and allocation-driving search
// budgets before decoding directly from the same stable descriptor. This
// avoids retaining a second raw copy of a potentially large derived index and
// prevents a compact hostile JSON shape from amplifying past the live resource
// envelope before semantic validation can run.
func decodeAndHashRegularJSON(path string, maximumBytes int64, target any) (int64, string, error) {
	if maximumBytes <= 0 {
		return 0, "", errors.New("JSON byte limit must be positive")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !before.Mode().IsRegular() || before.Size() > maximumBytes {
		return 0, "", fmt.Errorf("JSON file is not regular or exceeds %d bytes", maximumBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameDatasetFileState(before, opened) {
		return 0, "", errors.New("JSON file identity changed while opening")
	}
	if _, ok := target.(*search.Index); !ok {
		return 0, "", errors.New("streaming derived JSON preflight supports only a search index")
	}
	if err := search.PreflightPersistedIndex(file); err != nil {
		return 0, "", fmt.Errorf("preflight search index resources: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("rewind preflighted search index: %w", err)
	}
	hash := sha256.New()
	reader := io.TeeReader(io.LimitReader(file, maximumBytes+1), hash)
	if err := decodeStrictJSONReader(reader, target); err != nil {
		return 0, "", err
	}
	after, err := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !sameDatasetFileState(opened, after) || !sameDatasetFileState(after, pathAfter) {
		return 0, "", errors.New("JSON file changed while decoding")
	}
	if after.Size() > maximumBytes {
		return 0, "", fmt.Errorf("JSON file exceeds %d bytes", maximumBytes)
	}
	return after.Size(), hex.EncodeToString(hash.Sum(nil)), nil
}

func sameDatasetFileState(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) &&
		left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

const maxSignedInt64 = int64(^uint64(0) >> 1)

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func validationErrors(report model.ValidationReport) string {
	messages := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity != "error" && diagnostic.Severity != "fatal" {
			continue
		}
		messages = append(messages, diagnostic.Code+": "+diagnostic.Message)
	}
	sort.Strings(messages)
	if len(messages) > 8 {
		return strings.Join(messages[:8], "; ") + fmt.Sprintf("; and %d more", len(messages)-8)
	}
	return strings.Join(messages, "; ")
}

func loadLegacyBundle(root string) (model.Bundle, error) {
	return loadLegacyBundleWithLimits(root, legacyLoadLimits{
		manifestBytes:  maximumLegacyManifestFileSize,
		fileBytes:      maximumLegacyJSONLFileSize,
		totalBytes:     maximumLegacyJSONLTotalBytes,
		lineBytes:      maximumLegacyJSONLLineBytes,
		recordsPerFile: maximumLegacyRecordsPerFile,
		totalRecords:   maximumLegacyRecordsTotal,
	})
}

type legacyLoadLimits struct {
	manifestBytes  int64
	fileBytes      int64
	totalBytes     int64
	lineBytes      int
	recordsPerFile int
	totalRecords   int
}

type legacyLoadBudget struct {
	bytes   int64
	records int
}

func loadLegacyBundleWithLimits(root string, limits legacyLoadLimits) (model.Bundle, error) {
	var bundle model.Bundle
	manifestData, manifestSize, err := readBoundedDatasetFile(root, "rkc.manifest.json", limits.manifestBytes)
	if err != nil {
		return bundle, fmt.Errorf("load manifest: %w", err)
	}
	if manifestSize > limits.totalBytes {
		return bundle, fmt.Errorf("legacy dataset exceeds %d total bytes", limits.totalBytes)
	}
	if err := decodeStrictJSON(manifestData, &bundle.Snapshot); err != nil {
		return bundle, fmt.Errorf("load manifest: %w", err)
	}
	budget := legacyLoadBudget{bytes: manifestSize}
	files := []struct {
		name   string
		target any
	}{
		{"graph/artifacts.jsonl", &bundle.Artifacts},
		{"graph/nodes.jsonl", &bundle.Nodes},
		{"graph/edges.jsonl", &bundle.Edges},
		{"graph/evidence.jsonl", &bundle.Evidence},
		{"graph/diagnostics.jsonl", &bundle.Diagnostics},
	}
	for _, item := range files {
		var err error
		target := item.target
		switch values := target.(type) {
		case *[]model.Artifact:
			err = readLegacyJSONL(root, item.name, values, limits, &budget)
		case *[]model.Node:
			err = readLegacyJSONL(root, item.name, values, limits, &budget)
		case *[]model.Edge:
			err = readLegacyJSONL(root, item.name, values, limits, &budget)
		case *[]model.Evidence:
			err = readLegacyJSONL(root, item.name, values, limits, &budget)
		case *[]model.Diagnostic:
			err = readLegacyJSONL(root, item.name, values, limits, &budget)
		}
		if err != nil {
			return bundle, fmt.Errorf("load legacy %s: %w", item.name, err)
		}
	}
	return bundle, nil
}

// Handler returns the read-only HTTP API and static atlas handler. It never
// mounts the privileged workbench command surface.
func (dataset *Dataset) Handler() http.Handler {
	return dataset.HandlerWithWorkbench(nil)
}

// HandlerWithWorkbench adds the explicitly enabled local command surface while
// preserving the read-only Handler contract for every existing caller.
func (dataset *Dataset) HandlerWithWorkbench(workbench *Workbench) http.Handler {
	if dataset.pagination == nil {
		prepared := *dataset
		prepared.pagination = newPaginationState(dataset.Search)
		dataset = &prepared
	}
	if workbench != nil && !dataset.staticSiteTrusted {
		// Defense in depth for callers other than cmd/rkc: never mount imported
		// atlas JavaScript beside privileged APIs. The normal serve path prepares
		// assets explicitly and reports generation errors; a direct library caller
		// gets the APIs but no untrusted application shell if regeneration fails.
		trusted := *dataset
		if err := trusted.PrepareWorkbenchBrowser(); err != nil {
			trusted.staticSite = nil
			trusted.staticSiteTrusted = true
		}
		dataset = &trusted
	}
	if workbench != nil {
		workbench.attachDataset(dataset)
	}
	currentDataset := func() *Dataset {
		if workbench == nil {
			return dataset
		}
		return workbench.currentDataset(dataset)
	}
	datasetHandler := func(handler func(*Dataset, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, request *http.Request) {
			// Resolve the active immutable dataset exactly once for this response.
			// The browser binds every parallel bootstrap response to this header and
			// rejects a bundle assembled across an atomic activation boundary.
			selected := currentDataset()
			w.Header().Set(snapshotGenerationHeader, selected.Manifest.ID)
			handler(selected, w, request)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/capabilities", datasetHandler((*Dataset).handleCapabilities))
	mux.HandleFunc("GET /api/v1/context", datasetHandler((*Dataset).handleContext))
	mux.HandleFunc("GET /api/v1/health", datasetHandler((*Dataset).handleHealth))
	mux.HandleFunc("GET /api/v1/manifest", datasetHandler((*Dataset).handleManifest))
	mux.HandleFunc("GET /api/v1/coverage", datasetHandler((*Dataset).handleCoverage))
	mux.HandleFunc("GET /api/v1/facets", datasetHandler((*Dataset).handleFacets))
	mux.HandleFunc("GET /api/v1/artifacts", datasetHandler((*Dataset).handleArtifacts))
	mux.HandleFunc("GET /api/v1/artifacts/{artifactID}", datasetHandler((*Dataset).handleArtifact))
	mux.HandleFunc("GET /api/v1/nodes", datasetHandler((*Dataset).handleNodes))
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", datasetHandler((*Dataset).handleNode))
	mux.HandleFunc("GET /api/v1/edges", datasetHandler((*Dataset).handleEdges))
	mux.HandleFunc("GET /api/v1/evidence/{evidenceID}", datasetHandler((*Dataset).handleEvidence))
	mux.HandleFunc("GET /api/v1/diagnostics", datasetHandler((*Dataset).handleDiagnostics))
	mux.HandleFunc("GET /api/v1/search", datasetHandler((*Dataset).handleSearch))
	mux.HandleFunc("GET /api/v1/graph/neighborhood", datasetHandler((*Dataset).handleNeighborhood))
	mux.HandleFunc("GET /api/v1/graph/path", datasetHandler((*Dataset).handlePath))
	mux.HandleFunc("GET /api/v1/graph/components", datasetHandler((*Dataset).handleComponents))
	mux.HandleFunc("GET /api/v1/impact", datasetHandler((*Dataset).handleImpact))
	if workbench != nil {
		mux.HandleFunc("GET /api/v1/workbench/session", workbench.handleSession)
		mux.HandleFunc("GET /api/v1/workbench/directories", workbench.handleDirectories)
		mux.HandleFunc("POST /api/v1/workbench/jobs", workbench.handleJobs)
		mux.HandleFunc("GET /api/v1/workbench/jobs/{jobID}", workbench.handleJob)
		mux.HandleFunc("DELETE /api/v1/workbench/jobs/{jobID}", workbench.handleCancelJob)
	}
	mux.HandleFunc("/", datasetHandler((*Dataset).handleStaticSite))
	return securityHeaders(mux, workbench != nil)
}

func (dataset *Dataset) handleStaticSite(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeProblem(w, http.StatusMethodNotAllowed, "Method not allowed", "static site supports GET and HEAD only")
		return
	}
	requested := strings.TrimPrefix(pathpkg.Clean("/"+request.URL.Path), "/")
	if requested == "api" || strings.HasPrefix(requested, "api/") {
		http.NotFound(w, request)
		return
	}
	if requested == "." || requested == "" {
		requested = "index.html"
	}
	asset, ok := dataset.staticSite[requested]
	if !ok && strings.HasSuffix(request.URL.Path, "/") {
		asset, ok = dataset.staticSite[strings.TrimSuffix(requested, "/")+"/index.html"]
	}
	if !ok && pathpkg.Ext(requested) == "" {
		// Client-side routes are backed by the verified, in-memory application
		// shell. Missing asset paths with an extension still return 404.
		asset, ok = dataset.staticSite["index.html"]
		requested = "index.html"
	}
	if !ok {
		http.NotFound(w, request)
		return
	}
	if contentType := mime.TypeByExtension(pathpkg.Ext(requested)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("ETag", `"sha256-`+asset.digest+`"`)
	http.ServeContent(w, request, requested, dataset.LoadedAt, bytes.NewReader(asset.data))
}

func (dataset *Dataset) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "schema_version": dataset.Manifest.SchemaVersion, "snapshot_id": dataset.Manifest.ID,
		"loaded_at": dataset.LoadedAt, "nodes": len(dataset.Bundle.Nodes), "edges": len(dataset.Bundle.Edges),
		"search_index_version": dataset.Search.Version, "integrity": dataset.Integrity,
	})
}
func (dataset *Dataset) handleManifest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dataset.Manifest)
}
func (dataset *Dataset) handleCoverage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dataset.Coverage)
}

func (dataset *Dataset) handleFacets(w http.ResponseWriter, _ *http.Request) {
	languages := map[string]int{}
	nodeKinds := map[string]int{}
	resolutions := map[string]int{}
	diagnostics := map[string]int{}
	for _, artifact := range dataset.Bundle.Artifacts {
		if artifact.Language != "" {
			languages[artifact.Language]++
		}
	}
	for _, node := range dataset.Bundle.Nodes {
		nodeKinds[node.Kind]++
	}
	for _, edge := range dataset.Bundle.Edges {
		resolutions[edge.Resolution]++
	}
	for _, diagnostic := range dataset.Bundle.Diagnostics {
		diagnostics[diagnostic.Severity]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"languages": languages, "node_kinds": nodeKinds,
		"edge_resolutions": resolutions, "diagnostics": diagnostics,
	})
}

func (dataset *Dataset) handleArtifacts(w http.ResponseWriter, request *http.Request) {
	parsed, err := dataset.parsePageRequest(request, "collection", "language", "status", "path_prefix")
	if err != nil {
		writePageError(w, err)
		return
	}
	language, status, pathPrefix := parsed.values.Get("language"), parsed.values.Get("status"), parsed.values.Get("path_prefix")
	page, err := collectionPage(dataset, request, parsed, dataset.Bundle.Artifacts, parseLimit(request, 100, 5000), func(artifact model.Artifact) bool {
		return (language == "" || artifact.Language == language) && (status == "" || artifact.Status == status) && (pathPrefix == "" || strings.HasPrefix(artifact.Path, pathPrefix))
	})
	if err != nil {
		writePageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (dataset *Dataset) handleArtifact(w http.ResponseWriter, request *http.Request) {
	artifact, ok := dataset.ArtifactByID[request.PathValue("artifactID")]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Artifact not found", request.PathValue("artifactID"))
		return
	}
	var nodes []model.Node
	for _, node := range dataset.Bundle.Nodes {
		if node.ArtifactID == artifact.ID {
			nodes = append(nodes, node)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": artifact, "nodes": nodes})
}

func (dataset *Dataset) handleNodes(w http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	mode := "collection"
	if query != "" {
		mode = "ranked"
	}
	parsed, err := dataset.parsePageRequest(request, mode, "q", "kind", "language")
	if err != nil {
		writePageError(w, err)
		return
	}
	kind, language := parsed.values.Get("kind"), parsed.values.Get("language")
	limit := parseLimit(request, 100, 1000)
	if query != "" {
		response, err := dataset.rankedPage(request, parsed, search.Query{Text: query, Kinds: optionalSet(kind), Languages: optionalSet(language), ObjectTypes: map[string]struct{}{"node": {}}, Limit: limit})
		if err != nil {
			writePageError(w, err)
			return
		}
		items := make([]model.Node, 0, len(response.Hits))
		for _, hit := range response.Hits {
			if node, ok := dataset.NodeByID[hit.Document.ID]; ok {
				items = append(items, node)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": response.Total, "truncated": response.Truncated, "snapshot_id": response.SnapshotID, "next_cursor": response.NextCursor, "retrieval": response.Response})
		return
	}
	page, err := collectionPage(dataset, request, parsed, dataset.Bundle.Nodes, limit, func(node model.Node) bool {
		return (kind == "" || node.Kind == kind) && (language == "" || node.Language == language)
	})
	if err != nil {
		writePageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (dataset *Dataset) handleNode(w http.ResponseWriter, request *http.Request) {
	id := request.PathValue("nodeID")
	node, ok := dataset.NodeByID[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Node not found", id)
		return
	}
	var evidence []model.Evidence
	for _, evidenceID := range node.EvidenceIDs {
		if item, ok := dataset.EvidenceByID[evidenceID]; ok {
			evidence = append(evidence, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": node, "incoming_edges": dataset.Graph.Incoming[id], "outgoing_edges": dataset.Graph.Outgoing[id], "evidence": evidence,
	})
}

func (dataset *Dataset) handleEdges(w http.ResponseWriter, request *http.Request) {
	parsed, err := dataset.parsePageRequest(request, "collection", "kind", "from", "to", "resolution")
	if err != nil {
		writePageError(w, err)
		return
	}
	kind, from, to, resolution := parsed.values.Get("kind"), parsed.values.Get("from"), parsed.values.Get("to"), parsed.values.Get("resolution")
	page, err := collectionPage(dataset, request, parsed, dataset.Bundle.Edges, parseLimit(request, 100, 5000), func(edge model.Edge) bool {
		return (kind == "" || edge.Kind == kind) && (from == "" || edge.From == from) && (to == "" || edge.To == to) && (resolution == "" || model.NormalizeResolution(edge.Resolution) == model.NormalizeResolution(resolution))
	})
	if err != nil {
		writePageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (dataset *Dataset) handleEvidence(w http.ResponseWriter, request *http.Request) {
	evidence, ok := dataset.EvidenceByID[request.PathValue("evidenceID")]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Evidence not found", request.PathValue("evidenceID"))
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

func (dataset *Dataset) handleDiagnostics(w http.ResponseWriter, request *http.Request) {
	parsed, err := dataset.parsePageRequest(request, "collection", "severity", "code")
	if err != nil {
		writePageError(w, err)
		return
	}
	severity, code := parsed.values.Get("severity"), parsed.values.Get("code")
	page, err := collectionPage(dataset, request, parsed, dataset.Bundle.Diagnostics, parseLimit(request, 100, 5000), func(diagnostic model.Diagnostic) bool {
		return (severity == "" || diagnostic.Severity == severity) && (code == "" || diagnostic.Code == code)
	})
	if err != nil {
		writePageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (dataset *Dataset) handleSearch(w http.ResponseWriter, request *http.Request) {
	parsed, err := dataset.parsePageRequest(request, "ranked", "q", "kinds", "kind", "languages", "language", "object_types", "type", "path_prefix")
	if err != nil {
		writePageError(w, err)
		return
	}
	query := strings.TrimSpace(parsed.values.Get("q"))
	if query == "" {
		writeProblem(w, http.StatusBadRequest, "Missing query", "q is required")
		return
	}
	kinds := firstQuery(request, "kinds", "kind")
	languages := firstQuery(request, "languages", "language")
	objectTypes := firstQuery(request, "object_types", "type")
	response, err := dataset.rankedPage(request, parsed, search.Query{
		Text: query, Kinds: setFromCSV(kinds), Languages: setFromCSV(languages),
		ObjectTypes: setFromCSV(objectTypes), PathPrefix: parsed.values.Get("path_prefix"), Limit: parseLimit(request, 50, 1000),
	})
	if err != nil {
		writePageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (dataset *Dataset) handleNeighborhood(w http.ResponseWriter, request *http.Request) {
	seed := request.URL.Query().Get("node_id")
	if seed == "" {
		writeProblem(w, http.StatusBadRequest, "Missing node", "node_id is required")
		return
	}
	result, err := dataset.Graph.Neighborhood(seed, traversalOptions(request, 1, 500))
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Seed node not found", seed)
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Graph query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handlePath(w http.ResponseWriter, request *http.Request) {
	from := request.URL.Query().Get("from")
	to := request.URL.Query().Get("to")
	if from == "" || to == "" {
		writeProblem(w, http.StatusBadRequest, "Missing endpoints", "from and to are required")
		return
	}
	result, err := dataset.Graph.ShortestPath(from, to, traversalOptions(request, 8, 10000))
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Path endpoint not found", err.Error())
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Path query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handleImpact(w http.ResponseWriter, request *http.Request) {
	seed := request.URL.Query().Get("node_id")
	if seed == "" {
		writeProblem(w, http.StatusBadRequest, "Missing node", "node_id is required")
		return
	}
	options := traversalOptions(request, 4, 5000)
	if request.URL.Query().Get("direction") == "" {
		options.Direction = graphindex.DirectionIncoming
	}
	result, err := dataset.Graph.Impact(seed, options)
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Seed node not found", seed)
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Impact query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handleComponents(w http.ResponseWriter, request *http.Request) {
	components := dataset.Graph.StronglyConnectedComponents(setFromCSV(request.URL.Query().Get("edge_kinds")))
	cyclicOnly := firstQuery(request, "cyclic", "cycles_only") == "true"
	limit := parseLimit(request, 100, 10000)
	var items []graphindex.Component
	matched := 0
	for _, component := range components {
		if cyclicOnly && !component.Cyclic {
			continue
		}
		matched++
		if len(items) < limit {
			items = append(items, component)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": matched, "truncated": matched > len(items)})
}

func traversalOptions(request *http.Request, defaultDepth, defaultNodes int) graphindex.TraverseOptions {
	direction := graphindex.Direction(request.URL.Query().Get("direction"))
	switch direction {
	case graphindex.DirectionIncoming, graphindex.DirectionOutgoing, graphindex.DirectionBoth:
	default:
		direction = graphindex.DirectionBoth
	}
	return graphindex.TraverseOptions{
		Direction: direction, EdgeKinds: setFromCSV(request.URL.Query().Get("edge_kinds")), Resolutions: setFromCSV(request.URL.Query().Get("resolutions")),
		MaxDepth:          parseBoundedInt(request.URL.Query().Get("max_depth"), parseBoundedInt(request.URL.Query().Get("hops"), defaultDepth, 0, 64), 0, 64),
		MaxNodes:          parseBoundedInt(request.URL.Query().Get("max_nodes"), parseLimit(request, defaultNodes, 100000), 1, 100000),
		IncludeUnresolved: request.URL.Query().Get("include_unresolved") == "true",
	}
}

func readBoundedDatasetJSON(root, name string, limit int64, target any) error {
	data, _, err := readBoundedDatasetFile(root, name, limit)
	if err != nil {
		return err
	}
	return decodeStrictJSON(data, target)
}

func readBoundedDatasetFile(root, name string, limit int64) ([]byte, int64, error) {
	file, opened, err := openBoundedDatasetFile(root, name, limit)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("file exceeds %d bytes", limit)
	}
	if err := verifyBoundedDatasetFile(root, name, file, opened, int64(len(data))); err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

func openBoundedDatasetFile(root, name string, limit int64) (*os.File, os.FileInfo, error) {
	if limit <= 0 || limit == maxSignedInt64 {
		return nil, nil, errors.New("file byte limit must be positive and bounded")
	}
	canonical, err := canonicalLegacyDatasetPath(name)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureNoSymlinkComponents(root, canonical); err != nil {
		return nil, nil, err
	}
	fullPath := filepath.Join(root, filepath.FromSlash(canonical))
	before, err := os.Lstat(fullPath)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("dataset file is not regular")
	}
	if before.Size() > limit {
		return nil, nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, errors.New("dataset file identity changed while opening")
	}
	if opened.Size() > limit {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return file, opened, nil
}

func verifyBoundedDatasetFile(root, name string, file *os.File, opened os.FileInfo, bytesRead int64) error {
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != bytesRead {
		return errors.New("dataset file changed while reading")
	}
	return ensureNoSymlinkComponents(root, name)
}

func canonicalLegacyDatasetPath(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	canonical := filepath.ToSlash(clean)
	if name == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || canonical != name {
		return "", errors.New("dataset path is not a canonical relative path")
	}
	return canonical, nil
}

func ensureNoSymlinkComponents(root, name string) error {
	canonical, err := canonicalLegacyDatasetPath(name)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("dataset root is not a non-symlink directory")
	}
	parts := strings.Split(canonical, "/")
	current := root
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("dataset path component %q is a symlink", strings.Join(parts[:index+1], "/"))
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("dataset path component %q is not a directory", strings.Join(parts[:index+1], "/"))
		}
	}
	return nil
}

func readLegacyJSONL[T any](root, name string, target *[]T, limits legacyLoadLimits, budget *legacyLoadBudget) error {
	maxInt := int(^uint(0) >> 1)
	if limits.fileBytes <= 0 || limits.totalBytes <= 0 || limits.lineBytes <= 0 || limits.lineBytes == maxInt || limits.recordsPerFile <= 0 || limits.totalRecords <= 0 || budget == nil {
		return errors.New("legacy JSONL limits must be positive and bounded")
	}
	file, opened, err := openBoundedDatasetFile(root, name, limits.fileBytes)
	if err != nil {
		return err
	}
	defer file.Close()
	if budget.bytes > limits.totalBytes || opened.Size() > limits.totalBytes-budget.bytes {
		return fmt.Errorf("legacy dataset exceeds %d total bytes", limits.totalBytes)
	}
	bufferSize := 64 * 1024
	if limits.lineBytes < bufferSize {
		bufferSize = limits.lineBytes
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, bufferSize), limits.lineBytes+1)
	records := 0
	for scanner.Scan() {
		if len(scanner.Bytes()) > limits.lineBytes {
			return fmt.Errorf("JSONL line exceeds %d bytes", limits.lineBytes)
		}
		if records >= limits.recordsPerFile {
			return fmt.Errorf("JSONL record count exceeds %d", limits.recordsPerFile)
		}
		if budget.records >= limits.totalRecords {
			return fmt.Errorf("legacy dataset record count exceeds %d", limits.totalRecords)
		}
		var value T
		if err := decodeStrictJSON(scanner.Bytes(), &value); err != nil {
			return fmt.Errorf("decode record %d: %w", records+1, err)
		}
		*target = append(*target, value)
		records++
		budget.records++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan bounded JSONL: %w", err)
	}
	if err := verifyBoundedDatasetFile(root, name, file, opened, opened.Size()); err != nil {
		return err
	}
	budget.bytes += opened.Size()
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	return decodeStrictJSONReader(bytes.NewReader(data), target)
}

func decodeStrictJSONReader(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not permitted")
		}
		return err
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(value)
}
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail})
}
func parseLimit(request *http.Request, fallback, maximum int) int {
	return parseBoundedInt(request.URL.Query().Get("limit"), fallback, 1, maximum)
}
func parseBoundedInt(value string, fallback, minimum, maximum int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		return fallback
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}
func setFromCSV(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
func optionalSet(value string) map[string]struct{} {
	if value == "" {
		return nil
	}
	return map[string]struct{}{value: {}}
}
func securityHeaders(next http.Handler, noStoreOrigin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; worker-src 'none'; manifest-src 'none'; form-action 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		// A workbench origin is privileged. Do not let a browser reuse any page,
		// script, or other asset that it cached while the same loopback address
		// was serving an imported read-only atlas. API responses remain
		// non-cacheable for the portable read-only handler as well.
		if noStoreOrigin || request.URL.Path == "/api" || strings.HasPrefix(request.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, request)
	})
}

// ErrDatasetNotFound reports that no generated atlas exists at the requested
// output root or recognized parent layout.
var ErrDatasetNotFound = errors.New("generated RKC dataset not found")

func firstQuery(request *http.Request, names ...string) string {
	for _, name := range names {
		if value := request.URL.Query().Get(name); value != "" {
			return value
		}
	}
	return ""
}
