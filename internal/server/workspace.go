package server

import (
	"time"

	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/search"
)

// NewWorkspaceDataset creates the trusted empty browser shell used before a
// user selects a source. It does not read, scan, or publish a repository and
// carries no claim of analysis or export integrity. A completed source job
// replaces it through the normal immutable-dataset activation boundary.
func NewWorkspaceDataset() (*Dataset, error) {
	snapshot := model.Snapshot{SchemaVersion: model.SchemaVersion, ID: "rkc:workspace:empty", RootName: "Your workspace", Metadata: map[string]string{"rkc_workspace": "empty"}}
	bundle := model.Bundle{Snapshot: snapshot}
	dataset := &Dataset{
		Manifest: snapshot, Bundle: bundle, Coverage: model.BuildCoverage(bundle),
		NodeByID: map[string]model.Node{}, ArtifactByID: map[string]model.Artifact{}, EvidenceByID: map[string]model.Evidence{},
		Graph: graphindex.Build(nil, nil), Search: search.BuildFromBundle(bundle), Integrity: "workspace", LoadedAt: time.Now().UTC(),
	}
	dataset.pagination = newPaginationState(dataset.Search)
	if err := dataset.PrepareWorkbenchBrowser(); err != nil {
		return nil, err
	}
	return dataset, nil
}
