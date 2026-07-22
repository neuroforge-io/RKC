// Command rkc is the local Repository Knowledge Compiler CLI. It intentionally
// keeps deterministic analysis usable without a daemon, database server, model,
// or network connection.
package main

import (
	"fmt"
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
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "scan":
		return runScan(args[1:])
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
	case "diff":
		return runDiff(args[1:])
	case "snapshots":
		return runSnapshots(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "plugins":
		return runPlugins(args[1:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Print(`Repository Knowledge Compiler

Usage:
  rkc init [flags]
  rkc scan [flags] [repository]
  rkc check [flags]
  rkc query [flags] <query>
  rkc answer [flags] <question>
  rkc synthesize [flags]
  rkc path [flags] --from <node-id> --to <node-id>
  rkc impact [flags] --node <node-id>
  rkc components [flags]
  rkc diff [flags] <before-output> <after-output>
  rkc snapshots <list|show|export|recover> [flags]
  rkc plugins <list|validate|lock|verify> [flags]
  rkc serve [flags]
  rkc doctor [flags]
  rkc version

The reference build implements deterministic inventory, Python, Go, and
JavaScript/TypeScript syntax analysis, framework and document extraction,
evidence-backed graph export, ranked lexical/semantic/hybrid search, bounded
graph operations, semantic snapshot diffing, crash-safe snapshot persistence,
evidence-grounded local-model answers, a llama.cpp CLI provider, a static
browser, and read-only HTTP and MCP interfaces. Model-backed commands fail
closed until exact qualified assets and runtimes are supplied. Enforced WASM
isolation and PostgreSQL team mode remain explicit production milestones.
`)
}

func init() { log.SetFlags(0) }
