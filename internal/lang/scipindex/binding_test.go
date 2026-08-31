package scipindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSourceAffinityEmbeddedTextAndUnboundRejection(t *testing.T) {
	t.Parallel()
	source := []byte("package main\n")

	t.Run("exact embedded text", func(t *testing.T) {
		root := t.TempDir()
		file, artifact := writeAffinitySource(t, root, source)
		indexPath := writeIndex(t, root, affinityIndex(source, true))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		fragment, err := Extract(context.Background(), Options{
			Root: root, Inputs: inputs, Files: []pluginapi.FileRef{file},
			Artifacts: []rkcmodel.Artifact{artifact},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(fragment.Artifacts) != 1 || fragment.Artifacts[0].Status != "syntax_parsed" ||
			fragment.Artifacts[0].Attributes["semantic_authority"] != "unverified_external" {
			t.Fatalf("embedded-text fragment = %+v", fragment)
		}
	})

	t.Run("stale embedded text", func(t *testing.T) {
		root := t.TempDir()
		current := []byte("package changed\n")
		file, artifact := writeAffinitySource(t, root, current)
		indexPath := writeIndex(t, root, affinityIndex(source, true))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Extract(context.Background(), Options{
			Root: root, Inputs: inputs, Files: []pluginapi.FileRef{file},
			Artifacts: []rkcmodel.Artifact{artifact},
		})
		if err == nil || !strings.Contains(err.Error(), "embedded document text") {
			t.Fatalf("stale embedded text = %v", err)
		}
	})

	t.Run("no text and no receipt", func(t *testing.T) {
		root := t.TempDir()
		file, artifact := writeAffinitySource(t, root, source)
		indexPath := writeIndex(t, root, affinityIndex(source, false))
		inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Extract(context.Background(), Options{
			Root: root, Inputs: inputs, Files: []pluginapi.FileRef{file},
			Artifacts: []rkcmodel.Artifact{artifact},
		})
		if err == nil || !strings.Contains(err.Error(), "no embedded text") {
			t.Fatalf("unbound no-text document = %v", err)
		}
	})
}

func TestProducerAuthenticationChangesAggregateDigest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	indexPath := writeIndex(t, root, affinityIndex([]byte("package main\n"), true))
	inputs, unverifiedDigest, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	if inputs[0].compilerAuthenticated {
		t.Fatal("external input unexpectedly authenticated")
	}
	if err := MarkGeneratedByCurrentProcess(inputs[0]); err != nil {
		t.Fatal(err)
	}
	authenticated, authenticatedDigest, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated[0].compilerAuthenticated {
		t.Fatal("current-process generation marker was not restored")
	}
	if authenticatedDigest == unverifiedDigest {
		t.Fatal("SCIP cache identity ignored producer authentication")
	}
}

func TestGeneratedSourceReceiptAutoLoadsButCannotAuthorizeNoText(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := []byte("package main\n")
	file, artifact := writeAffinitySource(t, root, source)
	indexPath := writeIndex(t, root, affinityIndex(source, false))
	prepared, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := BuildSourceBinding(context.Background(), root, prepared[0])
	if err != nil {
		t.Fatal(err)
	}
	writeAffinityManifest(t, filepath.Dir(indexPath), filepath.Base(indexPath), prepared[0], receipt)
	bound, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	if bound[0].SourceBinding == nil || bound[0].SourceBinding.SourceSHA256 != receipt.SourceSHA256 {
		t.Fatalf("auto-loaded receipt = %+v", bound[0].SourceBinding)
	}
	if _, err := Extract(context.Background(), Options{
		Root: root, Inputs: bound, Files: []pluginapi.FileRef{file},
		Artifacts: []rkcmodel.Artifact{artifact},
	}); err == nil || !strings.Contains(err.Error(), "editable sidecar") {
		t.Fatalf("editable no-text receipt authorized semantic extraction: %v", err)
	}
}

func TestSourceBindingManifestRejectsStaleIndexAndUnsafeMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := []byte("package main\n")
	writeAffinitySource(t, root, source)
	indexPath := writeIndex(t, root, affinityIndex(source, false))
	prepared, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := BuildSourceBinding(context.Background(), root, prepared[0])
	if err != nil {
		t.Fatal(err)
	}
	stale := prepared[0]
	stale.SHA256 = strings.Repeat("0", 64)
	writeAffinityManifest(t, root, filepath.Base(indexPath), stale, receipt)
	if _, _, err := PrepareInputs(context.Background(), []string{indexPath}); err == nil ||
		!strings.Contains(err.Error(), "stale or foreign index") {
		t.Fatalf("stale manifest index = %v", err)
	}

	unsafe := message(
		fieldMessage(1, message(
			fieldMessage(2, message(fieldString(1, "compiler"), fieldString(2, "1"))),
			fieldString(3, "relative/root"), fieldVarint(4, 1),
		)),
	)
	unsafePath := writeNamedIndex(t, t.TempDir(), "unsafe.scip", unsafe)
	unsafeInputs, _, err := PrepareInputs(context.Background(), []string{unsafePath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), unsafeInputs[0]); err == nil ||
		!strings.Contains(err.Error(), "absolute URI") {
		t.Fatalf("unsafe project_root = %v", err)
	}
}

func affinityIndex(source []byte, embedded bool) []byte {
	fields := [][]byte{
		fieldString(1, "main.go"), fieldString(4, "Go"), fieldVarint(6, 1),
	}
	if embedded {
		fields = append(fields, fieldString(5, string(source)))
	}
	return indexMessage("scip-go", "1", message(fields...), nil)
}

func writeAffinitySource(
	t *testing.T, root string, source []byte,
) (pluginapi.FileRef, rkcmodel.Artifact) {
	t.Helper()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactID := rkcmodel.StableID("artifact", "main.go")
	sha := digest(source)
	return pluginapi.FileRef{
			ArtifactID: artifactID, Path: "main.go", Language: "go", SHA256: sha,
			SizeBytes: int64(len(source)), Materialized: path,
		}, rkcmodel.Artifact{
			ID: artifactID, Path: "main.go", Kind: "source", Language: "go",
			SHA256: sha, SizeBytes: int64(len(source)), Text: true, Status: "text",
		}
}

func writeAffinityManifest(
	t *testing.T, directory, name string, input Input, receipt SourceBinding,
) {
	t.Helper()
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Indexes: []ManifestIndex{{
			Language: "go", Path: name, SHA256: input.SHA256,
			SizeBytes: input.SizeBytes, Documents: receipt.DocumentCount,
			SourceBinding: &receipt,
		}},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ManifestName), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
