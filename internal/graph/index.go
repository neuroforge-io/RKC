// Package graph implements bounded graph operations over one immutable RKC
// snapshot. Algorithms are deterministic: input adjacency lists and outputs are
// sorted so repeated calls do not depend on map iteration order.
package graph

import (
	"errors"
	"sort"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

var ErrNodeNotFound = errors.New("graph node not found")

type Index struct {
	Nodes    map[string]rkcmodel.Node
	Edges    map[string]rkcmodel.Edge
	Outgoing map[string][]rkcmodel.Edge
	Incoming map[string][]rkcmodel.Edge
}

func Build(nodes []rkcmodel.Node, edges []rkcmodel.Edge) *Index {
	index := &Index{
		Nodes:    make(map[string]rkcmodel.Node, len(nodes)),
		Edges:    make(map[string]rkcmodel.Edge, len(edges)),
		Outgoing: map[string][]rkcmodel.Edge{}, Incoming: map[string][]rkcmodel.Edge{},
	}
	for _, node := range nodes {
		index.Nodes[node.ID] = node
	}
	for _, edge := range edges {
		index.Edges[edge.ID] = edge
		index.Outgoing[edge.From] = append(index.Outgoing[edge.From], edge)
		index.Incoming[edge.To] = append(index.Incoming[edge.To], edge)
	}
	for id := range index.Outgoing {
		sortEdges(index.Outgoing[id])
	}
	for id := range index.Incoming {
		sortEdges(index.Incoming[id])
	}
	return index
}

type Direction string

const (
	DirectionOutgoing Direction = "outgoing"
	DirectionIncoming Direction = "incoming"
	DirectionBoth     Direction = "both"
)

type TraverseOptions struct {
	Direction         Direction
	EdgeKinds         map[string]struct{}
	Resolutions       map[string]struct{}
	MaxDepth          int
	MaxNodes          int
	IncludeUnresolved bool
}

type Neighborhood struct {
	SeedID    string          `json:"seed_id"`
	Nodes     []rkcmodel.Node `json:"nodes"`
	Edges     []rkcmodel.Edge `json:"edges"`
	DepthByID map[string]int  `json:"depth_by_id"`
	Truncated bool            `json:"truncated"`
}

func (index *Index) Neighborhood(seedID string, options TraverseOptions) (Neighborhood, error) {
	if _, ok := index.Nodes[seedID]; !ok {
		return Neighborhood{}, ErrNodeNotFound
	}
	options = normalizeOptions(options)
	visited := map[string]int{seedID: 0}
	queue := []string{seedID}
	edges := map[string]rkcmodel.Edge{}
	truncated := false
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		depth := visited[current]
		if depth >= options.MaxDepth {
			continue
		}
		for _, step := range index.steps(current, options) {
			edges[step.Edge.ID] = step.Edge
			if _, seen := visited[step.Next]; seen {
				continue
			}
			if len(visited) >= options.MaxNodes {
				truncated = true
				continue
			}
			visited[step.Next] = depth + 1
			queue = append(queue, step.Next)
		}
	}
	return index.materializeNeighborhood(seedID, visited, edges, truncated), nil
}

type Path struct {
	Found   bool            `json:"found"`
	FromID  string          `json:"from_id"`
	ToID    string          `json:"to_id"`
	NodeIDs []string        `json:"node_ids,omitempty"`
	EdgeIDs []string        `json:"edge_ids,omitempty"`
	Nodes   []rkcmodel.Node `json:"nodes,omitempty"`
	Edges   []rkcmodel.Edge `json:"edges,omitempty"`
	Depth   int             `json:"depth,omitempty"`
	Visited int             `json:"visited"`
}

func (index *Index) ShortestPath(fromID, toID string, options TraverseOptions) (Path, error) {
	if _, ok := index.Nodes[fromID]; !ok {
		return Path{}, ErrNodeNotFound
	}
	if _, ok := index.Nodes[toID]; !ok {
		return Path{}, ErrNodeNotFound
	}
	options = normalizeOptions(options)
	if fromID == toID {
		return Path{Found: true, FromID: fromID, ToID: toID, NodeIDs: []string{fromID}, Nodes: []rkcmodel.Node{index.Nodes[fromID]}, Visited: 1}, nil
	}
	type predecessor struct{ nodeID, edgeID string }
	previous := map[string]predecessor{}
	depth := map[string]int{fromID: 0}
	queue := []string{fromID}
	found := false
	for len(queue) > 0 && !found {
		current := queue[0]
		queue = queue[1:]
		if depth[current] >= options.MaxDepth {
			continue
		}
		for _, step := range index.steps(current, options) {
			if _, seen := depth[step.Next]; seen {
				continue
			}
			if len(depth) >= options.MaxNodes {
				break
			}
			depth[step.Next] = depth[current] + 1
			previous[step.Next] = predecessor{nodeID: current, edgeID: step.Edge.ID}
			if step.Next == toID {
				found = true
				break
			}
			queue = append(queue, step.Next)
		}
	}
	result := Path{Found: found, FromID: fromID, ToID: toID, Visited: len(depth)}
	if !found {
		return result, nil
	}
	for current := toID; ; {
		result.NodeIDs = append(result.NodeIDs, current)
		if current == fromID {
			break
		}
		prev := previous[current]
		result.EdgeIDs = append(result.EdgeIDs, prev.edgeID)
		current = prev.nodeID
	}
	reverseStrings(result.NodeIDs)
	reverseStrings(result.EdgeIDs)
	result.Depth = len(result.EdgeIDs)
	for _, id := range result.NodeIDs {
		result.Nodes = append(result.Nodes, index.Nodes[id])
	}
	for _, id := range result.EdgeIDs {
		result.Edges = append(result.Edges, index.Edges[id])
	}
	return result, nil
}

type ImpactResult struct {
	SeedID        string          `json:"seed_id"`
	ImpactedNodes []rkcmodel.Node `json:"impacted_nodes"`
	ImpactEdges   []rkcmodel.Edge `json:"impact_edges"`
	DepthByID     map[string]int  `json:"depth_by_id"`
	Truncated     bool            `json:"truncated"`
}

// Impact traverses incoming relationships by default, answering "what depends
// on this?" rather than the easier but less useful "what does this depend on?".
func (index *Index) Impact(seedID string, options TraverseOptions) (ImpactResult, error) {
	if options.Direction == "" {
		options.Direction = DirectionIncoming
	}
	neighborhood, err := index.Neighborhood(seedID, options)
	if err != nil {
		return ImpactResult{}, err
	}
	var impacted []rkcmodel.Node
	for _, node := range neighborhood.Nodes {
		if node.ID != seedID {
			impacted = append(impacted, node)
		}
	}
	return ImpactResult{SeedID: seedID, ImpactedNodes: impacted, ImpactEdges: neighborhood.Edges, DepthByID: neighborhood.DepthByID, Truncated: neighborhood.Truncated}, nil
}

type Component struct {
	ID      int      `json:"id"`
	NodeIDs []string `json:"node_ids"`
	Cyclic  bool     `json:"cyclic"`
}

// StronglyConnectedComponents uses Tarjan's algorithm and returns components in
// deterministic lexical order. It is useful for architecture summaries and for
// exposing dependency cycles without pretending every cycle is a catastrophe.
func (index *Index) StronglyConnectedComponents(edgeKinds map[string]struct{}) []Component {
	var sequence int
	indices := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var components []Component
	nodeIDs := make([]string, 0, len(index.Nodes))
	for id := range index.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	var visit func(string)
	visit = func(nodeID string) {
		indices[nodeID] = sequence
		lowlink[nodeID] = sequence
		sequence++
		stack = append(stack, nodeID)
		onStack[nodeID] = true
		for _, edge := range index.Outgoing[nodeID] {
			if len(edgeKinds) > 0 {
				if _, ok := edgeKinds[edge.Kind]; !ok {
					continue
				}
			}
			next := edge.To
			if _, seen := indices[next]; !seen {
				visit(next)
				if lowlink[next] < lowlink[nodeID] {
					lowlink[nodeID] = lowlink[next]
				}
			} else if onStack[next] && indices[next] < lowlink[nodeID] {
				lowlink[nodeID] = indices[next]
			}
		}
		if lowlink[nodeID] == indices[nodeID] {
			component := Component{ID: len(components)}
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[last] = false
				component.NodeIDs = append(component.NodeIDs, last)
				if last == nodeID {
					break
				}
			}
			sort.Strings(component.NodeIDs)
			component.Cyclic = len(component.NodeIDs) > 1 || index.hasSelfLoop(component.NodeIDs[0], edgeKinds)
			components = append(components, component)
		}
	}
	for _, id := range nodeIDs {
		if _, seen := indices[id]; !seen {
			visit(id)
		}
	}
	sort.Slice(components, func(i, j int) bool { return components[i].NodeIDs[0] < components[j].NodeIDs[0] })
	for i := range components {
		components[i].ID = i
	}
	return components
}

type step struct {
	Next string
	Edge rkcmodel.Edge
}

func (index *Index) steps(nodeID string, options TraverseOptions) []step {
	var steps []step
	if options.Direction == DirectionOutgoing || options.Direction == DirectionBoth {
		for _, edge := range index.Outgoing[nodeID] {
			if index.acceptEdge(edge, options) {
				steps = append(steps, step{Next: edge.To, Edge: edge})
			}
		}
	}
	if options.Direction == DirectionIncoming || options.Direction == DirectionBoth {
		for _, edge := range index.Incoming[nodeID] {
			if index.acceptEdge(edge, options) {
				steps = append(steps, step{Next: edge.From, Edge: edge})
			}
		}
	}
	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Next == steps[j].Next {
			return steps[i].Edge.ID < steps[j].Edge.ID
		}
		return steps[i].Next < steps[j].Next
	})
	return steps
}

func (index *Index) acceptEdge(edge rkcmodel.Edge, options TraverseOptions) bool {
	if !options.IncludeUnresolved && rkcmodel.NormalizeResolution(edge.Resolution) == rkcmodel.ResolutionUnresolved {
		return false
	}
	if len(options.EdgeKinds) > 0 {
		if _, ok := options.EdgeKinds[edge.Kind]; !ok {
			return false
		}
	}
	if len(options.Resolutions) > 0 {
		if _, ok := options.Resolutions[rkcmodel.NormalizeResolution(edge.Resolution)]; !ok {
			return false
		}
	}
	return true
}

func normalizeOptions(options TraverseOptions) TraverseOptions {
	if options.Direction == "" {
		options.Direction = DirectionBoth
	}
	if options.MaxDepth <= 0 {
		options.MaxDepth = 4
	}
	if options.MaxDepth > 64 {
		options.MaxDepth = 64
	}
	if options.MaxNodes <= 0 {
		options.MaxNodes = 5000
	}
	if options.MaxNodes > 100000 {
		options.MaxNodes = 100000
	}
	return options
}

func (index *Index) materializeNeighborhood(seed string, visited map[string]int, edgeMap map[string]rkcmodel.Edge, truncated bool) Neighborhood {
	nodeIDs := make([]string, 0, len(visited))
	for id := range visited {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool {
		if visited[nodeIDs[i]] == visited[nodeIDs[j]] {
			return nodeIDs[i] < nodeIDs[j]
		}
		return visited[nodeIDs[i]] < visited[nodeIDs[j]]
	})
	nodes := make([]rkcmodel.Node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		nodes = append(nodes, index.Nodes[id])
	}
	edges := make([]rkcmodel.Edge, 0, len(edgeMap))
	for _, edge := range edgeMap {
		if _, from := visited[edge.From]; !from {
			continue
		}
		if _, to := visited[edge.To]; !to {
			continue
		}
		edges = append(edges, edge)
	}
	sortEdges(edges)
	return Neighborhood{SeedID: seed, Nodes: nodes, Edges: edges, DepthByID: visited, Truncated: truncated}
}

func (index *Index) hasSelfLoop(nodeID string, edgeKinds map[string]struct{}) bool {
	for _, edge := range index.Outgoing[nodeID] {
		if edge.To != nodeID {
			continue
		}
		if len(edgeKinds) == 0 {
			return true
		}
		if _, ok := edgeKinds[edge.Kind]; ok {
			return true
		}
	}
	return false
}

func sortEdges(edges []rkcmodel.Edge) {
	sort.Slice(edges, func(i, j int) bool {
		left := edges[i].From + "\x00" + edges[i].To + "\x00" + edges[i].Kind + "\x00" + edges[i].ID
		right := edges[j].From + "\x00" + edges[j].To + "\x00" + edges[j].Kind + "\x00" + edges[j].ID
		return left < right
	})
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}
