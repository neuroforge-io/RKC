package graph

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestCounterfactualPathReportsBoundedStructuralOutcomes(t *testing.T) {
	nodes, edges := graphFixture()
	for index := range nodes {
		nodes[index].EvidenceIDs = []string{"node-" + nodes[index].ID}
	}
	for index := range edges {
		edges[index].EvidenceIDs = []string{"edge-" + edges[index].ID}
	}
	index := Build(nodes, edges)
	options := TraverseOptions{Direction: DirectionOutgoing, IncludeUnresolved: true, MaxDepth: 3, MaxNodes: 20}

	rerouted, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"ab"}, Traversal: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rerouted.SchemaVersion != CounterfactualSchemaVersion || rerouted.Authoritative || rerouted.Analysis != "bounded_structural_counterfactual" ||
		rerouted.Outcome != "rerouted" || !rerouted.Baseline.Found || !rerouted.Counterfactual.Found ||
		!reflect.DeepEqual(rerouted.Baseline.EdgeIDs, []string{"ab", "bd"}) ||
		!reflect.DeepEqual(rerouted.Counterfactual.EdgeIDs, []string{"ac", "cd"}) ||
		rerouted.Scope.MaximumDepth != 3 || rerouted.Scope.MaximumNodes != 20 ||
		!strings.Contains(rerouted.Statement, "within the stated traversal bounds") {
		t.Fatalf("rerouted result = %+v", rerouted)
	}
	for _, evidence := range []string{"edge-ab", "edge-ac", "edge-bd", "edge-cd", "node-a", "node-b", "node-c", "node-d"} {
		if !containsString(rerouted.EvidenceIDs, evidence) {
			t.Errorf("rerouted evidence omits %q: %v", evidence, rerouted.EvidenceIDs)
		}
	}

	disconnected, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "a", ToID: "d", WithoutNodeIDs: []string{"b"},
		Traversal: TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 3, MaxNodes: 20},
	})
	if err != nil || disconnected.Outcome != "no_route_found" || disconnected.Counterfactual.Found ||
		disconnected.Counterfactual.Truncated || !disconnected.Counterfactual.SearchExhausted ||
		!strings.Contains(disconnected.Statement, "exhaustively") || !strings.Contains(disconnected.Statement, "not proof") {
		t.Fatalf("no-route result = %+v, %v", disconnected, err)
	}

	noEffect, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"cd"}, Traversal: options,
	})
	if err != nil || noEffect.Outcome != "no_effect" ||
		!reflect.DeepEqual(noEffect.Baseline.EdgeIDs, noEffect.Counterfactual.EdgeIDs) {
		t.Fatalf("no-effect result = %+v, %v", noEffect, err)
	}

	missingBaseline, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "u", ToID: "d", WithoutEdgeIDs: []string{"ab"}, Traversal: options,
	})
	if err != nil || missingBaseline.Outcome != "baseline_not_found" || missingBaseline.Baseline.Found {
		t.Fatalf("missing-baseline result = %+v, %v", missingBaseline, err)
	}

	// The derived traversal never mutates canonical adjacency or later queries.
	path, err := index.ShortestPath("a", "d", options)
	if err != nil || !reflect.DeepEqual(path.EdgeIDs, []string{"ab", "bd"}) {
		t.Fatalf("canonical graph changed after counterfactual: %+v, %v", path, err)
	}
}

func TestCounterfactualPathReportsSearchTruncationInsteadOfRouteAbsence(t *testing.T) {
	nodes, edges := graphFixture()
	// Preserve the two-edge baseline a->b->d, but make the route after
	// suppressing a->b require three edges a->c->e->d. A depth-two search can
	// prove the baseline route while only partially exploring the derived view.
	nodes = append(nodes, rkcmodel.Node{ID: "e", Name: "E"})
	edges[3] = rkcmodel.Edge{
		ID: "ce", Kind: "calls", From: "c", To: "e", Resolution: rkcmodel.ResolutionDeclared,
	}
	edges = append(edges, rkcmodel.Edge{
		ID: "ed", Kind: "calls", From: "e", To: "d", Resolution: rkcmodel.ResolutionDeclared,
	})
	index := Build(nodes, edges)

	depthLimited, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"ab"},
		Traversal: TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 2, MaxNodes: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if depthLimited.Outcome != "search_truncated" || !depthLimited.Baseline.Found ||
		depthLimited.Baseline.Truncated || depthLimited.Counterfactual.Found ||
		!depthLimited.Counterfactual.Truncated || !depthLimited.Counterfactual.DepthLimitReached ||
		depthLimited.Counterfactual.NodeLimitReached || depthLimited.Counterfactual.SearchExhausted ||
		!strings.Contains(depthLimited.Statement, "counterfactual: depth limit") ||
		strings.Contains(depthLimited.Statement, "No alternative route") {
		t.Fatalf("depth-truncated result = %+v", depthLimited)
	}

	nodeLimited, err := index.CounterfactualPath(CounterfactualRequest{
		FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"ab"},
		Traversal: TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 8, MaxNodes: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nodeLimited.Outcome != "search_truncated" || !nodeLimited.Baseline.Truncated ||
		!nodeLimited.Baseline.NodeLimitReached || !nodeLimited.Counterfactual.Truncated ||
		!nodeLimited.Counterfactual.NodeLimitReached ||
		!strings.Contains(nodeLimited.Statement, "node limit") {
		t.Fatalf("node-truncated result = %+v", nodeLimited)
	}
}

func TestCounterfactualPathRejectsUnboundedOrTrivialInterventions(t *testing.T) {
	nodes, edges := graphFixture()
	index := Build(nodes, edges)
	cases := []CounterfactualRequest{
		{},
		{FromID: "a", ToID: "d"},
		{FromID: "a", ToID: "d", WithoutNodeIDs: []string{"missing"}},
		{FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"missing"}},
		{FromID: "a", ToID: "d", WithoutNodeIDs: []string{"b", "b"}},
		{FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"ab", "ab"}},
		{FromID: "a", ToID: "d", WithoutNodeIDs: []string{"a"}},
		{FromID: "a", ToID: "d", WithoutNodeIDs: []string{"d"}},
	}
	tooMany := CounterfactualRequest{FromID: "a", ToID: "d"}
	for count := 0; count <= maximumCounterfactualSuppressions; count++ {
		tooMany.WithoutEdgeIDs = append(tooMany.WithoutEdgeIDs, "ab")
	}
	cases = append(cases, tooMany)
	for _, request := range cases {
		if _, err := index.CounterfactualPath(request); !errors.Is(err, ErrInvalidCounterfactual) {
			t.Errorf("request %+v error = %v", request, err)
		}
	}
	if _, err := (*Index)(nil).CounterfactualPath(CounterfactualRequest{FromID: "a", ToID: "d", WithoutEdgeIDs: []string{"ab"}}); !errors.Is(err, ErrInvalidCounterfactual) {
		t.Fatalf("nil index error = %v", err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
