package rkcmodel

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestIdentifiersAndDigestsAreStable(t *testing.T) {
	t.Parallel()

	id := StableID("node", "alpha", "beta")
	if id != StableID("node", "alpha", "beta") {
		t.Fatal("StableID changed for identical input")
	}
	if id == StableID("node", "alph", "abeta") {
		t.Fatal("StableID failed to preserve part boundaries")
	}
	if id == StableID("edge", "alpha", "beta") {
		t.Fatal("StableID failed to preserve namespaces")
	}
	if !strings.HasPrefix(id, "rkc:node:") || len(strings.TrimPrefix(id, "rkc:node:")) != 24 {
		t.Fatalf("unexpected stable ID format: %q", id)
	}

	if got, want := ContentID([]byte("abc")), "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("ContentID() = %q, want %q", got, want)
	}
	left := map[string]any{"b": 2, "a": 1}
	right := map[string]any{"a": 1, "b": 2}
	if DigestJSON(left) == "" || DigestJSON(left) != DigestJSON(right) {
		t.Fatal("DigestJSON should use deterministic JSON map ordering")
	}
	if got := DigestJSON(make(chan int)); got != "" {
		t.Fatalf("DigestJSON unsupported value = %q, want empty", got)
	}
}

func TestSortBundleCanonicalizesEveryCollection(t *testing.T) {
	t.Parallel()

	bundle := Bundle{
		Artifacts: []Artifact{{ID: "b", Path: "z"}, {ID: "b", Path: "a"}, {ID: "a", Path: "a"}},
		Nodes: []Node{
			{ID: "z", EvidenceIDs: []string{"e2", "e1"}},
			{ID: "a", EvidenceIDs: []string{"e4", "e3"}},
		},
		Edges: []Edge{
			{ID: "z", Resolution: " resolved ", EvidenceIDs: []string{"e2", "e1"}},
			{ID: "a", Resolution: "SYNTAX-INFERRED", EvidenceIDs: []string{"e4", "e3"}},
		},
		Evidence:    []Evidence{{ID: "z"}, {ID: "a"}},
		Diagnostics: []Diagnostic{{ID: "z"}, {ID: "a"}},
		Conflicts: []Conflict{
			{ID: "z", CandidateIDs: []string{"n2", "n1"}, EvidenceIDs: []string{"e2", "e1"}},
			{ID: "a"},
		},
		Documents: []Document{
			{ID: "z", SubjectIDs: []string{"n2", "n1"}, Sections: []DocumentSection{{ID: "b", Ordinal: 2}, {ID: "c", Ordinal: 1}, {ID: "a", Ordinal: 1}}},
			{ID: "a"},
		},
		Claims: []Claim{{ID: "z", EvidenceIDs: []string{"e2", "e1"}}, {ID: "a"}},
		Paths:  []ExecutionPath{{ID: "z"}, {ID: "a"}},
	}

	SortBundle(&bundle)
	if got := []string{bundle.Artifacts[0].ID, bundle.Artifacts[1].ID, bundle.Artifacts[2].ID}; !reflect.DeepEqual(got, []string{"a", "b", "b"}) {
		t.Fatalf("artifact order = %v", got)
	}
	for name, got := range map[string][]string{
		"nodes":       {bundle.Nodes[0].ID, bundle.Nodes[1].ID},
		"edges":       {bundle.Edges[0].ID, bundle.Edges[1].ID},
		"evidence":    {bundle.Evidence[0].ID, bundle.Evidence[1].ID},
		"diagnostics": {bundle.Diagnostics[0].ID, bundle.Diagnostics[1].ID},
		"conflicts":   {bundle.Conflicts[0].ID, bundle.Conflicts[1].ID},
		"documents":   {bundle.Documents[0].ID, bundle.Documents[1].ID},
		"claims":      {bundle.Claims[0].ID, bundle.Claims[1].ID},
		"paths":       {bundle.Paths[0].ID, bundle.Paths[1].ID},
	} {
		if !reflect.DeepEqual(got, []string{"a", "z"}) {
			t.Errorf("%s order = %v", name, got)
		}
	}
	if got := bundle.Edges[1].Resolution; got != ResolutionCompilerResolved {
		t.Errorf("normalized resolution = %q", got)
	}
	if got := bundle.Edges[0].Resolution; got != ResolutionSyntaxInferred {
		t.Errorf("normalized alias = %q", got)
	}
	if !sort.StringsAreSorted(bundle.Nodes[1].EvidenceIDs) || !sort.StringsAreSorted(bundle.Edges[1].EvidenceIDs) ||
		!sort.StringsAreSorted(bundle.Conflicts[1].CandidateIDs) || !sort.StringsAreSorted(bundle.Conflicts[1].EvidenceIDs) ||
		!sort.StringsAreSorted(bundle.Claims[1].EvidenceIDs) || !sort.StringsAreSorted(bundle.Documents[1].SubjectIDs) {
		t.Fatal("nested canonical ID lists were not sorted")
	}
	sections := bundle.Documents[1].Sections
	if got := []string{sections[0].ID, sections[1].ID, sections[2].ID}; !reflect.DeepEqual(got, []string{"a", "c", "b"}) {
		t.Fatalf("document section order = %v", got)
	}
}

func TestCanonicalBundleIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 7, 22, 1, 2, 3, 4, time.FixedZone("local", 10*60*60))
	bundle := Bundle{
		Snapshot: Snapshot{
			ID: "snapshot", CreatedAt: created, RootPath: "/machine/local",
			Metadata: map[string]string{"host": "alpha", "pid": "42", "duration_ms": "9", "keep": "yes"},
		},
		Nodes: []Node{{ID: "b", Kind: "function", Name: "B"}, {ID: "a", Kind: "function", Name: "A"}},
		Edges: []Edge{{ID: "edge", Resolution: "resolved"}},
	}
	canonical := CanonicalBundle(bundle)
	if !canonical.Snapshot.CreatedAt.IsZero() || canonical.Snapshot.RootPath != "" {
		t.Fatalf("machine-local snapshot fields retained: %+v", canonical.Snapshot)
	}
	if !reflect.DeepEqual(canonical.Snapshot.Metadata, map[string]string{"keep": "yes"}) {
		t.Fatalf("metadata = %#v", canonical.Snapshot.Metadata)
	}
	if bundle.Snapshot.CreatedAt != created || bundle.Snapshot.RootPath != "/machine/local" || len(bundle.Snapshot.Metadata) != 4 || bundle.Nodes[0].ID != "b" {
		t.Fatal("CanonicalBundle mutated its input")
	}

	variant := bundle
	variant.Snapshot.CreatedAt = created.Add(time.Hour)
	variant.Snapshot.RootPath = "/other/host"
	variant.Snapshot.Metadata = map[string]string{"host": "beta", "pid": "77", "duration_ms": "900", "keep": "yes"}
	variant.Nodes = []Node{bundle.Nodes[1], bundle.Nodes[0]}
	if CanonicalDigest(bundle) == "" || CanonicalDigest(bundle) != CanonicalDigest(variant) {
		t.Fatal("canonical digest changed for operational metadata or input ordering")
	}
	encoded, err := CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Bundle
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Snapshot.RootPath != "" {
		t.Fatalf("CanonicalJSON produced invalid/non-canonical JSON: %v", err)
	}
}

func TestSortFragmentUsesBundleCanonicalRules(t *testing.T) {
	t.Parallel()

	fragment := Fragment{
		Artifacts:   []Artifact{{ID: "b", Path: "b"}, {ID: "a", Path: "a"}},
		Nodes:       []Node{{ID: "b"}, {ID: "a"}},
		Edges:       []Edge{{ID: "b", Resolution: "observed"}, {ID: "a", Resolution: "resolved"}},
		Evidence:    []Evidence{{ID: "b"}, {ID: "a"}},
		Diagnostics: []Diagnostic{{ID: "b"}, {ID: "a"}},
		Conflicts:   []Conflict{{ID: "b"}, {ID: "a"}},
		Documents:   []Document{{ID: "b"}, {ID: "a"}},
		Claims:      []Claim{{ID: "b"}, {ID: "a"}},
		Paths:       []ExecutionPath{{ID: "b"}, {ID: "a"}},
	}
	SortFragment(&fragment)
	if fragment.Artifacts[0].ID != "a" || fragment.Nodes[0].ID != "a" || fragment.Edges[0].ID != "a" ||
		fragment.Evidence[0].ID != "a" || fragment.Diagnostics[0].ID != "a" || fragment.Conflicts[0].ID != "a" ||
		fragment.Documents[0].ID != "a" || fragment.Claims[0].ID != "a" || fragment.Paths[0].ID != "a" {
		t.Fatalf("fragment was not canonicalized: %+v", fragment)
	}
}

func TestBuildCoverageAccountsForEvidenceDocumentationAndStatuses(t *testing.T) {
	t.Parallel()

	bundle := Bundle{
		Snapshot: Snapshot{ID: "s"},
		Artifacts: []Artifact{
			{ID: "parsed", Text: true, Status: "parsed"},
			{ID: "syntax", Text: true, Status: " syntax_parsed "},
			{ID: "semantic", Text: true, Status: "semantic_parsed"},
			{ID: "attr-semantic", Text: true, Status: "included", Attributes: map[string]string{"semantic_parsed": "true"}},
			{ID: "excluded", Text: true, Status: "excluded"},
			{ID: "inventory", Text: true, Status: "inventory_only"},
			{ID: "oversized", Text: true, Status: "oversized"},
			{ID: "unsupported", Text: true, Status: "unsupported"},
			{ID: "redacted", Text: true, Status: "redacted"},
			{ID: "binary", Status: "binary"},
			{ID: "unreadable", Text: true, Status: "unreadable"},
		},
		Documents: []Document{
			{ID: "doc", Status: "validated", SubjectIDs: []string{"public-generated"}},
			{ID: "rejected", Status: "rejected", SubjectIDs: []string{"public-rejected"}},
			{ID: "stale", Status: "stale", SubjectIDs: []string{"public-stale"}},
		},
		Claims: []Claim{{ID: "c1", EvidenceIDs: []string{"e"}}, {ID: "c2"}},
		Nodes: []Node{
			{ID: "public-generated", Kind: "function", PublicSurface: true, EvidenceIDs: []string{"e"}},
			{ID: "public-docstring", Kind: "method", Visibility: "public", Attributes: map[string]any{"docstring": " docs "}},
			{ID: "public-documentation", Kind: "class", Visibility: "exported", Attributes: map[string]any{"documentation": []byte("guide")}},
			{ID: "public-rejected", Kind: "interface", PublicSurface: true},
			{ID: "public-stale", Kind: "type", PublicSurface: true},
			{ID: "private", Kind: "variable", EvidenceIDs: []string{"e"}},
			{ID: "secret-high-number", Kind: "secret", Attributes: map[string]any{"confidence": 0.91}},
			{ID: "secret-high-class", Kind: "secret", Attributes: map[string]any{"confidence_class": "high"}},
			{ID: "secret-low", Kind: "secret", Attributes: map[string]any{"confidence": "0.2"}},
		},
		Edges: []Edge{
			{ID: "declared", Kind: "calls", Resolution: ResolutionDeclared},
			{ID: "resolved-alias", Kind: "calls", Resolution: "resolved"},
			{ID: "inferred", Kind: "imports", Resolution: ResolutionSyntaxInferred},
			{ID: "unknown", Kind: "related_to", Resolution: "future"},
		},
		Evidence:    []Evidence{{ID: "e", Kind: "syntax_inferred"}, {ID: "m", Kind: "manifest"}},
		Diagnostics: []Diagnostic{{ID: "d1", Severity: "warning"}, {ID: "d2", Severity: "warning"}, {ID: "d3", Severity: "error"}},
		Conflicts:   []Conflict{{ID: "conflict"}},
	}
	got := BuildCoverage(bundle)
	if got.SnapshotID != "s" || got.ArtifactsEncountered != 11 || got.ArtifactsInventoried != 11 || got.TextArtifacts != 10 {
		t.Fatalf("artifact totals: %+v", got)
	}
	if got.ArtifactsSyntacticallyParsed != 3 || got.ArtifactsSemanticallyParsed != 2 || got.ArtifactsExcluded != 5 || got.ArtifactsBinary != 1 || got.ArtifactsUnreadable != 1 {
		t.Fatalf("artifact accounting: %+v", got)
	}
	if got.NodesTotal != 9 || got.SymbolsTotal != 6 || got.SymbolsWithEvidence != 2 || got.PublicSymbols != 5 || got.PublicSymbolsDocumented != 3 {
		t.Fatalf("node accounting: %+v", got)
	}
	if got.SecretFindings != 3 || got.HighConfidenceSecretFindings != 2 {
		t.Fatalf("secret accounting: %+v", got)
	}
	if got.ResolvedEdges != 2 || got.UnresolvedEdges != 2 || got.ClaimsTotal != 2 || got.ClaimsWithEvidence != 1 || got.ConflictsTotal != 1 {
		t.Fatalf("edge/claim accounting: %+v", got)
	}
	if got.InventoryAccountingRatio != 1 || got.SyntacticParseRatio != .75 || got.SemanticParseRatio != .5 ||
		got.SymbolEvidenceRatio != 2.0/6.0 || got.PublicDocumentationRatio != .6 || got.EdgeResolutionRatio != .5 || got.ClaimCitationRatio != .5 {
		t.Fatalf("coverage ratios: %+v", got)
	}
	if got.DiagnosticsBySeverity["warning"] != 2 || got.NodeKinds["secret"] != 3 || got.EdgeKinds["calls"] != 2 ||
		got.EvidenceKinds["manifest"] != 1 || got.ArtifactStatuses["syntax_parsed"] != 1 || got.DeterministicOutputDigest == "" {
		t.Fatalf("coverage maps/digest: %+v", got)
	}
}

func TestCoverageAndAttributeHelpersHandleZeroAndMalformedValues(t *testing.T) {
	t.Parallel()

	empty := BuildCoverage(Bundle{})
	if empty.InventoryAccountingRatio != 1 || empty.SyntacticParseRatio != 1 || empty.SemanticParseRatio != 1 ||
		empty.SymbolEvidenceRatio != 1 || empty.PublicDocumentationRatio != 1 || empty.EdgeResolutionRatio != 1 || empty.ClaimCitationRatio != 1 {
		t.Fatalf("zero-denominator ratios = %+v", empty)
	}
	if ratio(1, 4) != .25 {
		t.Fatal("ratio did not divide")
	}
	for _, test := range []struct {
		value any
		want  bool
	}{{true, true}, {"true", true}, {"not-bool", false}, {1, false}} {
		if got := boolString(test.value); got != test.want {
			t.Errorf("boolString(%#v) = %v", test.value, got)
		}
	}
	values := map[string]any{"string": "value", "bytes": []byte("bytes"), "number": 3}
	if attributeString(values, "string") != "value" || attributeString(values, "bytes") != "bytes" || attributeString(values, "number") != "" || attributeString(nil, "x") != "" {
		t.Fatal("attributeString conversion mismatch")
	}
	floatValues := map[string]any{"f64": float64(1.5), "f32": float32(2.5), "int": int(3), "i64": int64(4), "string": "5.5", "bad": "x", "bool": true}
	for key, want := range map[string]float64{"f64": 1.5, "f32": 2.5, "int": 3, "i64": 4, "string": 5.5, "bad": 0, "bool": 0, "missing": 0} {
		if got := attributeFloat(floatValues, key); got != want {
			t.Errorf("attributeFloat(%q) = %v, want %v", key, got, want)
		}
	}
	if attributeFloat(nil, "x") != 0 {
		t.Fatal("nil attributes should produce zero")
	}
}

func TestValidateBundleAcceptsConsistentBundle(t *testing.T) {
	t.Parallel()

	bundle := validBundleForTest()
	report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() || len(report.Diagnostics) != 0 {
		t.Fatalf("valid bundle rejected: %+v", report.Diagnostics)
	}
	if (ValidationReport{Diagnostics: []Diagnostic{{Severity: "warning"}}}).HasErrors() {
		t.Fatal("warning should not count as an error")
	}
	if !(ValidationReport{Diagnostics: []Diagnostic{{Severity: "fatal"}}}).HasErrors() || !(ValidationReport{Diagnostics: []Diagnostic{{Severity: "error"}}}).HasErrors() {
		t.Fatal("fatal and error diagnostics must count as errors")
	}
}

func TestValidateBundleReportsMalformedReferencesVocabularyAndDuplicates(t *testing.T) {
	t.Parallel()

	bundle := Bundle{
		Artifacts: []Artifact{
			{Path: "empty"},
			{ID: "artifact", Status: "future", Kind: "future", SizeBytes: -1, LineCount: -1, SHA256: "bad"},
			{ID: "artifact", Path: "artifact.go", Kind: "source", Status: "included"},
		},
		Evidence: []Evidence{
			{Method: "empty"},
			{ID: "evidence", Kind: "future", Confidence: math.NaN(), Source: &SourceRange{ArtifactID: "missing-artifact", StartByte: -1, EndByte: -2, StartLine: -1, EndLine: -1, StartColumn: -1, EndColumn: -1}},
			{ID: "evidence", Kind: "manifest", Method: "parser", Confidence: 2},
			{ID: "negative", Kind: "manifest", Method: "parser", Confidence: -0.1},
		},
		Nodes: []Node{
			{QualifiedName: "empty"},
			{ID: "node", Kind: "future", ArtifactID: "missing-artifact", EvidenceIDs: []string{"missing-evidence"}, Source: &SourceRange{ArtifactID: "other-artifact", StartByte: -1, StartLine: -1}},
			{ID: "node", Kind: "function", Name: "duplicate"},
			{ID: "node2", Kind: "method", Name: "NoEvidence"},
		},
		Edges: []Edge{
			{Kind: "empty"},
			{ID: "edge", Kind: "future", From: "missing-source", To: "missing-target", Resolution: "future", Confidence: math.NaN(), EvidenceIDs: []string{"missing-evidence"}},
			{ID: "edge", Kind: "calls", From: "node", To: "node2", Resolution: ResolutionDeclared, Confidence: 2},
		},
		Claims: []Claim{
			{SubjectID: "empty"},
			{ID: "claim", SubjectID: "missing-subject", EvidenceIDs: []string{"missing-evidence"}, Certainty: "future", Validation: "future"},
			{ID: "claim", SubjectID: "node"},
		},
		Diagnostics: []Diagnostic{
			{Code: "empty"},
			{ID: "diagnostic", Severity: "future", Source: &SourceRange{ArtifactID: "missing-artifact", Path: "wrong.go"}},
			{ID: "diagnostic", Severity: "note", Code: "DUP", Message: "duplicate"},
		},
		Conflicts: []Conflict{
			{SubjectID: "empty"},
			{ID: "conflict", SubjectID: "missing-subject", CandidateIDs: []string{"missing-candidate"}, PreferredID: "not-a-candidate", EvidenceIDs: []string{"missing-evidence"}},
			{ID: "conflict", Kind: "duplicate", SubjectID: "node", CandidateIDs: []string{"node", "artifact"}},
		},
		Documents: []Document{
			{Path: "empty"},
			{ID: "document", Status: "future", ContentSHA256: "bad", SubjectIDs: []string{"missing-subject"}, Sections: []DocumentSection{
				{Ordinal: 0},
				{ID: "section", ParentID: "missing-parent", Ordinal: -1, EvidenceIDs: []string{"missing-evidence"}, ClaimIDs: []string{"missing-claim"}},
				{ID: "section", Ordinal: 1, PlainText: "duplicate"},
			}},
			{ID: "document", Kind: "guide", Title: "Duplicate", Generator: "test", Status: "draft"},
		},
		Paths: []ExecutionPath{
			{Name: "empty"},
			{ID: "path"},
			{ID: "path", Name: "broken", EntryNodeID: "wrong-entry", ExitNodeID: "wrong-exit", NodeIDs: []string{"node", "missing-node"}, EdgeIDs: []string{"missing-edge"}, EvidenceIDs: []string{"missing-evidence"}},
			{ID: "path-count", Name: "count", EntryNodeID: "node", ExitNodeID: "node2", NodeIDs: []string{"node", "node2"}},
			{ID: "path-edge", Name: "edge", EntryNodeID: "node", ExitNodeID: "node2", NodeIDs: []string{"node", "node2"}, EdgeIDs: []string{"edge"}},
		},
	}
	report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	wantCodes := []string{
		"RKC-MOD-001", "RKC-MOD-002", "RKC-MOD-003", "RKC-MOD-004", "RKC-MOD-005", "RKC-MOD-006", "RKC-MOD-007",
		"RKC-MOD-010", "RKC-MOD-011", "RKC-MOD-012", "RKC-MOD-013", "RKC-MOD-014", "RKC-MOD-015", "RKC-MOD-016",
		"RKC-MOD-020", "RKC-MOD-021", "RKC-MOD-022", "RKC-MOD-023", "RKC-MOD-024", "RKC-MOD-025", "RKC-MOD-026", "RKC-MOD-027", "RKC-MOD-028",
		"RKC-MOD-030", "RKC-MOD-031", "RKC-MOD-032", "RKC-MOD-033", "RKC-MOD-034", "RKC-MOD-035", "RKC-MOD-036", "RKC-MOD-037",
		"RKC-MOD-040", "RKC-MOD-041", "RKC-MOD-042", "RKC-MOD-043", "RKC-MOD-044", "RKC-MOD-045", "RKC-MOD-046", "RKC-MOD-047",
		"RKC-MOD-050", "RKC-MOD-051", "RKC-MOD-052", "RKC-MOD-053", "RKC-MOD-054", "RKC-MOD-055",
		"RKC-MOD-060", "RKC-MOD-061", "RKC-MOD-062", "RKC-MOD-063", "RKC-MOD-064",
		"RKC-MOD-070", "RKC-MOD-071", "RKC-MOD-072", "RKC-MOD-073", "RKC-MOD-074", "RKC-MOD-075", "RKC-MOD-076", "RKC-MOD-077",
		"RKC-MOD-080", "RKC-MOD-081", "RKC-MOD-082", "RKC-MOD-083", "RKC-MOD-084", "RKC-MOD-085", "RKC-MOD-086", "RKC-MOD-087", "RKC-MOD-088", "RKC-MOD-089", "RKC-MOD-090", "RKC-MOD-091",
		"RKC-MOD-100", "RKC-MOD-101", "RKC-MOD-102", "RKC-MOD-103", "RKC-MOD-104", "RKC-MOD-105", "RKC-MOD-106", "RKC-MOD-107", "RKC-MOD-108", "RKC-MOD-109",
	}
	seen := map[string]bool{}
	for index, diagnostic := range report.Diagnostics {
		seen[diagnostic.Code] = true
		if diagnostic.Stage != "model_validate" || diagnostic.Message == "" || !strings.HasPrefix(diagnostic.ID, "rkc:diagnostic:") {
			t.Errorf("malformed validation diagnostic: %+v", diagnostic)
		}
		if index > 0 && report.Diagnostics[index-1].ID > diagnostic.ID {
			t.Error("diagnostics are not deterministically sorted by ID")
		}
	}
	for _, code := range wantCodes {
		if !seen[code] {
			t.Errorf("missing diagnostic code %s; got %#v", code, seen)
		}
	}
	if !report.HasErrors() {
		t.Fatal("malformed bundle should have errors")
	}
}

func TestVocabularyClassificationAndNormalization(t *testing.T) {
	t.Parallel()

	if !IsKnownNodeKind("function") || IsKnownNodeKind("future") || !IsKnownEdgeKind("calls") || IsKnownEdgeKind("future") ||
		!IsKnownArtifactKind("source") || IsKnownArtifactKind("future") || !IsKnownArtifactStatus("included") || IsKnownArtifactStatus("future") ||
		!IsKnownEvidenceKind("manifest") || IsKnownEvidenceKind("future") || !IsKnownSeverity("fatal") || IsKnownSeverity("future") ||
		!IsKnownDocumentStatus("validated") || IsKnownDocumentStatus("future") || !IsKnownClaimValidation("accepted") || IsKnownClaimValidation("future") ||
		!IsKnownClaimCertainty("supported") || IsKnownClaimCertainty("future") || !IsKnownSnapshotStatus("committed") || IsKnownSnapshotStatus("future") {
		t.Fatal("known vocabulary membership mismatch")
	}
	for input, want := range map[string]string{
		" Resolved ":       ResolutionCompilerResolved,
		"OBSERVED":         ResolutionRuntimeObserved,
		"syntax-inferred":  ResolutionSyntaxInferred,
		"runtime-observed": ResolutionRuntimeObserved,
		"model_inferred":   ResolutionModelInferred,
		" unknown-future ": "unknown-future",
	} {
		if got := NormalizeResolution(input); got != want {
			t.Errorf("NormalizeResolution(%q) = %q, want %q", input, got, want)
		}
	}
	for _, value := range []string{ResolutionDeclared, ResolutionCompilerResolved, ResolutionSyntaxInferred, ResolutionRuntimeObserved, ResolutionDocumentationAsserted, ResolutionModelInferred, ResolutionUnresolved, "resolved"} {
		if !IsKnownResolution(value) {
			t.Errorf("known resolution %q rejected", value)
		}
	}
	if IsKnownResolution("future") {
		t.Fatal("unknown resolution accepted")
	}
	for value, want := range map[string]bool{
		ResolutionDeclared: true, ResolutionCompilerResolved: true, ResolutionRuntimeObserved: true,
		"resolved": true, ResolutionSyntaxInferred: false, ResolutionDocumentationAsserted: false,
		ResolutionModelInferred: false, ResolutionUnresolved: false, "future": false,
	} {
		if got := IsResolvedResolution(value); got != want {
			t.Errorf("IsResolvedResolution(%q) = %v, want %v", value, got, want)
		}
	}
	for kind := range NodeKinds {
		if got := IsSymbolKind(kind); got && kind == "repository" {
			t.Fatal("repository must not be a symbol")
		}
	}
	if !IsSymbolKind("function") || !IsSymbolKind("deployment") || IsSymbolKind("repository") || IsSymbolKind("future") {
		t.Fatal("symbol classification mismatch")
	}
}

func TestValidationHelpersCoverSourceAgreementAndEmptySubjects(t *testing.T) {
	t.Parallel()

	var diagnostics []Diagnostic
	validateSourceRange(&diagnostics, &SourceRange{
		ArtifactID: "artifact", Path: "other.go",
		StartLine: 1, EndLine: 1, StartColumn: 5, EndColumn: 4,
	}, map[string]Artifact{"artifact": {ID: "artifact", Path: "expected.go"}}, "TEST", "subject")
	if len(diagnostics) != 2 {
		t.Fatalf("source column/path disagreement diagnostics = %+v", diagnostics)
	}
	if knownSubject("", map[string]struct{}{"": {}}) {
		t.Fatal("empty ID must never be a known subject")
	}
}

func validBundleForTest() Bundle {
	return Bundle{
		Snapshot: Snapshot{
			SchemaVersion: SchemaVersion, ID: "snapshot", RootName: "repository",
			ContentDigest: strings.Repeat("a", 64), Status: "committed", Tool: ToolInfo{Name: "rkc", Version: "test"},
		},
		Artifacts: []Artifact{{ID: "artifact", Kind: "source", Path: "main.go", Status: "included", SHA256: strings.Repeat("b", 64)}},
		Evidence: []Evidence{{
			ID: "evidence", Kind: "syntax_inferred", Method: "parser", Confidence: 0.9,
			Source: &SourceRange{ArtifactID: "artifact", Path: "main.go"},
		}},
		Nodes:       []Node{{ID: "node", Kind: "function", Name: "main", ArtifactID: "artifact", Source: &SourceRange{ArtifactID: "artifact", Path: "main.go"}, EvidenceIDs: []string{"evidence"}}},
		Edges:       []Edge{{ID: "edge", Kind: "calls", From: "node", To: "node", Resolution: ResolutionDeclared, EvidenceIDs: []string{"evidence"}}},
		Claims:      []Claim{{ID: "claim", SubjectID: "node", Text: "main exists", Certainty: "supported", Generator: "test", EvidenceIDs: []string{"evidence"}, Validation: "accepted"}},
		Diagnostics: []Diagnostic{{ID: "diagnostic", Severity: "note", Code: "TEST", Message: "valid", Source: &SourceRange{ArtifactID: "artifact", Path: "main.go"}}},
		Conflicts:   []Conflict{{ID: "conflict", Kind: "contradiction", SubjectID: "node", CandidateIDs: []string{"node", "artifact"}, PreferredID: "node", EvidenceIDs: []string{"evidence"}}},
		Documents: []Document{{
			ID: "document", Kind: "guide", Title: "Guide", Path: "GUIDE.md", SubjectIDs: []string{"node"}, Generator: "test",
			ContentSHA256: strings.Repeat("c", 64), Status: "validated",
			Sections: []DocumentSection{
				{ID: "section-1", Ordinal: 0, Markdown: "# Main", ClaimIDs: []string{"claim"}, EvidenceIDs: []string{"evidence"}},
				{ID: "section-2", ParentID: "section-1", Ordinal: 1, PlainText: "Details"},
				{ID: "section-3", ParentID: "node", Ordinal: 2, PlainText: "Node details"},
			},
		}},
		Paths: []ExecutionPath{{ID: "path", Name: "self", EntryNodeID: "node", ExitNodeID: "node", NodeIDs: []string{"node"}, EvidenceIDs: []string{"evidence"}}},
	}
}
