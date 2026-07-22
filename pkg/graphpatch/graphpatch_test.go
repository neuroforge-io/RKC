package graphpatch

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestValidateAcceptsWellFormedPatch(t *testing.T) {
	t.Parallel()

	patch := validPatch()
	report := Validate(patch, ValidationOptions{
		ExpectedSnapshotID: "snapshot", AllowedPluginID: "plugin", StrictVocabulary: true, RequireEvidence: true,
		Limits: Limits{MaxArtifacts: 1, MaxNodes: 1, MaxEdges: 1, MaxEvidence: 1, MaxDiagnostics: 1, MaxDocuments: 1, MaxClaims: 1},
	})
	if !report.Accepted || len(report.Issues) != 0 {
		t.Fatalf("valid patch rejected: %+v", report)
	}
}

func TestValidateReportsEnvelopeOwnershipAndAllLimitViolations(t *testing.T) {
	t.Parallel()

	patch := validPatch()
	patch.ProtocolVersion = "future"
	patch.SchemaVersion = "future"
	patch.SnapshotID = "wrong"
	patch.Producer.PluginID = ""
	patch.Fragment.Artifacts = append(patch.Fragment.Artifacts, patch.Fragment.Artifacts[0])
	patch.Fragment.Nodes = append(patch.Fragment.Nodes, patch.Fragment.Nodes[0])
	patch.Fragment.Edges = append(patch.Fragment.Edges, patch.Fragment.Edges[0])
	patch.Fragment.Evidence = append(patch.Fragment.Evidence, patch.Fragment.Evidence[0])
	patch.Fragment.Diagnostics = []rkcmodel.Diagnostic{{ID: "d1"}, {ID: "d2"}}
	patch.Fragment.Documents = []rkcmodel.Document{{ID: "d1"}, {ID: "d2"}}
	patch.Fragment.Claims = []rkcmodel.Claim{{ID: "c1"}, {ID: "c2"}}
	report := Validate(patch, ValidationOptions{
		ExpectedSnapshotID: "snapshot", AllowedPluginID: "allowed",
		Limits: Limits{MaxArtifacts: 1, MaxNodes: 1, MaxEdges: 1, MaxEvidence: 1, MaxDiagnostics: 1, MaxDocuments: 1, MaxClaims: 1},
	})
	if report.Accepted {
		t.Fatal("invalid envelope and limits were accepted")
	}
	wantCodes := map[string]int{"RKC-PATCH-001": 1, "RKC-PATCH-002": 1, "RKC-PATCH-003": 1, "RKC-PATCH-004": 1, "RKC-PATCH-005": 1, "RKC-PATCH-006": 7}
	gotCodes := issueCodeCounts(report.Issues)
	for code, want := range wantCodes {
		if gotCodes[code] != want {
			t.Errorf("issue count %s = %d, want %d; issues=%+v", code, gotCodes[code], want, report.Issues)
		}
	}
	assertIssuesSorted(t, report.Issues)
}

func TestValidateReportsMalformedRecordsVocabularyDuplicatesAndBounds(t *testing.T) {
	t.Parallel()

	patch := Patch{
		ProtocolVersion: ProtocolVersion, SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: "snapshot", Producer: Producer{PluginID: "plugin"},
		Fragment: rkcmodel.Fragment{
			Nodes: []rkcmodel.Node{
				{ID: " ", Kind: "future", Name: ""},
				{ID: "duplicate", Kind: "future", Name: ""},
				{ID: "duplicate", Kind: "function", Name: "valid"},
			},
			Edges: []rkcmodel.Edge{
				{ID: "duplicate", Kind: "future", Resolution: "future"},
				{ID: "edge", Kind: "calls", From: "a", To: "b", Resolution: "future"},
			},
			Evidence: []rkcmodel.Evidence{
				{ID: "duplicate", Confidence: -0.1},
				{ID: "too-high", Confidence: 1.1},
				{ID: " ", Confidence: .5},
			},
		},
	}
	report := Validate(patch, ValidationOptions{StrictVocabulary: true})
	if report.Accepted {
		t.Fatal("malformed local records were accepted")
	}
	wantPresent := []string{"RKC-PATCH-010", "RKC-PATCH-011", "RKC-PATCH-012", "RKC-PATCH-013", "RKC-PATCH-014", "RKC-PATCH-015", "RKC-PATCH-020", "RKC-PATCH-021"}
	counts := issueCodeCounts(report.Issues)
	for _, code := range wantPresent {
		if counts[code] == 0 {
			t.Errorf("missing %s: %+v", code, report.Issues)
		}
	}
	if counts["RKC-PATCH-015"] != 2 {
		t.Errorf("confidence bound errors = %d", counts["RKC-PATCH-015"])
	}
	assertIssuesSorted(t, report.Issues)
}

func TestValidateEvidencePolicyWarningsDoNotRejectPatch(t *testing.T) {
	t.Parallel()

	patch := Patch{
		ProtocolVersion: ProtocolVersion, SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: "snapshot", Producer: Producer{PluginID: "plugin"},
		Fragment: rkcmodel.Fragment{Nodes: []rkcmodel.Node{
			{ID: "without", Kind: "function", Name: "Without"},
			{ID: "external", Kind: "method", Name: "External", EvidenceIDs: []string{"host-evidence"}},
		}},
	}
	report := Validate(patch, ValidationOptions{RequireEvidence: true})
	if !report.Accepted || issueCodeCounts(report.Issues)["RKC-PATCH-016"] != 1 || issueCodeCounts(report.Issues)["RKC-PATCH-017"] != 1 {
		t.Fatalf("evidence warnings = %+v", report)
	}
	for _, issue := range report.Issues {
		if issue.Severity != "warning" {
			t.Errorf("policy issue should be a warning: %+v", issue)
		}
	}
}

func TestValidationReportSeverityState(t *testing.T) {
	t.Parallel()

	report := ValidationReport{Accepted: true}
	report.add("warning", "warning", "message", "object")
	if !report.Accepted {
		t.Fatal("warning rejected report")
	}
	report.add("fatal", "fatal", "message", "object")
	if report.Accepted {
		t.Fatal("fatal issue did not reject report")
	}
	if len(report.Issues) != 2 || report.Issues[0].ObjectID != "object" {
		t.Fatalf("report issues = %+v", report.Issues)
	}
}

func TestApplyCommitsCompleteCandidateTransactionallyAndCanonically(t *testing.T) {
	t.Parallel()

	bundle := baseBundle()
	patch := validPatch()
	report := Apply(&bundle, patch, ValidationOptions{ExpectedSnapshotID: "snapshot", AllowedPluginID: "plugin", StrictVocabulary: true, RequireEvidence: true})
	if !report.Accepted || len(report.Issues) != 0 {
		t.Fatalf("Apply rejected valid patch: %+v", report)
	}
	if len(bundle.Artifacts) != 2 || len(bundle.Nodes) != 2 || len(bundle.Edges) != 2 || len(bundle.Evidence) != 2 ||
		len(bundle.Diagnostics) != 2 || len(bundle.Conflicts) != 2 || len(bundle.Documents) != 2 || len(bundle.Claims) != 2 || len(bundle.Paths) != 2 {
		t.Fatalf("not every fragment family was applied: %+v", bundle)
	}
	if bundle.Artifacts[0].ID != "a-artifact" || bundle.Nodes[0].ID != "a-node" || bundle.Edges[0].ID != "a-edge" ||
		bundle.Evidence[0].ID != "a-evidence" || bundle.Diagnostics[0].ID != "a-diagnostic" || bundle.Conflicts[0].ID != "a-conflict" ||
		bundle.Documents[0].ID != "a-document" || bundle.Claims[0].ID != "a-claim" || bundle.Paths[0].ID != "a-path" {
		t.Fatalf("committed candidate was not canonicalized: %+v", bundle)
	}
	if bundle.Edges[0].Resolution != rkcmodel.ResolutionCompilerResolved {
		t.Fatalf("resolution was not normalized: %q", bundle.Edges[0].Resolution)
	}
	validation := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if validation.HasErrors() || len(validation.Diagnostics) != 0 {
		t.Fatalf("committed bundle invalid: %+v", validation.Diagnostics)
	}
}

func TestApplyRejectsEveryHostIDCollisionWithoutMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*Patch)
	}{
		{"artifact", func(p *Patch) { p.Fragment = rkcmodel.Fragment{Artifacts: []rkcmodel.Artifact{{ID: "z-artifact"}}} }},
		{"node", func(p *Patch) {
			p.Fragment = rkcmodel.Fragment{Nodes: []rkcmodel.Node{{ID: "z-node", Kind: "function", Name: "collision"}}}
		}},
		{"edge", func(p *Patch) {
			p.Fragment = rkcmodel.Fragment{Edges: []rkcmodel.Edge{{ID: "z-edge", Kind: "calls", From: "z-node", To: "z-node", Resolution: rkcmodel.ResolutionDeclared}}}
		}},
		{"evidence", func(p *Patch) {
			p.Fragment = rkcmodel.Fragment{Evidence: []rkcmodel.Evidence{{ID: "z-evidence", Confidence: .5}}}
		}},
		{"document", func(p *Patch) { p.Fragment = rkcmodel.Fragment{Documents: []rkcmodel.Document{{ID: "z-document"}}} }},
		{"claim", func(p *Patch) { p.Fragment = rkcmodel.Fragment{Claims: []rkcmodel.Claim{{ID: "z-claim"}}} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := baseBundle()
			before := deepCloneBundle(t, bundle)
			patch := emptyPatch()
			test.setup(&patch)
			report := Apply(&bundle, patch, ValidationOptions{})
			if report.Accepted || issueCodeCounts(report.Issues)["RKC-PATCH-030"] != 1 {
				t.Fatalf("collision report = %+v", report)
			}
			if !reflect.DeepEqual(bundle, before) {
				t.Fatalf("collision mutated host bundle:\nbefore=%+v\nafter=%+v", before, bundle)
			}
		})
	}
}

func TestApplyRollsBackHostValidationFailureIncludingNestedSlices(t *testing.T) {
	t.Parallel()

	bundle := baseBundle()
	// These are deliberately valid but non-canonical. Candidate sorting must not
	// reorder the host's shared backing arrays if the candidate is rejected.
	bundle.Nodes[0].EvidenceIDs = []string{"z-evidence", "a-host-evidence"}
	bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{ID: "a-host-evidence", Kind: "syntax_inferred", Method: "parser", Confidence: .8})
	bundle.Edges[0].EvidenceIDs = []string{"z-evidence", "a-host-evidence"}
	bundle.Claims[0].EvidenceIDs = []string{"z-evidence", "a-host-evidence"}
	bundle.Conflicts[0].CandidateIDs = []string{"z-node", "a-candidate"}
	bundle.Conflicts[0].EvidenceIDs = []string{"z-evidence", "a-host-evidence"}
	bundle.Documents[0].SubjectIDs = []string{"z-node", "a-subject"}
	before := deepCloneBundle(t, bundle)
	patch := emptyPatch()
	patch.Fragment.Edges = []rkcmodel.Edge{{ID: "invalid-edge", Kind: "calls", From: "missing-source", To: "missing-target", Resolution: rkcmodel.ResolutionDeclared}}
	report := Apply(&bundle, patch, ValidationOptions{})
	if report.Accepted || issueCodeCounts(report.Issues)["RKC-MOD-032"] != 1 || issueCodeCounts(report.Issues)["RKC-MOD-033"] != 1 {
		t.Fatalf("host validation failure report = %+v", report)
	}
	if !reflect.DeepEqual(bundle, before) {
		t.Fatalf("rejected candidate leaked into host:\nbefore=%+v\nafter=%+v", before, bundle)
	}
}

func TestApplyRejectsInvalidPatchBeforeHostMutation(t *testing.T) {
	t.Parallel()

	bundle := baseBundle()
	before := deepCloneBundle(t, bundle)
	patch := validPatch()
	patch.ProtocolVersion = "future"
	report := Apply(&bundle, patch, ValidationOptions{})
	if report.Accepted || issueCodeCounts(report.Issues)["RKC-PATCH-001"] != 1 {
		t.Fatalf("invalid patch report = %+v", report)
	}
	if !reflect.DeepEqual(bundle, before) {
		t.Fatal("pre-validation failure mutated host")
	}
}

func TestApplyRejectsNonJSONCandidateWithoutMutation(t *testing.T) {
	t.Parallel()

	bundle := baseBundle()
	bundle.Nodes[0].Attributes = map[string]any{"unsupported": make(chan int)}
	// A shallow snapshot is sufficient here: the marshal failure occurs before
	// canonical sorting or any possible nested mutation.
	beforeNode := bundle.Nodes[0]
	report := Apply(&bundle, emptyPatch(), ValidationOptions{})
	if report.Accepted || issueCodeCounts(report.Issues)["RKC-PATCH-031"] != 1 {
		t.Fatalf("non-JSON candidate report = %+v", report)
	}
	if bundle.Nodes[0].ID != beforeNode.ID || bundle.Nodes[0].Attributes["unsupported"] != beforeNode.Attributes["unsupported"] {
		t.Fatal("marshal failure mutated host")
	}
}

func TestApplyCommitsHostWarningsAndPatchIDsIncludesCollisionFamilies(t *testing.T) {
	t.Parallel()

	bundle := baseBundle()
	patch := emptyPatch()
	patch.Fragment.Nodes = []rkcmodel.Node{{ID: "new-node", Kind: "function", Name: "NoEvidence"}}
	report := Apply(&bundle, patch, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	counts := issueCodeCounts(report.Issues)
	if !report.Accepted || counts["RKC-PATCH-016"] != 1 || counts["RKC-MOD-025"] != 1 || len(bundle.Nodes) != 2 {
		t.Fatalf("warning-only host validation = %+v; nodes=%+v", report, bundle.Nodes)
	}
	for _, issue := range report.Issues {
		if issue.Severity != "warning" {
			t.Fatalf("warning-only validation emitted %+v", issue)
		}
	}

	all := emptyPatch()
	all.Fragment = rkcmodel.Fragment{
		Artifacts: []rkcmodel.Artifact{{ID: "artifact"}}, Nodes: []rkcmodel.Node{{ID: "node"}}, Edges: []rkcmodel.Edge{{ID: "edge"}},
		Evidence: []rkcmodel.Evidence{{ID: "evidence"}}, Documents: []rkcmodel.Document{{ID: "document"}}, Claims: []rkcmodel.Claim{{ID: "claim"}},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "diagnostic"}}, Conflicts: []rkcmodel.Conflict{{ID: "conflict"}}, Paths: []rkcmodel.ExecutionPath{{ID: "path"}},
	}
	if got := patchIDs(all); !reflect.DeepEqual(got, map[string]string{"artifact": "artifact", "node": "node", "edge": "edge", "evidence": "evidence", "document": "document", "claim": "claim"}) {
		t.Fatalf("patchIDs = %#v", got)
	}
}

func validPatch() Patch {
	patch := emptyPatch()
	patch.Fragment = rkcmodel.Fragment{
		Artifacts:   []rkcmodel.Artifact{{ID: "a-artifact", Kind: "source", Path: "new.go", Status: "included", Text: true}},
		Evidence:    []rkcmodel.Evidence{{ID: "a-evidence", Kind: "syntax_inferred", Method: "parser", Confidence: .9, Source: &rkcmodel.SourceRange{ArtifactID: "a-artifact", Path: "new.go"}}},
		Nodes:       []rkcmodel.Node{{ID: "a-node", Kind: "method", Name: "New", ArtifactID: "a-artifact", EvidenceIDs: []string{"a-evidence"}}},
		Edges:       []rkcmodel.Edge{{ID: "a-edge", Kind: "calls", From: "z-node", To: "a-node", Resolution: "resolved", EvidenceIDs: []string{"a-evidence"}}},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "a-diagnostic", Severity: "note", Code: "NOTE", Message: "note"}},
		Conflicts:   []rkcmodel.Conflict{{ID: "a-conflict", Kind: "contradiction", SubjectID: "a-node", CandidateIDs: []string{"a-node", "z-node"}, EvidenceIDs: []string{"a-evidence"}}},
		Documents:   []rkcmodel.Document{{ID: "a-document", Kind: "guide", Title: "New", SubjectIDs: []string{"a-node"}, Generator: "plugin", Status: "validated"}},
		Claims:      []rkcmodel.Claim{{ID: "a-claim", SubjectID: "a-node", Text: "New exists", Certainty: "supported", Generator: "plugin", EvidenceIDs: []string{"a-evidence"}, Validation: "accepted"}},
		Paths:       []rkcmodel.ExecutionPath{{ID: "a-path", Name: "new", EntryNodeID: "z-node", ExitNodeID: "a-node", NodeIDs: []string{"z-node", "a-node"}, EdgeIDs: []string{"a-edge"}, EvidenceIDs: []string{"a-evidence"}}},
	}
	return patch
}

func emptyPatch() Patch {
	return Patch{ProtocolVersion: ProtocolVersion, SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: "snapshot", Producer: Producer{PluginID: "plugin", Version: "1"}}
}

func baseBundle() rkcmodel.Bundle {
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion, ID: "snapshot", RootName: "repository", ContentDigest: strings.Repeat("a", 64),
			Status: "committed", Tool: rkcmodel.ToolInfo{Name: "rkc", Version: "test"},
		},
		Artifacts:   []rkcmodel.Artifact{{ID: "z-artifact", Kind: "source", Path: "old.go", Status: "included", Text: true}},
		Evidence:    []rkcmodel.Evidence{{ID: "z-evidence", Kind: "syntax_inferred", Method: "parser", Confidence: .9, Source: &rkcmodel.SourceRange{ArtifactID: "z-artifact", Path: "old.go"}}},
		Nodes:       []rkcmodel.Node{{ID: "z-node", Kind: "function", Name: "Old", ArtifactID: "z-artifact", EvidenceIDs: []string{"z-evidence"}}},
		Edges:       []rkcmodel.Edge{{ID: "z-edge", Kind: "calls", From: "z-node", To: "z-node", Resolution: rkcmodel.ResolutionDeclared, EvidenceIDs: []string{"z-evidence"}}},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "z-diagnostic", Severity: "note", Code: "NOTE", Message: "note"}},
		Conflicts:   []rkcmodel.Conflict{{ID: "z-conflict", Kind: "contradiction", SubjectID: "z-node", CandidateIDs: []string{"z-node", "z-artifact"}, EvidenceIDs: []string{"z-evidence"}}},
		Documents:   []rkcmodel.Document{{ID: "z-document", Kind: "guide", Title: "Old", SubjectIDs: []string{"z-node"}, Generator: "host", Status: "validated"}},
		Claims:      []rkcmodel.Claim{{ID: "z-claim", SubjectID: "z-node", Text: "Old exists", Certainty: "supported", Generator: "host", EvidenceIDs: []string{"z-evidence"}, Validation: "accepted"}},
		Paths:       []rkcmodel.ExecutionPath{{ID: "z-path", Name: "old", EntryNodeID: "z-node", NodeIDs: []string{"z-node"}, EvidenceIDs: []string{"z-evidence"}}},
	}
}

func deepCloneBundle(t *testing.T, bundle rkcmodel.Bundle) rkcmodel.Bundle {
	t.Helper()
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var clone rkcmodel.Bundle
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func issueCodeCounts(issues []ValidationIssue) map[string]int {
	counts := map[string]int{}
	for _, issue := range issues {
		counts[issue.Code]++
	}
	return counts
}

func assertIssuesSorted(t *testing.T, issues []ValidationIssue) {
	t.Helper()
	if !sort.SliceIsSorted(issues, func(i, j int) bool {
		left := issues[i].Code + "\x00" + issues[i].ObjectID + "\x00" + issues[i].Message
		right := issues[j].Code + "\x00" + issues[j].ObjectID + "\x00" + issues[j].Message
		return left < right
	}) {
		values := make([]string, len(issues))
		for i, issue := range issues {
			values[i] = strings.Join([]string{issue.Code, issue.ObjectID, issue.Message}, "/")
		}
		t.Fatalf("issues not sorted: %v", values)
	}
}
