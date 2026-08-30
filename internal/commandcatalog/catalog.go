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
		{"diff", "Compare two compiled snapshots.", ModeRead, []string{"--help"}, generalGuidance},
		{"snapshots", "Inspect, export, select, or recover snapshots.", ModeWrites, []string{"list", "--help"}, generalGuidance},
		{"runs", "Inspect validated scheduler run journals.", ModeRead, []string{"list", "--help"}, generalGuidance},
		{"plugins", "Inspect, validate, lock, or verify plugins.", ModeWrites, []string{"list", "--help"}, generalGuidance},
		{"cache", "Inspect, verify, or prune the stage cache.", ModeWrites, []string{"inspect", "--help"}, generalGuidance},
		{"version", "Print the RKC version.", ModeRead, []string{}, generalGuidance},
		{"help", "Show the complete command overview.", ModeRead, []string{}, generalGuidance},
	}
}
