// Package search provides a deterministic embedded lexical index. It is not a
// replacement for SQLite FTS5 in the production store; it gives the reference
// implementation ranked search, field filters, and retrieval traces without a
// third-party dependency.
package search

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const IndexVersion = "1"

type Document struct {
	ID            string            `json:"id"`
	ObjectType    string            `json:"object_type"`
	Kind          string            `json:"kind,omitempty"`
	Language      string            `json:"language,omitempty"`
	Title         string            `json:"title"`
	QualifiedName string            `json:"qualified_name,omitempty"`
	Signature     string            `json:"signature,omitempty"`
	Path          string            `json:"path,omitempty"`
	Body          string            `json:"body,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type Posting struct {
	DocumentID string   `json:"document_id"`
	TermCount  int      `json:"term_count"`
	FieldBoost float64  `json:"field_boost"`
	Fields     []string `json:"fields,omitempty"`
}

type Index struct {
	Version        string               `json:"version"`
	Documents      map[string]Document  `json:"documents"`
	Postings       map[string][]Posting `json:"postings"`
	DocumentLength map[string]int       `json:"document_length"`
	AverageLength  float64              `json:"average_length"`
	DocumentCount  int                  `json:"document_count"`
}

type Query struct {
	Text        string
	Kinds       map[string]struct{}
	Languages   map[string]struct{}
	ObjectTypes map[string]struct{}
	PathPrefix  string
	Limit       int
}

type Hit struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
	Reasons  []string `json:"reasons"`
	Terms    []string `json:"terms"`
}

type Response struct {
	Query        string `json:"query"`
	Hits         []Hit  `json:"hits"`
	Truncated    bool   `json:"truncated"`
	Mode         string `json:"mode"`
	IndexVersion string `json:"index_version"`
}

type builderDoc struct {
	document Document
	terms    map[string]termFields
	length   int
}

type termFields struct {
	count  int
	boost  float64
	fields map[string]struct{}
}

func BuildFromBundle(bundle rkcmodel.Bundle) *Index {
	documents := make([]Document, 0, len(bundle.Nodes)+len(bundle.Artifacts)+len(bundle.Documents))
	artifactPaths := make(map[string]string, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		artifactPaths[artifact.ID] = artifact.Path
	}
	for _, node := range bundle.Nodes {
		bodyParts := []string{}
		if node.Attributes != nil {
			for _, key := range []string{"docstring", "summary", "description", "purpose"} {
				if value, ok := node.Attributes[key].(string); ok && value != "" {
					bodyParts = append(bodyParts, value)
				}
			}
		}
		path := ""
		if node.Source != nil {
			path = node.Source.Path
		} else {
			path = artifactPaths[node.ArtifactID]
		}
		documents = append(documents, Document{
			ID: node.ID, ObjectType: "node", Kind: node.Kind, Language: node.Language,
			Title: node.Name, QualifiedName: node.QualifiedName, Signature: node.Signature,
			Path: path, Body: strings.Join(bodyParts, "\n"),
		})
	}
	for _, artifact := range bundle.Artifacts {
		documents = append(documents, Document{
			ID: artifact.ID, ObjectType: "artifact", Kind: artifact.Kind, Language: artifact.Language,
			Title: filepath.Base(artifact.Path), QualifiedName: artifact.Path, Path: artifact.Path,
			Body: artifact.MediaType + " " + artifact.Status,
		})
	}
	for _, document := range bundle.Documents {
		var body strings.Builder
		for _, section := range document.Sections {
			body.WriteString(section.Heading)
			body.WriteByte('\n')
			body.WriteString(section.PlainText)
			body.WriteByte('\n')
		}
		documents = append(documents, Document{
			ID: document.ID, ObjectType: "document", Kind: document.Kind, Title: document.Title,
			QualifiedName: document.Path, Path: document.Path, Body: body.String(),
		})
	}
	return Build(documents)
}

func Build(documents []Document) *Index {
	index := &Index{
		Version: IndexVersion, Documents: map[string]Document{}, Postings: map[string][]Posting{},
		DocumentLength: map[string]int{}, DocumentCount: len(documents),
	}
	var totalLength int
	for _, document := range documents {
		built := buildDocument(document)
		index.Documents[document.ID] = document
		index.DocumentLength[document.ID] = built.length
		totalLength += built.length
		for term, fields := range built.terms {
			fieldNames := make([]string, 0, len(fields.fields))
			for field := range fields.fields {
				fieldNames = append(fieldNames, field)
			}
			sort.Strings(fieldNames)
			index.Postings[term] = append(index.Postings[term], Posting{
				DocumentID: document.ID, TermCount: fields.count, FieldBoost: fields.boost, Fields: fieldNames,
			})
		}
	}
	if len(documents) > 0 {
		index.AverageLength = float64(totalLength) / float64(len(documents))
	}
	for term := range index.Postings {
		sort.Slice(index.Postings[term], func(i, j int) bool { return index.Postings[term][i].DocumentID < index.Postings[term][j].DocumentID })
	}
	return index
}

func buildDocument(document Document) builderDoc {
	built := builderDoc{document: document, terms: map[string]termFields{}}
	fields := []struct {
		name  string
		value string
		boost float64
	}{
		{"id", document.ID, 3.0},
		{"title", document.Title, 8.0},
		{"qualified_name", document.QualifiedName, 7.0},
		{"signature", document.Signature, 6.0},
		{"path", document.Path, 4.0},
		{"kind", document.Kind, 2.0},
		{"language", document.Language, 2.0},
		{"body", document.Body, 1.0},
	}
	for _, field := range fields {
		for _, term := range tokenize(field.value) {
			current := built.terms[term]
			current.count++
			if field.boost > current.boost {
				current.boost = field.boost
			}
			if current.fields == nil {
				current.fields = map[string]struct{}{}
			}
			current.fields[field.name] = struct{}{}
			built.terms[term] = current
			built.length++
		}
	}
	return built
}

func (index *Index) Search(query Query) Response {
	text, parsed := parseQuery(query.Text)
	query = applyParsedFilters(query, parsed)
	terms := tokenize(text)
	if query.Limit <= 0 {
		query.Limit = 50
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}

	type accumulator struct {
		score   float64
		reasons map[string]struct{}
		terms   map[string]struct{}
	}
	accumulators := map[string]*accumulator{}
	for _, term := range unique(terms) {
		postings := index.Postings[term]
		if len(postings) == 0 {
			continue
		}
		idf := math.Log(1 + (float64(index.DocumentCount)-float64(len(postings))+0.5)/(float64(len(postings))+0.5))
		for _, posting := range postings {
			document := index.Documents[posting.DocumentID]
			if !matchesFilters(document, query) {
				continue
			}
			length := float64(index.DocumentLength[posting.DocumentID])
			average := index.AverageLength
			if average <= 0 {
				average = 1
			}
			tf := float64(posting.TermCount)
			const k1 = 1.2
			const b = 0.75
			bm25 := idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*length/average))
			score := bm25 * posting.FieldBoost
			current := accumulators[posting.DocumentID]
			if current == nil {
				current = &accumulator{reasons: map[string]struct{}{}, terms: map[string]struct{}{}}
				accumulators[posting.DocumentID] = current
			}
			current.score += score
			current.terms[term] = struct{}{}
			for _, field := range posting.Fields {
				current.reasons[field+":"+term] = struct{}{}
			}
		}
	}

	// Exact normalized name/path matches receive deterministic bonuses.
	normalizedText := normalize(text)
	for id, current := range accumulators {
		document := index.Documents[id]
		for field, value := range map[string]string{"title": document.Title, "qualified_name": document.QualifiedName, "signature": document.Signature, "path": document.Path, "id": document.ID} {
			normalizedValue := normalize(value)
			if normalizedValue == normalizedText && normalizedText != "" {
				current.score += 100
				current.reasons["exact_"+field] = struct{}{}
			} else if strings.HasPrefix(normalizedValue, normalizedText) && normalizedText != "" {
				current.score += 12
				current.reasons["prefix_"+field] = struct{}{}
			}
		}
	}

	hits := make([]Hit, 0, len(accumulators))
	for id, current := range accumulators {
		reasons := keys(current.reasons)
		matchedTerms := keys(current.terms)
		hits = append(hits, Hit{Document: index.Documents[id], Score: roundScore(current.score), Reasons: reasons, Terms: matchedTerms})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			left := hits[i].Document.QualifiedName + "\x00" + hits[i].Document.ID
			right := hits[j].Document.QualifiedName + "\x00" + hits[j].Document.ID
			return left < right
		}
		return hits[i].Score > hits[j].Score
	})
	truncated := len(hits) > query.Limit
	if truncated {
		hits = hits[:query.Limit]
	}
	return Response{Query: query.Text, Hits: hits, Truncated: truncated, Mode: "embedded-bm25-lexical", IndexVersion: index.Version}
}

// MatchesQuery applies the same explicit and inline kind/language/type/path
// filters as lexical search. Retrieval layers use this when adding graph
// neighbors so graph expansion cannot bypass a caller's requested scope.
func MatchesQuery(document Document, query Query) bool {
	_, parsed := parseQuery(query.Text)
	return matchesFilters(document, applyParsedFilters(query, parsed))
}

func applyParsedFilters(query Query, parsed parsedQuery) Query {
	if query.Kinds == nil {
		query.Kinds = parsed.kinds
	}
	if query.Languages == nil {
		query.Languages = parsed.languages
	}
	if query.ObjectTypes == nil {
		query.ObjectTypes = parsed.objectTypes
	}
	if query.PathPrefix == "" {
		query.PathPrefix = parsed.pathPrefix
	}
	return query
}

func (index *Index) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".search-index-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode search index: %w", err)
	}
	if index.Version != IndexVersion {
		return nil, fmt.Errorf("unsupported search index version %s", index.Version)
	}
	return &index, nil
}

type parsedQuery struct {
	kinds       map[string]struct{}
	languages   map[string]struct{}
	objectTypes map[string]struct{}
	pathPrefix  string
}

func parseQuery(input string) (string, parsedQuery) {
	parsed := parsedQuery{}
	var text []string
	for _, part := range strings.Fields(input) {
		key, value, ok := strings.Cut(part, ":")
		if !ok || value == "" {
			text = append(text, part)
			continue
		}
		switch strings.ToLower(key) {
		case "kind":
			if parsed.kinds == nil {
				parsed.kinds = map[string]struct{}{}
			}
			parsed.kinds[value] = struct{}{}
		case "lang", "language":
			if parsed.languages == nil {
				parsed.languages = map[string]struct{}{}
			}
			parsed.languages[value] = struct{}{}
		case "type":
			if parsed.objectTypes == nil {
				parsed.objectTypes = map[string]struct{}{}
			}
			parsed.objectTypes[value] = struct{}{}
		case "path":
			parsed.pathPrefix = value
		default:
			text = append(text, part)
		}
	}
	return strings.Join(text, " "), parsed
}

func matchesFilters(document Document, query Query) bool {
	if len(query.Kinds) > 0 {
		if _, ok := query.Kinds[document.Kind]; !ok {
			return false
		}
	}
	if len(query.Languages) > 0 {
		if _, ok := query.Languages[document.Language]; !ok {
			return false
		}
	}
	if len(query.ObjectTypes) > 0 {
		if _, ok := query.ObjectTypes[document.ObjectType]; !ok {
			return false
		}
	}
	if query.PathPrefix != "" && !strings.HasPrefix(document.Path, query.PathPrefix) {
		return false
	}
	return true
}

func tokenize(value string) []string {
	value = splitCamel(value)
	var terms []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		term := strings.ToLower(string(current))
		if len(term) > 1 || unicode.IsDigit(current[0]) {
			terms = append(terms, term)
		}
		current = current[:0]
	}
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' {
			current = append(current, char)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func splitCamel(value string) string {
	runes := []rune(value)
	var out []rune
	for i, char := range runes {
		if i > 0 && unicode.IsUpper(char) && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			out = append(out, ' ')
		}
		out = append(out, char)
	}
	return string(out)
}

func normalize(value string) string { return strings.Join(tokenize(value), " ") }
func unique(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}
func keys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
func roundScore(value float64) float64 { return math.Round(value*1000000) / 1000000 }
