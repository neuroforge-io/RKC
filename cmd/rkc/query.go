package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
)

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	kinds := fs.String("kinds", "", "comma-separated node kinds")
	languages := fs.String("languages", "", "comma-separated languages")
	objects := fs.String("objects", "", "comma-separated object types")
	pathPrefix := fs.String("path-prefix", "", "restrict results to path prefix")
	limit := fs.Int("limit", 20, "maximum results")
	graphHops := fs.Int("graph-hops", 0, "bounded graph expansion hops after retrieval")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("query text is required")
	}
	dataset, err := loadDataset(*dir)
	if err != nil {
		return err
	}
	query := search.Query{Text: strings.Join(fs.Args(), " "), Kinds: splitSet(*kinds), Languages: splitSet(*languages), ObjectTypes: splitSet(*objects), PathPrefix: *pathPrefix, Limit: *limit}
	engine := retrieval.Engine{Lexical: dataset.Search, Graph: dataset.Graph}
	result, err := engine.Search(context.Background(), query, retrieval.Options{Mode: retrieval.ModeLexical, GraphHops: *graphHops, GraphNodeLimit: 500})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	for index, hit := range result.Hits {
		fmt.Printf("%2d. %8.3f %-12s %-16s %s\n", index+1, hit.Score, hit.Document.ObjectType, hit.Document.Kind, firstNonBlank(hit.Document.QualifiedName, hit.Document.Title))
	}
	if result.Truncated {
		fmt.Println("... results truncated")
	}
	return nil
}
