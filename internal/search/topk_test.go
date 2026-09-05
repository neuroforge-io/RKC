// Copyright 2026 NeuroForgeIO. Licensed under the Apache License, Version 2.0.

package search

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The exhaustive oracle below preserves the previous complete-sort retrieval
// path. It guards ranking, rounded ties, trace fidelity, filters, and excerpts
// while the live implementation retains only the requested best candidates.
func TestSearchTopKMatchesExhaustiveReference(t *testing.T) {
	random := rand.New(rand.NewSource(71))
	documents := make([]Document, 605)
	for i := range documents {
		documents[i] = Document{
			ID: fmt.Sprintf("id-%04d", i), ObjectType: []string{"node", "artifact"}[i%2],
			Kind: []string{"function", "method", "file"}[i%3], Language: []string{"go", "python"}[i%2],
			Title:         []string{"Common", "CommonGraph", "GraphSearch", "search", "Ångström"}[random.Intn(5)],
			QualifiedName: []string{"same", "same\x00nested", "pkg.GraphSearch", "pkg.Other"}[random.Intn(4)],
			Path:          fmt.Sprintf("src/%d/common.go", i%7), Signature: "func CommonGraph()",
			Body:     strings.Repeat("common graph search Ångström ", 1+random.Intn(5)),
			Metadata: map[string]string{"fixture": "unchanged"},
		}
	}
	documents[0].Body = strings.Repeat("界", MaximumResultBodyBytes) + " distantneedle"
	index := Build(documents)
	before, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	queries := []Query{
		{Text: "common"}, {Text: "common common graph"}, {Text: "GraphSearch"},
		{Text: "graph kind:function lang:go type:node path:src/"},
		{Text: "common lang:python", Languages: map[string]struct{}{"go": {}}},
		{Text: "Ångström"}, {Text: "distantneedle"}, {Text: "unknownterm"}, {Text: "kind:function"},
	}
	for _, query := range queries {
		for _, limit := range []int{1, 7, 50, 0, 5000} {
			query.Limit = limit
			got, want := index.Search(query), exhaustiveSearchReference(index, query)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("query %q limit %d differs from exhaustive ranking:\ngot %+v\nwant %+v", query.Text, limit, got, want)
			}
		}
	}
	after, err := json.Marshal(index)
	if err != nil || string(before) != string(after) {
		t.Fatal("search mutated the shared immutable index")
	}
}

func TestSearchTopKRoundsBeforeTiesAndAcceptsUnsortedPostings(t *testing.T) {
	index := &Index{
		Version: IndexVersion, DocumentCount: 2, AverageLength: 1,
		Documents: map[string]Document{
			"a": {ID: "a", QualifiedName: "aaa"},
			"z": {ID: "z", QualifiedName: "zzz"},
		},
		DocumentLength: map[string]int{"a": 1, "z": 1},
		Postings: map[string][]Posting{"term": {
			{DocumentID: "z", TermCount: 1, FieldBoost: 1.0000003 / math.Log(1.2), Fields: []string{"body"}},
			{DocumentID: "a", TermCount: 1, FieldBoost: 1.0000002 / math.Log(1.2), Fields: []string{"title"}},
		}},
	}
	got := index.Search(Query{Text: "term", Limit: 1})
	if len(got.Hits) != 1 || got.Hits[0].Document.ID != "a" || got.Hits[0].Score != 1 || !got.Truncated || !reflect.DeepEqual(got.Hits[0].Reasons, []string{"title:term"}) {
		t.Fatalf("raw score or posting order overrode rounded tie ranking: %+v", got)
	}
}

func TestSearchBonusesUseStableFieldOrderAtRoundingBoundary(t *testing.T) {
	// The previous map traversal could add the prefix bonus before the exact
	// bonus, changing the rounded score by a millionth for this boundary.
	document := Document{Title: "term", QualifiedName: "term extra"}
	for range 100 {
		if got := roundScore(applySearchBonuses(document, "term", 0.0000255, nil)); got != 112.000026 {
			t.Fatalf("unstable bonus order: %.9f", got)
		}
	}
}

func BenchmarkSearchBroadTopK(b *testing.B) {
	documents := make([]Document, 20000)
	for i := range documents {
		documents[i] = Document{
			ID: fmt.Sprintf("node-%05d", i), ObjectType: "node", Kind: "function", Language: "go",
			Title: "CommonHandler", QualifiedName: fmt.Sprintf("service%05d.CommonHandler", i),
			Signature: "func CommonHandler()", Path: fmt.Sprintf("service/%05d.go", i),
			Body: "Common graph handler uses shared repository configuration and returns a deterministic response.",
		}
	}
	index := Build(documents)
	for _, query := range []string{"common", "common graph handler"} {
		b.Run(query, func(b *testing.B) {
			for _, method := range []struct {
				name string
				run  func(Query) Response
			}{
				{"exhaustive_reference", func(query Query) Response { return exhaustiveSearchReference(index, query) }},
				{"bounded_candidates", index.Search},
			} {
				b.Run(method.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						response := method.run(Query{Text: query, Limit: 20})
						if len(response.Hits) != 20 || !response.Truncated {
							b.Fatal("invalid bounded search response")
						}
					}
				})
			}
		})
	}
}

func exhaustiveSearchReference(index *Index, query Query) Response {
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
		document := index.Documents[id]
		reasons := keys(current.reasons)
		matchedTerms := keys(current.terms)
		hits = append(hits, Hit{Document: document, Score: roundScore(current.score), Reasons: reasons, Terms: matchedTerms})
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
