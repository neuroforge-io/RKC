package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestMessyDataScanExportSearchAndCacheEquivalence(t *testing.T) {
	root := t.TempDir()
	const fixtureReference = "env:RKC_FIXTURE_API_KEY"
	const materializedValue = "fictional-key-84612"
	referenceField := fmt.Sprintf("%q:%q", "api_key", fixtureReference)
	materializedField := fmt.Sprintf("%q:%q", "api_key", materializedValue)
	materializedDigest := ""
	fixtureDir := filepath.Join("..", "..", "fixtures", "messy-data")
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(fixtureDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name() == "messages.ndjson" {
			// Materialize only the one declared reference in this private copy.
			// Keep malformed records, large numbers and every other byte intact.
			if strings.Count(string(body), referenceField) != 1 {
				t.Fatal("NDJSON fixture must contain exactly one declared credential reference")
			}
			body = []byte(strings.Replace(string(body), referenceField, materializedField, 1))
			findings := secrets.Scan(body)
			if len(findings) != 1 || findings[0].Kind != "json_secret_field" || findings[0].Confidence != .9 ||
				findings[0].KeyName != "api_key" || string(body[findings[0].StartByte:findings[0].EndByte]) != materializedValue {
				t.Fatalf("materialized credential positive control lost its exact source receipt: %+v", findings)
			}
			digest := sha256.Sum256(body)
			materializedDigest = hex.EncodeToString(digest[:])
		}
		mustWritePipelineFile(t, filepath.Join(root, entry.Name()), string(body))
	}
	if materializedDigest == "" {
		t.Fatal("messy-data fixture omitted the materialized NDJSON positive control")
	}
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Root: root, ToolVersion: "messy-data-test", SkipGitInspection: true, DisablePlugins: true, Cache: cache}
	bundle, coverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Documents) != len(entries) || coverage.ArtifactsInventoried != len(entries) {
		t.Fatalf("missing messy source documents: documents=%d artifacts=%d", len(bundle.Documents), coverage.ArtifactsInventoried)
	}
	for _, document := range bundle.Documents {
		if document.Generator != docparse.SourcePluginID {
			t.Fatalf("source provenance = %+v", document)
		}
	}
	materializedReceipts := 0
	for _, artifact := range bundle.Artifacts {
		if artifact.Status != "text" {
			t.Fatalf("source document projection falsely upgraded syntax precision: %+v", artifact)
		}
		if artifact.Path == "messages.ndjson" {
			materializedReceipts++
			if artifact.SHA256 != materializedDigest {
				t.Fatalf("source receipt used the checked-in reference instead of materialized original bytes: %+v", artifact)
			}
		}
	}
	if materializedReceipts != 1 {
		t.Fatalf("materialized NDJSON source must retain one artifact receipt, got %d", materializedReceipts)
	}
	if report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true}); report.HasErrors() {
		t.Fatalf("canonical source evidence failed validation: %+v", report)
	}
	encoded, _ := json.Marshal(bundle)
	for _, value := range []string{"fictional-pass-3842", "fictional-pass-9175", materializedValue} {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("canonical source leaked %q", value)
		}
	}
	warm, warmCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	requireCanonicalScanEquality(t, bundle, coverage, warm, warmCoverage)
	options.Cache = nil
	sequential, sequentialCoverage, err := scanSequential(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	requireCanonicalScanEquality(t, bundle, coverage, sequential, sequentialCoverage)
	output := filepath.Join(t.TempDir(), "atlas")
	if err := export.WriteAll(bundle, coverage, export.Options{Root: root, Output: output, IncludeSources: true}); err != nil {
		t.Fatal(err)
	}
	index, err := search.Load(filepath.Join(output, "search", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := index.Search(search.Query{Text: "Orchid", ObjectTypes: map[string]struct{}{"document": {}}, Limit: 20})
	if len(result.Hits) != 4 {
		t.Fatalf("messy source documents not searchable: %+v", result)
	}
	if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range []string{"fictional-pass-3842", "fictional-pass-9175", materializedValue} {
			if strings.Contains(string(body), value) {
				t.Errorf("export %s leaked %q", path, value)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMessyDataStageDisableAndSelectiveInvalidation(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "notes.txt"), "notes before\n")
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Guide\n")
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Root: root, SkipGitInspection: true, ToolVersion: "data-cache-test", DisablePlugins: true, Cache: cache}
	if _, _, err := Scan(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Updated guide\n")
	plan, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedStage(t, plan, "source-documents"); got.Disposition != "cache-hit" {
		t.Fatalf("unrelated Markdown invalidated source document cache: %+v", got)
	}
	mustWritePipelineFile(t, filepath.Join(root, "notes.txt"), "notes after\n")
	plan, err = Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedStage(t, plan, "source-documents"); got.Disposition != "execute" {
		t.Fatalf("changed notes retained stale document cache: %+v", got)
	}
	options.DisableFrameworks = true
	bundle, _, err := Scan(context.Background(), options)
	if err != nil || len(bundle.Documents) != 0 || len(bundle.Artifacts) != 2 {
		t.Fatalf("disabled framework document contract = %+v, %v", bundle, err)
	}
}
