// Package workspace maintains private source registrations and atomically
// publishes immutable, verified atlas generations for read-only consumers.
package workspace

import "time"

const SchemaVersion = "rkc-workspace/v1"
const MaximumSources = 32

// Registry contains operator-private paths. Never expose it through an API.
type Registry struct {
	SchemaVersion string   `json:"schema_version"`
	Generation    uint64   `json:"generation"`
	Sources       []Source `json:"sources"`
}

// Source is an explicitly registered local folder or remote Git source.
type Source struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	Kind            string    `json:"kind"`
	LocalPath       string    `json:"local_path,omitempty"`
	RemoteURL       string    `json:"remote_url,omitempty"`
	Ref             string    `json:"ref,omitempty"`
	Excludes        []string  `json:"excludes"`
	ExcludePatterns []string  `json:"exclude_patterns,omitempty"`
	Limits          Limits    `json:"limits"`
	Active          *Active   `json:"active,omitempty"`
	Previous        *Active   `json:"previous,omitempty"`
	Freshness       Freshness `json:"freshness"`
}

// Limits bound every inventory and compilation; zero never disables a cap.
type Limits struct {
	MaxFiles           int   `json:"max_files"`
	MaxRepositoryBytes int64 `json:"max_repository_bytes"`
	MaxFileBytes       int64 `json:"max_file_bytes"`
	MaxTextBytes       int64 `json:"max_text_bytes"`
}

func DefaultLimits() Limits {
	return Limits{MaxFiles: 100000, MaxRepositoryBytes: 20 << 30, MaxFileBytes: 64 << 20, MaxTextBytes: 2 << 20}
}

// Active binds an immutable private atlas to its exact export manifest.
type Active struct {
	AtlasPath       string `json:"atlas_path"`
	SnapshotID      string `json:"snapshot_id"`
	Generation      string `json:"generation"`
	ManifestSHA256  string `json:"manifest_sha256"`
	Fingerprint     string `json:"fingerprint"`
	CompilerVersion string `json:"compiler_version"`
	SourceAdvanced  bool   `json:"source_advanced,omitempty"`
}

// Freshness is safe for public machine descriptors: errors are fixed codes,
// never subprocess output or path-bearing error messages. Current means the
// source matched the active generation at CheckedAt, not continuous freshness.
type Freshness struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	ErrorCode string    `json:"error_code,omitempty"`
}
