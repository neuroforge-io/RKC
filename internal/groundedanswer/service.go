// Package groundedanswer turns ranked repository retrieval into bounded,
// citation-bearing model answers. Retrieval chooses candidates only: every fact
// given to the model is re-resolved from the canonical RKC bundle, and every
// published claim must cite packet evidence that maps back to canonical nodes.
package groundedanswer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const ProtocolVersion = "rkc-grounded-answer/v1"

const (
	defaultMaximumRetrievalHits    = 32
	defaultMaximumNodes            = 24
	defaultMaximumEdges            = 64
	defaultMaximumEvidence         = 64
	defaultMinimumEvidence         = 1
	defaultMaximumQuestionBytes    = 4 * 1024
	defaultMaximumContextTextBytes = 64 * 1024
	defaultMaximumFieldBytes       = 8 * 1024
	defaultMaximumPromptBytes      = 256 * 1024
	defaultMaximumClaims           = 12
	defaultMaximumUnresolved       = 16
	hardMaximumRetrievalHits       = 1_000
	hardMaximumNodes               = 256
	hardMaximumEdges               = 1_024
	hardMaximumEvidence            = 1_024
	hardMaximumQuestionBytes       = 64 * 1024
	hardMaximumContextTextBytes    = 4 * 1024 * 1024
	hardMaximumFieldBytes          = 256 * 1024
	hardMaximumPromptBytes         = 8 * 1024 * 1024
	hardMaximumClaims              = 128
	hardMaximumUnresolved          = 128
	maximumStableIdentifierBytes   = 512
)

var (
	ErrInvalidRequest       = errors.New("invalid grounded-answer request")
	ErrInvalidBundle        = errors.New("invalid canonical bundle")
	ErrInvalidProvider      = errors.New("invalid model provider")
	ErrModelProtocol        = errors.New("grounded-answer model protocol violation")
	ErrUnsupportedModelTask = errors.New("grounded-answer model task is unsupported")
)

type Status string

const (
	StatusAnswered  Status = "answered"
	StatusAbstained Status = "abstained"
)

const (
	AbstentionInsufficientEvidence = "insufficient_evidence"
	AbstentionContextBudget        = "context_budget_exceeded"
	AbstentionNoValidClaims        = "no_valid_grounded_claims"
)

// Options are hard service-side bounds. Provider token, memory, and process
// limits remain independently enforced by modelruntime.Provider.
type Options struct {
	MaximumRetrievalHits    int
	MaximumNodes            int
	MaximumEdges            int
	MaximumEvidence         int
	MinimumEvidence         int
	MaximumQuestionBytes    int
	MaximumContextTextBytes int
	MaximumFieldBytes       int
	MaximumPromptBytes      int
	MaximumClaims           int
	MaximumUnresolved       int
	AllowInference          bool
}

type Request struct {
	Question  string
	Retrieval search.Response
	Bundle    rkcmodel.Bundle
	Task      modelruntime.Task
	Inference modelruntime.InferenceOptions
	Deadline  *time.Time
}

// Citation binds one allowed evidence record to every canonical node that used
// that evidence in the selected graph context.
type Citation struct {
	ID         string                `json:"id"`
	EvidenceID string                `json:"evidence_id"`
	NodeIDs    []string              `json:"node_ids"`
	Source     *rkcmodel.SourceRange `json:"source,omitempty"`
}

type Claim struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Category    string   `json:"category,omitempty"`
	Certainty   string   `json:"certainty"`
	EvidenceIDs []string `json:"evidence_ids"`
	NodeIDs     []string `json:"node_ids"`
	CitationIDs []string `json:"citation_ids"`
}

type Abstention struct {
	Code              string `json:"code"`
	Reason            string `json:"reason"`
	AvailableEvidence int    `json:"available_evidence"`
	RequiredEvidence  int    `json:"required_evidence"`
}

type RetrievalProvenance struct {
	Query        string   `json:"query,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	IndexVersion string   `json:"index_version,omitempty"`
	HitIDs       []string `json:"hit_ids,omitempty"`
}

type Provenance struct {
	SnapshotID       string                       `json:"snapshot_id"`
	BundleDigest     string                       `json:"bundle_digest"`
	QuestionNodeID   string                       `json:"question_node_id"`
	PacketID         string                       `json:"packet_id"`
	PacketDigest     string                       `json:"packet_digest"`
	PromptDigest     string                       `json:"prompt_digest"`
	PromptBytes      int                          `json:"prompt_bytes"`
	Retrieval        RetrievalProvenance          `json:"retrieval"`
	SelectedNodeIDs  []string                     `json:"selected_node_ids"`
	SelectedEvidence []string                     `json:"selected_evidence_ids"`
	Provider         modelruntime.ModelDescriptor `json:"provider"`
	ModelRequestID   string                       `json:"model_request_id,omitempty"`
	ModelResponseID  string                       `json:"model_response_id,omitempty"`
	ModelID          string                       `json:"model_id,omitempty"`
	Usage            modelruntime.Usage           `json:"usage,omitempty"`
}

// Truncation accounts for every bounded input collection and text budget. A
// true flag never silently changes the answer contract; it remains visible to
// callers deciding whether to widen limits and retry.
type Truncation struct {
	Retrieval            bool `json:"retrieval"`
	CandidateHits        int  `json:"candidate_hits"`
	ConsideredHits       int  `json:"considered_hits"`
	UnknownHits          int  `json:"unknown_hits"`
	Nodes                bool `json:"nodes"`
	CandidateNodes       int  `json:"candidate_nodes"`
	IncludedNodes        int  `json:"included_nodes"`
	Edges                bool `json:"edges"`
	CandidateEdges       int  `json:"candidate_edges"`
	IncludedEdges        int  `json:"included_edges"`
	Evidence             bool `json:"evidence"`
	CandidateEvidence    int  `json:"candidate_evidence"`
	IncludedEvidence     int  `json:"included_evidence"`
	MissingEvidence      int  `json:"missing_evidence_references"`
	ContextText          bool `json:"context_text"`
	IncludedContextBytes int  `json:"included_context_bytes"`
	Prompt               bool `json:"prompt"`
	ModelUnresolved      bool `json:"model_unresolved_questions"`
}

type Audit struct {
	RejectedClaims               []modelruntime.RejectedClaim `json:"rejected_claims,omitempty"`
	SummaryRejectedReasons       []string                     `json:"summary_rejected_reasons,omitempty"`
	UntrustedUnresolvedQuestions []string                     `json:"untrusted_unresolved_questions,omitempty"`
}

type Result struct {
	ProtocolVersion string      `json:"protocol_version"`
	RequestID       string      `json:"request_id"`
	Status          Status      `json:"status"`
	Question        string      `json:"question"`
	Claims          []Claim     `json:"claims,omitempty"`
	Citations       []Citation  `json:"citations,omitempty"`
	Abstention      *Abstention `json:"abstention,omitempty"`
	Provenance      Provenance  `json:"provenance"`
	Truncation      Truncation  `json:"truncation"`
	Audit           Audit       `json:"audit"`
}

type Service struct {
	provider   modelruntime.Provider
	descriptor modelruntime.ModelDescriptor
	options    Options
}

func New(provider modelruntime.Provider, options Options) (*Service, error) {
	if nilInterface(provider) {
		return nil, fmt.Errorf("%w: provider is required", ErrInvalidProvider)
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	descriptor := provider.Descriptor()
	if err := validateIdentifier(descriptor.ID, "provider model ID"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProvider, err)
	}
	return &Service{provider: provider, descriptor: descriptor, options: normalized}, nil
}

func normalizeOptions(options Options) (Options, error) {
	values := []struct {
		name     string
		value    *int
		fallback int
		maximum  int
	}{
		{"maximum retrieval hits", &options.MaximumRetrievalHits, defaultMaximumRetrievalHits, hardMaximumRetrievalHits},
		{"maximum nodes", &options.MaximumNodes, defaultMaximumNodes, hardMaximumNodes},
		{"maximum edges", &options.MaximumEdges, defaultMaximumEdges, hardMaximumEdges},
		{"maximum evidence", &options.MaximumEvidence, defaultMaximumEvidence, hardMaximumEvidence},
		{"minimum evidence", &options.MinimumEvidence, defaultMinimumEvidence, hardMaximumEvidence},
		{"maximum question bytes", &options.MaximumQuestionBytes, defaultMaximumQuestionBytes, hardMaximumQuestionBytes},
		{"maximum context text bytes", &options.MaximumContextTextBytes, defaultMaximumContextTextBytes, hardMaximumContextTextBytes},
		{"maximum field bytes", &options.MaximumFieldBytes, defaultMaximumFieldBytes, hardMaximumFieldBytes},
		{"maximum prompt bytes", &options.MaximumPromptBytes, defaultMaximumPromptBytes, hardMaximumPromptBytes},
		{"maximum claims", &options.MaximumClaims, defaultMaximumClaims, hardMaximumClaims},
		{"maximum unresolved questions", &options.MaximumUnresolved, defaultMaximumUnresolved, hardMaximumUnresolved},
	}
	for _, item := range values {
		if *item.value == 0 {
			*item.value = item.fallback
		}
		if *item.value < 1 || *item.value > item.maximum {
			return Options{}, fmt.Errorf("%w: %s must be between 1 and %d", ErrInvalidRequest, item.name, item.maximum)
		}
	}
	if options.MinimumEvidence > options.MaximumEvidence {
		return Options{}, fmt.Errorf("%w: minimum evidence exceeds maximum evidence", ErrInvalidRequest)
	}
	return options, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Answer builds a bounded evidence packet, invokes the injected provider once,
// and publishes only structurally grounded claims. Provider errors and protocol
// violations are errors; lack of evidence or valid claims is an auditable
// abstention.
func (service *Service) Answer(ctx context.Context, request Request) (Result, error) {
	if service == nil || nilInterface(service.provider) {
		return Result{}, fmt.Errorf("%w: service is not configured", ErrInvalidProvider)
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if current := service.provider.Descriptor(); !reflect.DeepEqual(current, service.descriptor) {
		return Result{}, fmt.Errorf("%w: provider descriptor changed after service construction", ErrInvalidProvider)
	}
	question := strings.TrimSpace(request.Question)
	if question == "" {
		return Result{}, fmt.Errorf("%w: question is required", ErrInvalidRequest)
	}
	if question != request.Question {
		return Result{}, fmt.Errorf("%w: question must not have surrounding whitespace", ErrInvalidRequest)
	}
	if !utf8.ValidString(question) || strings.IndexByte(question, 0) >= 0 {
		return Result{}, fmt.Errorf("%w: question must be valid NUL-free UTF-8", ErrInvalidRequest)
	}
	if len(question) > service.options.MaximumQuestionBytes {
		return Result{}, fmt.Errorf("%w: question exceeds %d bytes", ErrInvalidRequest, service.options.MaximumQuestionBytes)
	}
	task := request.Task
	if task == "" {
		task = modelruntime.TaskModuleSummary
	}
	if !service.provider.Supports(task) {
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupportedModelTask, task)
	}

	prepared, err := prepareContext(question, task, request.Retrieval, request.Bundle, service.options)
	if err != nil {
		return Result{}, err
	}
	requestID := rkcmodel.StableID(
		"grounded_answer_request", prepared.bundleDigest, prepared.packet.PacketID,
		prepared.retrieval.Query, prepared.retrieval.Mode, prepared.retrieval.IndexVersion,
		strings.Join(prepared.retrieval.HitIDs, ","), service.descriptor.ID,
		service.descriptor.Digest, service.descriptor.RuntimeDigest,
	)
	result := Result{
		ProtocolVersion: ProtocolVersion,
		RequestID:       requestID,
		Question:        question,
		Status:          StatusAbstained,
		Provenance: Provenance{
			SnapshotID:       prepared.packet.SnapshotID,
			BundleDigest:     prepared.bundleDigest,
			QuestionNodeID:   prepared.packet.Subject.ID,
			PacketID:         prepared.packet.PacketID,
			PacketDigest:     prepared.packetDigest,
			PromptDigest:     prepared.promptDigest,
			PromptBytes:      prepared.promptBytes,
			Retrieval:        prepared.retrieval,
			SelectedNodeIDs:  append([]string(nil), prepared.nodeIDs...),
			SelectedEvidence: append([]string(nil), prepared.evidenceIDs...),
			Provider:         service.descriptor,
		},
		Truncation: prepared.truncation,
	}
	if len(prepared.evidenceIDs) < service.options.MinimumEvidence || len(prepared.nodeIDs) == 0 {
		result.Abstention = insufficientEvidence(len(prepared.evidenceIDs), service.options.MinimumEvidence)
		return result, nil
	}
	if prepared.promptBytes > service.options.MaximumPromptBytes {
		result.Truncation.Prompt = true
		result.Abstention = &Abstention{
			Code: AbstentionContextBudget,
			Reason: fmt.Sprintf(
				"bounded evidence prompt requires %d bytes; service limit is %d",
				prepared.promptBytes, service.options.MaximumPromptBytes,
			),
			AvailableEvidence: len(prepared.evidenceIDs),
			RequiredEvidence:  service.options.MinimumEvidence,
		}
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	providerPacket, err := clonePacket(prepared.packet)
	if err != nil {
		return Result{}, fmt.Errorf("clone grounded-answer packet: %w", err)
	}
	var deadline *time.Time
	if request.Deadline != nil {
		copy := *request.Deadline
		deadline = &copy
	}
	modelRequest := modelruntime.Request{
		RequestID: requestID,
		Task:      task,
		Packet:    providerPacket,
		Options:   request.Inference,
		Deadline:  deadline,
	}
	modelResponse, err := service.provider.Generate(ctx, modelRequest)
	if err != nil {
		return Result{}, fmt.Errorf("generate grounded answer: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	modelResponse, err = cloneResponse(modelResponse)
	if err != nil {
		return Result{}, fmt.Errorf("%w: copy response: %v", ErrModelProtocol, err)
	}
	if modelResponse.RequestID != requestID {
		return Result{}, fmt.Errorf("%w: response request ID %q does not match %q", ErrModelProtocol, modelResponse.RequestID, requestID)
	}
	if modelResponse.ModelID != service.descriptor.ID {
		return Result{}, fmt.Errorf("%w: response model ID %q does not match bound provider %q", ErrModelProtocol, modelResponse.ModelID, service.descriptor.ID)
	}
	if modelResponse.Usage.PromptTokens < 0 || modelResponse.Usage.OutputTokens < 0 || modelResponse.Usage.WallTimeMillis < 0 || modelResponse.Usage.PeakRSSBytes < 0 {
		return Result{}, fmt.Errorf("%w: model usage counters must be non-negative", ErrModelProtocol)
	}
	result.Provenance.ModelRequestID = requestID
	result.Provenance.ModelResponseID = modelResponse.RequestID
	result.Provenance.ModelID = modelResponse.ModelID
	result.Provenance.Usage = modelResponse.Usage

	generatorVersion := service.descriptor.Revision
	if generatorVersion == "" {
		generatorVersion = service.descriptor.RuntimeRevision
	}
	validation := modelruntime.ValidateResponse(prepared.packet, modelResponse, generatorVersion)
	claims, citations, additionallyRejected := convertClaims(
		requestID, prepared.packet.SnapshotID, validation.Accepted,
		prepared.ownership, prepared.evidenceByID,
	)
	validation.Rejected = append(validation.Rejected, additionallyRejected...)
	sortRejectedClaims(validation.Rejected)
	result.Claims = claims
	result.Citations = citations
	result.Audit.RejectedClaims = validation.Rejected
	result.Audit.SummaryRejectedReasons = append([]string(nil), validation.SummaryRejectedReasons...)
	result.Audit.UntrustedUnresolvedQuestions, result.Truncation.ModelUnresolved = boundedUnresolved(
		modelResponse.UnresolvedQuestions, service.options.MaximumUnresolved, service.options.MaximumFieldBytes,
	)
	if len(result.Claims) == 0 {
		result.Abstention = &Abstention{
			Code:              AbstentionNoValidClaims,
			Reason:            "the model returned no claims that satisfied the grounded citation contract",
			AvailableEvidence: len(prepared.evidenceIDs),
			RequiredEvidence:  service.options.MinimumEvidence,
		}
		return result, nil
	}
	result.Status = StatusAnswered
	return result, nil
}

func insufficientEvidence(available, required int) *Abstention {
	return &Abstention{
		Code: AbstentionInsufficientEvidence,
		Reason: fmt.Sprintf(
			"retrieval resolved to %d canonical evidence records; at least %d are required",
			available, required,
		),
		AvailableEvidence: available,
		RequiredEvidence:  required,
	}
}

type preparedContext struct {
	packet       modelruntime.EvidencePacket
	bundleDigest string
	packetDigest string
	promptDigest string
	promptBytes  int
	retrieval    RetrievalProvenance
	nodeIDs      []string
	evidenceIDs  []string
	ownership    map[string][]string
	evidenceByID map[string]rkcmodel.Evidence
	truncation   Truncation
}

type bundleCatalog struct {
	nodes         map[string]rkcmodel.Node
	evidence      map[string]rkcmodel.Evidence
	artifacts     map[string]rkcmodel.Artifact
	documents     map[string]rkcmodel.Document
	artifactNodes map[string][]string
}

func prepareContext(question string, task modelruntime.Task, retrieval search.Response, bundle rkcmodel.Bundle, options Options) (preparedContext, error) {
	if _, err := json.Marshal(bundle); err != nil {
		return preparedContext{}, fmt.Errorf("%w: encode bundle: %v", ErrInvalidBundle, err)
	}
	canonical := rkcmodel.CanonicalBundle(bundle)
	catalog, err := indexBundle(canonical)
	if err != nil {
		return preparedContext{}, err
	}
	truncation := Truncation{CandidateHits: len(retrieval.Hits)}
	considered := len(retrieval.Hits)
	if considered > options.MaximumRetrievalHits {
		considered = options.MaximumRetrievalHits
		truncation.Retrieval = true
	}
	if retrieval.Truncated {
		truncation.Retrieval = true
	}
	truncation.ConsideredHits = considered

	var selectedHitIDs []string
	var candidateNodeIDs []string
	seenNodes := map[string]struct{}{}
	for _, hit := range retrieval.Hits[:considered] {
		id := hit.Document.ID
		if err := validateIdentifier(id, "retrieval hit ID"); err != nil {
			truncation.UnknownHits++
			continue
		}
		resolved := resolveHitNodes(id, catalog)
		if len(resolved) == 0 {
			truncation.UnknownHits++
			continue
		}
		selectedHitIDs = append(selectedHitIDs, id)
		for _, nodeID := range resolved {
			if _, duplicate := seenNodes[nodeID]; duplicate {
				continue
			}
			seenNodes[nodeID] = struct{}{}
			candidateNodeIDs = append(candidateNodeIDs, nodeID)
		}
	}
	truncation.CandidateNodes = len(candidateNodeIDs)
	if len(candidateNodeIDs) > options.MaximumNodes {
		candidateNodeIDs = candidateNodeIDs[:options.MaximumNodes]
		truncation.Nodes = true
	}
	selectedNodes := map[string]struct{}{}
	for _, id := range candidateNodeIDs {
		selectedNodes[id] = struct{}{}
	}

	var candidateEdges []rkcmodel.Edge
	for _, edge := range canonical.Edges {
		_, from := selectedNodes[edge.From]
		_, to := selectedNodes[edge.To]
		if from && to {
			candidateEdges = append(candidateEdges, edge)
		}
	}
	truncation.CandidateEdges = len(candidateEdges)
	if len(candidateEdges) > options.MaximumEdges {
		candidateEdges = candidateEdges[:options.MaximumEdges]
		truncation.Edges = true
	}

	ownerSets := map[string]map[string]struct{}{}
	var candidateEvidenceIDs []string
	seenEvidence := map[string]struct{}{}
	addEvidence := func(evidenceID string, owners ...string) {
		if _, exists := catalog.evidence[evidenceID]; !exists {
			truncation.MissingEvidence++
			return
		}
		set := ownerSets[evidenceID]
		if set == nil {
			set = map[string]struct{}{}
			ownerSets[evidenceID] = set
		}
		for _, owner := range owners {
			if _, selected := selectedNodes[owner]; selected {
				set[owner] = struct{}{}
			}
		}
		if _, duplicate := seenEvidence[evidenceID]; !duplicate {
			seenEvidence[evidenceID] = struct{}{}
			candidateEvidenceIDs = append(candidateEvidenceIDs, evidenceID)
		}
	}
	for _, nodeID := range candidateNodeIDs {
		node := catalog.nodes[nodeID]
		for _, evidenceID := range sortedCopy(node.EvidenceIDs) {
			addEvidence(evidenceID, nodeID)
		}
	}
	for _, edge := range candidateEdges {
		for _, evidenceID := range sortedCopy(edge.EvidenceIDs) {
			addEvidence(evidenceID, edge.From, edge.To)
		}
	}
	truncation.CandidateEvidence = len(candidateEvidenceIDs)
	if len(candidateEvidenceIDs) > options.MaximumEvidence {
		candidateEvidenceIDs = candidateEvidenceIDs[:options.MaximumEvidence]
		truncation.Evidence = true
	}
	includedEvidence := make(map[string]struct{}, len(candidateEvidenceIDs))
	for _, id := range candidateEvidenceIDs {
		includedEvidence[id] = struct{}{}
	}

	budget := &textBudget{remaining: options.MaximumContextTextBytes, maximumField: options.MaximumFieldBytes}
	relatedNodes := make([]rkcmodel.Node, 0, len(candidateNodeIDs))
	for _, id := range candidateNodeIDs {
		relatedNodes = append(relatedNodes, sanitizeNode(catalog.nodes[id], includedEvidence, budget))
	}
	edges := make([]rkcmodel.Edge, 0, len(candidateEdges))
	for _, edge := range candidateEdges {
		edges = append(edges, sanitizeEdge(edge, includedEvidence, budget))
	}
	evidence := make([]rkcmodel.Evidence, 0, len(candidateEvidenceIDs))
	evidenceByID := make(map[string]rkcmodel.Evidence, len(candidateEvidenceIDs))
	ownership := make(map[string][]string, len(candidateEvidenceIDs))
	for _, id := range candidateEvidenceIDs {
		item := sanitizeEvidence(catalog.evidence[id], budget)
		evidence = append(evidence, item)
		evidenceByID[id] = item
		for owner := range ownerSets[id] {
			ownership[id] = append(ownership[id], owner)
		}
		sort.Strings(ownership[id])
	}
	truncation.ContextText = budget.truncated
	truncation.IncludedContextBytes = options.MaximumContextTextBytes - budget.remaining
	truncation.IncludedNodes = len(relatedNodes)
	truncation.IncludedEdges = len(edges)
	truncation.IncludedEvidence = len(evidence)

	questionNodeID := rkcmodel.StableID("grounded_question", canonical.Snapshot.ID, question)
	packet := modelruntime.EvidencePacket{
		SchemaVersion: rkcmodel.SchemaVersion,
		SnapshotID:    canonical.Snapshot.ID,
		Task:          task,
		Subject: rkcmodel.Node{
			ID: questionNodeID, Kind: "grounded_question", Name: "Repository question",
			Attributes: map[string]any{"question": question},
		},
		RelatedNodes:           relatedNodes,
		Edges:                  edges,
		Evidence:               evidence,
		AllowedClaimCategories: allowedAnswerCategories(),
		Policy: modelruntime.PacketPolicy{
			RequireCitations: true,
			AllowInference:   options.AllowInference,
			MaximumClaims:    options.MaximumClaims,
		},
	}
	packetWithoutID, err := json.Marshal(packet)
	if err != nil {
		return preparedContext{}, fmt.Errorf("encode grounded-answer packet identity: %w", err)
	}
	packet.PacketID = rkcmodel.StableID(
		"grounded_answer_packet", canonical.Snapshot.ID, string(task), contentDigest(packetWithoutID),
	)
	prompt, err := modelruntime.BuildPrompt(modelruntime.Request{Task: task, Packet: packet})
	if err != nil {
		return preparedContext{}, fmt.Errorf("build grounded-answer prompt: %w", err)
	}
	packetJSON, err := json.Marshal(packet)
	if err != nil {
		return preparedContext{}, fmt.Errorf("encode grounded-answer packet: %w", err)
	}
	return preparedContext{
		packet:       packet,
		bundleDigest: rkcmodel.CanonicalDigest(canonical),
		packetDigest: contentDigest(packetJSON),
		promptDigest: contentDigest([]byte(prompt)),
		promptBytes:  len(prompt),
		retrieval: RetrievalProvenance{
			Query:        boundedProvenanceString(retrieval.Query, options.MaximumQuestionBytes),
			Mode:         boundedProvenanceString(retrieval.Mode, options.MaximumFieldBytes),
			IndexVersion: boundedProvenanceString(retrieval.IndexVersion, options.MaximumFieldBytes),
			HitIDs:       selectedHitIDs,
		},
		nodeIDs:      append([]string(nil), candidateNodeIDs...),
		evidenceIDs:  append([]string(nil), candidateEvidenceIDs...),
		ownership:    ownership,
		evidenceByID: evidenceByID,
		truncation:   truncation,
	}, nil
}

func indexBundle(bundle rkcmodel.Bundle) (bundleCatalog, error) {
	if err := validateIdentifier(bundle.Snapshot.ID, "snapshot ID"); err != nil {
		return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	catalog := bundleCatalog{
		nodes: map[string]rkcmodel.Node{}, evidence: map[string]rkcmodel.Evidence{},
		artifacts: map[string]rkcmodel.Artifact{}, documents: map[string]rkcmodel.Document{},
		artifactNodes: map[string][]string{},
	}
	retrievableIDs := map[string]string{}
	registerRetrievable := func(id, kind string) error {
		if previous, duplicate := retrievableIDs[id]; duplicate {
			return fmt.Errorf("retrievable ID %q is ambiguous between %s and %s", id, previous, kind)
		}
		retrievableIDs[id] = kind
		return nil
	}
	for _, artifact := range bundle.Artifacts {
		if err := validateIdentifier(artifact.ID, "artifact ID"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, duplicate := catalog.artifacts[artifact.ID]; duplicate {
			return bundleCatalog{}, fmt.Errorf("%w: duplicate artifact ID %q", ErrInvalidBundle, artifact.ID)
		}
		if err := registerRetrievable(artifact.ID, "artifact"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		catalog.artifacts[artifact.ID] = artifact
	}
	for _, evidence := range bundle.Evidence {
		if err := validateIdentifier(evidence.ID, "evidence ID"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, duplicate := catalog.evidence[evidence.ID]; duplicate {
			return bundleCatalog{}, fmt.Errorf("%w: duplicate evidence ID %q", ErrInvalidBundle, evidence.ID)
		}
		catalog.evidence[evidence.ID] = evidence
	}
	for _, node := range bundle.Nodes {
		if err := validateIdentifier(node.ID, "node ID"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, duplicate := catalog.nodes[node.ID]; duplicate {
			return bundleCatalog{}, fmt.Errorf("%w: duplicate node ID %q", ErrInvalidBundle, node.ID)
		}
		if err := registerRetrievable(node.ID, "node"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if node.ArtifactID != "" {
			if _, exists := catalog.artifacts[node.ArtifactID]; !exists {
				return bundleCatalog{}, fmt.Errorf("%w: node %q references missing artifact %q", ErrInvalidBundle, node.ID, node.ArtifactID)
			}
			catalog.artifactNodes[node.ArtifactID] = append(catalog.artifactNodes[node.ArtifactID], node.ID)
		}
		for _, evidenceID := range node.EvidenceIDs {
			if _, exists := catalog.evidence[evidenceID]; !exists {
				return bundleCatalog{}, fmt.Errorf("%w: node %q references missing evidence %q", ErrInvalidBundle, node.ID, evidenceID)
			}
		}
		catalog.nodes[node.ID] = node
	}
	for id := range catalog.artifactNodes {
		sort.Strings(catalog.artifactNodes[id])
	}
	for _, document := range bundle.Documents {
		if err := validateIdentifier(document.ID, "document ID"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, duplicate := catalog.documents[document.ID]; duplicate {
			return bundleCatalog{}, fmt.Errorf("%w: duplicate document ID %q", ErrInvalidBundle, document.ID)
		}
		if err := registerRetrievable(document.ID, "document"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		for _, subjectID := range document.SubjectIDs {
			if _, exists := catalog.nodes[subjectID]; !exists {
				return bundleCatalog{}, fmt.Errorf("%w: document %q references missing subject %q", ErrInvalidBundle, document.ID, subjectID)
			}
		}
		catalog.documents[document.ID] = document
	}
	seenEdges := map[string]struct{}{}
	for _, edge := range bundle.Edges {
		if err := validateIdentifier(edge.ID, "edge ID"); err != nil {
			return bundleCatalog{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, duplicate := seenEdges[edge.ID]; duplicate {
			return bundleCatalog{}, fmt.Errorf("%w: duplicate edge ID %q", ErrInvalidBundle, edge.ID)
		}
		seenEdges[edge.ID] = struct{}{}
		if _, exists := catalog.nodes[edge.From]; !exists {
			return bundleCatalog{}, fmt.Errorf("%w: edge %q has missing from-node %q", ErrInvalidBundle, edge.ID, edge.From)
		}
		if _, exists := catalog.nodes[edge.To]; !exists {
			return bundleCatalog{}, fmt.Errorf("%w: edge %q has missing to-node %q", ErrInvalidBundle, edge.ID, edge.To)
		}
		for _, evidenceID := range edge.EvidenceIDs {
			if _, exists := catalog.evidence[evidenceID]; !exists {
				return bundleCatalog{}, fmt.Errorf("%w: edge %q references missing evidence %q", ErrInvalidBundle, edge.ID, evidenceID)
			}
		}
	}
	return catalog, nil
}

func resolveHitNodes(id string, catalog bundleCatalog) []string {
	if _, exists := catalog.nodes[id]; exists {
		return []string{id}
	}
	if nodes, exists := catalog.artifactNodes[id]; exists {
		return append([]string(nil), nodes...)
	}
	if document, exists := catalog.documents[id]; exists {
		result := sortedCopy(document.SubjectIDs)
		return uniqueStrings(result)
	}
	return nil
}

type textBudget struct {
	remaining    int
	maximumField int
	truncated    bool
}

func (budget *textBudget) take(value string) string {
	valid := strings.ToValidUTF8(value, "\uFFFD")
	if valid != value {
		budget.truncated = true
	}
	value = valid
	maximum := budget.maximumField
	if maximum > budget.remaining {
		maximum = budget.remaining
	}
	if len(value) > maximum {
		value = truncateUTF8(value, maximum)
		budget.truncated = true
	}
	budget.remaining -= len(value)
	return value
}

func sanitizeNode(node rkcmodel.Node, includedEvidence map[string]struct{}, budget *textBudget) rkcmodel.Node {
	copy := rkcmodel.Node{
		ID: node.ID, LogicalID: boundedIdentifier(node.LogicalID), Kind: budget.take(node.Kind),
		Name: budget.take(node.Name), QualifiedName: budget.take(node.QualifiedName),
		Signature: budget.take(node.Signature), Language: budget.take(node.Language),
		Visibility: budget.take(node.Visibility), Stability: budget.take(node.Stability),
		PublicSurface: node.PublicSurface, ArtifactID: boundedIdentifier(node.ArtifactID),
		SemanticHash: boundedIdentifier(node.SemanticHash),
	}
	if node.Source != nil {
		source := sanitizeSource(*node.Source, budget)
		copy.Source = &source
	}
	for _, id := range sortedCopy(node.EvidenceIDs) {
		if _, included := includedEvidence[id]; included {
			copy.EvidenceIDs = append(copy.EvidenceIDs, id)
		}
	}
	for _, key := range []string{"description", "docstring", "purpose", "summary"} {
		value, ok := node.Attributes[key].(string)
		if !ok || value == "" {
			continue
		}
		if copy.Attributes == nil {
			copy.Attributes = map[string]any{}
		}
		copy.Attributes[key] = budget.take(value)
	}
	return copy
}

func sanitizeEdge(edge rkcmodel.Edge, includedEvidence map[string]struct{}, budget *textBudget) rkcmodel.Edge {
	copy := rkcmodel.Edge{
		ID: edge.ID, Kind: budget.take(edge.Kind), From: edge.From, To: edge.To,
		Resolution: budget.take(edge.Resolution), Confidence: edge.Confidence,
		Producer: budget.take(edge.Producer), Lifecycle: budget.take(edge.Lifecycle),
	}
	for _, id := range sortedCopy(edge.EvidenceIDs) {
		if _, included := includedEvidence[id]; included {
			copy.EvidenceIDs = append(copy.EvidenceIDs, id)
		}
	}
	return copy
}

func sanitizeEvidence(evidence rkcmodel.Evidence, budget *textBudget) rkcmodel.Evidence {
	copy := rkcmodel.Evidence{
		ID: evidence.ID, Kind: budget.take(evidence.Kind), Method: budget.take(evidence.Method),
		Confidence: evidence.Confidence, Tool: budget.take(evidence.Tool),
		ToolVersion: budget.take(evidence.ToolVersion), InputDigest: boundedIdentifier(evidence.InputDigest),
		ObservedAt: evidence.ObservedAt, Detail: budget.take(evidence.Detail),
	}
	if evidence.Source != nil {
		source := sanitizeSource(*evidence.Source, budget)
		copy.Source = &source
	}
	return copy
}

func sanitizeSource(source rkcmodel.SourceRange, budget *textBudget) rkcmodel.SourceRange {
	return rkcmodel.SourceRange{
		ArtifactID: boundedIdentifier(source.ArtifactID), Path: budget.take(source.Path),
		StartByte: source.StartByte, EndByte: source.EndByte,
		StartLine: source.StartLine, StartColumn: source.StartColumn,
		EndLine: source.EndLine, EndColumn: source.EndColumn,
		Anchor: budget.take(source.Anchor),
	}
}

func allowedAnswerCategories() []string {
	return []string{
		"answer", "argument", "behavior", "branch", "conflict", "dependency", "entry", "error", "exit",
		"limitation", "missing_documentation", "missing_test", "public_surface", "purpose", "relationship",
		"responsibility", "return", "risk", "side_effect", "step", "unresolved_relationship",
	}
}

func convertClaims(requestID, snapshotID string, accepted []rkcmodel.Claim, ownership map[string][]string, evidenceByID map[string]rkcmodel.Evidence) ([]Claim, []Citation, []modelruntime.RejectedClaim) {
	citationByID := map[string]Citation{}
	seenClaims := map[string]struct{}{}
	var claims []Claim
	var rejected []modelruntime.RejectedClaim
	for _, acceptedClaim := range accepted {
		evidenceIDs := sortedCopy(acceptedClaim.EvidenceIDs)
		var nodeIDs []string
		var citationIDs []string
		var pendingCitations []Citation
		valid := true
		var reasons []string
		for _, evidenceID := range evidenceIDs {
			owners := sortedCopy(ownership[evidenceID])
			if len(owners) == 0 {
				valid = false
				reasons = append(reasons, "evidence citation has no selected canonical node: "+evidenceID)
				continue
			}
			item, exists := evidenceByID[evidenceID]
			if !exists {
				valid = false
				reasons = append(reasons, "evidence citation is absent from bounded context: "+evidenceID)
				continue
			}
			citationID := rkcmodel.StableID("answer_citation", snapshotID, evidenceID, strings.Join(owners, ","))
			citation := Citation{ID: citationID, EvidenceID: evidenceID, NodeIDs: owners}
			if item.Source != nil {
				source := *item.Source
				citation.Source = &source
			}
			pendingCitations = append(pendingCitations, citation)
			citationIDs = append(citationIDs, citationID)
			nodeIDs = append(nodeIDs, owners...)
		}
		nodeIDs = uniqueStrings(sortedCopy(nodeIDs))
		citationIDs = uniqueStrings(sortedCopy(citationIDs))
		claimKey := strings.Join([]string{acceptedClaim.Text, acceptedClaim.Category, acceptedClaim.Certainty, strings.Join(evidenceIDs, ",")}, "\x00")
		if _, duplicate := seenClaims[claimKey]; duplicate {
			valid = false
			reasons = append(reasons, "duplicate grounded claim")
		}
		if len(nodeIDs) == 0 {
			valid = false
			reasons = append(reasons, "claim has no canonical node citation")
		}
		if !valid {
			sort.Strings(reasons)
			rejected = append(rejected, modelruntime.RejectedClaim{
				Claim: modelruntime.ClaimDraft{
					Text: acceptedClaim.Text, Category: acceptedClaim.Category,
					Certainty: acceptedClaim.Certainty, EvidenceIDs: evidenceIDs,
				},
				Reasons: reasons,
			})
			continue
		}
		seenClaims[claimKey] = struct{}{}
		for _, citation := range pendingCitations {
			citationByID[citation.ID] = citation
		}
		claimID := rkcmodel.StableID("grounded_answer_claim", requestID, claimKey)
		claims = append(claims, Claim{
			ID: claimID, Text: acceptedClaim.Text, Category: acceptedClaim.Category,
			Certainty: acceptedClaim.Certainty, EvidenceIDs: evidenceIDs,
			NodeIDs: nodeIDs, CitationIDs: citationIDs,
		})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	citations := make([]Citation, 0, len(citationByID))
	for _, citation := range citationByID {
		citations = append(citations, citation)
	}
	sort.Slice(citations, func(i, j int) bool { return citations[i].ID < citations[j].ID })
	return claims, citations, rejected
}

func clonePacket(packet modelruntime.EvidencePacket) (modelruntime.EvidencePacket, error) {
	data, err := json.Marshal(packet)
	if err != nil {
		return modelruntime.EvidencePacket{}, err
	}
	var copy modelruntime.EvidencePacket
	if err := json.Unmarshal(data, &copy); err != nil {
		return modelruntime.EvidencePacket{}, err
	}
	return copy, nil
}

func cloneResponse(response modelruntime.Response) (modelruntime.Response, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return modelruntime.Response{}, err
	}
	var copy modelruntime.Response
	if err := json.Unmarshal(data, &copy); err != nil {
		return modelruntime.Response{}, err
	}
	return copy, nil
}

func sortRejectedClaims(rejected []modelruntime.RejectedClaim) {
	for index := range rejected {
		sort.Strings(rejected[index].Reasons)
	}
	sort.SliceStable(rejected, func(i, j int) bool {
		left := rejected[i].Claim.Text + "\x00" + strings.Join(rejected[i].Reasons, "\x00")
		right := rejected[j].Claim.Text + "\x00" + strings.Join(rejected[j].Reasons, "\x00")
		return left < right
	})
}

func boundedUnresolved(values []string, maximum, maximumBytes int) ([]string, bool) {
	truncated := len(values) > maximum
	if len(values) > maximum {
		values = values[:maximum]
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToValidUTF8(value, "\uFFFD"))
		if value == "" {
			continue
		}
		if len(value) > maximumBytes {
			value = truncateUTF8(value, maximumBytes)
			truncated = true
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, truncated
}

func validateIdentifier(value, label string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s is empty or has surrounding whitespace", label)
	}
	if len(value) > maximumStableIdentifierBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maximumStableIdentifierBytes)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must be valid NUL-free UTF-8", label)
	}
	return nil
}

func boundedIdentifier(value string) string {
	if len(value) <= maximumStableIdentifierBytes && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 {
		return value
	}
	return truncateUTF8(strings.ToValidUTF8(strings.ReplaceAll(value, "\x00", ""), "\uFFFD"), maximumStableIdentifierBytes)
}

func boundedProvenanceString(value string, maximum int) string {
	return truncateUTF8(strings.ToValidUTF8(value, "\uFFFD"), maximum)
}

func truncateUTF8(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func sortedCopy(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	return copy
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func contentDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}
