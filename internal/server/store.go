package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

// IntegrityVerifiedStore identifies a dataset reconstructed from the
// authenticated canonical records of a SnapshotReader rather than an atlas
// export manifest.
const IntegrityVerifiedStore = "verified_store"

// LoadStore reconstructs the server's immutable indexes from a committed
// canonical store snapshot. No persisted projection or search index is trusted
// independently of the validated bundle.
func LoadStore(ctx context.Context, reader rkcstore.SnapshotReader, snapshotID rkcstore.SnapshotID) (*Dataset, error) {
	if ctx == nil {
		return nil, errors.New("load store: context is required")
	}
	if reader == nil || reflect.ValueOf(reader).Kind() == reflect.Ptr && reflect.ValueOf(reader).IsNil() {
		return nil, errors.New("load store: snapshot reader is required")
	}
	if snapshotID == "" {
		return nil, errors.New("load store: snapshot ID is required")
	}
	bundle, err := reader.Bundle(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load store bundle: %w", err)
	}
	if bundle.Snapshot.ID != string(snapshotID) {
		return nil, fmt.Errorf("load store: bundle snapshot %q does not match requested snapshot %q", bundle.Snapshot.ID, snapshotID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !storeBundleHasCanonicalShape(bundle) {
		return nil, errors.New("load store: canonical bundle is not in canonical form")
	}
	validation := model.ValidateBundle(bundle, model.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if validation.HasErrors() {
		return nil, fmt.Errorf("load store: validate bundle: %s", validationErrors(validation))
	}
	coverage, err := reader.Coverage(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load store coverage: %w", err)
	}
	if coverage.SnapshotID != string(snapshotID) {
		return nil, errors.New("load store: coverage snapshot does not match the requested snapshot")
	}
	expectedCoverage := model.BuildCoverage(bundle)
	if !reflect.DeepEqual(coverage, expectedCoverage) {
		return nil, errors.New("load store: coverage does not match the canonical bundle")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	graph := graphindex.Build(bundle.Nodes, bundle.Edges)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	searchIndex := search.BuildFromBundle(bundle)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	browserAssets, err := rkcexport.BrowserAssets(bundle, coverage)
	if err != nil {
		return nil, fmt.Errorf("load store browser: %w", err)
	}
	staticSite, err := captureGeneratedBrowser(browserAssets)
	if err != nil {
		return nil, fmt.Errorf("load store browser: %w", err)
	}

	dataset := &Dataset{
		Manifest:     bundle.Snapshot,
		Coverage:     coverage,
		Bundle:       bundle,
		NodeByID:     make(map[string]model.Node, len(bundle.Nodes)),
		ArtifactByID: make(map[string]model.Artifact, len(bundle.Artifacts)),
		EvidenceByID: make(map[string]model.Evidence, len(bundle.Evidence)),
		Graph:        graph,
		Search:       searchIndex,
		Integrity:    IntegrityVerifiedStore,
		LoadedAt:     time.Now().UTC(),
		staticSite:   staticSite,
	}
	for _, node := range bundle.Nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, artifact := range bundle.Artifacts {
		dataset.ArtifactByID[artifact.ID] = artifact
	}
	for _, evidence := range bundle.Evidence {
		dataset.EvidenceByID[evidence.ID] = evidence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return dataset, nil
}

func captureGeneratedBrowser(files map[string][]byte) (map[string]staticSiteAsset, error) {
	if len(files) == 0 || len(files) > maximumStaticSiteFileCount {
		return nil, errors.New("generated browser has an invalid file count")
	}
	assets := make(map[string]staticSiteAsset, len(files))
	var total int64
	for name, data := range files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
		if clean != name || clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(name) {
			return nil, fmt.Errorf("generated browser has unsafe path %q", name)
		}
		size := int64(len(data))
		if size > maximumStaticSiteFileSize || size > maximumStaticSiteTotalBytes-total {
			return nil, fmt.Errorf("generated browser exceeds static-site limits at %q", name)
		}
		total += size
		sum := sha256.Sum256(data)
		assets[name] = staticSiteAsset{data: append([]byte(nil), data...), digest: hex.EncodeToString(sum[:])}
	}
	return assets, nil
}

func storeBundleHasCanonicalShape(bundle model.Bundle) bool {
	expected := model.CanonicalBundle(bundle)
	actual := bundle
	actual.Snapshot.CreatedAt = time.Time{}
	actual.Snapshot.RootPath = ""
	if bundle.Snapshot.Metadata != nil {
		actual.Snapshot.Metadata = make(map[string]string, len(bundle.Snapshot.Metadata))
		for key, value := range bundle.Snapshot.Metadata {
			actual.Snapshot.Metadata[key] = value
		}
		delete(actual.Snapshot.Metadata, "host")
		delete(actual.Snapshot.Metadata, "pid")
		delete(actual.Snapshot.Metadata, "duration_ms")
	}
	return reflect.DeepEqual(actual, expected)
}
