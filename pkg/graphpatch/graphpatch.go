// Package graphpatch defines the only supported mutation contract for external
// analyzers. Plugins describe desired changes; the host validates and applies
// them transactionally. Plugins never receive direct database handles.
package graphpatch

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

const ProtocolVersion = "1.0"

type Producer struct {
	PluginID   string `json:"plugin_id"`
	Version    string `json:"version"`
	Runtime    string `json:"runtime,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

type Patch struct {
	ProtocolVersion string            `json:"protocol_version"`
	SchemaVersion   string            `json:"schema_version"`
	SnapshotID      string            `json:"snapshot_id"`
	Producer        Producer          `json:"producer"`
	GeneratedAt     time.Time         `json:"generated_at,omitempty"`
	InputDigest     string            `json:"input_digest,omitempty"`
	Fragment        rkcmodel.Fragment `json:"fragment"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type Limits struct {
	MaxArtifacts   int
	MaxNodes       int
	MaxEdges       int
	MaxEvidence    int
	MaxDiagnostics int
	MaxDocuments   int
	MaxClaims      int
}

type ValidationOptions struct {
	ExpectedSnapshotID string
	AllowedPluginID    string
	StrictVocabulary   bool
	RequireEvidence    bool
	Limits             Limits
}

type ValidationIssue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	ObjectID string `json:"object_id,omitempty"`
}

type ValidationReport struct {
	Accepted bool              `json:"accepted"`
	Issues   []ValidationIssue `json:"issues,omitempty"`
}

func (report *ValidationReport) add(severity, code, message, objectID string) {
	report.Issues = append(report.Issues, ValidationIssue{Severity: severity, Code: code, Message: message, ObjectID: objectID})
	if severity == "error" || severity == "fatal" {
		report.Accepted = false
	}
}

func Validate(patch Patch, options ValidationOptions) ValidationReport {
	report := ValidationReport{Accepted: true}
	if patch.ProtocolVersion != ProtocolVersion {
		report.add("error", "RKC-PATCH-001", "unsupported GraphPatch protocol version", patch.ProtocolVersion)
	}
	if patch.SchemaVersion != rkcmodel.SchemaVersion {
		report.add("error", "RKC-PATCH-002", "GraphPatch schema version does not match host", patch.SchemaVersion)
	}
	if options.ExpectedSnapshotID != "" && patch.SnapshotID != options.ExpectedSnapshotID {
		report.add("error", "RKC-PATCH-003", "GraphPatch snapshot does not match active build", patch.SnapshotID)
	}
	if patch.Producer.PluginID == "" {
		report.add("error", "RKC-PATCH-004", "producer plugin_id is required", "")
	}
	if options.AllowedPluginID != "" && patch.Producer.PluginID != options.AllowedPluginID {
		report.add("error", "RKC-PATCH-005", "producer does not match installed plugin", patch.Producer.PluginID)
	}
	checkLimit := func(name string, count, limit int) {
		if limit > 0 && count > limit {
			report.add("error", "RKC-PATCH-006", fmt.Sprintf("%s count %d exceeds limit %d", name, count, limit), "")
		}
	}
	checkLimit("artifacts", len(patch.Fragment.Artifacts), options.Limits.MaxArtifacts)
	checkLimit("nodes", len(patch.Fragment.Nodes), options.Limits.MaxNodes)
	checkLimit("edges", len(patch.Fragment.Edges), options.Limits.MaxEdges)
	checkLimit("evidence", len(patch.Fragment.Evidence), options.Limits.MaxEvidence)
	checkLimit("diagnostics", len(patch.Fragment.Diagnostics), options.Limits.MaxDiagnostics)
	checkLimit("documents", len(patch.Fragment.Documents), options.Limits.MaxDocuments)
	checkLimit("claims", len(patch.Fragment.Claims), options.Limits.MaxClaims)

	fragmentBundle := rkcmodel.Bundle{
		Snapshot:    rkcmodel.Snapshot{ID: patch.SnapshotID},
		Artifacts:   append([]rkcmodel.Artifact(nil), patch.Fragment.Artifacts...),
		Nodes:       append([]rkcmodel.Node(nil), patch.Fragment.Nodes...),
		Edges:       append([]rkcmodel.Edge(nil), patch.Fragment.Edges...),
		Evidence:    append([]rkcmodel.Evidence(nil), patch.Fragment.Evidence...),
		Diagnostics: append([]rkcmodel.Diagnostic(nil), patch.Fragment.Diagnostics...),
		Conflicts:   append([]rkcmodel.Conflict(nil), patch.Fragment.Conflicts...),
		Documents:   append([]rkcmodel.Document(nil), patch.Fragment.Documents...),
		Claims:      append([]rkcmodel.Claim(nil), patch.Fragment.Claims...),
		Paths:       append([]rkcmodel.ExecutionPath(nil), patch.Fragment.Paths...),
	}

	// A patch may legitimately reference nodes already present in the host, so
	// full endpoint validation happens in Apply. Here we validate local records,
	// IDs, vocabulary, producer ownership, and evidence shape.
	seen := map[string]string{}
	for _, node := range patch.Fragment.Nodes {
		validateUnique(&report, seen, node.ID, "node")
		if options.StrictVocabulary && !rkcmodel.IsKnownNodeKind(node.Kind) {
			report.add("error", "RKC-PATCH-010", "unknown node kind", node.Kind)
		}
		if node.ID == "" || node.Name == "" {
			report.add("error", "RKC-PATCH-011", "node id and name are required", node.ID)
		}
	}
	for _, edge := range patch.Fragment.Edges {
		validateUnique(&report, seen, edge.ID, "edge")
		if edge.From == "" || edge.To == "" {
			report.add("error", "RKC-PATCH-012", "edge endpoints are required", edge.ID)
		}
		if options.StrictVocabulary && !rkcmodel.IsKnownEdgeKind(edge.Kind) {
			report.add("error", "RKC-PATCH-013", "unknown edge kind", edge.Kind)
		}
		if !rkcmodel.IsKnownResolution(edge.Resolution) {
			report.add("error", "RKC-PATCH-014", "unknown edge resolution", edge.Resolution)
		}
	}
	for _, evidence := range patch.Fragment.Evidence {
		validateUnique(&report, seen, evidence.ID, "evidence")
		if evidence.Confidence < 0 || evidence.Confidence > 1 {
			report.add("error", "RKC-PATCH-015", "evidence confidence must be in [0,1]", evidence.ID)
		}
	}
	if options.RequireEvidence {
		evidenceIDs := map[string]struct{}{}
		for _, evidence := range patch.Fragment.Evidence {
			evidenceIDs[evidence.ID] = struct{}{}
		}
		for _, node := range patch.Fragment.Nodes {
			if rkcmodel.IsSymbolKind(node.Kind) && len(node.EvidenceIDs) == 0 {
				report.add("warning", "RKC-PATCH-016", "symbol has no evidence in patch", node.ID)
			}
			for _, evidenceID := range node.EvidenceIDs {
				if _, ok := evidenceIDs[evidenceID]; !ok {
					report.add("warning", "RKC-PATCH-017", "node references evidence outside patch; host must resolve it", evidenceID)
				}
			}
		}
	}
	_ = fragmentBundle // retained to make the relationship to the canonical model explicit.
	sort.Slice(report.Issues, func(i, j int) bool {
		left := report.Issues[i].Code + "\x00" + report.Issues[i].ObjectID + "\x00" + report.Issues[i].Message
		right := report.Issues[j].Code + "\x00" + report.Issues[j].ObjectID + "\x00" + report.Issues[j].Message
		return left < right
	})
	return report
}

func validateUnique(report *ValidationReport, seen map[string]string, id, kind string) {
	if strings.TrimSpace(id) == "" {
		report.add("error", "RKC-PATCH-020", kind+" id is empty", "")
		return
	}
	if prior, ok := seen[id]; ok {
		report.add("error", "RKC-PATCH-021", fmt.Sprintf("duplicate id shared by %s and %s", prior, kind), id)
		return
	}
	seen[id] = kind
}

// Apply appends a validated patch and then runs canonical host validation. It
// deliberately does not silently overwrite host records with equal IDs.
func Apply(bundle *rkcmodel.Bundle, patch Patch, options ValidationOptions) ValidationReport {
	report := Validate(patch, options)
	if !report.Accepted {
		return report
	}

	existing := map[string]string{}
	for _, artifact := range bundle.Artifacts {
		existing[artifact.ID] = "artifact"
	}
	for _, node := range bundle.Nodes {
		existing[node.ID] = "node"
	}
	for _, edge := range bundle.Edges {
		existing[edge.ID] = "edge"
	}
	for _, evidence := range bundle.Evidence {
		existing[evidence.ID] = "evidence"
	}
	for _, document := range bundle.Documents {
		existing[document.ID] = "document"
	}
	for _, claim := range bundle.Claims {
		existing[claim.ID] = "claim"
	}

	for id, kind := range patchIDs(patch) {
		if prior, ok := existing[id]; ok {
			report.add("error", "RKC-PATCH-030", fmt.Sprintf("patch %s id collides with host %s", kind, prior), id)
		}
	}
	if !report.Accepted {
		return report
	}

	bundle.Artifacts = append(bundle.Artifacts, patch.Fragment.Artifacts...)
	bundle.Nodes = append(bundle.Nodes, patch.Fragment.Nodes...)
	bundle.Edges = append(bundle.Edges, patch.Fragment.Edges...)
	bundle.Evidence = append(bundle.Evidence, patch.Fragment.Evidence...)
	bundle.Diagnostics = append(bundle.Diagnostics, patch.Fragment.Diagnostics...)
	bundle.Conflicts = append(bundle.Conflicts, patch.Fragment.Conflicts...)
	bundle.Documents = append(bundle.Documents, patch.Fragment.Documents...)
	bundle.Claims = append(bundle.Claims, patch.Fragment.Claims...)
	bundle.Paths = append(bundle.Paths, patch.Fragment.Paths...)
	rkcmodel.SortBundle(bundle)

	hostReport := rkcmodel.ValidateBundle(*bundle, rkcmodel.ValidationOptions{StrictVocabulary: options.StrictVocabulary, RequireEvidence: options.RequireEvidence})
	for _, diagnostic := range hostReport.Diagnostics {
		severity := diagnostic.Severity
		if severity == "fatal" {
			severity = "error"
		}
		report.add(severity, diagnostic.Code, diagnostic.Message, diagnostic.ID)
	}
	return report
}

func patchIDs(patch Patch) map[string]string {
	ids := map[string]string{}
	for _, value := range patch.Fragment.Artifacts {
		ids[value.ID] = "artifact"
	}
	for _, value := range patch.Fragment.Nodes {
		ids[value.ID] = "node"
	}
	for _, value := range patch.Fragment.Edges {
		ids[value.ID] = "edge"
	}
	for _, value := range patch.Fragment.Evidence {
		ids[value.ID] = "evidence"
	}
	for _, value := range patch.Fragment.Documents {
		ids[value.ID] = "document"
	}
	for _, value := range patch.Fragment.Claims {
		ids[value.ID] = "claim"
	}
	return ids
}
