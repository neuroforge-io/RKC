package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/workspace"
)

func TestRunWorkspaceDiscoveryAndInvalidStartup(t *testing.T) {
	path := filepath.Join(canonicalWorkspaceTempDir(t), "workspace", "registry.json")
	store, err := workspace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"rkc.repositories\"}}\n"
	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--workspace", path}, strings.NewReader(input), &out, &diagnostics); code != 0 || diagnostics.Len() != 0 || !strings.Contains(out.String(), "rkc-workspace-repositories/v1") {
		t.Fatalf("workspace startup: code=%d output=%s diagnostics=%s", code, out.String(), diagnostics.String())
	}
	for _, args := range [][]string{
		{"--workspace", ""}, {"--workspace", " "}, {"--workspace", " registry.json"}, {"--workspace", path, "--dir", ".rkc"},
		{"--workspace", path, "--database", "fixture.sqlite"}, {"--workspace", path, "--snapshot", "snapshot"},
		{"--workspace", path, "--repository", "source"}, {"--workspace", filepath.Join(canonicalWorkspaceTempDir(t), "missing.json")},
	} {
		out.Reset()
		diagnostics.Reset()
		if code := run(context.Background(), args, strings.NewReader(input), &out, &diagnostics); code == 0 || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatalf("invalid startup accepted: %v code=%d stdout=%s diagnostics=%s", args, code, out.String(), diagnostics.String())
		}
	}
	for _, test := range []struct {
		ctx    context.Context
		input  io.Reader
		output io.Writer
	}{
		{context.Background(), failingReader{errors.New("synthetic read failure")}, &bytes.Buffer{}},
		{context.Background(), strings.NewReader(input), failingWriter{errors.New("synthetic write failure")}},
	} {
		diagnostics.Reset()
		if code := run(test.ctx, []string{"--workspace", path}, test.input, test.output, &diagnostics); code == 0 || diagnostics.Len() == 0 {
			t.Fatalf("transport failure lost: %d %s", code, diagnostics.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	diagnostics.Reset()
	if code := run(ctx, []string{"--workspace", path}, strings.NewReader(input), &out, &diagnostics); code != 0 || !strings.Contains(out.String(), `"code":-32800`) {
		t.Fatalf("cancellation response lost: %d %s %s", code, out.String(), diagnostics.String())
	}
}

func TestRunWorkspaceLoadsActuallyExportedVerifiedAtlas(t *testing.T) {
	path := filepath.Join(canonicalWorkspaceTempDir(t), "workspace", "registry.json")
	store, err := workspace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := workspace.Source{ID: "fixture", Label: "Synthetic fixture", Kind: "local", LocalPath: canonicalWorkspaceTempDir(t), Excludes: []string{}, Limits: workspace.DefaultLimits()}
	if err := store.Add(source); err != nil {
		t.Fatal(err)
	}
	err = store.Refresh(context.Background(), source.ID, func(ctx context.Context, source workspace.Source, parent string) (*workspace.Active, error) {
		generation, err := workspace.CreateGeneration(parent, source.ID)
		if err != nil {
			return nil, err
		}
		atlas := writeTestDataset(t, generation)
		manifest, err := os.ReadFile(filepath.Join(atlas, "rkc-export-manifest.json"))
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(manifest)
		return &workspace.Active{AtlasPath: atlas, SnapshotID: "rkc:snapshot:test", Generation: filepath.Base(generation), ManifestSHA256: hex.EncodeToString(digest[:]), Fingerprint: strings.Repeat("a", 64), CompilerVersion: "test"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"rkc.coverage\",\"arguments\":{\"repository\":\"fixture\"}}}\n")
	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--workspace", path}, input, &out, &diagnostics); code != 0 || diagnostics.Len() != 0 || !strings.Contains(out.String(), `"isError":false`) || !strings.Contains(out.String(), `"snapshot_id":"rkc:snapshot:test"`) {
		t.Fatalf("real atlas failed: %d %s %s", code, out.String(), diagnostics.String())
	}
}

// Darwin temporary roots can contain the system /var symlink. Resolve only
// synthetic fixture paths before exercising the production no-symlink policy.
func canonicalWorkspaceTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
