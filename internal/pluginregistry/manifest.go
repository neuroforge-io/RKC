// Package pluginregistry implements versioned plugin manifests and lockfiles.
// It does not execute plugins. Execution belongs to a capability-restricted
// host; the registry only decides what is installed, compatible, allowed, and
// reproducibly selected.
package pluginregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// ManifestVersion is the manifest schema accepted by Validate and the lockfile
// schema emitted by BuildLock.
const ManifestVersion = "1.0"

// Manifest is a plugin's declarative identity, runtime, capability request,
// input/output contract, limits, determinism claim, and distribution metadata.
// A valid manifest describes requested authority; it does not grant capabilities
// or execute the plugin.
type Manifest struct {
	SchemaURI     string       `json:"$schema,omitempty"`
	SchemaVersion string       `json:"schema_version"`
	Plugin        Identity     `json:"plugin"`
	Runtime       Runtime      `json:"runtime"`
	Capabilities  Permissions  `json:"capabilities"`
	Inputs        Inputs       `json:"inputs"`
	Outputs       Outputs      `json:"outputs"`
	Limits        Limits       `json:"limits"`
	Determinism   Determinism  `json:"determinism"`
	Distribution  Distribution `json:"distribution,omitempty"`
}

// Identity names and versions a plugin and records its declared API and license.
// Validate requires every field through License and restricts ID to the portable
// lowercase identifier grammar.
type Identity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	APIVersion  string `json:"api_version"`
	License     string `json:"license"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
}

// Runtime identifies the requested host kind, canonical plugin-relative
// entrypoint, protocol, and optional artifact digest. Digest presence is policy
// controlled; this record does not verify the artifact.
type Runtime struct {
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
	Protocol   string `json:"protocol"`
	SHA256     string `json:"sha256,omitempty"`
}

// Permissions declares capabilities requested by a plugin. These values are not
// grants: Validate applies only its Policy gates and the execution host must
// enforce every admitted capability.
type Permissions struct {
	FilesystemRead  []string `json:"filesystem_read,omitempty"`
	FilesystemWrite []string `json:"filesystem_write,omitempty"`
	Environment     []string `json:"environment,omitempty"`
	Network         []string `json:"network,omitempty"`
	ProcessSpawn    []string `json:"process_spawn,omitempty"`
	Clock           bool     `json:"clock"`
	Random          bool     `json:"random"`
}

// Inputs declares exact selection metadata for languages, media types, globs,
// prerequisites, and capabilities. The declarations do not prove that a plugin
// can process a selected input.
type Inputs struct {
	Languages    []string `json:"languages,omitempty"`
	MediaTypes   []string `json:"media_types,omitempty"`
	Globs        []string `json:"globs,omitempty"`
	Requires     []string `json:"requires,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Outputs declares the graph and diagnostic kinds a plugin may emit. Validate
// checks graph kinds against the RKC schema, but downstream fragment validation
// remains authoritative for actual output.
type Outputs struct {
	NodeKinds       []string `json:"node_kinds,omitempty"`
	EdgeKinds       []string `json:"edge_kinds,omitempty"`
	EvidenceKinds   []string `json:"evidence_kinds,omitempty"`
	DiagnosticCodes []string `json:"diagnostic_codes,omitempty"`
}

// Limits declares requested execution ceilings. Validate checks minimums and
// configured policy maxima; the execution host is responsible for enforcing
// them at runtime.
type Limits struct {
	TimeoutSeconds int   `json:"timeout_seconds"`
	MemoryMiB      int64 `json:"memory_mib"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
	MaxParallelism int   `json:"max_parallelism"`
	OpenFiles      int   `json:"open_files,omitempty"`
	Processes      int   `json:"processes,omitempty"`
}

// Determinism records a plugin's declared reproducibility level and cache inputs.
// It is metadata for selection and caching, not evidence that execution is
// deterministic.
type Determinism struct {
	Level       string   `json:"level"`
	CacheInputs []string `json:"cache_inputs,omitempty"`
}

// Distribution records source, artifact, and signing provenance claimed by a
// plugin. Validate can require a nonempty Signature, but it does not perform
// cryptographic signature, certificate, or transparency-log verification.
type Distribution struct {
	SourceURL           string `json:"source_url,omitempty"`
	ArtifactURL         string `json:"artifact_url,omitempty"`
	Signature           string `json:"signature,omitempty"`
	CertificateIdentity string `json:"certificate_identity,omitempty"`
	RekorEntry          string `json:"rekor_entry,omitempty"`
}

// Lockfile records a versioned, reproducibly ordered plugin selection. Use
// LoadLock and VerifyLock before treating its records as valid bindings.
type Lockfile struct {
	SchemaVersion string         `json:"schema_version"`
	Plugins       []LockedPlugin `json:"plugins"`
}

// LockedPlugin binds an ID and version to normalized manifest and optional
// runtime-artifact digests plus selection provenance.
type LockedPlugin struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Runtime        string `json:"runtime"`
	Source         string `json:"source,omitempty"`
}

// Policy constrains manifest admission. Its zero value permits every recognized
// runtime, requires neither digest nor signature, and sets no policy maximum for
// memory or timeout, while denying requested network and process-spawn
// capabilities.
type Policy struct {
	AllowedRuntimes   map[string]struct{}
	AllowNetwork      bool
	AllowProcessSpawn bool
	MaximumMemoryMiB  int64
	MaximumTimeout    int
	RequireDigest     bool
	RequireSignature  bool
}

// LoadManifest reads one JSON value with unknown fields rejected, normalizes its
// list fields, and validates it with the zero-value Policy. In particular, that
// denies network and process-spawn requests but does not require a digest or
// signature.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Manifest{}, errors.New("plugin manifest contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, err
	}
	Normalize(&manifest)
	if err := Validate(manifest, Policy{}); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Normalize mutates manifest by trimming, deduplicating, and sorting its input,
// output, permission, and cache-input lists. It does not normalize scalar fields
// or validate the result; manifest must be non-nil.
func Normalize(manifest *Manifest) {
	manifest.Inputs.Languages = uniqueSorted(manifest.Inputs.Languages)
	manifest.Inputs.MediaTypes = uniqueSorted(manifest.Inputs.MediaTypes)
	manifest.Inputs.Globs = uniqueSorted(manifest.Inputs.Globs)
	manifest.Inputs.Requires = uniqueSorted(manifest.Inputs.Requires)
	manifest.Inputs.Capabilities = uniqueSorted(manifest.Inputs.Capabilities)
	manifest.Outputs.NodeKinds = uniqueSorted(manifest.Outputs.NodeKinds)
	manifest.Outputs.EdgeKinds = uniqueSorted(manifest.Outputs.EdgeKinds)
	manifest.Outputs.EvidenceKinds = uniqueSorted(manifest.Outputs.EvidenceKinds)
	manifest.Outputs.DiagnosticCodes = uniqueSorted(manifest.Outputs.DiagnosticCodes)
	manifest.Capabilities.FilesystemRead = uniqueSorted(manifest.Capabilities.FilesystemRead)
	manifest.Capabilities.FilesystemWrite = uniqueSorted(manifest.Capabilities.FilesystemWrite)
	manifest.Capabilities.Environment = uniqueSorted(manifest.Capabilities.Environment)
	manifest.Capabilities.Network = uniqueSorted(manifest.Capabilities.Network)
	manifest.Capabilities.ProcessSpawn = uniqueSorted(manifest.Capabilities.ProcessSpawn)
	manifest.Determinism.CacheInputs = uniqueSorted(manifest.Determinism.CacheInputs)
}

// Validate checks manifest schema, required identity/runtime fields, portable
// entrypoint syntax, known output kinds, core declared limits, determinism level,
// and the supplied Policy. It neither normalizes the manifest nor verifies
// runtime bytes or distribution signatures.
func Validate(manifest Manifest, policy Policy) error {
	var failures []string
	if manifest.SchemaVersion != ManifestVersion {
		failures = append(failures, "schema_version must be "+ManifestVersion)
	}
	if manifest.Plugin.ID == "" || !validID(manifest.Plugin.ID) {
		failures = append(failures, "plugin.id is invalid")
	}
	if manifest.Plugin.Name == "" {
		failures = append(failures, "plugin.name is required")
	}
	if manifest.Plugin.Version == "" {
		failures = append(failures, "plugin.version is required")
	}
	if manifest.Plugin.APIVersion == "" {
		failures = append(failures, "plugin.api_version is required")
	}
	if manifest.Plugin.License == "" {
		failures = append(failures, "plugin.license is required")
	}
	switch manifest.Runtime.Kind {
	case "builtin", "wasm-wasi", "native-worker":
	default:
		failures = append(failures, "runtime.kind is invalid")
	}
	if manifest.Runtime.Entrypoint == "" {
		failures = append(failures, "runtime.entrypoint is required")
	} else if !validEntrypoint(manifest.Runtime.Entrypoint) {
		failures = append(failures, "runtime.entrypoint must be a canonical relative path contained by the plugin directory")
	}
	if manifest.Runtime.Protocol == "" {
		failures = append(failures, "runtime.protocol is required")
	}
	if policy.RequireDigest && len(manifest.Runtime.SHA256) != 64 {
		failures = append(failures, "runtime.sha256 is required by policy")
	}
	if policy.RequireSignature && manifest.Distribution.Signature == "" {
		failures = append(failures, "distribution.signature is required by policy")
	}
	if len(policy.AllowedRuntimes) > 0 {
		if _, ok := policy.AllowedRuntimes[manifest.Runtime.Kind]; !ok {
			failures = append(failures, "runtime is denied by policy")
		}
	}
	if !policy.AllowNetwork && len(manifest.Capabilities.Network) > 0 {
		failures = append(failures, "network capability is denied by policy")
	}
	if !policy.AllowProcessSpawn && len(manifest.Capabilities.ProcessSpawn) > 0 {
		failures = append(failures, "process_spawn capability is denied by policy")
	}
	if manifest.Limits.TimeoutSeconds <= 0 {
		failures = append(failures, "limits.timeout_seconds must be positive")
	}
	if manifest.Limits.MemoryMiB < 16 {
		failures = append(failures, "limits.memory_mib must be at least 16")
	}
	if manifest.Limits.MaxOutputBytes < 1024 {
		failures = append(failures, "limits.max_output_bytes must be at least 1024")
	}
	if manifest.Limits.MaxParallelism <= 0 {
		failures = append(failures, "limits.max_parallelism must be positive")
	}
	if policy.MaximumMemoryMiB > 0 && manifest.Limits.MemoryMiB > policy.MaximumMemoryMiB {
		failures = append(failures, "plugin memory exceeds policy")
	}
	if policy.MaximumTimeout > 0 && manifest.Limits.TimeoutSeconds > policy.MaximumTimeout {
		failures = append(failures, "plugin timeout exceeds policy")
	}
	for _, kind := range manifest.Outputs.NodeKinds {
		if !rkcmodel.IsKnownNodeKind(kind) {
			failures = append(failures, "unknown node kind: "+kind)
		}
	}
	for _, kind := range manifest.Outputs.EdgeKinds {
		if !rkcmodel.IsKnownEdgeKind(kind) {
			failures = append(failures, "unknown edge kind: "+kind)
		}
	}
	for _, kind := range manifest.Outputs.EvidenceKinds {
		if !rkcmodel.IsKnownEvidenceKind(kind) {
			failures = append(failures, "unknown evidence kind: "+kind)
		}
	}
	switch manifest.Determinism.Level {
	case "deterministic", "toolchain-dependent", "environment-dependent", "nondeterministic":
	default:
		failures = append(failures, "determinism.level is invalid")
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// ManifestDigest returns the lowercase SHA-256 of the manifest's normalized JSON
// representation. It does not validate the manifest or hash the runtime artifact.
func ManifestDigest(manifest Manifest) string {
	Normalize(&manifest)
	data, _ := json.Marshal(manifest)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BuildLock converts manifests into digest bindings sorted by plugin ID and
// version. It does not validate inputs or remove duplicate identities.
func BuildLock(manifests []Manifest) Lockfile {
	plugins := make([]LockedPlugin, 0, len(manifests))
	for _, manifest := range manifests {
		plugins = append(plugins, LockedPlugin{ID: manifest.Plugin.ID, Version: manifest.Plugin.Version, ManifestSHA256: ManifestDigest(manifest), ArtifactSHA256: manifest.Runtime.SHA256, Runtime: manifest.Runtime.Kind, Source: manifest.Distribution.ArtifactURL})
	}
	sort.Slice(plugins, func(i, j int) bool {
		if plugins[i].ID == plugins[j].ID {
			return plugins[i].Version < plugins[j].Version
		}
		return plugins[i].ID < plugins[j].ID
	})
	return Lockfile{SchemaVersion: ManifestVersion, Plugins: plugins}
}

// LockDigest returns the lowercase SHA-256 of JSON after sorting a copy of the
// lock's plugin records by ID and version. It does not validate schema, digest
// fields, or duplicate records.
func LockDigest(lock Lockfile) string {
	lock.Plugins = append([]LockedPlugin(nil), lock.Plugins...)
	sort.Slice(lock.Plugins, func(i, j int) bool {
		if lock.Plugins[i].ID == lock.Plugins[j].ID {
			return lock.Plugins[i].Version < lock.Plugins[j].Version
		}
		return lock.Plugins[i].ID < lock.Plugins[j].ID
	})
	data, _ := json.Marshal(lock)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Select returns manifests whose declared language and capability exactly match
// nonempty filters, sorted by plugin ID. It performs no validation or policy
// admission, and empty filters impose no restriction.
func Select(manifests []Manifest, language, capability string) []Manifest {
	var output []Manifest
	for _, manifest := range manifests {
		if language != "" && !contains(manifest.Inputs.Languages, language) {
			continue
		}
		if capability != "" && !contains(manifest.Inputs.Capabilities, capability) {
			continue
		}
		output = append(output, manifest)
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Plugin.ID < output[j].Plugin.ID })
	return output
}

func validID(value string) bool {
	for index, runeValue := range value {
		if runeValue >= 'a' && runeValue <= 'z' || runeValue >= '0' && runeValue <= '9' {
			continue
		}
		if index > 0 && (runeValue == '.' || runeValue == '_' || runeValue == '-') {
			continue
		}
		return false
	}
	return value != ""
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	output := make([]string, 0, len(seen))
	for value := range seen {
		output = append(output, value)
	}
	sort.Strings(output)
	return output
}

// LoadLock decodes a strict plugin lockfile and normalizes plugin order.
func LoadLock(path string) (Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Lockfile{}, err
	}
	var lock Lockfile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return Lockfile{}, fmt.Errorf("decode plugin lockfile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Lockfile{}, errors.New("plugin lockfile contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return Lockfile{}, err
	}
	if lock.SchemaVersion != ManifestVersion {
		return Lockfile{}, fmt.Errorf("plugin lockfile schema_version must be %s", ManifestVersion)
	}
	sort.Slice(lock.Plugins, func(i, j int) bool {
		if lock.Plugins[i].ID == lock.Plugins[j].ID {
			return lock.Plugins[i].Version < lock.Plugins[j].Version
		}
		return lock.Plugins[i].ID < lock.Plugins[j].ID
	})
	seen := map[string]struct{}{}
	for _, plugin := range lock.Plugins {
		key := plugin.ID + "@" + plugin.Version
		if plugin.ID == "" || plugin.Version == "" || len(plugin.ManifestSHA256) != 64 {
			return Lockfile{}, fmt.Errorf("invalid locked plugin %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return Lockfile{}, fmt.Errorf("duplicate locked plugin %s", key)
		}
		seen[key] = struct{}{}
	}
	return lock, nil
}

// Discover finds strict plugin.json manifests below root. Invalid manifests are
// returned as errors rather than silently omitted.
func Discover(root string) ([]Manifest, []string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			base := entry.Name()
			if base == ".git" || base == ".rkc" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "plugin.json" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	manifests := make([]Manifest, 0, len(paths))
	for _, path := range paths {
		manifest, err := LoadManifest(path)
		if err != nil {
			return nil, paths, fmt.Errorf("load %s: %w", path, err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, paths, nil
}

// VerifyLock compares discovered manifests and runtime artifacts with a lock.
// root is used only to render stable relative paths in failures.
func VerifyLock(root string, lock Lockfile, manifests []Manifest, manifestPaths []string) []error {
	var failures []error
	locked := map[string]LockedPlugin{}
	for _, item := range lock.Plugins {
		locked[item.ID+"@"+item.Version] = item
	}
	seen := map[string]struct{}{}
	for index, manifest := range manifests {
		key := manifest.Plugin.ID + "@" + manifest.Plugin.Version
		item, ok := locked[key]
		if !ok {
			failures = append(failures, fmt.Errorf("plugin %s is not present in lockfile", key))
			continue
		}
		seen[key] = struct{}{}
		if digest := ManifestDigest(manifest); digest != item.ManifestSHA256 {
			failures = append(failures, fmt.Errorf("plugin %s manifest digest %s does not match lock %s", key, digest, item.ManifestSHA256))
		}
		if item.ArtifactSHA256 != manifest.Runtime.SHA256 {
			failures = append(failures, fmt.Errorf("plugin %s runtime digest differs between manifest and lock", key))
		}
		if index < len(manifestPaths) && manifest.Runtime.Entrypoint != "" && manifest.Runtime.Kind != "builtin" {
			pluginDirectory, err := filepath.Abs(filepath.Dir(manifestPaths[index]))
			if err != nil {
				failures = append(failures, fmt.Errorf("plugin %s resolve plugin directory: %w", key, err))
				continue
			}
			pluginDirectory, err = filepath.EvalSymlinks(pluginDirectory)
			if err != nil {
				failures = append(failures, fmt.Errorf("plugin %s resolve plugin directory symlinks: %w", key, err))
				continue
			}
			artifactPath := filepath.Join(pluginDirectory, filepath.FromSlash(manifest.Runtime.Entrypoint))
			resolvedArtifact, err := filepath.EvalSymlinks(artifactPath)
			if err != nil {
				failures = append(failures, fmt.Errorf("plugin %s resolve runtime artifact %s: %w", key, relativePath(root, artifactPath), err))
				continue
			}
			if !pathContainedBy(pluginDirectory, resolvedArtifact) {
				failures = append(failures, fmt.Errorf("plugin %s runtime artifact escapes plugin directory", key))
				continue
			}
			artifactPath = resolvedArtifact
			data, err := os.ReadFile(artifactPath)
			if err != nil {
				failures = append(failures, fmt.Errorf("plugin %s read runtime artifact %s: %w", key, relativePath(root, artifactPath), err))
			} else {
				sum := sha256.Sum256(data)
				digest := hex.EncodeToString(sum[:])
				if digest != manifest.Runtime.SHA256 {
					failures = append(failures, fmt.Errorf("plugin %s artifact digest %s does not match manifest %s", key, digest, manifest.Runtime.SHA256))
				}
			}
		}
	}
	for key := range locked {
		if _, ok := seen[key]; !ok {
			failures = append(failures, fmt.Errorf("locked plugin %s was not discovered", key))
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Error() < failures[j].Error() })
	return failures
}

func validEntrypoint(value string) bool {
	if value == "" || strings.Contains(value, "\\") || !filepath.IsLocal(filepath.FromSlash(value)) {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	return cleaned == value && cleaned != "."
}

func pathContainedBy(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func relativePath(root, path string) string {
	if root != "" {
		if relative, err := filepath.Rel(root, path); err == nil {
			return filepath.ToSlash(relative)
		}
	}
	return filepath.ToSlash(path)
}
