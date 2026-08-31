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

func TestValidateRejectsAdversarialTraceShapes(t *testing.T) {
	base := validBoundaryTrace(t)
	tests := []struct {
		name   string
		reseal bool
		want   string
		mutate func(*Trace)
	}{
		{"schema", true, "schema_version", func(trace *Trace) { trace.SchemaVersion = "future" }},
		{"missing id", false, "id is missing", func(trace *Trace) { trace.ID = "" }},
		{"short id", false, "SHA-256", func(trace *Trace) { trace.ID = "abcd" }},
		{"nonhex id", false, "hexadecimal", func(trace *Trace) { trace.ID = strings.Repeat("z", 64) }},
		{"mismatched id", false, "canonical content", func(trace *Trace) { trace.ID = strings.Repeat("0", 64) }},
		{"command identity", true, "command identity", func(trace *Trace) { trace.Command = "" }},
		{"redacted command mismatch", true, "redacted trace command", func(trace *Trace) { trace.CommandRedacted = true }},
		{"unadmitted command", true, "admitted command", func(trace *Trace) { trace.Command = redactedCommand }},
		{"command digest", true, "command_sha256", func(trace *Trace) { trace.CommandSHA256 = "bad" }},
		{"working directory", true, "working_directory", func(trace *Trace) { trace.WorkingDirectory = "subdir" }},
		{"negative duration", true, "duration_ms", func(trace *Trace) { trace.DurationMS = -1 }},
		{"repository identity", true, "repository affinity", func(trace *Trace) { trace.Repository.RepositoryID = "" }},
		{"environment bound", true, "environment-key bound", func(trace *Trace) { trace.EnvironmentKeys = make([]string, MaximumEnvironmentKeys+1) }},
		{"environment syntax", true, "environment key", func(trace *Trace) { trace.EnvironmentKeys = []string{"NOT-VALID"} }},
		{"environment duplicate", true, "duplicated", func(trace *Trace) { trace.EnvironmentKeys = []string{"SAFE", "SAFE"} }},
		{"artifact bound", true, "artifact bound", func(trace *Trace) { trace.Artifacts = make([]TraceArtifact, MaximumTraceArtifacts+1) }},
		{"test bound", true, "test bound", func(trace *Trace) { trace.Tests = make([]TraceTest, MaximumTraceTests+1) }},
		{"source bound", true, "source bound", func(trace *Trace) { trace.Sources = make([]TraceSource, MaximumTraceSources+1) }},
		{"artifact path", true, "invalid path", func(trace *Trace) { trace.Artifacts[0].Path = "../escape.go" }},
		{"artifact counts", true, "statement counts", func(trace *Trace) { trace.Artifacts[0].ExecutedStatements = 3 }},
		{"artifact source identity", true, "source identity", func(trace *Trace) { trace.Artifacts[0].SourceSHA256 = "bad" }},
		{"artifact duplicate", true, "duplicated", func(trace *Trace) { trace.Artifacts = append(trace.Artifacts, trace.Artifacts[0]) }},
		{"artifact range bound", true, "range bound", func(trace *Trace) {
			trace.Artifacts[0].Statements = MaximumExecutedRanges + 1
			trace.Artifacts[0].ExecutedStatements = MaximumExecutedRanges + 1
			trace.Artifacts[0].ExecutedRanges = make([]ExecutedRange, MaximumExecutedRanges+1)
		}},
		{"artifact range consistency", true, "inconsistent", func(trace *Trace) { trace.Artifacts[0].ExecutedRanges = nil }},
		{"artifact invalid range", true, "invalid executed range", func(trace *Trace) { trace.Artifacts[0].ExecutedRanges[0].Count = 0 }},
		{"test identity", true, "test has invalid identity", func(trace *Trace) { trace.Tests[0].Name = "bad/name" }},
		{"test duplicate", true, "duplicated", func(trace *Trace) { trace.Tests = append(trace.Tests, trace.Tests[0]) }},
		{"test status", true, "invalid status", func(trace *Trace) { trace.Tests[0].Status = "unknown" }},
		{"source identity", true, "trace source is invalid", func(trace *Trace) { trace.Sources[0].Kind = "" }},
		{"source duplicate", true, "duplicated", func(trace *Trace) { trace.Sources = append(trace.Sources, trace.Sources[0]) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneBoundaryTrace(t, base)
			test.mutate(&candidate)
			if test.reseal {
				candidate.ID = ""
				candidate.ID = IDFor(candidate)
			}
			if err := Validate(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestRuntimeIdentityValidatorsCoverPortableBoundaryForms(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if !validDigest(digest) || validDigest(strings.ToUpper(digest)) || validDigest("short") || validDigest(strings.Repeat("z", 64)) {
		t.Fatal("digest validation accepted a non-canonical form")
	}
	for _, invalid := range []string{"", string([]byte{0xff}), "bad\npath", `bad\path`, "/absolute", ".", "..", "../escape", "a/../b"} {
		if validRelativePath(invalid) {
			t.Errorf("validRelativePath(%q) = true", invalid)
		}
	}
	if !validRelativePath("safe/path.go") || !containsControl(string([]byte{0xff})) || !containsControl("bad\n") || containsControl("safe") {
		t.Fatal("relative-path or control-character boundary mismatch")
	}

	repository := TraceRepository{RepositoryID: "repo", ContentDigest: digest, ArtifactCount: 1, GitUnavailable: true}
	if !validRepositoryIdentity(repository) {
		t.Fatal("valid repository identity was rejected")
	}
	repository.GitCommit = strings.Repeat("b", 40)
	if validRepositoryIdentity(repository) {
		t.Fatal("unavailable Git identity retained a commit")
	}
	repository.GitUnavailable = false
	if !validRepositoryIdentity(repository) || validGitCommit("short") || validGitCommit(strings.Repeat("Z", 40)) || validGitCommit(strings.Repeat("z", 40)) {
		t.Fatal("Git commit validation mismatch")
	}

	validTest := TraceTest{Package: "example.com/demo", Name: "TestSafe", Status: "pass"}
	if !validTraceTestIdentity(validTest) {
		t.Fatal("valid test identity was rejected")
	}
	invalidTests := []TraceTest{
		{Name: ""},
		{Name: "Bad/Raw"},
		{Name: "bad-name"},
		{Name: "TestSafe", SubtestsRedacted: true},
		{Name: "TestSafe" + redactedSubtestSuffix, Package: "bad package", SubtestsRedacted: true},
		{Name: "TestSafe", Package: "not-redacted", PackageRedacted: true},
	}
	for _, test := range invalidTests {
		if validTraceTestIdentity(test) {
			t.Errorf("invalid test identity accepted: %+v", test)
		}
	}
	if !validTraceTestIdentity(TraceTest{Name: "TestSafe", Package: redactedPackage, PackageRedacted: true}) {
		t.Fatal("canonical redacted package identity was rejected")
	}
}

func TestCoverageAndTestEventParsersExerciseFailurePrecedence(t *testing.T) {
	for _, test := range []struct {
		line string
		want string
	}{
		{"missing-colon", "malformed coverage record"},
		{"demo.go:1.1,2.2", "malformed coverage range"},
		{"demo.go:1.1,2.2 1", "malformed coverage counts"},
		{"demo.go:1.1 1 1", "malformed coverage span"},
		{"demo.go:x.1,2.2 1 1", "invalid coverage position"},
		{"demo.go:1.1,x.2 1 1", "invalid coverage position"},
		{"demo.go:1.1,2.2 -1 0", "invalid statement count"},
		{"demo.go:1.1,2.2 1 -1", "invalid executed count"},
	} {
		if _, err := parseCoverageLine(test.line); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("parseCoverageLine(%q) = %v, want %q", test.line, err, test.want)
		}
	}
	absolute, err := parseCoverageLine("/usr/local/go/runtime.go:1.1,2.2 1 1")
	if err != nil || absolute != nil {
		t.Fatalf("absolute toolchain record = %+v, %v", absolute, err)
	}
	notExecuted, err := parseCoverageLine("demo.go:1.1,2.2 2 0")
	if err != nil || notExecuted.ExecutedStatements != 0 || len(notExecuted.ExecutedRanges) != 0 {
		t.Fatalf("zero-count record = %+v, %v", notExecuted, err)
	}

	profile := filepath.Join(t.TempDir(), "coverage.out")
	content := "mode: atomic\n\ndemo.go:1.1,2.2 2 1\ndemo.go:3.1,4.2 3 0\n"
	if err := os.WriteFile(profile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := parseCoverageFile(profile)
	if err != nil || len(artifacts) != 1 || artifacts[0].Statements != 5 || artifacts[0].ExecutedStatements != 2 {
		t.Fatalf("merged coverage = %+v, %v", artifacts, err)
	}
	if _, err := parseCoverageFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing coverage profile was accepted")
	}
	oversized := filepath.Join(t.TempDir(), "oversized.out")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCoverageFile(oversized); err == nil {
		t.Fatal("oversized coverage token was accepted")
	}

	events := strings.Join([]string{
		"not-json",
		`{"Action":"pass"}`,
		`{"Action":"skip","Package":"example.com/demo","Test":"TestOne","Elapsed":0.001}`,
		`{"Action":"pass","Package":"example.com/demo","Test":"TestOne","Elapsed":0.002}`,
		`{"Action":"fail","Package":"example.com/demo","Test":"TestOne","Elapsed":0.001}`,
	}, "\n")
	results, err := parseGoTestJSON([]byte(events))
	if err != nil || len(results) != 1 || results[0].Status != "fail" || results[0].Elapsed != 2 {
		t.Fatalf("merged test events = %+v, %v", results, err)
	}
	for _, event := range []string{
		`{"Action":"pass","Package":"bad package","Test":"TestOne"}`,
		`{"Action":"pass","Package":"example.com/demo","Test":"bad-name"}`,
		`{"Action":"pass","Package":"example.com/demo","Test":"TestOne","Elapsed":-1}`,
	} {
		if _, err := parseGoTestJSON([]byte(event)); err == nil {
			t.Fatalf("invalid go test event accepted: %s", event)
		}
	}
	if mergeTestStatus("fail", "pass") != "fail" || mergeTestStatus("skip", "pass") != "pass" {
		t.Fatal("terminal test status precedence changed")
	}
}

func TestRuntimeIdentifierRedactionBoundaries(t *testing.T) {
	if value, redacted, err := safePackageIdentity(""); err != nil || value != "" || redacted {
		t.Fatalf("empty package = %q, %v, %v", value, redacted, err)
	}
	for _, value := range []string{strings.Repeat("a", MaximumPackageIdentifierBytes+1), "bad package", "bad\npackage"} {
		if _, _, err := safePackageIdentity(value); err == nil {
			t.Errorf("unsafe package accepted: %q", value)
		}
	}
	secretPackage := "ghp_" + strings.Repeat("A", 36)
	if value, redacted, err := safePackageIdentity(secretPackage); err != nil || value != redactedPackage || !redacted {
		t.Fatalf("credential-shaped package = %q, %v, %v", value, redacted, err)
	}
	if value, redacted, err := safePackageIdentity("example.com/safe"); err != nil || value == "" || redacted {
		t.Fatalf("safe package = %q, %v, %v", value, redacted, err)
	}

	for _, value := range []string{"", strings.Repeat("T", MaximumTestIdentifierBytes+1), "bad\nname", "bad-name", "TestSafe/"} {
		if _, _, err := safeTestIdentity(value); err == nil {
			t.Errorf("unsafe test accepted: %q", value)
		}
	}
	if name, redacted, err := safeTestIdentity("TestSafe"); err != nil || name != "TestSafe" || redacted {
		t.Fatalf("safe test = %q, %v, %v", name, redacted, err)
	}
	if name, redacted, err := safeTestIdentity("TestSafe/user-controlled"); err != nil || name != "TestSafe"+redactedSubtestSuffix || !redacted {
		t.Fatalf("subtest redaction = %q, %v, %v", name, redacted, err)
	}
}

func TestAffinityBindingsRejectAmbiguityAndMutation(t *testing.T) {
	ignoredDigest, ignoredCount := contentAffinity([]rkcmodel.Artifact{
		{Path: ".rkc-trace.json", SHA256: strings.Repeat("a", 64)},
		{Path: "b.go", Kind: "source", SizeBytes: 2, SHA256: strings.Repeat("b", 64)},
		{Path: "a.go", Kind: "source", SizeBytes: 1, SHA256: strings.Repeat("a", 64)},
	})
	if ignoredDigest == "" || ignoredCount != 2 {
		t.Fatalf("content affinity = %q, %d", ignoredDigest, ignoredCount)
	}

	root := t.TempDir()
	moduleA := writeBoundaryFile(t, root, "a/go.mod", "module example.com/shared\n")
	moduleB := writeBoundaryFile(t, root, "b/go.mod", "module example.com/shared\n")
	if _, err := goModuleBindings(root, []rkcmodel.Artifact{moduleA, moduleB}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate module identity = %v", err)
	}
	stale := moduleA
	stale.SHA256 = strings.Repeat("0", 64)
	if _, err := goModuleBindings(root, []rkcmodel.Artifact{stale}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale module identity = %v", err)
	}
	missing := moduleA
	missing.Path = "missing/go.mod"
	if _, err := goModuleBindings(root, []rkcmodel.Artifact{missing}); err == nil || !strings.Contains(err.Error(), "read inventoried") {
		t.Fatalf("missing module identity = %v", err)
	}

	for _, test := range []struct {
		data string
		want string
	}{
		{"package demo\n", ""},
		{"module \"example.com/quoted\"\n", "example.com/quoted"},
		{"module \"unterminated\n", "error"},
		{"module /absolute\n", "error"},
	} {
		module, err := parseGoModuleIdentity([]byte(test.data))
		if test.want == "error" {
			if err == nil {
				t.Errorf("malformed module accepted: %q", test.data)
			}
		} else if err != nil || module != test.want {
			t.Errorf("module identity = %q, %v; want %q", module, err, test.want)
		}
	}
	if _, err := parseGoModuleIdentity([]byte(strings.Repeat("x", int(captureMaximumTextBytes)+1))); err == nil {
		t.Fatal("oversized module identity token was accepted")
	}

	candidates := map[string]rkcmodel.Artifact{
		"a/file.go": {ID: "a", Path: "a/file.go"},
		"b/file.go": {ID: "b", Path: "b/file.go"},
		"exact.go":  {ID: "exact", Path: "exact.go"},
	}
	if _, err := resolveCoverageArtifact("example.com/shared/file.go", candidates, []goModuleBinding{
		{module: "example.com/shared", directory: "a"},
		{module: "example.com/shared", directory: "b"},
	}); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous module binding = %v", err)
	}
	if got, err := resolveCoverageArtifact("exact.go", candidates, nil); err != nil || got.ID != "exact" {
		t.Fatalf("exact coverage binding = %+v, %v", got, err)
	}
	if _, err := resolveCoverageArtifact("missing.go", candidates, nil); err == nil {
		t.Fatal("unbound coverage path was accepted")
	}

	observed := []TraceArtifact{
		{Path: "exact.go", Statements: 1},
		{Path: "exact.go", Statements: 2},
	}
	bound, err := bindTraceArtifacts(root, observed, []rkcmodel.Artifact{
		{ID: "ignored", Path: "ignored.go", Status: "excluded", SHA256: strings.Repeat("c", 64)},
		{ID: "exact", Path: "exact.go", SizeBytes: 3, SHA256: strings.Repeat("d", 64)},
	})
	if err != nil || len(bound) != 1 || bound[0].Statements != 3 || bound[0].SourceSHA256 != strings.Repeat("d", 64) {
		t.Fatalf("duplicate binding = %+v, %v", bound, err)
	}
	if _, err := bindTraceArtifacts(root, []TraceArtifact{{Path: "missing.go"}}, nil); err == nil {
		t.Fatal("foreign coverage path was bound")
	}
}

func TestTraceInputDecoderAndDigestFailClosed(t *testing.T) {
	dir := t.TempDir()
	trace := validBoundaryTrace(t)
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{"unknown field", append(append([]byte(nil), encoded[:len(encoded)-1]...), []byte(`,"unknown":true}`)...), "unknown field"},
		{"trailing content", append(append([]byte(nil), encoded...), []byte(` {}`)...), "trailing JSON"},
	} {
		path := filepath.Join(dir, test.name+".json")
		if err := os.WriteFile(path, test.data, 0o600); err != nil {
			t.Fatal(err)
		}
		input := TraceInput{Path: path, SHA256: boundaryDigest(test.data), SizeBytes: int64(len(test.data))}
		if _, err := LoadTrace(context.Background(), input); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("LoadTrace(%s) = %v, want %q", test.name, err, test.want)
		}
	}
	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(validPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	input := TraceInput{
		Path: validPath, SHA256: boundaryDigest(encoded), SizeBytes: int64(len(encoded)),
		integrityAuthenticatedTraceID: "different",
	}
	if _, err := LoadTrace(context.Background(), input); err == nil || !strings.Contains(err.Error(), "capture-integrity identity") {
		t.Fatalf("capture-integrity substitution = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := PrepareTraceInputs(cancelled, []string{validPath}); err == nil {
		t.Fatal("cancelled trace preparation proceeded")
	}
	if inputs, digest, err := PrepareTraceInputs(context.Background(), nil); err != nil || inputs != nil || digest != "" || AggregateDigest(nil) != "" {
		t.Fatalf("empty trace inputs = %+v, %q, %v", inputs, digest, err)
	}
}

func TestCaptureIntegrityRegistryAndBuffersStayBounded(t *testing.T) {
	currentProcessCaptureIntegrities.Lock()
	previousIDs := currentProcessCaptureIntegrities.ids
	previousOrder := currentProcessCaptureIntegrities.order
	currentProcessCaptureIntegrities.ids = map[string]struct{}{}
	currentProcessCaptureIntegrities.order = nil
	currentProcessCaptureIntegrities.Unlock()
	defer func() {
		currentProcessCaptureIntegrities.Lock()
		currentProcessCaptureIntegrities.ids = previousIDs
		currentProcessCaptureIntegrities.order = previousOrder
		currentProcessCaptureIntegrities.Unlock()
	}()

	markCurrentProcessCaptureIntegrity(Trace{})
	for index := 0; index <= maximumCurrentProcessCaptureIntegrities; index++ {
		id := rkcmodel.StableID("trace-registry-test", string(rune(index)))
		markCurrentProcessCaptureIntegrity(Trace{ID: id})
		markCurrentProcessCaptureIntegrity(Trace{ID: id})
	}
	currentProcessCaptureIntegrities.Lock()
	registrySize := len(currentProcessCaptureIntegrities.ids)
	orderSize := len(currentProcessCaptureIntegrities.order)
	currentProcessCaptureIntegrities.Unlock()
	if registrySize != maximumCurrentProcessCaptureIntegrities || orderSize != maximumCurrentProcessCaptureIntegrities {
		t.Fatalf("capture-integrity registry sizes = %d, %d", registrySize, orderSize)
	}

	var buffer boundedBuffer
	payload := make([]byte, MaximumCaptureOutputBytes+1)
	if written, err := buffer.Write(payload); err != nil || written != len(payload) || !buffer.truncated || len(buffer.bytes()) != MaximumCaptureOutputBytes {
		t.Fatalf("bounded write = %d, %v, truncated=%v, retained=%d", written, err, buffer.truncated, len(buffer.bytes()))
	}
	if written, err := buffer.Write([]byte("more")); err != nil || written != 4 {
		t.Fatalf("post-bound write = %d, %v", written, err)
	}
	if digest, size := fileDigest(filepath.Join(t.TempDir(), "missing")); digest != "" || size != 0 {
		t.Fatalf("missing file digest = %q, %d", digest, size)
	}
	if digest, size := fileDigest(t.TempDir()); digest != "" || size != 0 {
		t.Fatalf("directory digest = %q, %d", digest, size)
	}
}

func TestRuntimeDiffRejectsOrphanAuthorityAndHandlesDecodedLists(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{ID: "caller", Kind: "function", QualifiedName: "demo.Caller", Attributes: map[string]any{
				"execution_asserted_trace_ids":     []any{"assertion"},
				"execution_not_observed_trace_ids": []any{"assertion"},
			}},
			{ID: "callee", Kind: "function", QualifiedName: "demo.Callee"},
			{ID: "assertion-trace", Kind: "trace", Attributes: map[string]any{
				"trace_id": "assertion", "producer_authenticated": false,
			}},
		},
		Edges: []rkcmodel.Edge{{
			ID: "call", Kind: "calls", From: "caller", To: "callee",
			Resolution: rkcmodel.ResolutionCompilerResolved,
		}},
	}
	diff := BuildDiff(bundle)
	if diff.AssertedFunctions != 1 || diff.FunctionsNotObserved != 1 ||
		diff.CallObservationReason != "runtime assertions contain no producer-authenticated call-event evidence" {
		t.Fatalf("decoded assertion diff = %+v", diff)
	}
	bundle.Nodes[2].Attributes["producer_authenticated"] = true
	diff = BuildDiff(bundle)
	if diff.CallObservationReason != "producer-authenticated traces contain no authenticated call-event evidence" {
		t.Fatalf("producer-without-call-events diff = %+v", diff)
	}
	if !hasStringValues([]any{"value"}) || hasStringValues([]any{1, ""}) || hasStringValues("value") {
		t.Fatal("decoded string-list handling changed")
	}
}

func validBoundaryTrace(t *testing.T) Trace {
	t.Helper()
	trace := sealTrace(Trace{
		SchemaVersion: SchemaVersion,
		Command:       "go",
		Artifacts: []TraceArtifact{{
			Path: "demo.go", Statements: 2, ExecutedStatements: 1,
			ExecutedRanges: []ExecutedRange{{StartLine: 1, EndLine: 2, Count: 1}},
		}},
		Tests: []TraceTest{{Package: "example.com/demo", Name: "TestSafe", Status: "pass"}},
		Sources: []TraceSource{{
			Kind: "coverage", Path: "coverage.out", SHA256: strings.Repeat("c", 64), SizeBytes: 1,
		}},
	})
	if err := Validate(trace); err != nil {
		t.Fatalf("valid boundary trace: %v", err)
	}
	return trace
}

func cloneBoundaryTrace(t *testing.T, trace Trace) Trace {
	t.Helper()
	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	var cloned Trace
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func writeBoundaryFile(t *testing.T, root, relative, content string) rkcmodel.Artifact {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return rkcmodel.Artifact{
		Path: relative, SizeBytes: int64(len(content)), SHA256: boundaryDigest([]byte(content)),
	}
}

func boundaryDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestCaptureRejectsNilContextAndRecordsNonzeroExitAsAssertion(t *testing.T) {
	if _, err := Capture(nil, CaptureOptions{Repository: ".", Command: []string{"true"}}); err == nil {
		t.Fatal("nil capture context was accepted")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RKC_TRACE_NONZERO_HELPER", "1")
	result, err := Capture(context.Background(), CaptureOptions{
		Repository: t.TempDir(),
		Command:    []string{executable, "-test.run=^TestTraceNonzeroExitHelper$"},
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trace.ExitCode == 0 || !authenticatedCaptureIntegrity(result.Trace) {
		t.Fatalf("nonzero capture = exit %d, integrity %v", result.Trace.ExitCode, authenticatedCaptureIntegrity(result.Trace))
	}
}

func TestTraceNonzeroExitHelper(t *testing.T) {
	if os.Getenv("RKC_TRACE_NONZERO_HELPER") != "1" {
		return
	}
	t.Fatal("intentional nonzero trace-capture helper exit")
}
