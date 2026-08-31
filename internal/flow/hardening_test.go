package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func analyzeFlowWithLimits(t *testing.T, source string, configure func(*analysisLimits)) (rkcmodel.Fragment, Stats) {
	t.Helper()
	root, bundle, _ := flowFixture(t, source)
	limits := defaultAnalysisLimits
	configure(&limits)
	fragment, stats, err := analyzeWithLimits(context.Background(), Options{
		Root: root, Artifacts: bundle.Artifacts, Bundle: bundle,
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	knownNodes := make(map[string]struct{}, len(bundle.Nodes)+len(bundle.Artifacts)+len(fragment.Nodes))
	for _, node := range bundle.Nodes {
		knownNodes[node.ID] = struct{}{}
	}
	for _, artifact := range bundle.Artifacts {
		knownNodes[artifact.ID] = struct{}{}
	}
	for _, node := range fragment.Nodes {
		knownNodes[node.ID] = struct{}{}
	}
	for _, edge := range fragment.Edges {
		if _, exists := knownNodes[edge.From]; !exists {
			t.Fatalf("flow edge %s has missing source %s", edge.ID, edge.From)
		}
		if _, exists := knownNodes[edge.To]; !exists {
			t.Fatalf("flow edge %s has missing target %s", edge.ID, edge.To)
		}
	}
	return fragment, stats
}

func requireFlowDiagnostic(t *testing.T, fragment rkcmodel.Fragment, code string) {
	t.Helper()
	for _, diagnostic := range fragment.Diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("diagnostic %s missing: %+v", code, fragment.Diagnostics)
}

func TestFlowEndpointGuardOmitsOrphansWithoutClaimingABound(t *testing.T) {
	analyzer := newGoAnalyzer(context.Background(), Options{Bundle: rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{{ID: "source", Kind: "value", Name: "source"}},
	}}, &callGraph{byFunction: map[string][]callSite{}, functionByID: map[string]rkcmodel.Node{}}, defaultAnalysisLimits)
	if analyzer.addEdge("flows_to", "source", "missing", "declared", nil) {
		t.Fatal("edge with a missing target was admitted")
	}
	if len(analyzer.fragment.Edges) != 0 || analyzer.stats.BoundedExceeded != 0 {
		t.Fatalf("orphan guard emitted edges or claimed a bound: edges=%d bounded=%d", len(analyzer.fragment.Edges), analyzer.stats.BoundedExceeded)
	}
	requireFlowDiagnostic(t, analyzer.fragment, "RKC-FLOW-2033")
}

func TestDeferredBindingsRespectPerFunctionEdgeBound(t *testing.T) {
	fragment, stats := analyzeFlowWithLimits(t, `package main

func target(first, second int) {}

func caller(first, second int) {
	target(first, second)
}
`, func(limits *analysisLimits) {
		limits.flowEdgesPerFunc = 1
	})
	if stats.BindsToEdges != 1 {
		t.Fatalf("deferred binds_to = %d, want 1 under the caller edge bound", stats.BindsToEdges)
	}
	if stats.BoundedExceeded == 0 {
		t.Fatal("deferred binding truncation did not report a bound")
	}
	requireFlowDiagnostic(t, fragment, "RKC-FLOW-2020")
}

func TestCallGraphPreservesEvidenceOnlyCallSpan(t *testing.T) {
	const source = `package main

func callee() {}

func caller() {
	callee()
}
`
	_, bundle, _ := flowFixture(t, source)
	graph, bounded := buildCallGraph(bundle, defaultAnalysisLimits)
	if bounded {
		t.Fatal("small call graph unexpectedly bounded out")
	}
	for _, sites := range graph.byFunction {
		for _, site := range sites {
			if site.Spelling != "callee" {
				continue
			}
			if site.Span == nil {
				t.Fatal("go.ast call edge lost its evidence-backed call span")
			}
			if site.Span.Path != "main.go" || site.Span.StartLine != 6 || site.Span.EndLine != 6 {
				t.Fatalf("call span = %+v, want main.go:6", *site.Span)
			}
			return
		}
	}
	t.Fatal("callee call site not found")
}

func TestFlowLiteralMetadataDoesNotDiscloseValues(t *testing.T) {
	const privateLiteral = "rkc-private-literal-47d39e"
	source := `package main

func literalValues() string {
	secret := "` + privateLiteral + `"
	number := 731942
	_ = number
	return secret
}
`
	root, bundle, _ := flowFixture(t, source)
	fragment, _, err := Analyze(context.Background(), Options{
		Root: root, Artifacts: bundle.Artifacts, Bundle: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), privateLiteral) || strings.Contains(string(encoded), "731942") {
		t.Fatalf("serialized flow fragment disclosed a source literal: %s", encoded)
	}

	wantDigestBytes := sha256.Sum256([]byte(PluginID + "\x00go-literal\x00STRING\x00" + privateLiteral))
	wantDigest := hex.EncodeToString(wantDigestBytes[:])
	rawDigestBytes := sha256.Sum256([]byte(privateLiteral))
	rawDigest := hex.EncodeToString(rawDigestBytes[:])
	foundSecretDigest := false
	literals := 0
	for _, node := range fragment.Nodes {
		if node.Kind != "value" || node.Attributes["flow_role"] != roleLiteral {
			continue
		}
		literals++
		for key := range node.Attributes {
			switch key {
			case "flow_role", "literal_type", "literal_length_bytes", "literal_sha256":
			default:
				t.Fatalf("literal node %s exposed unexpected attribute %q", node.ID, key)
			}
		}
		digest, ok := node.Attributes["literal_sha256"].(string)
		if !ok || len(digest) != sha256.Size*2 {
			t.Fatalf("literal node %s has invalid digest metadata: %+v", node.ID, node.Attributes)
		}
		if _, ok := node.Attributes["literal_type"].(string); !ok {
			t.Fatalf("literal node %s has no literal type: %+v", node.ID, node.Attributes)
		}
		if _, ok := node.Attributes["literal_length_bytes"].(int); !ok {
			t.Fatalf("literal node %s has no byte length: %+v", node.ID, node.Attributes)
		}
		if digest == rawDigest {
			t.Fatalf("literal node %s used a non-domain-separated digest", node.ID)
		}
		if digest == wantDigest {
			foundSecretDigest = true
		}
	}
	if literals != 2 || !foundSecretDigest {
		t.Fatalf("literal metadata incomplete: literals=%d secret_digest=%v", literals, foundSecretDigest)
	}
}

func TestFlowFactsCarryCanonicalEvidence(t *testing.T) {
	source := `package main

func evidence(input int) int {
	if input > 0 {
		return input
	}
	return 1
}
`
	root, bundle, _ := flowFixture(t, source)
	const artifactDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bundle.Artifacts[0].SHA256 = artifactDigest
	fragment, _, err := Analyze(context.Background(), Options{
		Root: root, Artifacts: bundle.Artifacts, Bundle: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) == 0 || len(fragment.Edges) == 0 {
		t.Fatalf("fixture emitted no flow facts: nodes=%d edges=%d", len(fragment.Nodes), len(fragment.Edges))
	}
	evidenceByID := map[string]rkcmodel.Evidence{}
	for _, evidence := range fragment.Evidence {
		if _, duplicate := evidenceByID[evidence.ID]; duplicate {
			t.Fatalf("duplicate evidence ID %s", evidence.ID)
		}
		if !rkcmodel.IsKnownEvidenceKind(evidence.Kind) {
			t.Fatalf("evidence %s uses non-canonical kind %q", evidence.ID, evidence.Kind)
		}
		evidenceByID[evidence.ID] = evidence
	}
	check := func(factID, factKind string, evidenceIDs []string) {
		t.Helper()
		if len(evidenceIDs) != 1 {
			t.Fatalf("%s %s evidence IDs = %v, want exactly one", factKind, factID, evidenceIDs)
		}
		method := factKind
		wantID := rkcmodel.StableID("evidence", PluginID, PluginVersion, method, factID)
		if evidenceIDs[0] != wantID {
			t.Fatalf("%s %s evidence ID = %s, want %s", factKind, factID, evidenceIDs[0], wantID)
		}
		evidence, ok := evidenceByID[wantID]
		if !ok {
			t.Fatalf("%s %s cites missing evidence %s", factKind, factID, wantID)
		}
		if evidence.Method != PluginID+"."+method || evidence.Tool != PluginID || evidence.ToolVersion != PluginVersion {
			t.Fatalf("evidence %s producer metadata = %+v", wantID, evidence)
		}
		if evidence.Source == nil || evidence.Source.Path != "main.go" || evidence.InputDigest != artifactDigest {
			t.Fatalf("evidence %s has incomplete source citation: %+v", wantID, evidence)
		}
	}
	for _, node := range fragment.Nodes {
		check(node.ID, "node."+node.Kind, node.EvidenceIDs)
	}
	for _, edge := range fragment.Edges {
		check(edge.ID, "edge."+edge.Kind, edge.EvidenceIDs)
	}
	if len(evidenceByID) != len(fragment.Nodes)+len(fragment.Edges) {
		t.Fatalf("evidence count = %d, want one for each of %d facts", len(evidenceByID), len(fragment.Nodes)+len(fragment.Edges))
	}
}

func TestFlowLineageIncludesExactMetadata(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{ID: "source", Kind: "value", Attributes: map[string]any{"flow_role": roleSource, "variable": "raw"}},
			{ID: "write", Kind: "value", Attributes: map[string]any{"flow_role": roleWrite, "variable": "clean"}},
			{ID: "sink", Kind: "value", Attributes: map[string]any{"flow_role": roleSink, "sink_via": "database/sql.(*DB).Exec"}},
		},
		Edges: []rkcmodel.Edge{
			{ID: "source-write", Kind: "flows_to", From: "source", To: "write"},
			{ID: "write-sink", Kind: "flows_to", From: "write", To: "sink"},
		},
	}
	sinks, err := Sinks(bundle, "source")
	if err != nil {
		t.Fatal(err)
	}
	if sinks.Start != "source" {
		t.Fatalf("sink lineage start = %q, want source", sinks.Start)
	}
	details := map[string]LineageDetail{}
	for _, detail := range sinks.Details {
		details[detail.Node] = detail
	}
	if details["write"].Variable != "clean" || details["write"].FlowRole != roleWrite {
		t.Fatalf("write lineage detail incomplete: %+v", details["write"])
	}
	if details["sink"].SinkVia != "database/sql.(*DB).Exec" || details["sink"].FlowRole != roleSink {
		t.Fatalf("sink lineage detail incomplete: %+v", details["sink"])
	}
	origins, err := Origins(bundle, "sink")
	if err != nil {
		t.Fatal(err)
	}
	if origins.Start != "sink" {
		t.Fatalf("origin lineage start = %q, want sink", origins.Start)
	}
	originDetails := map[string]LineageDetail{}
	for _, detail := range origins.Details {
		originDetails[detail.Node] = detail
	}
	if originDetails["source"].Variable != "raw" || originDetails["source"].FlowRole != roleSource {
		t.Fatalf("source lineage detail incomplete: %+v", originDetails["source"])
	}
}

func TestFlowSinkClassificationRequiresKnownQualifiedAPI(t *testing.T) {
	if !qualifiedPackageMatch("scip-go stdlib go1.25 database/sql/(*DB).Exec().", "database/sql") {
		t.Fatal("compiler-qualified database/sql symbol was not recognized")
	}
	if qualifiedPackageMatch("scip-go gomod example.com/db v1.0.0 example.com/database/sql/Exec().", "database/sql") {
		t.Fatal("lookalike compiler-qualified package was accepted")
	}
	scipQualified := "scip-go stdlib go1.25 database/sql/(*DB).Exec()."
	scipBuilder := valueFlowBuilder{analyzer: &goAnalyzer{callGraph: &callGraph{
		functionByID: map[string]rkcmodel.Node{
			"scip-exec": {ID: "scip-exec", Kind: "method", Name: "Exec", QualifiedName: scipQualified},
		},
	}}}
	if via, ok := scipBuilder.sinkName("scip-exec", true, "db.Exec"); !ok || via != scipQualified {
		t.Fatalf("compiler-qualified sink = %q, %v", via, ok)
	}
	localSource := `package main

type worker struct{}

func (worker) Run(value string) {}
func Exec(value string) {}
func Query(value string) {}
func Create(value string) {}

func localCalls(w worker, value string) {
	w.Run(value)
	Exec(value)
	Query(value)
	Create(value)
}
`
	localBundle, localStats := flowAnalyze(t, localSource)
	if localStats.Sinks != 0 {
		t.Fatalf("repository-local sink-like names produced %d sinks", localStats.Sinks)
	}
	for _, node := range localBundle.Nodes {
		if node.Attributes["flow_role"] == roleSink {
			t.Fatalf("repository-local call was classified as sink: %+v", node)
		}
	}

	knownSource := `package main

import "database/sql"

func knownSink(db *sql.DB, value string) {
	db.Exec(value)
}
`
	knownBundle, knownStats := flowAnalyze(t, knownSource)
	if knownStats.Sinks != 1 {
		t.Fatalf("known database/sql API produced %d sinks, want 1", knownStats.Sinks)
	}
	foundQualified := false
	for _, node := range knownBundle.Nodes {
		if node.Attributes["flow_role"] != roleSink {
			continue
		}
		via, _ := node.Attributes["sink_via"].(string)
		if strings.HasPrefix(via, "database/sql.") {
			foundQualified = true
		}
	}
	if !foundQualified {
		t.Fatal("known database/sql sink did not retain its qualified API")
	}
}

func TestFlowAggregateLimitsFailClosed(t *testing.T) {
	const functions = `package main

func alpha() {}
func beta() {}
func gamma() { alpha(); beta() }
`
	t.Run("call edges", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.callEdgesTotal = 1
		})
		if stats.CallEdges != 1 {
			t.Fatalf("call edges = %d, want 1", stats.CallEdges)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2000")
	})
	t.Run("CFG functions", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.cfgFunctions = 1
		})
		if stats.CFGFunctions != 1 {
			t.Fatalf("CFG functions = %d, want 1", stats.CFGFunctions)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2001")
	})
	t.Run("CFG blocks", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.cfgBlocksTotal = 2
		})
		if stats.CFGBlocks != 2 {
			t.Fatalf("CFG blocks = %d, want 2", stats.CFGBlocks)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2011")
	})
	t.Run("CFG edges", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.cfgEdgesTotal = 1
		})
		if stats.CFGEdges != 1 {
			t.Fatalf("CFG edges = %d, want 1", stats.CFGEdges)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2012")
	})
	t.Run("value-flow functions", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.flowFunctions = 1
		})
		if stats.ValueFunctions != 1 {
			t.Fatalf("value-flow functions = %d, want 1", stats.ValueFunctions)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2021")
	})
	t.Run("value nodes", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

func values() {
	one := 1
	two := one
	_ = two
}
`, func(limits *analysisLimits) {
			limits.valueNodesTotal = 1
		})
		if stats.ValueNodes != 1 {
			t.Fatalf("value nodes = %d, want 1", stats.ValueNodes)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2022")
	})
	t.Run("value-flow edges", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

func edges() {
	one := 1
	two := one
	_ = two
}
`, func(limits *analysisLimits) {
			limits.flowEdgesTotal = 1
		})
		if stats.ValueEdges != 1 || stats.BindsToEdges+stats.ReturnsToEdges+stats.SanitizeEdges+stats.EnvReads != 0 {
			t.Fatalf("flow edge stats exceeded bound: %+v", stats)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2023")
	})
	t.Run("environment reads", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

import "os"

func environment() {
	_ = os.Getenv("ONE")
	_ = os.Getenv("TWO")
}
`, func(limits *analysisLimits) {
			limits.envReads = 1
		})
		if stats.EnvReads != 1 {
			t.Fatalf("environment reads = %d, want 1", stats.EnvReads)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2024")
	})
	t.Run("complete fact records", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.factRecordsTotal = 3
		})
		if stats.FactRecords != 2 || len(fragment.Nodes)+len(fragment.Edges)+len(fragment.Evidence) != 2 {
			t.Fatalf("aggregate fact records escaped their atomic bound: stats=%+v fragment=%+v", stats, fragment)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2030")
	})
	t.Run("estimated retained bytes", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, functions, func(limits *analysisLimits) {
			limits.estimatedFactBytes = 1
		})
		if stats.FactRecords != 0 || stats.EstimatedFactBytes != 0 ||
			len(fragment.Nodes)+len(fragment.Edges)+len(fragment.Evidence) != 0 {
			t.Fatalf("byte-bounded analysis emitted a partial fact: stats=%+v fragment=%+v", stats, fragment)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2031")
	})
}

func TestFlowGeneratedTextIsBounded(t *testing.T) {
	identifier := "function" + strings.Repeat("x", maximumFlowTextBytes*4)
	fragment, stats := analyzeFlowWithLimits(t, "package main\n\nfunc "+identifier+"() { value := 1; _ = value }\n", func(*analysisLimits) {})
	if stats.FactRecords == 0 {
		t.Fatal("long-identifier fixture produced no flow facts")
	}
	for _, node := range fragment.Nodes {
		for field, value := range map[string]string{
			"name": node.Name, "qualified_name": node.QualifiedName, "signature": node.Signature,
		} {
			if len(value) > maximumFlowTextBytes {
				t.Fatalf("node %s %s has %d bytes; maximum is %d", node.ID, field, len(value), maximumFlowTextBytes)
			}
		}
		assertBoundedFlowAttributes(t, node.Attributes)
	}
	for _, edge := range fragment.Edges {
		assertBoundedFlowAttributes(t, edge.Attributes)
	}
}

func assertBoundedFlowAttributes(t *testing.T, attributes map[string]any) {
	t.Helper()
	for key, value := range attributes {
		if len(key) > maximumFlowTextBytes {
			t.Fatalf("flow attribute key has %d bytes", len(key))
		}
		if text, ok := value.(string); ok && len(text) > maximumFlowTextBytes {
			t.Fatalf("flow attribute %q has %d bytes", key, len(text))
		}
	}
}

func TestFlowPerFunctionLimitsOmitOverflow(t *testing.T) {
	t.Run("successors", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

func choose(value int) int {
	switch value {
	case 1:
		return 1
	case 2:
		return 2
	default:
		return 0
	}
}
`, func(limits *analysisLimits) {
			limits.successorsPerBlock = 1
		})
		if stats.CFGBlocks != 0 || stats.CFGEdges != 0 || stats.CFGBoundedFunctions != 1 {
			t.Fatalf("successor-bounded CFG was partially emitted: %+v", stats)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2013")
	})
	t.Run("value nodes", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

func values() {
	one := 1
	two := one
	_ = two
}
`, func(limits *analysisLimits) {
			limits.valueNodesPerFunc = 1
		})
		if stats.ValueNodes != 1 {
			t.Fatalf("per-function value nodes = %d, want 1", stats.ValueNodes)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2020")
	})
	t.Run("value-flow edges", func(t *testing.T) {
		fragment, stats := analyzeFlowWithLimits(t, `package main

func edges() {
	one := 1
	two := one
	_ = two
}
`, func(limits *analysisLimits) {
			limits.flowEdgesPerFunc = 1
		})
		if stats.ValueEdges != 1 {
			t.Fatalf("per-function value-flow edges = %d, want 1", stats.ValueEdges)
		}
		requireFlowDiagnostic(t, fragment, "RKC-FLOW-2020")
	})
}

func TestNameOnlyAPIsRemainNonAuthoritativeHypotheses(t *testing.T) {
	bundle, stats := flowAnalyze(t, `package main

func Getenv(name string) string { return name }
func Query(value string) string { return value }
func validate(value string) string { return value }

func demo() string {
	value := Getenv("NOT_OS")
	value = Query(value)
	return validate(value)
}
`)
	if stats.EnvReads != 0 || stats.Sources != 0 || stats.SanitizeEdges != 0 {
		t.Fatalf("basename heuristics became canonical truth: %+v", stats)
	}
	hypotheses := 0
	for _, edge := range bundle.Edges {
		if edge.Attributes["hypothesis"] != "sanitizer_name" {
			continue
		}
		hypotheses++
		if edge.Kind != "related_to" || edge.Resolution != rkcmodel.ResolutionSyntaxInferred ||
			edge.Confidence != 0.25 || edge.Attributes["non_authoritative"] != true {
			t.Fatalf("unsafe sanitizer hypothesis = %+v", edge)
		}
	}
	if hypotheses != 1 {
		t.Fatalf("sanitizer hypotheses = %d, want 1", hypotheses)
	}
}
