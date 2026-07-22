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

const ManifestVersion = "1.0"

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

type Identity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	APIVersion  string `json:"api_version"`
	License     string `json:"license"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
}

type Runtime struct {
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
	Protocol   string `json:"protocol"`
	SHA256     string `json:"sha256,omitempty"`
}

type Permissions struct {
	FilesystemRead  []string `json:"filesystem_read,omitempty"`
	FilesystemWrite []string `json:"filesystem_write,omitempty"`
	Environment     []string `json:"environment,omitempty"`
	Network         []string `json:"network,omitempty"`
	ProcessSpawn    []string `json:"process_spawn,omitempty"`
	Clock           bool     `json:"clock"`
	Random          bool     `json:"random"`
}

type Inputs struct {
	Languages    []string `json:"languages,omitempty"`
	MediaTypes   []string `json:"media_types,omitempty"`
	Globs        []string `json:"globs,omitempty"`
	Requires     []string `json:"requires,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type Outputs struct {
	NodeKinds       []string `json:"node_kinds,omitempty"`
	EdgeKinds       []string `json:"edge_kinds,omitempty"`
	EvidenceKinds   []string `json:"evidence_kinds,omitempty"`
	DiagnosticCodes []string `json:"diagnostic_codes,omitempty"`
}

type Limits struct {
	TimeoutSeconds int   `json:"timeout_seconds"`
	MemoryMiB      int64 `json:"memory_mib"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
	MaxParallelism int   `json:"max_parallelism"`
	OpenFiles      int   `json:"open_files,omitempty"`
	Processes      int   `json:"processes,omitempty"`
}

type Determinism struct {
	Level       string   `json:"level"`
	CacheInputs []string `json:"cache_inputs,omitempty"`
}

type Distribution struct {
	SourceURL           string `json:"source_url,omitempty"`
	ArtifactURL         string `json:"artifact_url,omitempty"`
	Signature           string `json:"signature,omitempty"`
	CertificateIdentity string `json:"certificate_identity,omitempty"`
	RekorEntry          string `json:"rekor_entry,omitempty"`
}

type Lockfile struct {
	SchemaVersion string         `json:"schema_version"`
	Plugins       []LockedPlugin `json:"plugins"`
}

type LockedPlugin struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Runtime        string `json:"runtime"`
	Source         string `json:"source,omitempty"`
}

type Policy struct {
	AllowedRuntimes   map[string]struct{}
	AllowNetwork      bool
	AllowProcessSpawn bool
	MaximumMemoryMiB  int64
	MaximumTimeout    int
	RequireDigest     bool
	RequireSignature  bool
}

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

func ManifestDigest(manifest Manifest) string {
	Normalize(&manifest)
	data, _ := json.Marshal(manifest)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

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
