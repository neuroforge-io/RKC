package pipeline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestShortJSONCredentialsPreserveSourcePathsAnalysisExportAndContext(t *testing.T) {
	root := t.TempDir()
	codePath := "internal/gitworktree/worktree.go"
	mustWritePipelineFile(t, filepath.Join(root, filepath.FromSlash(codePath)), "package gitworktree\n\n// Clone returns one direct result.\nfunc Clone() bool { return true }\n")
	mustWritePipelineFile(t, filepath.Join(root, "credentials.jsonl"), "{\"topic\":\"lantern credential configuration\",\"token\":\"two\",\"password\":\"one\"}\n")
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Lantern worktree\n\nThe clone operation produces one worktree.\n\n{\"token\":\"two\"}\n")
	options := Options{Root: root, ToolVersion: "short-source-secret-test", SkipGitInspection: true, DisablePythonAST: true, DisableTypeScript: true}
	bundle, coverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, artifact := range bundle.Artifacts {
		paths[artifact.Path] = true
		if artifact.ID != rkcmodel.StableID("artifact", artifact.Path) {
			t.Fatalf("short credential changed artifact identity: %+v", artifact)
		}
	}
	if !paths[codePath] || !paths["credentials.jsonl"] || !paths["README.md"] {
		t.Fatalf("short secret corrupted inventoried paths: %+v", bundle.Artifacts)
	}
	foundClone := false
	for _, node := range bundle.Nodes {
		if node.Kind == "function" && node.Name == "Clone" {
			foundClone = true
			if node.Source == nil || node.Source.Path != codePath || !strings.Contains(node.Attributes["docstring"].(string), "one direct result") {
				t.Fatalf("short secret corrupted code analysis/citation: %+v", node)
			}
		}
	}
	if !foundClone {
		t.Fatal("short JSON credential renamed or removed the unrelated Clone function")
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{`\"token\":\"two\"`, `\"password\":\"one\"`} {
		if strings.Contains(string(data), literal) {
			t.Fatalf("canonical source body exposed short sensitive field: %s", literal)
		}
	}
	output := filepath.Join(t.TempDir(), "atlas")
	if err := export.WriteAll(bundle, coverage, export.Options{Root: root, Output: output, IncludeSources: true}); err != nil {
		t.Fatalf("short JSON secret broke verified source export: %v", err)
	}
	dataset, err := server.Load(output)
	if err != nil {
		t.Fatalf("short JSON secret broke exported dataset load: %v", err)
	}
	packet, err := dataset.BuildContext(context.Background(), "type:document lantern", 12, 32768)
	if err != nil || len(packet.Items) != 2 {
		t.Fatalf("short JSON secret broke portable source context: %v %+v", err, packet)
	}
	for _, item := range packet.Items {
		if item.Source == nil || len(item.EvidenceIDs) == 0 {
			t.Fatalf("source context lost original citations: %+v", item)
		}
		if strings.Contains(item.Text, `"token":"two"`) || strings.Contains(item.Text, `"password":"one"`) {
			t.Fatalf("context exposed a short credential field: %+v", item)
		}
	}
}
