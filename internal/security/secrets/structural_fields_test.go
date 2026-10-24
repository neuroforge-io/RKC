package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSanitizeBundlePreservesCanonicalStructureWhileMaskingMatchingPayload(t *testing.T) {
	t.Parallel()
	bundle, digest, source, literals := structuralFixture(t)
	validation := rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true}
	if report := rkcmodel.ValidateBundle(bundle, validation); report.HasErrors() {
		t.Fatalf("invalid fixture before redaction: %+v", report.Diagnostics)
	}
	snapshot := bundle.Snapshot
	artifacts := append([]rkcmodel.Artifact(nil), bundle.Artifacts...)
	edge := bundle.Edges[0]
	if count := SanitizeBundle(&bundle, literals); count == 0 {
		t.Fatal("matching source values did not exercise payload redaction")
	}
	if !reflect.DeepEqual(bundle.Snapshot, snapshot) || !reflect.DeepEqual(bundle.Artifacts, artifacts) || !reflect.DeepEqual(bundle.Edges[0], edge) {
		t.Fatal("source credential literals changed canonical identity, digests, or vocabularies")
	}
	node, evidence, document, claim := bundle.Nodes[0], bundle.Evidence[0], bundle.Documents[0], bundle.Claims[0]
	if node.Kind != "function" || node.SemanticHash != digest || evidence.Kind != "documentation_asserted" || evidence.InputDigest != digest ||
		document.ContentSHA256 != digest || document.Status != "validated" || claim.Certainty != "supported" || claim.Validation != "accepted" || bundle.Diagnostics[0].Severity != "warning" {
		t.Fatal("valid structural metadata was treated as free-form credential payload")
	}
	if node.Signature != redactionToken+" "+redactionToken || node.Attributes["sha256"] != redactionToken || node.Attributes["kind"] != redactionToken ||
		evidence.Detail != redactionToken || claim.Text != redactionToken || bundle.Diagnostics[0].Message != redactionToken {
		t.Fatal("structural protection leaked into free-form text or generic attributes")
	}
	for _, section := range document.Sections {
		for _, body := range []string{section.Markdown, section.PlainText} {
			if len(body) != len(source) {
				t.Fatal("source-body redaction changed byte accounting")
			}
			for _, finding := range Scan(source) {
				if body[finding.StartByte:finding.EndByte] != strings.Repeat("*", finding.EndByte-finding.StartByte) {
					t.Fatal("source-body redaction retained original credential bytes")
				}
			}
			for _, literal := range literals {
				if strings.Contains(body, literal) {
					t.Fatal("source-body credential value survived redaction")
				}
			}
		}
	}
	if report := rkcmodel.ValidateBundle(bundle, validation); report.HasErrors() {
		t.Fatalf("redaction invalidated a valid canonical bundle: %+v", report.Diagnostics)
	}
}

func TestMalformedStructuralValuesStillSanitizeAndFailStrictValidation(t *testing.T) {
	t.Parallel()
	bundle, digest, _, literals := structuralFixture(t)
	badDigest := digest + "-invalid"
	bundle.Snapshot.ContentDigest = badDigest
	bundle.Snapshot.ConfigDigest = badDigest
	bundle.Snapshot.PolicyDigest = badDigest
	bundle.Snapshot.PluginLockDigest = badDigest
	bundle.Snapshot.ToolchainDigest = badDigest
	bundle.Snapshot.Git.WorkingTreeDigest = badDigest
	bundle.Snapshot.Status = "unknown_building"
	bundle.Artifacts[0].SHA256 = badDigest
	bundle.Artifacts[0].Kind = "unknown_directory"
	bundle.Artifacts[0].Status = "unknown_syntax_parsed"
	bundle.Nodes[0].Kind = "unknown_function"
	bundle.Nodes[0].SemanticHash = badDigest
	bundle.Edges[0].Kind = "unknown_declares"
	bundle.Edges[0].Resolution = "unknown_compiler_resolved"
	bundle.Evidence[0].Kind = "unknown_documentation_asserted"
	bundle.Evidence[0].InputDigest = badDigest
	bundle.Documents[0].ContentSHA256 = badDigest
	bundle.Documents[0].Status = "unknown_validated"
	bundle.Claims[0].Certainty = "unknown_supported"
	bundle.Claims[0].Validation = "unknown_accepted"
	bundle.Diagnostics[0].Severity = "unknown_warning"
	literals = append(literals, "unknown_warning")
	SanitizeBundle(&bundle, literals)
	values := []string{bundle.Snapshot.ContentDigest, bundle.Snapshot.ConfigDigest, bundle.Snapshot.PolicyDigest,
		bundle.Snapshot.PluginLockDigest, bundle.Snapshot.ToolchainDigest, bundle.Snapshot.Git.WorkingTreeDigest,
		bundle.Snapshot.Status, bundle.Artifacts[0].SHA256, bundle.Artifacts[0].Kind, bundle.Artifacts[0].Status,
		bundle.Nodes[0].Kind, bundle.Nodes[0].SemanticHash, bundle.Edges[0].Kind, bundle.Edges[0].Resolution,
		bundle.Evidence[0].Kind, bundle.Evidence[0].InputDigest, bundle.Documents[0].ContentSHA256,
		bundle.Documents[0].Status, bundle.Claims[0].Certainty, bundle.Claims[0].Validation, bundle.Diagnostics[0].Severity}
	for index, value := range values {
		if !strings.Contains(value, redactionToken) {
			t.Errorf("malformed structural field %d escaped ordinary sanitization", index)
		}
	}
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	codes := map[string]bool{}
	for _, diagnostic := range report.Diagnostics {
		codes[diagnostic.Code] = true
	}
	for _, code := range []string{"RKC-MOD-007", "RKC-MOD-022", "RKC-MOD-053", "RKC-MOD-084"} {
		if !codes[code] {
			t.Errorf("strict validator no longer rejects malformed structural value: missing %s", code)
		}
	}
}

func TestStructuralBoundaryDoesNotExtendToMapsArbitraryStructsOrIDs(t *testing.T) {
	t.Parallel()
	digestBytes := sha256.Sum256([]byte("payload boundary fixture"))
	digest := hex.EncodeToString(digestBytes[:])
	id := rkcmodel.StableID("artifact", "fixture")
	payload := &rkcmodel.Artifact{SHA256: digest, Kind: "directory"}
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: id, SHA256: digest}},
		Nodes: []rkcmodel.Node{{Kind: "function", Name: "function", Attributes: map[string]any{
			"sha256": digest, "kind": "function", "password": digest,
			"nested_typed_payload": payload,
			"nested_slice":         []any{rkcmodel.Node{Kind: "function", SemanticHash: digest}},
			digest:                 "ordinary value",
		}}},
	}
	SanitizeBundle(&bundle, []string{digest, "function", "directory", "artifact"})
	if bundle.Artifacts[0].SHA256 != digest || bundle.Nodes[0].Kind != "function" {
		t.Fatal("canonical structural metadata was not retained")
	}
	if bundle.Artifacts[0].ID != strings.ReplaceAll(id, "artifact", redactionToken) || bundle.Nodes[0].Name != redactionToken {
		t.Fatal("an ID or display string gained an unrequested structural exemption")
	}
	attributes := bundle.Nodes[0].Attributes
	for _, key := range []string{"sha256", "kind", "password"} {
		if attributes[key] != redactionToken {
			t.Errorf("open attribute %s was mistaken for canonical metadata", key)
		}
	}
	if attributes[redactionToken] != "ordinary value" {
		t.Fatal("digest-shaped generic map key was not sanitized")
	}
	if payload.SHA256 != redactionToken || payload.Kind != redactionToken {
		t.Fatal("a typed struct nested inside generic payload escaped redaction")
	}
	nested := attributes["nested_slice"].([]any)[0].(rkcmodel.Node)
	if nested.Kind != redactionToken || nested.SemanticHash != redactionToken {
		t.Fatal("an interface/slice payload regained structural privileges")
	}
	// A distinct Go type with identical field names is not canonical metadata.
	type imitationArtifact rkcmodel.Artifact
	imitation := imitationArtifact{SHA256: digest, Kind: "directory"}
	count := 0
	redactReflect(reflect.ValueOf(&imitation), []string{digest, "directory"}, &count)
	if imitation.SHA256 != redactionToken || imitation.Kind != redactionToken || count != 2 {
		t.Fatal("field names on an arbitrary struct conferred a structural exemption")
	}
}

func TestTypedDigestShapesAndCanonicalContentAddress(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("digest shape fixture"))
	digest := hex.EncodeToString(sum[:])
	upper := strings.ToUpper(digest)
	tests := []struct {
		name       string
		sha        string
		contentID  string
		preserveID bool
	}{
		{"lowercase address", digest, "sha256:" + digest, true},
		{"uppercase typed digest", upper, "sha256:" + upper, false},
		{"unknown address algorithm", digest, "sha512:" + digest, false},
		{"uppercase address algorithm", digest, "SHA256:" + digest, false},
		{"short address", digest, "sha256:" + digest[:32], false},
		{"nonhex address", digest, "sha256:" + digest[:63] + "g", false},
		{"long address", digest, "sha256:" + digest + "a", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{{SHA256: test.sha, ContentID: test.contentID}}}
			SanitizeBundle(&bundle, []string{digest[:12], upper[:12]})
			if bundle.Artifacts[0].SHA256 != test.sha {
				t.Fatal("a valid typed SHA-256 digest changed after a substring collision")
			}
			if test.preserveID {
				if bundle.Artifacts[0].ContentID != test.contentID {
					t.Fatal("canonical sha256 content address changed")
				}
			} else if !strings.Contains(bundle.Artifacts[0].ContentID, redactionToken) {
				t.Fatal("noncanonical content address escaped ordinary payload redaction")
			}
		})
	}
	for _, invalid := range []string{digest[:32], digest + "0", digest[:63] + "g", digest[:62] + "é"} {
		bundle := rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{{SHA256: invalid}}}
		SanitizeBundle(&bundle, []string{digest[:12]})
		if !strings.Contains(bundle.Artifacts[0].SHA256, redactionToken) {
			t.Fatal("invalid digest length or alphabet gained a structural exemption")
		}
	}
	// Known legacy aliases remain validator input, not fixed canonical tokens.
	for _, value := range []string{"compiler-resolved", "Compiler_Resolved", " compiler_resolved "} {
		bundle := rkcmodel.Bundle{Edges: []rkcmodel.Edge{{Resolution: value}}}
		SanitizeBundle(&bundle, []string{value})
		if bundle.Edges[0].Resolution != redactionToken {
			t.Fatal("noncanonical resolution spelling gained a structural exemption")
		}
	}
}

func structuralFixture(t *testing.T) (rkcmodel.Bundle, string, []byte, []string) {
	t.Helper()
	sum := sha256.Sum256([]byte("canonical structural fixture\n"))
	digest := hex.EncodeToString(sum[:])
	values := []string{digest, "function", "directory", "syntax_parsed", "declares", "compiler_resolved",
		"documentation_asserted", "building", "validated", "supported", "accepted", "warning"}
	credentials := map[string]string{}
	for index, value := range values {
		credentials[fmt.Sprintf("fixture_%d_token", index)] = value
	}
	// Materialize credential-shaped key/value pairs only at runtime. The exact
	// inputs still exercise the native scanner and source-span redaction.
	source, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	findings := Scan(source)
	if len(findings) != len(values) {
		t.Fatalf("positive controls lost credential findings: got %d, want %d", len(findings), len(values))
	}
	literals := SensitiveLiterals(source, findings)
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "snapshot", Status: "building", RootName: "fixture", RootPath: "fixture",
			ContentDigest: digest, ConfigDigest: digest, PolicyDigest: digest, PluginLockDigest: digest, ToolchainDigest: digest,
			Git: rkcmodel.GitInfo{WorkingTreeDigest: digest}, Tool: rkcmodel.ToolInfo{Name: "fixture", Version: "test"}},
		Artifacts: []rkcmodel.Artifact{
			{ID: "artifact", Path: "data.log", Kind: "file", Status: "syntax_parsed", SHA256: digest, ContentID: "sha256:" + digest},
			{ID: "folder", Path: "folder", Kind: "directory", Status: "recorded"},
		},
		Nodes: []rkcmodel.Node{{ID: "node", Kind: "function", Name: "Fixture", Signature: "function " + digest, ArtifactID: "artifact", SemanticHash: digest,
			EvidenceIDs: []string{"evidence"}, Attributes: map[string]any{"sha256": digest, "kind": "function"}}},
		Edges:       []rkcmodel.Edge{{ID: "edge", Kind: "declares", From: "node", To: "node", Resolution: "compiler_resolved", EvidenceIDs: []string{"evidence"}}},
		Evidence:    []rkcmodel.Evidence{{ID: "evidence", Kind: "documentation_asserted", Method: "fixture", Confidence: 1, InputDigest: digest, Detail: digest}},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "diagnostic", Severity: "warning", Code: "RKC-TEST-001", Message: digest}},
		Claims:      []rkcmodel.Claim{{ID: "claim", SubjectID: "node", Text: digest, Certainty: "supported", Generator: "fixture", EvidenceIDs: []string{"evidence"}, Validation: "accepted"}},
		Documents: []rkcmodel.Document{{ID: "document", Kind: "source_document", Title: "Fixture", Generator: "fixture", ContentSHA256: digest, Status: "validated", SubjectIDs: []string{"node"},
			Sections: []rkcmodel.DocumentSection{{ID: "section", Markdown: string(source), PlainText: string(source), ClaimIDs: []string{"claim"}, EvidenceIDs: []string{"evidence"}}}}},
	}
	return bundle, digest, source, literals
}
