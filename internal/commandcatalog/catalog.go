// Package commandcatalog defines the user-facing RKC command workflows shared
// by the generated browser and the protected local workbench.
package commandcatalog

// Context binds command defaults to the atlas selected by the serving process.
// Zero values produce portable copy-and-edit examples for a static export.
type Context struct {
	DatasetArgs []string
	CheckArgs   []string
}

// Mode describes whether a workflow is read-only, writes local state, or can
// invoke a separately qualified model runtime.
type Mode string

const (
	// ModeRead marks workflows that consume existing state without publishing it.
	ModeRead Mode = "read"
	// ModeWrites marks workflows that may publish local RKC state or artifacts.
	ModeWrites Mode = "writes"
	// ModeModel marks workflows that may invoke an explicitly qualified model.
	ModeModel Mode = "model"
)

// Command is one guided workflow exposed by the browser command center.
type Command struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        Mode     `json:"mode"`
	DefaultArgs []string `json:"default_args"`
	Guidance    string   `json:"guidance"`
}

const (
	generalGuidance  = "The preview is the exact argument vector. Quotes and backslash escapes are supported, and no shell is used."
	semanticGuidance = "Add --scip-index /path/index.scip for compiler-grade semantics; repeat it for polyglot repositories. The preview is exact and no shell is used."
)

// Commands returns an independent, ordered catalogue with valid CLI argument
// vectors. Dataset-reading defaults target the exact served selection when the
// caller supplies one; static exports use the conventional .rkc directory.
func Commands(context Context) []Command {
	dataset := append([]string(nil), context.DatasetArgs...)
	if len(dataset) == 0 {
		dataset = []string{"--dir", ".rkc"}
	}
	check := append([]string(nil), context.CheckArgs...)
	if len(check) == 0 {
		check = []string{"--coverage", ".rkc/coverage.json"}
	}
	withDataset := func(tail ...string) []string {
		result := append([]string(nil), dataset...)
		return append(result, tail...)
	}
	return []Command{
		{"wizard", "Launch the guided terminal first run.", ModeWrites, []string{"--help"}, "The interactive guide requires a terminal; this safe default shows its help. It covers common first-run workflows, not every CLI option."},
		{"quickstart", "Build and verify a ready-to-search atlas.", ModeWrites, []string{"."}, semanticGuidance},
		{"init", "Create or preview a complete local configuration.", ModeWrites, []string{"--stdout"}, generalGuidance},
		{"doctor", "Diagnose repository and optional capabilities.", ModeRead, []string{"--repository", "."}, generalGuidance},
		{"plan", "Preview the stage DAG, SCIP input, and cache decisions.", ModeRead, []string{"."}, semanticGuidance},
		{"scan", "Compile with optional compiler-grade SCIP semantics.", ModeWrites, []string{"--no-python", "--out", ".rkc", "--state-dir", ".rkc-state", "."}, semanticGuidance},
		{"check", "Enforce coverage, integrity, and security gates.", ModeRead, check, generalGuidance},
		{"query", "Search the selected compiled repository atlas.", ModeRead, withDataset("resource guard"), generalGuidance},
		{"answer", "Produce a citation-checked answer with a qualified model.", ModeModel, withDataset("--repair-passes", "2", "How does this repository work?"), generalGuidance},
		{"synthesize", "Build bounded evidence packets or use a qualified model.", ModeModel, append([]string{"--packet-only=true"}, withDataset("--query", "How does this repository work?")...), generalGuidance},
		{"path", "Find a bounded path between graph nodes.", ModeRead, []string{"--help"}, generalGuidance},
		{"impact", "Traverse bounded impact relationships.", ModeRead, []string{"--help"}, generalGuidance},
		{"components", "List strongly connected components.", ModeRead, withDataset(), generalGuidance},
		{"counterfactual", "Compare a graph route after a bounded hypothetical intervention.", ModeRead, []string{"--help"}, "Choose source and target nodes, then omit one or more exact nodes or edges. Results are non-authoritative structural hypotheses within explicit traversal bounds, never claims of runtime causation. The preview is exact and no shell is used."},
		{"diff", "Compare two compiled snapshots.", ModeRead, []string{"--help"}, generalGuidance},
		{"snapshots", "Inspect, export, select, or recover snapshots.", ModeWrites, []string{"list", "--help"}, generalGuidance},
		{"runs", "Inspect validated scheduler run journals.", ModeRead, []string{"list", "--help"}, generalGuidance},
		{"plugins", "Inspect, validate, lock, or verify plugins.", ModeWrites, []string{"list", "--help"}, generalGuidance},
		{"scip", "Generate, verify, or pin compiler-grade SCIP indexes.", ModeWrites, []string{"languages"}, "Add --scip-index /path/index.scip to scan/quickstart/open, or let them generate indexes with --scip-generate <language>. Pin indexers first with 'rkc scip pin'. The preview is exact and no shell is used."},
		{"flow", "Trace value-flow origins, sinks, paths, and environment reads.", ModeRead, append([]string{"report"}, dataset...), "Every atlas carries bounded call graphs, CFGs, and value-flow edges. Inspect deterministic sources and sinks, bounded reachability, environment readers, and separately labelled non-authoritative sanitizer-name hypotheses. The preview is exact and no shell is used."},
		{"trace", "Capture, verify, and report bounded runtime assertions.", ModeWrites, append([]string{"report"}, dataset...), "Capture an authorized Go test run into a source-affine digest-bound trace and import it with scan --trace. Current traces are operator assertions: they do not prove producer identity, call execution, dead code, or missing tests. The preview is exact and no shell is used."},
		{"history", "Compile Git history into semantic symbol deltas.", ModeRead, []string{"--help"}, "History reporting requires an explicit compiled history file. Use the guided help to build one from a repository, then import it with scan --history. The preview is exact and no shell is used."},
		{"cache", "Inspect, verify, or prune the stage cache.", ModeWrites, []string{"inspect", "--help"}, generalGuidance},
		{"version", "Print the RKC version.", ModeRead, []string{}, generalGuidance},
		{"help", "Show the complete command overview.", ModeRead, []string{}, generalGuidance},
	}
}
