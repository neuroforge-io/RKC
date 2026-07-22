package modelruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
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
	artifactByID := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		artifactByID[artifact.ID] = artifact
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

	excerpts, sourceStats := buildSourceExcerpts(evidence, artifactByID, options)
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

func buildSourceExcerpts(evidence []rkcmodel.Evidence, artifacts map[string]rkcmodel.Artifact, options PacketBuildOptions) ([]SourceExcerpt, sourceBuildStats) {
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
		key := strings.Join([]string{
			item.Source.ArtifactID,
			item.Source.Path,
			fmt.Sprint(item.Source.StartByte),
			fmt.Sprint(item.Source.EndByte),
			fmt.Sprint(item.Source.StartLine),
			fmt.Sprint(item.Source.EndLine),
			fmt.Sprint(item.Source.StartColumn),
			fmt.Sprint(item.Source.EndColumn),
			item.Source.Anchor,
		}, "\x00")
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
		artifact, exists := artifacts[item.Source.ArtifactID]
		if !exists || strings.TrimSpace(item.Source.ArtifactID) == "" {
			stats.missing = append(stats.missing, item.Source.Path+": source artifact is not present in the inventory")
			continue
		}
		text, truncated, err := readSourceRange(root, artifact, *item.Source, maximum)
		if err != nil {
			stats.missing = append(stats.missing, item.Source.Path+": "+err.Error())
			continue
		}
		if options.RedactSecrets {
			findings := secrets.Scan(text)
			stats.redactions += len(findings)
			text = secrets.Redact(text, findings)
		}
		if len(text) > maximum {
			text = text[:maximum]
			truncated = true
		}
		stats.includedBytes += len(text)
		stats.truncated = stats.truncated || truncated
		excerpts = append(excerpts, SourceExcerpt{EvidenceID: item.ID, Source: *item.Source, Text: string(text), Truncated: truncated})
	}
	sort.Strings(stats.missing)
	return excerpts, stats
}

func readSourceRange(root string, artifact rkcmodel.Artifact, source rkcmodel.SourceRange, maximum int) ([]byte, bool, error) {
	if maximum <= 0 {
		return nil, true, nil
	}
	if source.ArtifactID == "" || artifact.ID != source.ArtifactID {
		return nil, false, errors.New("source range does not identify its inventoried artifact")
	}
	if artifact.Path == "" || artifact.Path != source.Path {
		return nil, false, errors.New("source path does not match its inventoried artifact")
	}
	if !artifact.Text {
		return nil, false, errors.New("source artifact was not inventoried as text")
	}
	if artifact.SizeBytes < 0 {
		return nil, false, errors.New("source artifact has an invalid inventoried size")
	}
	expectedDigest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return nil, false, errors.New("source artifact has no valid inventoried SHA-256")
	}

	absolute, pathInfo, err := resolvePacketSource(root, source.Path)
	if err != nil {
		return nil, false, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat opened source: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, false, errors.New("source path identity changed while opening")
	}
	if openedInfo.Size() != artifact.SizeBytes {
		return nil, false, errors.New("source size changed after inventory")
	}

	useBytes := source.StartByte != 0 || source.EndByte != 0
	// Byte offsets are authoritative when both machine offsets and human line
	// coordinates are present. Invalid offsets fail closed instead of falling
	// back to a broader line range.
	useLines := !useBytes && (source.StartLine != 0 || source.EndLine != 0)
	if useBytes {
		if source.StartByte < 0 || source.EndByte <= source.StartByte || source.EndByte > artifact.SizeBytes {
			return nil, false, errors.New("source byte range is outside its inventoried artifact")
		}
	} else if useLines {
		if source.StartLine < 1 {
			return nil, false, errors.New("source start line must be positive")
		}
		if source.EndLine == 0 {
			source.EndLine = source.StartLine
		}
		if source.EndLine < source.StartLine {
			return nil, false, errors.New("source line range is inverted")
		}
	} else {
		return nil, false, errors.New("source range has no bounded byte or line span")
	}

	hash := sha256.New()
	excerpt := make([]byte, 0, maximum)
	buffer := make([]byte, 32*1024)
	var offset int64
	line := 1
	lastByte := byte(0)
	selectedBytes := 0
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			lastByte = chunk[n-1]
			if _, err := hash.Write(chunk); err != nil {
				return nil, false, fmt.Errorf("hash source artifact: %w", err)
			}
			if useBytes {
				chunkStart := offset
				chunkEnd := offset + int64(n)
				start := maxInt64(source.StartByte, chunkStart)
				end := minInt64(source.EndByte, chunkEnd)
				if start < end {
					selection := chunk[start-chunkStart : end-chunkStart]
					selectedBytes += len(selection)
					excerpt = appendBounded(excerpt, selection, maximum)
				}
			} else {
				for _, value := range chunk {
					if line >= source.StartLine && line <= source.EndLine {
						selectedBytes++
						if len(excerpt) < maximum {
							excerpt = append(excerpt, value)
						}
					}
					if value == '\n' {
						line++
					}
				}
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false, fmt.Errorf("read source artifact: %w", readErr)
		}
		if n == 0 {
			return nil, false, io.ErrNoProgress
		}
	}
	if offset != artifact.SizeBytes {
		return nil, false, errors.New("source size changed while verifying content")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), artifact.SHA256) {
		return nil, false, errors.New("source content changed after inventory")
	}
	if useLines {
		lastLine := line
		if offset == 0 {
			lastLine = 0
		} else if lastByte == '\n' {
			lastLine--
		}
		if source.StartLine > lastLine || source.EndLine > lastLine {
			return nil, false, errors.New("source line range is outside its inventoried artifact")
		}
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("restat source artifact: %w", err)
	}
	if !os.SameFile(openedInfo, finalInfo) || finalInfo.Size() != artifact.SizeBytes || !finalInfo.ModTime().Equal(openedInfo.ModTime()) {
		return nil, false, errors.New("source identity changed while verifying content")
	}
	return excerpt, selectedBytes > maximum, nil
}

func resolvePacketSource(root, sourcePath string) (string, os.FileInfo, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", nil, fmt.Errorf("inspect repository root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", nil, errors.New("repository root must be a real directory, not a symlink")
	}
	clean := filepath.Clean(filepath.FromSlash(sourcePath))
	if sourcePath == "" || clean == "." || filepath.IsAbs(clean) || filepath.ToSlash(clean) != sourcePath {
		return "", nil, errors.New("source path is not canonical and repository-relative")
	}
	relative, err := filepath.Rel(root, filepath.Join(root, clean))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", nil, errors.New("source path escapes repository root")
	}
	current := root
	parts := strings.Split(filepath.ToSlash(clean), "/")
	var info os.FileInfo
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", nil, errors.New("source path contains an unsafe component")
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err = os.Lstat(current)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, errors.New("source path contains a symlink")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", nil, errors.New("source path contains a non-directory component")
		}
	}
	if info == nil || !info.Mode().IsRegular() {
		return "", nil, errors.New("source path is not a regular file")
	}
	return current, info, nil
}

func appendBounded(target, source []byte, maximum int) []byte {
	remaining := maximum - len(target)
	if remaining <= 0 {
		return target
	}
	if len(source) > remaining {
		source = source[:remaining]
	}
	return append(target, source...)
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
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
