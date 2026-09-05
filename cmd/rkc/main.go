// Package main contains the local Repository Knowledge Compiler CLI and its
// command implementations.
//
// Command rkc is the local Repository Knowledge Compiler CLI. It intentionally
// keeps deterministic analysis usable without a daemon, database server, model,
// or network connection.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
)

var version = "0.3.0-reference"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rkc:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	err := dispatch(args)
	// The standard flag package returns ErrHelp after printing a command's
	// usage. Help is a successful user request, not a runtime failure.
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func dispatch(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "context":
		return runContext(args[1:])
	case "capabilities":
		return runCapabilities(args[1:])
	case "knowledge":
		return runKnowledge(args[1:])
	case "wizard", "tui":
		return runWizard(args[1:])
	case "scan":
		return runScan(args[1:])
	case "quickstart":
		return runQuickstart(args[1:])
	case "gui":
		return runOpen(append([]string{"--welcome", "--workbench"}, args[1:]...))
	case "open", "start":
		return runOpen(args[1:])
	case "plan":
		return runPlan(args[1:])
	case "serve":
		return runServe(args[1:])
	case "check":
		return runCheck(args[1:])
	case "init":
		return runInit(args[1:])
	case "query", "search":
		return runQuery(args[1:])
	case "answer", "ask":
		return runAnswer(args[1:])
	case "synthesize", "explain":
		return runSynthesize(args[1:])
	case "path":
		return runPath(args[1:])
	case "impact":
		return runImpact(args[1:])
	case "components":
		return runComponents(args[1:])
	case "counterfactual":
		return runCounterfactual(args[1:])
	case "diff":
		return runDiff(args[1:])
	case "snapshots":
		return runSnapshots(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "plugins":
		return runPlugins(args[1:])
	case "flow":
		return runFlow(args[1:])
	case "trace":
		return runTrace(args[1:])
	case "history":
		return runHistory(args[1:])
	case "scip":
		return runScip(args[1:])
	case "cache":
		return runCache(args[1:])
	case "runs":
		return runRuns(args[1:])
	case "version", "--version", "-version":
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		if len(args) != 1 {
			return errors.New("help does not accept arguments; use 'rkc <command> --help'")
		}
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q; run 'rkc help' to list commands", args[0])
	}
}

func printUsage() {
	_ = printUsageTo(os.Stdout)
}

func printUsageTo(output io.Writer) error {
	_, err := fmt.Fprint(output, `NeuroForgeIO · Repository Knowledge Compiler
Apache-2.0 open source. Copyright 2026 NeuroForgeIO and RKC contributors.
Redistributions must preserve the applicable LICENSE and NOTICE terms;
third-party materials retain their own licenses and ownership.

Usage:
  rkc <command> [options]

Get started:
  rkc wizard
  rkc open .
  rkc quickstart .
  rkc doctor --repository .
  rkc init --path rkc.json
  rkc scan --config rkc.json --no-python --out .rkc --state-dir .rkc-state .
  rkc serve --dir .rkc

Core commands:
  wizard       Guided terminal first run (alias: tui)
  gui          Open a workspace and choose a source in the browser
  open         Compile, verify, and open a local browser atlas (alias: start)
  quickstart   Build and verify a ready-to-search atlas in one command
  init         Generate a complete, safe local configuration
  doctor       Diagnose configuration and optional local capabilities
  plan         Preview the stage DAG, cache reuse, and invalidation reasons
  scan         Compile a local directory or remote Git repository
  check        Enforce coverage, integrity, and security quality gates
  serve        Browse an atlas through the read-only local HTTP server

Explore and explain:
  context      Retrieve a cited context packet as JSON or Markdown
  query        Search the compiled repository atlas (alias: search)
  answer       Produce a citation-checked answer (alias: ask)
  synthesize   Build evidence packets or run a qualified local model
  path         Find a bounded graph path between two nodes
  impact       Traverse bounded impact relationships from one node
  components   List strongly connected graph components
  counterfactual Compare a route after bounded hypothetical node/edge removal
  diff         Compare two compiled atlas snapshots

Storage and extension:
  capabilities Discover machine-readable interfaces, workflows, and outputs
  knowledge    Build or verify portable packs from one or more compiled atlases
  snapshots    List, show, export, select, or recover snapshots
  plugins      List, validate, lock, or verify plugin manifests
  scip         Generate, verify, or pin compiler-grade SCIP indexes
  flow         Trace value-flow origins, sinks, paths, and environment reads
  trace        Capture, verify, and report runtime execution evidence
  history      Compile Git history into semantic symbol deltas
  cache        Inspect, verify, or prune the incremental stage cache
  runs         List or strictly inspect durable scheduler run journals

Other:
  version      Print the RKC version
  help         Show this help

Run 'rkc <command> --help' for command-specific flags. Lexical search and all
deterministic compilation paths work without a model. Model-backed commands
fail closed until exact qualified assets and runtimes are supplied. The Python
adapter additionally requires its Linux user-systemd isolation boundary; use
'scan --no-python' for the portable deterministic profile.
`)
	return err
}

func init() { log.SetFlags(0) }
