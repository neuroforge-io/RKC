package workspace

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSecretReviewBindsEntireFileAndExactFinding(t *testing.T) {
	review := SecretReview{Path: "tests/fixture.go", SHA256: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 16), Reason: "test_fixture"}
	bundle := rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{{ID: "file", Path: review.Path, SHA256: review.SHA256}}, Nodes: []rkcmodel.Node{{ID: "finding", Kind: "secret", ArtifactID: "file", Source: &rkcmodel.SourceRange{ArtifactID: "file", Path: review.Path}, Attributes: map[string]any{"confidence": 0.92, "fingerprint": review.Fingerprint, "value_retained": false}}}}
	if got := CountReviewedSecrets(bundle, []SecretReview{review}); got != 1 {
		t.Fatalf("exact reviewed fixture not recognized: %d", got)
	}
	if bundle.Nodes[0].Attributes["value_retained"] != false || len(bundle.Nodes) != 1 {
		t.Fatal("review changed redaction or findings")
	}
	bundle.Nodes = append(bundle.Nodes, bundle.Nodes[0])
	bundle.Nodes[1].ID = "duplicate-finding"
	if CountReviewedSecrets(bundle, []SecretReview{review}) != 1 {
		t.Fatal("duplicate semantic finding inflated reviewed count")
	}
	bundle.Nodes = bundle.Nodes[:1]
	bundle.Artifacts[0].SHA256 = strings.Repeat("c", 64)
	if CountReviewedSecrets(bundle, []SecretReview{review}) != 0 {
		t.Fatal("review survived replacement of source contents")
	}
	bundle.Artifacts[0].SHA256 = review.SHA256
	bundle.Nodes[0].Attributes["confidence"] = 0.86
	if CountReviewedSecrets(bundle, []SecretReview{review}) != 0 {
		t.Fatal("low-confidence review reduced high-confidence gate")
	}
	bundle.Nodes[0].Attributes["confidence"] = 0.92
	bundle.Nodes[0].Attributes["fingerprint"] = strings.Repeat("d", 16)
	if CountReviewedSecrets(bundle, []SecretReview{review}) != 0 {
		t.Fatal("unreviewed finding was accepted")
	}
}

func TestSecretReviewPolicyPreservesSourceAndLastGood(t *testing.T) {
	store := fixtureStore(t)
	source := fixtureSource(t)
	if err := store.Add(source); err != nil {
		t.Fatal(err)
	}
	review := SecretReview{Path: "tests/fixture.go", SHA256: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 16), Reason: "test_fixture"}
	if err := store.SetSecretReviews(source.ID, []SecretReview{review}); err != nil {
		t.Fatal(err)
	}
	if store.Registry.Sources[0].Freshness.Status != "pending" {
		t.Fatal("review fabricated an active atlas")
	}
	active := fixtureActive(store, "a")
	store.Registry.Sources[0].Active = active
	if err := store.SetSecretReviews(source.ID, nil); err != nil {
		t.Fatal(err)
	}
	registry, err := Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := registry.Sources[0]
	if got.Active == nil || *got.Active != *active || got.LocalPath != source.LocalPath || got.Limits != source.Limits || len(got.Excludes) != 1 || got.Excludes[0] != source.Excludes[0] || len(got.SecretReviews) != 0 || got.Freshness.Status != "stale" {
		t.Fatal("review changed source admission or lost the last verified atlas")
	}
	if err := store.SetSecretReviews("missing", nil); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestSecretReviewsRequirePrivateStrictOperatorFile(t *testing.T) {
	review := SecretReview{Path: "tests/fixture.go", SHA256: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 16), Reason: "test_fixture"}
	root := workspaceTempDir(t)
	privateFile, err := privatepath.CreateTemp(root, "reviews-*.json")
	if err != nil {
		t.Fatal(err)
	}
	file := privateFile.Name()
	if err := privateFile.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]SecretReview{review})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSecretReviews(file); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if err := validateSecretReviews([]SecretReview{review, review}); err == nil {
		t.Fatal("duplicate review accepted")
	}
	for _, field := range []string{"path", "sha256", "fingerprint", "reason"} {
		bad := review
		switch field {
		case "path":
			bad.Path = "../outside"
		case "sha256":
			bad.SHA256 = "partial"
		case "fingerprint":
			bad.Fingerprint = "partial"
		case "reason":
			bad.Reason = "allow_real_credentials"
		}
		if err := validateSecretReviews([]SecretReview{bad}); err == nil {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	if err := os.WriteFile(file, []byte(`[{"unexpected":true}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretReviews(file); err == nil {
		t.Fatal("unrecognized review fields accepted")
	}
}
