// Package answerapp wires bounded repository retrieval to evidence-grounded
// answer generation. It deliberately owns no persistence: callers receive one
// result and decide how to render it, while the canonical RKC bundle remains
// the only source of repository facts.
package answerapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	result, err := service.grounder.Answer(ctx, groundedanswer.Request{
		Question:  question,
		Retrieval: retrieved,
		Bundle:    service.bundle,
		Task:      request.Task,
		Inference: request.Inference,
		Deadline:  copyDeadline(request.Deadline),
	})
	if err != nil {
		return groundedanswer.Result{}, fmt.Errorf("ground answer: %w", err)
	}
	return result, nil
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
