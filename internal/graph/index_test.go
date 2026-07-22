package graph

import (
	"errors"
	"reflect"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestBuildCreatesDeterministicAdjacency(t *testing.T) {
	t.Parallel()

	nodes, edges := graphFixture()
	index := Build(nodes, []rkcmodel.Edge{edges[3], edges[1], edges[5], edges[0], edges[4], edges[2]})
	if len(index.Nodes) != len(nodes) || len(index.Edges) != len(edges) {
		t.Fatalf("index sizes nodes=%d edges=%d", len(index.Nodes), len(index.Edges))
	}
	if got := edgeIDs(index.Outgoing["a"]); !reflect.DeepEqual(got, []string{"ab", "ac"}) {
		t.Fatalf("outgoing a order = %v", got)
	}
	if got := edgeIDs(index.Incoming["d"]); !reflect.DeepEqual(got, []string{"bd", "cd"}) {
		t.Fatalf("incoming d order = %v", got)
	}
}

func TestNeighborhoodDirectionsFiltersDepthAndBounds(t *testing.T) {
	t.Parallel()

	nodes, edges := graphFixture()
	index := Build(nodes, edges)
	tests := []struct {
		name       string
		options    TraverseOptions
		wantNodes  []string
		wantEdges  []string
		wantDepths map[string]int
		wantTrunc  bool
	}{
		{
			name:       "default both directions excludes unresolved",
			options:    TraverseOptions{},
			wantNodes:  []string{"a", "b", "c", "d"},
			wantEdges:  []string{"ab", "ac", "bd", "da"},
			wantDepths: map[string]int{"a": 0, "b": 1, "c": 1, "d": 1},
		},
		{
			name:       "outgoing depth one",
			options:    TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 1},
			wantNodes:  []string{"a", "b", "c"},
			wantEdges:  []string{"ab", "ac"},
			wantDepths: map[string]int{"a": 0, "b": 1, "c": 1},
		},
		{
			name:       "incoming",
			options:    TraverseOptions{Direction: DirectionIncoming, MaxDepth: 1},
			wantNodes:  []string{"a", "d"},
			wantEdges:  []string{"da"},
			wantDepths: map[string]int{"a": 0, "d": 1},
		},
		{
			name:       "edge kind filter",
			options:    TraverseOptions{Direction: DirectionOutgoing, EdgeKinds: map[string]struct{}{"calls": {}}, MaxDepth: 3},
			wantNodes:  []string{"a", "b", "d"},
			wantEdges:  []string{"ab", "bd"},
			wantDepths: map[string]int{"a": 0, "b": 1, "d": 2},
		},
		{
			name:       "resolution filter normalized at edge",
			options:    TraverseOptions{Direction: DirectionOutgoing, Resolutions: map[string]struct{}{rkcmodel.ResolutionCompilerResolved: {}}, MaxDepth: 2},
			wantNodes:  []string{"a", "c"},
			wantEdges:  []string{"ac"},
			wantDepths: map[string]int{"a": 0, "c": 1},
		},
		{
			name:       "include unresolved",
			options:    TraverseOptions{Direction: DirectionOutgoing, IncludeUnresolved: true, MaxDepth: 2},
			wantNodes:  []string{"a", "b", "c", "d"},
			wantEdges:  []string{"ab", "ac", "bd", "cd"},
			wantDepths: map[string]int{"a": 0, "b": 1, "c": 1, "d": 2},
		},
		{
			name:       "node bound truncates",
			options:    TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 4, MaxNodes: 2},
			wantNodes:  []string{"a", "b"},
			wantEdges:  []string{"ab"},
			wantDepths: map[string]int{"a": 0, "b": 1},
			wantTrunc:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := index.Neighborhood("a", test.options)
			if err != nil {
				t.Fatal(err)
			}
			if got.SeedID != "a" || got.Truncated != test.wantTrunc || !reflect.DeepEqual(nodeIDs(got.Nodes), test.wantNodes) ||
				!reflect.DeepEqual(edgeIDs(got.Edges), test.wantEdges) || !reflect.DeepEqual(got.DepthByID, test.wantDepths) {
				t.Fatalf("Neighborhood() = %+v", got)
			}
		})
	}
	if _, err := index.Neighborhood("missing", TraverseOptions{}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("missing seed error = %v", err)
	}
}

func TestShortestPathHandlesIdentityMissingBoundsAndDeterministicTies(t *testing.T) {
	t.Parallel()

	nodes, edges := graphFixture()
	// Add a second equally short resolved route. Lexical step ordering must pick b.
	edges[3].Resolution = rkcmodel.ResolutionDeclared
	index := Build(nodes, edges)

	path, err := index.ShortestPath("a", "d", TraverseOptions{Direction: DirectionOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if !path.Found || path.Depth != 2 || path.FromID != "a" || path.ToID != "d" || path.Visited != 4 ||
		!reflect.DeepEqual(path.NodeIDs, []string{"a", "b", "d"}) || !reflect.DeepEqual(path.EdgeIDs, []string{"ab", "bd"}) ||
		!reflect.DeepEqual(nodeIDs(path.Nodes), path.NodeIDs) || !reflect.DeepEqual(edgeIDs(path.Edges), path.EdgeIDs) {
		t.Fatalf("ShortestPath deterministic route = %+v", path)
	}

	identity, err := index.ShortestPath("a", "a", TraverseOptions{})
	if err != nil || !identity.Found || identity.Depth != 0 || identity.Visited != 1 || !reflect.DeepEqual(identity.NodeIDs, []string{"a"}) || len(identity.EdgeIDs) != 0 {
		t.Fatalf("identity path = %+v, %v", identity, err)
	}
	for _, endpoints := range [][2]string{{"missing", "a"}, {"a", "missing"}} {
		if _, err := index.ShortestPath(endpoints[0], endpoints[1], TraverseOptions{}); !errors.Is(err, ErrNodeNotFound) {
			t.Errorf("ShortestPath(%q,%q) error = %v", endpoints[0], endpoints[1], err)
		}
	}

	tooShallow, err := index.ShortestPath("a", "d", TraverseOptions{Direction: DirectionOutgoing, MaxDepth: 1})
	if err != nil || tooShallow.Found || tooShallow.Visited != 3 {
		t.Fatalf("depth-bounded path = %+v, %v", tooShallow, err)
	}
	tooSmall, err := index.ShortestPath("a", "d", TraverseOptions{Direction: DirectionOutgoing, MaxNodes: 1})
	if err != nil || tooSmall.Found || tooSmall.Visited != 1 {
		t.Fatalf("node-bounded path = %+v, %v", tooSmall, err)
	}
	unreachable, err := index.ShortestPath("u", "a", TraverseOptions{Direction: DirectionOutgoing})
	if err != nil || unreachable.Found || unreachable.Visited != 1 {
		t.Fatalf("unreachable path = %+v, %v", unreachable, err)
	}
}

func TestImpactDefaultsIncomingAndPropagatesTruncation(t *testing.T) {
	t.Parallel()

	nodes, edges := graphFixture()
	index := Build(nodes, edges)
	impact, err := index.Impact("d", TraverseOptions{MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if impact.SeedID != "d" || !reflect.DeepEqual(nodeIDs(impact.ImpactedNodes), []string{"b"}) ||
		!reflect.DeepEqual(edgeIDs(impact.ImpactEdges), []string{"bd"}) || !reflect.DeepEqual(impact.DepthByID, map[string]int{"d": 0, "b": 1}) {
		// c->d is unresolved and excluded by default.
		t.Fatalf("incoming impact = %+v", impact)
	}
	truncated, err := index.Impact("d", TraverseOptions{Direction: DirectionIncoming, IncludeUnresolved: true, MaxDepth: 1, MaxNodes: 2})
	if err != nil || !truncated.Truncated || len(truncated.ImpactedNodes) != 1 {
		t.Fatalf("truncated impact = %+v, %v", truncated, err)
	}
	if _, err := index.Impact("missing", TraverseOptions{}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("missing impact seed error = %v", err)
	}
}

func TestStronglyConnectedComponentsAndEdgeKindFiltering(t *testing.T) {
	t.Parallel()

	nodes, edges := graphFixture()
	index := Build(nodes, edges)
	components := index.StronglyConnectedComponents(nil)
	if len(components) != 2 {
		t.Fatalf("components = %+v", components)
	}
	if components[0].ID != 0 || !reflect.DeepEqual(components[0].NodeIDs, []string{"a", "b", "c", "d"}) || !components[0].Cyclic {
		t.Errorf("main component = %+v", components[0])
	}
	if components[1].ID != 1 || !reflect.DeepEqual(components[1].NodeIDs, []string{"u"}) || !components[1].Cyclic {
		t.Errorf("self-loop component = %+v", components[1])
	}

	callsOnly := index.StronglyConnectedComponents(map[string]struct{}{"calls": {}})
	if len(callsOnly) != len(nodes) {
		t.Fatalf("calls-only components = %+v", callsOnly)
	}
	for index, component := range callsOnly {
		if component.ID != index || component.Cyclic {
			t.Errorf("filtered component = %+v", component)
		}
	}
	if !index.hasSelfLoop("u", map[string]struct{}{"references": {}}) || index.hasSelfLoop("u", map[string]struct{}{"calls": {}}) {
		t.Fatal("self-loop kind filtering mismatch")
	}
}

func TestOptionNormalizationAcceptanceAndHelpers(t *testing.T) {
	t.Parallel()

	defaults := normalizeOptions(TraverseOptions{})
	if defaults.Direction != DirectionBoth || defaults.MaxDepth != 4 || defaults.MaxNodes != 5000 {
		t.Fatalf("defaults = %+v", defaults)
	}
	capped := normalizeOptions(TraverseOptions{Direction: DirectionIncoming, MaxDepth: 100, MaxNodes: 200000})
	if capped.Direction != DirectionIncoming || capped.MaxDepth != 64 || capped.MaxNodes != 100000 {
		t.Fatalf("caps = %+v", capped)
	}
	index := Build(nil, nil)
	edge := rkcmodel.Edge{Kind: "calls", Resolution: "resolved"}
	if !index.acceptEdge(edge, TraverseOptions{}) || index.acceptEdge(edge, TraverseOptions{EdgeKinds: map[string]struct{}{"imports": {}}}) ||
		index.acceptEdge(edge, TraverseOptions{Resolutions: map[string]struct{}{rkcmodel.ResolutionDeclared: {}}}) ||
		index.acceptEdge(rkcmodel.Edge{Resolution: rkcmodel.ResolutionUnresolved}, TraverseOptions{}) ||
		!index.acceptEdge(rkcmodel.Edge{Resolution: rkcmodel.ResolutionUnresolved}, TraverseOptions{IncludeUnresolved: true}) {
		t.Fatal("edge acceptance filters mismatch")
	}
	values := []string{"a", "b", "c", "d"}
	reverseStrings(values)
	if !reflect.DeepEqual(values, []string{"d", "c", "b", "a"}) {
		t.Fatalf("reverseStrings = %v", values)
	}
	parallel := Build(
		[]rkcmodel.Node{{ID: "a"}, {ID: "b"}},
		[]rkcmodel.Edge{
			{ID: "z", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "a", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
		},
	)
	steps := parallel.steps("a", TraverseOptions{Direction: DirectionOutgoing, IncludeUnresolved: true})
	if len(steps) != 2 || steps[0].Edge.ID != "a" || steps[1].Edge.ID != "z" {
		t.Fatalf("parallel edge step ordering = %+v", steps)
	}
}

func graphFixture() ([]rkcmodel.Node, []rkcmodel.Edge) {
	nodes := []rkcmodel.Node{
		{ID: "d", Name: "D"}, {ID: "b", Name: "B"}, {ID: "u", Name: "U"}, {ID: "a", Name: "A"}, {ID: "c", Name: "C"},
	}
	edges := []rkcmodel.Edge{
		{ID: "ab", Kind: "calls", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
		{ID: "ac", Kind: "imports", From: "a", To: "c", Resolution: "resolved"},
		{ID: "bd", Kind: "calls", From: "b", To: "d", Resolution: rkcmodel.ResolutionSyntaxInferred},
		{ID: "cd", Kind: "calls", From: "c", To: "d", Resolution: rkcmodel.ResolutionUnresolved},
		{ID: "da", Kind: "depends_on", From: "d", To: "a", Resolution: rkcmodel.ResolutionDeclared},
		{ID: "uu", Kind: "references", From: "u", To: "u", Resolution: rkcmodel.ResolutionDeclared},
	}
	return nodes, edges
}

func nodeIDs(nodes []rkcmodel.Node) []string {
	ids := make([]string, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	return ids
}

func edgeIDs(edges []rkcmodel.Edge) []string {
	ids := make([]string, len(edges))
	for i, edge := range edges {
		ids[i] = edge.ID
	}
	return ids
}
