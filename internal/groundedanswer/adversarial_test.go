package groundedanswer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestAdversarialModelClaimsAreIndividuallyRejected(t *testing.T) {
	provider := testProvider()
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		return modelruntime.Response{
			RequestID: request.RequestID,
			ModelID:   provider.descriptor.ID,
			Summary:   "<script>unsupported summary</script>",
			Claims: []modelruntime.ClaimDraft{
				{Text: "`Alpha` calls `Beta`.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-edge"}},
				{Text: "An uncited assertion.", Category: "answer", Certainty: "supported"},
				{Text: "Outside evidence.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"forged-evidence"}},
				{Text: "Duplicate citations.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-alpha", "e-alpha"}},
				{Text: "<b>Unsafe</b> markup.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-alpha"}},
				{Text: "An inference.", Category: "answer", Certainty: "inferred", EvidenceIDs: []string{"e-alpha"}},
				{Text: "Invalid certainty.", Category: "answer", Certainty: "certain", EvidenceIDs: []string{"e-alpha"}},
				{Text: "Unknown `FabricatedAPI`.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-alpha"}},
				{Text: "Wrong category.", Category: "marketing", Certainty: "supported", EvidenceIDs: []string{"e-alpha"}},
				{Text: "`Alpha` calls `Beta`.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-edge"}},
			},
			UnresolvedQuestions: []string{
				" z question ", "a question", "a question", strings.Repeat("x", 40), "extra question",
			},
		}
	}
	service, err := New(provider, Options{MaximumUnresolved: 3, MaximumFieldBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Answer(context.Background(), Request{
		Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: testBundle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAnswered || len(result.Claims) != 1 || result.Claims[0].Text != "`Alpha` calls `Beta`." || len(result.Citations) != 1 {
		t.Fatalf("adversarial result = %+v", result)
	}
	if len(result.Audit.RejectedClaims) != 9 {
		t.Fatalf("rejected claims = %d: %+v", len(result.Audit.RejectedClaims), result.Audit.RejectedClaims)
	}
	reasons := strings.Builder{}
	for _, rejection := range result.Audit.RejectedClaims {
		reasons.WriteString(strings.Join(rejection.Reasons, ";"))
		reasons.WriteByte('\n')
	}
	for _, fragment := range []string{
		"claim is uncited", "evidence outside packet: forged-evidence", "duplicate evidence citation: e-alpha",
		"claim contains unsafe markup", "inference is disabled", "certainty is invalid: certain",
		"claim appears to mention an unknown code identifier", "claim category is not allowed: marketing",
		"duplicate grounded claim",
	} {
		if !strings.Contains(reasons.String(), fragment) {
			t.Errorf("missing rejection reason %q in:\n%s", fragment, reasons.String())
		}
	}
	if !reflect.DeepEqual(result.Audit.SummaryRejectedReasons, []string{
		"free-form summary lacks claim-level evidence binding", "summary contains unsafe markup",
	}) {
		t.Fatalf("summary rejection = %+v", result.Audit.SummaryRejectedReasons)
	}
	if !result.Truncation.ModelUnresolved || !reflect.DeepEqual(result.Audit.UntrustedUnresolvedQuestions, []string{"a question", "z question"}) {
		t.Fatalf("bounded unresolved questions = %+v, truncation=%+v", result.Audit.UntrustedUnresolvedQuestions, result.Truncation)
	}
}

func TestNoValidModelClaimProducesAuditableAbstention(t *testing.T) {
	provider := testProvider()
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		return modelruntime.Response{
			RequestID: request.RequestID, ModelID: provider.descriptor.ID,
			Claims:              []modelruntime.ClaimDraft{{Text: "Unsupported.", Category: "answer", Certainty: "supported"}},
			UnresolvedQuestions: []string{"Which evidence supports this?"},
		}
	}
	service, _ := New(provider, Options{})
	result, err := service.Answer(context.Background(), Request{
		Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: testBundle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAbstained || result.Abstention == nil || result.Abstention.Code != AbstentionNoValidClaims ||
		len(result.Claims) != 0 || len(result.Citations) != 0 || len(result.Audit.RejectedClaims) != 1 || provider.calls != 1 {
		t.Fatalf("no-valid-claims result = %+v calls=%d", result, provider.calls)
	}
}

func TestModelResponseEnvelopeMustMatchBoundProviderRequest(t *testing.T) {
	tests := []struct {
		name    string
		respond func(*stubProvider, modelruntime.Request) modelruntime.Response
		want    string
	}{
		{
			"request mismatch",
			func(provider *stubProvider, request modelruntime.Request) modelruntime.Response {
				return modelruntime.Response{RequestID: "other", ModelID: provider.descriptor.ID}
			},
			"response request ID",
		},
		{
			"model mismatch",
			func(_ *stubProvider, request modelruntime.Request) modelruntime.Response {
				return modelruntime.Response{RequestID: request.RequestID, ModelID: "other-model"}
			},
			"response model ID",
		},
		{
			"negative usage",
			func(provider *stubProvider, request modelruntime.Request) modelruntime.Response {
				return modelruntime.Response{RequestID: request.RequestID, ModelID: provider.descriptor.ID, Usage: modelruntime.Usage{OutputTokens: -1}}
			},
			"usage counters",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := testProvider()
			provider.respond = func(request modelruntime.Request) modelruntime.Response { return test.respond(provider, request) }
			service, _ := New(provider, Options{})
			_, err := service.Answer(context.Background(), Request{
				Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: testBundle(),
			})
			if !errors.Is(err, ErrModelProtocol) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("protocol error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProviderCannotMutateAuthoritativePacketOrIdentity(t *testing.T) {
	provider := testProvider()
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		request.Packet.RelatedNodes[0].ID = "forged-node"
		request.Packet.RelatedNodes[0].EvidenceIDs = []string{"forged-evidence"}
		request.Packet.Evidence[0].ID = "forged-evidence"
		return modelruntime.Response{
			RequestID: request.RequestID, ModelID: provider.descriptor.ID,
			Claims: []modelruntime.ClaimDraft{{
				Text: "`Alpha` is declared.", Category: "answer", Certainty: "supported", EvidenceIDs: []string{"e-alpha"},
			}},
		}
	}
	service, _ := New(provider, Options{})
	result, err := service.Answer(context.Background(), Request{
		Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: testBundle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAnswered || len(result.Claims) != 1 ||
		!reflect.DeepEqual(result.Claims[0].EvidenceIDs, []string{"e-alpha"}) ||
		!reflect.DeepEqual(result.Claims[0].NodeIDs, []string{"node-alpha"}) {
		t.Fatalf("provider mutated authoritative validation state: %+v", result)
	}

	provider.descriptor.ID = "changed-after-construction"
	if _, err := service.Answer(context.Background(), Request{Question: "Question", Bundle: testBundle()}); !errors.Is(err, ErrInvalidProvider) || provider.calls != 1 {
		t.Fatalf("descriptor mutation error=%v calls=%d", err, provider.calls)
	}
}

func TestCancellationAfterNonCooperativeProviderIsHonored(t *testing.T) {
	provider := testProvider()
	ctx, cancel := context.WithCancel(context.Background())
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		cancel()
		return modelruntime.Response{RequestID: request.RequestID, ModelID: provider.descriptor.ID}
	}
	service, _ := New(provider, Options{})
	_, err := service.Answer(ctx, Request{Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: testBundle()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-provider cancellation error = %v", err)
	}
}

func TestCanonicalBundleIntegrityFailuresAreRejectedBeforeInference(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*rkcmodel.Bundle)
		want   string
	}{
		{"missing snapshot", func(bundle *rkcmodel.Bundle) { bundle.Snapshot.ID = "" }, "snapshot ID"},
		{"duplicate artifact", func(bundle *rkcmodel.Bundle) { bundle.Artifacts = append(bundle.Artifacts, bundle.Artifacts[0]) }, "duplicate artifact"},
		{"duplicate node", func(bundle *rkcmodel.Bundle) { bundle.Nodes = append(bundle.Nodes, bundle.Nodes[0]) }, "duplicate node"},
		{"duplicate evidence", func(bundle *rkcmodel.Bundle) { bundle.Evidence = append(bundle.Evidence, bundle.Evidence[0]) }, "duplicate evidence"},
		{"duplicate document", func(bundle *rkcmodel.Bundle) { bundle.Documents = append(bundle.Documents, bundle.Documents[0]) }, "duplicate document"},
		{"duplicate edge", func(bundle *rkcmodel.Bundle) { bundle.Edges = append(bundle.Edges, bundle.Edges[0]) }, "duplicate edge"},
		{"ambiguous retrievable", func(bundle *rkcmodel.Bundle) { bundle.Documents[0].ID = bundle.Nodes[0].ID }, "ambiguous"},
		{"missing node artifact", func(bundle *rkcmodel.Bundle) { bundle.Nodes[0].ArtifactID = "missing" }, "missing artifact"},
		{"missing node evidence", func(bundle *rkcmodel.Bundle) { bundle.Nodes[0].EvidenceIDs = []string{"missing"} }, "missing evidence"},
		{"missing document subject", func(bundle *rkcmodel.Bundle) { bundle.Documents[0].SubjectIDs = []string{"missing"} }, "missing subject"},
		{"missing edge from", func(bundle *rkcmodel.Bundle) { bundle.Edges[0].From = "missing" }, "missing from-node"},
		{"missing edge to", func(bundle *rkcmodel.Bundle) { bundle.Edges[0].To = "missing" }, "missing to-node"},
		{"missing edge evidence", func(bundle *rkcmodel.Bundle) { bundle.Edges[0].EvidenceIDs = []string{"missing"} }, "references missing evidence"},
		{"oversized identifier", func(bundle *rkcmodel.Bundle) {
			bundle.Nodes[0].ID = strings.Repeat("n", maximumStableIdentifierBytes+1)
		}, "exceeds"},
		{"whitespace identifier", func(bundle *rkcmodel.Bundle) { bundle.Evidence[0].ID = " e-alpha" }, "surrounding whitespace"},
		{"non-json attribute", func(bundle *rkcmodel.Bundle) { bundle.Nodes[0].Attributes["bad"] = func() {} }, "encode bundle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := testProvider()
			service, _ := New(provider, Options{})
			bundle := testBundle()
			test.mutate(&bundle)
			_, err := service.Answer(context.Background(), Request{
				Question: "How does Alpha reach Beta?", Retrieval: testRetrieval(), Bundle: bundle,
			})
			if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), test.want) || provider.calls != 0 {
				t.Fatalf("bundle error = %v, want %q, calls=%d", err, test.want, provider.calls)
			}
		})
	}
}

func TestHelperBoundsRemainUTF8SafeAndStable(t *testing.T) {
	if got := truncateUTF8("a界b", 3); got != "a" || !utf8.ValidString(got) {
		t.Fatalf("truncateUTF8 = %q", got)
	}
	if got := truncateUTF8("abc", 0); got != "" {
		t.Fatalf("zero truncate = %q", got)
	}
	if got := truncateUTF8("abc", 3); got != "abc" {
		t.Fatalf("exact truncate = %q", got)
	}
	if got := uniqueStrings([]string{"a", "a", "b", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("uniqueStrings = %v", got)
	}
	if got := uniqueStrings([]string{"a"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("single uniqueStrings = %v", got)
	}
	if digest := contentDigest([]byte("same")); !strings.HasPrefix(digest, "sha256:") || digest != contentDigest([]byte("same")) || digest == contentDigest([]byte("different")) {
		t.Fatalf("content digest = %q", digest)
	}
}

func TestRetrievalHitIdentityValidationIsNonAuthoritative(t *testing.T) {
	provider := testProvider()
	service, _ := New(provider, Options{})
	retrieval := search.Response{Hits: []search.Hit{
		{Document: search.Document{ID: " node-alpha"}},
		{Document: search.Document{ID: strings.Repeat("x", maximumStableIdentifierBytes+1)}},
		{Document: search.Document{ID: "node-alpha"}},
		{Document: search.Document{ID: "doc-beta"}},
	}}
	result, err := service.Answer(context.Background(), Request{Question: "How does Alpha reach Beta?", Retrieval: retrieval, Bundle: testBundle()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAnswered || result.Truncation.UnknownHits != 2 || provider.calls != 1 {
		t.Fatalf("untrusted hit IDs = %+v calls=%d", result, provider.calls)
	}
}
