package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/neuroforge-io/RKC/internal/graph"
)

// runCounterfactual compares a canonical graph route with a bounded derived
// view in which explicitly selected nodes or edges are absent. It never edits
// the atlas and never presents structural reachability as runtime causation.
func runCounterfactual(args []string) error {
	fs := flag.NewFlagSet("counterfactual", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	from := fs.String("from", "", "source node ID, logical ID, qualified name, or unique name")
	to := fs.String("to", "", "target node ID, logical ID, qualified name, or unique name")
	withoutNodes := stringList{}
	withoutEdges := stringList{}
	fs.Var(&withoutNodes, "without-node", "hypothetically omit this node ID, logical ID, qualified name, or unique name; repeatable")
	fs.Var(&withoutEdges, "without-edge", "hypothetically omit this exact edge ID; repeatable")
	direction := fs.String("direction", "outgoing", "incoming, outgoing, or both")
	edgeKinds := fs.String("edge-kinds", "", "comma-separated edge kinds")
	resolutions := fs.String("resolutions", "", "comma-separated resolution classes")
	depth := fs.Int("depth", 12, "maximum traversal depth")
	limit := fs.Int("limit", 10000, "maximum visited nodes per scenario")
	includeUnresolved := fs.Bool("include-unresolved", false, "include unresolved edges")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("counterfactual does not accept positional arguments")
	}
	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		return errors.New("--from and --to are required")
	}
	if len(withoutNodes)+len(withoutEdges) == 0 {
		return errors.New("at least one --without-node or --without-edge intervention is required")
	}
	dataset, err := loadSelectedDataset(context.Background(), *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	source, err := resolveNode(dataset, *from)
	if err != nil {
		return err
	}
	target, err := resolveNode(dataset, *to)
	if err != nil {
		return err
	}
	withoutNodeIDs := make([]string, 0, len(withoutNodes))
	for _, reference := range withoutNodes {
		node, err := resolveNode(dataset, reference)
		if err != nil {
			return fmt.Errorf("resolve --without-node %q: %w", reference, err)
		}
		withoutNodeIDs = append(withoutNodeIDs, node.ID)
	}
	options, err := traversalOptions(*direction, *edgeKinds, *resolutions, *depth, *limit, *includeUnresolved)
	if err != nil {
		return err
	}
	result, err := dataset.Graph.CounterfactualPath(graph.CounterfactualRequest{
		FromID: source.ID, ToID: target.ID,
		WithoutNodeIDs: withoutNodeIDs, WithoutEdgeIDs: append([]string(nil), withoutEdges...),
		Traversal: options,
	})
	if err != nil {
		return err
	}
	result.SnapshotID = dataset.Bundle.Snapshot.ID
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	fmt.Println("Bounded structural counterfactual (non-authoritative)")
	fmt.Printf("Snapshot: %s\n", result.SnapshotID)
	fmt.Printf("Outcome: %s\n", strings.ReplaceAll(result.Outcome, "_", " "))
	fmt.Printf("Finding: %s\n", result.Statement)
	if len(result.Assumption.WithoutNodeIDs) > 0 {
		fmt.Printf("Without nodes: %s\n", strings.Join(result.Assumption.WithoutNodeIDs, ", "))
	}
	if len(result.Assumption.WithoutEdgeIDs) > 0 {
		fmt.Printf("Without edges: %s\n", strings.Join(result.Assumption.WithoutEdgeIDs, ", "))
	}
	printCounterfactualRoute("Baseline", result.Baseline)
	printCounterfactualRoute("After intervention", result.Counterfactual)
	fmt.Printf("Scope: direction=%s depth<=%d nodes<=%d unresolved=%t\n",
		result.Scope.Direction, result.Scope.MaximumDepth, result.Scope.MaximumNodes, result.Scope.IncludeUnresolved)
	if len(result.EvidenceIDs) > 0 {
		fmt.Printf("Evidence records: %d\n", len(result.EvidenceIDs))
	}
	return nil
}

func printCounterfactualRoute(label string, path graph.Path) {
	if !path.Found {
		switch {
		case path.Truncated:
			fmt.Printf("%s: search truncated at the %s after visiting %d nodes; route existence remains unresolved\n",
				label, graphPathLimitText(path), path.Visited)
		case path.SearchExhausted:
			fmt.Printf("%s: no route found after exhausting the admissible reachable graph (visited %d nodes)\n",
				label, path.Visited)
		default:
			fmt.Printf("%s: no route found, but search completion is unknown (visited %d nodes)\n",
				label, path.Visited)
		}
		return
	}
	fmt.Printf("%s: depth %d, visited %d nodes\n", label, path.Depth, path.Visited)
	for index, node := range path.Nodes {
		fmt.Printf("  %d. %s [%s]\n", index+1, displayNode(node.Name, node.QualifiedName), node.Kind)
		if index < len(path.Edges) {
			edge := path.Edges[index]
			fmt.Printf("     -> %s [%s] (%s)\n", edge.Kind, edge.Resolution, edge.ID)
		}
	}
}

func graphPathLimitText(path graph.Path) string {
	var limits []string
	if path.DepthLimitReached {
		limits = append(limits, "depth limit")
	}
	if path.NodeLimitReached {
		limits = append(limits, "node limit")
	}
	if len(limits) == 0 {
		return "configured traversal bound"
	}
	return strings.Join(limits, " and ")
}
