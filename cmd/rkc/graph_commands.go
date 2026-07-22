package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/internal/graph"
)

func runPath(args []string) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	from := fs.String("from", "", "source node ID, logical ID, qualified name, or unique name")
	to := fs.String("to", "", "target node ID, logical ID, qualified name, or unique name")
	direction := fs.String("direction", "outgoing", "incoming, outgoing, or both")
	edgeKinds := fs.String("edge-kinds", "", "comma-separated edge kinds")
	resolutions := fs.String("resolutions", "", "comma-separated resolution classes")
	depth := fs.Int("depth", 12, "maximum traversal depth")
	limit := fs.Int("limit", 10000, "maximum visited nodes")
	includeUnresolved := fs.Bool("include-unresolved", false, "include unresolved edges")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("--from and --to are required")
	}
	dataset, err := loadDataset(*dir)
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
	options, err := traversalOptions(*direction, *edgeKinds, *resolutions, *depth, *limit, *includeUnresolved)
	if err != nil {
		return err
	}
	path, err := dataset.Graph.ShortestPath(source.ID, target.ID, options)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(path)
	}
	if !path.Found {
		fmt.Printf("No path found from %s to %s after visiting %d nodes.\n", source.QualifiedName, target.QualifiedName, path.Visited)
		return nil
	}
	fmt.Printf("Path depth %d, visited %d nodes:\n", path.Depth, path.Visited)
	for index, node := range path.Nodes {
		fmt.Printf("  %d. %s [%s] (%s)\n", index+1, displayNode(node.Name, node.QualifiedName), node.Kind, node.ID)
		if index < len(path.Edges) {
			edge := path.Edges[index]
			fmt.Printf("     -> %s [%s, confidence %.2f]\n", edge.Kind, edge.Resolution, edge.Confidence)
		}
	}
	return nil
}

func runImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	nodeReference := fs.String("node", "", "seed node ID, logical ID, qualified name, or unique name")
	direction := fs.String("direction", "incoming", "incoming, outgoing, or both")
	edgeKinds := fs.String("edge-kinds", "", "comma-separated edge kinds")
	resolutions := fs.String("resolutions", "", "comma-separated resolution classes")
	depth := fs.Int("depth", 4, "maximum traversal depth")
	limit := fs.Int("limit", 1000, "maximum visited nodes")
	includeUnresolved := fs.Bool("include-unresolved", false, "include unresolved edges")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nodeReference == "" {
		return errors.New("--node is required")
	}
	dataset, err := loadDataset(*dir)
	if err != nil {
		return err
	}
	seed, err := resolveNode(dataset, *nodeReference)
	if err != nil {
		return err
	}
	options, err := traversalOptions(*direction, *edgeKinds, *resolutions, *depth, *limit, *includeUnresolved)
	if err != nil {
		return err
	}
	result, err := dataset.Graph.Impact(seed.ID, options)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	fmt.Printf("Impact set for %s: %d node(s), %d edge(s), truncated=%t\n", displayNode(seed.Name, seed.QualifiedName), len(result.ImpactedNodes), len(result.ImpactEdges), result.Truncated)
	for _, item := range result.ImpactedNodes {
		fmt.Printf("  depth %-2d %-14s %s (%s)\n", result.DepthByID[item.ID], item.Kind, displayNode(item.Name, item.QualifiedName), item.ID)
	}
	return nil
}

func runComponents(args []string) error {
	fs := flag.NewFlagSet("components", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	edgeKinds := fs.String("edge-kinds", "calls,imports,depends_on", "comma-separated edge kinds")
	cyclesOnly := fs.Bool("cycles-only", false, "show only cyclic components")
	limit := fs.Int("limit", 100, "maximum components")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dataset, err := loadDataset(*dir)
	if err != nil {
		return err
	}
	components := dataset.Graph.StronglyConnectedComponents(splitSet(*edgeKinds))
	filtered := components[:0]
	for _, component := range components {
		if *cyclesOnly && !component.Cyclic {
			continue
		}
		filtered = append(filtered, component)
		if len(filtered) >= *limit {
			break
		}
	}
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"items": filtered, "total": len(components), "truncated": len(filtered) < len(components)})
	}
	fmt.Printf("%d component(s) shown from %d total; edge kinds: %s\n", len(filtered), len(components), strings.Join(keys(splitSet(*edgeKinds)), ","))
	for _, component := range filtered {
		fmt.Printf("  component %d: nodes=%d cyclic=%t\n", component.ID, len(component.NodeIDs), component.Cyclic)
		for _, id := range component.NodeIDs {
			node := dataset.Graph.Nodes[id]
			fmt.Printf("    %-14s %s\n", node.Kind, displayNode(node.Name, node.QualifiedName))
		}
	}
	return nil
}

func displayNode(name, qualified string) string {
	if qualified != "" {
		return qualified
	}
	return name
}

func keys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	// The graph package sorts all canonical outputs. This list is presentation,
	// but sorting it still prevents command output from twitching pointlessly.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j] < result[i] {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

var _ = graph.DirectionBoth
