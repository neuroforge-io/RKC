package modelruntime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/internal/security/secrets"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

// PacketBuildOptions bounds repository material supplied to a model. The graph
// is authoritative; source excerpts are supporting evidence, not an invitation
// for the model to rediscover the repository from scratch.
type PacketBuildOptions struct {
	RepositoryRoot          string
	Task                    Task
	MaximumRelatedNodes     int
	MaximumEdges            int
	MaximumEvidence         int
	MaximumExcerptBytes     int
	MaximumTotalSourceBytes int
	MaximumClaims           int
	MaximumSummaryChars     int
	AllowInference          bool
	RedactSecrets           bool
}

// PacketBuildReport makes truncation and unavailable source explicit. A packet
// that silently omitted half its evidence would merely relocate uncertainty.
type PacketBuildReport struct {
	Packet                  EvidencePacket `json:"packet"`
	CandidateRelatedNodes   int            `json:"candidate_related_nodes"`
	CandidateEdges          int            `json:"candidate_edges"`
	CandidateEvidence       int            `json:"candidate_evidence"`
	IncludedSourceBytes     int            `json:"included_source_bytes"`
	RedactedSecretFindings  int            `json:"redacted_secret_findings"`
	MissingSourceArtifacts  []string       `json:"missing_source_artifacts,omitempty"`
	TruncatedRelatedNodes   bool           `json:"truncated_related_nodes,omitempty"`
	TruncatedEdges          bool           `json:"truncated_edges,omitempty"`
	TruncatedEvidence       bool           `json:"truncated_evidence,omitempty"`
	TruncatedSourceExcerpts bool           `json:"truncated_source_excerpts,omitempty"`
}

// BuildEvidencePacket constructs a deterministic one-hop evidence packet for a
// subject. It deliberately accepts a Bundle rather than a server Dataset so it
// can be reused by the CLI, workers, tests, and future storage adapters.
func BuildEvidencePacket(bundle rkcmodel.Bundle, subjectID string, options PacketBuildOptions) (PacketBuildReport, error) {
	if subjectID == "" {
		return PacketBuildReport{}, errors.New("subject ID is required")
	}
	options = normalizePacketOptions(options)

	nodeByID := make(map[string]rkcmodel.Node, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		nodeByID[node.ID] = node
	}
	evidenceByID := make(map[string]rkcmodel.Evidence, len(bundle.Evidence))
	for _, evidence := range bundle.Evidence {
		evidenceByID[evidence.ID] = evidence
	}
	subject, ok := nodeByID[subjectID]
	if !ok {
		return PacketBuildReport{}, fmt.Errorf("subject node not found: %s", subjectID)
	}

	var candidateEdges []rkcmodel.Edge
	for _, edge := range bundle.Edges {
		if edge.From == subjectID || edge.To == subjectID {
			candidateEdges = append(candidateEdges, edge)
		}
	}
	sort.Slice(candidateEdges, func(i, j int) bool {
		li := candidateEdges[i].Kind + "\x00" + candidateEdges[i].From + "\x00" + candidateEdges[i].To + "\x00" + candidateEdges[i].ID
		lj := candidateEdges[j].Kind + "\x00" + candidateEdges[j].From + "\x00" + candidateEdges[j].To + "\x00" + candidateEdges[j].ID
		return li < lj
	})
	report := PacketBuildReport{CandidateEdges: len(candidateEdges)}
	if len(candidateEdges) > options.MaximumEdges {
		candidateEdges = candidateEdges[:options.MaximumEdges]
		report.TruncatedEdges = true
	}

	relatedSet := map[string]struct{}{}
	for _, edge := range candidateEdges {
		if edge.From != subjectID {
			relatedSet[edge.From] = struct{}{}
		}
		if edge.To != subjectID {
			relatedSet[edge.To] = struct{}{}
		}
	}
	relatedIDs := make([]string, 0, len(relatedSet))
	for id := range relatedSet {
		relatedIDs = append(relatedIDs, id)
	}
	sort.Strings(relatedIDs)
	report.CandidateRelatedNodes = len(relatedIDs)
	if len(relatedIDs) > options.MaximumRelatedNodes {
		relatedIDs = relatedIDs[:options.MaximumRelatedNodes]
		report.TruncatedRelatedNodes = true
	}
	relatedAllowed := map[string]struct{}{}
	relatedNodes := make([]rkcmodel.Node, 0, len(relatedIDs))
	for _, id := range relatedIDs {
		if node, exists := nodeByID[id]; exists {
			relatedNodes = append(relatedNodes, node)
			relatedAllowed[id] = struct{}{}
		}
	}
	// Remove edges whose non-subject endpoint was truncated. This keeps packets
	// internally coherent rather than leaving dangling IDs for the model to adorn.
	filteredEdges := candidateEdges[:0]
	for _, edge := range candidateEdges {
		other := edge.To
		if other == subjectID {
			other = edge.From
		}
		if other == subjectID {
			filteredEdges = append(filteredEdges, edge)
			continue
		}
		if _, allowed := relatedAllowed[other]; allowed {
			filteredEdges = append(filteredEdges, edge)
		}
	}
	candidateEdges = filteredEdges

	evidenceIDs := map[string]struct{}{}
	for _, id := range subject.EvidenceIDs {
		evidenceIDs[id] = struct{}{}
	}
	for _, node := range relatedNodes {
		for _, id := range node.EvidenceIDs {
			evidenceIDs[id] = struct{}{}
		}
	}
	for _, edge := range candidateEdges {
		for _, id := range edge.EvidenceIDs {
			evidenceIDs[id] = struct{}{}
		}
	}
	orderedEvidenceIDs := make([]string, 0, len(evidenceIDs))
	for id := range evidenceIDs {
		orderedEvidenceIDs = append(orderedEvidenceIDs, id)
	}
	sort.Strings(orderedEvidenceIDs)
	report.CandidateEvidence = len(orderedEvidenceIDs)
	if len(orderedEvidenceIDs) > options.MaximumEvidence {
		orderedEvidenceIDs = orderedEvidenceIDs[:options.MaximumEvidence]
		report.TruncatedEvidence = true
	}
	evidence := make([]rkcmodel.Evidence, 0, len(orderedEvidenceIDs))
	for _, id := range orderedEvidenceIDs {
		if item, exists := evidenceByID[id]; exists {
			evidence = append(evidence, item)
		}
	}

	excerpts, sourceStats := buildSourceExcerpts(evidence, options)
	report.IncludedSourceBytes = sourceStats.includedBytes
	report.RedactedSecretFindings = sourceStats.redactions
	report.MissingSourceArtifacts = sourceStats.missing
	report.TruncatedSourceExcerpts = sourceStats.truncated

	packet := EvidencePacket{
		SchemaVersion:          rkcmodel.SchemaVersion,
		SnapshotID:             bundle.Snapshot.ID,
		Task:                   options.Task,
		Subject:                subject,
		RelatedNodes:           relatedNodes,
		Edges:                  append([]rkcmodel.Edge(nil), candidateEdges...),
		Evidence:               evidence,
		SourceExcerpts:         excerpts,
		AllowedClaimCategories: allowedCategories(options.Task),
		Policy: PacketPolicy{
			RequireCitations:         true,
			AllowInference:           options.AllowInference,
			MaximumClaims:            options.MaximumClaims,
			MaximumSummaryCharacters: options.MaximumSummaryChars,
		},
	}
	packet.PacketID = rkcmodel.StableID("model_packet", packet.SnapshotID, string(packet.Task), packet.Subject.ID, strings.Join(orderedEvidenceIDs, ","), fmt.Sprint(report.IncludedSourceBytes))
	report.Packet = packet
	return report, nil
}

func normalizePacketOptions(options PacketBuildOptions) PacketBuildOptions {
	if options.Task == "" {
		options.Task = TaskSymbolSummary
	}
	if options.MaximumRelatedNodes <= 0 {
		options.MaximumRelatedNodes = 24
	}
	if options.MaximumEdges <= 0 {
		options.MaximumEdges = 64
	}
	if options.MaximumEvidence <= 0 {
		options.MaximumEvidence = 64
	}
	if options.MaximumExcerptBytes <= 0 {
		options.MaximumExcerptBytes = 8 * 1024
	}
	if options.MaximumTotalSourceBytes <= 0 {
		options.MaximumTotalSourceBytes = 64 * 1024
	}
	if options.MaximumClaims <= 0 {
		options.MaximumClaims = 12
	}
	if options.MaximumSummaryChars <= 0 {
		options.MaximumSummaryChars = 2000
	}
	// Secure by default. A caller must explicitly set RepositoryRoot and may
	// separately choose to publish raw source elsewhere; model packets do not.
	if !options.RedactSecrets {
		options.RedactSecrets = true
	}
	return options
}

type sourceBuildStats struct {
	includedBytes int
	redactions    int
	missing       []string
	truncated     bool
}

func buildSourceExcerpts(evidence []rkcmodel.Evidence, options PacketBuildOptions) ([]SourceExcerpt, sourceBuildStats) {
	if strings.TrimSpace(options.RepositoryRoot) == "" {
		return nil, sourceBuildStats{}
	}
	root, err := filepath.Abs(options.RepositoryRoot)
	if err != nil {
		return nil, sourceBuildStats{missing: []string{"repository-root:" + err.Error()}}
	}
	var excerpts []SourceExcerpt
	stats := sourceBuildStats{}
	seen := map[string]struct{}{}
	for _, item := range evidence {
		if item.Source == nil || item.Source.Path == "" {
			continue
		}
		key := item.Source.ArtifactID + "\x00" + item.Source.Path + "\x00" + fmt.Sprint(item.Source.StartByte) + "\x00" + fmt.Sprint(item.Source.EndByte)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		remaining := options.MaximumTotalSourceBytes - stats.includedBytes
		if remaining <= 0 {
			stats.truncated = true
			break
		}
		maximum := options.MaximumExcerptBytes
		if maximum > remaining {
			maximum = remaining
		}
		text, truncated, err := readSourceRange(root, *item.Source, maximum)
		if err != nil {
			stats.missing = append(stats.missing, item.Source.Path+": "+err.Error())
			continue
		}
		if options.RedactSecrets {
			findings := secrets.Scan(text)
			stats.redactions += len(findings)
			text = secrets.Redact(text, findings)
		}
		stats.includedBytes += len(text)
		stats.truncated = stats.truncated || truncated
		excerpts = append(excerpts, SourceExcerpt{EvidenceID: item.ID, Source: *item.Source, Text: string(text), Truncated: truncated})
	}
	sort.Strings(stats.missing)
	return excerpts, stats
}

func readSourceRange(root string, source rkcmodel.SourceRange, maximum int) ([]byte, bool, error) {
	if maximum <= 0 {
		return nil, true, nil
	}
	candidate := filepath.Join(root, filepath.FromSlash(source.Path))
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return nil, false, err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, false, errors.New("source path escapes repository root")
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return nil, false, err
	}
	var excerpt []byte
	if source.EndByte > source.StartByte && source.StartByte >= 0 && source.EndByte <= int64(len(data)) {
		excerpt = append([]byte(nil), data[source.StartByte:source.EndByte]...)
	} else if source.StartLine > 0 {
		excerpt = sliceLines(data, source.StartLine, source.EndLine)
	} else {
		excerpt = append([]byte(nil), data...)
	}
	truncated := false
	if len(excerpt) > maximum {
		excerpt = excerpt[:maximum]
		truncated = true
	}
	return excerpt, truncated, nil
}

func sliceLines(data []byte, start, end int) []byte {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if start > len(lines) {
		return nil
	}
	if end > len(lines) {
		end = len(lines)
	}
	return bytes.Join(lines[start-1:end], nil)
}

func allowedCategories(task Task) []string {
	switch task {
	case TaskModuleSummary:
		return []string{"purpose", "responsibility", "dependency", "public_surface", "risk", "limitation"}
	case TaskExecutionExplanation:
		return []string{"entry", "step", "branch", "side_effect", "exit", "limitation"}
	case TaskGapAnalysis:
		return []string{"missing_documentation", "missing_test", "unresolved_relationship", "conflict", "limitation"}
	default:
		return []string{"purpose", "argument", "return", "side_effect", "relationship", "error", "limitation"}
	}
}
