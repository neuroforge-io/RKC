package search

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestLargeCanonicalSignatureProducesLoadableBoundedSearchIndex(t *testing.T) {
	signature := "const large = " + strings.Repeat("界", 20000)
	bundle := rkcmodel.Bundle{Nodes: []rkcmodel.Node{{ID: "node", Kind: "constant", Name: "large", Signature: signature}}}
	index, err := BuildFromBundleBounded(bundle)
	if err != nil {
		t.Fatal(err)
	}
	document := index.Documents["node"]
	if len(document.Signature) > maximumSearchIdentifierBytes || !utf8.ValidString(document.Signature) || document.Metadata["signature_truncated"] != "true" {
		t.Fatal("signature projection is not bounded and explicit")
	}
	if bundle.Nodes[0].Signature != signature {
		t.Fatal("canonical source signature was changed")
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightPersistedIndex(bytes.NewReader(encoded)); err != nil {
		t.Fatalf("producer wrote an unreadable search index: %v", err)
	}
	if err := ValidateBundleIndex(index, bundle, false); err != nil {
		t.Fatalf("projection lost canonical binding: %v", err)
	}
	document.Metadata["signature_original_bytes"] = "1"
	if err := ValidateBundleIndex(index, bundle, false); err == nil {
		t.Fatal("tampered truncation receipt was accepted")
	}
}
