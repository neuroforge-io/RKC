package envkeys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractEnvironmentContractsRedactsAndIsDeterministic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sensitiveValue := "not-for" + "-output-123456789"
	writeEnvTestFile(t, root, "z.env", "APP_MODE=from-z\nSECOND=2\n")
	writeEnvTestFile(t, root, "a.env.example", strings.Join([]string{
		"# comment",
		"; another comment",
		"export APP_MODE = from-a",
		"DB_PASSWORD=" + sensitiveValue,
		"NO_DEFAULT",
		"1INVALID=value",
		"WITH-DASH=value",
		"",
	}, "\n"))

	files := []pluginapi.FileRef{
		{ArtifactID: "z", Path: "z.env", SHA256: "sha-z"},
		{ArtifactID: "a", Path: "a.env.example", SHA256: "sha-a"},
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

	byName := map[string]map[string]any{}
	for _, node := range got.Nodes {
		byName[node.Name] = node.Attributes
	}
	if len(byName) != 4 {
		t.Fatalf("environment nodes = %d (%v), want 4", len(byName), mapKeys(byName))
	}
	if got := byName["APP_MODE"]["default_sha256"]; got != privateValueDigest("dotenv-default", "from-a") {
		t.Errorf("duplicate APP_MODE fingerprint = %#v, want deterministic first-file value", got)
	}
	if _, present := byName["DB_PASSWORD"]["default_sha256"]; present {
		t.Error("secret default received a guessable fingerprint")
	}
	if got := byName["DB_PASSWORD"]["secret_like"]; got != true {
		t.Errorf("secret_like = %#v, want true", got)
	}
	if _, present := byName["APP_MODE"]["default"]; present {
		t.Error("non-secret default was copied into the atlas")
	}
	if got := byName["APP_MODE"]["default_class"]; got != "literal" {
		t.Errorf("APP_MODE class = %#v, want literal", got)
	}
	if got := byName["NO_DEFAULT"]["has_default"]; got != false {
		t.Errorf("NO_DEFAULT has_default = %#v, want false", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sensitiveValue) {
		t.Fatal("sensitive default leaked into extracted fragment")
	}
	if strings.Contains(string(encoded), "from-a") || strings.Contains(string(encoded), "from-z") {
		t.Fatal("ordinary environment default leaked into extracted fragment")
	}
}

func TestExtractEnvironmentDiagnostics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeEnvTestFile(t, root, "oversize.env", strings.Repeat("A", 2*1024*1024+1))
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{
		{ArtifactID: "missing", Path: "missing.env", SHA256: "missing"},
		{ArtifactID: "oversize", Path: "oversize.env", SHA256: "oversize"},
	}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	codes := map[string]bool{}
	for _, diagnostic := range fragment.Diagnostics {
		codes[diagnostic.Code] = true
	}
	for _, code := range []string{"RKC-CFG-1001", "RKC-CFG-1002"} {
		if !codes[code] {
			t.Errorf("missing diagnostic %s in %#v", code, fragment.Diagnostics)
		}
	}
}

func TestExtractEnvironmentRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvTestFile(t, parent, "outside.env", "OUTSIDE=value\n")

	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../outside.env", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-CFG-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-CFG-1001", fragment.Diagnostics)
	}
}

func TestIsCandidateAndLikelySecret(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path string
		want bool
	}{{".env", true}, {"config/.env.production", true}, {"service.env", true}, {"ENV.EXAMPLE", true}, {"settings/env.sample.local", true}, {"env.template", true}, {"environment.go", false}, {"config.json", false}} {
		if got := IsCandidate(test.path); got != test.want {
			t.Errorf("IsCandidate(%q) = %v, want %v", test.path, got, test.want)
		}
	}
	for _, name := range []string{"api_token", "clientCredential", "PRIVATE_KEY_PATH", "password_file"} {
		if !likelySecret(name) {
			t.Errorf("likelySecret(%q) = false", name)
		}
	}
	if likelySecret("PUBLIC_PORT") {
		t.Error("likelySecret(PUBLIC_PORT) = true")
	}
}

func TestScalarClassificationCoversSafeDefaultClasses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value string
		want  string
	}{
		{"", "empty"},
		{"${HOME}", "expression"},
		{"true", "boolean"},
		{"42.5", "number"},
		{"https://example.test/api", "url"},
		{"./config", "path"},
		{"plain text", "literal"},
	} {
		t.Run(test.want, func(t *testing.T) {
			if got := scalarClassification(test.value); got != test.want {
				t.Fatalf("scalarClassification(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func writeEnvTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mapKeys[V any](input map[string]V) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	return keys
}

var _ = reflect.DeepEqual
