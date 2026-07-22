package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const VectorIndexVersion = "1"

type EmbeddingDescriptor struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Digest     string `json:"digest,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type EmbeddingProvider interface {
	Descriptor() EmbeddingDescriptor
	Embed(context.Context, []string) ([][]float32, error)
}

type VectorRecord struct {
	DocumentID    string    `json:"document_id"`
	ContentSHA256 string    `json:"content_sha256"`
	Values        []float32 `json:"values"`
}

type VectorIndex struct {
	Version    string              `json:"version"`
	Descriptor EmbeddingDescriptor `json:"descriptor"`
	Documents  map[string]Document `json:"documents"`
	Vectors    []VectorRecord      `json:"vectors"`
}

type VectorBuildOptions struct {
	BatchSize        int
	MaximumTextBytes int
}

func BuildVectorIndex(ctx context.Context, lexical *Index, provider EmbeddingProvider, options VectorBuildOptions) (*VectorIndex, error) {
	if lexical == nil || provider == nil {
		return nil, errors.New("lexical index and embedding provider are required")
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 16
	}
	if options.BatchSize > 256 {
		options.BatchSize = 256
	}
	if options.MaximumTextBytes <= 0 {
		options.MaximumTextBytes = 16 * 1024
	}
	ids := make([]string, 0, len(lexical.Documents))
	for id := range lexical.Documents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := &VectorIndex{Version: VectorIndexVersion, Descriptor: provider.Descriptor(), Documents: make(map[string]Document, len(ids))}
	for start := 0; start < len(ids); start += options.BatchSize {
		end := start + options.BatchSize
		if end > len(ids) {
			end = len(ids)
		}
		texts := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			document := lexical.Documents[id]
			result.Documents[id] = document
			texts = append(texts, embeddingText(document, options.MaximumTextBytes))
		}
		vectors, err := provider.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed documents %d-%d: %w", start, end, err)
		}
		if len(vectors) != len(texts) {
			return nil, fmt.Errorf("embedding provider returned %d vectors for %d documents", len(vectors), len(texts))
		}
		for offset, vector := range vectors {
			if err := normalizeVector(vector); err != nil {
				return nil, fmt.Errorf("normalize embedding for %s: %w", ids[start+offset], err)
			}
			if result.Descriptor.Dimensions == 0 {
				result.Descriptor.Dimensions = len(vector)
			}
			if len(vector) != result.Descriptor.Dimensions {
				return nil, errors.New("embedding provider returned inconsistent dimensions")
			}
			digest := sha256.Sum256([]byte(texts[offset]))
			result.Vectors = append(result.Vectors, VectorRecord{DocumentID: ids[start+offset], ContentSHA256: hex.EncodeToString(digest[:]), Values: vector})
		}
	}
	return result, nil
}

func (index *VectorIndex) Search(query Query, queryVector []float32) (Response, error) {
	if index == nil || index.Version != VectorIndexVersion {
		return Response{}, errors.New("unsupported or missing vector index")
	}
	if len(queryVector) != index.Descriptor.Dimensions {
		return Response{}, fmt.Errorf("query embedding dimension %d does not match index dimension %d", len(queryVector), index.Descriptor.Dimensions)
	}
	queryVector = append([]float32(nil), queryVector...)
	if err := normalizeVector(queryVector); err != nil {
		return Response{}, err
	}
	_, parsed := parseQuery(query.Text)
	query = applyParsedFilters(query, parsed)
	if query.Limit <= 0 {
		query.Limit = 50
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}
	hits := make([]Hit, 0, len(index.Vectors))
	for _, record := range index.Vectors {
		document, ok := index.Documents[record.DocumentID]
		if !ok || !matchesFilters(document, query) || len(record.Values) != len(queryVector) {
			continue
		}
		score := dotProduct(queryVector, record.Values)
		if math.IsNaN(score) || math.IsInf(score, 0) {
			continue
		}
		hits = append(hits, Hit{Document: document, Score: roundScore(score), Reasons: []string{"dense_cosine"}})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Document.ID < hits[j].Document.ID
		}
		return hits[i].Score > hits[j].Score
	})
	truncated := len(hits) > query.Limit
	if truncated {
		hits = hits[:query.Limit]
	}
	return Response{Query: query.Text, Hits: hits, Truncated: truncated, Mode: "dense-cosine", IndexVersion: index.Version}, nil
}

// Fuse combines lexical and semantic rankings using reciprocal-rank fusion,
// avoiding unsafe assumptions that BM25 and cosine scores share a scale.
func Fuse(query string, lexical, semantic Response, limit int) Response {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	const rankConstant = 60.0
	type fused struct {
		hit     Hit
		score   float64
		reasons map[string]struct{}
	}
	values := map[string]*fused{}
	add := func(response Response, channel string) {
		for rank, hit := range response.Hits {
			current := values[hit.Document.ID]
			if current == nil {
				current = &fused{hit: hit, reasons: map[string]struct{}{}}
				values[hit.Document.ID] = current
			}
			current.score += 1 / (rankConstant + float64(rank+1))
			current.reasons[fmt.Sprintf("%s_rank:%d", channel, rank+1)] = struct{}{}
		}
	}
	add(lexical, "lexical")
	add(semantic, "semantic")
	hits := make([]Hit, 0, len(values))
	for _, value := range values {
		value.hit.Score = roundScore(value.score)
		value.hit.Reasons = keys(value.reasons)
		hits = append(hits, value.hit)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Document.ID < hits[j].Document.ID
		}
		return hits[i].Score > hits[j].Score
	})
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	return Response{Query: query, Hits: hits, Truncated: truncated, Mode: "hybrid-rrf", IndexVersion: IndexVersion + "+vector-" + VectorIndexVersion}
}

func (index *VectorIndex) Save(path string) error {
	if index == nil || index.Version != VectorIndexVersion {
		return errors.New("cannot save invalid vector index")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".vector-index-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func LoadVectorIndex(path string) (*VectorIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var index VectorIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode vector index: %w", err)
	}
	if index.Version != VectorIndexVersion || index.Descriptor.Dimensions <= 0 {
		return nil, errors.New("unsupported or invalid vector index")
	}
	if strings.TrimSpace(index.Descriptor.Provider) == "" || strings.TrimSpace(index.Descriptor.Model) == "" {
		return nil, errors.New("vector index embedding provider and model are required")
	}
	seen := make(map[string]struct{}, len(index.Vectors))
	for _, record := range index.Vectors {
		if strings.TrimSpace(record.DocumentID) == "" {
			return nil, errors.New("vector document id is required")
		}
		if _, duplicate := seen[record.DocumentID]; duplicate {
			return nil, fmt.Errorf("duplicate vector for document %s", record.DocumentID)
		}
		seen[record.DocumentID] = struct{}{}
		if _, ok := index.Documents[record.DocumentID]; !ok {
			return nil, fmt.Errorf("vector %s references a missing document", record.DocumentID)
		}
		if len(record.Values) != index.Descriptor.Dimensions {
			return nil, fmt.Errorf("vector %s has invalid dimensions", record.DocumentID)
		}
		if err := validateStoredVector(record.Values); err != nil {
			return nil, fmt.Errorf("vector %s is invalid: %w", record.DocumentID, err)
		}
	}
	return &index, nil
}

func embeddingText(document Document, maximumBytes int) string {
	text := strings.Join([]string{
		"type: " + document.ObjectType,
		"kind: " + document.Kind,
		"language: " + document.Language,
		"title: " + document.Title,
		"qualified_name: " + document.QualifiedName,
		"signature: " + document.Signature,
		"path: " + document.Path,
		"content: " + document.Body,
	}, "\n")
	if len(text) <= maximumBytes {
		return text
	}
	text = text[:maximumBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func normalizeVector(vector []float32) error {
	if len(vector) == 0 {
		return errors.New("embedding vector is empty")
	}
	var sum float64
	for _, value := range vector {
		floatValue := float64(value)
		if math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
			return errors.New("embedding vector contains a non-finite value")
		}
		sum += floatValue * floatValue
	}
	if sum == 0 {
		return errors.New("embedding vector has zero norm")
	}
	norm := float32(math.Sqrt(sum))
	for index := range vector {
		vector[index] /= norm
	}
	return nil
}

func validateStoredVector(vector []float32) error {
	if len(vector) == 0 {
		return errors.New("embedding vector is empty")
	}
	var sum float64
	for _, value := range vector {
		floatValue := float64(value)
		if math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
			return errors.New("embedding vector contains a non-finite value")
		}
		sum += floatValue * floatValue
	}
	if sum == 0 {
		return errors.New("embedding vector has zero norm")
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-3 {
		return errors.New("embedding vector is not normalized")
	}
	return nil
}

func dotProduct(left, right []float32) float64 {
	var result float64
	for index := range left {
		result += float64(left[index]) * float64(right[index])
	}
	return result
}
