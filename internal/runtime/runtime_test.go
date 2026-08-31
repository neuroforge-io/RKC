package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestParseCoverageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	content := `mode: set
example.com/demo/demo.go:3.14,5.2 2 1
example.com/demo/demo.go:7.1,9.30 3 0
/usr/local/go/src/fmt/print.go:1.1,2.2 1 1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := parseCoverageFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1 (absolute paths excluded)", len(artifacts))
	}
	artifact := artifacts[0]
	if artifact.Path != "example.com/demo/demo.go" || artifact.Statements != 5 || artifact.ExecutedStatements != 2 {
		t.Fatalf("artifact = %+v", artifact)
	}
	if len(artifact.ExecutedRanges) != 1 || artifact.ExecutedRanges[0].StartLine != 3 || artifact.ExecutedRanges[0].EndLine != 5 {
		t.Fatalf("ranges = %+v", artifact.ExecutedRanges)
	}
}

func TestParseCoverageRejectsMalformed(t *testing.T) {
	for name, content := range map[string]string{
		"no colon":  "hello world\n",
		"bad range": "example.com/demo.go:1.1 2 1\n",
		"bad count": "example.com/demo.go:1.1,2.2 x 1\n",
		"bad line":  "example.com/demo.go:a.1,2.2 2 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "coverage.out")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := parseCoverageFile(path); err == nil {
				t.Fatalf("malformed coverage %q was accepted", name)
			}
		})
	}
}

func TestParseGoTestJSON(t *testing.T) {
	output := `{"Action":"run","Package":"example.com/z","Test":"TestAlpha"}
{"Action":"output","Package":"example.com/z","Test":"TestAlpha","Output":"ok\n"}
{"Action":"pass","Package":"example.com/z","Test":"TestAlpha","Elapsed":0.01}
{"Action":"run","Package":"example.com/z","Test":"TestBeta"}
{"Action":"fail","Package":"example.com/z","Test":"TestBeta","Elapsed":0.02}
{"Action":"skip","Package":"example.com/z","Test":"TestGamma"}
{"Action":"pass","Package":"example.com/a","Test":"TestAlpha","Elapsed":0.03}
{"Action":"output","Output":"PASS\n"}
`
	tests, err := parseGoTestJSON([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 4 {
		t.Fatalf("tests = %d, want 4", len(tests))
	}
	if tests[0].Package != "example.com/a" || tests[0].Name != "TestAlpha" || tests[0].Status != "pass" || tests[0].Elapsed != 30 {
		t.Fatalf("test = %+v", tests[0])
	}
	if tests[2].Status != "fail" || tests[3].Status != "skip" {
		t.Fatalf("statuses = %+v", tests)
	}
}

func TestCaptureGoTest(t *testing.T) {
	root := t.TempDir()
	captureTemp := t.TempDir()
	t.Setenv("TMPDIR", captureTemp)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/cap\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cap.go"), []byte("package cap\n\nfunc Alpha() string { return \"alpha\" }\n\nfunc Beta() string { return Alpha() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cap_test.go"), []byte("package cap\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {\n\tif Alpha() != \"alpha\" { t.Fatal(\"bad\") }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Capture(context.Background(), CaptureOptions{
		Repository: root, Command: []string{"go", "test", "./..."},
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace.Artifacts) == 0 {
		t.Fatal("capture produced no coverage artifacts")
	}
	if len(result.Trace.Tests) != 1 || result.Trace.Tests[0].Status != "pass" {
		t.Fatalf("captured tests = %+v", result.Trace.Tests)
	}
	if len(result.Trace.ID) != 64 {
		t.Fatalf("trace id = %q", result.Trace.ID)
	}
	if result.Trace.Repository.RepositoryID == "" || !validDigest(result.Trace.Repository.ContentDigest) || result.Trace.Repository.ArtifactCount == 0 {
		t.Fatalf("repository affinity = %+v", result.Trace.Repository)
	}
	for _, artifact := range result.Trace.Artifacts {
		if strings.Contains(artifact.Path, "example.com/cap/") || !validDigest(artifact.SourceSHA256) {
			t.Fatalf("coverage source was not canonically bound: %+v", artifact)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifact.Path)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if artifact.SourceSHA256 != hex.EncodeToString(sum[:]) || artifact.SourceSizeBytes != int64(len(data)) {
			t.Fatalf("coverage source identity is stale: %+v", artifact)
		}
	}
	encoded, err := json.Marshal(result.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "./...") {
		t.Fatal("trace persisted command arguments")
	}
	if err := Validate(result.Trace); err != nil {
		t.Fatal(err)
	}
	leftovers, err := filepath.Glob(filepath.Join(captureTemp, "rkc-trace-coverage-*.out"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("coverage temp files were not removed: %v", leftovers)
	}
}

func TestCaptureRejectsExplicitInstrumentation(t *testing.T) {
	root := t.TempDir()
	if _, err := Capture(context.Background(), CaptureOptions{
		Repository: root, Command: []string{"go", "test", "-coverprofile=/tmp/x.out"},
	}); err == nil {
		t.Fatal("explicit -coverprofile was overwritten")
	}
	if _, err := Capture(context.Background(), CaptureOptions{
		Repository: root, Command: []string{"go", "test", "-json"},
	}); err == nil {
		t.Fatal("explicit -json was overwritten")
	}
	if _, err := Capture(context.Background(), CaptureOptions{
		Repository: "", Command: []string{"true"},
	}); err == nil {
		t.Fatal("empty repository was accepted")
	}
	if _, err := Capture(context.Background(), CaptureOptions{
		Repository: root, Command: nil,
	}); err == nil {
		t.Fatal("empty command was accepted")
	}
}

func traceFixture(t *testing.T) (rkcmodel.Bundle, Trace) {
	t.Helper()
	artifactID := rkcmodel.StableID("artifact", "demo.go")
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{RepositoryID: rkcmodel.StableID("repository", "runtime-test"), Git: rkcmodel.GitInfo{Unavailable: true}},
		Artifacts: []rkcmodel.Artifact{{
			ID: artifactID, Path: "demo.go", Kind: "source", Language: "go",
			SizeBytes: 100, SHA256: strings.Repeat("d", 64), Text: true, Status: "syntax_parsed",
		}},
		Nodes: []rkcmodel.Node{
			{ID: "fn-a", Kind: "function", Name: "Alpha", QualifiedName: "demo.Alpha", Language: "go", ArtifactID: artifactID,
				Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 3, EndLine: 5}},
			{ID: "fn-b", Kind: "function", Name: "Beta", QualifiedName: "demo.Beta", Language: "go", ArtifactID: artifactID,
				Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 7, EndLine: 9}},
		},
		Edges: []rkcmodel.Edge{
			{ID: "edge-b-calls-a", Kind: "calls", From: "fn-b", To: "fn-a", Resolution: "syntax_inferred", Attributes: map[string]any{"span": map[string]any{"path": "demo.go", "start_line": 8, "end_line": 8}}},
		},
	}
	trace := sealTraceForBundle(Trace{
		SchemaVersion: SchemaVersion,
		Command:       "go",
		ExitCode:      0,
		Artifacts: []TraceArtifact{{
			Path: "demo.go", Statements: 4, ExecutedStatements: 2,
			ExecutedRanges: []ExecutedRange{{StartLine: 3, EndLine: 5, Count: 1}},
		}},
		Tests: []TraceTest{{Name: "TestAlpha", Status: "pass"}},
	}, &bundle)
	return bundle, trace
}

func TestImportKeepsCoverageClaimsTraceScoped(t *testing.T) {
	bundle, trace := traceFixture(t)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerObservedFunctions != 0 || stats.AssertedFunctions != 1 || stats.FunctionsNotObserved != 1 {
		t.Fatalf("function truth boundary = %+v", stats)
	}
	assertedByID := map[string]bool{}
	notObservedByID := map[string]bool{}
	for _, node := range bundle.Nodes {
		if node.Attributes["executed"] == true {
			t.Fatalf("coverage assertion established execution truth: %+v", node)
		}
		assertedByID[node.ID] = hasStringValues(node.Attributes["execution_asserted_trace_ids"])
		notObservedByID[node.ID] = hasStringValues(node.Attributes["execution_not_observed_trace_ids"])
	}
	if !assertedByID["fn-a"] || assertedByID["fn-b"] || notObservedByID["fn-a"] || !notObservedByID["fn-b"] {
		t.Fatalf("trace-scoped claims: asserted=%v not-observed=%v", assertedByID, notObservedByID)
	}
	// Statement coverage is never a call event, regardless of which spans it
	// claims were covered.
	observed := false
	for _, edge := range bundle.Edges {
		if value, ok := edge.Attributes["observed"].(bool); ok && value {
			observed = true
		}
	}
	if observed {
		t.Fatal("edge from an unexecuted caller was observed")
	}
	if stats.ProducerObservedCallEdges != 0 || stats.UndemonstratedCallEdges != 1 {
		t.Fatalf("call observation = %d/%d", stats.ProducerObservedCallEdges, stats.UndemonstratedCallEdges)
	}
	if stats.ProducerAuthenticatedEvidence != 0 || stats.AssertionEvidence != 5 || !stats.CaptureIntegrityAuthenticated ||
		stats.ProducerAuthenticated || stats.ProducerAuthenticatedPaths != 0 || stats.TraceTestClaims != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	for _, evidence := range bundle.Evidence {
		if evidence.Kind != "user_asserted" || evidence.Confidence != 0.5 || evidence.Attributes["producer_authenticated"] == true {
			t.Fatalf("current capture crossed the runtime truth boundary: %+v", evidence)
		}
	}
	if len(bundle.Paths) != 0 {
		t.Fatal("aggregate coverage invented a per-test execution path")
	}
}

func TestImportMultiHopExecutionPath(t *testing.T) {
	artifactID := rkcmodel.StableID("artifact", "demo.go")
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: artifactID, Path: "demo.go", Kind: "source", Language: "go", SizeBytes: 200, SHA256: strings.Repeat("d", 64), Text: true, Status: "syntax_parsed"}},
		Nodes: []rkcmodel.Node{
			{ID: "fn-a", Kind: "function", Name: "A", QualifiedName: "demo.A", Language: "go", ArtifactID: artifactID, Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 3, EndLine: 5}},
			{ID: "fn-b", Kind: "function", Name: "B", QualifiedName: "demo.B", Language: "go", ArtifactID: artifactID, Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 7, EndLine: 9}},
			{ID: "fn-c", Kind: "function", Name: "C", QualifiedName: "demo.C", Language: "go", ArtifactID: artifactID, Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 11, EndLine: 13}},
		},
		Edges: []rkcmodel.Edge{
			{ID: "b-calls-c", Kind: "calls", From: "fn-b", To: "fn-c", Resolution: "syntax_inferred", Attributes: map[string]any{"span": map[string]any{"path": "demo.go", "start_line": 8, "end_line": 8}}},
			{ID: "c-calls-a", Kind: "calls", From: "fn-c", To: "fn-a", Resolution: "syntax_inferred", Attributes: map[string]any{"span": map[string]any{"path": "demo.go", "start_line": 12, "end_line": 12}}},
		},
	}
	trace := sealTraceForBundle(Trace{
		SchemaVersion: SchemaVersion, Command: "go", ExitCode: 0,
		Artifacts: []TraceArtifact{{
			Path: "demo.go", Statements: 6, ExecutedStatements: 6,
			ExecutedRanges: []ExecutedRange{
				{StartLine: 3, EndLine: 5, Count: 1},
				{StartLine: 7, EndLine: 9, Count: 1},
				{StartLine: 11, EndLine: 13, Count: 1},
			},
		}},
		Tests: []TraceTest{{Name: "TestB", Status: "pass"}},
	}, &bundle)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerObservedFunctions != 0 || stats.AssertedFunctions != 3 || stats.ProducerObservedCallEdges != 0 ||
		stats.UndemonstratedCallEdges != 2 || stats.CallObservationAvailable {
		t.Fatalf("stats = %+v", stats)
	}
	if len(bundle.Paths) != 0 {
		t.Fatal("aggregate coverage invented an ordered call path")
	}
}

func TestImportExecutedCallerEdge(t *testing.T) {
	bundle, trace := traceFixture(t)
	// Cover Beta too. Statement coverage still cannot prove the dynamic call.
	bundle.Artifacts[0].Path = "demo.go"
	trace.Artifacts[0].ExecutedRanges = []ExecutedRange{
		{StartLine: 3, EndLine: 5, Count: 1},
		{StartLine: 7, EndLine: 9, Count: 1},
	}
	trace = sealTrace(trace)
	markCurrentProcessCaptureIntegrity(trace)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerObservedCallEdges != 0 || stats.UndemonstratedCallEdges != 1 || stats.CallObservationAvailable {
		t.Fatalf("call observation = %d/%d", stats.ProducerObservedCallEdges, stats.UndemonstratedCallEdges)
	}
	for _, edge := range bundle.Edges {
		if edge.Kind == "calls" {
			if edge.Attributes["observed"] == true {
				t.Fatalf("statement coverage promoted a call edge: %+v", edge)
			}
		}
	}
}

func TestImportValidates(t *testing.T) {
	bundle, trace := traceFixture(t)
	trace.SchemaVersion = "9.9"
	if _, err := Import(context.Background(), &bundle, trace); err == nil {
		t.Fatal("invalid schema was imported")
	}
	if _, err := Import(nil, &bundle, traceFixtureTrace()); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := Import(context.Background(), nil, traceFixtureTrace()); err == nil {
		t.Fatal("nil bundle was accepted")
	}
}

func traceFixtureTrace() Trace {
	return sealTrace(Trace{SchemaVersion: SchemaVersion, Command: "go"})
}

func sealTrace(trace Trace) Trace {
	if trace.SchemaVersion == "" {
		trace.SchemaVersion = SchemaVersion
	}
	if trace.Command == "" {
		trace.Command = "go"
	}
	if trace.Command != "go" && trace.Command != "go.exe" && trace.Command != redactedCommand {
		trace.Command = redactedCommand
		trace.CommandRedacted = true
	}
	if trace.CommandSHA256 == "" {
		sum := sha256.Sum256([]byte("test command"))
		trace.CommandSHA256 = hex.EncodeToString(sum[:])
	}
	if trace.WorkingDirectory == "" {
		trace.WorkingDirectory = "."
	}
	if trace.Repository.RepositoryID == "" {
		trace.Repository = TraceRepository{
			RepositoryID:  rkcmodel.StableID("repository", "runtime-test"),
			ContentDigest: strings.Repeat("a", 64), GitUnavailable: true,
		}
	}
	for index := range trace.Artifacts {
		if trace.Artifacts[index].SourceSHA256 == "" {
			trace.Artifacts[index].SourceSHA256 = strings.Repeat("d", 64)
		}
	}
	trace.ID = ""
	trace.ID = IDFor(trace)
	return trace
}

func sealTraceForBundle(trace Trace, bundle *rkcmodel.Bundle) Trace {
	if bundle.Snapshot.RepositoryID == "" {
		bundle.Snapshot.RepositoryID = rkcmodel.StableID("repository", "runtime-test")
	}
	if bundle.Snapshot.Git.Commit == "" {
		bundle.Snapshot.Git.Unavailable = true
	}
	artifactByPath := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		artifactByPath[artifact.Path] = artifact
	}
	for index := range trace.Artifacts {
		if artifact, ok := artifactByPath[trace.Artifacts[index].Path]; ok {
			trace.Artifacts[index].SourceSHA256 = artifact.SHA256
			trace.Artifacts[index].SourceSizeBytes = artifact.SizeBytes
		}
	}
	digest, count := contentAffinity(bundle.Artifacts)
	trace.Repository = TraceRepository{
		RepositoryID: bundle.Snapshot.RepositoryID, ContentDigest: digest,
		ArtifactCount: count, GitCommit: bundle.Snapshot.Git.Commit,
		GitUnavailable: bundle.Snapshot.Git.Unavailable,
	}
	trace = sealTrace(trace)
	markCurrentProcessCaptureIntegrity(trace)
	return trace
}

func TestValidateRejectsBadSpansAndStatuses(t *testing.T) {
	trace := traceFixtureTrace()
	trace.Artifacts = []TraceArtifact{{Path: "x.go", ExecutedRanges: []ExecutedRange{{StartLine: 5, EndLine: 2}}}}
	trace = sealTrace(trace)
	if err := Validate(trace); err == nil {
		t.Fatal("reversed span was accepted")
	}
	trace = traceFixtureTrace()
	trace.Artifacts = []TraceArtifact{{
		Path: "x.go", SourceSHA256: strings.Repeat("a", 64),
		Statements: 1, ExecutedStatements: 1,
	}}
	trace = sealTrace(trace)
	if err := Validate(trace); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("executed statements without ranges were accepted: %v", err)
	}
	trace = traceFixtureTrace()
	trace.Tests = []TraceTest{{Name: "T", Status: "exploded"}}
	trace = sealTrace(trace)
	if err := Validate(trace); err == nil {
		t.Fatal("invalid status was accepted")
	}
	trace = traceFixtureTrace()
	trace.ID = ""
	if err := Validate(trace); err == nil {
		t.Fatal("missing id was accepted")
	}
}

func TestDigestMatchesIDFor(t *testing.T) {
	trace := traceFixtureTrace()
	trace.Artifacts = []TraceArtifact{{Path: "x.go", Statements: 2, ExecutedStatements: 1, ExecutedRanges: []ExecutedRange{{StartLine: 1, EndLine: 2, Count: 1}}}}
	if Digest(trace) != IDFor(trace) {
		t.Fatal("Digest and IDFor disagree")
	}
	if Digest(trace) == "" {
		t.Fatal("digest is empty")
	}
}

func TestImportCallSiteSpanEvidence(t *testing.T) {
	artifactID := rkcmodel.StableID("artifact", "demo.go")
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: artifactID, Path: "demo.go", Kind: "source", Language: "go", SizeBytes: 100, SHA256: strings.Repeat("d", 64), Text: true, Status: "syntax_parsed"}},
		Nodes: []rkcmodel.Node{
			{ID: "fn-a", Kind: "function", Name: "A", QualifiedName: "demo.A", Language: "go", ArtifactID: artifactID, Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 3, EndLine: 5}},
			{ID: "fn-b", Kind: "function", Name: "B", QualifiedName: "demo.B", Language: "go", ArtifactID: artifactID, Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "demo.go", StartLine: 7, EndLine: 9}},
		},
		Edges: []rkcmodel.Edge{{
			ID: "b-calls-a-span", Kind: "calls", From: "fn-b", To: "fn-a", Resolution: "syntax_inferred",
			Attributes: map[string]any{"span": map[string]any{
				"path": "demo.go", "start_line": float64(7), "end_line": float64(9),
			}},
		}},
	}
	trace := sealTraceForBundle(Trace{
		SchemaVersion: SchemaVersion, Command: "go", ExitCode: 0,
		Artifacts: []TraceArtifact{{
			Path: "demo.go", Statements: 4, ExecutedStatements: 2,
			ExecutedRanges: []ExecutedRange{
				{StartLine: 3, EndLine: 5, Count: 1},
				{StartLine: 7, EndLine: 9, Count: 1},
			},
		}},
		Tests: []TraceTest{{Name: "TestB", Status: "pass"}},
	}, &bundle)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerObservedCallEdges != 0 || stats.UndemonstratedCallEdges != 1 || stats.CallObservationAvailable {
		t.Fatalf("statement coverage claimed call observation: %+v", stats)
	}
}

func TestImportSkipsZeroExecutionEvidence(t *testing.T) {
	artifactID := rkcmodel.StableID("artifact", "demo.go")
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: artifactID, Path: "demo.go", Kind: "source", Language: "go", SizeBytes: 100, SHA256: strings.Repeat("d", 64), Text: true, Status: "syntax_parsed"}},
	}
	trace := sealTraceForBundle(Trace{
		SchemaVersion: SchemaVersion, Command: "go", ExitCode: 0,
		Artifacts: []TraceArtifact{{
			Path: "demo.go", Statements: 5, ExecutedStatements: 0,
		}},
	}, &bundle)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerAuthenticatedEvidence != 0 || stats.AssertionEvidence != 1 {
		t.Fatalf("evidence counts = producer-authenticated %d, assertion %d; want assertion provenance only", stats.ProducerAuthenticatedEvidence, stats.AssertionEvidence)
	}
}

func TestExternalSelfHashedTraceRemainsAnAssertion(t *testing.T) {
	bundle, captured := traceFixture(t)
	external := captured
	external.DurationMS++
	external.ID = ""
	external.ID = IDFor(external)

	stats, err := Import(context.Background(), &bundle, external)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerAuthenticated || stats.ProducerObservedFunctions != 0 ||
		stats.AssertedFunctions != 1 || stats.ProducerAuthenticatedEvidence != 0 ||
		stats.AssertionEvidence != 5 || stats.AssertedTests != 1 ||
		stats.FunctionsNotObserved != 1 || stats.CaptureIntegrityAuthenticated {
		t.Fatalf("unverified trace stats = %+v", stats)
	}
	for _, node := range bundle.Nodes {
		if node.Attributes["executed"] == true {
			t.Fatalf("unverified trace established execution truth: %+v", node)
		}
	}
	for _, evidence := range bundle.Evidence {
		if evidence.Kind == "runtime_observed" || evidence.Kind == "test_result" {
			t.Fatalf("unverified trace established authoritative evidence: %+v", evidence)
		}
	}
	diff := BuildDiff(bundle)
	if len(diff.ProducerAuthenticatedTraceIDs) != 0 || len(diff.UnverifiedAssertionIDs) != 1 ||
		diff.UnverifiedAssertionIDs[0] != external.ID || diff.ProducerAuthenticatedTests != 0 ||
		diff.ProducerObservedFunctions != 0 || diff.AssertedFunctions != 1 ||
		diff.FunctionsNotObserved != 1 || len(diff.CaptureIntegrityAssertionIDs) != 0 {
		t.Fatalf("unverified trace diff = %+v", diff)
	}
}

func TestBuildDiffRequiresAuthenticatedCallEventTrace(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{ID: "caller", Kind: "function", QualifiedName: "demo.Caller"},
			{ID: "callee", Kind: "function", QualifiedName: "demo.Callee"},
			{ID: "assertion", Kind: "trace", Attributes: map[string]any{
				"trace_id": "assertion", "producer_authenticated": false,
				"call_observation_available": true,
			}},
		},
		Edges: []rkcmodel.Edge{{
			ID: "call", Kind: "calls", From: "caller", To: "callee", Resolution: "compiler_resolved",
			Attributes: map[string]any{"observed": true, "observed_trace_ids": []string{"assertion"}},
		}},
	}
	diff := BuildDiff(bundle)
	if diff.CallObservationAvailable || diff.ProducerObservedCallEdges != 0 || diff.UndemonstratedCallEdges != 1 {
		t.Fatalf("assertion trace authenticated call events: %+v", diff)
	}

	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{ID: "producer", Kind: "trace", Attributes: map[string]any{
		"trace_id": "producer", "producer_authenticated": true,
		"call_observation_available": true,
	}})
	bundle.Edges[0].Attributes["observed_trace_ids"] = []string{"assertion", "producer"}
	diff = BuildDiff(bundle)
	if !diff.CallObservationAvailable || diff.ProducerObservedCallEdges != 1 || diff.UndemonstratedCallEdges != 0 ||
		len(diff.ProducerAuthenticatedTraceIDs) != 1 || diff.ProducerAuthenticatedTraceIDs[0] != "producer" {
		t.Fatalf("authenticated call event was not admitted: %+v", diff)
	}

	bundle.Evidence = []rkcmodel.Evidence{
		{ID: "low-confidence", Kind: "test_result", Confidence: 0.5, Attributes: map[string]any{
			"trace_id": "producer", "producer_authenticated": true, "status": "pass",
		}},
		{ID: "bad-status", Kind: "test_result", Confidence: 1, Attributes: map[string]any{
			"trace_id": "producer", "producer_authenticated": true, "status": "unknown",
		}},
		{ID: "authenticated", Kind: "test_result", Confidence: 1, Attributes: map[string]any{
			"trace_id": "producer", "producer_authenticated": true, "status": "fail",
		}},
	}
	diff = BuildDiff(bundle)
	if diff.ProducerAuthenticatedTests != 1 || diff.ProducerAuthenticatedFailed != 1 ||
		diff.ProducerAuthenticatedPassed != 0 {
		t.Fatalf("malformed test events crossed the authority boundary: %+v", diff)
	}
}
