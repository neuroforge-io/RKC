package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func writeJSONStdout(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
func loadDataset(dir string) (*server.Dataset, error) { return server.Load(dir) }

func resolveNode(dataset *server.Dataset, reference string) (rkcmodel.Node, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return rkcmodel.Node{}, errors.New("node reference is empty")
	}
	if node, ok := dataset.NodeByID[reference]; ok {
		return node, nil
	}
	var matches []rkcmodel.Node
	for _, node := range dataset.Bundle.Nodes {
		if node.LogicalID == reference || node.QualifiedName == reference || node.Name == reference {
			matches = append(matches, node)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return rkcmodel.Node{}, fmt.Errorf("node not found: %s", reference)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	ids := make([]string, 0, len(matches))
	for _, item := range matches {
		ids = append(ids, item.ID)
	}
	return rkcmodel.Node{}, fmt.Errorf("node reference %q is ambiguous: %s", reference, strings.Join(ids, ", "))
}

func splitSet(value string) map[string]struct{} {
	var result map[string]struct{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			if result == nil {
				result = map[string]struct{}{}
			}
			result[item] = struct{}{}
		}
	}
	return result
}

func traversalOptions(direction, edgeKinds, resolutions string, depth, limit int, includeUnresolved bool) (graph.TraverseOptions, error) {
	var dir graph.Direction
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "incoming":
		dir = graph.DirectionIncoming
	case "outgoing", "":
		dir = graph.DirectionOutgoing
	case "both":
		dir = graph.DirectionBoth
	default:
		return graph.TraverseOptions{}, fmt.Errorf("invalid direction %q", direction)
	}
	if depth < 0 {
		return graph.TraverseOptions{}, errors.New("depth must be non-negative")
	}
	if limit <= 0 {
		return graph.TraverseOptions{}, errors.New("limit must be positive")
	}
	return graph.TraverseOptions{Direction: dir, EdgeKinds: splitSet(edgeKinds), Resolutions: splitSet(resolutions), MaxDepth: depth, MaxNodes: limit, IncludeUnresolved: includeUnresolved}, nil
}
