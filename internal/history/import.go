package history

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// ImportStats reports what a compiled history contributed to the bundle.
type ImportStats struct {
	SymbolsTouched  int `json:"symbols_touched"`
	SupersedesEdges int `json:"supersedes_edges"`
	CommitsBound    int `json:"commits_bound"`
	EvidenceRecords int `json:"evidence_records"`
}

// Import applies a compiled history to a bundle: matching symbol nodes gain
// explicitly observed lifecycle attributes and conservative unique rename
// candidates become supersedes edges when both endpoints exist. Matching uses the
// language, kind, and qualified name recorded by the syntax extractors, so a
// symbol that moved packages appears as a new lifecycle rather than a forged
// identity.
func Import(ctx context.Context, bundle *rkcmodel.Bundle, compiled History) (ImportStats, error) {
	if ctx == nil {
		return ImportStats{}, errors.New("history import context is required")
	}
	if bundle == nil {
		return ImportStats{}, errors.New("history import bundle is required")
	}
	if compiled.SchemaVersion != SchemaVersion {
		return ImportStats{}, fmt.Errorf("history schema_version %q is unsupported", compiled.SchemaVersion)
	}
	if err := validateCompiledHistory(compiled); err != nil {
		return ImportStats{}, fmt.Errorf("invalid compiled history: %w", err)
	}
	if err := validateBundleAffinity(bundle, compiled); err != nil {
		return ImportStats{}, fmt.Errorf("history source affinity: %w", err)
	}
	stats := ImportStats{CommitsBound: len(compiled.Commits)}
	// Index compiled lifecycles by language+kind+qualified name.
	byKey := map[string]SymbolHistory{}
	for _, symbol := range compiled.Symbols {
		key := lifecycleKey(symbol.Language, symbol.Kind, symbol.QualifiedName)
		byKey[key] = symbol
	}
	existingEdges := map[string]struct{}{}
	for _, edge := range bundle.Edges {
		existingEdges[edge.ID] = struct{}{}
	}
	existingEvidence := map[string]struct{}{}
	for _, evidence := range bundle.Evidence {
		existingEvidence[evidence.ID] = struct{}{}
	}
	for index := range bundle.Nodes {
		if err := ctx.Err(); err != nil {
			return ImportStats{}, err
		}
		node := &bundle.Nodes[index]
		lifecycle, ok := byKey[lifecycleKey(node.Language, node.Kind, node.QualifiedName)]
		if !ok {
			continue
		}
		if node.Attributes == nil {
			node.Attributes = map[string]any{}
		}
		node.Attributes["history_first_observed_commit"] = lifecycle.FirstObserved
		node.Attributes["history_last_observed_commit"] = lifecycle.LastObserved
		node.Attributes["history_touched_commits"] = len(lifecycle.CommitsTouching)
		node.Attributes["history_window_truncated"] = compiled.WindowTruncated
		node.Attributes["history_source_id"] = compiled.SourceID
		node.Attributes["history_repository_id"] = compiled.RepositoryID
		node.Attributes["history_source_revision"] = compiled.SourceRevision
		node.Attributes["history_revision_policy"] = compiled.RevisionPolicy
		node.Attributes["history_ancestry_policy"] = compiled.AncestryPolicy
		if len(lifecycle.Files) > 0 {
			node.Attributes["history_files"] = lifecycle.Files
		}
		evidenceID := rkcmodel.StableID(
			"evidence", PluginID, PluginVersion, compiled.SourceID, lifecycle.ID, "observed-lifecycle",
		)
		if appendHistoryEvidence(bundle, existingEvidence, evidenceID, rkcmodel.Evidence{
			ID: evidenceID, Kind: "syntax_inferred", Method: "history.first_parent.interface_delta",
			Confidence: 1, Tool: PluginID, ToolVersion: PluginVersion,
			InputDigest: compiled.SourceID,
			Detail:      "bounded first-parent symbol interface observations",
			Attributes: map[string]any{
				"first_observed_commit": lifecycle.FirstObserved,
				"last_observed_commit":  lifecycle.LastObserved,
				"language":              lifecycle.Language,
				"window_truncated":      compiled.WindowTruncated,
				"history_source_id":     compiled.SourceID,
			},
		}) {
			stats.EvidenceRecords++
		}
		if !containsString(node.EvidenceIDs, evidenceID) {
			node.EvidenceIDs = append(node.EvidenceIDs, evidenceID)
			sort.Strings(node.EvidenceIDs)
		}
		stats.SymbolsTouched++
	}
	// Supersedes edges for conservative rename pairs.
	nodeByKey := map[string]string{}
	for _, node := range bundle.Nodes {
		nodeByKey[lifecycleKey(node.Language, node.Kind, node.QualifiedName)] = node.ID
	}
	for _, refactor := range compiled.Refactors {
		fromID, fromOK := nodeByKey[lifecycleKey(refactor.Language, refactor.Kind, refactor.QualifiedFrom)]
		toID, toOK := nodeByKey[lifecycleKey(refactor.Language, refactor.Kind, refactor.QualifiedTo)]
		if !fromOK || !toOK || fromID == toID {
			continue
		}
		edgeID := rkcmodel.StableID("edge", "supersedes", fromID, toID)
		if _, exists := existingEdges[edgeID]; exists {
			continue
		}
		existingEdges[edgeID] = struct{}{}
		evidenceID := rkcmodel.StableID(
			"evidence", PluginID, PluginVersion, compiled.SourceID, refactor.Commit, refactor.Language,
			refactor.Kind, refactor.QualifiedFrom, refactor.QualifiedTo,
		)
		if appendHistoryEvidence(bundle, existingEvidence, evidenceID, rkcmodel.Evidence{
			ID: evidenceID, Kind: "syntax_inferred", Method: "history.unique_signature_pair",
			Confidence: 0.7, Tool: PluginID, ToolVersion: PluginVersion,
			InputDigest: compiled.SourceID,
			Detail:      "unique same-signature removal and addition observed in one commit",
			Attributes: map[string]any{
				"commit": refactor.Commit, "kind": refactor.Kind, "language": refactor.Language,
				"history_source_id": compiled.SourceID,
			},
		}) {
			stats.EvidenceRecords++
		}
		bundle.Edges = append(bundle.Edges, rkcmodel.Edge{
			ID: edgeID, Kind: "supersedes", From: fromID, To: toID,
			Resolution: "syntax_inferred", Confidence: 0.7,
			Producer:    PluginID,
			EvidenceIDs: []string{evidenceID},
			Attributes: map[string]any{
				"commit": refactor.Commit, "kind": refactor.Kind, "language": refactor.Language,
				"history_source_id":      compiled.SourceID,
				"history_plugin_version": PluginVersion,
			},
		})
		stats.SupersedesEdges++
	}
	sort.Slice(bundle.Edges, func(i, j int) bool { return bundle.Edges[i].ID < bundle.Edges[j].ID })
	sort.Slice(bundle.Evidence, func(i, j int) bool { return bundle.Evidence[i].ID < bundle.Evidence[j].ID })
	return stats, nil
}

// validateBundleAffinity proves that a portable compiled history describes the
// exact clean commit represented by the target bundle. Ancestor-only imports
// fail closed because a bundle does not contain enough Git state to prove that
// relationship without consulting mutable external repository state.
func validateBundleAffinity(bundle *rkcmodel.Bundle, compiled History) error {
	snapshot := bundle.Snapshot
	if snapshot.Git.Unavailable || !validGitObjectID(snapshot.Git.Commit) {
		return errors.New("the target bundle has no valid Git revision")
	}
	if snapshot.Git.Dirty {
		return errors.New("the target bundle is not a clean exact-head snapshot")
	}
	if snapshot.RepositoryID == "" || snapshot.RepositoryID != compiled.RepositoryID {
		return errors.New("the target bundle belongs to a different repository identity")
	}
	if snapshot.Git.Commit != compiled.SourceRevision {
		return errors.New("the target bundle does not represent the compiled exact head")
	}

	if compiled.SourceReference != "" {
		if snapshot.Git.Origin != compiled.SourceReference ||
			snapshot.Metadata["source_reference"] != compiled.SourceReference {
			return errors.New("the target bundle source reference does not match the compiled history")
		}
		if snapshot.RepositoryID != rkcmodel.StableID("repository", compiled.SourceReference) {
			return errors.New("the target bundle repository identity is not bound to its source reference")
		}
		return nil
	}

	if snapshot.Git.Origin != "" || snapshot.Metadata["source_reference"] != "" {
		return errors.New("the target bundle has source provenance absent from the compiled history")
	}
	if snapshot.RootName != compiled.Repository ||
		snapshot.RepositoryID != rkcmodel.StableID("repository", compiled.Repository) {
		return errors.New("the target bundle repository label does not match the compiled history")
	}
	return nil
}

func appendHistoryEvidence(
	bundle *rkcmodel.Bundle,
	existing map[string]struct{},
	id string,
	evidence rkcmodel.Evidence,
) bool {
	if _, duplicate := existing[id]; duplicate {
		return false
	}
	existing[id] = struct{}{}
	bundle.Evidence = append(bundle.Evidence, evidence)
	return true
}

func lifecycleKey(language, kind, qualified string) string {
	return language + "\x00" + kind + "\x00" + qualified
}
