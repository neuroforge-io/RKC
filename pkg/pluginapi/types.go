// Package pluginapi contains transport-safe contracts shared by plugin hosts,
// SDKs, and workers. It intentionally contains no database or pipeline types.
package pluginapi

import (
	"time"

	"github.com/repository-knowledge-compiler/rkc/pkg/graphpatch"
)

const ProtocolVersion = "1.0"

type Capability string

const (
	CapabilityDetect    Capability = "detect"
	CapabilityNormalize Capability = "normalize"
	CapabilityExtract   Capability = "extract"
	CapabilityObserve   Capability = "observe"
	CapabilityRender    Capability = "render"
	CapabilityExport    Capability = "export"
)

type FileRef struct {
	ArtifactID   string            `json:"artifact_id"`
	Path         string            `json:"path"`
	Language     string            `json:"language,omitempty"`
	MediaType    string            `json:"media_type,omitempty"`
	SHA256       string            `json:"sha256"`
	SizeBytes    int64             `json:"size_bytes,omitempty"`
	Materialized string            `json:"materialized_path,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type ResourceLimits struct {
	MemoryMiB      int64 `json:"memory_mib,omitempty"`
	CPUTimeMillis  int64 `json:"cpu_time_millis,omitempty"`
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	OutputBytes    int64 `json:"output_bytes,omitempty"`
	OpenFiles      int   `json:"open_files,omitempty"`
	Processes      int   `json:"processes,omitempty"`
}

type Request struct {
	ProtocolVersion string         `json:"protocol_version"`
	SchemaVersion   string         `json:"schema_version"`
	RequestID       string         `json:"request_id"`
	SnapshotID      string         `json:"snapshot_id"`
	Capability      Capability     `json:"capability"`
	Workspace       string         `json:"workspace,omitempty"`
	Files           []FileRef      `json:"files,omitempty"`
	Configuration   map[string]any `json:"configuration,omitempty"`
	Limits          ResourceLimits `json:"limits,omitempty"`
	Deadline        *time.Time     `json:"deadline,omitempty"`
}

type Usage struct {
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	CPUTimeMillis  int64 `json:"cpu_time_millis,omitempty"`
	PeakRSSBytes   int64 `json:"peak_rss_bytes,omitempty"`
	ReadBytes      int64 `json:"read_bytes,omitempty"`
	WrittenBytes   int64 `json:"written_bytes,omitempty"`
}

type Response struct {
	ProtocolVersion string            `json:"protocol_version"`
	RequestID       string            `json:"request_id"`
	Status          string            `json:"status"`
	Patch           graphpatch.Patch  `json:"patch,omitempty"`
	Usage           Usage             `json:"usage,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}
