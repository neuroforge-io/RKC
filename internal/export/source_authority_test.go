package export

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func privateSourceFixture(t *testing.T, relative string, data []byte) (Options, model.Artifact) {
	t.Helper()
	root := t.TempDir()
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, data, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := exportFixture(root, relative, data).Artifacts[0]
	artifact.ID = model.StableID("artifact", relative)
	info, err := os.Stat(absolute)
	if err != nil {
		t.Fatal(err)
	}
	file := pluginapi.FileRef{ArtifactID: artifact.ID, Path: relative, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes, Materialized: absolute}
	return Options{Root: root, SourceFiles: map[string]pluginapi.FileRef{artifact.ID: file}, SourceIdentities: map[string]os.FileInfo{artifact.ID: info}}, artifact
}

func TestPrivateSourceAuthorityReadsRawPathWithoutPublishingIt(t *testing.T) {
	data := []byte("retained source evidence\n")
	opts, artifact := privateSourceFixture(t, "private-source-material.txt", data)
	artifact.Path = "[REDACTED].txt"
	got, err := readSourceArtifact(opts, artifact)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("private source read failed: %v", err)
	}
	serialized, err := json.Marshal(opts)
	if err != nil || bytes.Contains(serialized, []byte("private-source-material")) || bytes.Contains(serialized, []byte("SourceFiles")) {
		t.Fatalf("private registry entered serialized options: %s (%v)", serialized, err)
	}
	copied, err := copySourceAuthority(opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	delete(opts.SourceFiles, artifact.ID)
	delete(opts.SourceIdentities, artifact.ID)
	if got, err := readSourceArtifact(copied, artifact); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("caller map mutation changed frozen source authority: %v", err)
	}
}

func TestPrivateSourceAuthorityNeverFallsBackOrLeaksRawErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Options, *model.Artifact)
	}{
		{"missing entry", func(t *testing.T, opts *Options, artifact *model.Artifact) { delete(opts.SourceFiles, artifact.ID) }},
		{"missing baseline", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			delete(opts.SourceIdentities, artifact.ID)
		}},
		{"wrong ID", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			file := opts.SourceFiles[artifact.ID]
			file.ArtifactID = "wrong"
			opts.SourceFiles[artifact.ID] = file
		}},
		{"wrong path binding", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			file := opts.SourceFiles[artifact.ID]
			other := filepath.Join(opts.Root, "wrong-but-same-content.txt")
			if err := os.WriteFile(other, []byte("retained source evidence\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(other)
			if err != nil {
				t.Fatal(err)
			}
			file.Path = "wrong-but-same-content.txt"
			opts.SourceFiles[artifact.ID], opts.SourceIdentities[artifact.ID] = file, info
		}},
		{"wrong digest", func(t *testing.T, opts *Options, artifact *model.Artifact) { artifact.SHA256 = strings.Repeat("0", 64) }},
		{"wrong size", func(t *testing.T, opts *Options, artifact *model.Artifact) { artifact.SizeBytes++ }},
		{"unsafe raw path", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			file := opts.SourceFiles[artifact.ID]
			file.Path = "../private-source-material.txt"
			opts.SourceFiles[artifact.ID] = file
		}},
		{"deleted source", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			if err := os.Remove(opts.SourceFiles[artifact.ID].Materialized); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced inode", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			path := opts.SourceFiles[artifact.ID].Materialized
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("retained source evidence\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"mutated content", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			if err := os.WriteFile(opts.SourceFiles[artifact.ID].Materialized, []byte("modified source evidence\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"modified bytes with current metadata", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			path := opts.SourceFiles[artifact.ID].Materialized
			if err := os.WriteFile(path, []byte("modified source evidence\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			opts.SourceIdentities[artifact.ID] = info
		}},
		{"invalid digest", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			artifact.SHA256 = strings.Repeat("g", 64)
			file := opts.SourceFiles[artifact.ID]
			file.SHA256 = artifact.SHA256
			opts.SourceFiles[artifact.ID] = file
		}},
		{"negative size", func(t *testing.T, opts *Options, artifact *model.Artifact) {
			artifact.SizeBytes = -1
			file := opts.SourceFiles[artifact.ID]
			file.SizeBytes = artifact.SizeBytes
			opts.SourceFiles[artifact.ID] = file
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts, artifact := privateSourceFixture(t, "private-source-material.txt", []byte("retained source evidence\n"))
			test.mutate(t, &opts, &artifact)
			_, err := readSourceArtifact(opts, artifact)
			if err == nil || strings.Contains(err.Error(), "private-source-material") || strings.Contains(err.Error(), opts.Root) {
				t.Fatalf("unsafe source accepted or raw identity leaked: %v", err)
			}
		})
	}
}

func TestSourceAuthorityRetainsOnlySafeLegacyFallback(t *testing.T) {
	opts, artifact := privateSourceFixture(t, "clean.txt", []byte("retained\n"))
	opts.SourceFiles, opts.SourceIdentities = nil, nil
	if data, err := readSourceArtifact(opts, artifact); err != nil || string(data) != "retained\n" {
		t.Fatalf("clean legacy source fallback failed: %v", err)
	}
	for _, path := range []string{"[REDACTED].txt", "********.txt", "missing-private-name.txt"} {
		artifact.Path = path
		_, err := readSourceArtifact(opts, artifact)
		if err == nil || strings.Contains(err.Error(), "missing-private-name") || strings.Contains(err.Error(), opts.Root) {
			t.Fatalf("redacted/unavailable legacy path accepted or leaked: %v", err)
		}
	}
	if _, err := copySourceAuthority(Options{SourceIdentities: map[string]os.FileInfo{}}, 0); err == nil {
		t.Fatal("orphan source identities accepted")
	}
	if _, err := copySourceAuthority(Options{SourceFiles: map[string]pluginapi.FileRef{"extra": {}}}, 0); err == nil {
		t.Fatal("source authority exceeded bounded inventory")
	}
}

func TestWriteAllUsesPrivateAuthorityAndMetadataOnlyStoredFallback(t *testing.T) {
	data := []byte("retained source evidence\n")
	opts, artifact := privateSourceFixture(t, "private-source-material.txt", data)
	bundle := exportFixture(opts.Root, artifact.Path, data)
	bundle.Artifacts[0] = artifact
	bundle.Nodes[1].ArtifactID = artifact.ID
	bundle.Evidence[0].Source.ArtifactID = artifact.ID
	secrets.SanitizeBundle(&bundle, []string{"private-source-material"})
	opts.Output, opts.IncludeSources = t.TempDir(), true
	if err := WriteAll(bundle, model.BuildCoverage(bundle), opts); err != nil {
		t.Fatalf("private source export failed: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(opts.Output, "normalized", "_redacted", "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("private normalized source is missing: %v %v", paths, err)
	}
	envelope, err := os.ReadFile(paths[0])
	if err != nil || bytes.Contains(envelope, []byte("private-source-material")) || !bytes.Contains(envelope, data) || !bytes.Contains(envelope, []byte(artifact.ID)) {
		t.Fatalf("private normalized envelope lost content/identity or leaked name: %v", err)
	}
	stored := Options{Root: opts.Root, Output: t.TempDir()}
	if err := WriteAll(bundle, model.BuildCoverage(bundle), stored); err != nil {
		t.Fatalf("redacted stored snapshot metadata export failed: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(stored.Output, "notebooklm", "manifest.json"))
	if err != nil || !bytes.Contains(manifest, []byte(`"repository_text_status": "unavailable_without_verified_source_root"`)) {
		t.Fatalf("metadata-only stored export misreported source access: %s (%v)", manifest, err)
	}
	stored.IncludeSources = true
	stored.Output = filepath.Join(t.TempDir(), "unpublished")
	if err := WriteAll(bundle, model.BuildCoverage(bundle), stored); err == nil {
		t.Fatal("stored redacted source body request used display-path authority")
	}
	if _, err := os.Stat(stored.Output); !os.IsNotExist(err) {
		t.Fatalf("rejected source request created partial output: %v", err)
	}
}

func TestNormalizedDisplayCollisionsHaveStableDistinctPrivateNames(t *testing.T) {
	artifacts := []model.Artifact{
		{ID: "artifact-a", Path: "report-[REDACTED].txt", Text: true, Status: "parsed"},
		{ID: "artifact-b", Path: "report-[REDACTED].txt", Text: true, Status: "parsed"},
		{ID: "artifact-c", Path: "Clean.txt", Text: true, Status: "parsed"},
		{ID: "artifact-d", Path: "clean.txt", Text: true, Status: "parsed"},
		{ID: "artifact-e", Path: "readme.txt", Text: true, Status: "parsed"},
	}
	first, err := normalizedSourcePaths(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizedSourcePaths([]model.Artifact{artifacts[4], artifacts[3], artifacts[2], artifacts[1], artifacts[0]})
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("normalized output depends on input order: %v", err)
	}
	seen := map[string]bool{}
	for id, path := range first {
		if seen[strings.ToLower(path)] || strings.Contains(path, "[REDACTED]") {
			t.Fatalf("normalized display paths collide: %v", first)
		}
		seen[strings.ToLower(path)] = true
		if id == "artifact-e" && path != "readme.txt.md" {
			t.Fatalf("familiar clean source path changed: %s", path)
		}
	}
	artifacts[1].ID = artifacts[0].ID
	if _, err := normalizedSourcePaths(artifacts); err == nil {
		t.Fatal("duplicate artifact identities accepted")
	}
}
