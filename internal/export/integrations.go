package export

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/internal/model"
)

func writeIntegrations(bundle model.Bundle, opts Options) error {
	dir := filepath.Join(opts.Output, "integrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	writers := []struct {
		name string
		fn   func(string, model.Bundle) error
	}{
		{"diagnostics.sarif.json", writeSARIF},
		{"graph.graphml", writeGraphML},
		{"architecture.mmd", writeMermaid},
		{"symbols.csv", writeSymbolCSV},
		{"edges.csv", writeEdgeCSV},
	}
	for _, writer := range writers {
		if err := writer.fn(filepath.Join(dir, writer.name), bundle); err != nil {
			return fmt.Errorf("write integration %s: %w", writer.name, err)
		}
	}
	return nil
}

type sarifLog struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema"`
	Runs    []sarifRun `json:"runs"`
}
type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}
type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}
type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules,omitempty"`
}
type sarifRule struct {
	ID               string       `json:"id"`
	Name             string       `json:"name,omitempty"`
	ShortDescription sarifMessage `json:"shortDescription"`
	HelpURI          string       `json:"helpUri,omitempty"`
}
type sarifResult struct {
	RuleID     string          `json:"ruleId"`
	Level      string          `json:"level"`
	Message    sarifMessage    `json:"message"`
	Locations  []sarifLocation `json:"locations,omitempty"`
	Properties map[string]any  `json:"properties,omitempty"`
}
type sarifMessage struct {
	Text string `json:"text"`
}
type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}
type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}
type sarifArtifactLocation struct {
	URI string `json:"uri"`
}
type sarifRegion struct {
	StartLine   int `json:"startLine,omitempty"`
	StartColumn int `json:"startColumn,omitempty"`
	EndLine     int `json:"endLine,omitempty"`
	EndColumn   int `json:"endColumn,omitempty"`
}

func writeSARIF(path string, bundle model.Bundle) error {
	ruleByCode := map[string]sarifRule{}
	results := make([]sarifResult, 0, len(bundle.Diagnostics))
	for _, diagnostic := range bundle.Diagnostics {
		code := diagnostic.Code
		if code == "" {
			code = "RKC-UNSPECIFIED"
		}
		if _, exists := ruleByCode[code]; !exists {
			ruleByCode[code] = sarifRule{
				ID: code, Name: strings.ReplaceAll(strings.ToLower(code), "-", "_"),
				ShortDescription: sarifMessage{Text: firstSentence(diagnostic.Message)}, HelpURI: diagnostic.HelpURI,
			}
		}
		result := sarifResult{
			RuleID: code, Level: sarifLevel(diagnostic.Severity), Message: sarifMessage{Text: diagnostic.Message},
			Properties: map[string]any{"diagnostic_id": diagnostic.ID, "stage": diagnostic.Stage, "plugin": diagnostic.Plugin},
		}
		if diagnostic.Source != nil && diagnostic.Source.Path != "" {
			region := &sarifRegion{StartLine: diagnostic.Source.StartLine, EndLine: diagnostic.Source.EndLine}
			if diagnostic.Source.StartColumn > 0 {
				region.StartColumn = diagnostic.Source.StartColumn + 1
			}
			if diagnostic.Source.EndColumn > 0 {
				region.EndColumn = diagnostic.Source.EndColumn + 1
			}
			result.Locations = []sarifLocation{{PhysicalLocation: sarifPhysicalLocation{
				ArtifactLocation: sarifArtifactLocation{URI: filepath.ToSlash(diagnostic.Source.Path)}, Region: region,
			}}}
		}
		results = append(results, result)
	}
	rules := make([]sarifRule, 0, len(ruleByCode))
	for _, rule := range ruleByCode {
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	log := sarifLog{Version: "2.1.0", Schema: "https://json.schemastore.org/sarif-2.1.0.json", Runs: []sarifRun{{
		Tool:    sarifTool{Driver: sarifDriver{Name: bundle.Snapshot.Tool.Name, Version: bundle.Snapshot.Tool.Version, Rules: rules}},
		Results: results,
	}}}
	return writeJSON(path, log)
}

func sarifLevel(severity string) string {
	switch strings.ToLower(severity) {
	case "fatal", "error":
		return "error"
	case "warning":
		return "warning"
	default:
		return "note"
	}
}
func firstSentence(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexAny(value, ".\n"); index >= 0 {
		value = strings.TrimSpace(value[:index+1])
	}
	if value == "" {
		return "RKC diagnostic"
	}
	return value
}

type graphML struct {
	XMLName xml.Name     `xml:"graphml"`
	XMLNS   string       `xml:"xmlns,attr"`
	Keys    []graphMLKey `xml:"key"`
	Graph   graphMLGraph `xml:"graph"`
}
type graphMLKey struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}
type graphMLGraph struct {
	ID          string        `xml:"id,attr"`
	EdgeDefault string        `xml:"edgedefault,attr"`
	Nodes       []graphMLNode `xml:"node"`
	Edges       []graphMLEdge `xml:"edge"`
}
type graphMLNode struct {
	ID   string        `xml:"id,attr"`
	Data []graphMLData `xml:"data"`
}
type graphMLEdge struct {
	ID     string        `xml:"id,attr"`
	Source string        `xml:"source,attr"`
	Target string        `xml:"target,attr"`
	Data   []graphMLData `xml:"data"`
}
type graphMLData struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

func writeGraphML(path string, bundle model.Bundle) error {
	nodes := make([]graphMLNode, 0, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		nodes = append(nodes, graphMLNode{ID: node.ID, Data: []graphMLData{
			{Key: "node_kind", Value: node.Kind}, {Key: "node_name", Value: firstNonEmpty(node.QualifiedName, node.Name)},
			{Key: "node_language", Value: node.Language}, {Key: "node_visibility", Value: node.Visibility},
		}})
	}
	edges := make([]graphMLEdge, 0, len(bundle.Edges))
	for _, edge := range bundle.Edges {
		edges = append(edges, graphMLEdge{ID: edge.ID, Source: edge.From, Target: edge.To, Data: []graphMLData{
			{Key: "edge_kind", Value: edge.Kind}, {Key: "edge_resolution", Value: edge.Resolution},
			{Key: "edge_confidence", Value: strconv.FormatFloat(edge.Confidence, 'f', -1, 64)},
		}})
	}
	document := graphML{XMLNS: "http://graphml.graphdrawing.org/xmlns", Keys: []graphMLKey{
		{ID: "node_kind", For: "node", AttrName: "kind", AttrType: "string"},
		{ID: "node_name", For: "node", AttrName: "name", AttrType: "string"},
		{ID: "node_language", For: "node", AttrName: "language", AttrType: "string"},
		{ID: "node_visibility", For: "node", AttrName: "visibility", AttrType: "string"},
		{ID: "edge_kind", For: "edge", AttrName: "kind", AttrType: "string"},
		{ID: "edge_resolution", For: "edge", AttrName: "resolution", AttrType: "string"},
		{ID: "edge_confidence", For: "edge", AttrName: "confidence", AttrType: "double"},
	}, Graph: graphMLGraph{ID: bundle.Snapshot.ID, EdgeDefault: "directed", Nodes: nodes, Edges: edges}}
	data, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append([]byte(xml.Header), data...)
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func writeMermaid(path string, bundle model.Bundle) error {
	allowed := map[string]struct{}{
		"repository": {}, "project": {}, "package": {}, "module": {}, "api_service": {}, "api_endpoint": {},
		"build_target": {}, "database_table": {}, "schema": {}, "cli_command": {}, "document": {},
	}
	selected := make(map[string]model.Node)
	for _, node := range bundle.Nodes {
		if _, ok := allowed[node.Kind]; ok && len(selected) < 1000 {
			selected[node.ID] = node
		}
	}
	aliases := map[string]string{}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	for index, id := range ids {
		alias := fmt.Sprintf("N%d", index)
		aliases[id] = alias
		label := firstNonEmpty(selected[id].QualifiedName, selected[id].Name, selected[id].Kind)
		label = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", " ").Replace(label)
		fmt.Fprintf(&b, "  %s[\"%s\\n(%s)\"]\n", alias, label, selected[id].Kind)
	}
	for _, edge := range bundle.Edges {
		from, fromOK := aliases[edge.From]
		to, toOK := aliases[edge.To]
		if !fromOK || !toOK {
			continue
		}
		label := strings.NewReplacer("\"", "'", "\n", " ").Replace(edge.Kind)
		fmt.Fprintf(&b, "  %s -->|%s| %s\n", from, label, to)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeSymbolCSV(path string, bundle model.Bundle) error {
	return writeCSV(path, []string{"id", "logical_id", "kind", "name", "qualified_name", "signature", "language", "visibility", "public_surface", "artifact_id", "source_path", "start_line", "end_line", "evidence_count"}, func(writer *csv.Writer) error {
		for _, node := range bundle.Nodes {
			if !model.IsSymbolKind(node.Kind) {
				continue
			}
			sourcePath, startLine, endLine := "", "", ""
			if node.Source != nil {
				sourcePath = node.Source.Path
				startLine = strconv.Itoa(node.Source.StartLine)
				endLine = strconv.Itoa(node.Source.EndLine)
			}
			if err := writer.Write([]string{node.ID, node.LogicalID, node.Kind, node.Name, node.QualifiedName, node.Signature, node.Language, node.Visibility, strconv.FormatBool(node.PublicSurface), node.ArtifactID, sourcePath, startLine, endLine, strconv.Itoa(len(node.EvidenceIDs))}); err != nil {
				return err
			}
		}
		return nil
	})
}
func writeEdgeCSV(path string, bundle model.Bundle) error {
	return writeCSV(path, []string{"id", "kind", "from", "to", "resolution", "confidence", "producer", "evidence_count"}, func(writer *csv.Writer) error {
		for _, edge := range bundle.Edges {
			if err := writer.Write([]string{edge.ID, edge.Kind, edge.From, edge.To, edge.Resolution, strconv.FormatFloat(edge.Confidence, 'f', -1, 64), edge.Producer, strconv.Itoa(len(edge.EvidenceIDs))}); err != nil {
				return err
			}
		}
		return nil
	})
}
func writeCSV(path string, header []string, writeRows func(*csv.Writer) error) error {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = false
	if err := writer.Write(header); err != nil {
		return err
	}
	if err := writeRows(writer); err != nil {
		return err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}
