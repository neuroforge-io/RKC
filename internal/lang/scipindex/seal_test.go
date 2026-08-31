package scipindex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSealRepositorySourcesBindsPreAndPostCompilerBytes(t *testing.T) {
	root := t.TempDir()
	source := []byte("package main\n")
	file, artifact := writeAffinitySource(t, root, source)
	snapshot, err := CaptureSourceSnapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := writeIndex(t, root, affinityIndex(source, false))
	inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err := SealRepositorySources(context.Background(), root, inputs[0], snapshot, &sealed); err != nil {
		t.Fatal(err)
	}
	sealedPath := filepath.Join(t.TempDir(), "sealed.scip")
	if err := os.WriteFile(sealedPath, sealed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sealedInputs, _, err := PrepareInputs(context.Background(), []string{sealedPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkGeneratedByCurrentProcess(sealedInputs[0]); err != nil {
		t.Fatal(err)
	}
	sealedInputs, _, err = PrepareInputs(context.Background(), []string{sealedPath})
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := Extract(context.Background(), Options{
		Root: root, Inputs: sealedInputs, Files: []pluginapi.FileRef{file},
		Artifacts: []rkcmodel.Artifact{artifact},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Artifacts) != 1 || fragment.Artifacts[0].Status != "semantic_parsed" {
		t.Fatalf("sealed semantic fragment = %+v", fragment)
	}
}

func TestSealRepositorySourcesRejectsSourceMutationDuringIndexer(t *testing.T) {
	root := t.TempDir()
	source := []byte("package main\n")
	writeAffinitySource(t, root, source)
	snapshot, err := CaptureSourceSnapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := writeIndex(t, root, affinityIndex(source, false))
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	err = SealRepositorySources(context.Background(), root, inputs[0], snapshot, &sealed)
	if err == nil || (!strings.Contains(err.Error(), "changed after pre-index admission") &&
		!strings.Contains(err.Error(), "changed during SCIP indexing")) {
		t.Fatalf("source mutation was not rejected: %v", err)
	}
}

func TestSealRepositorySourcesRejectsUnindexedConfigurationMutation(t *testing.T) {
	root := t.TempDir()
	source := []byte("package main\n")
	writeAffinitySource(t, root, source)
	configuration := filepath.Join(root, "go.mod")
	if err := os.WriteFile(configuration, []byte("module example.test/original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureSourceSnapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := writeIndex(t, root, affinityIndex(source, false))
	if err := os.WriteFile(configuration, []byte("module example.test/changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	err = SealRepositorySources(context.Background(), root, inputs[0], snapshot, &sealed)
	if err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("unindexed compiler configuration mutation was not rejected: %v", err)
	}
}

func TestSealRepositorySourcesRejectsCompilerTextForDifferentBytes(t *testing.T) {
	root := t.TempDir()
	current := []byte("package current\n")
	writeAffinitySource(t, root, current)
	snapshot, err := CaptureSourceSnapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := writeIndex(t, root, affinityIndex([]byte("package stale\n"), true))
	inputs, _, err := PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	err = SealRepositorySources(context.Background(), root, inputs[0], snapshot, &sealed)
	if err == nil || !strings.Contains(err.Error(), "compiler-emitted text") {
		t.Fatalf("foreign compiler text was not rejected: %v", err)
	}
}

func TestApplyDefaultPositionEncodingIsNarrowAndDeterministic(t *testing.T) {
	tests := []struct {
		name     string
		document []byte
		want     int32
	}{
		{
			name:     "missing",
			document: message(fieldString(1, "main.py"), fieldString(4, "Python")),
			want:     2,
		},
		{
			name: "explicit zero",
			document: message(
				fieldString(1, "main.py"), fieldString(4, "Python"), fieldVarint(6, 0),
			),
			want: 2,
		},
		{
			name: "explicit encoding is preserved",
			document: message(
				fieldString(1, "main.py"), fieldString(4, "Python"), fieldVarint(6, 1),
			),
			want: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, err := applyDefaultPositionEncoding(test.document, 2)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseDocument(first)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.positionEncoding != test.want {
				t.Fatalf("position encoding = %d, want %d", parsed.positionEncoding, test.want)
			}
			second, err := applyDefaultPositionEncoding(first, 2)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("position-encoding normalization is not idempotent")
			}
		})
	}
}

func TestApplyDefaultPositionEncodingRejectsAmbiguousOrInvalidInput(t *testing.T) {
	duplicate := message(
		fieldString(1, "main.py"),
		fieldVarint(6, 0),
		fieldVarint(6, 2),
	)
	if _, err := applyDefaultPositionEncoding(duplicate, 2); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate encoding fields were not rejected: %v", err)
	}
	for _, fallback := range []uint64{0, 4} {
		if _, err := applyDefaultPositionEncoding(message(fieldString(1, "main.py")), fallback); err == nil {
			t.Fatalf("unsupported fallback %d was not rejected", fallback)
		}
	}
}
