package search

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPreflightPersistedIndexAcceptsBoundedCanonicalIndex(t *testing.T) {
	t.Parallel()
	index, err := BuildBounded([]Document{{
		ID: "artifact", ObjectType: "artifact", Title: "Guide", Path: "docs/guide.md",
		Body: "grounded repository evidence", Metadata: map[string]string{"digest": strings.Repeat("a", 64)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightPersistedIndex(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid index failed preflight: %v", err)
	}
}

func TestBoundedIndexAndPreflightRejectAllocationAmplifiers(t *testing.T) {
	t.Parallel()
	oversized := Document{ID: "large", Body: strings.Repeat("x", MaximumIndexedDocumentBytes+1)}
	if _, err := BuildBounded([]Document{oversized}); err == nil || !strings.Contains(err.Error(), "per-document limit") {
		t.Fatalf("oversized document passed bounded build: %v", err)
	}

	metadata := make(map[string]string, maximumDocumentMetadataFields+1)
	for index := 0; index <= maximumDocumentMetadataFields; index++ {
		metadata[strings.Repeat("k", index+1)] = "value"
	}
	index := Build([]Document{{ID: "metadata", Body: "body", Metadata: metadata}})
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightPersistedIndex(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
		t.Fatalf("metadata allocation amplifier passed preflight: %v", err)
	}
}

func TestTokenizeSkipsPathologicalTerms(t *testing.T) {
	t.Parallel()
	pathological := strings.Repeat("x", MaximumIndexedTermBytes+1)
	index := Build([]Document{{ID: "term", Body: pathological + " grounded"}})
	if _, indexed := index.Postings[pathological]; indexed {
		t.Fatal("pathological term was indexed")
	}
	if len(index.Postings["grounded"]) != 1 {
		t.Fatalf("normal term was lost: %+v", index.Postings)
	}
}
