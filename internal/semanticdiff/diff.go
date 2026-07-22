// Package semanticdiff compares two immutable repository snapshots at the
// artifact, logical-node, and relationship levels. It reports evidence-backed
// structural changes rather than a decorative pile of changed lines.
package semanticdiff

import (
	"sort"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityRisk     Severity = "risk"
	SeverityBreaking Severity = "breaking"
)

type ArtifactChange struct {
	Kind   string             `json:"kind"`
	Before *rkcmodel.Artifact `json:"before,omitempty"`
	After  *rkcmodel.Artifact `json:"after,omitempty"`
	Fields []string           `json:"fields,omitempty"`
}

type NodeChange struct {
	Kind       string         `json:"kind"`
	Severity   Severity       `json:"severity"`
	LogicalKey string         `json:"logical_key"`
	Before     *rkcmodel.Node `json:"before,omitempty"`
	After      *rkcmodel.Node `json:"after,omitempty"`
	Fields     []string       `json:"fields,omitempty"`
	Reasons    []string       `json:"reasons,omitempty"`
}

type EdgeChange struct {
	Kind   string         `json:"kind"`
	Before *rkcmodel.Edge `json:"before,omitempty"`
	After  *rkcmodel.Edge `json:"after,omitempty"`
	Fields []string       `json:"fields,omitempty"`
}

type Summary struct {
	ArtifactsAdded    int `json:"artifacts_added"`
	ArtifactsRemoved  int `json:"artifacts_removed"`
	ArtifactsModified int `json:"artifacts_modified"`
	NodesAdded        int `json:"nodes_added"`
	NodesRemoved      int `json:"nodes_removed"`
	NodesModified     int `json:"nodes_modified"`
	EdgesAdded        int `json:"edges_added"`
	EdgesRemoved      int `json:"edges_removed"`
	EdgesModified     int `json:"edges_modified"`
	BreakingChanges   int `json:"breaking_changes"`
	RiskChanges       int `json:"risk_changes"`
}

type Report struct {
	SchemaVersion string           `json:"schema_version"`
	FromSnapshot  string           `json:"from_snapshot"`
	ToSnapshot    string           `json:"to_snapshot"`
	Summary       Summary          `json:"summary"`
	Artifacts     []ArtifactChange `json:"artifacts"`
	Nodes         []NodeChange     `json:"nodes"`
	Edges         []EdgeChange     `json:"edges"`
}

func Compare(before, after rkcmodel.Bundle) Report {
	report := Report{SchemaVersion: "1", FromSnapshot: before.Snapshot.ID, ToSnapshot: after.Snapshot.ID}
	report.Artifacts = compareArtifacts(before.Artifacts, after.Artifacts)
	report.Nodes = compareNodes(before.Nodes, after.Nodes)
	report.Edges = compareEdges(before.Edges, after.Edges)
	for _, change := range report.Artifacts {
		switch change.Kind {
		case "added":
			report.Summary.ArtifactsAdded++
		case "removed":
			report.Summary.ArtifactsRemoved++
		default:
			report.Summary.ArtifactsModified++
		}
	}
	for _, change := range report.Nodes {
		switch change.Kind {
		case "added":
			report.Summary.NodesAdded++
		case "removed":
			report.Summary.NodesRemoved++
		default:
			report.Summary.NodesModified++
		}
		switch change.Severity {
		case SeverityBreaking:
			report.Summary.BreakingChanges++
		case SeverityRisk:
			report.Summary.RiskChanges++
		}
	}
	for _, change := range report.Edges {
		switch change.Kind {
		case "added":
			report.Summary.EdgesAdded++
		case "removed":
			report.Summary.EdgesRemoved++
		default:
			report.Summary.EdgesModified++
		}
	}
	return report
}

func compareArtifacts(before, after []rkcmodel.Artifact) []ArtifactChange {
	beforeByPath := map[string]rkcmodel.Artifact{}
	afterByPath := map[string]rkcmodel.Artifact{}
	for _, artifact := range before {
		beforeByPath[artifact.Path] = artifact
	}
	for _, artifact := range after {
		afterByPath[artifact.Path] = artifact
	}
	keys := unionKeys(beforeByPath, afterByPath)
	var changes []ArtifactChange
	for _, key := range keys {
		left, leftOK := beforeByPath[key]
		right, rightOK := afterByPath[key]
		switch {
		case !leftOK:
			value := right
			changes = append(changes, ArtifactChange{Kind: "added", After: &value})
		case !rightOK:
			value := left
			changes = append(changes, ArtifactChange{Kind: "removed", Before: &value})
		default:
			fields := artifactFields(left, right)
			if len(fields) > 0 {
				l, r := left, right
				changes = append(changes, ArtifactChange{Kind: "modified", Before: &l, After: &r, Fields: fields})
			}
		}
	}
	return changes
}

func compareNodes(before, after []rkcmodel.Node) []NodeChange {
	beforeByKey := map[string]rkcmodel.Node{}
	afterByKey := map[string]rkcmodel.Node{}
	for _, node := range before {
		beforeByKey[nodeKey(node)] = node
	}
	for _, node := range after {
		afterByKey[nodeKey(node)] = node
	}
	keys := unionKeys(beforeByKey, afterByKey)
	var changes []NodeChange
	for _, key := range keys {
		left, leftOK := beforeByKey[key]
		right, rightOK := afterByKey[key]
		switch {
		case !leftOK:
			value := right
			changes = append(changes, NodeChange{Kind: "added", Severity: SeverityInfo, LogicalKey: key, After: &value})
		case !rightOK:
			value := left
			severity, reasons := classifyNodeRemoval(left)
			changes = append(changes, NodeChange{Kind: "removed", Severity: severity, LogicalKey: key, Before: &value, Reasons: reasons})
		default:
			fields := nodeFields(left, right)
			if len(fields) > 0 {
				severity, reasons := classifyNodeModification(left, right, fields)
				l, r := left, right
				changes = append(changes, NodeChange{Kind: "modified", Severity: severity, LogicalKey: key, Before: &l, After: &r, Fields: fields, Reasons: reasons})
			}
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Severity == changes[j].Severity {
			return changes[i].LogicalKey < changes[j].LogicalKey
		}
		return severityRank(changes[i].Severity) > severityRank(changes[j].Severity)
	})
	return changes
}

func compareEdges(before, after []rkcmodel.Edge) []EdgeChange {
	beforeByKey := map[string]rkcmodel.Edge{}
	afterByKey := map[string]rkcmodel.Edge{}
	for _, edge := range before {
		beforeByKey[edgeKey(edge)] = edge
	}
	for _, edge := range after {
		afterByKey[edgeKey(edge)] = edge
	}
	keys := unionKeys(beforeByKey, afterByKey)
	var changes []EdgeChange
	for _, key := range keys {
		left, leftOK := beforeByKey[key]
		right, rightOK := afterByKey[key]
		switch {
		case !leftOK:
			value := right
			changes = append(changes, EdgeChange{Kind: "added", After: &value})
		case !rightOK:
			value := left
			changes = append(changes, EdgeChange{Kind: "removed", Before: &value})
		default:
			fields := edgeFields(left, right)
			if len(fields) > 0 {
				l, r := left, right
				changes = append(changes, EdgeChange{Kind: "modified", Before: &l, After: &r, Fields: fields})
			}
		}
	}
	return changes
}

func nodeKey(node rkcmodel.Node) string {
	if node.LogicalID != "" {
		return node.LogicalID
	}
	if node.QualifiedName != "" {
		return node.Language + "\x00" + node.Kind + "\x00" + node.QualifiedName
	}
	return node.ID
}
func edgeKey(edge rkcmodel.Edge) string { return edge.Kind + "\x00" + edge.From + "\x00" + edge.To }

func artifactFields(left, right rkcmodel.Artifact) []string {
	var fields []string
	if left.SHA256 != right.SHA256 {
		fields = append(fields, "content")
	}
	if left.Status != right.Status {
		fields = append(fields, "status")
	}
	if left.Language != right.Language {
		fields = append(fields, "language")
	}
	if left.MediaType != right.MediaType {
		fields = append(fields, "media_type")
	}
	if left.Generated != right.Generated {
		fields = append(fields, "generated")
	}
	if left.Vendored != right.Vendored {
		fields = append(fields, "vendored")
	}
	return fields
}
func nodeFields(left, right rkcmodel.Node) []string {
	var fields []string
	if left.Name != right.Name {
		fields = append(fields, "name")
	}
	if left.QualifiedName != right.QualifiedName {
		fields = append(fields, "qualified_name")
	}
	if left.Signature != right.Signature {
		fields = append(fields, "signature")
	}
	if left.Visibility != right.Visibility {
		fields = append(fields, "visibility")
	}
	if left.PublicSurface != right.PublicSurface {
		fields = append(fields, "public_surface")
	}
	if left.ArtifactID != right.ArtifactID {
		fields = append(fields, "artifact")
	}
	if left.SemanticHash != right.SemanticHash {
		fields = append(fields, "semantic_hash")
	}
	return fields
}
func edgeFields(left, right rkcmodel.Edge) []string {
	var fields []string
	if rkcmodel.NormalizeResolution(left.Resolution) != rkcmodel.NormalizeResolution(right.Resolution) {
		fields = append(fields, "resolution")
	}
	if left.Confidence != right.Confidence {
		fields = append(fields, "confidence")
	}
	if left.Producer != right.Producer {
		fields = append(fields, "producer")
	}
	return fields
}

func classifyNodeRemoval(node rkcmodel.Node) (Severity, []string) {
	if isPublic(node) {
		return SeverityBreaking, []string{"public symbol removed"}
	}
	return SeverityInfo, nil
}
func classifyNodeModification(before, after rkcmodel.Node, fields []string) (Severity, []string) {
	fieldSet := stringSet(fields)
	var reasons []string
	severity := SeverityInfo
	if isPublic(before) && fieldSet["signature"] {
		severity = SeverityBreaking
		reasons = append(reasons, "public signature changed")
	}
	if isPublic(before) && !isPublic(after) {
		severity = SeverityBreaking
		reasons = append(reasons, "public symbol became non-public")
	}
	if fieldSet["qualified_name"] && severity != SeverityBreaking {
		severity = SeverityRisk
		reasons = append(reasons, "logical name changed")
	}
	if fieldSet["semantic_hash"] && severity == SeverityInfo {
		severity = SeverityRisk
		reasons = append(reasons, "implementation semantics changed")
	}
	return severity, reasons
}
func isPublic(node rkcmodel.Node) bool {
	return node.PublicSurface || node.Visibility == "public" || node.Visibility == "exported"
}
func severityRank(value Severity) int {
	switch value {
	case SeverityBreaking:
		return 3
	case SeverityRisk:
		return 2
	default:
		return 1
	}
}
func stringSet(values []string) map[string]bool {
	output := map[string]bool{}
	for _, value := range values {
		output[value] = true
	}
	return output
}

func unionKeys[V any](left, right map[string]V) []string {
	seen := map[string]struct{}{}
	for key := range left {
		seen[key] = struct{}{}
	}
	for key := range right {
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
