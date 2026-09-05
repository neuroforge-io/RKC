package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// NodeListOptions filters canonical nodes or optionally performs ranked node
// retrieval. Keep query and filters unchanged while following a cursor. Unknown
// filter values match no records; malformed parameters or cursors return errors.
type NodeListOptions struct {
	// Limit bounds this page to 1..1000 nodes; zero uses the server default 100.
	Limit int
	// Cursor is the previous page's opaque NextCursor, passed unchanged.
	Cursor string
	// ExpectedSnapshotID rejects a response from another snapshot when nonempty.
	// Set it to the first page's SnapshotID before requesting subsequent pages.
	ExpectedSnapshotID string
	// Query optionally selects lexical node retrieval instead of inventory order.
	Query string
	// Kind restricts results to this exact node kind when nonempty.
	Kind string
	// Language restricts results to this exact language when nonempty.
	Language string
}

// ArtifactListOptions filters canonical inventory records.
type ArtifactListOptions struct {
	// Limit bounds this page to 1..5000 artifacts; zero uses server default 100.
	Limit int
	// Cursor is the previous page's opaque NextCursor, passed unchanged.
	Cursor string
	// ExpectedSnapshotID pins the response to a prior page's snapshot if set.
	ExpectedSnapshotID string
	// Language restricts results to this exact language when nonempty.
	Language string
	// Status restricts results to this exact inventory status when nonempty.
	Status string
	// PathPrefix matches the beginning of the repository-relative artifact path.
	PathPrefix string
}

// EdgeListOptions filters canonical graph relationships.
type EdgeListOptions struct {
	// Limit bounds this page to 1..5000 edges; zero uses server default 100.
	Limit int
	// Cursor is the previous page's opaque NextCursor, passed unchanged.
	Cursor string
	// ExpectedSnapshotID pins the response to a prior page's snapshot if set.
	ExpectedSnapshotID string
	// Kind restricts results to this exact relationship kind when nonempty.
	Kind string
	// From restricts results to edges whose source is this opaque node ID.
	From string
	// To restricts results to edges whose destination is this opaque node ID.
	To string
	// Resolution restricts edge resolution, using the server's canonical aliases.
	Resolution string
}

// DiagnosticListOptions filters canonical analyzer and validation findings.
type DiagnosticListOptions struct {
	// Limit bounds this page to 1..5000 findings; zero uses server default 100.
	Limit int
	// Cursor is the previous page's opaque NextCursor, passed unchanged.
	Cursor string
	// ExpectedSnapshotID pins the response to a prior page's snapshot if set.
	ExpectedSnapshotID string
	// Severity restricts results to this exact severity when nonempty.
	Severity string
	// Code restricts results to this exact diagnostic code when nonempty.
	Code string
}

// SearchPageOptions filters and continues a ranked search. Keep query and
// filters unchanged across pages; Limit may change. Zero values use defaults.
type SearchPageOptions struct {
	// Limit bounds this page to 1..1000 hits; zero uses server default 50.
	Limit int
	// Cursor is the previous response's opaque NextCursor, passed unchanged.
	Cursor string
	// ExpectedSnapshotID pins the response to a prior page's snapshot if set.
	ExpectedSnapshotID string
	// Kinds restricts results to the listed canonical indexed-object kinds.
	Kinds []string
	// Languages restricts results to the listed normalized languages.
	Languages []string
	// ObjectTypes restricts results to node, artifact, or document projections.
	ObjectTypes []string
	// PathPrefix matches the beginning of the repository-relative indexed path.
	PathPrefix string
}

// ListNodes retrieves one page of canonical nodes. Query switches ordering to
// lexical rank. The client checks both the response snapshot header and body.
func (client *Client) ListNodes(ctx context.Context, options NodeListOptions) (rkcapi.NodePage, error) {
	values, err := collectionValues(options.Limit, 1000, options.Cursor)
	if err != nil {
		return rkcapi.NodePage{}, err
	}
	setCollectionFilters(values, map[string]string{"q": options.Query, "kind": options.Kind, "language": options.Language})
	return getCollection[rkcmodel.Node](client, ctx, "/api/v1/nodes", values, options.ExpectedSnapshotID)
}

// ListArtifacts retrieves one page of canonical inventory records and checks
// that the response snapshot header and body identify the same dataset.
func (client *Client) ListArtifacts(ctx context.Context, options ArtifactListOptions) (rkcapi.ArtifactPage, error) {
	values, err := collectionValues(options.Limit, 5000, options.Cursor)
	if err != nil {
		return rkcapi.ArtifactPage{}, err
	}
	setCollectionFilters(values, map[string]string{"language": options.Language, "status": options.Status, "path_prefix": options.PathPrefix})
	return getCollection[rkcmodel.Artifact](client, ctx, "/api/v1/artifacts", values, options.ExpectedSnapshotID)
}

// ListEdges retrieves one page of canonical relationships and checks that the
// response snapshot header and body identify the same dataset.
func (client *Client) ListEdges(ctx context.Context, options EdgeListOptions) (rkcapi.EdgePage, error) {
	values, err := collectionValues(options.Limit, 5000, options.Cursor)
	if err != nil {
		return rkcapi.EdgePage{}, err
	}
	setCollectionFilters(values, map[string]string{"kind": options.Kind, "from": options.From, "to": options.To, "resolution": options.Resolution})
	return getCollection[rkcmodel.Edge](client, ctx, "/api/v1/edges", values, options.ExpectedSnapshotID)
}

// ListDiagnostics retrieves one page of canonical findings and checks that the
// response snapshot header and body identify the same dataset.
func (client *Client) ListDiagnostics(ctx context.Context, options DiagnosticListOptions) (rkcapi.DiagnosticPage, error) {
	values, err := collectionValues(options.Limit, 5000, options.Cursor)
	if err != nil {
		return rkcapi.DiagnosticPage{}, err
	}
	setCollectionFilters(values, map[string]string{"severity": options.Severity, "code": options.Code})
	return getCollection[rkcmodel.Diagnostic](client, ctx, "/api/v1/diagnostics", values, options.ExpectedSnapshotID)
}

// SearchPage retrieves one snapshot-bound page of ranked indexed objects. It
// requires the pagination contract; Search remains compatible with older servers.
// Unknown filter values match no records. Invalid parameters or expired,
// modified, or mismatched cursors produce HTTP 400 errors.
// All cursor and filter errors are returned to the caller without retrying or
// silently restarting the search on a different snapshot.
func (client *Client) SearchPage(ctx context.Context, query string, options SearchPageOptions) (SearchResponse, error) {
	values, err := collectionValues(options.Limit, 1000, options.Cursor)
	if err != nil {
		return SearchResponse{}, err
	}
	values.Set("q", query)
	setCollectionFilters(values, map[string]string{"path_prefix": options.PathPrefix})
	for key, items := range map[string][]string{"kinds": options.Kinds, "languages": options.Languages, "object_types": options.ObjectTypes} {
		if len(items) > 0 {
			values.Set(key, strings.Join(items, ","))
		}
	}
	var response searchPageEnvelope
	if err := client.get(ctx, "/api/v1/search", values, &response); err != nil {
		return SearchResponse{}, err
	}
	if err := expectedCollectionSnapshot(options.ExpectedSnapshotID, response.SnapshotID); err != nil {
		return SearchResponse{}, err
	}
	if err := validatePageMetadata(len(response.Hits), response.Hits != nil, response.Total, response.Truncated, response.NextCursor, values, 50); err != nil {
		return SearchResponse{}, err
	}
	populateSearchCompatibility(&response.SearchResponse)
	return response.SearchResponse, nil
}

// A distinct target opts SearchPage into header/body verification without
// changing the behavior of Search against older, unbound HTTP responses.
type searchPageEnvelope struct{ SearchResponse }

func collectionValues(limit, maximum int, cursor string) (url.Values, error) {
	if limit < 0 || limit > maximum {
		return nil, fmt.Errorf("page limit must be zero (server default) or between 1 and %d", maximum)
	}
	values := url.Values{}
	if limit != 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	return values, nil
}

func setCollectionFilters(values url.Values, filters map[string]string) {
	for key, value := range filters {
		if value != "" {
			values.Set(key, value)
		}
	}
}

func getCollection[T any](client *Client, ctx context.Context, endpoint string, values url.Values, expected string) (rkcapi.CollectionPage[T], error) {
	var page rkcapi.CollectionPage[T]
	if err := client.get(ctx, endpoint, values, &page); err != nil {
		return rkcapi.CollectionPage[T]{}, err
	}
	if err := expectedCollectionSnapshot(expected, page.SnapshotID); err != nil {
		return rkcapi.CollectionPage[T]{}, err
	}
	if err := validatePageMetadata(len(page.Items), page.Items != nil, page.Total, page.Truncated, page.NextCursor, values, 100); err != nil {
		return rkcapi.CollectionPage[T]{}, err
	}
	return page, nil
}

// Bound every page before exposing it to traversal code. A malformed response
// must not cause silent omissions, unbounded results, or an endless cursor loop.
func validatePageMetadata(count int, arrayPresent bool, total int, truncated bool, cursor string, request url.Values, defaultLimit int) error {
	limit := defaultLimit
	if requested := request.Get("limit"); requested != "" {
		limit, _ = strconv.Atoi(requested) // collectionValues admitted this integer.
	}
	if !arrayPresent {
		return fmt.Errorf("RKC page requires a nonnull result array")
	}
	if total < 0 || total < count {
		return fmt.Errorf("RKC page total %d is smaller than its %d results", total, count)
	}
	if count > limit {
		return fmt.Errorf("RKC page has %d results, exceeding requested limit %d", count, limit)
	}
	if truncated != (cursor != "") {
		return fmt.Errorf("RKC page truncated flag disagrees with continuation cursor")
	}
	if truncated && (count == 0 || total <= count) {
		return fmt.Errorf("RKC truncated page must contain results and report further matches")
	}
	if len(cursor) > 4096 {
		return fmt.Errorf("RKC page continuation cursor exceeds 4096 bytes")
	}
	if cursor != "" && cursor == request.Get("cursor") {
		return fmt.Errorf("RKC page repeats its requested continuation cursor")
	}
	return nil
}

func expectedCollectionSnapshot(expected, actual string) error {
	if expected != "" && expected != actual {
		return fmt.Errorf("RKC page snapshot %q differs from expected snapshot %q", actual, expected)
	}
	return nil
}
