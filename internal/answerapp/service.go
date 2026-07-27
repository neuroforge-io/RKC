// Package answerapp wires bounded repository retrieval to evidence-grounded
// answer generation. It deliberately owns no persistence: callers receive one
// result and decide how to render it, while the canonical RKC bundle remains
// the only source of repository facts.
package answerapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/groundedanswer"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	maximumResultLimit    = 1_000
	maximumGraphHops      = 4
	maximumGraphNodeLimit = 5_000
	maximumRepairQueries  = 3
	maximumRepairBytes    = 512
)

var ErrInvalidRequest = errors.New("invalid answer request")

// Request contains only caller-controlled retrieval filters and bounded model
// options. Answer always uses Question as the retrieval query so model context,
// provenance, and the displayed question cannot describe different requests.
type Request struct {
	Question       string
	RetrievalMode  retrieval.Mode
	Kinds          map[string]struct{}
	Languages      map[string]struct{}
	ObjectTypes    map[string]struct{}
	PathPrefix     string
	Limit          int
	GraphHops      int
	GraphNodeLimit int
	Task           modelruntime.Task
	Inference      modelruntime.InferenceOptions
	Deadline       *time.Time
}

// Service joins one immutable canonical bundle, its derived retrieval engine,
// and a grounded-answer service. Provider lifecycle remains with the caller.
type Service struct {
	bundle    rkcmodel.Bundle
	retrieval *retrieval.Engine
	grounder  *groundedanswer.Service
}

// New constructs an answer service without taking ownership of provider.
func New(
	bundle rkcmodel.Bundle,
	engine *retrieval.Engine,
	provider modelruntime.Provider,
	options groundedanswer.Options,
) (*Service, error) {
	if engine == nil || engine.Lexical == nil {
		return nil, errors.New("answer retrieval requires a lexical index")
	}
	grounder, err := groundedanswer.New(provider, options)
	if err != nil {
		return nil, fmt.Errorf("configure grounded answers: %w", err)
	}
	return &Service{bundle: bundle, retrieval: engine, grounder: grounder}, nil
}

// Answer performs the requested retrieval with optional bounded graph
// expansion and then re-resolves every hit from the canonical bundle before
// generation. An omitted mode deliberately defaults to lexical retrieval;
// semantic and hybrid modes fail closed unless the service was configured with
// both a vector index and an embedding provider.
func (service *Service) Answer(ctx context.Context, request Request) (groundedanswer.Result, error) {
	if service == nil || service.retrieval == nil || service.grounder == nil {
		return groundedanswer.Result{}, errors.New("answer service is not configured")
	}
	if ctx == nil {
		return groundedanswer.Result{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return groundedanswer.Result{}, err
	}
	question := strings.TrimSpace(request.Question)
	if question == "" {
		return groundedanswer.Result{}, fmt.Errorf("%w: question is required", ErrInvalidRequest)
	}
	if request.Question != question {
		return groundedanswer.Result{}, fmt.Errorf("%w: question must not have surrounding whitespace", ErrInvalidRequest)
	}
	if request.Limit < 1 || request.Limit > maximumResultLimit {
		return groundedanswer.Result{}, fmt.Errorf("%w: result limit must be between 1 and %d", ErrInvalidRequest, maximumResultLimit)
	}
	if request.GraphHops < 0 || request.GraphHops > maximumGraphHops {
		return groundedanswer.Result{}, fmt.Errorf("%w: graph hops must be between 0 and %d", ErrInvalidRequest, maximumGraphHops)
	}
	if request.GraphNodeLimit < 1 || request.GraphNodeLimit > maximumGraphNodeLimit {
		return groundedanswer.Result{}, fmt.Errorf("%w: graph node limit must be between 1 and %d", ErrInvalidRequest, maximumGraphNodeLimit)
	}
	mode := request.RetrievalMode
	if mode == "" {
		mode = retrieval.ModeLexical
	}
	switch mode {
	case retrieval.ModeLexical, retrieval.ModeSemantic, retrieval.ModeHybrid:
	default:
		return groundedanswer.Result{}, fmt.Errorf("%w: unsupported retrieval mode %q", ErrInvalidRequest, mode)
	}

	retrieved, err := service.retrieval.Search(ctx, search.Query{
		Text:        question,
		Kinds:       copySet(request.Kinds),
		Languages:   copySet(request.Languages),
		ObjectTypes: copySet(request.ObjectTypes),
		PathPrefix:  request.PathPrefix,
		Limit:       request.Limit,
	}, retrieval.Options{
		Mode:           mode,
		GraphHops:      request.GraphHops,
		GraphNodeLimit: request.GraphNodeLimit,
	})
	if err != nil {
		return groundedanswer.Result{}, fmt.Errorf("retrieve answer evidence: %w", err)
	}
	return service.verifyAndCompile(ctx, request, question, mode, retrieved)
}

func (service *Service) verifyAndCompile(
	ctx context.Context,
	request Request,
	question string,
	mode retrieval.Mode,
	retrieved search.Response,
) (groundedanswer.Result, error) {
	maximumPasses := service.grounder.MaximumRepairPasses()
	maximumHits := service.grounder.MaximumRetrievalHits()
	current := retrieved
	var attempts []groundedanswer.Result
	var audit []groundedanswer.VerificationPass
	var totalUsage modelruntime.Usage
	var incomingQueries []string
	verificationState := "repair_exhausted"

	for pass := 0; pass <= maximumPasses; pass++ {
		result, err := service.grounder.Answer(ctx, groundedanswer.Request{
			Question:         question,
			Retrieval:        current,
			Bundle:           service.bundle,
			Task:             request.Task,
			Inference:        request.Inference,
			VerificationPass: pass,
			Deadline:         copyDeadline(request.Deadline),
		})
		if err != nil {
			return groundedanswer.Result{}, fmt.Errorf("ground answer pass %d: %w", pass, err)
		}
		attempts = append(attempts, result)
		totalUsage = addUsage(totalUsage, result.Provenance.Usage)
		kind := "initial"
		if pass > 0 {
			kind = "repair"
		}
		audit = append(audit, groundedanswer.VerificationPass{
			Pass: pass, Kind: kind, RepairQueries: append([]string(nil), incomingQueries...),
			RequestID: result.RequestID, Status: result.Status,
			AcceptedClaims: len(result.Claims), RejectedClaims: len(result.Audit.RejectedClaims),
			UnresolvedQuestions: len(result.Audit.UntrustedUnresolvedQuestions),
			SelectedEvidence:    len(result.Provenance.SelectedEvidence),
			PromptBytes:         result.Provenance.PromptBytes, Usage: result.Provenance.Usage,
		})

		if !needsRepair(result) {
			verificationState = "verified"
			break
		}
		queries := repairQueries(result)
		if pass == maximumPasses {
			break
		}
		if len(queries) == 0 {
			break
		}
		additions := make([]search.Response, 0, len(queries))
		for _, queryText := range queries {
			if err := ctx.Err(); err != nil {
				return groundedanswer.Result{}, err
			}
			response, err := service.retrieval.Search(ctx, search.Query{
				Text:        queryText,
				Kinds:       copySet(request.Kinds),
				Languages:   copySet(request.Languages),
				ObjectTypes: copySet(request.ObjectTypes),
				PathPrefix:  request.PathPrefix,
				Limit:       request.Limit,
			}, retrieval.Options{
				Mode: mode, GraphHops: request.GraphHops, GraphNodeLimit: request.GraphNodeLimit,
			})
			if err != nil {
				return groundedanswer.Result{}, fmt.Errorf("retrieve repair evidence pass %d: %w", pass+1, err)
			}
			additions = append(additions, response)
		}
		hitLimit := request.Limit * (pass + 2)
		if hitLimit > maximumHits {
			hitLimit = maximumHits
		}
		current = mergeRetrieval(question, current, additions, hitLimit)
		incomingQueries = queries
	}

	best := selectBestAttempt(attempts)
	if verificationState == "repair_exhausted" {
		if best.Status == groundedanswer.StatusAnswered {
			verificationState = "grounded_with_unresolved_gaps"
		} else {
			verificationState = "abstained_after_repairs"
		}
	}
	best.Verification = groundedanswer.Verification{
		State: verificationState, Passes: audit, TotalUsage: totalUsage, RepairLimit: maximumPasses,
	}
	return best, nil
}

func needsRepair(result groundedanswer.Result) bool {
	if len(result.Audit.RejectedClaims) > 0 || len(result.Audit.UntrustedUnresolvedQuestions) > 0 {
		return true
	}
	return result.Abstention != nil && result.Abstention.Code == groundedanswer.AbstentionNoValidClaims
}

func repairQueries(result groundedanswer.Result) []string {
	if result.Abstention != nil && result.Abstention.Code == groundedanswer.AbstentionInsufficientEvidence {
		return nil
	}
	candidates := make([]string, 0, len(result.Audit.RejectedClaims)+len(result.Audit.UntrustedUnresolvedQuestions))
	for _, rejected := range result.Audit.RejectedClaims {
		candidates = append(candidates, rejected.Claim.Text)
	}
	candidates = append(candidates, result.Audit.UntrustedUnresolvedQuestions...)
	seen := map[string]struct{}{}
	queries := make([]string, 0, maximumRepairQueries)
	for _, candidate := range candidates {
		query := sanitizeRepairQuery(candidate)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		queries = append(queries, query)
		if len(queries) == maximumRepairQueries {
			break
		}
	}
	return queries
}

func sanitizeRepairQuery(value string) string {
	value = strings.ToValidUTF8(value, " ")
	value = strings.Map(func(character rune) rune {
		switch {
		case character == ':' || character == '\\':
			return ' '
		case unicode.IsControl(character):
			return ' '
		default:
			return character
		}
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maximumRepairBytes {
		value = value[:maximumRepairBytes]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		value = strings.TrimSpace(value)
	}
	return value
}

func mergeRetrieval(question string, base search.Response, additions []search.Response, limit int) search.Response {
	if limit < 1 {
		limit = 1
	}
	type ranked struct {
		hit     search.Hit
		score   float64
		reasons map[string]struct{}
	}
	accumulated := map[string]*ranked{}
	responses := append([]search.Response{base}, additions...)
	truncated := false
	for responseIndex, response := range responses {
		truncated = truncated || response.Truncated
		for rank, hit := range response.Hits {
			id := hit.Document.ID
			if id == "" {
				continue
			}
			current := accumulated[id]
			if current == nil {
				current = &ranked{hit: hit, reasons: map[string]struct{}{}}
				accumulated[id] = current
			}
			current.score += 1 / float64(60+rank+1)
			for _, reason := range hit.Reasons {
				current.reasons[reason] = struct{}{}
			}
			current.reasons[fmt.Sprintf("verification_query:%d", responseIndex)] = struct{}{}
		}
	}
	hits := make([]search.Hit, 0, len(accumulated))
	for _, current := range accumulated {
		current.hit.Score = current.score
		current.hit.Reasons = current.hit.Reasons[:0]
		for reason := range current.reasons {
			current.hit.Reasons = append(current.hit.Reasons, reason)
		}
		sort.Strings(current.hit.Reasons)
		hits = append(hits, current.hit)
	}
	sort.Slice(hits, func(left, right int) bool {
		if hits[left].Score == hits[right].Score {
			return hits[left].Document.ID < hits[right].Document.ID
		}
		return hits[left].Score > hits[right].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
		truncated = true
	}
	return search.Response{
		Query: question, Hits: hits, Truncated: truncated,
		Mode: base.Mode + "+verification", IndexVersion: base.IndexVersion,
	}
}

func selectBestAttempt(attempts []groundedanswer.Result) groundedanswer.Result {
	best := attempts[0]
	for _, candidate := range attempts[1:] {
		if betterAttempt(candidate, best) {
			best = candidate
		}
	}
	return best
}

func betterAttempt(candidate, current groundedanswer.Result) bool {
	if len(candidate.Claims) != len(current.Claims) {
		return len(candidate.Claims) > len(current.Claims)
	}
	candidateGaps := len(candidate.Audit.RejectedClaims) + len(candidate.Audit.UntrustedUnresolvedQuestions)
	currentGaps := len(current.Audit.RejectedClaims) + len(current.Audit.UntrustedUnresolvedQuestions)
	if candidateGaps != currentGaps {
		return candidateGaps < currentGaps
	}
	return candidate.Status == groundedanswer.StatusAnswered && current.Status != groundedanswer.StatusAnswered
}

func addUsage(total, next modelruntime.Usage) modelruntime.Usage {
	total.PromptTokens = boundedAddInt(total.PromptTokens, next.PromptTokens)
	total.OutputTokens = boundedAddInt(total.OutputTokens, next.OutputTokens)
	total.WallTimeMillis = boundedAddInt64(total.WallTimeMillis, next.WallTimeMillis)
	if next.PeakRSSBytes > total.PeakRSSBytes {
		total.PeakRSSBytes = next.PeakRSSBytes
	}
	return total
}

func boundedAddInt(left, right int) int {
	if right > 0 && left > int(^uint(0)>>1)-right {
		return int(^uint(0) >> 1)
	}
	return left + right
}

func boundedAddInt64(left, right int64) int64 {
	const maximum = int64(^uint64(0) >> 1)
	if right > 0 && left > maximum-right {
		return maximum
	}
	return left + right
}

func copySet(values map[string]struct{}) map[string]struct{} {
	if values == nil {
		return nil
	}
	copy := make(map[string]struct{}, len(values))
	for value := range values {
		copy[value] = struct{}{}
	}
	return copy
}

func copyDeadline(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
