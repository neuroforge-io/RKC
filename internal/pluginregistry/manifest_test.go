package pluginregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func validManifest(id, version string) Manifest {
	return Manifest{
		SchemaVersion: ManifestVersion,
		Plugin: Identity{
			ID:         id,
			Name:       "Test Plugin",
			Version:    version,
			APIVersion: "1.0",
			License:    "Apache-2.0",
		},
		Runtime: Runtime{Kind: "builtin", Entrypoint: "builtin:test", Protocol: "json-v1"},
		Inputs: Inputs{
			Languages:    []string{"go"},
			Capabilities: []string{"extract"},
		},
		Outputs: Outputs{
			NodeKinds:     []string{"function"},
			EdgeKinds:     []string{"calls"},
			EvidenceKinds: []string{"declared"},
		},
		Limits:      Limits{TimeoutSeconds: 30, MemoryMiB: 64, MaxOutputBytes: 4096, MaxParallelism: 1},
		Determinism: Determinism{Level: "deterministic", CacheInputs: []string{"artifact.sha256"}},
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeCoversAllSetFields(t *testing.T) {
	manifest := validManifest("plugin.normalize", "1.0.0")
	manifest.Inputs.Languages = []string{" go ", "python", "go", ""}
	manifest.Inputs.MediaTypes = []string{" text/plain ", "text/plain"}
	manifest.Inputs.Globs = []string{"z/**", "a/**", "z/**"}
	manifest.Inputs.Requires = []string{" tool-b ", "tool-a", "tool-a"}
	manifest.Inputs.Capabilities = []string{"scan", "extract", "scan"}
	manifest.Outputs.NodeKinds = []string{"module", "function", "module"}
	manifest.Outputs.EdgeKinds = []string{"calls", "contains", "calls"}
	manifest.Outputs.EvidenceKinds = []string{"syntax_inferred", "declared", "declared"}
	manifest.Outputs.DiagnosticCodes = []string{"B", "A", "B"}
	manifest.Capabilities.FilesystemRead = []string{"repo:b", " repo:a ", "repo:b"}
	manifest.Capabilities.FilesystemWrite = []string{"out:b", "out:a", "out:b"}
	manifest.Capabilities.Environment = []string{"B", "A", "B"}
	manifest.Capabilities.Network = []string{"b.example", "a.example", "b.example"}
	manifest.Capabilities.ProcessSpawn = []string{"tool-b", "tool-a", "tool-b"}
	manifest.Determinism.CacheInputs = []string{"z", "a", "z"}

	Normalize(&manifest)
	checks := []struct {
		name string
		got  []string
		want []string
	}{
		{"languages", manifest.Inputs.Languages, []string{"go", "python"}},
		{"media types", manifest.Inputs.MediaTypes, []string{"text/plain"}},
		{"globs", manifest.Inputs.Globs, []string{"a/**", "z/**"}},
		{"requires", manifest.Inputs.Requires, []string{"tool-a", "tool-b"}},
		{"input capabilities", manifest.Inputs.Capabilities, []string{"extract", "scan"}},
		{"node kinds", manifest.Outputs.NodeKinds, []string{"function", "module"}},
		{"edge kinds", manifest.Outputs.EdgeKinds, []string{"calls", "contains"}},
		{"evidence kinds", manifest.Outputs.EvidenceKinds, []string{"declared", "syntax_inferred"}},
		{"diagnostics", manifest.Outputs.DiagnosticCodes, []string{"A", "B"}},
		{"filesystem read", manifest.Capabilities.FilesystemRead, []string{"repo:a", "repo:b"}},
		{"filesystem write", manifest.Capabilities.FilesystemWrite, []string{"out:a", "out:b"}},
		{"environment", manifest.Capabilities.Environment, []string{"A", "B"}},
		{"network", manifest.Capabilities.Network, []string{"a.example", "b.example"}},
		{"process spawn", manifest.Capabilities.ProcessSpawn, []string{"tool-a", "tool-b"}},
		{"cache inputs", manifest.Determinism.CacheInputs, []string{"a", "z"}},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("Normalize %s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got := uniqueSorted([]string{" b ", "a", "a", ""}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("uniqueSorted() = %v", got)
	}
}

func TestValidateAcceptsSupportedConfigurations(t *testing.T) {
	for _, runtime := range []string{"builtin", "wasm-wasi", "native-worker"} {
		for _, determinism := range []string{"deterministic", "toolchain-dependent", "environment-dependent", "nondeterministic"} {
			manifest := validManifest("plugin.valid", "1.0.0")
			manifest.Runtime.Kind = runtime
			manifest.Determinism.Level = determinism
			manifest.Runtime.SHA256 = strings.Repeat("a", 64)
			manifest.Distribution.Signature = "sigstore:example"
			manifest.Capabilities.Network = []string{"api.example.test"}
			manifest.Capabilities.ProcessSpawn = []string{"compiler"}
			policy := Policy{
				AllowedRuntimes:   map[string]struct{}{runtime: {}},
				AllowNetwork:      true,
				AllowProcessSpawn: true,
				MaximumMemoryMiB:  64,
				MaximumTimeout:    30,
				RequireDigest:     true,
				RequireSignature:  true,
			}
			if err := Validate(manifest, policy); err != nil {
				t.Errorf("Validate(runtime=%s, determinism=%s): %v", runtime, determinism, err)
			}
		}
	}
}

func TestValidateReportsEveryContractAndPolicyFailure(t *testing.T) {
	manifest := validManifest("ValidID", "")
	manifest.SchemaVersion = "0"
	manifest.Plugin.Name = ""
	manifest.Plugin.APIVersion = ""
	manifest.Plugin.License = ""
	manifest.Runtime = Runtime{Kind: "container"}
	manifest.Capabilities.Network = []string{"internet"}
	manifest.Capabilities.ProcessSpawn = []string{"shell"}
	manifest.Outputs = Outputs{NodeKinds: []string{"unknown-node"}, EdgeKinds: []string{"unknown-edge"}, EvidenceKinds: []string{"unknown-evidence"}}
	manifest.Limits = Limits{TimeoutSeconds: 50, MemoryMiB: 8, MaxOutputBytes: 1, MaxParallelism: 0}
	manifest.Determinism.Level = "sometimes"
	policy := Policy{
		AllowedRuntimes:  map[string]struct{}{"builtin": {}},
		MaximumMemoryMiB: 4,
		MaximumTimeout:   10,
		RequireDigest:    true,
		RequireSignature: true,
	}
	err := Validate(manifest, policy)
	if err == nil {
		t.Fatal("Validate(invalid manifest) succeeded")
	}
	wants := []string{
		"schema_version must be 1.0",
		"plugin.id is invalid", "plugin.name is required", "plugin.version is required", "plugin.api_version is required", "plugin.license is required",
		"runtime.kind is invalid", "runtime.entrypoint is required", "runtime.protocol is required", "runtime.sha256 is required by policy", "runtime is denied by policy",
		"distribution.signature is required by policy", "network capability is denied by policy", "process_spawn capability is denied by policy",
		"limits.timeout_seconds must be positive", "limits.memory_mib must be at least 16", "limits.max_output_bytes must be at least 1024", "limits.max_parallelism must be positive",
		"plugin memory exceeds policy", "plugin timeout exceeds policy",
		"unknown node kind: unknown-node", "unknown edge kind: unknown-edge", "unknown evidence kind: unknown-evidence",
		"determinism.level is invalid",
	}
	// TimeoutSeconds is positive, so the positivity failure should not be present;
	// exercise it separately while retaining the maximum-time branch above.
	wants = removeString(wants, "limits.timeout_seconds must be positive")
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error missing %q: %v", want, err)
		}
	}
	parts := strings.Split(err.Error(), "; ")
	if !sort.StringsAreSorted(parts) {
		t.Fatalf("validation failures are not stable/sorted: %v", parts)
	}

	manifest = validManifest("plugin.zero-timeout", "1")
	manifest.Limits.TimeoutSeconds = 0
	if err := Validate(manifest, Policy{}); err == nil || !strings.Contains(err.Error(), "limits.timeout_seconds must be positive") {
		t.Fatalf("Validate(zero timeout) = %v", err)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"a", "a.b", "a-b", "a_b", "a1.2"} {
		if !validID(id) {
			t.Errorf("validID(%q) = false", id)
		}
	}
	for _, id := range []string{"", ".a", "-a", "_a", "A", "a/b", "a b"} {
		if validID(id) {
			t.Errorf("validID(%q) = true", id)
		}
	}
}

func TestEntrypointAndArtifactContainment(t *testing.T) {
	for _, value := range []string{"worker", "bin/worker", "builtin:test"} {
		if !validEntrypoint(value) {
			t.Errorf("validEntrypoint(%q) = false", value)
		}
	}
	for _, value := range []string{"", ".", "./worker", "bin/../worker", "../worker", "/absolute/worker", `bin\worker`} {
		if validEntrypoint(value) {
			t.Errorf("validEntrypoint(%q) = true", value)
		}
		manifest := validManifest("plugin.entrypoint", "1")
		manifest.Runtime.Entrypoint = value
		if err := Validate(manifest, Policy{}); err == nil || !strings.Contains(err.Error(), "runtime.entrypoint") {
			t.Errorf("Validate(entrypoint=%q) = %v", value, err)
		}
	}
	root := filepath.Join(string(filepath.Separator), "plugins")
	if !pathContainedBy(root, filepath.Join(root, "bin", "worker")) || !pathContainedBy(root, root) || pathContainedBy(root, filepath.Join(filepath.Dir(root), "outside")) {
		t.Fatal("pathContainedBy returned incorrect containment")
	}
}

func TestLoadManifestStrictDecodeAndNormalization(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "plugin.json")
	manifest := validManifest("plugin.load", "1.2.3")
	manifest.Inputs.Languages = []string{"python", "go", "python"}
	writeJSONFile(t, path, manifest)
	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Inputs.Languages, []string{"go", "python"}) {
		t.Fatalf("LoadManifest languages = %v", loaded.Inputs.Languages)
	}

	if _, err := LoadManifest(filepath.Join(directory, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadManifest(missing) = %v", err)
	}
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown field", data: `{"schema_version":"1.0","unexpected":true}`, want: "unknown field"},
		{name: "multiple values", data: string(mustJSON(t, manifest)) + ` {}`, want: "multiple JSON values"},
		{name: "trailing malformed", data: string(mustJSON(t, manifest)) + ` x`, want: "invalid character"},
		{name: "invalid manifest", data: `{}`, want: "plugin.id is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := filepath.Join(directory, strings.ReplaceAll(test.name, " ", "-")+".json")
			if err := os.WriteFile(candidate, []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadManifest() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestManifestAndLockDigestsAreDeterministic(t *testing.T) {
	first := validManifest("plugin.b", "2")
	first.Inputs.Languages = []string{"python", "go", "go"}
	second := first
	second.Inputs.Languages = []string{"go", "python"}
	if ManifestDigest(first) != ManifestDigest(second) {
		t.Fatal("ManifestDigest changed with set ordering")
	}
	if !reflect.DeepEqual(first.Inputs.Languages, []string{"python", "go", "go"}) {
		t.Fatal("ManifestDigest mutated caller")
	}

	a := validManifest("plugin.a", "2")
	a.Runtime.SHA256 = strings.Repeat("a", 64)
	a.Distribution.ArtifactURL = "https://example.test/a"
	b := validManifest("plugin.a", "1")
	c := validManifest("plugin.c", "1")
	lock := BuildLock([]Manifest{c, a, b})
	got := []string{lock.Plugins[0].ID + "@" + lock.Plugins[0].Version, lock.Plugins[1].ID + "@" + lock.Plugins[1].Version, lock.Plugins[2].ID + "@" + lock.Plugins[2].Version}
	if !reflect.DeepEqual(got, []string{"plugin.a@1", "plugin.a@2", "plugin.c@1"}) {
		t.Fatalf("BuildLock order = %v", got)
	}
	if lock.SchemaVersion != ManifestVersion || lock.Plugins[1].ArtifactSHA256 != a.Runtime.SHA256 || lock.Plugins[1].Source != a.Distribution.ArtifactURL || lock.Plugins[1].Runtime != "builtin" {
		t.Fatalf("BuildLock fields = %+v", lock)
	}
	digest := LockDigest(lock)
	reversed := Lockfile{SchemaVersion: lock.SchemaVersion, Plugins: []LockedPlugin{lock.Plugins[2], lock.Plugins[0], lock.Plugins[1]}}
	originalOrder := []string{reversed.Plugins[0].ID + "@" + reversed.Plugins[0].Version, reversed.Plugins[1].ID + "@" + reversed.Plugins[1].Version, reversed.Plugins[2].ID + "@" + reversed.Plugins[2].Version}
	if digest != LockDigest(reversed) || len(digest) != 64 {
		t.Fatalf("LockDigest is not deterministic: %q vs %q", digest, LockDigest(reversed))
	}
	if got := []string{reversed.Plugins[0].ID + "@" + reversed.Plugins[0].Version, reversed.Plugins[1].ID + "@" + reversed.Plugins[1].Version, reversed.Plugins[2].ID + "@" + reversed.Plugins[2].Version}; !reflect.DeepEqual(got, originalOrder) {
		t.Fatalf("LockDigest mutated caller order: %v != %v", got, originalOrder)
	}
}

func TestLoadLockStrictValidationAndOrdering(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "rkc.lock.json")
	lock := Lockfile{SchemaVersion: ManifestVersion, Plugins: []LockedPlugin{
		{ID: "z", Version: "1", ManifestSHA256: strings.Repeat("a", 64)},
		{ID: "a", Version: "2", ManifestSHA256: strings.Repeat("b", 64)},
		{ID: "a", Version: "1", ManifestSHA256: strings.Repeat("c", 64)},
	}}
	writeJSONFile(t, path, lock)
	loaded, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{loaded.Plugins[0].ID + loaded.Plugins[0].Version, loaded.Plugins[1].ID + loaded.Plugins[1].Version, loaded.Plugins[2].ID + loaded.Plugins[2].Version}
	if !reflect.DeepEqual(got, []string{"a1", "a2", "z1"}) {
		t.Fatalf("LoadLock order = %v", got)
	}
	if _, err := LoadLock(filepath.Join(directory, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadLock(missing) = %v", err)
	}

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown", data: `{"schema_version":"1.0","plugins":[],"extra":true}`, want: "unknown field"},
		{name: "multiple", data: string(mustJSON(t, lock)) + ` {}`, want: "multiple JSON values"},
		{name: "trailing", data: string(mustJSON(t, lock)) + ` x`, want: "invalid character"},
		{name: "schema", data: string(mustJSON(t, Lockfile{SchemaVersion: "0"})), want: "schema_version"},
		{name: "blank id", data: string(mustJSON(t, Lockfile{SchemaVersion: ManifestVersion, Plugins: []LockedPlugin{{Version: "1", ManifestSHA256: strings.Repeat("a", 64)}}})), want: "invalid locked plugin"},
		{name: "blank version", data: string(mustJSON(t, Lockfile{SchemaVersion: ManifestVersion, Plugins: []LockedPlugin{{ID: "a", ManifestSHA256: strings.Repeat("a", 64)}}})), want: "invalid locked plugin"},
		{name: "bad digest", data: string(mustJSON(t, Lockfile{SchemaVersion: ManifestVersion, Plugins: []LockedPlugin{{ID: "a", Version: "1", ManifestSHA256: "short"}}})), want: "invalid locked plugin"},
		{name: "duplicate", data: string(mustJSON(t, Lockfile{SchemaVersion: ManifestVersion, Plugins: []LockedPlugin{{ID: "a", Version: "1", ManifestSHA256: strings.Repeat("a", 64)}, {ID: "a", Version: "1", ManifestSHA256: strings.Repeat("b", 64)}}})), want: "duplicate locked plugin"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := filepath.Join(directory, test.name+".json")
			if err := os.WriteFile(candidate, []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadLock(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadLock() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSelectFiltersAndSorts(t *testing.T) {
	goExtract := validManifest("z.go", "1")
	goExtract.Inputs = Inputs{Languages: []string{"go"}, Capabilities: []string{"extract"}}
	goOther := validManifest("a.go", "1")
	goOther.Inputs = Inputs{Languages: []string{"go"}, Capabilities: []string{"other"}}
	python := validManifest("m.python", "1")
	python.Inputs = Inputs{Languages: []string{"python"}, Capabilities: []string{"extract"}}
	manifests := []Manifest{goExtract, python, goOther}

	selected := Select(manifests, "go", "extract")
	if len(selected) != 1 || selected[0].Plugin.ID != "z.go" {
		t.Fatalf("Select(go, extract) = %+v", selected)
	}
	selected = Select(manifests, "go", "")
	if got := []string{selected[0].Plugin.ID, selected[1].Plugin.ID}; !reflect.DeepEqual(got, []string{"a.go", "z.go"}) {
		t.Fatalf("Select(go) order = %v", got)
	}
	selected = Select(manifests, "", "extract")
	if got := []string{selected[0].Plugin.ID, selected[1].Plugin.ID}; !reflect.DeepEqual(got, []string{"m.python", "z.go"}) {
		t.Fatalf("Select(extract) order = %v", got)
	}
	if got := Select(manifests, "rust", ""); len(got) != 0 {
		t.Fatalf("Select(rust) = %+v", got)
	}
	if !contains([]string{"a", "b"}, "b") || contains([]string{"a"}, "missing") {
		t.Fatal("contains helper returned incorrect result")
	}
}

func TestDiscoverSortedSkipsInternalTreesAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "z", "plugin.json"), validManifest("z.plugin", "1"))
	writeJSONFile(t, filepath.Join(root, "a", "plugin.json"), validManifest("a.plugin", "1"))
	for _, skipped := range []string{".git", ".rkc", "node_modules"} {
		writeJSONFile(t, filepath.Join(root, skipped, "nested", "plugin.json"), validManifest("skip."+strings.TrimPrefix(skipped, "."), "1"))
	}
	manifests, paths, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 || len(paths) != 2 || manifests[0].Plugin.ID != "a.plugin" || manifests[1].Plugin.ID != "z.plugin" || !sort.StringsAreSorted(paths) {
		t.Fatalf("Discover() manifests=%v paths=%v", []string{manifests[0].Plugin.ID, manifests[1].Plugin.ID}, paths)
	}

	badRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(badRoot, "a", "plugin.json"), validManifest("a.plugin", "1"))
	if err := os.MkdirAll(filepath.Join(badRoot, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "b", "plugin.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifests, paths, err = Discover(badRoot)
	if err == nil || !strings.Contains(err.Error(), "load ") || manifests != nil || len(paths) != 2 {
		t.Fatalf("Discover(invalid) = manifests=%v paths=%v err=%v", manifests, paths, err)
	}
	if _, _, err := Discover(filepath.Join(root, "missing")); err == nil {
		t.Fatal("Discover(missing root) succeeded")
	}
}

func TestVerifyLockSuccessAndFailureModes(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("runtime artifact")
	sum := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(sum[:])
	manifest := validManifest("plugin.native", "1")
	manifest.Runtime = Runtime{Kind: "native-worker", Entrypoint: "bin/worker", Protocol: "json-v1", SHA256: artifactDigest}
	manifestPath := filepath.Join(pluginDir, "plugin.json")
	artifactPath := filepath.Join(pluginDir, "bin", "worker")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, artifact, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, manifestPath, manifest)
	lock := BuildLock([]Manifest{manifest})
	if failures := VerifyLock(root, lock, []Manifest{manifest}, []string{manifestPath}); len(failures) != 0 {
		t.Fatalf("VerifyLock(valid) = %v", failures)
	}
	// A shorter path list intentionally performs manifest/lock verification
	// without artifact verification.
	if failures := VerifyLock(root, lock, []Manifest{manifest}, nil); len(failures) != 0 {
		t.Fatalf("VerifyLock(without manifest path) = %v", failures)
	}

	tests := []struct {
		name      string
		lock      Lockfile
		manifests []Manifest
		paths     []string
		prepare   func(t *testing.T)
		want      string
	}{
		{name: "not locked", lock: Lockfile{SchemaVersion: ManifestVersion}, manifests: []Manifest{manifest}, paths: []string{manifestPath}, want: "is not present in lockfile"},
		{name: "locked undiscovered", lock: lock, want: "was not discovered"},
		{name: "manifest digest", lock: mutateLock(lock, func(item *LockedPlugin) { item.ManifestSHA256 = strings.Repeat("0", 64) }), manifests: []Manifest{manifest}, paths: []string{manifestPath}, want: "manifest digest"},
		{name: "runtime digest", lock: mutateLock(lock, func(item *LockedPlugin) { item.ArtifactSHA256 = strings.Repeat("1", 64) }), manifests: []Manifest{manifest}, paths: []string{manifestPath}, want: "runtime digest differs"},
		{name: "missing artifact", lock: lock, manifests: []Manifest{manifest}, paths: []string{manifestPath}, prepare: func(t *testing.T) {
			if err := os.Remove(artifactPath); err != nil {
				t.Fatal(err)
			}
		}, want: "resolve runtime artifact plugin/bin/worker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				t.Cleanup(func() {
					_ = os.MkdirAll(filepath.Dir(artifactPath), 0o755)
					_ = os.WriteFile(artifactPath, artifact, 0o755)
				})
				test.prepare(t)
			}
			failures := VerifyLock(root, test.lock, test.manifests, test.paths)
			if len(failures) == 0 || !strings.Contains(joinErrors(failures), test.want) {
				t.Fatalf("VerifyLock() = %v, want %q", failures, test.want)
			}
			messages := make([]string, len(failures))
			for index := range failures {
				messages[index] = failures[index].Error()
			}
			if !sort.StringsAreSorted(messages) {
				t.Fatalf("VerifyLock failures not sorted: %v", messages)
			}
		})
	}

	if err := os.WriteFile(artifactPath, []byte("wrong artifact"), 0o755); err != nil {
		t.Fatal(err)
	}
	if failures := VerifyLock(root, lock, []Manifest{manifest}, []string{manifestPath}); len(failures) == 0 || !strings.Contains(joinErrors(failures), "artifact digest") {
		t.Fatalf("VerifyLock(corrupt artifact) = %v", failures)
	}

	builtin := validManifest("plugin.builtin", "1")
	builtin.Runtime.SHA256 = strings.Repeat("a", 64)
	builtinLock := BuildLock([]Manifest{builtin})
	if failures := VerifyLock(root, builtinLock, []Manifest{builtin}, []string{filepath.Join(root, "does-not-exist", "plugin.json")}); len(failures) != 0 {
		t.Fatalf("VerifyLock(builtin) unexpectedly read artifact: %v", failures)
	}
}

func TestVerifyLockRejectsLexicalAndSymlinkArtifactEscapes(t *testing.T) {
	root := t.TempDir()
	pluginDirectory := filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(pluginDirectory, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external-worker")
	payload := []byte("external executable")
	if err := os.WriteFile(external, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	manifestPath := filepath.Join(pluginDirectory, "plugin.json")

	for _, test := range []struct {
		name       string
		entrypoint string
		prepare    func(t *testing.T)
	}{
		{name: "lexical traversal", entrypoint: "../external-worker"},
		{name: "symlink traversal", entrypoint: "bin/worker", prepare: func(t *testing.T) {
			if err := os.Symlink(external, filepath.Join(pluginDirectory, "bin", "worker")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare(t)
			}
			manifest := validManifest("plugin.escape", "1")
			manifest.Runtime = Runtime{Kind: "native-worker", Entrypoint: test.entrypoint, Protocol: "json-v1", SHA256: digest}
			lock := BuildLock([]Manifest{manifest})
			failures := VerifyLock(root, lock, []Manifest{manifest}, []string{manifestPath})
			if len(failures) == 0 || !strings.Contains(joinErrors(failures), "escapes plugin directory") {
				t.Fatalf("VerifyLock(%s) = %v, want containment failure", test.name, failures)
			}
		})
	}
}

func TestRelativePath(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "plugins", "worker")
	if got := relativePath(root, child); got != "plugins/worker" {
		t.Fatalf("relativePath() = %q", got)
	}
	if got := relativePath("", child); got != filepath.ToSlash(child) {
		t.Fatalf("relativePath(empty root) = %q", got)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mutateLock(lock Lockfile, mutate func(*LockedPlugin)) Lockfile {
	output := Lockfile{SchemaVersion: lock.SchemaVersion, Plugins: append([]LockedPlugin(nil), lock.Plugins...)}
	mutate(&output.Plugins[0])
	return output
}

func joinErrors(values []error) string {
	parts := make([]string, len(values))
	for index := range values {
		parts[index] = values[index].Error()
	}
	return strings.Join(parts, "\n")
}

func removeString(values []string, target string) []string {
	output := values[:0]
	for _, value := range values {
		if value != target {
			output = append(output, value)
		}
	}
	return output
}
