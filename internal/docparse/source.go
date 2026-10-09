package docparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	// SourcePluginID identifies the deterministic messy-data document producer.
	SourcePluginID = "rkc.source-documents"
	// SourcePluginVersion pins chunking, record interpretation, and redaction.
	SourcePluginVersion = "0.1.0"
	// MaximumSourceFileBytes bounds one admitted input independently of scan policy.
	MaximumSourceFileBytes = 8 * 1024 * 1024
	// MaximumSourceTotalBytes bounds original input bytes per extraction pass.
	MaximumSourceTotalBytes = 64 * 1024 * 1024
	// MaximumSourceSectionBytes bounds a section's plain-text projection.
	MaximumSourceSectionBytes = 16 * 1024
	// MaximumSourceSections bounds graph expansion per source document.
	MaximumSourceSections = 4096
	// MaximumSourceDocuments and the cumulative section/body limits bound the
	// graph and duplicated Markdown/plain-text representation for one pass.
	MaximumSourceDocuments            = 4096
	MaximumSourceTotalSections        = 16384
	MaximumSourceProjectionBytes      = 8 * 1024 * 1024
	MaximumSourceTotalProjectionBytes = 32 * 1024 * 1024
)

// IsSourceCandidate admits plain prose and exported records, never unknown code.
// ReStructuredText is preserved as text; its directives are never executed.
func IsSourceCandidate(file pluginapi.FileRef) bool {
	switch file.Language {
	case "text", "rst", "log", "csv", "tsv", "jsonl":
		return true
	default:
		return false
	}
}

type sourcePart struct {
	start, end int
	body       string
	method     string
	record     int
	valid      bool
	truncated  bool
}

type sourceReceipt struct {
	parts           []sourcePart
	records         int
	invalidRecords  int
	complete        bool
	parseError      bool
	projectionBytes int
}

// ExtractSources creates cited documents from bounded UTF-8 text and tabular or
// line-delimited exports. Input remains producer-unverified source assertions;
// parsing a log never establishes that its reported events actually occurred.
// Unsupported sizes/encodings retain inventory accounting with diagnostics.
func ExtractSources(ctx context.Context, options Options) (rkcmodel.Fragment, error) {
	if ctx == nil {
		return rkcmodel.Fragment{}, errors.New("source document context is required")
	}
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	total := 0
	sectionTotal, projectionTotal := 0, 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return rkcmodel.Fragment{}, err
		}
		if !IsSourceCandidate(file) {
			continue
		}
		if len(fragment.Documents) >= MaximumSourceDocuments || sectionTotal >= MaximumSourceTotalSections || projectionTotal >= MaximumSourceTotalProjectionBytes {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1001", "Source document exceeds the aggregate document, section, or projection budget; inventory and bounded artifact search remain available."))
			continue
		}
		if file.SizeBytes > MaximumSourceFileBytes || file.SizeBytes > int64(MaximumSourceTotalBytes-total) {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1001", "Source document exceeds the file or aggregate extraction byte budget; inventory and bounded artifact search remain available."))
			continue
		}
		data, err := readSource(ctx, file, options.Root)
		if err != nil {
			if ctx.Err() != nil {
				return rkcmodel.Fragment{}, ctx.Err()
			}
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1002", err.Error()))
			continue
		}
		if len(data) > MaximumSourceTotalBytes-total {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1001", "Source document exceeds the aggregate extraction byte budget; inventory and bounded artifact search remain available."))
			continue
		}
		total += len(data)
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1003", "Source documents require UTF-8 without NUL bytes; lossy decoding is not performed."))
			continue
		}
		receipt := parseSource(ctx, data, file.Language)
		if err := ctx.Err(); err != nil {
			return rkcmodel.Fragment{}, err
		}
		retained := 0
		for _, part := range receipt.parts {
			if sectionTotal >= MaximumSourceTotalSections || projectionTotal+len(part.body) > MaximumSourceTotalProjectionBytes {
				receipt.complete = false
				break
			}
			sectionTotal++
			projectionTotal += len(part.body)
			retained++
		}
		receipt.parts = receipt.parts[:retained]
		if !receipt.complete {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1004", "Source document reached the section or projection byte budget; section receipts identify the retained source ranges."))
		}
		if receipt.invalidRecords > 0 || receipt.parseError {
			fragment.Diagnostics = append(fragment.Diagnostics, sourceDiagnostic(file, "RKC-DATA-1005", "Export contains malformed records or inconsistent column counts; affected content is retained as unverified text, without inferred field semantics."))
		}
		if err := appendSourceDocument(ctx, &fragment, file, data, receipt); err != nil {
			return rkcmodel.Fragment{}, err
		}
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}

func readSource(ctx context.Context, file pluginapi.FileRef, root string) ([]byte, error) {
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		return nil, fmt.Errorf("read source document: %w", err)
	}
	defer input.Close()
	data, err := io.ReadAll(io.LimitReader(sourceContextReader{ctx: ctx, reader: input}, MaximumSourceFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source document bytes: %w", err)
	}
	if len(data) > MaximumSourceFileBytes {
		return nil, errors.New("source document exceeds the 8 MiB extraction limit")
	}
	if file.SHA256 != "" {
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return nil, errors.New("source document bytes no longer match the inventoried SHA-256")
		}
	}
	return data, nil
}

type sourceContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader sourceContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > 64*1024 {
		buffer = buffer[:64*1024]
	}
	return reader.reader.Read(buffer)
}

func parseSource(ctx context.Context, data []byte, language string) sourceReceipt {
	redacted, err := RedactSourceContext(ctx, data, language)
	receipt := sourceReceipt{complete: true}
	if err != nil {
		return receipt
	}
	switch language {
	case "jsonl":
		start := 0
		for start < len(data) && ctx.Err() == nil {
			end := nextLineEnd(data, start)
			line := bytes.TrimSpace(data[start:end])
			if len(line) > 0 {
				receipt.records++
				valid := json.Valid(line)
				if !valid {
					receipt.invalidRecords++
				}
				addSourcePart(&receipt, sourcePart{start: start, end: end, body: string(redacted[start:end]), method: "source.jsonl_record", record: receipt.records, valid: valid})
			}
			start = end
		}
	case "csv", "tsv":
		parseSourceCSV(ctx, data, redacted, language, &receipt)
	default:
		for start := 0; start < len(data) && ctx.Err() == nil; {
			end := sourceChunkEnd(data, start)
			addSourcePart(&receipt, sourcePart{start: start, end: end, body: string(redacted[start:end]), method: "source.text_chunk", valid: true})
			start = end
		}
	}
	return receipt
}

func parseSourceCSV(ctx context.Context, data, redacted []byte, language string, receipt *sourceReceipt) {
	reader := csv.NewReader(sourceContextReader{ctx: ctx, reader: bytes.NewReader(data)})
	reader.FieldsPerRecord = -1
	if language == "tsv" {
		reader.Comma = '\t'
	}
	var headers []string
	start := 0
	for ctx.Err() == nil {
		fields, err := reader.Read()
		end := int(reader.InputOffset())
		if err == io.EOF {
			return
		}
		if err != nil {
			receipt.parseError = true
			// Malformed quoting makes record recovery ambiguous. Retain the entire
			// remaining suffix as bounded text rather than silently losing it.
			for start < len(data) && ctx.Err() == nil {
				end = sourceChunkEnd(data, start)
				addSourcePart(receipt, sourcePart{start: start, end: end, body: string(redacted[start:end]), method: "source.text_chunk", valid: false})
				start = end
			}
			return
		}
		receipt.records++
		valid := headers == nil || len(fields) == len(headers)
		if headers == nil {
			headers = append([]string(nil), fields...)
		}
		if !valid {
			receipt.invalidRecords++
		}
		if len(receipt.parts) >= MaximumSourceSections || receipt.projectionBytes >= MaximumSourceProjectionBytes {
			receipt.complete = false
			start = end
			continue
		}
		// Source display is byte-preserving and redacted. Column names are a
		// searchable projection only; no schema/type/business intent is inferred.
		body := string(redacted[start:end])
		projectionTruncated := false
		if receipt.records > 1 && valid {
			var projection strings.Builder
		projectFields:
			for index, value := range fields {
				if ctx.Err() != nil {
					return
				}
				if secrets.IsSecretName(headers[index]) {
					value = "[REDACTED]"
				}
				for _, piece := range []string{"Column ", strconv.Itoa(index + 1), " (", headers[index], "): ", value, "\n"} {
					if writeSourceProjection(&projection, piece) {
						projectionTruncated = true
						break projectFields
					}
				}
			}
			projected := []byte(projection.String())
			findings, err := secrets.ScanContext(ctx, projected)
			if err != nil {
				return
			}
			body = string(secrets.Redact(projected, findings))
		}
		addSourcePart(receipt, sourcePart{start: start, end: end, body: body, method: "source.csv_record", record: receipt.records, valid: valid, truncated: projectionTruncated})
		start = end
	}
}

func addSourcePart(receipt *sourceReceipt, part sourcePart) {
	if part.truncated {
		receipt.complete = false
	}
	if len(receipt.parts) >= MaximumSourceSections {
		receipt.complete = false
		return
	}
	if len(part.body) > MaximumSourceSectionBytes {
		end := MaximumSourceSectionBytes
		for end > 0 && !utf8.RuneStart(part.body[end]) {
			end--
		}
		part.body = part.body[:end]
		part.truncated = true
		receipt.complete = false
	}
	if strings.TrimSpace(part.body) != "" {
		if receipt.projectionBytes+len(part.body) > MaximumSourceProjectionBytes {
			receipt.complete = false
			return
		}
		receipt.projectionBytes += len(part.body)
		receipt.parts = append(receipt.parts, part)
	}
}

// writeSourceProjection bounds display construction before allocating a
// repeated wide CSV header for each row; true means some bytes were omitted.
func writeSourceProjection(builder *strings.Builder, value string) bool {
	available := MaximumSourceSectionBytes - builder.Len()
	if len(value) <= available {
		builder.WriteString(value)
		return false
	}
	end := available
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	builder.WriteString(value[:end])
	return true
}

func sourceChunkEnd(data []byte, start int) int {
	end := start + MaximumSourceSectionBytes
	if end >= len(data) {
		return len(data)
	}
	if newline := bytes.LastIndexByte(data[start:end], '\n'); newline >= 0 {
		return start + newline + 1
	}
	for end > start && !utf8.RuneStart(data[end]) {
		end--
	}
	return end
}

func nextLineEnd(data []byte, start int) int {
	if offset := bytes.IndexByte(data[start:], '\n'); offset >= 0 {
		return start + offset + 1
	}
	return len(data)
}

func appendSourceDocument(ctx context.Context, fragment *rkcmodel.Fragment, file pluginapi.FileRef, data []byte, receipt sourceReceipt) error {
	lineOffsets := []int{0}
	for index, value := range data {
		if index%65536 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if value == '\n' {
			lineOffsets = append(lineOffsets, index+1)
		}
	}
	nodeID := rkcmodel.StableID("node", SourcePluginID, file.Path)
	evidenceID := rkcmodel.StableID("evidence", SourcePluginID, file.ArtifactID, "document")
	source := sourceRange(file, lineOffsets, 0, len(data))
	fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{ID: evidenceID, Kind: "documentation_asserted", Method: "source.document", Confidence: 1, Source: source, Tool: SourcePluginID, ToolVersion: SourcePluginVersion, InputDigest: file.SHA256})
	fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{ID: nodeID, LogicalID: rkcmodel.StableID("logical", SourcePluginID, file.Path), Kind: "document", Name: filepath.Base(file.Path), QualifiedName: file.Path, Language: file.Language, Visibility: "repository", ArtifactID: file.ArtifactID, Source: source, EvidenceIDs: []string{evidenceID}})
	fragment.Edges = append(fragment.Edges, rkcmodel.Edge{ID: rkcmodel.StableID("edge", "derived_from", nodeID, file.ArtifactID), Kind: "derived_from", From: nodeID, To: file.ArtifactID, Resolution: "declared", Confidence: 1, Producer: SourcePluginID, EvidenceIDs: []string{evidenceID}})
	document := rkcmodel.Document{ID: rkcmodel.StableID("document", SourcePluginID, file.Path), LogicalID: rkcmodel.StableID("logical-document", SourcePluginID, file.Path), Kind: "source_document", Title: filepath.Base(file.Path), Path: file.Path, SubjectIDs: []string{nodeID}, Generator: SourcePluginID, GeneratorVersion: SourcePluginVersion, Status: "validated", Attributes: map[string]any{"artifact_id": file.ArtifactID, "source_sha256": file.SHA256, "source_format": file.Language, "complete": receipt.complete, "record_count": receipt.records, "invalid_record_count": receipt.invalidRecords, "producer_verified": false}}
	for ordinal, part := range receipt.parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		start, end := strconv.Itoa(part.start), strconv.Itoa(part.end)
		sectionID := rkcmodel.StableID("node", SourcePluginID, file.Path, start, end)
		partEvidence := rkcmodel.StableID("evidence", SourcePluginID, file.ArtifactID, start, end)
		partSource := sourceRange(file, lineOffsets, part.start, part.end)
		heading := fmt.Sprintf("Lines %d–%d", partSource.StartLine, partSource.EndLine)
		if part.record > 0 {
			heading = fmt.Sprintf("Record %d · %s", part.record, heading)
		}
		attributes := map[string]any{"start_byte": part.start, "end_byte": part.end, "start_line": partSource.StartLine, "end_line": partSource.EndLine, "record_index": part.record, "record_valid": part.valid, "projection": "secret-redacted-source"}
		attributes["projection_truncated"] = part.truncated
		if part.method == "source.csv_record" && part.record > 1 && part.valid {
			attributes["projection"] = "secret-redacted-column-values"
		}
		fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{ID: partEvidence, Kind: "documentation_asserted", Method: part.method, Confidence: 1, Source: partSource, Tool: SourcePluginID, ToolVersion: SourcePluginVersion, InputDigest: file.SHA256})
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{ID: sectionID, LogicalID: rkcmodel.StableID("logical", SourcePluginID, file.Path, start, end), Kind: "document_section", Name: heading, QualifiedName: file.Path + "#bytes-" + start + "-" + end, Language: file.Language, Visibility: "repository", ArtifactID: file.ArtifactID, Source: partSource, EvidenceIDs: []string{partEvidence}, Attributes: map[string]any{"ordinal": ordinal}})
		fragment.Edges = append(fragment.Edges, rkcmodel.Edge{ID: rkcmodel.StableID("edge", "contains", nodeID, sectionID), Kind: "contains", From: nodeID, To: sectionID, Resolution: "declared", Confidence: 1, Producer: SourcePluginID, EvidenceIDs: []string{partEvidence}})
		document.Sections = append(document.Sections, rkcmodel.DocumentSection{ID: sectionID, ParentID: nodeID, Ordinal: ordinal, Heading: heading, Markdown: sourceFence(part.body), PlainText: part.body, EvidenceIDs: []string{partEvidence}, Attributes: attributes})
	}
	fragment.Documents = append(fragment.Documents, document)
	return nil
}

func sourceRange(file pluginapi.FileRef, lineOffsets []int, start, end int) *rkcmodel.SourceRange {
	startLine := sort.Search(len(lineOffsets), func(index int) bool { return lineOffsets[index] > start })
	lastByte := start
	if end > start {
		lastByte = end - 1
	}
	endLine := sort.Search(len(lineOffsets), func(index int) bool { return lineOffsets[index] > lastByte })
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartByte: int64(start), EndByte: int64(end), StartLine: startLine, EndLine: endLine}
}

func sourceFence(body string) string {
	longest, run := 2, 0
	for _, value := range body {
		if value == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	return fence + "text\n" + strings.TrimRight(body, "\r\n") + "\n" + fence
}

func sourceDiagnostic(file pluginapi.FileRef, code, message string) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", SourcePluginID, file.ArtifactID, code), Severity: "warning", Code: code, Message: message, Stage: "document_parse", Plugin: SourcePluginID, Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path}}
}

// RedactSource preserves byte/line layout while masking known secret patterns
// and sensitive export columns/JSON fields. This is also used for artifact-body
// exports, keeping their privacy contract aligned with document projections.
// Detection is conservative; absence of matches is not proof of secrecy.
func RedactSource(data []byte, language string) []byte {
	redacted, _ := RedactSourceContext(context.Background(), data, language)
	return redacted
}

// RedactSourceContext masks source exports with cancellable scan/record loops.
func RedactSourceContext(ctx context.Context, data []byte, language string) ([]byte, error) {
	findings, err := SourceRedactionsContext(ctx, data, language)
	if err != nil {
		return nil, err
	}
	return secrets.RedactContext(ctx, data, findings)
}

// SourceRedactions returns the secret scanner's findings plus conservative
// sensitive-column spans. Finding fingerprints describe only source positions.
func SourceRedactions(data []byte, language string) []secrets.Finding {
	findings, _ := SourceRedactionsContext(context.Background(), data, language)
	return findings
}

// SourceRedactionsContext checks cancellation between scanner/CSV records.
func SourceRedactionsContext(ctx context.Context, data []byte, language string) ([]secrets.Finding, error) {
	findings, err := secrets.ScanContext(ctx, data)
	if err != nil {
		return nil, err
	}
	if language == "csv" || language == "tsv" {
		columns, err := csvSecretRanges(ctx, data, language)
		if err != nil {
			return nil, err
		}
		findings = append(findings, columns...)
	}
	return findings, ctx.Err()
}

func csvSecretRanges(ctx context.Context, data []byte, language string) ([]secrets.Finding, error) {
	reader := csv.NewReader(sourceContextReader{ctx: ctx, reader: bytes.NewReader(data)})
	reader.FieldsPerRecord = -1
	if language == "tsv" {
		reader.Comma = '\t'
	}
	header, err := reader.Read()
	if err != nil {
		return nil, ctx.Err()
	}
	var sensitive []int
	for index, name := range header {
		if secrets.IsSecretName(name) {
			sensitive = append(sensitive, index)
		}
	}
	lineOffsets := []int{0}
	for index, value := range data {
		if index%65536 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if value == '\n' {
			lineOffsets = append(lineOffsets, index+1)
		}
	}
	var findings []secrets.Finding
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recordStart := int(reader.InputOffset())
		fields, err := reader.Read()
		if err == io.EOF {
			return findings, nil
		}
		if err != nil || len(fields) != len(header) {
			// Field alignment is unknowable from this point. Quarantine its
			// whole display suffix rather than expose a possible secret column.
			if len(sensitive) > 0 {
				return append(findings, csvFinding(lineOffsets, recordStart, len(data), "sensitive_csv_suffix")), ctx.Err()
			}
			return findings, ctx.Err()
		}
		for index, value := range fields {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			kind := "sensitive_csv_column"
			if !secrets.IsSecretName(header[index]) {
				// CSV doubles embedded quotes. A JSON/string assignment inside
				// an ordinary notes cell can be invisible to the raw scanner;
				// inspect its decoded value and mask the entire original cell.
				if !strings.ContainsRune(value, '"') {
					continue
				}
				embedded, err := secrets.ScanContext(ctx, []byte(value))
				if err != nil {
					return nil, err
				}
				if len(embedded) == 0 {
					continue
				}
				kind = "sensitive_csv_embedded_value"
			}
			line, column := reader.FieldPos(index)
			start := lineOffsets[line-1] + column - 1
			end := int(reader.InputOffset())
			if index+1 < len(fields) {
				nextLine, nextColumn := reader.FieldPos(index + 1)
				end = lineOffsets[nextLine-1] + nextColumn - 2
			}
			findings = append(findings, csvFinding(lineOffsets, start, end, kind))
		}
	}
}

func csvFinding(lineOffsets []int, start, end int, kind string) secrets.Finding {
	startLine := sort.Search(len(lineOffsets), func(index int) bool { return lineOffsets[index] > start })
	endLine := sort.Search(len(lineOffsets), func(index int) bool { return lineOffsets[index] > end })
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", kind, start, end)))
	return secrets.Finding{Kind: kind, Confidence: 1, StartByte: start, EndByte: end, StartLine: startLine, EndLine: endLine, StartColumn: start - lineOffsets[startLine-1], EndColumn: end - lineOffsets[endLine-1], Fingerprint: hex.EncodeToString(digest[:8])}
}
