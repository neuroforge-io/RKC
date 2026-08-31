package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// ImportStats reports what one trace contributed to the bundle.
type ImportStats struct {
	TraceArtifactClaims           int    `json:"trace_artifact_claims"`
	ProducerObservedFunctions     int    `json:"producer_observed_functions"`
	FunctionsNotObserved          int    `json:"functions_not_observed"`
	ProducerObservedCallEdges     int    `json:"producer_observed_call_edges"`
	UndemonstratedCallEdges       int    `json:"undemonstrated_call_edges"`
	CallObservationAvailable      bool   `json:"call_observation_available"`
	CallObservationReason         string `json:"call_observation_reason"`
	ProducerAuthenticatedEvidence int    `json:"producer_authenticated_evidence"`
	AssertionEvidence             int    `json:"assertion_evidence"`
	AssertedFunctions             int    `json:"asserted_functions"`
	AssertedTests                 int    `json:"asserted_tests"`
	ProducerAuthenticated         bool   `json:"producer_authenticated"`
	ProducerAuthentication        string `json:"producer_authentication"`
	CaptureIntegrityAuthenticated bool   `json:"capture_integrity_authenticated"`
	CaptureIntegrity              string `json:"capture_integrity"`
	ProducerAuthenticatedPaths    int    `json:"producer_authenticated_execution_paths"`
	TraceTestClaims               int    `json:"trace_test_claims"`
	Digest                        string `json:"trace_digest"`
}

// Import applies one validated trace to a bundle. Current statement coverage
// is output from an explicitly authorized but unauthenticated producer. It can
// therefore add source-affine operator assertions, but it cannot set canonical
// execution truth, authenticate test outcomes, or prove a temporal path.
func Import(ctx context.Context, bundle *rkcmodel.Bundle, trace Trace) (ImportStats, error) {
	if ctx == nil {
		return ImportStats{}, errors.New("trace import context is required")
	}
	if bundle == nil {
		return ImportStats{}, errors.New("trace import bundle is required")
	}
	if err := ctx.Err(); err != nil {
		return ImportStats{}, err
	}
	if err := Validate(trace); err != nil {
		return ImportStats{}, err
	}
	captureIntegrity := authenticatedCaptureIntegrity(trace)
	currentDigest, currentCount := contentAffinity(bundle.Artifacts)
	currentRepository := TraceRepository{
		RepositoryID: bundle.Snapshot.RepositoryID, ContentDigest: currentDigest,
		ArtifactCount: currentCount, GitCommit: bundle.Snapshot.Git.Commit,
		GitUnavailable: bundle.Snapshot.Git.Unavailable,
	}
	if !sameRepositoryAffinity(trace.Repository, currentRepository) {
		return ImportStats{}, errors.New("trace repository affinity does not match the scanned snapshot")
	}
	for _, node := range bundle.Nodes {
		if node.Kind == "trace" && node.Attributes["trace_id"] == trace.ID {
			return ImportStats{}, fmt.Errorf("trace %s is already imported", trace.ID)
		}
	}
	stats := ImportStats{
		TraceArtifactClaims: len(trace.Artifacts), TraceTestClaims: len(trace.Tests), Digest: trace.ID,
		CallObservationReason:         "no authenticated call-event producer is admitted",
		ProducerAuthenticated:         false,
		ProducerAuthentication:        "unavailable; current capture integrity and source affinity do not authenticate command output",
		CaptureIntegrityAuthenticated: captureIntegrity,
		CaptureIntegrity:              "portable self-hashed record",
	}
	if captureIntegrity {
		stats.CaptureIntegrity = "exact record produced by this RKC process; command output remains producer-unverified"
	}

	bundleArtifactByPath := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		bundleArtifactByPath[artifact.Path] = artifact
	}

	// Capture canonicalizes module-qualified coverage paths and binds every
	// coverage claim to exact source bytes. Import accepts exact paths only.
	executedIntervals := map[string][]lineInterval{}
	boundArtifacts := map[string]TraceArtifact{}
	for _, observed := range trace.Artifacts {
		if err := ctx.Err(); err != nil {
			return ImportStats{}, err
		}
		artifact, ok := bundleArtifactByPath[observed.Path]
		if !ok || artifact.SHA256 != observed.SourceSHA256 || artifact.SizeBytes != observed.SourceSizeBytes {
			return ImportStats{}, errors.New("trace source identity does not match the scanned snapshot")
		}
		bound := observed.Path
		executedIntervals[bound] = mergedLineIntervals(observed.ExecutedRanges)
		combined := boundArtifacts[bound]
		combined.Path = bound
		combined.Statements += observed.Statements
		combined.ExecutedStatements += observed.ExecutedStatements
		combined.ExecutedRanges = append(combined.ExecutedRanges, observed.ExecutedRanges...)
		boundArtifacts[bound] = combined
	}

	for index := range bundle.Nodes {
		node := &bundle.Nodes[index]
		if !isFunctionLike(node.Kind) || node.Source == nil {
			continue
		}
		intervals, covered := executedIntervals[node.Source.Path]
		if !covered {
			continue
		}
		executed := spanExecuted(intervals, node.Source.StartLine, node.Source.EndLine)
		if node.Attributes == nil {
			node.Attributes = map[string]any{}
		}
		if executed {
			method := "trace.function.assertion"
			evidenceID := rkcmodel.StableID("evidence", PluginID, method, node.ID, trace.ID)
			source := *node.Source
			bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "user_asserted", Method: method, Confidence: 0.5,
				Source: &source, Tool: PluginID, ToolVersion: PluginVersion,
				InputDigest: trace.ID,
				Detail:      "trace claims that an executed statement span intersects this function",
				Attributes: map[string]any{
					"trace_id": trace.ID, "producer_authenticated": false,
					"capture_integrity_authenticated": captureIntegrity,
					"source_stability":                "pre_post_endpoints_only",
				},
			})
			node.EvidenceIDs = appendUniqueString(node.EvidenceIDs, evidenceID)
			node.Attributes["execution_asserted_trace_ids"] = appendUniqueString(node.Attributes["execution_asserted_trace_ids"], trace.ID)
			stats.AssertedFunctions++
			stats.AssertionEvidence++
		} else {
			node.Attributes["execution_not_observed_trace_ids"] = appendUniqueString(node.Attributes["execution_not_observed_trace_ids"], trace.ID)
			evidenceID := rkcmodel.StableID("evidence", PluginID, "trace.function.not_observed.assertion", node.ID, trace.ID)
			source := *node.Source
			bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{
				ID: evidenceID, Kind: "user_asserted", Method: "trace.function.not_observed.assertion", Confidence: 0.5,
				Source: &source, Tool: PluginID, ToolVersion: PluginVersion,
				InputDigest: trace.ID,
				Detail:      "trace claims that no executed statement span intersected this function in this capture",
				Attributes: map[string]any{
					"trace_id": trace.ID, "producer_authenticated": false,
					"capture_integrity_authenticated": captureIntegrity,
					"negative_scope":                  "this_trace_only",
					"source_stability":                "pre_post_endpoints_only",
				},
			})
			node.EvidenceIDs = appendUniqueString(node.EvidenceIDs, evidenceID)
			stats.FunctionsNotObserved++
			stats.AssertionEvidence++
		}
	}

	for index := range bundle.Edges {
		edge := &bundle.Edges[index]
		if edge.Kind != "calls" || rkcmodel.NormalizeResolution(edge.Resolution) == rkcmodel.ResolutionUnresolved {
			continue
		}
		// Statement coverage claims that source spans were covered; it neither
		// authenticates execution nor identifies a dynamic call edge. Only a future
		// producer-authenticated call-event source may set observed=true.
		stats.UndemonstratedCallEdges++
	}

	boundPaths := make([]string, 0, len(boundArtifacts))
	for path := range boundArtifacts {
		boundPaths = append(boundPaths, path)
	}
	sort.Strings(boundPaths)
	for _, path := range boundPaths {
		observed := boundArtifacts[path]
		if observed.ExecutedStatements <= 0 {
			continue
		}
		artifact := bundleArtifactByPath[path]
		method := "trace.coverage.assertion"
		id := rkcmodel.StableID("evidence", PluginID, method, path, trace.ID)
		bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{
			ID: id, Kind: "user_asserted", Method: method, Confidence: 0.5,
			Source: &rkcmodel.SourceRange{
				ArtifactID: artifact.ID, Path: path,
				StartLine: artifactRangeStart(observed), EndLine: artifactRangeEnd(observed),
			},
			Tool: PluginID, ToolVersion: PluginVersion, InputDigest: trace.ID,
			Detail: fmt.Sprintf("claims %d/%d statements executed", observed.ExecutedStatements, observed.Statements),
			Attributes: map[string]any{
				"trace_id": trace.ID, "producer_authenticated": false,
				"capture_integrity_authenticated": captureIntegrity,
				"source_stability":                "pre_post_endpoints_only",
			},
		})
		stats.AssertionEvidence++
	}
	for _, test := range trace.Tests {
		method := "trace.test.assertion"
		id := rkcmodel.StableID("evidence", PluginID, method, trace.ID, test.Package, test.Name)
		bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{
			ID: id, Kind: "user_asserted", Method: method, Confidence: 0.5,
			Tool: PluginID, ToolVersion: PluginVersion, InputDigest: trace.ID,
			Detail: qualifiedTestName(test),
			Attributes: map[string]any{
				"trace_id": trace.ID, "package": test.Package, "test": test.Name,
				"status": test.Status, "elapsed_ms": test.Elapsed,
				"producer_authenticated":          false,
				"capture_integrity_authenticated": captureIntegrity,
			},
		})
		stats.AssertionEvidence++
		stats.AssertedTests++
	}
	traceMethod := "trace.capture.assertion"
	traceName := "Unverified runtime assertion " + trace.ID[:12]
	traceDetail := fmt.Sprintf("asserts %d bound coverage artifacts and %d terminal test-result claims", len(boundArtifacts), len(trace.Tests))
	if captureIntegrity {
		traceName = "Integrity-authenticated capture assertion " + trace.ID[:12]
		traceDetail = fmt.Sprintf("this RKC process captured assertions for %d bound coverage artifacts and %d terminal test-result claims; producer authority is unavailable", len(boundArtifacts), len(trace.Tests))
	}
	traceEvidenceID := rkcmodel.StableID("evidence", PluginID, traceMethod, trace.ID)
	bundle.Evidence = append(bundle.Evidence, rkcmodel.Evidence{
		ID: traceEvidenceID, Kind: "user_asserted", Method: traceMethod, Confidence: 0.5,
		Tool: PluginID, ToolVersion: PluginVersion, InputDigest: trace.ID,
		Detail: traceDetail,
		Attributes: map[string]any{
			"trace_id": trace.ID, "exit_code": trace.ExitCode,
			"producer_authenticated":          false,
			"producer_authentication":         stats.ProducerAuthentication,
			"capture_integrity_authenticated": captureIntegrity,
			"capture_integrity":               stats.CaptureIntegrity,
			"source_stability":                "pre_post_endpoints_only",
			"call_observation_available":      false,
			"call_observation_reason":         stats.CallObservationReason,
		},
	})
	stats.AssertionEvidence++
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
		ID:   rkcmodel.StableID("node", PluginID, "trace", trace.ID),
		Kind: "trace", Name: traceName, QualifiedName: "runtime:" + trace.ID,
		EvidenceIDs: []string{traceEvidenceID},
		Attributes: map[string]any{
			"trace_id": trace.ID, "command": trace.Command, "exit_code": trace.ExitCode,
			"duration_ms": trace.DurationMS, "artifacts": len(boundArtifacts), "tests": len(trace.Tests),
			"producer_authenticated":          false,
			"producer_authentication":         stats.ProducerAuthentication,
			"capture_integrity_authenticated": captureIntegrity,
			"capture_integrity":               stats.CaptureIntegrity,
			"source_stability":                "pre_post_endpoints_only",
			"call_observation_available":      false,
			"call_observation_reason":         stats.CallObservationReason,
		},
	})
	postDigest, postCount := contentAffinity(bundle.Artifacts)
	if postDigest != currentDigest || postCount != currentCount {
		return ImportStats{}, errors.New("snapshot artifacts changed during trace import")
	}
	return stats, nil
}

// spanExecuted reports whether a producer-reported covered range intersects
// the span. The caller assigns assertion authority to that result.
type lineInterval struct {
	start int
	end   int
}

func mergedLineIntervals(ranges []ExecutedRange) []lineInterval {
	intervals := make([]lineInterval, 0, len(ranges))
	for _, span := range ranges {
		intervals = append(intervals, lineInterval{start: span.StartLine, end: span.EndLine})
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end < intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})
	merged := intervals[:0]
	for _, interval := range intervals {
		if len(merged) == 0 || interval.start > merged[len(merged)-1].end {
			merged = append(merged, interval)
			continue
		}
		if interval.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = interval.end
		}
	}
	return merged
}

func spanExecuted(intervals []lineInterval, startLine, endLine int) bool {
	if len(intervals) == 0 || startLine <= 0 {
		return false
	}
	if endLine < startLine {
		endLine = startLine
	}
	index := sort.Search(len(intervals), func(index int) bool { return intervals[index].end >= startLine })
	return index < len(intervals) && intervals[index].start <= endLine
}

func isFunctionLike(kind string) bool {
	switch kind {
	case "function", "method", "constructor", "test":
		return true
	default:
		return false
	}
}

func appendUniqueString(value any, addition string) []string {
	values := make([]string, 0, 2)
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
	}
	for _, current := range values {
		if current == addition {
			sort.Strings(values)
			return values
		}
	}
	values = append(values, addition)
	sort.Strings(values)
	return values
}

func qualifiedTestName(test TraceTest) string {
	if test.Package == "" {
		return test.Name
	}
	return test.Package + "/" + test.Name
}

func artifactRangeStart(artifact TraceArtifact) int {
	if len(artifact.ExecutedRanges) == 0 {
		return 0
	}
	start := artifact.ExecutedRanges[0].StartLine
	for _, span := range artifact.ExecutedRanges[1:] {
		if span.StartLine < start {
			start = span.StartLine
		}
	}
	return start
}

func artifactRangeEnd(artifact TraceArtifact) int {
	if len(artifact.ExecutedRanges) == 0 {
		return 0
	}
	end := artifact.ExecutedRanges[0].EndLine
	for _, span := range artifact.ExecutedRanges[1:] {
		if span.EndLine > end {
			end = span.EndLine
		}
	}
	return end
}

// Digest computes the canonical content SHA-256 of a trace. It matches IDFor.
func Digest(trace Trace) string { return IDFor(trace) }
