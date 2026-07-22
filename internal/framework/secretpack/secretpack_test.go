package secretpack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractSecretsWithholdsValuesAndIsDeterministic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sensitiveValue := "ghp_" + strings.Repeat("A", 32)
	writeSecretpackTestFile(t, root, "b.txt", "no findings here\n")
	writeSecretpackTestFile(t, root, "a.txt", "token = \""+sensitiveValue+"\"\npassword=changeme\n")
	files := []pluginapi.FileRef{
		{ArtifactID: "b", Path: "b.txt", Language: "text", SHA256: "sha-b"},
		{ArtifactID: "a", Path: "a.txt", Language: "text", SHA256: "sha-a"},
	}
	got, err := Extract(Options{Root: root, Files: files})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversed, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{files[1], files[0]}})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("Extract() output depends on input file order")
	}
	if len(got.Nodes) != 1 || len(got.Evidence) != 1 || len(got.Edges) != 1 || len(got.Diagnostics) != 1 {
		t.Fatalf("fragment counts nodes=%d evidence=%d edges=%d diagnostics=%d, want 1 each", len(got.Nodes), len(got.Evidence), len(got.Edges), len(got.Diagnostics))
	}
	finding := got.Nodes[0]
	if finding.Kind != "secret" || finding.Visibility != "restricted" || finding.Attributes["value_retained"] != false {
		t.Errorf("secret node = %#v", finding)
	}
	if finding.Source == nil || finding.Source.StartLine != 1 || finding.Source.EndByte <= finding.Source.StartByte {
		t.Errorf("secret source range = %#v", finding.Source)
	}
	if got.Diagnostics[0].Code != "RKC-SEC-1102" || got.Diagnostics[0].Severity != "warning" {
		t.Errorf("diagnostic = %#v", got.Diagnostics[0])
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sensitiveValue) {
		t.Fatal("secret value leaked into graph fragment")
	}
	if !strings.Contains(string(encoded), finding.Attributes["fingerprint"].(string)) {
		t.Error("finding fingerprint missing from serialized output")
	}
}

func TestExtractSecretpackReadDiagnostic(t *testing.T) {
	t.Parallel()
	fragment, err := Extract(Options{Root: t.TempDir(), Files: []pluginapi.FileRef{{ArtifactID: "missing", Path: "missing.txt", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-SEC-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-SEC-1001", fragment.Diagnostics)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("missing file produced %d nodes", len(fragment.Nodes))
	}
}

func TestExtractSecretpackRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSecretpackTestFile(t, parent, "outside.txt", "token=ghp_"+strings.Repeat("B", 32)+"\n")
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../outside.txt", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path produced %d secret nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-SEC-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-SEC-1001", fragment.Diagnostics)
	}
}

func writeSecretpackTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
