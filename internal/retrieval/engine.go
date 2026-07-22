// Package retrieval combines lexical ranking, dense embeddings, and bounded
// repository-graph expansion into one auditable GraphRAG retrieval path.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/search"
)

type Mode string

const (
	ModeLexical  Mode = "lexical"
	ModeSemantic Mode = "semantic"
	ModeHybrid   Mode = "hybrid"
)

type Options struct {
	Mode           Mode
	GraphHops      int
	GraphNodeLimit int
}

type Engine struct {
	Lexical  *search.Index
	Vector   *search.VectorIndex
	Embedder search.EmbeddingProvider
	Graph    *graph.Index
}

func (engine *Engine) Search(ctx context.Context, query search.Query, options Options) (search.Response, error) {
	if engine == nil || engine.Lexical == nil {
		return search.Response{}, errors.New("lexical index is required")
	}
	if err := ctx.Err(); err != nil {
		return search.Response{}, err
	}
	if options.Mode == "" {
		options.Mode = ModeHybrid
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 1000 {
		limit = 1000
	}
	candidateQuery := query
	candidateQuery.Limit = min(limit*4, 1000)
	lexical := engine.Lexical.Search(candidateQuery)
	var result search.Response
	switch options.Mode {
	case ModeLexical:
		result = lexical
	case ModeSemantic, ModeHybrid:
		if engine.Vector == nil || engine.Embedder == nil {
			return search.Response{}, errors.New("semantic search requires a vector index and embedding provider")
		}
		vectors, err := engine.Embedder.Embed(ctx, []string{query.Text})
		if err != nil {
			return search.Response{}, fmt.Errorf("embed query: %w", err)
		}
		if len(vectors) != 1 {
			return search.Response{}, errors.New("embedding provider returned an invalid query vector count")
		}
		semantic, err := engine.Vector.Search(candidateQuery, vectors[0])
		if err != nil {
			return search.Response{}, err
		}
		if options.Mode == ModeSemantic {
			result = semantic
		} else {
			result = search.Fuse(query.Text, lexical, semantic, candidateQuery.Limit)
		}
	default:
		return search.Response{}, fmt.Errorf("unknown retrieval mode %q", options.Mode)
	}
	if options.GraphHops > 0 && engine.Graph != nil {
		result = engine.expandGraph(ctx, result, query, options)
	}
	result.Query = query.Text
	result.Truncated = len(result.Hits) > limit || result.Truncated
	if len(result.Hits) > limit {
		result.Hits = result.Hits[:limit]
	}
	return result, nil
}

func (engine *Engine) expandGraph(ctx context.Context, response search.Response, query search.Query, options Options) search.Response {
	if options.GraphHops > 4 {
		options.GraphHops = 4
	}
	if options.GraphNodeLimit <= 0 {
		options.GraphNodeLimit = 200
	} else if options.GraphNodeLimit > 5000 {
		options.GraphNodeLimit = 5000
	}
	type accumulated struct {
		hit     search.Hit
		score   float64
		reasons map[string]struct{}
	}
	values := map[string]*accumulated{}
	for _, hit := range response.Hits {
		current := &accumulated{hit: hit, score: hit.Score, reasons: map[string]struct{}{}}
		for _, reason := range hit.Reasons {
			current.reasons[reason] = struct{}{}
		}
		values[hit.Document.ID] = current
	}
	seeds := append([]search.Hit(nil), response.Hits...)
	remainingWork := options.GraphNodeLimit
	for _, seed := range seeds {
		if err := ctx.Err(); err != nil {
			response.Truncated = true
			break
		}
		if remainingWork <= 0 {
			response.Truncated = true
			break
		}
		if seed.Document.ObjectType != "node" {
			continue
		}
		neighborhood, err := engine.Graph.Neighborhood(seed.Document.ID, graph.TraverseOptions{
			Direction: graph.DirectionBoth, MaxDepth: options.GraphHops, MaxNodes: remainingWork,
		})
		if err != nil {
			continue
		}
		remainingWork -= len(neighborhood.Nodes)
		if neighborhood.Truncated {
			response.Truncated = true
		}
		for _, node := range neighborhood.Nodes {
			if node.ID == seed.Document.ID {
				continue
			}
			document, ok := engine.Lexical.Documents[node.ID]
			if !ok {
				continue
			}
			if !search.MatchesQuery(document, query) {
				continue
			}
			depth := neighborhood.DepthByID[node.ID]
			bonus := seed.Score * math.Pow(0.55, float64(depth))
			current := values[node.ID]
			if current == nil {
				current = &accumulated{hit: search.Hit{Document: document}, reasons: map[string]struct{}{}}
				values[node.ID] = current
			}
			current.score += bonus
			current.reasons[fmt.Sprintf("graph_from:%s:depth:%d", seed.Document.ID, depth)] = struct{}{}
		}
	}
	response.Hits = response.Hits[:0]
	for _, current := range values {
		current.hit.Score = math.Round(current.score*1_000_000) / 1_000_000
		current.hit.Reasons = make([]string, 0, len(current.reasons))
		for reason := range current.reasons {
			current.hit.Reasons = append(current.hit.Reasons, reason)
		}
		sort.Strings(current.hit.Reasons)
		response.Hits = append(response.Hits, current.hit)
	}
	sort.Slice(response.Hits, func(i, j int) bool {
		if response.Hits[i].Score == response.Hits[j].Score {
			return response.Hits[i].Document.ID < response.Hits[j].Document.ID
		}
		return response.Hits[i].Score > response.Hits[j].Score
	})
	response.Mode += "+graph"
	return response
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
