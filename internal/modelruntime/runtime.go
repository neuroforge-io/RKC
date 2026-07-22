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

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

var (
	ErrBudgetExceeded  = errors.New("model memory budget exceeded")
	ErrUnsupportedTask = errors.New("model provider does not support task")
)

type Task string

const (
	TaskSymbolSummary        Task = "symbol_summary"
	TaskModuleSummary        Task = "module_summary"
	TaskExecutionExplanation Task = "execution_explanation"
	TaskGapAnalysis          Task = "documentation_gap_analysis"
)

type ModelDescriptor struct {
	ID               string `json:"id"`
	Architecture     string `json:"architecture,omitempty"`
	ParameterCount   int64  `json:"parameter_count,omitempty"`
	QuantizationBits int    `json:"quantization_bits,omitempty"`
	WeightBytes      int64  `json:"weight_bytes"`
	ContextLimit     int    `json:"context_limit"`
	Tokenizer        string `json:"tokenizer,omitempty"`
}

type InferenceOptions struct {
	ContextTokens        int   `json:"context_tokens"`
	MaxOutputTokens      int   `json:"max_output_tokens"`
	Threads              int   `json:"threads,omitempty"`
	BatchSize            int   `json:"batch_size,omitempty"`
	Parallel             int   `json:"parallel,omitempty"`
	KVBytesPerToken      int64 `json:"kv_bytes_per_token,omitempty"`
	RuntimeOverheadBytes int64 `json:"runtime_overhead_bytes,omitempty"`
}

type Budget struct {
	MaximumRSSBytes   int64 `json:"maximum_rss_bytes"`
	SafetyMarginBytes int64 `json:"safety_margin_bytes,omitempty"`
}

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
	estimate := Estimate{
		WeightsBytes: model.WeightBytes,
		KVCacheBytes: int64(options.ContextTokens) * options.KVBytesPerToken * int64(options.Parallel),
		RuntimeBytes: options.RuntimeOverheadBytes,
		PromptBytes:  promptBytes,
		OutputBytes:  int64(options.MaxOutputTokens) * 8,
	}
	estimate.EstimatedPeakBytes = estimate.WeightsBytes + estimate.KVCacheBytes + estimate.RuntimeBytes + estimate.PromptBytes + estimate.OutputBytes + budget.SafetyMarginBytes
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

type SourceExcerpt struct {
	EvidenceID string               `json:"evidence_id"`
	Source     rkcmodel.SourceRange `json:"source"`
	Text       string               `json:"text"`
	Truncated  bool                 `json:"truncated,omitempty"`
}

type PacketPolicy struct {
	RequireCitations         bool `json:"require_citations"`
	AllowInference           bool `json:"allow_inference"`
	MaximumClaims            int  `json:"maximum_claims"`
	MaximumSummaryCharacters int  `json:"maximum_summary_characters"`
}

type Request struct {
	RequestID string           `json:"request_id"`
	Task      Task             `json:"task"`
	Packet    EvidencePacket   `json:"packet"`
	Options   InferenceOptions `json:"options"`
	Deadline  *time.Time       `json:"deadline,omitempty"`
}

type ClaimDraft struct {
	Text        string   `json:"text"`
	Category    string   `json:"category,omitempty"`
	Certainty   string   `json:"certainty"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type Response struct {
	RequestID           string       `json:"request_id"`
	Summary             string       `json:"summary,omitempty"`
	Claims              []ClaimDraft `json:"claims,omitempty"`
	UnresolvedQuestions []string     `json:"unresolved_questions,omitempty"`
	ModelID             string       `json:"model_id"`
	Usage               Usage        `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens   int   `json:"prompt_tokens,omitempty"`
	OutputTokens   int   `json:"output_tokens,omitempty"`
	WallTimeMillis int64 `json:"wall_time_millis,omitempty"`
	PeakRSSBytes   int64 `json:"peak_rss_bytes,omitempty"`
}

type Provider interface {
	Descriptor() ModelDescriptor
	Supports(Task) bool
	Generate(context.Context, Request) (Response, error)
	Close() error
}

type ClaimValidation struct {
	Accepted               []rkcmodel.Claim      `json:"accepted,omitempty"`
	Rejected               []RejectedClaim       `json:"rejected,omitempty"`
	AcceptedSummary        string                `json:"accepted_summary,omitempty"`
	SummaryRejectedReasons []string              `json:"summary_rejected_reasons,omitempty"`
	Diagnostics            []rkcmodel.Diagnostic `json:"diagnostics,omitempty"`
}

type RejectedClaim struct {
	Claim   ClaimDraft `json:"claim"`
	Reasons []string   `json:"reasons"`
}

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
	for index, draft := range response.Claims {
		var reasons []string
		if index >= maxClaims {
			reasons = append(reasons, "claim count exceeds packet policy")
		}
		if strings.TrimSpace(draft.Text) == "" {
			reasons = append(reasons, "empty claim")
		}
		if packet.Policy.RequireCitations && len(draft.EvidenceIDs) == 0 {
			reasons = append(reasons, "claim is uncited")
		}
		for _, evidenceID := range draft.EvidenceIDs {
			if _, ok := allowedEvidence[evidenceID]; !ok {
				reasons = append(reasons, "evidence outside packet: "+evidenceID)
			}
		}
		if draft.Certainty == "inferred" && !packet.Policy.AllowInference {
			reasons = append(reasons, "inference is disabled")
		}
		if draft.Certainty == "" {
			reasons = append(reasons, "certainty is required")
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
		if packet.Policy.MaximumSummaryCharacters > 0 && len([]rune(summary)) > packet.Policy.MaximumSummaryCharacters {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary exceeds packet character limit")
		}
		if mentionsImpossibleIdentifier(summary, knownTerms) {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary appears to mention an unknown code identifier")
		}
		if packet.Policy.RequireCitations && len(validation.Accepted) == 0 {
			validation.SummaryRejectedReasons = append(validation.SummaryRejectedReasons, "summary has no accepted cited claims")
		}
		if len(validation.SummaryRejectedReasons) == 0 {
			validation.AcceptedSummary = summary
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
