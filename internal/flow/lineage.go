package flow

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// flowEdgeKinds are the edge kinds the lineage walks traverse.
var flowEdgeKinds = map[string]bool{
	"flows_to": true, "binds_to": true, "returns_to": true,
	"sanitizes": true, "reads": true, "derived_from": true,
}

// LineageResult is one bounded backward or forward lineage walk.
type LineageResult struct {
	Start   string          `json:"start"`
	Origin  bool            `json:"origins"`
	Nodes   []string        `json:"nodes"`
	Details []LineageDetail `json:"details"`
	Bounded bool            `json:"bounded"`
	Roles   map[string]int  `json:"roles"`
}

// LineageDetail describes one hop in a lineage walk.
type LineageDetail struct {
	Node     string `json:"node"`
	Kind     string `json:"kind"`
	EdgeKind string `json:"edge_kind"`
	FlowRole string `json:"flow_role,omitempty"`
	Variable string `json:"variable,omitempty"`
	SinkVia  string `json:"sink_via,omitempty"`
}

// Origins walks backward from a node through value-flow edges to the bounded
// set of entities it derives from (sources, parameters, literals, env reads).
func Origins(bundle rkcmodel.Bundle, start string) (LineageResult, error) {
	return walk(bundle, start, true)
}

// Sinks walks forward from a node through value-flow edges to the bounded set
// of entities it can reach (sinks, call results, writes).
func Sinks(bundle rkcmodel.Bundle, start string) (LineageResult, error) {
	return walk(bundle, start, false)
}

func walk(bundle rkcmodel.Bundle, start string, origins bool) (LineageResult, error) {
	if strings.TrimSpace(start) == "" {
		return LineageResult{}, errors.New("lineage start node is required")
	}
	// Build direction-aware adjacency over value-flow edge kinds.
	backward := map[string][]string{}
	forward := map[string][]string{}
	detailsByPair := map[string]map[string]LineageDetail{}
	nodeByID := map[string]rkcmodel.Node{}
	for _, node := range bundle.Nodes {
		nodeByID[node.ID] = node
	}
	roleOf := map[string]string{}
	for _, node := range bundle.Nodes {
		if role, ok := node.Attributes["flow_role"].(string); ok && role != "" {
			roleOf[node.ID] = role
		}
	}
	for _, edge := range bundle.Edges {
		if !flowEdgeKinds[edge.Kind] {
			continue
		}
		backward[edge.To] = append(backward[edge.To], edge.From)
		forward[edge.From] = append(forward[edge.From], edge.To)
		if detailsByPair[edge.From] == nil {
			detailsByPair[edge.From] = map[string]LineageDetail{}
		}
		detailsByPair[edge.From][edge.To] = LineageDetail{
			Node: edge.To, EdgeKind: edge.Kind,
		}
	}
	if _, exists := nodeByID[start]; !exists {
		return LineageResult{}, fmt.Errorf("lineage start node %q is not in the atlas", start)
	}
	// Function nodes bridge into their parameter, return, and value portals so
	// lineage works on symbols, not only value entities.
	queue := []string{start}
	if portals, ok := portalIndex(bundle)[start]; ok {
		queue = append(queue, portals...)
	}
	return finishLineage(nodeByID, roleOf, backward, forward, detailsByPair, start, queue, origins), nil
}

// portalIndex maps each function-like node to its flow portals: value,
// parameter, and return_value entities that belong to it.
func portalIndex(bundle rkcmodel.Bundle) map[string][]string {
	portals := map[string][]string{}
	functionsByQualified := make(map[string]string, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if isFunctionLike(node.Kind) && node.QualifiedName != "" {
			functionsByQualified[node.QualifiedName] = node.ID
		}
	}
	for _, node := range bundle.Nodes {
		if node.Kind != "value" && node.Kind != "parameter" && node.Kind != "return_value" {
			continue
		}
		hash := strings.LastIndexByte(node.QualifiedName, '#')
		if hash <= 0 {
			continue
		}
		functionQualified := node.QualifiedName[:hash]
		functionID := functionsByQualified[functionQualified]
		if functionID == "" {
			continue
		}
		portals[functionID] = append(portals[functionID], node.ID)
	}
	for functionID := range portals {
		sort.Strings(portals[functionID])
	}
	return portals
}

func finishLineage(
	nodeByID map[string]rkcmodel.Node,
	roleOf map[string]string,
	backward, forward map[string][]string,
	detailsByPair map[string]map[string]LineageDetail,
	start string,
	queue []string,
	origins bool,
) LineageResult {
	adjacencyMap := forward
	if origins {
		adjacencyMap = backward
	}
	visited := map[string]struct{}{}
	details := []LineageDetail{}
	roles := map[string]int{}
	bounded := false
	for len(queue) > 0 && len(visited) < maximumLineageNodes {
		current := queue[0]
		queue = queue[1:]
		if _, seen := visited[current]; seen {
			continue
		}
		visited[current] = struct{}{}
		node := nodeByID[current]
		if role := roleOf[current]; role != "" {
			roles[role]++
		}
		if node.Kind == "environment_variable" {
			roles["environment_variable"]++
		}
		next := append([]string(nil), adjacencyMap[current]...)
		sort.Strings(next)
		for _, neighbor := range next {
			if _, seen := visited[neighbor]; seen {
				continue
			}
			detail := LineageDetail{Node: neighbor, EdgeKind: "flows_to"}
			if pair, ok := detailsByPair[current][neighbor]; ok {
				detail.EdgeKind = pair.EdgeKind
			} else if pair, ok := detailsByPair[neighbor][current]; ok {
				detail.EdgeKind = pair.EdgeKind
			}
			neighborNode := nodeByID[neighbor]
			if role := roleOf[neighbor]; role != "" {
				detail.FlowRole = role
			}
			detail.Kind = neighborNode.Kind
			detail.Variable = lineageVariable(neighborNode)
			detail.SinkVia = stringAttribute(neighborNode.Attributes, "sink_via")
			details = append(details, detail)
			queue = append(queue, neighbor)
		}
	}
	if len(queue) > 0 || len(visited) >= maximumLineageNodes {
		bounded = true
	}
	sort.Slice(details, func(i, j int) bool { return details[i].Node < details[j].Node })
	nodeIDs := make([]string, 0, len(visited))
	for id := range visited {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	return LineageResult{
		Start: start, Origin: origins, Nodes: nodeIDs, Details: details,
		Bounded: bounded, Roles: roles,
	}
}

// lineageVariable returns only an exact variable identifier already attached
// to the node. It never guesses one from a qualified name or source text.
func lineageVariable(node rkcmodel.Node) string {
	for _, key := range []string{"variable", "parameter", "environment_variable"} {
		if value := stringAttribute(node.Attributes, key); value != "" {
			return value
		}
	}
	if node.Kind == "environment_variable" {
		return node.Name
	}
	return ""
}

func stringAttribute(attributes map[string]any, key string) string {
	if attributes == nil {
		return ""
	}
	value, _ := attributes[key].(string)
	return value
}

// FlowPath is one bounded value-flow path between two nodes.
type FlowPath struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Nodes   []string `json:"nodes"`
	Edges   []string `json:"edges"`
	Found   bool     `json:"found"`
	Bounded bool     `json:"bounded"`
}

// Path finds a bounded value-flow path from start to target using
// bidirectional BFS over flow edges.
func Path(bundle rkcmodel.Bundle, start, target string) (FlowPath, error) {
	if strings.TrimSpace(start) == "" || strings.TrimSpace(target) == "" {
		return FlowPath{}, errors.New("flow path requires both endpoints")
	}
	forward := map[string][]string{}
	backward := map[string][]string{}
	for _, edge := range bundle.Edges {
		if !flowEdgeKinds[edge.Kind] {
			continue
		}
		forward[edge.From] = append(forward[edge.From], edge.To)
		backward[edge.To] = append(backward[edge.To], edge.From)
	}
	type frontier struct {
		node  string
		path  []string
		edges []string
	}
	edgeBetween := map[string]map[string]string{}
	for _, edge := range bundle.Edges {
		if !flowEdgeKinds[edge.Kind] {
			continue
		}
		if edgeBetween[edge.From] == nil {
			edgeBetween[edge.From] = map[string]string{}
		}
		edgeBetween[edge.From][edge.To] = edge.ID
	}
	visitedForward := map[string]struct{}{}
	visitedBackward := map[string]struct{}{}
	forwardPaths := map[string]frontier{}
	backwardPaths := map[string]frontier{}
	portals := portalIndex(bundle)
	startPortals := portals[start]
	targetPortals := portals[target]
	queueForward := []frontier{{node: start, path: []string{start}}}
	for _, portal := range startPortals {
		queueForward = append(queueForward, frontier{node: portal, path: []string{portal}})
	}
	queueBackward := []frontier{{node: target, path: []string{target}}}
	for _, portal := range targetPortals {
		queueBackward = append(queueBackward, frontier{node: portal, path: []string{portal}})
	}
	complete := func(forwardPath, forwardEdges, backwardPath, backwardEdges []string) FlowPath {
		merged := append(append([]string(nil), forwardPath...), reverse(backwardPath[1:])...)
		mergedEdges := append(append([]string(nil), forwardEdges...), reverse(backwardEdges)...)
		if startPortals != nil && (len(merged) == 0 || merged[0] != start) && portalOf(startPortals, merged[0]) {
			merged = append([]string{start}, merged...)
		}
		if targetPortals != nil && len(merged) > 0 && merged[len(merged)-1] != target && portalOf(targetPortals, merged[len(merged)-1]) {
			merged = append(merged, target)
		}
		return FlowPath{From: start, To: target, Nodes: merged, Edges: mergedEdges, Found: true, Bounded: false}
	}
	bounded := false
	for len(queueForward) > 0 || len(queueBackward) > 0 {
		if len(visitedForward) >= maximumPathNodes || len(visitedBackward) >= maximumPathNodes {
			bounded = true
			break
		}
		// Forward step.
		if len(queueForward) > 0 {
			current := queueForward[0]
			queueForward = queueForward[1:]
			if _, seen := visitedForward[current.node]; seen {
				continue
			}
			visitedForward[current.node] = struct{}{}
			forwardPaths[current.node] = current
			// From the start we follow edges pointing INTO the current node:
			// the requested path runs against the data-flow direction.
			next := append([]string(nil), backward[current.node]...)
			sort.Strings(next)
			for _, neighbor := range next {
				if _, seen := visitedForward[neighbor]; seen {
					continue
				}
				newPath := append(append([]string(nil), current.path...), neighbor)
				newEdges := append(append([]string(nil), current.edges...), edgeBetween[neighbor][current.node])
				if backwardPath, met := backwardPaths[neighbor]; met {
					return complete(newPath, newEdges, backwardPath.path, backwardPath.edges), nil
				}
				queueForward = append(queueForward, frontier{node: neighbor, path: newPath, edges: newEdges})
			}
		}
		// Backward step.
		if len(queueBackward) > 0 {
			current := queueBackward[0]
			queueBackward = queueBackward[1:]
			if _, seen := visitedBackward[current.node]; seen {
				continue
			}
			visitedBackward[current.node] = struct{}{}
			backwardPaths[current.node] = current
			// From the target we follow edges leaving the current node: the
			// target's data flows forward through the graph.
			next := append([]string(nil), forward[current.node]...)
			sort.Strings(next)
			for _, neighbor := range next {
				if _, seen := visitedBackward[neighbor]; seen {
					continue
				}
				newPath := append(append([]string(nil), current.path...), neighbor)
				newEdges := append(append([]string(nil), current.edges...), edgeBetween[current.node][neighbor])
				if forwardPath, met := forwardPaths[neighbor]; met {
					return complete(forwardPath.path, forwardPath.edges, newPath, newEdges), nil
				}
				queueBackward = append(queueBackward, frontier{node: neighbor, path: newPath, edges: newEdges})
			}
		}
	}
	return FlowPath{From: start, To: target, Bounded: bounded}, nil
}

func portalOf(portals []string, node string) bool {
	for _, portal := range portals {
		if portal == node {
			return true
		}
	}
	return false
}

func reverse(values []string) []string {
	result := make([]string, len(values))
	for index := range values {
		result[len(values)-1-index] = values[index]
	}
	return result
}

// Report is the deterministic flow report over one atlas. It is computed
// purely from the bundle, so the same atlas always yields the same report.
type Report struct {
	PluginVersion        string           `json:"plugin_version"`
	CallEdges            int              `json:"call_edges"`
	CallEdgesResolved    int              `json:"call_edges_resolved"`
	CallEdgeTargets      int              `json:"call_edge_targets"`
	CFGBlocks            int              `json:"cfg_blocks"`
	CFGEdges             int              `json:"cfg_edges"`
	ValueNodes           int              `json:"value_nodes"`
	FlowEdges            int              `json:"flow_edges"`
	BindsToEdges         int              `json:"binds_to_edges"`
	ReturnsToEdges       int              `json:"returns_to_edges"`
	SanitizeEdges        int              `json:"sanitize_edges"`
	EnvReads             int              `json:"env_reads"`
	EnvironmentVariables []string         `json:"environment_variables"`
	Sources              []string         `json:"sources"`
	Sinks                []string         `json:"sinks"`
	Sanitizers           []string         `json:"sanitizers"`
	SanitizerHypotheses  []string         `json:"sanitizer_hypotheses"`
	SourceSinkPaths      []SourceSinkPath `json:"source_sink_paths"`
	Bounded              bool             `json:"bounded"`
}

// SourceSinkPath is one bounded reachability pair.
type SourceSinkPath struct {
	Source string `json:"source"`
	Sink   string `json:"sink"`
	Nodes  int    `json:"nodes"`
}

// BuildReport compiles the bounded flow report from one bundle.
func BuildReport(bundle rkcmodel.Bundle) Report {
	report := Report{PluginVersion: PluginVersion}
	nodeByID := make(map[string]rkcmodel.Node, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		nodeByID[node.ID] = node
	}
	nodeName := func(id string) string {
		node, ok := nodeByID[id]
		if !ok {
			return ""
		}
		if node.QualifiedName != "" {
			return node.QualifiedName
		}
		return node.Name
	}
	callTargets := map[string]struct{}{}
	envVariables := map[string]struct{}{}
	sources := map[string]struct{}{}
	sinks := map[string]struct{}{}
	sanitizers := map[string]struct{}{}
	sanitizerHypotheses := map[string]struct{}{}
	sinkValues := []string{}
	for _, edge := range bundle.Edges {
		switch edge.Kind {
		case "calls":
			report.CallEdges++
			if target, ok := nodeByID[edge.To]; ok && isFunctionLike(target.Kind) {
				report.CallEdgesResolved++
			}
			callTargets[edge.To] = struct{}{}
		case "precedes":
			report.CFGEdges++
		case "flows_to":
			report.FlowEdges++
		case "binds_to":
			report.BindsToEdges++
		case "returns_to":
			report.ReturnsToEdges++
		case "sanitizes":
			report.SanitizeEdges++
			if name := nodeName(edge.From); name != "" {
				sanitizers[name] = struct{}{}
			}
		case "related_to":
			if edge.Attributes["hypothesis"] == "sanitizer_name" {
				if name := nodeName(edge.From); name != "" {
					sanitizerHypotheses[name] = struct{}{}
				}
			}
		case "reads":
			if nodeByID[edge.From].Kind == "environment_variable" {
				report.EnvReads++
			}
		}
	}
	report.CallEdgeTargets = len(callTargets)
	for _, node := range bundle.Nodes {
		switch node.Kind {
		case "cfg_block":
			report.CFGBlocks++
		case "value":
			report.ValueNodes++
			role, _ := node.Attributes["flow_role"].(string)
			switch role {
			case roleSource:
				if name := nodeName(node.ID); name != "" {
					sources[name] = struct{}{}
				}
			case roleSink:
				if name := nodeName(node.ID); name != "" {
					sinks[name] = struct{}{}
				}
				sinkValues = append(sinkValues, node.ID)
			}
		case "environment_variable":
			envVariables[node.Name] = struct{}{}
		}
	}
	report.EnvironmentVariables = sortedKeys(envVariables)
	report.Sources = sortedKeys(sources)
	report.Sinks = sortedKeys(sinks)
	report.Sanitizers = sortedKeys(sanitizers)
	report.SanitizerHypotheses = sortedKeys(sanitizerHypotheses)
	// Bounded source-to-sink reachability: walk backward from each sink value
	// until a source role or environment variable is found.
	backward := map[string][]string{}
	for _, edge := range bundle.Edges {
		if flowEdgeKinds[edge.Kind] {
			backward[edge.To] = append(backward[edge.To], edge.From)
		}
	}
	seenPairs := map[string]struct{}{}
	sort.Strings(sinkValues)
	for _, sinkID := range sinkValues {
		visited := map[string]struct{}{}
		queue := []string{sinkID}
		for len(queue) > 0 && len(visited) < maximumLineageNodes {
			current := queue[0]
			queue = queue[1:]
			if _, seen := visited[current]; seen {
				continue
			}
			visited[current] = struct{}{}
			node := nodeByID[current]
			if node.Kind == "environment_variable" || node.Attributes["flow_role"] == roleSource {
				sourceName := nodeName(current)
				pair := sourceName + "\x00" + sinkID
				if _, duplicate := seenPairs[pair]; duplicate {
					continue
				}
				seenPairs[pair] = struct{}{}
				report.SourceSinkPaths = append(report.SourceSinkPaths, SourceSinkPath{
					Source: sourceName, Sink: sinkID, Nodes: len(visited),
				})
				if len(report.SourceSinkPaths) >= maximumSourceSinkPaths {
					report.Bounded = true
					break
				}
				continue
			}
			next := append([]string(nil), backward[current]...)
			sort.Strings(next)
			for _, neighbor := range next {
				if _, seen := visited[neighbor]; seen {
					continue
				}
				queue = append(queue, neighbor)
			}
		}
		if report.Bounded {
			break
		}
	}
	sort.Slice(report.SourceSinkPaths, func(i, j int) bool {
		if report.SourceSinkPaths[i].Source != report.SourceSinkPaths[j].Source {
			return report.SourceSinkPaths[i].Source < report.SourceSinkPaths[j].Source
		}
		return report.SourceSinkPaths[i].Sink < report.SourceSinkPaths[j].Sink
	})
	return report
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
