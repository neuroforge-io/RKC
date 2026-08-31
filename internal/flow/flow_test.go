package flow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/lang/goast"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// flowFixture writes a small Go repository and compiles it through the
// deterministic syntax tier so the flow analysis runs over a realistic bundle
// (resolved calls edges included).
func flowFixture(t *testing.T, source string) (root string, bundle rkcmodel.Bundle, artifactID string) {
	t.Helper()
	root = t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactID = rkcmodel.StableID("artifact", "main.go")
	fragment, err := goast.Extract(goast.Options{
		Root: root,
		Files: []pluginapi.FileRef{{
			ArtifactID: artifactID, Path: "main.go", Language: "go",
			SizeBytes: int64(len(source)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle = rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{
			ID: artifactID, Path: "main.go", Kind: "source", Language: "go",
			SizeBytes: int64(len(source)), Text: true, Status: "syntax_parsed",
		}},
		Nodes: fragment.Nodes, Edges: fragment.Edges, Evidence: fragment.Evidence,
		Diagnostics: fragment.Diagnostics,
	}
	resolveHeuristicEdgesForTest(&bundle)
	rkcmodel.SortBundle(&bundle)
	return root, bundle, artifactID
}

func resolveHeuristicEdgesForTest(bundle *rkcmodel.Bundle) {
	// Mirror the pipeline resolver for unique-name call targets so flow sees
	// resolved call edges, and resolve well-known standard-library callees to
	// synthetic symbols exactly as a SCIP index would.
	byID := map[string]rkcmodel.Node{}
	byName := map[string][]string{}
	for _, node := range bundle.Nodes {
		byID[node.ID] = node
		if node.Kind == "function" || node.Kind == "method" {
			if node.Name != "" {
				byName[node.Name] = append(byName[node.Name], node.ID)
			}
		}
	}
	knownStdlib := map[string]string{
		"Getenv": "os.Getenv", "LookupEnv": "os.LookupEnv", "GetenvDefault": "os.GetenvDefault",
		"Exec": "database/sql.Exec", "ExecContext": "database/sql.ExecContext",
		"Query": "database/sql.Query", "QueryContext": "database/sql.QueryContext",
		"QueryRow": "database/sql.QueryRow", "QueryRowContext": "database/sql.QueryRowContext",
		"Prepare": "database/sql.Prepare", "PrepareContext": "database/sql.PrepareContext",
		"Open": "database/sql.Open", "Command": "os/exec.Command", "CommandContext": "os/exec.CommandContext",
	}
	for index := range bundle.Edges {
		edge := &bundle.Edges[index]
		if edge.Kind != "calls" || edge.Resolution != "unresolved" {
			continue
		}
		spelling, _ := edge.Attributes["spelling"].(string)
		if spelling == "" {
			continue
		}
		name := spelling
		if dot := strings.LastIndexByte(spelling, '.'); dot >= 0 {
			name = spelling[dot+1:]
		}
		candidates := byName[name]
		// An unqualified call with one repository-local declaration is local.
		// Qualified selector calls may share a basename with a local function,
		// so leave those to the synthetic compiler-grade fixtures below.
		if !strings.Contains(spelling, ".") && len(candidates) == 1 {
			edge.To = candidates[0]
			edge.Resolution = "syntax_inferred"
			edge.Confidence = 0.65
			continue
		}
		if qualified, ok := knownStdlib[name]; ok {
			nodeID := rkcmodel.StableID("node", "function", "stdlib", qualified)
			bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
				ID: nodeID, Kind: "function", Name: name, QualifiedName: qualified,
				Language: "go", Visibility: "public",
			})
			edge.To = nodeID
			edge.Resolution = "compiler_resolved"
			edge.Confidence = 1
			continue
		}
		if len(candidates) == 1 {
			edge.To = candidates[0]
			edge.Resolution = "syntax_inferred"
			edge.Confidence = 0.65
		}
	}
}

func flowAnalyze(t *testing.T, source string) (rkcmodel.Bundle, Stats) {
	t.Helper()
	root, bundle, artifactID := flowFixture(t, source)
	fragment, stats, err := Analyze(context.Background(), Options{
		Root: root, Files: nil, Artifacts: bundle.Artifacts, Bundle: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Nodes = append(bundle.Nodes, fragment.Nodes...)
	bundle.Edges = append(bundle.Edges, fragment.Edges...)
	bundle.Evidence = append(bundle.Evidence, fragment.Evidence...)
	bundle.Diagnostics = append(bundle.Diagnostics, fragment.Diagnostics...)
	rkcmodel.SortBundle(&bundle)
	nodeIDs := make(map[string]struct{}, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		nodeIDs[node.ID] = struct{}{}
	}
	// The production pipeline represents every artifact as a same-ID graph
	// node before adapters run; the focused fixture keeps only Artifact records.
	for _, artifact := range bundle.Artifacts {
		nodeIDs[artifact.ID] = struct{}{}
	}
	for _, edge := range bundle.Edges {
		if _, ok := nodeIDs[edge.From]; !ok {
			t.Fatalf("edge %s has missing source %s", edge.ID, edge.From)
		}
		if _, ok := nodeIDs[edge.To]; !ok {
			t.Fatalf("edge %s has missing target %s", edge.ID, edge.To)
		}
	}
	_ = artifactID
	return bundle, stats
}

func nodeByID(bundle rkcmodel.Bundle, id string) (rkcmodel.Node, bool) {
	for _, node := range bundle.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return rkcmodel.Node{}, false
}

func TestFlowAnalyzeBuildsCFG(t *testing.T) {
	source := `package main

func branch(flag bool) int {
	if flag {
		return 1
	}
	for i := 0; i < 3; i++ {
		if i == 1 {
			continue
		}
		return i
	}
	return 0
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.CFGFunctions != 1 {
		t.Fatalf("cfg functions = %d, want 1", stats.CFGFunctions)
	}
	if stats.CFGBlocks < 4 || stats.CFGEdges < 4 {
		t.Fatalf("cfg blocks=%d edges=%d; want a real CFG", stats.CFGBlocks, stats.CFGEdges)
	}
	blocks := 0
	precedes := 0
	for _, node := range bundle.Nodes {
		if node.Kind == "cfg_block" {
			blocks++
			if node.Attributes["kind"] == nil {
				t.Fatalf("cfg block %s has no kind attribute", node.ID)
			}
		}
	}
	for _, edge := range bundle.Edges {
		if edge.Kind == "precedes" {
			precedes++
			if edge.Attributes["kind"] == nil {
				t.Fatalf("precedes edge %s has no kind attribute", edge.ID)
			}
		}
	}
	if blocks != stats.CFGBlocks || precedes != stats.CFGEdges {
		t.Fatalf("published cfg blocks=%d edges=%d; stats=%d/%d", blocks, precedes, stats.CFGBlocks, stats.CFGEdges)
	}
}

func TestFlowAnalyzeValueFlowAndEnvironmentReads(t *testing.T) {
	source := `package main

import (
	"database/sql"
	"os"
)

func load() string {
	name := os.Getenv("DEMO_FEATURE")
	if name == "" {
		name = "fallback"
	}
	return name
}

func store(db *sql.DB, value string) error {
	_, err := db.Exec("INSERT INTO items (name) VALUES (?)", value)
	return err
}

func handler() error {
	db, err := sql.Open("sqlite3", "demo.db")
	if err != nil {
		return err
	}
	value := load()
	return store(db, value)
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.EnvReads != 1 {
		t.Fatalf("env reads = %d, want 1", stats.EnvReads)
	}
	if stats.Sources < 1 {
		t.Fatalf("sources = %d, want >= 1", stats.Sources)
	}
	if stats.Sinks < 1 {
		t.Fatalf("sinks = %d, want >= 1", stats.Sinks)
	}
	if stats.BindsToEdges < 1 {
		t.Fatalf("binds_to = %d, want >= 1", stats.BindsToEdges)
	}
	envFound := false
	readsFound := false
	kindByID := map[string]string{}
	for _, node := range bundle.Nodes {
		kindByID[node.ID] = node.Kind
		if node.Kind == "environment_variable" && node.Name == "DEMO_FEATURE" {
			envFound = true
		}
	}
	for _, edge := range bundle.Edges {
		if edge.Kind == "reads" && kindByID[edge.From] == "environment_variable" {
			readsFound = true
		}
	}
	if !envFound || !readsFound {
		t.Fatalf("environment node/reads missing: env=%v reads=%v", envFound, readsFound)
	}
	report := BuildReport(bundle)
	if len(report.EnvironmentVariables) != 1 || report.EnvironmentVariables[0] != "DEMO_FEATURE" {
		t.Fatalf("report env vars = %v", report.EnvironmentVariables)
	}
	if report.CallEdges == 0 || report.CallEdgesResolved == 0 {
		t.Fatalf("report call edges = %d/%d", report.CallEdges, report.CallEdgesResolved)
	}
}

func TestFlowBindsToCanonicalUnnamedParameter(t *testing.T) {
	source := `package main

func target(int) {}

func caller(value int) {
	target(value)
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.BindsToEdges != 1 {
		t.Fatalf("binds_to = %d, want 1 for the canonical unnamed parameter", stats.BindsToEdges)
	}
	parameter := parameterID(rkcmodel.StableID("node", "function", "example.com/demo.target"), 0)
	parameterNode, exists := nodeByID(bundle, parameter)
	if !exists || parameterNode.Attributes["unnamed"] != true || parameterNode.Name == "" {
		t.Fatalf("unnamed parameter node = %+v, exists=%v", parameterNode, exists)
	}
	found := false
	for _, edge := range bundle.Edges {
		if edge.Kind == "binds_to" && edge.To == parameter {
			found = true
		}
	}
	if !found {
		t.Fatal("binding to canonical unnamed parameter is missing")
	}
}

func TestFlowMapsVariadicActualsToFinalFormal(t *testing.T) {
	source := `package main

func target(prefix string, values ...int) {}

func caller() {
	target("x", 1, 2, 3)
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.BindsToEdges != 4 {
		t.Fatalf("binds_to = %d, want 4 positional bindings", stats.BindsToEdges)
	}
	targetID := rkcmodel.StableID("node", "function", "example.com/demo.target")
	variadicParameter := parameterID(targetID, 1)
	bindings := 0
	for _, edge := range bundle.Edges {
		if edge.Kind == "binds_to" && edge.To == variadicParameter {
			bindings++
		}
	}
	if bindings != 3 {
		t.Fatalf("variadic parameter bindings = %d, want 3", bindings)
	}
}

func TestFlowSourceToSinkReachability(t *testing.T) {
	source := `package main

import (
	"database/sql"
	"net/http"
)

func query(db *sql.DB, user string) (string, error) {
	var name string
	err := db.QueryRow("SELECT name FROM users WHERE id = ?", user).Scan(&name)
	return name, err
}

func handler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	user := r.FormValue("user")
	_, _ = query(db, user)
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.Sources < 1 {
		t.Fatalf("http handler sources = %d, want >= 1", stats.Sources)
	}
	report := BuildReport(bundle)
	if len(report.SourceSinkPaths) == 0 {
		t.Fatalf("no source-to-sink paths: %+v", report)
	}
	foundHandlerSource := false
	for _, path := range report.SourceSinkPaths {
		if strings.Contains(path.Source, "handler") {
			foundHandlerSource = true
		}
	}
	if !foundHandlerSource {
		t.Fatalf("source-to-sink paths do not include the handler: %+v", report.SourceSinkPaths)
	}
}

func TestFlowSanitizerDetection(t *testing.T) {
	source := `package main

import (
	"database/sql"
	"strings"
)

func sanitize(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func store(db *sql.DB, raw string) error {
	clean := sanitize(raw)
	_, err := db.Exec("INSERT INTO logs (text) VALUES (?)", clean)
	return err
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.SanitizeEdges != 0 {
		t.Fatalf("name-only sanitizer became canonical truth: %d", stats.SanitizeEdges)
	}
	report := BuildReport(bundle)
	if len(report.Sanitizers) != 0 || len(report.SanitizerHypotheses) != 1 {
		t.Fatalf("sanitizer truth/hypotheses = %v/%v", report.Sanitizers, report.SanitizerHypotheses)
	}
}

func TestFlowLineageWalks(t *testing.T) {
	source := `package main

import "os"

func origin() string {
	return os.Getenv("DATA_SOURCE")
}

func transform(value string) string {
	return value + "!suffix"
}

func consumer() string {
	return transform(origin())
}
`
	bundle, _ := flowAnalyze(t, source)
	var originID, consumerID string
	for _, node := range bundle.Nodes {
		if node.Name == "origin" {
			originID = node.ID
		}
		if node.Name == "consumer" {
			consumerID = node.ID
		}
	}
	if originID == "" || consumerID == "" {
		t.Fatalf("fixture functions missing: origin=%q consumer=%q", originID, consumerID)
	}
	origins, err := Origins(bundle, consumerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(origins.Nodes) == 0 {
		t.Fatal("consumer has no lineage")
	}
	if origins.Roles["environment_variable"] == 0 && origins.Roles["source"] == 0 {
		t.Fatalf("consumer lineage has no source origin: %+v", origins.Roles)
	}
	sinks, err := Sinks(bundle, consumerID)
	if err != nil {
		t.Fatal(err)
	}
	if sinks.Nodes == nil {
		t.Fatal("sink walk returned no nodes")
	}
	if _, err := Origins(bundle, "missing-node"); err == nil {
		t.Fatal("missing start node was accepted")
	}
	if _, err := Origins(bundle, ""); err == nil {
		t.Fatal("empty start node was accepted")
	}
}

func TestFlowPathSearch(t *testing.T) {
	source := `package main

func a() int { return 1 }
func b() int { return a() }
func c() int { return b() }
`
	bundle, _ := flowAnalyze(t, source)
	var aID, cID string
	for _, node := range bundle.Nodes {
		if node.Name == "a" && node.Kind == "function" {
			aID = node.ID
		}
		if node.Name == "c" && node.Kind == "function" {
			cID = node.ID
		}
	}
	path, err := Path(bundle, cID, aID)
	if err != nil {
		t.Fatal(err)
	}
	if !path.Found {
		t.Fatalf("path c->a not found: %+v", path)
	}
	if len(path.Nodes) < 3 || len(path.Edges) < 2 {
		t.Fatalf("path too short: %+v", path)
	}
	if _, err := Path(bundle, "", ""); err == nil {
		t.Fatal("empty path endpoints were accepted")
	}
}

func TestFlowParameterAndExpressionBranches(t *testing.T) {
	source := `package main

import "net/http"

type handler struct{ prefix string }

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw := r.FormValue("q")
	_ = raw
}

func (h handler) Callback(int, string) {}

func parens() string {
	long := "` + strings.Repeat("x", 80) + `"
	value := (long)
	return value
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.Sources < 1 {
		t.Fatalf("sources = %d, want the compiler-typed HTTP request parameter", stats.Sources)
	}
	roles := map[string]int{}
	for _, node := range bundle.Nodes {
		if node.Kind == "value" {
			if role, ok := node.Attributes["flow_role"].(string); ok {
				roles[role]++
			}
		}
	}
	if roles["literal"] < 2 {
		t.Fatalf("literal roles = %v", roles)
	}
	_ = bundle
}

func TestFlowRemainingStatementBranches(t *testing.T) {
	source := `package main

import "os"

func mixed(value any, flag bool) string {
	var name string
	if flag {
		name = os.LookupEnv("LOOKUP_VAR")[0]
	} else {
		name = os.GetenvDefault("DEFAULT_VAR", "fallback")
	}
	{
		inner := name + "!"
		_ = inner
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return "other"
	}
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.EnvReads != 1 {
		t.Fatalf("env reads = %d, want only compiler-qualified os.LookupEnv", stats.EnvReads)
	}
	envs := map[string]bool{}
	for _, node := range bundle.Nodes {
		if node.Kind == "environment_variable" {
			envs[node.Name] = true
		}
	}
	if !envs["LOOKUP_VAR"] || envs["DEFAULT_VAR"] {
		t.Fatalf("environment authority admitted a non-standard basename: %v", envs)
	}
	roles := map[string]int{}
	for _, node := range bundle.Nodes {
		if node.Kind == "value" {
			if role, ok := node.Attributes["flow_role"].(string); ok {
				roles[role]++
			}
		}
	}
	if roles["computed"] == 0 {
		t.Fatalf("computed roles missing: %v", roles)
	}
	_ = bundle
}

func TestFlowCFGStatementCoverage(t *testing.T) {
	source := `package main

import "time"

func complex(input string, values []int) int {
	var total int
	select {
	case <-time.After(time.Second):
		total++
	default:
		total--
	}
	for index, value := range values {
		if value == 0 {
			continue
		}
		if value < 0 {
			break
		}
		total += value
	}
	switch input {
	case "a":
		total += 1
		fallthrough
	case "b":
		total += 2
	default:
		total += 3
	}
	switch value := values[0]; {
	case value > 0:
		total += value
	case value < 0:
		total -= value
	}
	defer cleanup()
	go async()
	goto done
done:
	total *= 2
	return total
}

func cleanup() {}

func async() {}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.CFGFunctions != 3 {
		t.Fatalf("cfg functions = %d, want 3", stats.CFGFunctions)
	}
	if stats.CFGBlocks < 20 || stats.CFGEdges < 20 {
		t.Fatalf("cfg blocks=%d edges=%d; want a rich CFG", stats.CFGBlocks, stats.CFGEdges)
	}
	precedeKinds := map[string]bool{}
	for _, edge := range bundle.Edges {
		if edge.Kind == "precedes" {
			if kind, ok := edge.Attributes["kind"].(string); ok {
				precedeKinds[kind] = true
			}
		}
	}
	for _, kind := range []string{"branch", "join", "loop", "return", "break", "continue", "fallthrough", "goto", "exit"} {
		if !precedeKinds[kind] {
			t.Errorf("precedes kind %q missing: %v", kind, precedeKinds)
		}
	}
}

func TestFlowValueStatementCoverage(t *testing.T) {
	source := `package main

type holder struct{ inner []string }

func valueKinds(h holder) (string, error) {
	var declared = "seed"
	declared += "!"
	indexed := h.inner[0]
	sliced := h.inner[1:2]
	deref := &h
	_ = deref
	composite := holder{inner: []string{"a"}}
	asserted, ok := interface{}(composite).(holder)
	if !ok {
		return "", nil
	}
	result := declared + indexed + sliced + asserted.inner[0] + composite.inner[0]
	for key := range asserted.inner {
		_ = key
	}
	closure := func() string { return result }
	pointer := &result
	starred := *pointer
	_ = starred
	return closure(), nil
}
`
	bundle, stats := flowAnalyze(t, source)
	if stats.ValueNodes < 12 {
		t.Fatalf("value nodes = %d, want >= 12", stats.ValueNodes)
	}
	roles := map[string]int{}
	for _, node := range bundle.Nodes {
		if node.Kind == "value" {
			if role, ok := node.Attributes["flow_role"].(string); ok {
				roles[role]++
			}
		}
	}
	for _, role := range []string{"write", "literal", "field", "computed", "range", "external", "return"} {
		if roles[role] == 0 {
			t.Errorf("flow_role %q missing: %v", role, roles)
		}
	}
}

func TestFlowCFGMaxBlocksBounded(t *testing.T) {
	// A function with more statements than the per-function block bound must
	// be skipped with an explicit diagnostic, not partially compiled.
	var builder strings.Builder
	builder.WriteString("package main\n\nfunc huge() int {\n")
	for index := 0; index < 600; index++ {
		builder.WriteString("\tif true {\n\t\t_ = 1\n\t}\n")
	}
	builder.WriteString("\treturn 0\n}\n")
	root, bundle, _ := flowFixture(t, builder.String())
	fragment, stats, err := Analyze(context.Background(), Options{
		Root: root, Artifacts: bundle.Artifacts, Bundle: bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.CFGBlocks != 0 {
		t.Fatalf("oversized function compiled blocks: %d", stats.CFGBlocks)
	}
	bounded := false
	for _, diagnostic := range fragment.Diagnostics {
		if diagnostic.Code == "RKC-FLOW-2010" {
			bounded = true
		}
	}
	if !bounded {
		t.Fatal("oversized function produced no bound diagnostic")
	}
}

func TestFlowRequiresValidOptions(t *testing.T) {
	if _, _, err := Analyze(nil, Options{Root: "/tmp", Bundle: rkcmodel.Bundle{}}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, _, err := Analyze(context.Background(), Options{Root: "", Bundle: rkcmodel.Bundle{}}); err == nil {
		t.Fatal("empty root was accepted")
	}
}
