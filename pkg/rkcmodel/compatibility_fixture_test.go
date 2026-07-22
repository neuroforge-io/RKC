package rkcmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type compatibilityManifest struct {
	SchemaVersion   string `json:"schema_version"`
	RKCVersion      string `json:"rkc_version"`
	CanonicalSchema string `json:"canonical_schema"`
	SourceCommit    string `json:"source_commit"`
	SignedTag       string `json:"signed_tag"`
	TagObject       string `json:"tag_object"`
	TagSigningKey   string `json:"tag_signing_key"`
	Proof           struct {
		CIRun                 string `json:"ci_run"`
		CodeQLRun             string `json:"codeql_run"`
		GoCoverage            string `json:"go_coverage"`
		PythonCoverage        string `json:"python_coverage"`
		SelfCatalogueArtifact string `json:"self_catalogue_artifact"`
		SelfCatalogueID       int64  `json:"self_catalogue_artifact_id"`
		SelfCatalogueSize     int64  `json:"self_catalogue_size_bytes"`
	} `json:"proof"`
	Files []compatibilityFile `json:"files"`
}

type compatibilityFile struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	CanonicalDigest string `json:"canonical_digest,omitempty"`
}

type stableIDFixture struct {
	SchemaVersion string `json:"schema_version"`
	Vectors       []struct {
		Name      string   `json:"name"`
		Namespace string   `json:"namespace"`
		Parts     []string `json:"parts"`
		Expected  string   `json:"expected"`
	} `json:"vectors"`
}

const (
	referenceCommit       = "6556a12954fb90f63b4b12b73a09a04cf78deea8"
	referenceTagObject    = "f8e9cf2693a1c8de08facfa41a469389b4d3bc4b"
	referenceTagKey       = "136D2F0E1485164F058A04712DD4B300954F1D12"
	referenceCIRun        = "https://github.com/neuroforge-io/RKC/actions/runs/29912722050"
	referenceCodeQLRun    = "https://github.com/neuroforge-io/RKC/actions/runs/29912721796"
	referenceCatalogueID  = int64(8526706979)
	referenceCatalogueLen = int64(28483080)
)

func TestReferenceCompatibilityFixtures(t *testing.T) {
	t.Parallel()
	root := referenceFixtureRoot(t)
	var manifest compatibilityManifest
	readStrictFixtureJSON(t, filepath.Join(root, "manifest.json"), &manifest)
	if manifest.SchemaVersion != "1.0" || manifest.RKCVersion != "0.3.0-reference" || manifest.CanonicalSchema != SchemaVersion {
		t.Fatalf("unsupported compatibility manifest: %+v", manifest)
	}
	if manifest.SourceCommit != referenceCommit || manifest.TagObject != referenceTagObject ||
		manifest.TagSigningKey != referenceTagKey || manifest.SignedTag != "v0.3.0-reference" {
		t.Fatalf("invalid baseline identities: commit=%q tag=%q object=%q", manifest.SourceCommit, manifest.SignedTag, manifest.TagObject)
	}
	if manifest.Proof.CIRun != referenceCIRun || manifest.Proof.CodeQLRun != referenceCodeQLRun {
		t.Fatalf("baseline run identities changed: CI=%q CodeQL=%q", manifest.Proof.CIRun, manifest.Proof.CodeQLRun)
	}
	if manifest.Proof.GoCoverage != "90.03%" || manifest.Proof.PythonCoverage != "90.26%" {
		t.Fatalf("baseline coverage changed: Go=%q Python=%q", manifest.Proof.GoCoverage, manifest.Proof.PythonCoverage)
	}
	if manifest.Proof.SelfCatalogueArtifact != "rkc-self-catalogue-"+manifest.SourceCommit ||
		manifest.Proof.SelfCatalogueID != referenceCatalogueID ||
		manifest.Proof.SelfCatalogueSize != referenceCatalogueLen {
		t.Fatalf("self-catalogue artifact is not commit-bound: %q", manifest.Proof.SelfCatalogueArtifact)
	}

	wantFiles := make(map[string]compatibilityFile, len(manifest.Files))
	for _, item := range manifest.Files {
		if filepath.IsAbs(item.Path) || filepath.Clean(item.Path) != item.Path || strings.Contains(item.Path, `\`) || item.Path == "." {
			t.Fatalf("unsafe fixture path: %q", item.Path)
		}
		if _, duplicate := wantFiles[item.Path]; duplicate {
			t.Fatalf("duplicate fixture path: %q", item.Path)
		}
		if !lowerHex(item.SHA256, 64) || item.CanonicalDigest != "" && !lowerHex(item.CanonicalDigest, 64) {
			t.Fatalf("invalid fixture digest metadata: %+v", item)
		}
		wantFiles[item.Path] = item
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			t.Fatalf("fixture entry is not a regular file: %s", entry.Name())
		}
		observed = append(observed, entry.Name())
	}
	sort.Strings(observed)
	wanted := make([]string, 0, len(wantFiles))
	for path := range wantFiles {
		wanted = append(wanted, path)
	}
	sort.Strings(wanted)
	if fmt.Sprint(observed) != fmt.Sprint(wanted) {
		t.Fatalf("fixture inventory = %v, want %v", observed, wanted)
	}

	for path, item := range wantFiles {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != item.SHA256 {
			t.Fatalf("fixture %s SHA-256 changed", path)
		}
		if path == "bundle.json" {
			verifyCompatibilityBundle(t, data, item, manifest.CanonicalSchema)
		}
		if path == "stable-ids.json" {
			verifyStableIDFixture(t, data)
		}
	}
}

func referenceFixtureRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller information is unavailable")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "fixtures", "golden", "reference-0.3"))
}

func readStrictFixtureJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("fixture %s contains trailing JSON", path)
	}
}

func verifyCompatibilityBundle(t *testing.T, data []byte, item compatibilityFile, schema string) {
	t.Helper()
	var bundle Bundle
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		t.Fatalf("decode compatibility bundle: %v", err)
	}
	if bundle.Snapshot.SchemaVersion != schema {
		t.Fatalf("bundle schema = %q, want %q", bundle.Snapshot.SchemaVersion, schema)
	}
	report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() {
		t.Fatalf("compatibility bundle is invalid: %+v", report.Diagnostics)
	}
	if got := CanonicalDigest(bundle); got != item.CanonicalDigest {
		t.Fatalf("canonical digest = %q, want %q", got, item.CanonicalDigest)
	}
}

func verifyStableIDFixture(t *testing.T, data []byte) {
	t.Helper()
	var fixture stableIDFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode stable-ID fixture: %v", err)
	}
	if fixture.SchemaVersion != "1.0" || len(fixture.Vectors) == 0 {
		t.Fatalf("invalid stable-ID fixture header: %+v", fixture)
	}
	seenNames := map[string]struct{}{}
	seenIDs := map[string]struct{}{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || vector.Namespace == "" || len(vector.Parts) == 0 {
			t.Fatalf("incomplete stable-ID vector: %+v", vector)
		}
		if _, duplicate := seenNames[vector.Name]; duplicate {
			t.Fatalf("duplicate stable-ID vector name: %s", vector.Name)
		}
		seenNames[vector.Name] = struct{}{}
		if got := StableID(vector.Namespace, vector.Parts...); got != vector.Expected {
			t.Fatalf("StableID(%s) = %q, want %q", vector.Name, got, vector.Expected)
		}
		if _, duplicate := seenIDs[vector.Expected]; duplicate {
			t.Fatalf("stable-ID vectors collide at %q", vector.Expected)
		}
		seenIDs[vector.Expected] = struct{}{}
	}
}

func lowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
