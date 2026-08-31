// Package flow compiles bounded interprocedural control-flow and value-flow
// evidence into the canonical graph. It answers questions the reference graph
// cannot: where a value originates, what it can reach, whether a source can
// reach a sink, and under which branches a call can happen.
//
// The analysis is deliberately deterministic, bounded, and conservative:
//
//   - Call-graph edges come from the resolved syntax tier (go.ast calls and
//     compiler-grade SCIP references) and never invent callees.
//   - Control-flow graphs are built per Go function body from the parsed
//     source, with bounded blocks and successors per function.
//   - Value flow is intraprocedural with explicit interprocedural seams:
//     argument binding (binds_to), return propagation (returns_to), and
//     environment reads (reads). It tracks simple identifier assignments,
//     literals, call results, and derived expressions; it never executes
//     repository code.
//   - Source and sink roles require package/type authority. Sanitizer names
//     remain explicit low-confidence hypotheses above the truth layer and are
//     never traversed as protection.
//   - Lineage walks and source-to-sink paths are bounded; exceeding a bound
//     produces an explicit diagnostic instead of an unbounded traversal.
package flow

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// PluginID is the stable producer identity attached to every flow fact.
const PluginID = "rkc.flow"

// PluginVersion identifies the bounded flow-analysis semantics.
const PluginVersion = "1.1.0"

// Bounds keep every analysis pass deterministic and bounded regardless of
// repository size or adversarial input.
const (
	maximumCFGFunctions       = 4096
	maximumCFGBlocksPerFunc   = 256
	maximumCFGBlocksTotal     = 16384
	maximumCFGEdgesTotal      = 32768
	maximumSuccessorsPerBlock = 16
	maximumCallEdgesTotal     = 131072
	maximumValueNodesPerFunc  = 128
	maximumFlowEdgesPerFunc   = 512
	maximumFlowFunctions      = 4096
	maximumValueNodesTotal    = 16384
	maximumFlowEdgesTotal     = 32768
	maximumSourceSinkPaths    = 4096
	maximumLineageNodes       = 512
	maximumPathNodes          = 256
	maximumEnvReads           = 4096
	// Every emitted node or edge has exactly one evidence record. The record
	// and conservative retained-byte ceilings therefore bound the complete
	// generated fragment, not merely each individual graph category.
	maximumFactRecordsTotal        = 210000
	maximumEstimatedFactBytesTotal = int64(256 << 20)
	maximumFlowTextBytes           = 512
	maximumFlowDiagnostics         = 64
)

type analysisLimits struct {
	cfgFunctions       int
	cfgBlocksPerFunc   int
	cfgBlocksTotal     int
	cfgEdgesTotal      int
	successorsPerBlock int
	callEdgesTotal     int
	valueNodesPerFunc  int
	flowEdgesPerFunc   int
	flowFunctions      int
	valueNodesTotal    int
	flowEdgesTotal     int
	envReads           int
	factRecordsTotal   int
	estimatedFactBytes int64
	diagnosticsTotal   int
}

var defaultAnalysisLimits = analysisLimits{
	cfgFunctions:       maximumCFGFunctions,
	cfgBlocksPerFunc:   maximumCFGBlocksPerFunc,
	cfgBlocksTotal:     maximumCFGBlocksTotal,
	cfgEdgesTotal:      maximumCFGEdgesTotal,
	successorsPerBlock: maximumSuccessorsPerBlock,
	callEdgesTotal:     maximumCallEdgesTotal,
	valueNodesPerFunc:  maximumValueNodesPerFunc,
	flowEdgesPerFunc:   maximumFlowEdgesPerFunc,
	flowFunctions:      maximumFlowFunctions,
	valueNodesTotal:    maximumValueNodesTotal,
	flowEdgesTotal:     maximumFlowEdgesTotal,
	envReads:           maximumEnvReads,
	factRecordsTotal:   maximumFactRecordsTotal,
	estimatedFactBytes: maximumEstimatedFactBytesTotal,
	diagnosticsTotal:   maximumFlowDiagnostics,
}

func (limits analysisLimits) valid() bool {
	return limits.cfgFunctions > 0 && limits.cfgBlocksPerFunc > 0 && limits.cfgBlocksTotal > 0 &&
		limits.cfgEdgesTotal > 0 && limits.successorsPerBlock > 0 && limits.callEdgesTotal > 0 &&
		limits.valueNodesPerFunc > 0 && limits.flowEdgesPerFunc > 0 && limits.flowFunctions > 0 &&
		limits.valueNodesTotal > 0 && limits.flowEdgesTotal > 0 && limits.envReads > 0 &&
		limits.factRecordsTotal > 0 && limits.estimatedFactBytes > 0 && limits.diagnosticsTotal > 0
}

// Options supplies the trusted repository root, the inventoried Go files to
// re-parse, and the canonical bundle whose resolved call edges seed the call
// graph.
type Options struct {
	Root      string
	Files     []pluginapi.FileRef
	Artifacts []rkcmodel.Artifact
	Bundle    rkcmodel.Bundle
}

// Stats reports bounded flow-analysis counts for diagnostics and reporting.
type Stats struct {
	CFGFunctions        int   `json:"cfg_functions"`
	CFGBlocks           int   `json:"cfg_blocks"`
	CFGEdges            int   `json:"cfg_edges"`
	CFGBoundedFunctions int   `json:"cfg_bounded_functions"`
	CallEdges           int   `json:"call_edges"`
	CallEdgesResolved   int   `json:"call_edges_resolved"`
	ValueFunctions      int   `json:"value_functions"`
	ValueNodes          int   `json:"value_nodes"`
	ValueEdges          int   `json:"value_edges"`
	BindsToEdges        int   `json:"binds_to_edges"`
	ReturnsToEdges      int   `json:"returns_to_edges"`
	SanitizeEdges       int   `json:"sanitize_edges"`
	EnvReads            int   `json:"env_reads"`
	Sources             int   `json:"sources"`
	Sinks               int   `json:"sinks"`
	SourceSinkPaths     int   `json:"source_sink_paths"`
	BoundedExceeded     int   `json:"bounded_exceeded"`
	FactRecords         int   `json:"fact_records"`
	EstimatedFactBytes  int64 `json:"estimated_fact_bytes"`
}

// Analyze runs the bounded flow analysis over one repository snapshot. It
// returns a fragment of flow nodes, edges, evidence, and diagnostics plus the
// measured stats. The fragment is deterministic for identical inputs.
func Analyze(ctx context.Context, options Options) (rkcmodel.Fragment, Stats, error) {
	return analyzeWithLimits(ctx, options, defaultAnalysisLimits)
}

func analyzeWithLimits(ctx context.Context, options Options, limits analysisLimits) (rkcmodel.Fragment, Stats, error) {
	if ctx == nil {
		return rkcmodel.Fragment{}, Stats{}, errors.New("flow analysis context is required")
	}
	if strings.TrimSpace(options.Root) == "" {
		return rkcmodel.Fragment{}, Stats{}, errors.New("flow analysis root is required")
	}
	if !limits.valid() {
		return rkcmodel.Fragment{}, Stats{}, errors.New("flow analysis limits must be positive")
	}
	callGraph, callGraphBounded := buildCallGraph(options.Bundle, limits)
	stats := Stats{
		CallEdges:         callGraph.edges,
		CallEdgesResolved: callGraph.resolvedEdges,
	}
	fragment := rkcmodel.Fragment{}
	analyzer := newGoAnalyzer(ctx, options, callGraph, limits)
	if callGraphBounded {
		analyzer.noteBound("RKC-FLOW-2000", "bounded out at "+strconv.Itoa(limits.callEdgesTotal)+" call edges")
	}
	if err := analyzer.analyze(); err != nil {
		return rkcmodel.Fragment{}, Stats{}, err
	}
	stats.CFGFunctions = analyzer.stats.CFGFunctions
	stats.CFGBlocks = analyzer.stats.CFGBlocks
	stats.CFGEdges = analyzer.stats.CFGEdges
	stats.CFGBoundedFunctions = analyzer.stats.CFGBoundedFunctions
	stats.ValueFunctions = analyzer.stats.ValueFunctions
	stats.ValueNodes = analyzer.stats.ValueNodes
	stats.ValueEdges = analyzer.stats.ValueEdges
	stats.BindsToEdges = analyzer.stats.BindsToEdges
	stats.ReturnsToEdges = analyzer.stats.ReturnsToEdges
	stats.SanitizeEdges = analyzer.stats.SanitizeEdges
	stats.EnvReads = analyzer.stats.EnvReads
	stats.Sources = analyzer.stats.Sources
	stats.Sinks = analyzer.stats.Sinks
	stats.BoundedExceeded = analyzer.stats.BoundedExceeded
	stats.FactRecords = analyzer.stats.FactRecords
	stats.EstimatedFactBytes = analyzer.stats.EstimatedFactBytes
	fragment = analyzer.fragment
	sort.Slice(fragment.Nodes, func(i, j int) bool { return fragment.Nodes[i].ID < fragment.Nodes[j].ID })
	sort.Slice(fragment.Edges, func(i, j int) bool { return fragment.Edges[i].ID < fragment.Edges[j].ID })
	sort.Slice(fragment.Evidence, func(i, j int) bool { return fragment.Evidence[i].ID < fragment.Evidence[j].ID })
	sort.Slice(fragment.Diagnostics, func(i, j int) bool { return fragment.Diagnostics[i].ID < fragment.Diagnostics[j].ID })
	return fragment, stats, nil
}

// callGraph is the resolved interprocedural call graph seeded from the
// bundle's calls edges. Every call edge carries its call-site span when the
// producer recorded one.
type callGraph struct {
	// byFunction lists call sites per calling function node ID.
	byFunction map[string][]callSite
	// functionByID indexes function-like nodes for target lookup.
	functionByID  map[string]rkcmodel.Node
	edges         int
	resolvedEdges int
}

type callSite struct {
	CallerID string
	TargetID string
	EdgeID   string
	Spelling string
	Span     *rkcmodel.SourceRange
}

func buildCallGraph(bundle rkcmodel.Bundle, limits analysisLimits) (*callGraph, bool) {
	graph := &callGraph{
		byFunction:   map[string][]callSite{},
		functionByID: map[string]rkcmodel.Node{},
	}
	evidenceByID := make(map[string]rkcmodel.Evidence, len(bundle.Evidence))
	for _, evidence := range bundle.Evidence {
		evidenceByID[evidence.ID] = evidence
	}
	for _, node := range bundle.Nodes {
		if isFunctionLike(node.Kind) {
			graph.functionByID[node.ID] = node
		}
	}
	edges := append([]rkcmodel.Edge(nil), bundle.Edges...)
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	bounded := false
	for _, edge := range edges {
		if edge.Kind != "calls" {
			continue
		}
		if graph.edges >= limits.callEdgesTotal {
			bounded = true
			break
		}
		graph.edges++
		if _, resolved := graph.functionByID[edge.To]; resolved {
			graph.resolvedEdges++
		}
		spelling, _ := edge.Attributes["spelling"].(string)
		span := callSpan(edge, evidenceByID)
		graph.byFunction[edge.From] = append(graph.byFunction[edge.From], callSite{
			CallerID: edge.From, TargetID: edge.To, EdgeID: edge.ID,
			Spelling: spelling, Span: span,
		})
	}
	for functionID := range graph.byFunction {
		sites := graph.byFunction[functionID]
		sort.SliceStable(sites, func(i, j int) bool { return sites[i].TargetID < sites[j].TargetID })
		graph.byFunction[functionID] = sites
	}
	return graph, bounded
}

func callSpan(edge rkcmodel.Edge, evidenceByID map[string]rkcmodel.Evidence) *rkcmodel.SourceRange {
	// Compiler adapters may record a direct span attribute. Syntax adapters,
	// including go.ast, attach the exact call site to cited evidence instead.
	if raw, ok := edge.Attributes["span"].(map[string]any); ok {
		span := &rkcmodel.SourceRange{}
		if path, ok := raw["path"].(string); ok {
			span.Path = path
		}
		span.StartLine = flowIntegerAttribute(raw["start_line"])
		span.EndLine = flowIntegerAttribute(raw["end_line"])
		span.StartColumn = flowIntegerAttribute(raw["start_column"])
		span.EndColumn = flowIntegerAttribute(raw["end_column"])
		span.StartByte = int64(flowIntegerAttribute(raw["start_byte"]))
		span.EndByte = int64(flowIntegerAttribute(raw["end_byte"]))
		if span.Path != "" {
			return span
		}
	}
	for _, evidenceID := range edge.EvidenceIDs {
		evidence, ok := evidenceByID[evidenceID]
		if !ok || evidence.Source == nil || evidence.Source.Path == "" {
			continue
		}
		span := *evidence.Source
		return &span
	}
	return nil
}

func flowIntegerAttribute(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func isFunctionLike(kind string) bool {
	switch kind {
	case "function", "method", "constructor", "test":
		return true
	default:
		return false
	}
}

func readArtifactSource(root string, artifact rkcmodel.Artifact) ([]byte, error) {
	if !artifact.Text {
		return nil, errors.New("artifact is not text")
	}
	return sourcepath.ReadFile(root, artifact.Path)
}

func flowDiagnostic(code, message string) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{
		ID: rkcmodel.StableID("diagnostic", PluginID, code, message), Severity: "note", Code: code,
		Message: message, Stage: "value_flow", Plugin: PluginID + "@" + PluginVersion,
	}
}
