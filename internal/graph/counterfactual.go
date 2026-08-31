package graph

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	maximumCounterfactualSuppressions = 64
	// CounterfactualSchemaVersion identifies the additive bounded-search truth
	// metadata and search_truncated outcome contract.
	CounterfactualSchemaVersion = "1.1"
)

// ErrInvalidCounterfactual reports an ill-formed or unbounded structural
// counterfactual request.
var ErrInvalidCounterfactual = errors.New("invalid structural counterfactual")

// CounterfactualRequest asks whether a bounded graph route still exists after
// specified canonical nodes or edges are omitted from a derived traversal
// view. It does not mutate or contradict the canonical atlas.
type CounterfactualRequest struct {
	FromID         string
	ToID           string
	WithoutNodeIDs []string
	WithoutEdgeIDs []string
	Traversal      TraverseOptions
}

// CounterfactualAssumption records the exact hypothetical intervention. IDs
// are canonical graph identities and are sorted for deterministic output.
type CounterfactualAssumption struct {
	WithoutNodeIDs []string `json:"without_node_ids,omitempty"`
	WithoutEdgeIDs []string `json:"without_edge_ids,omitempty"`
}

// CounterfactualScope states the traversal boundary. A missing route is only
// a result within this scope; it is never promoted to proof of runtime
// impossibility or business causation.
type CounterfactualScope struct {
	Direction         Direction `json:"direction"`
	MaximumDepth      int       `json:"maximum_depth"`
	MaximumNodes      int       `json:"maximum_nodes"`
	IncludeUnresolved bool      `json:"include_unresolved"`
	EdgeKinds         []string  `json:"edge_kinds,omitempty"`
	Resolutions       []string  `json:"resolutions,omitempty"`
}

// CounterfactualResult compares the canonical baseline route with the route
// in a non-authoritative derived view. Outcome is one of search_truncated,
// baseline_not_found, no_effect, rerouted, or no_route_found. A no-route
// outcome is emitted only after an exhaustive filtered search; reaching either
// search cap produces search_truncated instead.
type CounterfactualResult struct {
	SchemaVersion  string                   `json:"schema_version"`
	SnapshotID     string                   `json:"snapshot_id,omitempty"`
	Analysis       string                   `json:"analysis"`
	Authoritative  bool                     `json:"authoritative"`
	Outcome        string                   `json:"outcome"`
	Statement      string                   `json:"statement"`
	Assumption     CounterfactualAssumption `json:"assumption"`
	Scope          CounterfactualScope      `json:"scope"`
	Baseline       Path                     `json:"baseline"`
	Counterfactual Path                     `json:"counterfactual"`
	EvidenceIDs    []string                 `json:"evidence_ids,omitempty"`
}

// CounterfactualPath performs two bounded shortest-path searches: the
// canonical baseline, then a derived view with the requested facts omitted.
// The result is a structural hypothesis over recorded edges, not a claim that
// disabling code in a live system would have the same effect.
func (index *Index) CounterfactualPath(request CounterfactualRequest) (CounterfactualResult, error) {
	if index == nil {
		return CounterfactualResult{}, fmt.Errorf("%w: graph index is required", ErrInvalidCounterfactual)
	}
	if request.FromID == "" || request.ToID == "" {
		return CounterfactualResult{}, fmt.Errorf("%w: from and to node IDs are required", ErrInvalidCounterfactual)
	}
	if len(request.WithoutNodeIDs)+len(request.WithoutEdgeIDs) == 0 {
		return CounterfactualResult{}, fmt.Errorf("%w: at least one node or edge intervention is required", ErrInvalidCounterfactual)
	}
	if len(request.WithoutNodeIDs)+len(request.WithoutEdgeIDs) > maximumCounterfactualSuppressions {
		return CounterfactualResult{}, fmt.Errorf("%w: at most %d interventions are allowed", ErrInvalidCounterfactual, maximumCounterfactualSuppressions)
	}

	withoutNodes, err := validatedCounterfactualIDs(request.WithoutNodeIDs, func(id string) bool {
		_, exists := index.Nodes[id]
		return exists
	}, "node")
	if err != nil {
		return CounterfactualResult{}, err
	}
	withoutEdges, err := validatedCounterfactualIDs(request.WithoutEdgeIDs, func(id string) bool {
		_, exists := index.Edges[id]
		return exists
	}, "edge")
	if err != nil {
		return CounterfactualResult{}, err
	}
	for _, id := range withoutNodes {
		if id == request.FromID || id == request.ToID {
			return CounterfactualResult{}, fmt.Errorf("%w: route endpoints cannot be suppressed", ErrInvalidCounterfactual)
		}
	}

	baselineOptions := request.Traversal
	baselineOptions.SuppressedNodeIDs = nil
	baselineOptions.SuppressedEdgeIDs = nil
	baseline, err := index.ShortestPath(request.FromID, request.ToID, baselineOptions)
	if err != nil {
		return CounterfactualResult{}, err
	}
	derivedOptions := baselineOptions
	derivedOptions.SuppressedNodeIDs = stringSet(withoutNodes)
	derivedOptions.SuppressedEdgeIDs = stringSet(withoutEdges)
	derived, err := index.ShortestPath(request.FromID, request.ToID, derivedOptions)
	if err != nil {
		return CounterfactualResult{}, err
	}
	normalized := normalizeOptions(baselineOptions)
	result := CounterfactualResult{
		SchemaVersion: CounterfactualSchemaVersion, Analysis: "bounded_structural_counterfactual", Authoritative: false,
		Assumption: CounterfactualAssumption{WithoutNodeIDs: withoutNodes, WithoutEdgeIDs: withoutEdges},
		Scope: CounterfactualScope{
			Direction: normalized.Direction, MaximumDepth: normalized.MaxDepth,
			MaximumNodes: normalized.MaxNodes, IncludeUnresolved: normalized.IncludeUnresolved,
			EdgeKinds: sortedSetKeys(normalized.EdgeKinds), Resolutions: sortedSetKeys(normalized.Resolutions),
		},
		Baseline: baseline, Counterfactual: derived,
	}
	switch {
	case baseline.Truncated || derived.Truncated:
		result.Outcome = "search_truncated"
		result.Statement = fmt.Sprintf(
			"One or more route searches reached a configured traversal cap (%s), so the comparison remains unresolved and no exhaustive no-route conclusion is supported.",
			counterfactualTruncationSummary(baseline, derived),
		)
	case !baseline.Found:
		result.Outcome = "baseline_not_found"
		result.Statement = "The baseline search exhaustively found no route in the recorded graph under the stated filters, so this intervention has no supported route-level effect to compare."
	case derived.Found && equalStrings(baseline.EdgeIDs, derived.EdgeIDs):
		result.Outcome = "no_effect"
		result.Statement = "The same shortest route remains after the intervention within the stated traversal bounds."
	case derived.Found:
		result.Outcome = "rerouted"
		result.Statement = "A different route remains after the intervention within the stated traversal bounds."
	default:
		result.Outcome = "no_route_found"
		result.Statement = "No alternative route was found after exhaustively searching every admissible reachable node in the derived recorded graph under the stated filters; this is not proof of runtime impossibility or causation."
	}
	result.EvidenceIDs = counterfactualEvidence(index, baseline, derived, withoutNodes, withoutEdges)
	return result, nil
}

func counterfactualTruncationSummary(baseline, derived Path) string {
	var searches []string
	if baseline.Truncated {
		searches = append(searches, "baseline: "+pathLimitSummary(baseline))
	}
	if derived.Truncated {
		searches = append(searches, "counterfactual: "+pathLimitSummary(derived))
	}
	return strings.Join(searches, "; ")
}

func pathLimitSummary(path Path) string {
	var limits []string
	if path.DepthLimitReached {
		limits = append(limits, "depth limit")
	}
	if path.NodeLimitReached {
		limits = append(limits, "node limit")
	}
	if len(limits) == 0 {
		return "search bound"
	}
	return strings.Join(limits, " and ")
}

func validatedCounterfactualIDs(values []string, exists func(string) bool, kind string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, id := range values {
		if id == "" {
			return nil, fmt.Errorf("%w: %s ID cannot be empty", ErrInvalidCounterfactual, kind)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate %s ID %q", ErrInvalidCounterfactual, kind, id)
		}
		if !exists(id) {
			return nil, fmt.Errorf("%w: %s %q does not exist", ErrInvalidCounterfactual, kind, id)
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedSetKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func counterfactualEvidence(index *Index, baseline, derived Path, withoutNodes, withoutEdges []string) []string {
	seen := map[string]struct{}{}
	add := func(values []string) {
		for _, value := range values {
			if value != "" {
				seen[value] = struct{}{}
			}
		}
	}
	for _, nodeID := range append(append([]string(nil), baseline.NodeIDs...), derived.NodeIDs...) {
		add(index.Nodes[nodeID].EvidenceIDs)
	}
	for _, edgeID := range append(append(append([]string(nil), baseline.EdgeIDs...), derived.EdgeIDs...), withoutEdges...) {
		add(index.Edges[edgeID].EvidenceIDs)
	}
	for _, nodeID := range withoutNodes {
		add(index.Nodes[nodeID].EvidenceIDs)
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
