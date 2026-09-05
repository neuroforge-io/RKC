// Copyright 2026 NeuroForgeIO. Licensed under the Apache License, Version 2.0.

package search

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestSearchPageMatchesExhaustiveReference(t *testing.T) {
	documents := make([]Document, 137)
	for i := range documents {
		documents[i] = Document{
			ID: fmt.Sprintf("id-%04d", i), ObjectType: []string{"node", "artifact"}[i%2],
			Kind: []string{"function", "method", "file"}[i%3], Language: []string{"go", "python"}[i%2],
			Title:         []string{"Common", "CommonGraph", "GraphSearch", "search", "Ångström"}[i%5],
			QualifiedName: []string{"same", "same\x00nested", "pkg.GraphSearch", "pkg.Other"}[i%4],
			Path:          fmt.Sprintf("src/%d/common.go", i%7), Signature: "func CommonGraph()",
			Body:     strings.Repeat("common graph search Ångström ", 1+i%5),
			Metadata: map[string]string{"fixture": "unchanged"},
		}
	}
	documents[0].Body = strings.Repeat("界", MaximumResultBodyBytes) + " common distantneedle"
	index := Build(documents)
	before, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	queries := []Query{
		{Text: "common"}, {Text: "common common graph"}, {Text: "GraphSearch"},
		{Text: "graph kind:function lang:go type:node path:src/"},
		{Text: "common lang:python", Languages: map[string]struct{}{"go": {}}},
		{Text: "common kind:function", Kinds: map[string]struct{}{}},
		{Text: "graph", PathPrefix: "src/2/", ObjectTypes: map[string]struct{}{"artifact": {}}},
		{Text: "Ångström"}, {Text: "distantneedle"}, {Text: "unknownterm"},
		{Text: "kind:function"}, {Text: ""},
	}
	for _, query := range queries {
		query.Limit = 1000
		want := exhaustiveSearchReference(index, query)
		for _, limits := range [][]int{{1}, {7}, {13, 1, 64, 3}, {0}, {5000}} {
			t.Run(fmt.Sprintf("%s/%v", query.Text, limits), func(t *testing.T) {
				var after *Position
				offset := 0
				for pageNumber := 0; ; pageNumber++ {
					query.Limit = limits[pageNumber%len(limits)]
					page := index.SearchPage(query, after)
					limit := query.Limit
					if limit <= 0 {
						limit = 50
					} else if limit > 1000 {
						limit = 1000
					}
					end := min(offset+limit, len(want.Hits))
					wantPage := want
					wantPage.Hits = want.Hits[offset:end]
					wantPage.Truncated = end < len(want.Hits)
					if page.Total != len(want.Hits) || !reflect.DeepEqual(page.Response, wantPage) {
						t.Fatalf("page %d total=%d want=%d:\ngot %+v\nwant %+v", pageNumber, page.Total, len(want.Hits), page.Response, wantPage)
					}
					if len(page.Hits) > 0 {
						last := page.Hits[len(page.Hits)-1]
						after = &Position{Score: last.Score, Key: last.Document.QualifiedName + "\x00" + last.Document.ID, ID: last.Document.ID}
					}
					offset = end
					if !page.Truncated {
						break
					}
					if pageNumber > len(want.Hits) {
						t.Fatal("continuation made no progress")
					}
				}
				final := index.SearchPage(query, after)
				if final.Total != len(want.Hits) || len(final.Hits) != 0 || final.Truncated {
					t.Fatalf("exhausted continuation returned more hits: %+v", final)
				}
			})
		}
	}
	after, err := json.Marshal(index)
	if err != nil || string(before) != string(after) {
		t.Fatal("pagination mutated the shared immutable index")
	}
}

func TestSearchPageUsesRoundedScoresBonusesAndCompleteTieKeys(t *testing.T) {
	index := &Index{
		Version: IndexVersion, DocumentCount: 4, AverageLength: 1,
		Documents: map[string]Document{
			"a":      {ID: "a", QualifiedName: "same"},
			"a\x00z": {ID: "a\x00z", QualifiedName: "same"},
			"z":      {ID: "z", QualifiedName: "same\x00nested"},
			"bonus":  {ID: "bonus", QualifiedName: "zzz", Title: "term"},
		},
		DocumentLength: map[string]int{"a": 1, "a\x00z": 1, "z": 1, "bonus": 1},
		Postings: map[string][]Posting{"term": {
			{DocumentID: "z", TermCount: 1, FieldBoost: 1.0000003 / math.Log(1+0.5/4.5), Fields: []string{"body"}},
			{DocumentID: "a\x00z", TermCount: 1, FieldBoost: 1.0000001 / math.Log(1+0.5/4.5), Fields: []string{"signature"}},
			{DocumentID: "a", TermCount: 1, FieldBoost: 1.0000002 / math.Log(1+0.5/4.5), Fields: []string{"title"}},
			{DocumentID: "bonus", TermCount: 1, FieldBoost: 0.5 / math.Log(1+0.5/4.5), Fields: []string{"title"}},
		}},
	}
	var after *Position
	for i, id := range []string{"bonus", "a", "a\x00z", "z"} {
		page := index.SearchPage(Query{Text: "term", Limit: 1}, after)
		if page.Total != 4 || len(page.Hits) != 1 || page.Hits[0].Document.ID != id || page.Truncated != (i < 3) {
			t.Fatalf("page %d lost rounded/bonus/tie ordering: %+v", i, page)
		}
		hit := page.Hits[0]
		wantScore := 1.0
		if i == 0 {
			wantScore = 100.5
		}
		if hit.Score != wantScore {
			t.Fatalf("page %d score=%f want=%f", i, hit.Score, wantScore)
		}
		after = &Position{Score: hit.Score, Key: hit.Document.QualifiedName + "\x00" + hit.Document.ID, ID: hit.Document.ID}
	}
	final := index.SearchPage(Query{Text: "term", Limit: 1}, after)
	if final.Total != 4 || len(final.Hits) != 0 || final.Truncated {
		t.Fatalf("final rounded-score page was not exhausted: %+v", final)
	}
}

func TestSearchPageTraversesBeyondMaximumPageSize(t *testing.T) {
	documents := make([]Document, 1107)
	for i := range documents {
		documents[i] = Document{ID: fmt.Sprintf("id-%04d", i), QualifiedName: "same", Body: "common"}
	}
	index := Build(documents)
	var after *Position
	offset := 0
	for pageNumber, limit := range []int{-1, 5000, 1, 1000} {
		page := index.SearchPage(Query{Text: "common", Limit: limit}, after)
		wantLength := []int{50, 1000, 1, 56}[pageNumber]
		if page.Total != len(documents) || len(page.Hits) != wantLength || page.Truncated != (pageNumber < 3) {
			t.Fatalf("page %d total/limit/remaining contract: total=%d hits=%d truncated=%t", pageNumber, page.Total, len(page.Hits), page.Truncated)
		}
		for i, hit := range page.Hits {
			if hit.Document.ID != documents[offset+i].ID {
				t.Fatalf("page %d hit %d=%q want=%q", pageNumber, i, hit.Document.ID, documents[offset+i].ID)
			}
		}
		offset += len(page.Hits)
		last := page.Hits[len(page.Hits)-1]
		after = &Position{Score: last.Score, Key: last.Document.QualifiedName + "\x00" + last.Document.ID, ID: last.Document.ID}
	}
}

func TestSearchPageBreaksCompositeKeyCollisionsByID(t *testing.T) {
	index := Build([]Document{
		{ID: "c", QualifiedName: "a\x00b", Body: "common"},
		{ID: "b\x00c", QualifiedName: "a", Body: "common"},
	})
	query := Query{Text: "common", Limit: 10}
	want := exhaustiveSearchReference(index, query)
	if len(want.Hits) != 2 || want.Hits[0].Score != want.Hits[1].Score ||
		want.Hits[0].Document.ID != "b\x00c" || want.Hits[1].Document.ID != "c" {
		t.Fatalf("fixture requires an equal-score composite-key collision: %+v", want)
	}
	query.Limit = 1
	var after *Position
	for i, hit := range want.Hits {
		page := index.SearchPage(query, after)
		if page.Total != 2 || len(page.Hits) != 1 || !reflect.DeepEqual(page.Hits[0], hit) || page.Truncated != (i == 0) {
			t.Fatalf("colliding key omitted or repeated a record on page %d: %+v", i, page)
		}
		after = &Position{Score: hit.Score, Key: hit.Document.QualifiedName + "\x00" + hit.Document.ID, ID: hit.Document.ID}
	}
	final := index.SearchPage(query, after)
	if final.Total != 2 || len(final.Hits) != 0 || final.Truncated {
		t.Fatalf("colliding-key continuation did not exhaust: %+v", final)
	}
}
