package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func TestSearchFTSIsRankedFilteredBoundedAndDeterministic(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("search-snapshot", "search-repository", "")
	writerTestCommit(t, database, bundle)

	query := search.Query{Text: "Alpha", Limit: 10}
	first, err := database.SearchFTS(context.Background(), "search-snapshot", query)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.SearchFTS(context.Background(), "search-snapshot", query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated FTS results differ:\n%+v\n%+v", first, second)
	}
	if first.Mode != "sqlite-fts5-bm25" || first.IndexVersion != "sqlite-fts5-1" ||
		first.Query != query.Text || len(first.Hits) < 3 {
		t.Fatalf("unexpected FTS response: %+v", first)
	}
	if first.Hits[0].Score < first.Hits[len(first.Hits)-1].Score {
		t.Fatalf("FTS ranking is ascending: %+v", first.Hits)
	}
	foundNode := false
	for _, hit := range first.Hits {
		if !reflect.DeepEqual(hit.Terms, []string{"alpha"}) ||
			!reflect.DeepEqual(hit.Reasons, []string{"fts5:bm25"}) {
			t.Fatalf("missing deterministic retrieval trace: %+v", hit)
		}
		if hit.Document.ID == "node-a" {
			foundNode = hit.Document.ObjectType == "node" &&
				hit.Document.Kind == "function" &&
				hit.Document.Language == "go" &&
				hit.Document.Path == "main.go"
		}
	}
	if !foundNode {
		t.Fatalf("node projection was not hydrated: %+v", first.Hits)
	}

	filtered, err := database.SearchFTS(context.Background(), "search-snapshot", search.Query{
		Text: "Alpha",
		Kinds: map[string]struct{}{
			"function": {},
		},
		Languages: map[string]struct{}{
			"go": {},
		},
		ObjectTypes: map[string]struct{}{
			"node": {},
		},
		PathPrefix: "main",
		Limit:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Hits) != 1 || filtered.Hits[0].Document.ID != "node-a" {
		t.Fatalf("filtered FTS response = %+v", filtered)
	}

	empty, err := database.SearchFTS(
		context.Background(), "search-snapshot", search.Query{Text: " \t\n"},
	)
	if err != nil || len(empty.Hits) != 0 || empty.Truncated {
		t.Fatalf("empty FTS response = %+v, %v", empty, err)
	}
}

func TestSearchFTSRejectsInvalidQueriesAndMissingSnapshots(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("search-errors", "search-repository", "")
	writerTestCommit(t, database, bundle)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name  string
		ctx   context.Context
		id    rkcstore.SnapshotID
		query search.Query
		want  error
	}{
		{
			name: "cancelled", ctx: cancelled, id: "search-errors",
			query: search.Query{Text: "Alpha"}, want: context.Canceled,
		},
		{
			name: "missing snapshot", ctx: context.Background(), id: "missing",
			query: search.Query{Text: "Alpha"}, want: rkcstore.ErrSnapshotNotFound,
		},
		{
			name: "negative limit", ctx: context.Background(), id: "search-errors",
			query: search.Query{Text: "Alpha", Limit: -1}, want: rkcstore.ErrInvalidQuery,
		},
		{
			name: "oversized query", ctx: context.Background(), id: "search-errors",
			query: search.Query{Text: strings.Repeat("x", ftsMaximumQueryBytes+1)},
			want:  rkcstore.ErrInvalidQuery,
		},
		{
			name: "oversized filter", ctx: context.Background(), id: "search-errors",
			query: search.Query{
				Text: "Alpha",
				Kinds: map[string]struct{}{
					strings.Repeat("x", ftsMaximumFilterBytes+1): {},
				},
			},
			want: rkcstore.ErrInvalidQuery,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := database.SearchFTS(test.ctx, test.id, test.query)
			if !errors.Is(err, test.want) {
				t.Fatalf("SearchFTS error = %v, want %v", err, test.want)
			}
		})
	}

	terms := make([]string, ftsMaximumTerms+1)
	for index := range terms {
		terms[index] = string(rune('a'+index%26)) + strings.Repeat("x", index/26+1)
	}
	if _, err := database.SearchFTS(context.Background(), "search-errors", search.Query{
		Text: strings.Join(terms, " "),
	}); !errors.Is(err, rkcstore.ErrInvalidQuery) {
		t.Fatalf("too-many-terms error = %v", err)
	}
}

func TestSearchFTSBoundsLargeMultibyteBodies(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("search-body", "search-repository", "")
	writerTestCommit(t, database, bundle)
	body := "Alpha " + strings.Repeat("界", ftsMaximumBodyBytes)
	if _, err := database.db.Exec(
		`UPDATE search_fts SET body = ?
		  WHERE snapshot_id = ? AND object_type = 'document' AND object_id = 'document'`,
		body, bundle.Snapshot.ID,
	); err != nil {
		t.Fatal(err)
	}
	response, err := database.SearchFTS(context.Background(), "search-body", search.Query{
		Text: "Alpha", ObjectTypes: map[string]struct{}{"document": {}}, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Hits) != 1 || len(response.Hits[0].Document.Body) > ftsMaximumBodyBytes ||
		!utf8.ValidString(response.Hits[0].Document.Body) ||
		!reflect.DeepEqual(response.Hits[0].Reasons, []string{"fts5:bm25", "body:truncated"}) {
		t.Fatalf("bounded multibyte FTS response = %+v", response)
	}
}
