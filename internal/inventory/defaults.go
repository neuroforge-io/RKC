package inventory

// DefaultExclusions returns the canonical safe exclusions shared by ordinary
// scans and runtime-evidence affinity capture. The returned slice is a copy so
// callers cannot mutate process-wide policy.
func DefaultExclusions() []string {
	return []string{
		".cache",
		".coverage",
		".git",
		".mypy_cache",
		".pytest_cache",
		".rkc",
		".rkc-coverage",
		".rkc-downloads",
		".rkc-history.json",
		".rkc-models",
		".rkc-runtime",
		".rkc-scip",
		".rkc-state",
		".rkc-trace.json",
		".rkc.rkc-derived",
		".ruff_cache",
		".venv",
		"__pycache__",
		"bin",
		"coverage",
		"coverage.out",
		"coverage.xml",
		"dist",
		"htmlcov",
		"venv",
	}
}
