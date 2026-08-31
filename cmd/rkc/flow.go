package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/neuroforge-io/RKC/internal/server"
	"os"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/flow"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func runFlow(args []string) error {
	if len(args) == 0 {
		return flowUsage()
	}
	switch args[0] {
	case "report":
		return runFlowReport(args[1:])
	case "origins":
		return runFlowLineage(args[1:], true)
	case "sinks":
		return runFlowLineage(args[1:], false)
	case "path":
		return runFlowPath(args[1:])
	case "env":
		return runFlowEnv(args[1:])
	case "help", "--help", "-h":
		return flowUsage()
	default:
		return fmt.Errorf("unknown flow subcommand %q; run 'rkc flow help'", args[0])
	}
}

func flowUsage() error {
	_, err := fmt.Fprint(os.Stdout, `Interprocedural control-flow and value-flow evidence for one atlas.

  rkc flow report --dir <atlas> [--json]
  rkc flow origins --dir <atlas> --node <id> [--json]
  rkc flow sinks   --dir <atlas> --node <id> [--json]
  rkc flow path    --dir <atlas> --from <id> --to <id> [--json]
  rkc flow env     --dir <atlas> --name <NAME> [--json]

The value-flow stage compiles bounded call graphs, per-function CFGs, and
value-flow edges (flows_to, binds_to, returns_to, reads) into every atlas during
the scan. These commands read deterministic sources, sinks, bounded
reachability, and environment readers. Sanitizer-like names are reported only
as low-confidence, non-authoritative hypotheses and never as proven protection.
`)
	return err
}

func flowDatasetFlags(fs *flag.FlagSet) (*string, *string, *string, *string) {
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	return dir, database, snapshotID, repositoryID
}

func flowDataset(fs *flag.FlagSet, args []string, dir, database, snapshotID, repositoryID *string) (*server.Dataset, error) {
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("flow does not accept positional arguments")
	}
	dataset, err := loadSelectedDataset(context.Background(), *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return nil, err
	}
	return dataset, nil
}

func runFlowReport(args []string) error {
	fs := flag.NewFlagSet("flow report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir, database, snapshotID, repositoryID := flowDatasetFlags(fs)
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	dataset, err := flowDataset(fs, args, dir, database, snapshotID, repositoryID)
	if err != nil {
		return err
	}
	report := flow.BuildReport(dataset.Bundle)
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	fmt.Printf("Flow report for snapshot %s\n", dataset.Bundle.Snapshot.ID)
	fmt.Printf("Call graph: %d edges, %d resolved, %d targets\n", report.CallEdges, report.CallEdgesResolved, report.CallEdgeTargets)
	fmt.Printf("Control flow: %d cfg blocks, %d precedes edges\n", report.CFGBlocks, report.CFGEdges)
	fmt.Printf("Value flow: %d value nodes, %d flows_to, %d binds_to, %d returns_to, %d authoritative sanitizes\n",
		report.ValueNodes, report.FlowEdges, report.BindsToEdges, report.ReturnsToEdges, report.SanitizeEdges)
	fmt.Printf("Environment: %d reads across %d variables\n", report.EnvReads, len(report.EnvironmentVariables))
	for _, name := range report.EnvironmentVariables {
		fmt.Printf("  ENV %s\n", name)
	}
	fmt.Printf("Sources (%d): %s\n", len(report.Sources), strings.Join(report.Sources, ", "))
	fmt.Printf("Sinks (%d): %s\n", len(report.Sinks), strings.Join(report.Sinks, ", "))
	fmt.Printf("Sanitizers (%d): %s\n", len(report.Sanitizers), strings.Join(report.Sanitizers, ", "))
	fmt.Printf("Non-authoritative sanitizer-name hypotheses (%d): %s\n",
		len(report.SanitizerHypotheses), strings.Join(report.SanitizerHypotheses, ", "))
	fmt.Printf("Source-to-sink paths (%d):\n", len(report.SourceSinkPaths))
	for _, path := range report.SourceSinkPaths {
		fmt.Printf("  %s -> %s (%d nodes)\n", path.Source, path.Sink, path.Nodes)
	}
	if report.Bounded {
		fmt.Println("Note: the report hit its bounded path limit; results are truncated.")
	}
	return nil
}

func runFlowLineage(args []string, origins bool) error {
	fs := flag.NewFlagSet("flow lineage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir, database, snapshotID, repositoryID := flowDatasetFlags(fs)
	node := fs.String("node", "", "node ID, logical ID, qualified name, or unique name")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	dataset, err := flowDataset(fs, args, dir, database, snapshotID, repositoryID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*node) == "" {
		return errors.New("--node is required")
	}
	resolved, err := resolveNode(dataset, *node)
	if err != nil {
		return err
	}
	var result flow.LineageResult
	if origins {
		result, err = flow.Origins(dataset.Bundle, resolved.ID)
	} else {
		result, err = flow.Sinks(dataset.Bundle, resolved.ID)
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	kind := "origins"
	if !origins {
		kind = "sinks"
	}
	fmt.Printf("%s of %s (%s):\n", strings.Title(kind), resolved.QualifiedName, resolved.Kind)
	if len(result.Nodes) == 0 {
		fmt.Println("  (no value-flow edges reach this node)")
	}
	for _, nodeID := range result.Nodes {
		if nodeID == resolved.ID {
			continue
		}
		label := flowNodeLabel(dataset.Bundle, nodeID)
		fmt.Printf("  %s\n", label)
	}
	if len(result.Roles) > 0 {
		keys := make([]string, 0, len(result.Roles))
		for role := range result.Roles {
			keys = append(keys, role)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, role := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", role, result.Roles[role]))
		}
		fmt.Printf("roles: %s\n", strings.Join(parts, " "))
	}
	if result.Bounded {
		fmt.Println("note: lineage reached its bounded walk limit.")
	}
	return nil
}

func flowNodeLabel(bundle rkcmodel.Bundle, id string) string {
	for _, node := range bundle.Nodes {
		if node.ID != id {
			continue
		}
		role, _ := node.Attributes["flow_role"].(string)
		label := node.QualifiedName
		if node.Kind == "environment_variable" {
			return "ENV " + node.Name
		}
		if variable, ok := node.Attributes["environment_variable"].(string); ok {
			return "ENV " + variable + " -> " + label
		}
		if sinkVia, ok := node.Attributes["sink_via"].(string); ok {
			return label + " [sink via " + sinkVia + "]"
		}
		if role != "" {
			return label + " [" + role + "]"
		}
		return label
	}
	return id
}

func runFlowPath(args []string) error {
	fs := flag.NewFlagSet("flow path", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir, database, snapshotID, repositoryID := flowDatasetFlags(fs)
	from := fs.String("from", "", "source node ID, logical ID, qualified name, or unique name")
	to := fs.String("to", "", "target node ID, logical ID, qualified name, or unique name")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	dataset, err := flowDataset(fs, args, dir, database, snapshotID, repositoryID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		return errors.New("--from and --to are required")
	}
	source, err := resolveNode(dataset, *from)
	if err != nil {
		return err
	}
	target, err := resolveNode(dataset, *to)
	if err != nil {
		return err
	}
	path, err := flow.Path(dataset.Bundle, source.ID, target.ID)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(path)
	}
	if !path.Found {
		fmt.Printf("No value-flow path from %s to %s.\n", source.QualifiedName, target.QualifiedName)
		if path.Bounded {
			fmt.Println("note: the search hit its bounded walk limit.")
		}
		return nil
	}
	fmt.Printf("Value-flow path (%d nodes, %d edges):\n", len(path.Nodes), len(path.Edges))
	for index, nodeID := range path.Nodes {
		arrow := ""
		if index < len(path.Edges) {
			arrow = "  --" + path.Edges[index] + "-->"
		}
		fmt.Printf("  %s %s\n", flowNodeLabel(dataset.Bundle, nodeID), arrow)
	}
	return nil
}

func runFlowEnv(args []string) error {
	fs := flag.NewFlagSet("flow env", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir, database, snapshotID, repositoryID := flowDatasetFlags(fs)
	name := fs.String("name", "", "environment variable name")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	dataset, err := flowDataset(fs, args, dir, database, snapshotID, repositoryID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	// Find the environment variable node, its readers, and their callers.
	envID := rkcmodel.StableID("node", "environment_variable", *name)
	envExists := false
	readers := map[string]struct{}{}
	for _, node := range dataset.Bundle.Nodes {
		if node.ID == envID {
			envExists = true
		}
	}
	for _, edge := range dataset.Bundle.Edges {
		if edge.Kind == "reads" && edge.From == envID {
			readers[edge.To] = struct{}{}
		}
	}
	result := struct {
		Environment string   `json:"environment"`
		Found       bool     `json:"found"`
		Readers     []string `json:"readers"`
		Callers     []string `json:"callers"`
	}{Environment: *name, Found: envExists}
	readerList := make([]string, 0, len(readers))
	for reader := range readers {
		readerList = append(readerList, reader)
	}
	sort.Strings(readerList)
	callers := map[string]struct{}{}
	for _, reader := range readerList {
		result.Readers = append(result.Readers, flowNodeLabel(dataset.Bundle, reader))
		for _, edge := range dataset.Bundle.Edges {
			if edge.Kind == "calls" && edge.To == reader {
				callers[flowNodeLabel(dataset.Bundle, edge.From)] = struct{}{}
			}
		}
	}
	for caller := range callers {
		result.Callers = append(result.Callers, caller)
	}
	sort.Strings(result.Callers)
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	if !envExists {
		fmt.Printf("Environment variable %s is not referenced by this atlas.\n", *name)
		return nil
	}
	fmt.Printf("Environment variable %s\n", *name)
	fmt.Printf("Readers (%d):\n", len(result.Readers))
	for _, reader := range result.Readers {
		fmt.Printf("  %s\n", reader)
	}
	fmt.Printf("Callers (%d):\n", len(result.Callers))
	for _, caller := range result.Callers {
		fmt.Printf("  %s\n", caller)
	}
	if len(result.Callers) == 0 {
		fmt.Println("note: readers have no resolved callers in the atlas.")
	}
	return nil
}

var _ = os.Getenv
