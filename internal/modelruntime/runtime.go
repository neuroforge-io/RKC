// Package modelruntime defines the optional bounded language-model layer. The
// deterministic graph remains authoritative; providers receive evidence packets
// and return structured claims that must survive validation before publication.
package modelruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

var (
	// ErrBudgetExceeded is wrapped when model admission estimates or enforced
	// runtime memory limits refuse an inference request.
	ErrBudgetExceeded = errors.New("model memory budget exceeded")
	// ErrUnsupportedTask reports that a provider does not implement the requested
	// model protocol task.
	ErrUnsupportedTask = errors.New("model provider does not support task")
)

// Task identifies the bounded synthesis operation requested from a Provider.
type Task string

const (
	// TaskSymbolSummary requests evidence-bound claims about one symbol.
	TaskSymbolSummary Task = "symbol_summary"
	// TaskModuleSummary requests evidence-bound claims about a module's purpose,
	// responsibilities, dependencies, surface, risks, and limitations.
	TaskModuleSummary Task = "module_summary"
	// TaskExecutionExplanation requests evidence-bound execution-flow claims.
	TaskExecutionExplanation Task = "execution_explanation"
	// TaskGapAnalysis requests evidence-bound documentation, test, relationship,
	// conflict, and limitation findings.
	TaskGapAnalysis Task = "documentation_gap_analysis"
)

// ModelDescriptor is provider-reported model and runtime provenance used for
// admission estimates and audit output. The record is not self-authenticating;
// callers requiring integrity must bind and verify its digests and revisions.
type ModelDescriptor struct {
	ID               string `json:"id"`
	Architecture     string `json:"architecture,omitempty"`
	ParameterCount   int64  `json:"parameter_count,omitempty"`
	QuantizationBits int    `json:"quantization_bits,omitempty"`
	WeightBytes      int64  `json:"weight_bytes"`
	ContextLimit     int    `json:"context_limit"`
	Tokenizer        string `json:"tokenizer,omitempty"`
	Digest           string `json:"digest,omitempty"`
	Revision         string `json:"revision,omitempty"`
	License          string `json:"license,omitempty"`
	Runtime          string `json:"runtime,omitempty"`
	RuntimeDigest    string `json:"runtime_digest,omitempty"`
	RuntimeRevision  string `json:"runtime_revision,omitempty"`
}

// InferenceOptions requests context, output, concurrency, and memory-estimation
// parameters. Providers apply defaults and validate or further restrict these
// values before execution.
type InferenceOptions struct {
	ContextTokens        int   `json:"context_tokens"`
	MaxOutputTokens      int   `json:"max_output_tokens"`
	Threads              int   `json:"threads,omitempty"`
	BatchSize            int   `json:"batch_size,omitempty"`
	Parallel             int   `json:"parallel,omitempty"`
	KVBytesPerToken      int64 `json:"kv_bytes_per_token,omitempty"`
	RuntimeOverheadBytes int64 `json:"runtime_overhead_bytes,omitempty"`
}

// Budget configures memory admission arithmetic. MaximumRSSBytes less than or
// equal to zero disables EstimateMemory's RSS ceiling, while SafetyMarginBytes is
// added to the predicted peak; runtime enforcement is provider-specific.
type Budget struct {
	MaximumRSSBytes   int64 `json:"maximum_rss_bytes"`
	SafetyMarginBytes int64 `json:"safety_margin_bytes,omitempty"`
}

// Estimate is the saturating component breakdown produced by EstimateMemory.
// Allowed records an admission calculation, not measured consumption or proof
// that a process will remain within the budget.
type Estimate struct {
	WeightsBytes       int64    `json:"weights_bytes"`
	KVCacheBytes       int64    `json:"kv_cache_bytes"`
	RuntimeBytes       int64    `json:"runtime_bytes"`
	PromptBytes        int64    `json:"prompt_bytes"`
	OutputBytes        int64    `json:"output_bytes"`
	EstimatedPeakBytes int64    `json:"estimated_peak_bytes"`
	Allowed            bool     `json:"allowed"`
	Reasons            []string `json:"reasons,omitempty"`
}

// EstimateMemory performs conservative, saturating admission arithmetic using
// documented defaults for missing context, output, parallelism, KV cost, and
// runtime overhead. It also checks the declared model context limit. It does not
// inspect the host, allocate memory, measure a model, or enforce a cgroup.
func EstimateMemory(model ModelDescriptor, options InferenceOptions, promptBytes int64, budget Budget) Estimate {
	if options.ContextTokens <= 0 {
		options.ContextTokens = 4096
	}
	if options.MaxOutputTokens <= 0 {
		options.MaxOutputTokens = 768
	}
	if options.Parallel <= 0 {
		options.Parallel = 1
	}
	if options.KVBytesPerToken <= 0 {
		// Conservative generic estimate. Providers should replace this with
		// architecture-specific measurements or metadata when available.
		options.KVBytesPerToken = 128 * 1024
	}
	if options.RuntimeOverheadBytes <= 0 {
		options.RuntimeOverheadBytes = 256 * 1024 * 1024
	}
	weights := model.WeightBytes
	if weights < 0 {
		weights = maxSignedInt64
	}
	if promptBytes < 0 {
		promptBytes = maxSignedInt64
	}
	estimate := Estimate{
		WeightsBytes: weights,
		KVCacheBytes: saturatingMultiply(saturatingMultiply(int64(options.ContextTokens), options.KVBytesPerToken), int64(options.Parallel)),
		RuntimeBytes: options.RuntimeOverheadBytes,
		PromptBytes:  promptBytes,
		OutputBytes:  saturatingMultiply(int64(options.MaxOutputTokens), 8),
	}
	estimate.EstimatedPeakBytes = saturatingSum(estimate.WeightsBytes, estimate.KVCacheBytes, estimate.RuntimeBytes, estimate.PromptBytes, estimate.OutputBytes, budget.SafetyMarginBytes)
	estimate.Allowed = budget.MaximumRSSBytes <= 0 || estimate.EstimatedPeakBytes <= budget.MaximumRSSBytes
	if !estimate.Allowed {
		estimate.Reasons = append(estimate.Reasons, fmt.Sprintf("estimated peak %d exceeds budget %d", estimate.EstimatedPeakBytes, budget.MaximumRSSBytes))
	}
	if model.ContextLimit > 0 && options.ContextTokens > model.ContextLimit {
		estimate.Allowed = false
		estimate.Reasons = append(estimate.Reasons, "requested context exceeds model context limit")
	}
	return estimate
}

const maxSignedInt64 = int64(^uint64(0) >> 1)

func saturatingMultiply(left, right int64) int64 {
	if left < 0 || right < 0 {
		return maxSignedInt64
	}
	if left != 0 && right > maxSignedInt64/left {
		return maxSignedInt64
	}
	return left * right
}

func saturatingSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value < 0 || value > maxSignedInt64-total {
			return maxSignedInt64
		}
		total += value
	}
	return total
}

// EvidencePacket is the bounded repository context presented to a Provider.
// Nodes, excerpts, identifiers, and source text remain untrusted data rather
// than instructions; packet construction and validation must precede inference.
type EvidencePacket struct {
	SchemaVersion          string              `json:"schema_version"`
	PacketID               string              `json:"packet_id"`
	SnapshotID             string              `json:"snapshot_id"`
	Task                   Task                `json:"task"`
	Subject                rkcmodel.Node       `json:"subject"`
	RelatedNodes           []rkcmodel.Node     `json:"related_nodes,omitempty"`
	Edges                  []rkcmodel.Edge     `json:"edges,omitempty"`
	Evidence               []rkcmodel.Evidence `json:"evidence"`
	SourceExcerpts         []SourceExcerpt     `json:"source_excerpts,omitempty"`
	AllowedClaimCategories []string            `json:"allowed_claim_categories,omitempty"`
	Policy                 PacketPolicy        `json:"policy"`
}

// SourceExcerpt carries untrusted source text and its claimed EvidenceID and
// range. This type alone does not prove that the excerpt matches either record.
type SourceExcerpt struct {
	EvidenceID string               `json:"evidence_id"`
	Source     rkcmodel.SourceRange `json:"source"`
	Text       string               `json:"text"`
	Truncated  bool                 `json:"truncated,omitempty"`
}

// PacketPolicy defines deterministic response admission constraints. A
// nonpositive MaximumClaims defaults to 12. MaximumSummaryCharacters can add a
// rejection reason, but protocol-v1 summaries are never publishable because they
// have no claim-level evidence binding.
type PacketPolicy struct {
	RequireCitations         bool `json:"require_citations"`
	AllowInference           bool `json:"allow_inference"`
	MaximumClaims            int  `json:"maximum_claims"`
	MaximumSummaryCharacters int  `json:"maximum_summary_characters"`
}

// Request combines an evidence packet, task, inference options, validation-pass
// number, and optional deadline for a Provider. Repository content inside Packet
// must remain inert untrusted data throughout generation.
type Request struct {
	RequestID      string           `json:"request_id"`
	Task           Task             `json:"task"`
	Packet         EvidencePacket   `json:"packet"`
	Options        InferenceOptions `json:"options"`
	ValidationPass int              `json:"validation_pass,omitempty"`
	Deadline       *time.Time       `json:"deadline,omitempty"`
}

// ClaimDraft is an untrusted model-generated claim candidate. EvidenceIDs are
// citation claims that ValidateResponse checks for packet membership; they are
// not by themselves proof that the cited evidence entails Text.
type ClaimDraft struct {
	Text        string   `json:"text"`
	Category    string   `json:"category,omitempty"`
	Certainty   string   `json:"certainty"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// Response is untrusted structured provider output awaiting ValidateResponse.
// ModelID and Usage are provider-reported metadata, and Summary cannot be
// promoted by protocol v1 because it lacks claim-level evidence bindings.
type Response struct {
	RequestID           string       `json:"request_id"`
	Summary             string       `json:"summary,omitempty"`
	Claims              []ClaimDraft `json:"claims,omitempty"`
	UnresolvedQuestions []string     `json:"unresolved_questions,omitempty"`
	ModelID             string       `json:"model_id"`
	Usage               Usage        `json:"usage,omitempty"`
}

// Usage is provider-reported inference telemetry. It is neither an attestation
// nor a substitute for independently enforced or measured resource limits.
type Usage struct {
	PromptTokens   int   `json:"prompt_tokens,omitempty"`
	OutputTokens   int   `json:"output_tokens,omitempty"`
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	PeakRSSBytes   int64 `json:"peak_rss_bytes,omitempty"`
}

// Provider is the lifecycle contract for a bounded model backend. Implementations
// describe supported tasks, generate structured untrusted responses, and release
// resources with Close. The interface makes no concurrency-safety guarantee.
type Provider interface {
	Descriptor() ModelDescriptor
	Supports(Task) bool
	Generate(context.Context, Request) (Response, error)
	Close() error
}

// ClaimValidation separates structurally admitted claim candidates from rejected
// drafts and records deterministic diagnostics. AcceptedSummary remains empty in
// protocol v1.
type ClaimValidation struct {
	Accepted               []rkcmodel.Claim      `json:"accepted,omitempty"`
	Rejected               []RejectedClaim       `json:"rejected,omitempty"`
	AcceptedSummary        string                `json:"accepted_summary,omitempty"`
	SummaryRejectedReasons []string              `json:"summary_rejected_reasons,omitempty"`
	Diagnostics            []rkcmodel.Diagnostic `json:"diagnostics,omitempty"`
}

// RejectedClaim retains an untrusted draft and the deterministic policy reasons
// that prevented publication.
type RejectedClaim struct {
	Claim   ClaimDraft `json:"claim"`
	Reasons []string   `json:"reasons"`
}

// ValidateResponse applies deterministic structural, length, markup, category,
// citation-ID, certainty, and identifier heuristics to model output. It does not
// establish semantic truth or entailment between a claim and cited evidence.
// Accepted claims therefore remain grounded candidates, while every nonempty
// protocol-v1 Summary is rejected because it has no claim-level evidence map.
func ValidateResponse(packet EvidencePacket, response Response, generatorVersion string) ClaimValidation {
	allowedEvidence := map[string]struct{}{}
	for _, evidence := range packet.Evidence {
		allowedEvidence[evidence.ID] = struct{}{}
	}
	knownTerms := map[string]struct{}{}
	for _, node := range append([]rkcmodel.Node{packet.Subject}, packet.RelatedNodes...) {
		for _, value := range []string{node.Name, node.QualifiedName, node.Signature} {
			for _, term := range identifierTerms(value) {
				knownTerms[term] = struct{}{}
			}
		}
	}
	maxClaims := packet.Policy.MaximumClaims
	if maxClaims <= 0 {
		maxClaims = 12
	}
	validation := ClaimValidation{}
	allowedCategories := map[string]struct{}{}
	for _, category := range packet.AllowedClaimCategories {
		allowedCategories[category] = struct{}{}
	}
	for index, draft := range response.Claims {
		var reasons []string
		if index >= maxClaims {
			reasons = append(reasons, "claim count exceeds packet policy")
		}
		if strings.TrimSpace(draft.Text) == "" {
			reasons = append(reasons, "empty claim")
		}
		if len([]rune(draft.Text)) > 2000 {
			reasons = append(reasons, "claim exceeds character limit")
		}
		if containsUnsafeMarkup(draft.Text) {
			reasons = append(reasons, "claim contains unsafe markup")
		}
		if !isAtomicClaim(draft.Text) {
			reasons = append(reasons, "claim must contain one atomic statement")
		}
		if len(allowedCategories) > 0 {
			if _, ok := allowedCategories[draft.Category]; !ok {
				reasons = append(reasons, "claim category is not allowed: "+draft.Category)
			}
		}
		if packet.Policy.RequireCitations && len(draft.EvidenceIDs) == 0 {
			reasons = append(reasons, "claim is uncited")
		}
		seenEvidence := map[string]struct{}{}
		for _, evidenceID := range draft.EvidenceIDs {
			if _, duplicate := seenEvidence[evidenceID]; duplicate {
				reasons = append(reasons, "duplicate evidence citation: "+evidenceID)
			}
			seenEvidence[evidenceID] = struct{}{}
			if _, ok := allowedEvidence[evidenceID]; !ok {
				reasons = append(reasons, "evidence outside packet: "+evidenceID)
			}
		}
		if draft.Certainty == "inferred" && !packet.Policy.AllowInference {
			reasons = append(reasons, "inference is disabled")
		}
		if _, ok := rkcmodel.ClaimCertaintyStates[draft.Certainty]; !ok {
			reasons = append(reasons, "certainty is invalid: "+draft.Certainty)
		} else if draft.Certainty == "uncertain" || draft.Certainty == "contradicted" {
			reasons = append(reasons, "certainty is not publishable: "+draft.Certainty)
		}
		if mentionsImpossibleIdentifier(draft.Text, knownTerms) {
			reasons = append(reasons, "claim appears to mention an unknown code identifier")
		}
		if len(reasons) > 0 {
			sort.Strings(reasons)
			validation.Rejected = append(validation.Rejected, RejectedClaim{Claim: draft, Reasons: reasons})
			continue
		}
		claim := rkcmodel.Claim{
			ID:        rkcmodel.StableID("claim", packet.PacketID, fmt.Sprintf("%d", index), draft.Text),
			SubjectID: packet.Subject.ID, Text: draft.Text, Category: draft.Category,
			Certainty: draft.Certainty, Generator: response.ModelID, GeneratorVersion: generatorVersion,
			EvidenceIDs: append([]string(nil), draft.EvidenceIDs...), Validation: "accepted",
		}
		sort.Strings(claim.EvidenceIDs)
		validation.Accepted = append(validation.Accepted, claim)
	}
	summary := strings.TrimSpace(response.Summary)
	if summary != "" {
		// Response.Summary has no claim-level evidence mapping in protocol v1.
		// Accepting it merely because some unrelated claim survived validation
		// would publish uncited model prose. Keep it in the audit record, but never
		// promote it to an accepted or rendered field.
		validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "free-form summary lacks claim-level evidence binding")
		if containsUnsafeMarkup(summary) {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary contains unsafe markup")
		}
		if packet.Policy.MaximumSummaryCharacters > 0 && len([]rune(summary)) > packet.Policy.MaximumSummaryCharacters {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary exceeds packet character limit")
		}
		if mentionsImpossibleIdentifier(summary, knownTerms) {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary appears to mention an unknown code identifier")
		}
	}
	for _, rejected := range validation.Rejected {
		validation.Diagnostics = append(validation.Diagnostics, rkcmodel.Diagnostic{
			ID:       rkcmodel.StableID("diagnostic", "model-claim-rejected", rejected.Claim.Text),
			Severity: "warning", Code: "RKC-MDL-001", Stage: "model_validate",
			Message: "model claim rejected: " + strings.Join(rejected.Reasons, "; "),
		})
	}
	if len(validation.SummaryRejectedReasons) > 0 {
		sort.Strings(validation.SummaryRejectedReasons)
		validation.Diagnostics = append(validation.Diagnostics, rkcmodel.Diagnostic{
			ID:       rkcmodel.StableID("diagnostic", "model-summary-rejected", packet.PacketID, summary),
			Severity: "warning", Code: "RKC-MDL-002", Stage: "model_validate",
			Message: "model summary rejected: " + strings.Join(validation.SummaryRejectedReasons, "; "),
		})
	}
	return validation
}

func containsUnsafeMarkup(value string) bool {
	lower := strings.ToLower(value)
	return strings.ContainsAny(value, "<>") || strings.Contains(lower, "javascript:") || strings.Contains(lower, "data:text/html")
}

func isAtomicClaim(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, ";\r\n") {
		return false
	}
	for index, character := range value {
		if character != '.' && character != '?' && character != '!' {
			continue
		}
		if index+1 < len(value) {
			next, _ := utf8.DecodeRuneInString(value[index+1:])
			if !unicode.IsSpace(next) {
				continue
			}
		}
		remainder := strings.TrimSpace(value[index+1:])
		if remainder != "" {
			return false
		}
	}
	return true
}

func identifierTerms(value string) []string {
	value = strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ", ",", " ", ":", " ", ".", " ", "/", " ", "*", " ", "&", " ").Replace(value)
	var terms []string
	for _, term := range strings.Fields(value) {
		term = strings.Trim(term, "`'\"")
		if len(term) >= 2 {
			terms = append(terms, term)
		}
	}
	return terms
}

// This is deliberately conservative. It only flags backtick-delimited tokens,
// because ordinary prose contains capitalized words that are not code symbols.
func mentionsImpossibleIdentifier(text string, known map[string]struct{}) bool {
	parts := strings.Split(text, "`")
	for index := 1; index < len(parts); index += 2 {
		identifier := strings.TrimSpace(parts[index])
		if identifier == "" {
			continue
		}
		if _, ok := known[identifier]; ok {
			continue
		}
		last := identifier
		if dot := strings.LastIndexAny(identifier, ".:/"); dot >= 0 {
			last = identifier[dot+1:]
		}
		if _, ok := known[last]; ok {
			continue
		}
		return true
	}
	return false
}
