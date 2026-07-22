package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/neuroforge-io/RKC/internal/semanticdiff"
)

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "text", "output format: text, json, or markdown")
	failBreaking := fs.Bool("fail-on-breaking", false, "return failure when breaking changes are found")
	failRisk := fs.Bool("fail-on-risk", false, "return failure when risk changes are found")
	maxItems := fs.Int("max-items", 200, "maximum detailed changes printed in text or Markdown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("diff requires before and after RKC output directories")
	}
	before, err := loadDataset(fs.Arg(0))
	if err != nil {
		return err
	}
	after, err := loadDataset(fs.Arg(1))
	if err != nil {
		return err
	}
	report := semanticdiff.Compare(before.Bundle, after.Bundle)
	switch strings.ToLower(*format) {
	case "json":
		if err := writeJSONStdout(report); err != nil {
			return err
		}
	case "markdown", "md":
		printDiffMarkdown(report, *maxItems)
	case "text":
		printDiffText(report, *maxItems)
	default:
		return fmt.Errorf("unknown diff format %q", *format)
	}
	if *failBreaking && report.Summary.BreakingChanges > 0 {
		return fmt.Errorf("semantic diff contains %d breaking change(s)", report.Summary.BreakingChanges)
	}
	if *failRisk && report.Summary.RiskChanges > 0 {
		return fmt.Errorf("semantic diff contains %d risk change(s)", report.Summary.RiskChanges)
	}
	return nil
}

func printDiffText(report semanticdiff.Report, maxItems int) {
	fmt.Printf("Semantic diff %s -> %s\n", report.FromSnapshot, report.ToSnapshot)
	fmt.Printf("Artifacts +%d -%d ~%d | Nodes +%d -%d ~%d | Edges +%d -%d ~%d | Breaking %d | Risk %d\n",
		report.Summary.ArtifactsAdded, report.Summary.ArtifactsRemoved, report.Summary.ArtifactsModified,
		report.Summary.NodesAdded, report.Summary.NodesRemoved, report.Summary.NodesModified,
		report.Summary.EdgesAdded, report.Summary.EdgesRemoved, report.Summary.EdgesModified,
		report.Summary.BreakingChanges, report.Summary.RiskChanges)
	count := 0
	for _, change := range report.Nodes {
		if count >= maxItems {
			fmt.Println("... detail truncated")
			break
		}
		subject := change.LogicalKey
		if change.After != nil && change.After.QualifiedName != "" {
			subject = change.After.QualifiedName
		} else if change.Before != nil && change.Before.QualifiedName != "" {
			subject = change.Before.QualifiedName
		}
		fmt.Printf("  %-9s %-8s %s", change.Severity, change.Kind, subject)
		if len(change.Fields) > 0 {
			fmt.Printf(" fields=%s", strings.Join(change.Fields, ","))
		}
		if len(change.Reasons) > 0 {
			fmt.Printf(" reasons=%s", strings.Join(change.Reasons, "; "))
		}
		fmt.Println()
		count++
	}
}

func printDiffMarkdown(report semanticdiff.Report, maxItems int) {
	fmt.Printf("# Repository semantic diff\n\n")
	fmt.Printf("**From:** `%s`  \n**To:** `%s`\n\n", report.FromSnapshot, report.ToSnapshot)
	fmt.Printf("| Area | Added | Removed | Modified |\n|---|---:|---:|---:|\n")
	fmt.Printf("| Artifacts | %d | %d | %d |\n", report.Summary.ArtifactsAdded, report.Summary.ArtifactsRemoved, report.Summary.ArtifactsModified)
	fmt.Printf("| Nodes | %d | %d | %d |\n", report.Summary.NodesAdded, report.Summary.NodesRemoved, report.Summary.NodesModified)
	fmt.Printf("| Edges | %d | %d | %d |\n\n", report.Summary.EdgesAdded, report.Summary.EdgesRemoved, report.Summary.EdgesModified)
	fmt.Printf("**Breaking:** %d  \n**Risk:** %d\n\n", report.Summary.BreakingChanges, report.Summary.RiskChanges)
	fmt.Println("## Symbol changes")
	fmt.Println()
	fmt.Println("| Severity | Change | Symbol | Fields | Reasons |")
	fmt.Println("|---|---|---|---|---|")
	for index, change := range report.Nodes {
		if index >= maxItems {
			break
		}
		subject := change.LogicalKey
		if change.After != nil && change.After.QualifiedName != "" {
			subject = change.After.QualifiedName
		} else if change.Before != nil && change.Before.QualifiedName != "" {
			subject = change.Before.QualifiedName
		}
		fmt.Printf("| %s | %s | `%s` | %s | %s |\n", change.Severity, change.Kind, escapeCell(subject), escapeCell(strings.Join(change.Fields, ", ")), escapeCell(strings.Join(change.Reasons, "; ")))
	}
}

func escapeCell(value string) string { return strings.ReplaceAll(value, "|", "\\|") }
