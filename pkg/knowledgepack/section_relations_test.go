package knowledgepack

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestDocumentSectionRetainsIndirectClaimAndSubjectLinks(t *testing.T) {
	input := sampleInput("section-links")
	input.Bundle.Documents[0].SubjectIDs = []string{"node", "artifact"}
	input.Bundle.Documents[0].Sections[0].ClaimIDs = []string{"claim"}
	input.Bundle.Documents[0].Sections[0].EvidenceIDs = nil
	before, err := json.Marshal(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	pack := mustPack(t, input)
	foundSection, foundClaim := false, false
	for _, unit := range pack.Units {
		switch unit.Kind {
		case "document_section":
			foundSection = true
			want := []Relation{
				{Kind: "describes", TargetObjectID: "artifact", Resolution: "explicit"},
				{Kind: "describes", TargetObjectID: "node", Resolution: "explicit"},
				{Kind: "presents_claim", TargetObjectID: "claim", Resolution: "explicit"},
			}
			if !reflect.DeepEqual(unit.Relations, want) {
				t.Fatalf("document/claim links lost: %+v", unit.Relations)
			}
			if len(unit.Citations) != 0 {
				t.Fatal("indirect claim evidence was promoted into direct section support")
			}
			if unit.Generator != "test" || unit.Validation != "draft" {
				t.Fatal("document generator or state changed")
			}
		case "claim":
			foundClaim = true
			if len(unit.Citations) != 1 || unit.Citations[0].EvidenceID != "evidence" || unit.Certainty != "inferred" || unit.Validation != "pending" || unit.Generator != "test-model" {
				t.Fatalf("claim grounding or review state changed: %+v", unit)
			}
		}
	}
	if !foundSection || !foundClaim {
		t.Fatal("fixture lost its section or claim")
	}
	after, err := json.Marshal(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("knowledge packing mutated document references")
	}
	if _, err := Verify(context.Background(), mustWritePack(t, pack)); err != nil {
		t.Fatal(err)
	}
}

func TestSectionKeepsDirectCitationsDistinctFromRejectedClaimEvidence(t *testing.T) {
	input := sampleInput("mixed-claim-support")
	input.Bundle.Evidence = append(input.Bundle.Evidence, rkcmodel.Evidence{ID: "counterevidence", Kind: "user_asserted", Method: "review", Confidence: 0.4})
	input.Bundle.Claims = append(input.Bundle.Claims, rkcmodel.Claim{
		ID: "counterclaim", SubjectID: "node", Text: "A reviewer disagrees.", Certainty: "contradicted", Validation: "rejected", Generator: "reviewer", EvidenceIDs: []string{"counterevidence"},
	})
	input.Bundle.Documents[0].SubjectIDs = []string{"node"}
	input.Bundle.Documents[0].Sections[0].ClaimIDs = []string{"counterclaim", "claim"}
	pack := mustPack(t, input)
	for _, unit := range pack.Units {
		if unit.Kind == "document_section" {
			if len(unit.Citations) != 1 || unit.Citations[0].EvidenceID != "evidence" {
				t.Fatalf("direct section evidence was changed: %+v", unit.Citations)
			}
			if len(unit.Relations) != 3 || unit.Relations[1].TargetObjectID != "claim" || unit.Relations[2].TargetObjectID != "counterclaim" {
				t.Fatalf("claim relations missing or unordered: %+v", unit.Relations)
			}
		}
		if unit.ObjectID == "counterclaim" && (unit.Certainty != "contradicted" || unit.Validation != "rejected" || unit.Citations[0].EvidenceID != "counterevidence") {
			t.Fatal("rejected claim was promoted or disconnected")
		}
	}
	if _, err := Verify(context.Background(), mustWritePack(t, pack)); err != nil {
		t.Fatal(err)
	}
}

func TestSectionRelationsRespectAggregateReferenceLimit(t *testing.T) {
	input := sampleInput("section-limit")
	input.Bundle.Documents[0].SubjectIDs = []string{"node"}
	input.Bundle.Documents[0].Sections[0].ClaimIDs = make([]string, maximumReferences)
	for index := range input.Bundle.Documents[0].Sections[0].ClaimIDs {
		input.Bundle.Documents[0].Sections[0].ClaimIDs[index] = "claim"
	}
	builder, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(context.Background(), input); err == nil || !strings.Contains(err.Error(), "4096 claim and subject relations") {
		t.Fatalf("combined references escaped the unit limit: %v", err)
	}
	if _, err := builder.Finish(); err == nil {
		t.Fatal("over-limit source produced a partial pack")
	}
	input.Bundle.Documents[0].Sections[0].ClaimIDs = input.Bundle.Documents[0].Sections[0].ClaimIDs[:maximumReferences-1]
	pack := mustPack(t, input)
	for _, unit := range pack.Units {
		if unit.Kind == "document_section" && len(unit.Relations) != maximumReferences {
			t.Fatal("exact reference limit was not retained")
		}
	}
	if _, err := Verify(context.Background(), mustWritePack(t, pack)); err != nil {
		t.Fatal(err)
	}
}
