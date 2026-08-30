// Package pluginapi contains transport-safe contracts shared by plugin hosts,
// SDKs, and workers. It intentionally contains no database or pipeline types.
package pluginapi

import (
	"time"

	"github.com/neuroforge-io/RKC/pkg/graphpatch"
)

// ProtocolVersion is the host-worker envelope version implemented by this
// package. It is independent of the canonical RKC data schema version.
const ProtocolVersion = "1.0"

// Capability identifies the single plugin operation requested by a host.
type Capability string

const (
	// CapabilityDetect asks a plugin to identify supported or applicable inputs.
	CapabilityDetect Capability = "detect"
	// CapabilityNormalize asks a plugin to produce a canonical input projection.
	CapabilityNormalize Capability = "normalize"
	// CapabilityExtract asks a plugin to derive graph or document records.
	CapabilityExtract Capability = "extract"
	// CapabilityObserve asks a plugin to report bounded runtime observations.
	CapabilityObserve Capability = "observe"
	// CapabilityRender asks a plugin to produce a human- or machine-readable view.
	CapabilityRender Capability = "render"
	// CapabilityExport asks a plugin to package selected RKC records for a consumer.
	CapabilityExport Capability = "export"
)

// FileRef identifies one immutable, host-inventoried input without transferring
// ownership or granting ambient filesystem access.
type FileRef struct {
	// ArtifactID is the canonical RKC artifact identifier for the input.
	ArtifactID string `json:"artifact_id"`
	// Path is the slash-separated path relative to the inventoried workspace.
	Path string `json:"path"`
	// Language is the host's normalized source-language label when known.
	Language string `json:"language,omitempty"`
	// MediaType is the detected or declared media type when known.
	MediaType string `json:"media_type,omitempty"`
	// SHA256 is the content digest recorded during host inventory.
	SHA256 string `json:"sha256"`
	// SizeBytes is the inventoried content length in bytes when known.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Materialized is a host-provided path to the input when materialization is
	// part of the invocation contract.
	Materialized string `json:"materialized_path,omitempty"`
	// Attributes carries bounded host metadata not represented by dedicated fields.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// ResourceLimits contains host-issued execution and output ceilings. A worker
// must not interpret an omitted limit as additional authority.
type ResourceLimits struct {
	// MemoryMiB is the maximum memory allowance in mebibytes when set.
	MemoryMiB int64 `json:"memory_mib,omitempty"`
	// CPUTimeMillis is the maximum consumed CPU time in milliseconds when set.
	CPUTimeMillis int64 `json:"cpu_time_millis,omitempty"`
	// WallTimeMillis is the maximum elapsed execution time in milliseconds when set.
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	// OutputBytes is the maximum worker-output size in bytes when set.
	OutputBytes int64 `json:"output_bytes,omitempty"`
	// OpenFiles is the maximum number of concurrently open files when set.
	OpenFiles int `json:"open_files,omitempty"`
	// Processes is the maximum process count, including descendants, when set.
	Processes int `json:"processes,omitempty"`
}

// Request is the versioned, bounded envelope sent from an RKC host to a plugin
// worker for one capability invocation.
type Request struct {
	// ProtocolVersion selects the plugin envelope contract.
	ProtocolVersion string `json:"protocol_version"`
	// SchemaVersion selects the canonical RKC data schema for returned records.
	SchemaVersion string `json:"schema_version"`
	// RequestID is the host-generated correlation identifier for this invocation.
	RequestID string `json:"request_id"`
	// SnapshotID identifies the immutable snapshot being extended or rendered.
	SnapshotID string `json:"snapshot_id"`
	// Capability is the single operation requested from the worker.
	Capability Capability `json:"capability"`
	// Workspace is a host-defined reference to a preopened or materialized root.
	Workspace string `json:"workspace,omitempty"`
	// Files lists the immutable, inventoried inputs selected for this invocation.
	Files []FileRef `json:"files,omitempty"`
	// Configuration contains task-scoped plugin settings selected by host policy.
	Configuration map[string]any `json:"configuration,omitempty"`
	// Limits contains the execution ceilings granted by the host.
	Limits ResourceLimits `json:"limits,omitempty"`
	// Deadline is the absolute time by which the worker must stop when provided.
	Deadline *time.Time `json:"deadline,omitempty"`
}

// Usage reports the measured resources consumed by one plugin invocation.
type Usage struct {
	// WallTimeMillis is elapsed execution time in milliseconds.
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	// CPUTimeMillis is consumed CPU time in milliseconds.
	CPUTimeMillis int64 `json:"cpu_time_millis,omitempty"`
	// PeakRSSBytes is peak resident memory in bytes when observable.
	PeakRSSBytes int64 `json:"peak_rss_bytes,omitempty"`
	// ReadBytes is the number of bytes read by the worker when observable.
	ReadBytes int64 `json:"read_bytes,omitempty"`
	// WrittenBytes is the number of bytes written by the worker when observable.
	WrittenBytes int64 `json:"written_bytes,omitempty"`
}

// Response is the versioned envelope returned by a plugin worker. The host
// still validates identity, limits, vocabulary, evidence, and graph invariants
// before accepting Patch.
type Response struct {
	// ProtocolVersion identifies the plugin envelope contract used by the worker.
	ProtocolVersion string `json:"protocol_version"`
	// RequestID correlates this response with its originating Request.
	RequestID string `json:"request_id"`
	// Status is the worker-reported completion state.
	Status string `json:"status"`
	// Patch contains proposed canonical mutations for host validation and application.
	Patch graphpatch.Patch `json:"patch,omitempty"`
	// Usage reports measured resources consumed by the invocation.
	Usage Usage `json:"usage,omitempty"`
	// Warnings contains non-fatal worker observations for the host and operator.
	Warnings []string `json:"warnings,omitempty"`
	// Metadata carries bounded response attributes not represented above.
	Metadata map[string]string `json:"metadata,omitempty"`
}
