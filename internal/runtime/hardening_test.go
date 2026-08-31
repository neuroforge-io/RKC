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

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestTraceCaptureMutationHelper(t *testing.T) {
	if os.Getenv("RKC_TRACE_MUTATION_HELPER") != "1" {
		return
	}
	root := os.Getenv("RKC_TRACE_MUTATION_ROOT")
	if err := os.WriteFile(filepath.Join(root, "generated-during-capture.txt"), []byte("changed\n"), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestTraceIdentityRejectsTamperingAndArgumentsStayPrivate(t *testing.T) {
	secret := "sentinel-secret-command-value"
	trace := sealTrace(Trace{Command: "runner"})
	trace.CommandSHA256 = commandShapeDigest([]string{"runner", "--token=" + secret, secret})
	trace.ID = IDFor(trace)
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("trace disclosed a command argument")
	}
	if err := Validate(trace); err != nil {
		t.Fatalf("valid trace rejected: %v", err)
	}
	trace.ExitCode = 7
	if err := Validate(trace); err == nil {
		t.Fatal("content-tampered trace retained a valid identity")
	}
}

func TestCommandShapeRedactsEveryValue(t *testing.T) {
	secret := "sentinel-secret-command-value"
	shape := strings.Join(commandShape([]string{"tool", "--token=" + secret, "-p" + secret, secret}), " ")
	if strings.Contains(shape, secret) {
		t.Fatalf("redacted command shape leaked %q", secret)
	}
	if shape != "custom-command <option=value> <option> <argument>" {
		t.Fatalf("shape = %q", shape)
	}
}

func commandShapeDigest(command []string) string {
	sum := sha256.Sum256(mustJSON(commandShape(command)))
	return hex.EncodeToString(sum[:])
}

func TestImportUnionsTraceScopedFunctionAssertions(t *testing.T) {
	bundle, first := traceFixture(t)
	if _, err := Import(context.Background(), &bundle, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.DurationMS++
	second.Artifacts[0].ExecutedRanges = []ExecutedRange{{StartLine: 7, EndLine: 9, Count: 1}}
	second.ID = ""
	second.ID = IDFor(second)
	markCurrentProcessCaptureIntegrity(second)
	if _, err := Import(context.Background(), &bundle, second); err != nil {
		t.Fatal(err)
	}
	asserted := map[string]bool{}
	traceNodes := 0
	for _, node := range bundle.Nodes {
		if node.Attributes["executed"] == true {
			t.Fatalf("trace-scoped assertion established execution truth: %+v", node)
		}
		asserted[node.ID] = hasStringValues(node.Attributes["execution_asserted_trace_ids"])
		if node.Kind == "trace" {
			traceNodes++
		}
	}
	if !asserted["fn-a"] || !asserted["fn-b"] {
		t.Fatalf("trace union lost a positive assertion: %v", asserted)
	}
	if traceNodes != 2 {
		t.Fatalf("trace nodes = %d, want 2", traceNodes)
	}
}

func TestEnvironmentKeyNormalizationIsExplicitAndBounded(t *testing.T) {
	empty, err := normalizeEnvironmentKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty opt-in normalized to %v", empty)
	}

	keys, err := normalizeEnvironmentKeys([]string{"RKC_ZETA", "RKC_ALPHA", "RKC_ZETA"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keys, ","); got != "RKC_ALPHA,RKC_ZETA" {
		t.Fatalf("normalized environment keys = %q", got)
	}
	if _, err := normalizeEnvironmentKeys([]string{"NOT-VALID"}); err == nil {
		t.Fatal("invalid environment key was accepted")
	}
	if _, err := normalizeEnvironmentKeys(make([]string, MaximumEnvironmentKeys+1)); err == nil {
		t.Fatal("environment-key aggregate bound was not enforced")
	}
}

func TestStatementCoverageNeverPromotesCallSitesToObserved(t *testing.T) {
	for _, test := range []struct {
		name       string
		evidence   []rkcmodel.Evidence
		evidenceID []string
	}{
		{name: "missing call-site span"},
		{
			name: "covered call-site span is still not a call event",
			evidence: []rkcmodel.Evidence{{
				ID: "call-site", Kind: "syntax_inferred", Method: "test", Confidence: 1,
				Source: &rkcmodel.SourceRange{Path: "demo.go", StartLine: 8, EndLine: 8},
			}},
			evidenceID: []string{"call-site"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle, trace := traceFixture(t)
			bundle.Edges[0].Attributes = nil
			bundle.Edges[0].EvidenceIDs = test.evidenceID
			bundle.Evidence = test.evidence
			trace.Artifacts[0].ExecutedRanges = []ExecutedRange{
				{StartLine: 3, EndLine: 5, Count: 1},
				{StartLine: 7, EndLine: 9, Count: 1},
			}
			trace.ID = ""
			trace.ID = IDFor(trace)
			markCurrentProcessCaptureIntegrity(trace)
			stats, err := Import(context.Background(), &bundle, trace)
			if err != nil {
				t.Fatal(err)
			}
			if stats.ProducerObservedCallEdges != 0 || stats.UndemonstratedCallEdges != 1 || stats.CallObservationAvailable {
				t.Fatalf("statement coverage claimed call observation: %+v", stats)
			}
			if bundle.Edges[0].Attributes["observed"] == true {
				t.Fatal("statement coverage wrote observed=true")
			}
			diff := BuildDiff(bundle)
			if diff.CallObservationAvailable || diff.CallObservationReason == "" || diff.ProducerObservedCallEdges != 0 || diff.UndemonstratedCallEdges != 1 {
				t.Fatalf("runtime diff obscured unavailable call-event evidence: %+v", diff)
			}
		})
	}
}

func TestTraceWideExecutedRangeBoundPreventsMultiplicativeAdmission(t *testing.T) {
	trace := traceFixtureTrace()
	trace.Artifacts = []TraceArtifact{
		{
			Path: "first.go", SourceSHA256: strings.Repeat("a", 64),
			Statements: MaximumTotalExecutedRanges, ExecutedStatements: MaximumTotalExecutedRanges,
			ExecutedRanges: make([]ExecutedRange, MaximumTotalExecutedRanges),
		},
		{
			Path: "second.go", SourceSHA256: strings.Repeat("b", 64),
			Statements: 1, ExecutedStatements: 1,
			ExecutedRanges: []ExecutedRange{{StartLine: 1, EndLine: 1, Count: 1}},
		},
	}
	for index := range trace.Artifacts[0].ExecutedRanges {
		trace.Artifacts[0].ExecutedRanges[index] = ExecutedRange{StartLine: index + 1, EndLine: index + 1, Count: 1}
	}
	trace.ID = IDFor(trace)
	if err := Validate(trace); err == nil || !strings.Contains(err.Error(), "total-executed-range bound") {
		t.Fatalf("multiplicative executed-range trace was not rejected: %v", err)
	}
}

func TestImportUsesIntervalsWithoutExpandingCoveredLines(t *testing.T) {
	bundle, trace := traceFixture(t)
	ranges := make([]ExecutedRange, MaximumTotalExecutedRanges)
	for index := range ranges {
		start := index*5000 + 1
		ranges[index] = ExecutedRange{StartLine: start, EndLine: start + 4095, Count: 1}
	}
	trace.Artifacts[0].Statements = MaximumTotalExecutedRanges
	trace.Artifacts[0].ExecutedStatements = MaximumTotalExecutedRanges
	trace.Artifacts[0].ExecutedRanges = ranges
	trace.ID = IDFor(trace)
	markCurrentProcessCaptureIntegrity(trace)
	stats, err := Import(context.Background(), &bundle, trace)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ProducerObservedFunctions != 0 || stats.AssertedFunctions == 0 || stats.ProducerObservedCallEdges != 0 {
		t.Fatalf("interval import truth = %+v", stats)
	}
}

func TestPrepareCommandRecognizesWindowsGoAndUsesPrivateTemp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	command, coveragePath, instrumented, err := prepareCommand([]string{"go.exe", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(coveragePath)
	if !instrumented || coveragePath == "" || len(command) < 3 {
		t.Fatalf("prepared command = %v, coverage = %q", command, coveragePath)
	}
	info, err := os.Stat(coveragePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("coverage temp permissions = %o", info.Mode().Perm())
	}
	if filepath.Dir(coveragePath) != os.TempDir() {
		t.Fatalf("coverage temp escaped temp dir: %q", coveragePath)
	}
}

func TestCaptureFailsClosedWhenRepositoryChangesDuringObservation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RKC_TRACE_MUTATION_HELPER", "1")
	t.Setenv("RKC_TRACE_MUTATION_ROOT", root)
	_, err = Capture(context.Background(), CaptureOptions{
		Repository: root,
		Command:    []string{executable, "-test.run=^TestTraceCaptureMutationHelper$"},
	})
	if err == nil || !strings.Contains(err.Error(), "changing repository") {
		t.Fatalf("repository mutation was not rejected: %v", err)
	}
}

func TestCoveragePathBindingRejectsForeignSuffixes(t *testing.T) {
	want := rkcmodel.Artifact{Path: "internal/payment/retry.go", SHA256: strings.Repeat("a", 64)}
	candidates := map[string]rkcmodel.Artifact{want.Path: want}

	if _, err := resolveCoverageArtifact(
		"evil.example/external/internal/payment/retry.go", candidates, nil,
	); err == nil {
		t.Fatal("foreign coverage path was accepted by suffix alone")
	}
	got, err := resolveCoverageArtifact(want.Path, candidates, nil)
	if err != nil || got.Path != want.Path {
		t.Fatalf("exact repository path was not accepted: got=%+v err=%v", got, err)
	}
	got, err = resolveCoverageArtifact(
		"example.com/repository/"+want.Path,
		candidates,
		[]goModuleBinding{{module: "example.com/repository"}},
	)
	if err != nil || got.Path != want.Path {
		t.Fatalf("verified Go module path was not accepted: got=%+v err=%v", got, err)
	}
}

func TestImportRejectsForeignStaleAndMismatchedSourceWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Trace)
	}{
		{name: "foreign repository", mutate: func(trace *Trace) {
			trace.Repository.RepositoryID = rkcmodel.StableID("repository", "foreign")
		}},
		{name: "stale content", mutate: func(trace *Trace) {
			trace.Repository.ContentDigest = strings.Repeat("b", 64)
		}},
		{name: "different git state", mutate: func(trace *Trace) {
			trace.Repository.GitUnavailable = false
			trace.Repository.GitCommit = strings.Repeat("c", 40)
		}},
		{name: "different source bytes", mutate: func(trace *Trace) {
			trace.Artifacts[0].SourceSHA256 = strings.Repeat("e", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle, trace := traceFixture(t)
			test.mutate(&trace)
			trace.ID = IDFor(trace)
			nodes, evidence := len(bundle.Nodes), len(bundle.Evidence)
			if _, err := Import(context.Background(), &bundle, trace); err == nil {
				t.Fatal("mismatched trace was imported")
			}
			if len(bundle.Nodes) != nodes || len(bundle.Evidence) != evidence {
				t.Fatal("failed affinity admission partially mutated the bundle")
			}
		})
	}
}

func TestGoTestIdentifiersAreBoundedControlSafeAndSecretSafe(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	output := []byte(`{"Action":"pass","Package":"` + secret + `","Test":"TestSafe/` + secret + `","Elapsed":0.01}` + "\n")
	tests, err := parseGoTestJSON(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 1 || tests[0].Package != redactedPackage || !tests[0].PackageRedacted ||
		tests[0].Name != "TestSafe"+redactedSubtestSuffix || !tests[0].SubtestsRedacted {
		t.Fatalf("sanitized tests = %+v", tests)
	}
	encoded, err := json.Marshal(tests)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("runtime test metadata retained credential-shaped material")
	}
	forged := traceFixtureTrace()
	forged.Tests = []TraceTest{{Package: secret, Name: "TestSafe", Status: "pass"}}
	forged.ID = IDFor(forged)
	if err := Validate(forged); err == nil {
		t.Fatal("hand-crafted credential-shaped package identity passed validation")
	}
	for name, event := range map[string]string{
		"control":   `{"Action":"pass","Package":"example.com/ok","Test":"TestSafe\u001bvalue"}`,
		"oversized": `{"Action":"pass","Package":"example.com/ok","Test":"` + strings.Repeat("T", MaximumTestIdentifierBytes+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGoTestJSON([]byte(event + "\n")); err == nil {
				t.Fatal("unsafe test identity was accepted")
			}
		})
	}
}
