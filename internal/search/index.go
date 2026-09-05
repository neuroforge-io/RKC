// Package search provides a deterministic embedded lexical index. It is not a
// replacement for SQLite FTS5 in the production store; it gives the reference
// implementation ranked search, field filters, and retrieval traces without a
// third-party dependency.
package search

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// IndexVersion is the persisted schema version emitted by Build and accepted by
// Load. It does not authenticate index contents.
const IndexVersion = "1"

// RepositoryTextCorpusVersion identifies a derived corpus whose admitted code
// and documentation artifacts contain complete secret-redacted bodies. Large
// structured-data artifacts carry explicit metadata-only receipts so dataset
// volume cannot exhaust the fixed live-server memory envelope.
const RepositoryTextCorpusVersion = "repository-text-v2"

// MaximumResultBodyBytes bounds one returned search document body. The full
// body remains indexed; result serialization and downstream context assembly
// receive an explicit truncation reason instead of unbounded repository text.
const MaximumResultBodyBytes = 64 * 1024

// The search resource envelope is enforced while building and is preflighted
// before a persisted index is decoded by a live server. The serialized-byte
// limit alone is not a memory guarantee because JSON maps and postings amplify
// in memory; all limits below are therefore independent and cumulative.
const (
	MaximumPersistedIndexBytes  int64 = 1536 * 1024 * 1024
	MaximumIndexedTextBytes     int64 = 256 * 1024 * 1024
	MaximumIndexedDocumentBytes       = 8 * 1024 * 1024
	// MaximumStructuredDataBodyBytes bounds full-body indexing for formats
	// commonly used as machine-generated datasets. Their canonical identity,
	// graph facts, parsed schema facts, and NotebookLM source remain available.
	MaximumStructuredDataBodyBytes       = 64 * 1024
	MaximumIndexDocuments                = 500_000
	MaximumDistinctTerms                 = 2_000_000
	MaximumTermDictionaryBytes     int64 = 256 * 1024 * 1024
	MaximumPostings                      = 8_000_000
	MaximumPostingFieldValues            = 16_000_000
	MaximumDocumentMetadataEntries       = 2_000_000
	MaximumTokenOccurrences              = 128_000_000
	MaximumIndexedTermBytes              = 1_024
)

const (
	repositoryTextBodyKind         = "secret-redacted-repository-text"
	repositoryTextMetadataOnlyKind = "bounded-metadata-only"
	structuredDataExclusionReason  = "structured-data-over-65536-bytes"
)

// Document is one searchable node, artifact, or parsed document. Build indexes
// ID, Title, QualifiedName, Signature, Path, Kind, Language, and Body with fixed
// field boosts; Metadata is persisted but is not tokenized or scored.
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

// Posting records a document's total count for one term, the highest boost of
// any field containing it, and the sorted set of matching field names.
type Posting struct {
	DocumentID string   `json:"document_id"`
	TermCount  int      `json:"term_count"`
	FieldBoost float64  `json:"field_boost"`
	Fields     []string `json:"fields,omitempty"`
}

// Index is the deterministic derived lexical index persisted by Save. Its maps
// are exported for serialization and inspection; mutating them can violate
// search invariants, and Load does not repair or fully validate them.
type Index struct {
	Version        string               `json:"version"`
	SnapshotID     string               `json:"snapshot_id,omitempty"`
	CorpusVersion  string               `json:"corpus_version,omitempty"`
	Documents      map[string]Document  `json:"documents"`
	Postings       map[string][]Posting `json:"postings"`
	DocumentLength map[string]int       `json:"document_length"`
	AverageLength  float64              `json:"average_length"`
	DocumentCount  int                  `json:"document_count"`
}

// Query combines lexical text with exact kind, language, object-type, and path
// prefix filters. Inline kind:, lang:/language:, type:, and path: filters fill
// only absent explicit filters. A nonpositive Limit defaults to 50 and values
// above 1000 are capped at 1000.
type Query struct {
	Text        string
	Kinds       map[string]struct{}
	Languages   map[string]struct{}
	ObjectTypes map[string]struct{}
	PathPrefix  string
	Limit       int
}

// Hit records one deterministically ranked document, its score rounded to six
// decimal places, and sorted scoring reasons and matched terms.
type Hit struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
	Reasons  []string `json:"reasons"`
	Terms    []string `json:"terms"`
}

// Response returns the original query text, bounded hits, whether additional
// matching candidates were truncated, and the ranking mode and index version
// used for the result.
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

// BuildFromBundle derives search documents from bundle nodes, artifacts, and
// parsed documents. It does not have repository file bytes, so artifact bodies
// contain metadata only. Exporters that have verified source access should use
// BuildFromBundleWithArtifactBodies instead.
func BuildFromBundle(bundle rkcmodel.Bundle) *Index {
	return buildFromBundle(bundle, nil)
}

// BuildFromBundleWithArtifactBodies derives the same canonical object corpus
// as BuildFromBundle and enriches admitted artifacts selected by
// IndexesRepositoryTextBody with complete, caller-supplied secret-redacted
// bodies. Deliberately bounded large structured-data artifacts receive explicit
// metadata-only receipts. Bodies are keyed by canonical artifact ID; unknown
// entries are ignored. The resulting index records an explicit corpus version
// and snapshot binding for validation at load time.
func BuildFromBundleWithArtifactBodies(bundle rkcmodel.Bundle, secretRedactedBodies map[string]string) *Index {
	if secretRedactedBodies == nil {
		secretRedactedBodies = map[string]string{}
	}
	return buildFromBundle(bundle, secretRedactedBodies)
}

// BuildFromBundleWithArtifactBodiesBounded constructs the complete repository
// corpus while enforcing the live-server resource envelope during indexing.
// Export paths must use this variant so they fail before publishing an index
// that cannot be loaded safely by the same release.
func BuildFromBundleWithArtifactBodiesBounded(bundle rkcmodel.Bundle, secretRedactedBodies map[string]string) (*Index, error) {
	if secretRedactedBodies == nil {
		secretRedactedBodies = map[string]string{}
	}
	index, err := BuildBounded(documentsFromBundle(bundle, secretRedactedBodies))
	if err != nil {
		return nil, err
	}
	index.SnapshotID = bundle.Snapshot.ID
	index.CorpusVersion = RepositoryTextCorpusVersion
	return index, nil
}

// BuildFromBundleBounded constructs the metadata-only corpus under the same
// deterministic resource envelope used for repository-text exports.
func BuildFromBundleBounded(bundle rkcmodel.Bundle) (*Index, error) {
	index, err := BuildBounded(documentsFromBundle(bundle, nil))
	if err != nil {
		return nil, err
	}
	index.SnapshotID = bundle.Snapshot.ID
	return index, nil
}

func buildFromBundle(bundle rkcmodel.Bundle, secretRedactedBodies map[string]string) *Index {
	documents := documentsFromBundle(bundle, secretRedactedBodies)
	index := Build(documents)
	index.SnapshotID = bundle.Snapshot.ID
	if secretRedactedBodies != nil {
		index.CorpusVersion = RepositoryTextCorpusVersion
	}
	return index
}

func documentsFromBundle(bundle rkcmodel.Bundle, secretRedactedBodies map[string]string) []Document {
	documents := make([]Document, 0, safePreallocationCapacity(
		len(bundle.Nodes), len(bundle.Artifacts), len(bundle.Documents),
	))
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
			Path: path, Body: redactSearchText(strings.Join(bodyParts, "\n")),
		})
	}
	for _, artifact := range bundle.Artifacts {
		body := artifact.MediaType + " " + artifact.Status
		var metadata map[string]string
		if sourceBody, ok := secretRedactedBodies[artifact.ID]; ok && IndexesRepositoryTextBody(artifact) {
			body = sourceBody
			metadata = repositoryTextMetadata(artifact, sourceBody)
		} else if secretRedactedBodies != nil && isAdmittedTextArtifact(artifact) && !IndexesRepositoryTextBody(artifact) {
			metadata = repositoryTextMetadataOnly(artifact)
		}
		documents = append(documents, Document{
			ID: artifact.ID, ObjectType: "artifact", Kind: artifact.Kind, Language: artifact.Language,
			Title: filepath.Base(artifact.Path), QualifiedName: artifact.Path, Path: artifact.Path,
			Body: body, Metadata: metadata,
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
			QualifiedName: document.Path, Path: document.Path, Body: redactSearchText(body.String()),
		})
	}
	return documents
}

func redactSearchText(value string) string {
	if value == "" {
		return ""
	}
	data := []byte(value)
	return string(secrets.Redact(data, secrets.Scan(data)))
}

// ValidateBundleObjectIDs rejects ambiguous identity collapse before a trusted
// search projection is built. RKC deliberately represents each artifact as a
// graph node with the same ID; that exact node/artifact pair is one canonical
// object and the richer artifact search document intentionally coalesces it.
// Every other same-ID pair is rejected instead of applying map
// last-writer-wins semantics.
func ValidateBundleObjectIDs(bundle rkcmodel.Bundle) error {
	nodes := make(map[string]rkcmodel.Node, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("search object ID %q is duplicated by nodes", node.ID)
		}
		nodes[node.ID] = node
	}
	artifacts := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		if _, duplicate := artifacts[artifact.ID]; duplicate {
			return fmt.Errorf("search object ID %q is duplicated by artifacts", artifact.ID)
		}
		if node, shared := nodes[artifact.ID]; shared && !isCanonicalArtifactNodeAlias(node, artifact) {
			return fmt.Errorf("search object ID %q is shared by node and artifact but is not a canonical artifact-node alias", artifact.ID)
		}
		artifacts[artifact.ID] = artifact
	}
	documents := make(map[string]struct{}, len(bundle.Documents))
	for _, document := range bundle.Documents {
		if _, duplicate := documents[document.ID]; duplicate {
			return fmt.Errorf("search object ID %q is duplicated by documents", document.ID)
		}
		if _, shared := nodes[document.ID]; shared {
			return fmt.Errorf("search object ID %q is shared by node and document", document.ID)
		}
		if _, shared := artifacts[document.ID]; shared {
			return fmt.Errorf("search object ID %q is shared by artifact and document", document.ID)
		}
		documents[document.ID] = struct{}{}
	}
	return nil
}

func isCanonicalArtifactNodeAlias(node rkcmodel.Node, artifact rkcmodel.Artifact) bool {
	return node.ID == artifact.ID &&
		node.ArtifactID == artifact.ID &&
		node.Kind == artifact.Kind
}

func isAdmittedTextArtifact(artifact rkcmodel.Artifact) bool {
	if !artifact.Text {
		return false
	}
	switch artifact.Status {
	case "text", "parsed", "syntax_parsed", "semantic_parsed":
		return true
	default:
		return false
	}
}

// IndexesRepositoryTextBody reports whether a canonical artifact receives its
// complete secret-redacted body in the in-memory lexical index. RKC indexes
// code, documentation, notebooks, configuration, and ordinary text in full.
// Only large structured dataset formats are metadata-only; those bytes remain
// inventoried, normalized, and exported to NotebookLM packs.
func IndexesRepositoryTextBody(artifact rkcmodel.Artifact) bool {
	if !isAdmittedTextArtifact(artifact) {
		return false
	}
	if artifact.SizeBytes <= MaximumStructuredDataBodyBytes {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(artifact.Language)) {
	case "json", "jsonl", "csv", "tsv", "xml":
		return false
	case "jupyter":
		return true
	case "":
		switch strings.ToLower(filepath.Ext(artifact.Path)) {
		case ".json", ".jsonl", ".ndjson", ".csv", ".tsv", ".xml":
			return false
		}
	}
	return true
}

func repositoryTextMetadata(artifact rkcmodel.Artifact, body string) map[string]string {
	digest := sha256.Sum256([]byte(body))
	return map[string]string{
		"rkc_body_kind":              repositoryTextBodyKind,
		"rkc_body_sha256":            hex.EncodeToString(digest[:]),
		"rkc_secret_redacted":        "true",
		"rkc_source_artifact_sha256": artifact.SHA256,
	}
}

func repositoryTextMetadataOnly(artifact rkcmodel.Artifact) map[string]string {
	return map[string]string{
		"rkc_body_kind":              repositoryTextMetadataOnlyKind,
		"rkc_body_exclusion":         structuredDataExclusionReason,
		"rkc_source_artifact_sha256": artifact.SHA256,
	}
}

// ValidateBundleIndex proves that index objects and postings are a mechanical
// projection of bundle. When requireRepositoryText is true, every admitted
// text artifact must carry either versioned secret-redacted body metadata or
// the exact deterministic metadata-only receipt required by the corpus policy.
func ValidateBundleIndex(index *Index, bundle rkcmodel.Bundle, requireRepositoryText bool) error {
	if index == nil {
		return fmt.Errorf("search index is nil")
	}
	if index.Version != IndexVersion {
		return fmt.Errorf("unsupported search index version %s", index.Version)
	}
	if index.SnapshotID != bundle.Snapshot.ID {
		return fmt.Errorf("search index snapshot does not match the canonical bundle")
	}
	if requireRepositoryText && index.CorpusVersion != RepositoryTextCorpusVersion {
		return fmt.Errorf("search index is missing the repository text corpus")
	}
	if index.CorpusVersion != "" && index.CorpusVersion != RepositoryTextCorpusVersion {
		return fmt.Errorf("unsupported search corpus version %s", index.CorpusVersion)
	}
	if err := ValidateBundleObjectIDs(bundle); err != nil {
		return err
	}

	if index.Documents == nil || index.Postings == nil || index.DocumentLength == nil {
		return fmt.Errorf("search index maps are missing")
	}
	if err := ValidateResourceEnvelope(index); err != nil {
		return err
	}
	expectedDocuments := documentsFromBundle(bundle, nil)
	expectedByID := make(map[string]Document, len(expectedDocuments))
	for _, document := range expectedDocuments {
		expectedByID[document.ID] = document
	}
	if len(index.Documents) != len(expectedByID) {
		return fmt.Errorf("search index document set does not match the canonical bundle")
	}
	artifacts := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		artifacts[artifact.ID] = artifact
	}
	documentIDs := make([]string, 0, len(expectedByID))
	for id := range expectedByID {
		documentIDs = append(documentIDs, id)
	}
	sort.Strings(documentIDs)
	var totalLength int
	expectedPostingCount := 0
	for _, id := range documentIDs {
		expected := expectedByID[id]
		actual, ok := index.Documents[id]
		if !ok {
			return fmt.Errorf("search index is missing canonical document %q", id)
		}
		normalized := actual
		artifact, artifactDocument := artifacts[id]
		artifactDocument = artifactDocument && expected.ObjectType == "artifact"
		enriched := artifactDocument && actual.Metadata["rkc_body_kind"] == repositoryTextBodyKind
		metadataOnly := artifactDocument && actual.Metadata["rkc_body_kind"] == repositoryTextMetadataOnlyKind
		if enriched {
			if !IndexesRepositoryTextBody(artifact) || !reflect.DeepEqual(actual.Metadata, repositoryTextMetadata(artifact, actual.Body)) {
				return fmt.Errorf("search index artifact body metadata is invalid for %q", id)
			}
			normalized.Body = expected.Body
			normalized.Metadata = expected.Metadata
		} else if metadataOnly {
			if IndexesRepositoryTextBody(artifact) || !reflect.DeepEqual(actual.Metadata, repositoryTextMetadataOnly(artifact)) {
				return fmt.Errorf("search index artifact exclusion metadata is invalid for %q", id)
			}
			normalized.Metadata = expected.Metadata
		} else if requireRepositoryText && artifactDocument && isAdmittedTextArtifact(artifact) {
			if IndexesRepositoryTextBody(artifact) {
				return fmt.Errorf("search index is missing repository text for %q", id)
			}
			return fmt.Errorf("search index is missing repository text exclusion receipt for %q", id)
		}
		if !reflect.DeepEqual(normalized, expected) {
			return fmt.Errorf("search index document differs from canonical identity %q", id)
		}
		built := buildDocument(actual)
		if index.DocumentLength[id] != built.length {
			return fmt.Errorf("search index document length is invalid for %q", id)
		}
		totalLength += built.length
		for term, fields := range built.terms {
			postings := index.Postings[term]
			position := sort.Search(len(postings), func(position int) bool {
				return postings[position].DocumentID >= id
			})
			if position >= len(postings) || postings[position].DocumentID != id {
				return fmt.Errorf("search index is missing posting %q for %q", term, id)
			}
			fieldNames := make([]string, 0, len(fields.fields))
			for field := range fields.fields {
				fieldNames = append(fieldNames, field)
			}
			sort.Strings(fieldNames)
			posting := postings[position]
			if posting.TermCount != fields.count || posting.FieldBoost != fields.boost || !reflect.DeepEqual(posting.Fields, fieldNames) {
				return fmt.Errorf("search index posting %q is invalid for %q", term, id)
			}
			expectedPostingCount++
		}
	}
	if index.DocumentCount != len(expectedByID) || len(index.DocumentLength) != len(expectedByID) {
		return fmt.Errorf("search index document accounting does not match its documents")
	}
	wantAverage := 0.0
	if len(expectedByID) > 0 {
		wantAverage = float64(totalLength) / float64(len(expectedByID))
	}
	if index.AverageLength != wantAverage {
		return fmt.Errorf("search index average document length is invalid")
	}
	actualPostingCount := 0
	for term, postings := range index.Postings {
		if term == "" || len(postings) == 0 {
			return fmt.Errorf("search index contains an empty posting list")
		}
		previous := ""
		for _, posting := range postings {
			if _, ok := index.Documents[posting.DocumentID]; !ok || (previous != "" && previous >= posting.DocumentID) {
				return fmt.Errorf("search index posting list %q is unsorted or references an unknown document", term)
			}
			previous = posting.DocumentID
			actualPostingCount++
		}
	}
	if actualPostingCount != expectedPostingCount {
		return fmt.Errorf("search index posting count does not match its documents")
	}
	return nil
}

// ValidateResourceEnvelope verifies the decoded index accounting limits. Live
// loading also performs a streaming preflight before decode; this second check
// makes the invariant explicit for in-memory callers and validated exports.
func ValidateResourceEnvelope(index *Index) error {
	if index == nil {
		return fmt.Errorf("search index is nil")
	}
	if len(index.Documents) > MaximumIndexDocuments {
		return fmt.Errorf("search document count %d exceeds the %d-document limit", len(index.Documents), MaximumIndexDocuments)
	}
	var textBytes int64
	metadataEntries := 0
	for id, document := range index.Documents {
		bytes, err := documentIndexedTextBytes(document)
		if err != nil {
			return err
		}
		if bytes > MaximumIndexedDocumentBytes {
			return fmt.Errorf("search document %q has %d indexed text bytes, above the %d-byte per-document limit", id, bytes, MaximumIndexedDocumentBytes)
		}
		if bytes > MaximumIndexedTextBytes-textBytes {
			return fmt.Errorf("search corpus indexed text exceeds the %d-byte limit", MaximumIndexedTextBytes)
		}
		textBytes += bytes
		if len(document.Metadata) > MaximumDocumentMetadataEntries-metadataEntries {
			return fmt.Errorf("search document metadata exceeds the %d-entry corpus limit", MaximumDocumentMetadataEntries)
		}
		metadataEntries += len(document.Metadata)
	}
	if len(index.Postings) > MaximumDistinctTerms {
		return fmt.Errorf("search term count %d exceeds the %d-term limit", len(index.Postings), MaximumDistinctTerms)
	}
	var dictionaryBytes int64
	postingCount := 0
	postingFieldValues := 0
	for term, postings := range index.Postings {
		if len(term) > MaximumIndexedTermBytes {
			return fmt.Errorf("search term exceeds the %d-byte term limit", MaximumIndexedTermBytes)
		}
		if int64(len(term)) > MaximumTermDictionaryBytes-dictionaryBytes {
			return fmt.Errorf("search term dictionary exceeds the %d-byte limit", MaximumTermDictionaryBytes)
		}
		dictionaryBytes += int64(len(term))
		if len(postings) > MaximumPostings-postingCount {
			return fmt.Errorf("search posting count exceeds the %d-posting limit", MaximumPostings)
		}
		postingCount += len(postings)
		for _, posting := range postings {
			if len(posting.Fields) > MaximumPostingFieldValues-postingFieldValues {
				return fmt.Errorf("search posting fields exceed the %d-value corpus limit", MaximumPostingFieldValues)
			}
			postingFieldValues += len(posting.Fields)
		}
	}
	tokenOccurrences := 0
	for id, length := range index.DocumentLength {
		if length < 0 || length > MaximumTokenOccurrences-tokenOccurrences {
			return fmt.Errorf("search document length for %q exceeds the %d-token corpus limit", id, MaximumTokenOccurrences)
		}
		tokenOccurrences += length
	}
	return nil
}

func safePreallocationCapacity(lengths ...int) int {
	maximumInt := int(^uint(0) >> 1)
	total := 0
	for _, length := range lengths {
		if length < 0 || total > maximumInt-length {
			return 0
		}
		total += length
	}
	return total
}

// Build constructs a deterministic lexical index. When IDs repeat, the last
// document in input order wins, including an empty ID. It sorts document IDs,
// posting lists, and field names, and indexes only the fields documented on
// Document.
func Build(documents []Document) *Index {
	index, _ := buildIndex(documents, false)
	return index
}

// BuildBounded constructs the same deterministic index as Build while failing
// as soon as the shared production resource envelope would be exceeded.
func BuildBounded(documents []Document) (*Index, error) {
	return buildIndex(documents, true)
}

func buildIndex(documents []Document, bounded bool) (*Index, error) {
	if bounded && len(documents) > MaximumIndexDocuments {
		return nil, fmt.Errorf("search document count %d exceeds the %d-document limit", len(documents), MaximumIndexDocuments)
	}
	documentsByID := make(map[string]Document, len(documents))
	for _, document := range documents {
		documentsByID[document.ID] = document
	}
	if bounded {
		var indexedTextBytes int64
		metadataEntries := 0
		for _, document := range documentsByID {
			bytes, err := documentIndexedTextBytes(document)
			if err != nil {
				return nil, err
			}
			if bytes > MaximumIndexedDocumentBytes {
				return nil, fmt.Errorf("search document %q has %d indexed text bytes, above the %d-byte per-document limit", document.ID, bytes, MaximumIndexedDocumentBytes)
			}
			if bytes > MaximumIndexedTextBytes-indexedTextBytes {
				return nil, fmt.Errorf("search corpus indexed text exceeds the %d-byte limit", MaximumIndexedTextBytes)
			}
			indexedTextBytes += bytes
			if len(document.Metadata) > MaximumDocumentMetadataEntries-metadataEntries {
				return nil, fmt.Errorf("search document metadata exceeds the %d-entry corpus limit", MaximumDocumentMetadataEntries)
			}
			metadataEntries += len(document.Metadata)
		}
	}
	documentIDs := make([]string, 0, len(documentsByID))
	for id := range documentsByID {
		documentIDs = append(documentIDs, id)
	}
	sort.Strings(documentIDs)

	index := &Index{
		Version: IndexVersion, Documents: map[string]Document{}, Postings: map[string][]Posting{},
		DocumentLength: map[string]int{}, DocumentCount: len(documentIDs),
	}
	var totalLength int
	postingCount := 0
	postingFieldValues := 0
	var termDictionaryBytes int64
	for _, id := range documentIDs {
		document := documentsByID[id]
		built := buildDocument(document)
		if bounded && built.length > MaximumTokenOccurrences-totalLength {
			return nil, fmt.Errorf("search corpus token occurrences exceed the %d-token limit", MaximumTokenOccurrences)
		}
		index.Documents[document.ID] = document
		index.DocumentLength[document.ID] = built.length
		totalLength += built.length
		for term, fields := range built.terms {
			if bounded {
				if _, exists := index.Postings[term]; !exists {
					if len(index.Postings) >= MaximumDistinctTerms {
						return nil, fmt.Errorf("search term count exceeds the %d-term limit", MaximumDistinctTerms)
					}
					if int64(len(term)) > MaximumTermDictionaryBytes-termDictionaryBytes {
						return nil, fmt.Errorf("search term dictionary exceeds the %d-byte limit", MaximumTermDictionaryBytes)
					}
					termDictionaryBytes += int64(len(term))
				}
				if postingCount >= MaximumPostings {
					return nil, fmt.Errorf("search posting count exceeds the %d-posting limit", MaximumPostings)
				}
				postingCount++
				if len(fields.fields) > MaximumPostingFieldValues-postingFieldValues {
					return nil, fmt.Errorf("search posting fields exceed the %d-value corpus limit", MaximumPostingFieldValues)
				}
				postingFieldValues += len(fields.fields)
			}
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
	if index.DocumentCount > 0 {
		index.AverageLength = float64(totalLength) / float64(index.DocumentCount)
	}
	for term := range index.Postings {
		sort.Slice(index.Postings[term], func(i, j int) bool { return index.Postings[term][i].DocumentID < index.Postings[term][j].DocumentID })
	}
	return index, nil
}

func documentIndexedTextBytes(document Document) (int64, error) {
	values := []string{
		document.ID, document.ObjectType, document.Kind, document.Language,
		document.Title, document.QualifiedName, document.Signature, document.Path,
		document.Body,
	}
	var total int64
	for _, value := range values {
		if int64(len(value)) > MaximumIndexedTextBytes-total {
			return 0, fmt.Errorf("search document %q indexed text byte count overflows its resource envelope", document.ID)
		}
		total += int64(len(value))
	}
	for key, value := range document.Metadata {
		for _, text := range []string{key, value} {
			if int64(len(text)) > MaximumIndexedTextBytes-total {
				return 0, fmt.Errorf("search document %q metadata byte count overflows its resource envelope", document.ID)
			}
			total += int64(len(text))
		}
	}
	return total, nil
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

// Search applies explicit and inline filters to documents matching at least one
// lexical term. A filter-only or empty lexical query returns no hits. Ranking is
// BM25 multiplied by fixed field boosts, plus deterministic exact and prefix
// bonuses; ties use QualifiedName then ID.
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

	// Keep only scores for the matching corpus. Explanations and full result
	// records are assembled after selection, so a broad query cannot allocate
	// per-term/per-field maps for hundreds of thousands of discarded hits.
	scores := map[string]float64{}
	uniqueTerms := unique(terms)
	for _, term := range uniqueTerms {
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
			scores[posting.DocumentID] += bm25 * posting.FieldBoost
		}
	}

	normalizedText := normalize(text)
	best := make(searchCandidateHeap, 0, min(query.Limit, len(scores)))
	for id, score := range scores {
		document := index.Documents[id]
		score = applySearchBonuses(document, normalizedText, score, nil)
		candidate := searchCandidate{document: document, score: roundScore(score), key: document.QualifiedName + "\x00" + document.ID}
		if len(best) < query.Limit {
			heap.Push(&best, candidate)
		} else if candidate.betterThan(best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
	}
	// Sorting at most Limit entries retains exactly the old score/name/ID
	// ordering, including score rounding before tie comparison.
	sort.Slice(best, func(i, j int) bool { return best[i].betterThan(best[j]) })
	hits := make([]Hit, len(best))
	selected := make(map[string]int, len(best))
	reasons := make([]map[string]struct{}, len(best))
	matched := make([]map[string]struct{}, len(best))
	for i, candidate := range best {
		hits[i] = Hit{Document: candidate.document, Score: candidate.score}
		selected[candidate.document.ID] = i
		reasons[i], matched[i] = map[string]struct{}{}, map[string]struct{}{}
		applySearchBonuses(candidate.document, normalizedText, 0, reasons[i])
	}
	// Revisit postings to explain only selected hits. This remains linear in
	// postings and does not assume callers supplied sorted posting slices.
	for _, term := range uniqueTerms {
		for _, posting := range index.Postings[term] {
			if i, ok := selected[posting.DocumentID]; ok {
				matched[i][term] = struct{}{}
				for _, field := range posting.Fields {
					reasons[i][field+":"+term] = struct{}{}
				}
			}
		}
	}
	for i := range hits {
		hits[i].Reasons, hits[i].Terms = keys(reasons[i]), keys(matched[i])
	}
	truncated := len(scores) > query.Limit
	for position := range hits {
		document := hits[position].Document
		if len(document.Body) <= MaximumResultBodyBytes {
			continue
		}
		excerpt, start, end := boundedMatchExcerpt(
			document.Body, hits[position].Terms, MaximumResultBodyBytes,
		)
		document.Body = excerpt
		document.Metadata = cloneMetadata(document.Metadata)
		document.Metadata["rkc_excerpt_start_byte"] = strconv.Itoa(start)
		document.Metadata["rkc_excerpt_end_byte"] = strconv.Itoa(end)
		hits[position].Document = document
		hits[position].Reasons = unique(append(hits[position].Reasons, "body:excerpt", "body:truncated"))
		sort.Strings(hits[position].Reasons)
	}
	return Response{Query: query.Text, Hits: hits, Truncated: truncated, Mode: "embedded-bm25-lexical", IndexVersion: index.Version}
}

// searchCandidate stores one ranked result without allocating its explanation.
type searchCandidate struct {
	document Document
	score    float64
	key      string
}

func (candidate searchCandidate) betterThan(other searchCandidate) bool {
	if candidate.score == other.score {
		return candidate.key < other.key
	}
	return candidate.score > other.score
}

// searchCandidateHeap keeps the worst retained candidate at the root, allowing
// replacement without sorting or retaining the complete matching corpus.
type searchCandidateHeap []searchCandidate

func (values searchCandidateHeap) Len() int           { return len(values) }
func (values searchCandidateHeap) Less(i, j int) bool { return values[j].betterThan(values[i]) }
func (values searchCandidateHeap) Swap(i, j int)      { values[i], values[j] = values[j], values[i] }
func (values *searchCandidateHeap) Push(value any) {
	*values = append(*values, value.(searchCandidate))
}
func (values *searchCandidateHeap) Pop() any {
	last := len(*values) - 1
	value := (*values)[last]
	(*values)[last] = searchCandidate{}
	*values = (*values)[:last]
	return value
}

func applySearchBonuses(document Document, normalizedText string, score float64, reasons map[string]struct{}) float64 {
	if normalizedText == "" {
		return score
	}
	// Stable addition order avoids map-iteration-dependent score rounding at
	// half-micro boundaries. Public score precision remains six decimal places.
	for _, field := range [...]struct{ name, value string }{
		{"title", document.Title}, {"qualified_name", document.QualifiedName},
		{"signature", document.Signature}, {"path", document.Path}, {"id", document.ID},
	} {
		normalizedValue := normalize(field.value)
		reason := ""
		if normalizedValue == normalizedText {
			score += 100
			reason = "exact_" + field.name
		} else if strings.HasPrefix(normalizedValue, normalizedText) {
			score += 12
			reason = "prefix_" + field.name
		}
		if reasons != nil && reason != "" {
			reasons[reason] = struct{}{}
		}
	}
	return score
}

func boundedMatchExcerpt(value string, terms []string, maximum int) (string, int, int) {
	if maximum <= 0 {
		return "", 0, 0
	}
	if len(value) <= maximum {
		return value, 0, len(value)
	}
	match := -1
	for _, term := range terms {
		position := indexFoldASCII(value, term)
		if position >= 0 && (match < 0 || position < match) {
			match = position
		}
	}
	if match < 0 {
		match = 0
	}
	start := match - maximum/3
	if start < 0 {
		start = 0
	}
	if start > len(value)-maximum {
		start = len(value) - maximum
	}
	for start > 0 && !utf8.RuneStart(value[start]) {
		start--
	}
	end := start + maximum
	if end > len(value) {
		end = len(value)
	}
	for end > start && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.ToValidUTF8(value[start:end], ""), start, end
}

func indexFoldASCII(value, term string) int {
	if term == "" {
		return -1
	}
	for _, char := range term {
		if char > unicode.MaxASCII {
			return indexFoldUnicode(value, term)
		}
	}
	for start := 0; start+len(term) <= len(value); start++ {
		matched := true
		for offset := range len(term) {
			left, right := value[start+offset], term[offset]
			if left >= 'A' && left <= 'Z' {
				left += 'a' - 'A'
			}
			if right >= 'A' && right <= 'Z' {
				right += 'a' - 'A'
			}
			if left != right {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	return -1
}

func indexFoldUnicode(value, term string) int {
	wantedRunes := utf8.RuneCountInString(term)
	if wantedRunes == 0 {
		return -1
	}
	for start := range value {
		end := start
		for count := 0; count < wantedRunes && end < len(value); count++ {
			_, width := utf8.DecodeRuneInString(value[end:])
			end += width
		}
		if utf8.RuneCountInString(value[start:end]) == wantedRunes && strings.EqualFold(value[start:end], term) {
			return start
		}
	}
	return -1
}

func cloneMetadata(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
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

// Save serializes index, syncs a sibling temporary file, and renames it to path.
// It does not validate index invariants, use safe-output ownership checks, or sync
// the parent directory after rename, so it is not a complete crash-durability
// barrier. Existing-target replacement behavior is platform-dependent.
func (index *Index) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	bounded := &maximumIndexWriter{writer: temp, maximum: MaximumPersistedIndexBytes}
	encoder := json.NewEncoder(bounded)
	if err := encoder.Encode(index); err != nil {
		return fmt.Errorf("encode bounded search index: %w", err)
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

type maximumIndexWriter struct {
	writer  io.Writer
	written int64
	maximum int64
}

// Write forwards data while enforcing the persisted search-index byte limit.
func (writer *maximumIndexWriter) Write(data []byte) (int, error) {
	if writer.maximum <= 0 || int64(len(data)) > writer.maximum-writer.written {
		return 0, fmt.Errorf("persisted search index exceeds the %d-byte limit", writer.maximum)
	}
	written, err := writer.writer.Write(data)
	writer.written += int64(written)
	return written, err
}

// Load reads the entire file without a size bound, accepts unknown JSON fields,
// and checks only IndexVersion. It does not validate map presence, counts,
// postings, document references, numeric values, or provenance, so it is not a
// trust boundary for attacker-controlled or otherwise unverified index files.
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
		candidate := string(current)
		if len(candidate) > MaximumIndexedTermBytes {
			current = current[:0]
			return
		}
		term := strings.ToLower(candidate)
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
