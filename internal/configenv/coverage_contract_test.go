package configenv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func writeConfigContractFile(t *testing.T, root, path, content string) pluginapi.FileRef {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return pluginapi.FileRef{
		ArtifactID: rkcmodel.StableID("artifact", path),
		Path:       path,
		SizeBytes:  int64(len(content)),
	}
}

func TestRetainedTextUsageRejectsInvalidDeepAndNestedValues(t *testing.T) {
	if count, oversized := retainedTextUsage(reflect.Value{}, 8, 0); count != 0 || oversized {
		t.Fatalf("invalid reflection value = %d/%v", count, oversized)
	}
	if _, oversized := retainedTextUsage(reflect.ValueOf("safe"), 8, 17); !oversized {
		t.Fatal("excessively deep retained value was accepted")
	}
	if _, oversized := retainedTextUsage(
		reflect.ValueOf([]string{"safe", strings.Repeat("x", 9)}), 8, 0,
	); !oversized {
		t.Fatal("oversized text nested in a slice was accepted")
	}
	if _, oversized := retainedTextUsage(
		reflect.ValueOf(map[string]string{"safe": strings.Repeat("x", 9)}), 8, 0,
	); !oversized {
		t.Fatal("oversized text nested in a map was accepted")
	}
}

func TestClassifiersCoverSafeMetadataBoundaries(t *testing.T) {
	for input, want := range map[string]string{
		"":                  "empty",
		"${{ matrix.os }}":  "expression",
		"42.5":              "number",
		"https://example/":  "url",
		"../private/output": "path",
		"ordinary":          "literal",
	} {
		if got := scalarClassification(input); got != want {
			t.Errorf("scalarClassification(%q) = %q, want %q", input, got, want)
		}
	}
	for input, want := range map[string]string{
		"ruff check .":    "quality",
		"make release":    "deploy",
		"compile package": "build",
		"   ":             "empty",
		"echo bounded":    "command",
	} {
		if got := commandClassification(input); got != want {
			t.Errorf("commandClassification(%q) = %q, want %q", input, got, want)
		}
	}
	if got := buildTagNames("linux && linux && true"); !reflect.DeepEqual(got, []string{"linux"}) {
		t.Fatalf("duplicate/excluded build tags = %v", got)
	}
	if got := workflowSystem("ci/custom.yaml"); got != "ci" {
		t.Fatalf("unknown workflow system = %q", got)
	}
}

func TestExtractionDeduplicatesFactsAndStopsAtHardBudgets(t *testing.T) {
	fragment, _ := configenvFixture(t, map[string]string{
		".github/workflows/duplicate.yml": "env:\n  DUPLICATE: one\n  DUPLICATE: two\n",
	})
	if got := len(nodesByKind(fragment, "environment_variable")); got != 1 {
		t.Fatalf("duplicate environment nodes = %d, want 1", got)
	}
	configures := 0
	for _, edge := range fragment.Edges {
		if edge.Kind == "configures" && edge.To == rkcmodel.StableID("node", "environment_variable", "DUPLICATE") {
			configures++
		}
	}
	if configures != 1 {
		t.Fatalf("duplicate configures edges = %d, want 1", configures)
	}
	assertFragmentReferencesAcceptedNodes(t, fragment, rkcmodel.StableID("artifact", ".github/workflows/duplicate.yml"))

	limits := defaultExtractionLimits
	limits.retainedFacts = 1
	limited, _ := configenvFixtureWithLimits(t, map[string]string{
		".github/workflows/limited.yml": "name: limited\n",
	}, limits)
	if len(limited.Nodes) != 0 || !hasDiagnostic(limited, "RKC-CFG-3007") {
		t.Fatalf("fact-budget result = %+v", limited)
	}

	limits = defaultExtractionLimits
	limits.configNodes = 1
	bounded, _ := configenvFixtureWithLimits(t, map[string]string{
		"a.go": "//go:build linux\n\npackage a\n",
		"b.go": "//go:build darwin\n\npackage b\n",
	}, limits)
	if len(bounded.Nodes) != 1 || !hasDiagnostic(bounded, "RKC-CFG-3001") {
		t.Fatalf("outer node-bound result = %+v", bounded)
	}
}

func TestExtractionFailsClosedOnInvalidLimitsAndCancellation(t *testing.T) {
	limits := defaultExtractionLimits
	limits.retainedBytes = 0
	if _, err := extractWithLimits(context.Background(), Options{Root: t.TempDir()}, limits); err == nil {
		t.Fatal("non-positive retained-byte limit was accepted")
	}

	root := t.TempDir()
	ref := writeConfigContractFile(t, root, ".github/workflows/cancel.yml", "name: cancel\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := extractWithLimits(ctx, Options{Root: root, Files: []pluginapi.FileRef{ref}}, defaultExtractionLimits); err != context.Canceled {
		t.Fatalf("cancelled extraction = %v", err)
	}
	if err := extractWorkflow(
		ctx, root, ref, defaultExtractionLimits,
		func(rkcmodel.Node) bool { return true },
		func(string, string, string, map[string]any, ...string) {},
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		func(string, string) {},
	); err != context.Canceled {
		t.Fatalf("cancelled workflow scan = %v", err)
	}

	tf := writeConfigContractFile(t, root, "infra/cancel.tf", "resource \"x\" \"y\" {}\n")
	nodes := 0
	extractTerraform(
		ctx, root, tf, defaultExtractionLimits,
		func(rkcmodel.Node) bool { nodes++; return true },
		func(string, string, string, map[string]any, ...string) {},
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		func(string, string) {},
	)
	if nodes != 0 {
		t.Fatalf("cancelled Terraform scan emitted %d nodes", nodes)
	}
}

func TestExtractorCallbackFailuresNeverPublishPartialFacts(t *testing.T) {
	root := t.TempDir()
	legacy := writeConfigContractFile(t, root, "legacy.go", "// +build linux\n\npackage legacy\n")
	edges := 0
	extractBuildTags(
		context.Background(), root, legacy,
		func(rkcmodel.Node) bool { t.Fatal("node callback followed rejected evidence"); return true },
		func(string, string, string, map[string]any, ...string) { edges++ },
		func(string, pluginapi.FileRef, int, string) string { return "" },
	)
	if edges != 0 {
		t.Fatalf("rejected build-tag evidence emitted %d edges", edges)
	}
	extractBuildTags(
		context.Background(), root, legacy,
		func(rkcmodel.Node) bool { return false },
		func(string, string, string, map[string]any, ...string) { edges++ },
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
	)
	if edges != 0 {
		t.Fatalf("rejected build-tag node emitted %d edges", edges)
	}

	workflow := writeConfigContractFile(t, root, ".github/workflows/callback.yml", "jobs:\n  job:\n    steps:\n      - :\n      - run: |\n          make\n      - name: after\n")
	workflowNodes := 0
	workflowEdges := 0
	err := extractWorkflow(
		context.Background(), root, workflow, defaultExtractionLimits,
		func(node rkcmodel.Node) bool { workflowNodes++; return node.Attributes["kind"] != "workflow" },
		func(string, string, string, map[string]any, ...string) { workflowEdges++ },
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		func(string, string) {},
	)
	if err != nil || workflowNodes != 1 || workflowEdges != 0 {
		t.Fatalf("rejected workflow root = nodes %d edges %d err %v", workflowNodes, workflowEdges, err)
	}

	stepEvidenceCalls := 0
	err = extractWorkflow(
		context.Background(), root, workflow, defaultExtractionLimits,
		func(rkcmodel.Node) bool { return true },
		func(string, string, string, map[string]any, ...string) {},
		func(method string, _ pluginapi.FileRef, _ int, _ string) string {
			if method == "configenv.step" {
				stepEvidenceCalls++
				return ""
			}
			return method
		},
		func(string, string) {},
	)
	if err != nil || stepEvidenceCalls == 0 {
		t.Fatalf("step-evidence rejection = calls %d err %v", stepEvidenceCalls, err)
	}

	lines := []workflowLine{
		{indent: 6, text: "- run: |", number: 1},
		{indent: 8, text: "make", number: 2},
		{indent: 6, text: "- name: next", number: 3},
	}
	if got := workflowCommand(lines, 0, "|"); got != "make" {
		t.Fatalf("bounded block command = %q", got)
	}
}

func TestEnvironmentAndTerraformRejectMalformedOrOversizedFacts(t *testing.T) {
	file := pluginapi.FileRef{ArtifactID: "artifact", Path: ".github/workflows/env.yml"}
	var nodes []rkcmodel.Node
	extractEnvBlock(
		[]workflowLine{
			{indent: 0, text: "env:", number: 1},
			{indent: 2, text: "BAD NAME: value", number: 2},
			{indent: 2, text: "SAFE: ../path", number: 3},
		},
		0, file,
		func(node rkcmodel.Node) bool { nodes = append(nodes, node); return true },
		func(string, string, string, map[string]any, ...string) {},
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		"parent",
	)
	if len(nodes) != 1 || nodes[0].Name != "SAFE" || nodes[0].Attributes["default_class"] != "path" {
		t.Fatalf("environment filtering = %+v", nodes)
	}

	root := t.TempDir()
	missing := pluginapi.FileRef{Path: ".github/workflows/missing.yml"}
	if _, _, _, err := readBoundedConfigFile(root, missing, 16); err == nil {
		t.Fatal("missing configuration file was accepted")
	}

	terraform := writeConfigContractFile(t, root, "infra/main.tf", "resource \"x\" \"y\" {}\n")
	limits := defaultExtractionLimits
	limits.configFileBytes = 8
	diagnostics := 0
	extractTerraform(
		context.Background(), root, terraform, limits,
		func(rkcmodel.Node) bool { t.Fatal("oversized Terraform emitted a node"); return true },
		func(string, string, string, map[string]any, ...string) {},
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		func(code, _ string) {
			if code == "RKC-CFG-3003" {
				diagnostics++
			}
		},
	)
	if diagnostics != 1 {
		t.Fatalf("Terraform oversized diagnostics = %d", diagnostics)
	}

	limits = defaultExtractionLimits
	evidenceCalls := 0
	extractTerraform(
		context.Background(), root, terraform, limits,
		func(rkcmodel.Node) bool { t.Fatal("node followed rejected evidence"); return true },
		func(string, string, string, map[string]any, ...string) {},
		func(string, pluginapi.FileRef, int, string) string { evidenceCalls++; return "" },
		func(string, string) {},
	)
	if evidenceCalls != 1 {
		t.Fatalf("Terraform evidence calls = %d", evidenceCalls)
	}

	nodeCalls := 0
	extractTerraform(
		context.Background(), root, terraform, limits,
		func(rkcmodel.Node) bool { nodeCalls++; return false },
		func(string, string, string, map[string]any, ...string) {
			t.Fatal("edge followed rejected Terraform node")
		},
		func(string, pluginapi.FileRef, int, string) string { return "evidence" },
		func(string, string) {},
	)
	if nodeCalls != 1 {
		t.Fatalf("Terraform node calls = %d", nodeCalls)
	}
}

func TestDiagnosticFloodIsBoundedAndExplicit(t *testing.T) {
	files := make(map[string]string, maximumDiagnostics+1)
	for index := 0; index <= maximumDiagnostics; index++ {
		files[filepath.ToSlash(filepath.Join(".github", "workflows", "f"+strconv.Itoa(index)+".yml"))] = "oversized\n"
	}
	limits := defaultExtractionLimits
	limits.configFileBytes = 1
	fragment, _ := configenvFixtureWithLimits(t, files, limits)
	if len(fragment.Diagnostics) != maximumDiagnostics+1 || !hasDiagnostic(fragment, "RKC-CFG-3008") {
		t.Fatalf("diagnostic flood = %d records, summary=%v", len(fragment.Diagnostics), hasDiagnostic(fragment, "RKC-CFG-3008"))
	}
}

func TestMultiScopeEnvironmentDeclarationsAndEdgesRemainSeparatelyGrounded(t *testing.T) {
	workflow := "env:\n  SHARED_MODE: scope-one-private\njobs:\n  build:\n    env:\n      SHARED_MODE: scope-two-private\n    steps:\n      - name: Test\n        run: go test ./...\n"
	fragment, _ := configenvFixture(t, map[string]string{
		".github/workflows/grounded.yml": workflow,
		"feature.go":                     "//go:build linux\n\npackage feature\n",
		"infra/main.tf":                  "resource \"example\" \"main\" {}\n",
	})
	evidenceByID := make(map[string]rkcmodel.Evidence, len(fragment.Evidence))
	for _, evidence := range fragment.Evidence {
		evidenceByID[evidence.ID] = evidence
	}

	environmentID := rkcmodel.StableID("node", "environment_variable", "SHARED_MODE")
	var environment *rkcmodel.Node
	for index := range fragment.Nodes {
		if fragment.Nodes[index].ID == environmentID {
			environment = &fragment.Nodes[index]
			break
		}
	}
	if environment == nil {
		t.Fatal("deduplicated SHARED_MODE node is missing")
	}
	if len(environment.EvidenceIDs) != 2 || environment.Attributes["declaration_count"] != 2 {
		t.Fatalf("merged environment grounding = evidence %v attributes %+v", environment.EvidenceIDs, environment.Attributes)
	}
	declarations, ok := environment.Attributes["declarations"].([]map[string]any)
	if !ok || len(declarations) != 2 {
		t.Fatalf("environment declarations = %#v", environment.Attributes["declarations"])
	}
	declarationLines := map[int]struct{}{}
	declarationParents := map[string]struct{}{}
	declarationDefaults := map[string]struct{}{}
	for _, declaration := range declarations {
		line, lineOK := declaration["source_line"].(int)
		parent, parentOK := declaration["ci_source"].(string)
		evidenceID, evidenceOK := declaration["evidence_id"].(string)
		if !lineOK || !parentOK || !evidenceOK {
			t.Fatalf("incomplete environment declaration = %+v", declaration)
		}
		if evidence, exists := evidenceByID[evidenceID]; !exists || evidence.Source == nil || evidence.Source.StartLine != line {
			t.Fatalf("declaration evidence %q is missing or line-mismatched: %+v", evidenceID, evidence)
		}
		declarationLines[line] = struct{}{}
		declarationParents[parent] = struct{}{}
		if digest, ok := declaration["default_sha256"].(string); !ok || len(digest) != 64 {
			t.Fatalf("declaration default digest is missing: %+v", declaration)
		} else {
			declarationDefaults[digest] = struct{}{}
		}
	}
	if len(declarationLines) != 2 || len(declarationParents) != 2 || len(declarationDefaults) != 2 {
		t.Fatalf("declaration scopes collapsed: lines=%v parents=%v defaults=%v", declarationLines, declarationParents, declarationDefaults)
	}
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "scope-one-private") || strings.Contains(string(encoded), "scope-two-private") {
		t.Fatal("merged environment declarations disclosed a private default")
	}

	groundingByParent := map[string]string{}
	for _, edge := range fragment.Edges {
		if (edge.Kind == "builds" || edge.Kind == "configures") && len(edge.EvidenceIDs) == 0 {
			t.Fatalf("%s edge %s has no source-specific evidence", edge.Kind, edge.ID)
		}
		for _, evidenceID := range edge.EvidenceIDs {
			if _, exists := evidenceByID[evidenceID]; !exists {
				t.Fatalf("edge %s references missing evidence %s", edge.ID, evidenceID)
			}
		}
		if edge.Kind == "configures" && edge.To == environmentID {
			if len(edge.EvidenceIDs) != 1 {
				t.Fatalf("scope edge %s evidence = %v", edge.ID, edge.EvidenceIDs)
			}
			groundingByParent[edge.From] = edge.EvidenceIDs[0]
		}
	}
	if len(groundingByParent) != 2 {
		t.Fatalf("environment scope edges = %v, want two parents", groundingByParent)
	}
	groundingIDs := map[string]struct{}{}
	for _, evidenceID := range groundingByParent {
		groundingIDs[evidenceID] = struct{}{}
	}
	if len(groundingIDs) != 2 {
		t.Fatalf("environment scope edges collapsed onto one declaration: %v", groundingByParent)
	}

	foundEdgeOnlyRunEvidence := false
	for _, evidence := range fragment.Evidence {
		if evidence.Method != "configenv.step_run" {
			continue
		}
		for _, node := range fragment.Nodes {
			if containsEvidenceID(node.EvidenceIDs, evidence.ID) {
				t.Fatalf("run-edge evidence %s unexpectedly attached to a node", evidence.ID)
			}
		}
		for _, edge := range fragment.Edges {
			if containsEvidenceID(edge.EvidenceIDs, evidence.ID) {
				foundEdgeOnlyRunEvidence = true
			}
		}
	}
	if !foundEdgeOnlyRunEvidence {
		t.Fatal("edge-only run-command evidence was orphan-filtered")
	}
}

func containsEvidenceID(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
