package runtime

import (
	"sort"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// RuntimeDiff separates static possibility, trace-scoped assertions, and
// producer-authenticated runtime observations. Statement coverage from the
// current producer is assertion-only and cannot demonstrate execution or
// dynamic call edges. CallObservationAvailable prevents a zero ratio from
// being misread as a complete call-event observation.
type RuntimeDiff struct {
	ProducerAuthenticatedTraceIDs []string             `json:"producer_authenticated_trace_ids,omitempty"`
	UnverifiedAssertionIDs        []string             `json:"unverified_assertion_ids,omitempty"`
	CaptureIntegrityAssertionIDs  []string             `json:"capture_integrity_assertion_ids,omitempty"`
	ProducerAuthenticatedTests    int                  `json:"producer_authenticated_tests"`
	ProducerAuthenticatedPassed   int                  `json:"producer_authenticated_passed_tests"`
	ProducerAuthenticatedFailed   int                  `json:"producer_authenticated_failed_tests"`
	ProducerAuthenticatedSkipped  int                  `json:"producer_authenticated_skipped_tests"`
	ProducerObservedFunctions     int                  `json:"producer_observed_functions"`
	AssertedFunctions             int                  `json:"asserted_functions"`
	FunctionsNotObserved          int                  `json:"functions_not_observed"`
	StaticCallEdges               int                  `json:"static_call_edges"`
	ResolvedCallEdges             int                  `json:"resolved_call_edges"`
	ProducerObservedCallEdges     int                  `json:"producer_observed_call_edges"`
	UndemonstratedCallEdges       int                  `json:"undemonstrated_call_edges"`
	CallObservationAvailable      bool                 `json:"call_observation_available"`
	CallObservationReason         string               `json:"call_observation_reason,omitempty"`
	ProducerCallObservationRatio  float64              `json:"producer_call_observation_ratio,omitempty"`
	ExecutionAssertedFunctions    []string             `json:"execution_asserted_functions,omitempty"`
	NotObservedFunctions          []string             `json:"not_observed_functions,omitempty"`
	UndemonstratedCalls           []UndemonstratedCall `json:"undemonstrated_calls,omitempty"`
}

// UndemonstratedCall is one statically possible call for which no admitted,
// producer-authenticated call event exists. It is not a non-execution claim.
type UndemonstratedCall struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Kind   string `json:"kind"`
}

// BuildDiff computes the bounded static-versus-dynamic diff from a bundle
// that has been imported with runtime evidence.
func BuildDiff(bundle rkcmodel.Bundle) RuntimeDiff {
	diff := RuntimeDiff{}
	nodeByID := map[string]rkcmodel.Node{}
	traceIDs := map[string]struct{}{}
	assertionIDs := map[string]struct{}{}
	captureIntegrityAssertionIDs := map[string]struct{}{}
	callEventTraceIDs := map[string]struct{}{}
	for _, node := range bundle.Nodes {
		nodeByID[node.ID] = node
		if node.Kind == "trace" {
			if traceID, ok := node.Attributes["trace_id"].(string); ok && traceID != "" {
				producerAuthenticated, _ := node.Attributes["producer_authenticated"].(bool)
				if producerAuthenticated {
					traceIDs[traceID] = struct{}{}
					if available, _ := node.Attributes["call_observation_available"].(bool); available {
						callEventTraceIDs[traceID] = struct{}{}
						diff.CallObservationAvailable = true
					}
				} else {
					assertionIDs[traceID] = struct{}{}
					if integrity, _ := node.Attributes["capture_integrity_authenticated"].(bool); integrity {
						captureIntegrityAssertionIDs[traceID] = struct{}{}
					}
				}
			}
			if reason, ok := node.Attributes["call_observation_reason"].(string); ok && reason != "" && diff.CallObservationReason == "" {
				diff.CallObservationReason = reason
			}
		}
	}
	for _, evidence := range bundle.Evidence {
		if evidence.Kind != "test_result" || evidence.Confidence != 1 || evidence.Attributes["producer_authenticated"] != true {
			continue
		}
		traceID, ok := evidence.Attributes["trace_id"].(string)
		if !ok || traceID == "" {
			continue
		}
		if _, authenticated := traceIDs[traceID]; !authenticated {
			continue
		}
		status, _ := evidence.Attributes["status"].(string)
		switch status {
		case "pass":
			diff.ProducerAuthenticatedTests++
			diff.ProducerAuthenticatedPassed++
		case "fail":
			diff.ProducerAuthenticatedTests++
			diff.ProducerAuthenticatedFailed++
		case "skip":
			diff.ProducerAuthenticatedTests++
			diff.ProducerAuthenticatedSkipped++
		}
	}
	for _, node := range bundle.Nodes {
		if !isFunctionLike(node.Kind) {
			continue
		}
		if executed, _ := node.Attributes["executed"].(bool); executed &&
			stringValuesIntersect(node.Attributes["executed_trace_ids"], traceIDs) {
			diff.ProducerObservedFunctions++
		}
		if stringValuesIntersect(node.Attributes["execution_asserted_trace_ids"], assertionIDs) {
			diff.AssertedFunctions++
			diff.ExecutionAssertedFunctions = append(diff.ExecutionAssertedFunctions, node.QualifiedName)
		}
		if stringValuesIntersect(node.Attributes["execution_not_observed_trace_ids"], assertionIDs) {
			diff.FunctionsNotObserved++
			diff.NotObservedFunctions = append(diff.NotObservedFunctions, node.QualifiedName)
		}
	}
	sort.Strings(diff.ExecutionAssertedFunctions)
	sort.Strings(diff.NotObservedFunctions)
	for _, edge := range bundle.Edges {
		if edge.Kind != "calls" {
			continue
		}
		diff.StaticCallEdges++
		if rkcmodel.NormalizeResolution(edge.Resolution) == rkcmodel.ResolutionUnresolved {
			continue
		}
		diff.ResolvedCallEdges++
		if value, ok := edge.Attributes["observed"].(bool); ok && value &&
			diff.CallObservationAvailable &&
			stringValuesIntersect(edge.Attributes["observed_trace_ids"], callEventTraceIDs) {
			diff.ProducerObservedCallEdges++
			continue
		}
		diff.UndemonstratedCallEdges++
		diff.UndemonstratedCalls = append(diff.UndemonstratedCalls, UndemonstratedCall{
			Caller: nodeByID[edge.From].QualifiedName,
			Callee: nodeByID[edge.To].QualifiedName,
			Kind:   nodeByID[edge.To].Kind,
		})
	}
	sort.Slice(diff.UndemonstratedCalls, func(i, j int) bool {
		if diff.UndemonstratedCalls[i].Caller != diff.UndemonstratedCalls[j].Caller {
			return diff.UndemonstratedCalls[i].Caller < diff.UndemonstratedCalls[j].Caller
		}
		return diff.UndemonstratedCalls[i].Callee < diff.UndemonstratedCalls[j].Callee
	})
	if diff.CallObservationAvailable && diff.ResolvedCallEdges > 0 {
		diff.ProducerCallObservationRatio = float64(diff.ProducerObservedCallEdges) / float64(diff.ResolvedCallEdges)
	}
	if diff.CallObservationAvailable {
		diff.CallObservationReason = ""
	} else if diff.CallObservationReason == "" {
		switch {
		case len(assertionIDs) > 0:
			diff.CallObservationReason = "runtime assertions contain no producer-authenticated call-event evidence"
		case len(traceIDs) > 0:
			diff.CallObservationReason = "producer-authenticated traces contain no authenticated call-event evidence"
		default:
			diff.CallObservationReason = "no authenticated call-event evidence is imported"
		}
	}
	diff.ProducerAuthenticatedTraceIDs = make([]string, 0, len(traceIDs))
	for id := range traceIDs {
		diff.ProducerAuthenticatedTraceIDs = append(diff.ProducerAuthenticatedTraceIDs, id)
	}
	sort.Strings(diff.ProducerAuthenticatedTraceIDs)
	for id := range assertionIDs {
		diff.UnverifiedAssertionIDs = append(diff.UnverifiedAssertionIDs, id)
	}
	sort.Strings(diff.UnverifiedAssertionIDs)
	for id := range captureIntegrityAssertionIDs {
		diff.CaptureIntegrityAssertionIDs = append(diff.CaptureIntegrityAssertionIDs, id)
	}
	sort.Strings(diff.CaptureIntegrityAssertionIDs)
	return diff
}

func stringValuesIntersect(value any, values map[string]struct{}) bool {
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if _, ok := values[item]; ok {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				if _, exists := values[text]; exists {
					return true
				}
			}
		}
	}
	return false
}

func hasStringValues(value any) bool {
	switch typed := value.(type) {
	case []string:
		return len(typed) > 0
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				return true
			}
		}
	}
	return false
}
