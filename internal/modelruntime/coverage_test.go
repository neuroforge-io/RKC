package modelruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestEstimateMemoryDefaultsLimitsAndSaturation(t *testing.T) {
	estimate := EstimateMemory(ModelDescriptor{WeightBytes: 1024, ContextLimit: 8192}, InferenceOptions{}, 128, Budget{})
	if !estimate.Allowed || estimate.KVCacheBytes != 4096*(128*1024) || estimate.OutputBytes != 768*8 {
		t.Fatalf("unexpected default estimate: %+v", estimate)
	}

	overContext := EstimateMemory(
		ModelDescriptor{WeightBytes: 1, ContextLimit: 4},
		InferenceOptions{ContextTokens: 8, MaxOutputTokens: 1, KVBytesPerToken: 1, RuntimeOverheadBytes: 1},
		1,
		Budget{MaximumRSSBytes: 1024},
	)
	if overContext.Allowed || !containsString(overContext.Reasons, "requested context exceeds model context limit") {
		t.Fatalf("context ceiling was not enforced: %+v", overContext)
	}

	saturated := EstimateMemory(
		ModelDescriptor{WeightBytes: -1},
		InferenceOptions{ContextTokens: math.MaxInt, MaxOutputTokens: math.MaxInt, Parallel: math.MaxInt, KVBytesPerToken: math.MaxInt, RuntimeOverheadBytes: 1},
		-1,
		Budget{MaximumRSSBytes: math.MaxInt64 - 1, SafetyMarginBytes: -1},
	)
	if saturated.Allowed || saturated.EstimatedPeakBytes != math.MaxInt64 {
		t.Fatalf("overflow must saturate and fail the finite budget: %+v", saturated)
	}
	if saturatingMultiply(-1, 2) != math.MaxInt64 || saturatingMultiply(math.MaxInt64, 2) != math.MaxInt64 || saturatingMultiply(3, 4) != 12 {
		t.Fatal("saturatingMultiply did not handle negative, overflow, and ordinary inputs")
	}
	if saturatingSum(1, -1) != math.MaxInt64 || saturatingSum(math.MaxInt64, 1) != math.MaxInt64 || saturatingSum(2, 3) != 5 {
		t.Fatal("saturatingSum did not handle negative, overflow, and ordinary inputs")
	}
}

func TestValidateResponseExercisesPublicationPolicy(t *testing.T) {
	packet := EvidencePacket{
		PacketID: "packet", Subject: rkcmodel.Node{ID: "subject", Name: "Login", QualifiedName: "auth.Login", Signature: "func Login(User) Result"},
		RelatedNodes:           []rkcmodel.Node{{ID: "helper", Name: "Helper"}},
		Evidence:               []rkcmodel.Evidence{{ID: "e1"}, {ID: "e2"}},
		AllowedClaimCategories: []string{"purpose"},
		Policy:                 PacketPolicy{RequireCitations: true, MaximumClaims: 20, MaximumSummaryCharacters: 4},
	}
	longClaim := strings.Repeat("x", 2001)
	response := Response{ModelID: "model", Summary: "<b>`Missing`</b>", Claims: []ClaimDraft{
		{Text: "`auth.Login` exists.", Category: "purpose", Certainty: "supported", EvidenceIDs: []string{"e2", "e1"}},
		{Text: "", Category: "wrong", Certainty: "invalid"},
		{Text: longClaim, Category: "purpose", Certainty: "supported"},
		{Text: "<script>alert(1)</script>", Category: "purpose", Certainty: "supported", EvidenceIDs: []string{"e1"}},
		{Text: "duplicate", Category: "purpose", Certainty: "supported", EvidenceIDs: []string{"e1", "e1", "outside"}},
		{Text: "inference", Category: "purpose", Certainty: "inferred", EvidenceIDs: []string{"e1"}},
		{Text: "uncertain", Category: "purpose", Certainty: "uncertain", EvidenceIDs: []string{"e1"}},
		{Text: "contradicted", Category: "purpose", Certainty: "contradicted", EvidenceIDs: []string{"e1"}},
		{Text: "`MissingSymbol` exists", Category: "purpose", Certainty: "supported", EvidenceIDs: []string{"e1"}},
	}}
	validation := ValidateResponse(packet, response, "v1")
	if len(validation.Accepted) != 1 || len(validation.Rejected) != len(response.Claims)-1 {
		t.Fatalf("unexpected accepted/rejected partition: %+v", validation)
	}
	accepted := validation.Accepted[0]
	if accepted.Generator != "model" || accepted.GeneratorVersion != "v1" || strings.Join(accepted.EvidenceIDs, ",") != "e1,e2" {
		t.Fatalf("accepted claim provenance was not normalized: %+v", accepted)
	}
	for _, reason := range []string{
		"free-form summary lacks claim-level evidence binding",
		"summary contains unsafe markup",
		"summary exceeds packet character limit",
		"summary appears to mention an unknown code identifier",
	} {
		if !containsString(validation.SummaryRejectedReasons, reason) {
			t.Fatalf("missing summary rejection %q: %+v", reason, validation)
		}
	}
	if len(validation.Diagnostics) != len(validation.Rejected)+1 {
		t.Fatalf("each rejection must be auditable: %+v", validation.Diagnostics)
	}

	inferred := ValidateResponse(EvidencePacket{
		PacketID: "p", Subject: rkcmodel.Node{ID: "n", Name: "Login"}, Evidence: []rkcmodel.Evidence{{ID: "e"}},
		Policy: PacketPolicy{AllowInference: true},
	}, Response{Claims: []ClaimDraft{{Text: "`Login` may run.", Certainty: "inferred", EvidenceIDs: []string{"e"}}}}, "v")
	if len(inferred.Accepted) != 1 {
		t.Fatalf("explicitly allowed inference was rejected: %+v", inferred)
	}

	claims := make([]ClaimDraft, 13)
	for i := range claims {
		claims[i] = ClaimDraft{Text: "plain statement", Certainty: "supported"}
	}
	defaultLimit := ValidateResponse(EvidencePacket{PacketID: "p", Subject: rkcmodel.Node{ID: "n"}}, Response{Claims: claims}, "v")
	if len(defaultLimit.Accepted) != 12 || len(defaultLimit.Rejected) != 1 || !containsString(defaultLimit.Rejected[0].Reasons, "claim count exceeds packet policy") {
		t.Fatalf("default claim ceiling was not enforced: %+v", defaultLimit)
	}

	known := map[string]struct{}{"Login": {}}
	if mentionsImpossibleIdentifier("plain text and ``", known) || mentionsImpossibleIdentifier("`Login` and `auth.Login`", known) || !mentionsImpossibleIdentifier("`Unknown`", known) {
		t.Fatal("identifier guard did not distinguish known, qualified, empty, and unknown identifiers")
	}
}

func TestResponseProtocolRejectsMalformedOrAmbiguousOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"no object", "model startup only"},
		{"unterminated", `noise {"claims":[`},
		{"malformed", `{"claims":oops}`},
		{"unknown field", `{"claims":[],"future":true}`},
		{"empty", `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseResponse([]byte(test.output)); !errors.Is(err, ErrModelOutputInvalid) {
				t.Fatalf("expected strict protocol rejection, got %v", err)
			}
		})
	}
	response, err := ParseResponse([]byte(`prefix {"summary":"quoted \"brace } safe","claims":[]} suffix {"ignored":true}`))
	if err != nil || !strings.Contains(response.Summary, "brace } safe") {
		t.Fatalf("balanced parser mishandled escaped quote/brace: response=%+v err=%v", response, err)
	}
}

func TestLlamaCPPProviderConfigurationAndPolicy(t *testing.T) {
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{}); err == nil || !strings.Contains(err.Error(), "model path is required") {
		t.Fatalf("missing model path was accepted: %v", err)
	}
	dummyDigest := strings.Repeat("0", sha256.Size*2)
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: filepath.Join(t.TempDir(), "missing"), ExpectedModelSHA256: dummyDigest, ExpectedExecutableSHA256: dummyDigest, UnsafeDisableResourceGuard: true}); err == nil || !strings.Contains(err.Error(), "resolve GGUF model") {
		t.Fatalf("missing model file was accepted: %v", err)
	}
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: t.TempDir(), ExpectedModelSHA256: dummyDigest, ExpectedExecutableSHA256: dummyDigest, UnsafeDisableResourceGuard: true}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory model was accepted: %v", err)
	}
	model := filepath.Join(t.TempDir(), "tiny.gguf")
	if err := os.WriteFile(model, []byte(ggufMagic), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: model}); !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "expected SHA-256") {
		t.Fatalf("missing expected model digest was accepted: %v", err)
	}
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: model, ExpectedModelSHA256: testFileSHA256(t, model)}); !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("missing expected executable digest was accepted: %v", err)
	}
	if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: model, Threads: 65}); err == nil || !strings.Contains(err.Error(), "threads") {
		t.Fatalf("unsafe thread count was accepted: %v", err)
	}
	for _, argument := range []string{"--model", "--ctx-size=12", "--device"} {
		if _, err := NewLlamaCPPProvider(LlamaCPPConfig{ModelPath: model, AdditionalArguments: []string{argument}}); err == nil || !strings.Contains(err.Error(), "controlled by RKC policy") {
			t.Fatalf("reserved argument %q was accepted: %v", argument, err)
		}
	}
	executable := filepath.Join(t.TempDir(), "llama-cli")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewLlamaCPPProvider(LlamaCPPConfig{
		Executable: executable, ModelPath: model, ModelID: "chosen", AdditionalArguments: []string{"--no-warmup"},
		ExpectedExecutableSHA256: testFileSHA256(t, executable), ExpectedModelSHA256: testFileSHA256(t, model),
		ModelRevision: "model-revision", ModelLicense: "Apache-2.0", RuntimeRevision: "runtime-revision",
		UnsafeDisableResourceGuard: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := provider.Descriptor()
	if descriptor.ID != "chosen" || descriptor.WeightBytes != 4 || descriptor.Digest != "sha256:"+testFileSHA256(t, model) || descriptor.RuntimeDigest != "sha256:"+testFileSHA256(t, executable) || descriptor.Revision != "model-revision" || descriptor.License != "Apache-2.0" || descriptor.RuntimeRevision != "runtime-revision" {
		t.Fatalf("descriptor/default provider contract failed: %+v", provider.Descriptor())
	}
	for _, task := range []Task{TaskSymbolSummary, TaskModuleSummary, TaskExecutionExplanation, TaskGapAnalysis} {
		if !provider.Supports(task) {
			t.Fatalf("provider unexpectedly rejected task %q", task)
		}
	}
	if provider.Supports(Task("future")) {
		t.Fatal("provider accepted an unknown task")
	}
	if _, err := provider.Generate(context.Background(), Request{Task: Task("future")}); !errors.Is(err, ErrUnsupportedTask) {
		t.Fatalf("Generate did not reject unsupported task: %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("idempotent close failed: %v", err)
	}
	if _, err := provider.Generate(context.Background(), Request{Task: TaskSymbolSummary}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed provider accepted generation: %v", err)
	}
}

func TestLlamaCPPProviderValidatesInferenceOptions(t *testing.T) {
	provider := testLlamaProvider(t, `printf '%s\n' '{"claims":[{"text":"ok","certainty":"supported","evidence_ids":[]}]}'`, LlamaCPPConfig{})
	base := Request{Task: TaskSymbolSummary, Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}}}
	tests := []struct {
		name    string
		options InferenceOptions
		want    string
	}{
		{"context high", InferenceOptions{ContextTokens: provider.config.ContextLimit + 1}, "context tokens"},
		{"output high", InferenceOptions{ContextTokens: 8, MaxOutputTokens: 9}, "output tokens"},
		{"threads high", InferenceOptions{ContextTokens: 8, MaxOutputTokens: 1, Threads: 65}, "threads"},
		{"batch high", InferenceOptions{ContextTokens: 8, MaxOutputTokens: 1, Threads: 1, BatchSize: 4097}, "batch size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Options = test.options
			if _, err := provider.Generate(context.Background(), request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q validation failure, got %v", test.want, err)
			}
		})
	}
}

func TestLlamaCPPProviderProcessFailuresAndBounds(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		stdoutMax  int64
		stderrMax  int64
		want       string
		wantTarget error
	}{
		{"stderr", `printf '%s' 'boom' >&2; exit 7`, 1024, 1024, "boom", nil},
		{"long stderr", `head -c 1300 /dev/zero | tr '\000' x >&2; exit 7`, 1024, 4096, "...", nil},
		{"no stderr", `exit 7`, 1024, 1024, "process failed", nil},
		{"invalid response", `printf '%s' '{}'`, 1024, 1024, "empty response", ErrModelOutputInvalid},
		{"stdout bounded", `head -c 128 /dev/zero | tr '\000' x`, 8, 1024, "", ErrModelOutputTooLarge},
		{"stderr bounded", `head -c 128 /dev/zero | tr '\000' x >&2; exit 7`, 1024, 8, "", ErrModelOutputTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := testLlamaProvider(t, test.script, LlamaCPPConfig{MaximumStdoutBytes: test.stdoutMax, MaximumStderrBytes: test.stderrMax})
			_, err := provider.Generate(context.Background(), Request{Task: TaskSymbolSummary, Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}}, Options: InferenceOptions{ContextTokens: 8, MaxOutputTokens: 1, KVBytesPerToken: 1, RuntimeOverheadBytes: 1}})
			if err == nil || (test.wantTarget != nil && !errors.Is(err, test.wantTarget)) || (test.want != "" && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("unexpected process result: %v", err)
			}
		})
	}

	provider := testLlamaProvider(t, `sleep 10`, LlamaCPPConfig{Timeout: 50 * time.Millisecond})
	_, err := provider.Generate(context.Background(), Request{Task: TaskSymbolSummary, Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}}, Options: InferenceOptions{ContextTokens: 8, MaxOutputTokens: 1, KVBytesPerToken: 1, RuntimeOverheadBytes: 1}})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("provider deadline was not surfaced: %v", err)
	}
}

func TestBuffersEnvironmentAndSmallHelpers(t *testing.T) {
	buffer := &boundedBuffer{limit: 3}
	if n, err := buffer.Write([]byte("ab")); err != nil || n != 2 {
		t.Fatalf("bounded write failed: n=%d err=%v", n, err)
	}
	if n, err := buffer.Write([]byte("cd")); !errors.Is(err, ErrModelOutputTooLarge) || n != 1 || buffer.String() != "abc" {
		t.Fatalf("partial bounded write mismatch: n=%d err=%v value=%q", n, err, buffer.String())
	}
	if n, err := buffer.Write([]byte("x")); !errors.Is(err, ErrModelOutputTooLarge) || n != 0 {
		t.Fatalf("latched buffer error mismatch: n=%d err=%v", n, err)
	}
	empty := &boundedBuffer{limit: 0}
	if _, err := empty.Write([]byte("x")); !errors.Is(err, ErrModelOutputTooLarge) {
		t.Fatalf("zero limit was not enforced: %v", err)
	}

	t.Setenv("RKC_SECRET_TEST", "must-not-pass")
	environment := resourceguard.SanitizedModelEnvironment([]string{"OMP_NUM_THREADS=1", "RKC_SECRET_TEST=leak", "MALFORMED"})
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "OMP_NUM_THREADS=1") || strings.Contains(joined, "RKC_SECRET_TEST") || !strings.Contains(joined, "CUDA_VISIBLE_DEVICES=-1") {
		t.Fatalf("model environment was not allowlisted: %q", joined)
	}
	if minInt(1, 2) != 1 || minInt(2, 1) != 1 || maxInt(1, 2) != 2 || maxInt(2, 1) != 2 {
		t.Fatal("integer helpers returned incorrect extrema")
	}
}

func TestPacketBuilderTruncationTasksAndSourceFailures(t *testing.T) {
	if _, err := BuildEvidencePacket(rkcmodel.Bundle{}, "", PacketBuildOptions{}); err == nil {
		t.Fatal("empty subject ID was accepted")
	}
	if _, err := BuildEvidencePacket(rkcmodel.Bundle{}, "missing", PacketBuildOptions{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing subject was accepted: %v", err)
	}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "s"},
		Nodes: []rkcmodel.Node{
			{ID: "n", EvidenceIDs: []string{"e1", "e2"}},
			{ID: "a", EvidenceIDs: []string{"e1"}},
			{ID: "b", EvidenceIDs: []string{"e2"}},
		},
		Edges: []rkcmodel.Edge{
			{ID: "2", Kind: "z", From: "n", To: "b", EvidenceIDs: []string{"e2"}},
			{ID: "1", Kind: "a", From: "a", To: "n", EvidenceIDs: []string{"e1"}},
			{ID: "3", Kind: "b", From: "n", To: "b", EvidenceIDs: []string{"e2"}},
			{ID: "self", Kind: "self", From: "n", To: "n"},
		},
		Evidence: []rkcmodel.Evidence{{ID: "e1"}, {ID: "e2"}},
	}
	report, err := BuildEvidencePacket(bundle, "n", PacketBuildOptions{MaximumEdges: 3, MaximumRelatedNodes: 1, MaximumEvidence: 1, Task: TaskGapAnalysis})
	if err != nil {
		t.Fatal(err)
	}
	if !report.TruncatedEdges || !report.TruncatedRelatedNodes || !report.TruncatedEvidence || len(report.Packet.RelatedNodes) != 1 || len(report.Packet.Evidence) != 1 {
		t.Fatalf("packet bounds were not explicit: %+v", report)
	}
	for _, task := range []Task{TaskModuleSummary, TaskExecutionExplanation, TaskGapAnalysis, TaskSymbolSummary} {
		if len(allowedCategories(task)) == 0 {
			t.Fatalf("task %q has no allowed claim categories", task)
		}
	}

	if excerpts, stats := buildSourceExcerpts(nil, nil, PacketBuildOptions{}); excerpts != nil || len(stats.missing) != 0 {
		t.Fatalf("source-free packet unexpectedly read files: excerpts=%v stats=%+v", excerpts, stats)
	}
	root := t.TempDir()
	contents := []byte("one\ntwo\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := packetArtifact("a", "source.txt", contents)
	evidence := []rkcmodel.Evidence{
		{ID: "none"},
		{ID: "missing", Source: &rkcmodel.SourceRange{ArtifactID: "", Path: "source.txt", StartLine: 1}},
		{ID: "valid", Source: &rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 1}},
		{ID: "duplicate", Source: &rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 1}},
		{ID: "past-budget", Source: &rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 2}},
	}
	excerpts, stats := buildSourceExcerpts(evidence, map[string]rkcmodel.Artifact{"a": artifact}, PacketBuildOptions{RepositoryRoot: root, MaximumExcerptBytes: 32, MaximumTotalSourceBytes: 4, RedactSecrets: true})
	if len(excerpts) != 1 || len(stats.missing) != 1 || !stats.truncated {
		t.Fatalf("source selection accounting mismatch: excerpts=%+v stats=%+v", excerpts, stats)
	}
}

func TestSourceRangeAndPathValidationBranches(t *testing.T) {
	root := t.TempDir()
	contents := []byte("one\ntwo\n")
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := packetArtifact("a", "source.txt", contents)
	tests := []struct {
		name     string
		artifact rkcmodel.Artifact
		source   rkcmodel.SourceRange
		maximum  int
		want     string
	}{
		{"zero maximum", artifact, rkcmodel.SourceRange{}, 0, ""},
		{"artifact mismatch", artifact, rkcmodel.SourceRange{ArtifactID: "b"}, 4, "identify"},
		{"path mismatch", artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "other"}, 4, "does not match"},
		{"non text", func() rkcmodel.Artifact { value := artifact; value.Text = false; return value }(), rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt"}, 4, "not inventoried as text"},
		{"negative size", func() rkcmodel.Artifact { value := artifact; value.SizeBytes = -1; return value }(), rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt"}, 4, "invalid inventoried size"},
		{"bad digest", func() rkcmodel.Artifact { value := artifact; value.SHA256 = "bad"; return value }(), rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt"}, 4, "valid inventoried SHA-256"},
		{"start line zero", artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", EndLine: 1}, 4, "start line"},
		{"inverted lines", artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 2, EndLine: 1}, 4, "inverted"},
		{"no span", artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt"}, 4, "no bounded"},
		{"outside lines", artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 3}, 4, "outside"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, truncated, err := readSourceRange(root, test.artifact, test.source, test.maximum)
			if test.maximum == 0 {
				if err != nil || !truncated {
					t.Fatalf("zero bound result: truncated=%v err=%v", truncated, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
	text, truncated, err := readSourceRange(root, artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartLine: 2}, 32)
	if err != nil || truncated || string(text) != "two\n" {
		t.Fatalf("single defaulted line range mismatch: text=%q truncated=%v err=%v", text, truncated, err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(root, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolvePacketSource(symlinkRoot, "source.txt"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink repository root was accepted: %v", err)
	}
	if _, _, err := resolvePacketSource(outside, "child"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("file repository root was accepted: %v", err)
	}
	for _, source := range []string{"", ".", "/absolute", "dir/../source.txt", "missing", "source.txt/child"} {
		if _, _, err := resolvePacketSource(root, source); err == nil {
			t.Fatalf("unsafe/nonexistent source path %q was accepted", source)
		}
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolvePacketSource(root, "directory"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory source was accepted: %v", err)
	}
	if got := appendBounded([]byte("full"), []byte("ignored"), 4); string(got) != "full" {
		t.Fatalf("append beyond bound mutated target: %q", got)
	}
}

func testLlamaProvider(t *testing.T, body string, override LlamaCPPConfig) *LlamaCPPProvider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	root := t.TempDir()
	model := filepath.Join(root, "tiny.gguf")
	if err := os.WriteFile(model, []byte(ggufMagic), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fake-llama")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	override.Executable = executable
	override.ModelPath = model
	override.ExpectedExecutableSHA256 = testFileSHA256(t, executable)
	override.ExpectedModelSHA256 = testFileSHA256(t, model)
	override.UnsafeDisableResourceGuard = true
	if override.Budget.MaximumRSSBytes == 0 {
		override.Budget.MaximumRSSBytes = 64 << 20
	}
	provider, err := NewLlamaCPPProvider(override)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
