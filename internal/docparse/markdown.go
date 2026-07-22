// Package docparse converts repository documentation into graph entities and
// structured document records without involving a language model.
package docparse

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.markdown"
	PluginVersion = "0.2.0"
)

var (
	atxHeading = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	link       = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)
)

type Options struct {
	Root       string
	SnapshotID string
	Files      []pluginapi.FileRef
	Artifacts  map[string]string // repository-relative path -> artifact ID
}

type section struct {
	level     int
	heading   string
	startLine int
	endLine   int
	lines     []string
	anchor    string
}

// Extract parses Markdown headings, sections, and local links. It intentionally
// avoids guessing that every inline code span names a symbol. Such guesses are
// wonderfully prolific and mostly wrong.
func Extract(options Options) (rkcmodel.Fragment, error) {
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		parsed, err := parseFile(options.Root, file)
		if err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, rkcmodel.Diagnostic{
				ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, err.Error()), Severity: "error",
				Code: "RKC-DOC-1001", Message: err.Error(), Stage: "document_parse", Plugin: PluginID,
				Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path},
			})
			continue
		}
		documentNodeID := rkcmodel.StableID("node", "document", file.Path)
		documentEvidenceID := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, "document")
		fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
			ID: documentEvidenceID, Kind: "documentation_asserted", Method: "markdown.document", Confidence: 1,
			Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: 1, EndLine: parsed.lineCount},
			Tool:   PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256,
		})
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
			ID: documentNodeID, LogicalID: rkcmodel.StableID("logical", "document", file.Path), Kind: "document",
			Name: parsed.title, QualifiedName: file.Path, Language: "markdown", Visibility: "repository",
			ArtifactID: file.ArtifactID, Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: 1, EndLine: parsed.lineCount},
			EvidenceIDs: []string{documentEvidenceID}, Attributes: map[string]any{"heading_count": len(parsed.sections)},
		})
		fragment.Edges = append(fragment.Edges, rkcmodel.Edge{
			ID: rkcmodel.StableID("edge", "derived_from", documentNodeID, file.ArtifactID), Kind: "derived_from",
			From: documentNodeID, To: file.ArtifactID, Resolution: "declared", Confidence: 1, Producer: PluginID,
			EvidenceIDs: []string{documentEvidenceID},
		})

		document := rkcmodel.Document{
			ID: rkcmodel.StableID("document", "markdown", file.Path), LogicalID: rkcmodel.StableID("logical-document", file.Path),
			Kind: "source_document", Title: parsed.title, Path: file.Path, SubjectIDs: []string{documentNodeID},
			Generator: PluginID, GeneratorVersion: PluginVersion, Status: "validated",
			Attributes: map[string]any{"artifact_id": file.ArtifactID, "source_sha256": file.SHA256},
		}
		parentByLevel := map[int]string{}
		for ordinal, item := range parsed.sections {
			sectionNodeID := rkcmodel.StableID("node", "document_section", file.Path, item.anchor)
			evidenceID := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, item.anchor)
			parentID := documentNodeID
			for level := item.level - 1; level >= 1; level-- {
				if candidate := parentByLevel[level]; candidate != "" {
					parentID = candidate
					break
				}
			}
			for level := item.level; level <= 6; level++ {
				delete(parentByLevel, level)
			}
			parentByLevel[item.level] = sectionNodeID
			fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "documentation_asserted", Method: "markdown.heading", Confidence: 1,
				Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: item.startLine, EndLine: item.endLine, Anchor: item.anchor},
				Tool:   PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: item.heading,
			})
			fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
				ID: sectionNodeID, LogicalID: rkcmodel.StableID("logical", "document_section", file.Path, item.anchor),
				Kind: "document_section", Name: item.heading, QualifiedName: file.Path + "#" + item.anchor,
				Language: "markdown", Visibility: "repository", ArtifactID: file.ArtifactID,
				Source:      &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: item.startLine, EndLine: item.endLine, Anchor: item.anchor},
				EvidenceIDs: []string{evidenceID}, Attributes: map[string]any{"level": item.level, "ordinal": ordinal},
			})
			fragment.Edges = append(fragment.Edges, rkcmodel.Edge{
				ID: rkcmodel.StableID("edge", "contains", parentID, sectionNodeID), Kind: "contains", From: parentID, To: sectionNodeID,
				Resolution: "declared", Confidence: 1, Producer: PluginID, EvidenceIDs: []string{evidenceID},
			})
			markdown := strings.TrimRight(strings.Join(item.lines, "\n"), "\n")
			document.Sections = append(document.Sections, rkcmodel.DocumentSection{
				ID: sectionNodeID, ParentID: parentID, Ordinal: ordinal, Heading: item.heading,
				Markdown: markdown, PlainText: stripMarkdown(markdown), EvidenceIDs: []string{evidenceID},
				Attributes: map[string]any{"level": item.level, "anchor": item.anchor, "start_line": item.startLine, "end_line": item.endLine},
			})
			extractLinks(&fragment, options, file, sectionNodeID, item, evidenceID)
		}
		fragment.Documents = append(fragment.Documents, document)
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}

type parsedDocument struct {
	title     string
	lineCount int
	sections  []section
}

func parseFile(root string, file pluginapi.FileRef) (parsedDocument, error) {
	path := filepath.Join(root, filepath.FromSlash(file.Path))
	input, err := os.Open(path)
	if err != nil {
		return parsedDocument{}, fmt.Errorf("read Markdown %s: %w", file.Path, err)
	}
	defer input.Close()

	var lines []string
	scanner := bufio.NewScanner(input)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 8*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return parsedDocument{}, fmt.Errorf("scan Markdown %s: %w", file.Path, err)
	}

	result := parsedDocument{title: strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path)), lineCount: len(lines)}
	var current *section
	inFence := false
	anchors := map[string]int{}
	flush := func(end int) {
		if current == nil {
			return
		}
		current.endLine = end
		result.sections = append(result.sections, *current)
		current = nil
	}
	for index := 0; index < len(lines); index++ {
		lineNumber := index + 1
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if match := atxHeading.FindStringSubmatch(line); match != nil {
				flush(lineNumber - 1)
				heading := strings.TrimSpace(match[2])
				anchor := uniqueAnchor(slugify(heading), anchors)
				current = &section{level: len(match[1]), heading: heading, startLine: lineNumber, lines: []string{line}, anchor: anchor}
				if len(result.sections) == 0 && current.level == 1 {
					result.title = heading
				}
				continue
			}
			if index+1 < len(lines) && strings.TrimSpace(line) != "" {
				underline := strings.TrimSpace(lines[index+1])
				if isSetext(underline) {
					flush(lineNumber - 1)
					level := 2
					if strings.HasPrefix(underline, "=") {
						level = 1
					}
					heading := strings.TrimSpace(line)
					anchor := uniqueAnchor(slugify(heading), anchors)
					current = &section{level: level, heading: heading, startLine: lineNumber, lines: []string{line, lines[index+1]}, anchor: anchor}
					if len(result.sections) == 0 && level == 1 {
						result.title = heading
					}
					index++
					continue
				}
			}
		}
		if current == nil {
			current = &section{level: 1, heading: result.title, startLine: 1, anchor: "document"}
		}
		current.lines = append(current.lines, line)
	}
	flush(len(lines))
	return result, nil
}

func extractLinks(fragment *rkcmodel.Fragment, options Options, file pluginapi.FileRef, from string, item section, sectionEvidence string) {
	base := filepath.ToSlash(filepath.Dir(file.Path))
	seen := map[string]struct{}{}
	for lineOffset, text := range item.lines {
		for _, match := range link.FindAllStringSubmatch(text, -1) {
			target := strings.TrimSpace(strings.Fields(match[1])[0])
			target = strings.Trim(target, "<>")
			if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			pathPart := strings.SplitN(target, "#", 2)[0]
			pathPart = strings.SplitN(pathPart, "?", 2)[0]
			resolved := filepath.ToSlash(filepath.Clean(filepath.Join(base, filepath.FromSlash(pathPart))))
			resolved = strings.TrimPrefix(resolved, "./")
			artifactID := options.Artifacts[resolved]
			if artifactID == "" {
				continue
			}
			key := from + "\x00" + artifactID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			lineNumber := item.startLine + lineOffset
			evidenceID := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, "link", fmt.Sprint(lineNumber), target)
			fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "documentation_asserted", Method: "markdown.link", Confidence: 1,
				Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: lineNumber, EndLine: lineNumber},
				Tool:   PluginID, ToolVersion: PluginVersion, Detail: target,
			})
			fragment.Edges = append(fragment.Edges, rkcmodel.Edge{
				ID: rkcmodel.StableID("edge", "references", from, artifactID, target), Kind: "references", From: from, To: artifactID,
				Resolution: "documentation_asserted", Confidence: 1, Producer: PluginID,
				EvidenceIDs: []string{sectionEvidence, evidenceID}, Attributes: map[string]any{"target": target},
			})
		}
	}
}

func isSetext(value string) bool {
	if len(value) < 3 {
		return false
	}
	for _, runeValue := range value {
		if runeValue != '=' && runeValue != '-' {
			return false
		}
	}
	return true
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, runeValue := range value {
		switch {
		case runeValue >= 'a' && runeValue <= 'z', runeValue >= '0' && runeValue <= '9':
			builder.WriteRune(runeValue)
			lastDash = false
		case !lastDash:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func uniqueAnchor(base string, anchors map[string]int) string {
	if base == "" {
		base = "section"
	}
	anchors[base]++
	if anchors[base] == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, anchors[base])
}

func stripMarkdown(value string) string {
	value = strings.ReplaceAll(value, "`", "")
	value = strings.ReplaceAll(value, "*", "")
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "#", "")
	return strings.TrimSpace(value)
}
