// Package client is a dependency-free Go client for the local RKC read API.
// It is intentionally small enough to embed in editor extensions, CI helpers,
// and integration tests without importing server internals.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// Client sends read-only requests to one RKC HTTP API. A Client may be reused
// across goroutines when its supplied HTTP client and transport are safe for
// concurrent use.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	token   string
}

// Option customizes a Client during construction.
type Option func(*Client)

// WithHTTPClient supplies the HTTP transport, timeout, and redirect policy used
// by a Client. A nil value is ignored, leaving the default 15-second client in
// place.
func WithHTTPClient(value *http.Client) Option {
	return func(client *Client) {
		if value != nil {
			client.http = value
		}
	}
}

// WithBearerToken authenticates requests with an HTTP Bearer token. Leading
// and trailing whitespace is removed; an empty token disables the header.
func WithBearerToken(value string) Option {
	return func(client *Client) { client.token = strings.TrimSpace(value) }
}

// New constructs a Client for an HTTP or HTTPS RKC base URL. The base URL may
// include a path prefix. New rejects URLs without a supported scheme or host.
func New(baseURL string, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse RKC base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("RKC base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("RKC base URL requires a host")
	}
	client := &Client{baseURL: parsed, http: &http.Client{Timeout: 15 * time.Second}}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// Health describes the active RKC dataset reported by the health endpoint.
type Health struct {
	// Status is the server's readiness state, normally "ok" for a loaded dataset.
	Status string `json:"status"`
	// SchemaVersion identifies the canonical RKC data schema in use.
	SchemaVersion string `json:"schema_version,omitempty"`
	// SnapshotID identifies the immutable snapshot currently being served.
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// NodeDetails is the complete response returned by the node endpoint. The Node
// method is a convenience for callers that need only the canonical node record.
type NodeDetails struct {
	// Node is the requested canonical graph node.
	Node rkcmodel.Node `json:"node"`
	// IncomingEdges contains direct edges whose destination is Node.
	IncomingEdges []rkcmodel.Edge `json:"incoming_edges"`
	// OutgoingEdges contains direct edges whose source is Node.
	OutgoingEdges []rkcmodel.Edge `json:"outgoing_edges"`
	// Evidence contains the evidence records referenced by Node.
	Evidence []rkcmodel.Evidence `json:"evidence"`
}

// SearchDocument is the indexed projection returned by the search endpoint.
// ObjectType distinguishes nodes, artifacts, and documents without coercing
// every result into a node.
type SearchDocument struct {
	// ID is the stable identifier of the indexed object.
	ID string `json:"id"`
	// ObjectType identifies the projection class, such as node, artifact, or document.
	ObjectType string `json:"object_type"`
	// Kind is the object's canonical RKC vocabulary kind when available.
	Kind string `json:"kind,omitempty"`
	// Language is the normalized source-language label when applicable.
	Language string `json:"language,omitempty"`
	// Title is the primary human-readable result label.
	Title string `json:"title"`
	// QualifiedName is the language-qualified symbol name when available.
	QualifiedName string `json:"qualified_name,omitempty"`
	// Signature is the indexed declaration signature when available.
	Signature string `json:"signature,omitempty"`
	// Path is the repository-relative source path when the object has one.
	Path string `json:"path,omitempty"`
	// Body is the bounded text projection used for retrieval and display.
	Body string `json:"body,omitempty"`
	// Metadata carries deterministic index attributes not represented above.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SearchHit pairs an indexed document with its relevance evidence.
type SearchHit struct {
	// Document is the canonical indexed projection that matched the query.
	Document SearchDocument `json:"document"`
	// Score is the search index's relevance score for Document.
	Score float64 `json:"score"`
	// Reasons describes which indexed signals contributed to the match.
	Reasons []string `json:"reasons"`
	// Terms contains the normalized query terms matched by Document.
	Terms []string `json:"terms"`
}

// SearchResponse is the ranked result set returned by Search or SearchPage.
type SearchResponse struct {
	// Query is the submitted query string echoed by the server.
	Query string `json:"query"`
	// Hits contains ranked canonical search results.
	Hits []SearchHit `json:"hits"`
	// Truncated reports whether matching hits remain after this page.
	Truncated bool `json:"truncated"`
	// Mode identifies the retrieval strategy used by the server.
	Mode string `json:"mode"`
	// IndexVersion identifies the search-index contract used for ranking.
	IndexVersion string `json:"index_version"`
	// Total counts all matching indexed objects, including earlier pages. Older
	// servers can omit this field; SearchPage requires the pagination contract.
	Total int `json:"total"`
	// SnapshotID identifies the immutable dataset used for these hits. Search
	// preserves compatibility with older servers that omit this field.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// NextCursor continues the same ranked search with unchanged query/filters.
	// It is opaque; an empty value means there is no next page.
	NextCursor string `json:"next_cursor,omitempty"`

	// Count is the number of Hits returned by Search.
	//
	// Deprecated: use len(Hits).
	Count int `json:"count,omitempty"`
	// Results is a lossy node-shaped projection populated from Hits.
	//
	// Deprecated: use Hits to preserve object type and indexed document fields.
	Results []SearchResult `json:"results,omitempty"`
}

// SearchResult is the legacy node-shaped search projection.
//
// Deprecated: use SearchHit, which preserves object type and indexed document
// fields.
type SearchResult struct {
	// Node is the lossy node-shaped projection of a matched document.
	Node rkcmodel.Node `json:"node"`
	// Score is the source SearchHit relevance score.
	Score float64 `json:"score"`
	// Reasons contains the source SearchHit matching signals.
	Reasons []string `json:"reasons,omitempty"`
}

// Neighborhood is a bounded graph traversal centered on one seed node.
type Neighborhood struct {
	// SeedID is the node from which traversal began.
	SeedID string `json:"seed_id"`
	// Nodes contains the seed and every reached node.
	Nodes []rkcmodel.Node `json:"nodes"`
	// Edges contains relationships observed within the traversal.
	Edges []rkcmodel.Edge `json:"edges"`
	// DepthByID maps each reached node ID to its distance from SeedID.
	DepthByID map[string]int `json:"depth_by_id"`
	// Truncated reports whether the node limit prevented a complete traversal.
	Truncated bool `json:"truncated"`

	// Center is a compatibility alias for SeedID.
	//
	// Deprecated: use SeedID.
	Center string `json:"center,omitempty"`
}

// PathResponse describes a bounded shortest-path search between two nodes.
type PathResponse struct {
	// Found reports whether a path was found within the requested bounds.
	Found bool `json:"found"`
	// FromID is the path's requested starting node.
	FromID string `json:"from_id"`
	// ToID is the path's requested destination node.
	ToID string `json:"to_id"`
	// NodeIDs lists path nodes in traversal order, including both endpoints.
	NodeIDs []string `json:"node_ids,omitempty"`
	// EdgeIDs lists path edges in traversal order.
	EdgeIDs []string `json:"edge_ids,omitempty"`
	// Nodes contains the canonical records corresponding to NodeIDs.
	Nodes []rkcmodel.Node `json:"nodes,omitempty"`
	// Edges contains the canonical records corresponding to EdgeIDs.
	Edges []rkcmodel.Edge `json:"edges,omitempty"`
	// Depth is the number of edges in a found path.
	Depth int `json:"depth,omitempty"`
	// Visited is the number of nodes examined during the bounded search.
	Visited int `json:"visited"`
}

// ImpactResponse describes nodes affected through a bounded graph traversal.
type ImpactResponse struct {
	// SeedID is the node whose impact was evaluated.
	SeedID string `json:"seed_id"`
	// ImpactedNodes contains reached nodes and excludes the seed itself.
	ImpactedNodes []rkcmodel.Node `json:"impacted_nodes"`
	// ImpactEdges contains relationships observed while finding impacted nodes.
	ImpactEdges []rkcmodel.Edge `json:"impact_edges"`
	// DepthByID maps reached node IDs, including SeedID, to traversal distance.
	DepthByID map[string]int `json:"depth_by_id"`
	// Truncated reports whether the node limit prevented a complete traversal.
	Truncated bool `json:"truncated"`

	// Root is a compatibility alias for SeedID.
	//
	// Deprecated: use SeedID.
	Root string `json:"root,omitempty"`
	// Nodes is a compatibility alias for ImpactedNodes.
	//
	// Deprecated: use ImpactedNodes.
	Nodes []rkcmodel.Node `json:"nodes,omitempty"`
	// Edges is a compatibility alias for ImpactEdges.
	//
	// Deprecated: use ImpactEdges.
	Edges []rkcmodel.Edge `json:"edges,omitempty"`
}

// SearchOptions narrows and bounds a Search request. Zero values leave the
// corresponding query parameters unset so the server applies its defaults.
type SearchOptions struct {
	// Limit is the maximum number of hits requested when positive.
	Limit int
	// Kind restricts results to a canonical RKC object kind when non-empty.
	Kind string
	// Language restricts results to a normalized language when non-empty.
	Language string
}

// NeighborhoodOptions controls a bounded Neighborhood traversal. Zero values
// leave the corresponding query parameters unset so the server applies its
// defaults.
type NeighborhoodOptions struct {
	// Hops is the maximum relationship depth requested when positive.
	Hops int
	// Direction is "incoming", "outgoing", or "both" when specified.
	Direction string
	// EdgeKinds restricts traversal to the listed canonical edge kinds.
	EdgeKinds []string
	// Limit is the maximum number of reached nodes requested when positive.
	Limit int
}

// ImpactOptions controls a bounded Impact traversal. The server defaults to
// incoming relationships, which answers which nodes depend on the seed.
type ImpactOptions struct {
	// Direction is "incoming", "outgoing", or "both" when specified.
	Direction string
	// EdgeKinds restricts traversal to the listed canonical edge kinds.
	EdgeKinds []string
	// MaxDepth is the maximum relationship depth requested when positive.
	MaxDepth int
	// Limit is the maximum number of reached nodes requested when positive.
	Limit int
}

// Health retrieves server readiness and active-snapshot identity.
func (client *Client) Health(ctx context.Context) (Health, error) {
	var output Health
	return output, client.get(ctx, "/api/v1/health", nil, &output)
}

// Manifest retrieves the active snapshot manifest.
func (client *Client) Manifest(ctx context.Context) (rkcmodel.Snapshot, error) {
	var output rkcmodel.Snapshot
	return output, client.get(ctx, "/api/v1/manifest", nil, &output)
}

// Coverage retrieves the active snapshot's coverage and completeness metrics.
func (client *Client) Coverage(ctx context.Context) (rkcmodel.Coverage, error) {
	var output rkcmodel.Coverage
	return output, client.get(ctx, "/api/v1/coverage", nil, &output)
}

// Node retrieves one canonical node by its opaque ID. Use NodeDetails when
// direct relationships or supporting evidence are also needed.
func (client *Client) Node(ctx context.Context, id string) (rkcmodel.Node, error) {
	output, err := client.NodeDetails(ctx, id)
	return output.Node, err
}

// NodeDetails returns a node together with its direct incoming/outgoing edges
// and supporting evidence.
func (client *Client) NodeDetails(ctx context.Context, id string) (NodeDetails, error) {
	var output NodeDetails
	return output, client.get(ctx, "/api/v1/nodes/"+url.PathEscape(id), nil, &output)
}

// Search retrieves ranked indexed objects matching query. The returned Hits
// preserve object types; deprecated compatibility projections are populated
// from those hits for older callers.
func (client *Client) Search(ctx context.Context, query string, options SearchOptions) (SearchResponse, error) {
	values := url.Values{"q": []string{query}}
	if options.Limit > 0 {
		values.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.Kind != "" {
		values.Set("kind", options.Kind)
	}
	if options.Language != "" {
		values.Set("language", options.Language)
	}
	var output SearchResponse
	if err := client.get(ctx, "/api/v1/search", values, &output); err != nil {
		return output, err
	}
	populateSearchCompatibility(&output)
	return output, nil
}

func populateSearchCompatibility(output *SearchResponse) {
	output.Count = len(output.Hits)
	output.Results = make([]SearchResult, 0, len(output.Hits))
	for _, hit := range output.Hits {
		output.Results = append(output.Results, SearchResult{
			Node: rkcmodel.Node{
				ID:            hit.Document.ID,
				Kind:          hit.Document.Kind,
				Name:          hit.Document.Title,
				QualifiedName: hit.Document.QualifiedName,
				Signature:     hit.Document.Signature,
				Language:      hit.Document.Language,
			},
			Score: hit.Score, Reasons: append([]string(nil), hit.Reasons...),
		})
	}
}

// Neighborhood retrieves a bounded graph traversal around nodeID.
func (client *Client) Neighborhood(ctx context.Context, nodeID string, options NeighborhoodOptions) (Neighborhood, error) {
	values := url.Values{"node_id": []string{nodeID}}
	if options.Hops > 0 {
		values.Set("hops", strconv.Itoa(options.Hops))
	}
	if options.Direction != "" {
		values.Set("direction", options.Direction)
	}
	if len(options.EdgeKinds) > 0 {
		values.Set("edge_kinds", strings.Join(options.EdgeKinds, ","))
	}
	if options.Limit > 0 {
		values.Set("limit", strconv.Itoa(options.Limit))
	}
	var output Neighborhood
	if err := client.get(ctx, "/api/v1/graph/neighborhood", values, &output); err != nil {
		return output, err
	}
	output.Center = output.SeedID
	return output, nil
}

// FindPath retrieves a bounded shortest path from one node to another. An empty
// edgeKinds slice leaves edge kinds unrestricted; a positive maxDepth requests
// a traversal-depth bound.
func (client *Client) FindPath(ctx context.Context, from, to string, edgeKinds []string, maxDepth int) (PathResponse, error) {
	values := url.Values{"from": []string{from}, "to": []string{to}}
	if len(edgeKinds) > 0 {
		values.Set("edge_kinds", strings.Join(edgeKinds, ","))
	}
	if maxDepth > 0 {
		values.Set("max_depth", strconv.Itoa(maxDepth))
	}
	var output PathResponse
	return output, client.get(ctx, "/api/v1/graph/path", values, &output)
}

// Impact retrieves nodes affected by nodeID through the selected relationship
// direction. The default server direction is incoming.
func (client *Client) Impact(ctx context.Context, nodeID string, options ImpactOptions) (ImpactResponse, error) {
	values := url.Values{"node_id": []string{nodeID}}
	if options.Direction != "" {
		values.Set("direction", options.Direction)
	}
	if len(options.EdgeKinds) > 0 {
		values.Set("edge_kinds", strings.Join(options.EdgeKinds, ","))
	}
	if options.MaxDepth > 0 {
		values.Set("max_depth", strconv.Itoa(options.MaxDepth))
	}
	if options.Limit > 0 {
		values.Set("limit", strconv.Itoa(options.Limit))
	}
	var output ImpactResponse
	if err := client.get(ctx, "/api/v1/impact", values, &output); err != nil {
		return output, err
	}
	output.Root = output.SeedID
	output.Nodes = append([]rkcmodel.Node(nil), output.ImpactedNodes...)
	output.Edges = append([]rkcmodel.Edge(nil), output.ImpactEdges...)
	return output, nil
}

func (client *Client) get(ctx context.Context, endpoint string, query url.Values, output any) error {
	requestURL := *client.baseURL
	escapedPath := path.Join(strings.TrimSuffix(client.baseURL.Path, "/"), endpoint)
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return fmt.Errorf("decode RKC endpoint path: %w", err)
	}
	requestURL.Path = decodedPath
	if escapedPath != decodedPath {
		// Preserve escaped slashes inside opaque node IDs. Assigning an already
		// escaped value to URL.Path would encode '%' a second time.
		requestURL.RawPath = escapedPath
	} else {
		requestURL.RawPath = ""
	}
	requestURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "rkc-go-client/0.2")
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("RKC GET %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Title   string `json:"title"`
			Detail  string `json:"detail"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &problem)
		modern := strings.TrimSpace(problem.Title)
		if detail := strings.TrimSpace(problem.Detail); detail != "" {
			if modern != "" {
				modern += ": "
			}
			modern += detail
		}
		message := first(modern, strings.TrimSpace(problem.Error), strings.TrimSpace(problem.Message), strings.TrimSpace(string(body)), response.Status)
		return fmt.Errorf("RKC GET %s: HTTP %d: %s", endpoint, response.StatusCode, message)
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode RKC %s response: %w", endpoint, err)
	}
	// New exchange contracts bind their payload to the same immutable selection
	// as the HTTP header. Reject mixed generations before handing data to callers.
	var snapshotID string
	switch value := output.(type) {
	case *rkcapi.ContextPacket:
		snapshotID = value.SnapshotID
	case *rkcapi.Capabilities:
		snapshotID = value.SnapshotID
	case *rkcapi.NodePage:
		snapshotID = value.SnapshotID
	case *rkcapi.ArtifactPage:
		snapshotID = value.SnapshotID
	case *rkcapi.EdgePage:
		snapshotID = value.SnapshotID
	case *rkcapi.DiagnosticPage:
		snapshotID = value.SnapshotID
	case *searchPageEnvelope:
		snapshotID = value.SnapshotID
	default:
		return nil
	}
	if snapshotID == "" || response.Header.Get("X-RKC-Snapshot-ID") != snapshotID {
		return fmt.Errorf("RKC %s response has a missing or mismatched snapshot identity", endpoint)
	}
	return nil
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown error"
}

// Capabilities discovers implemented workflows and output formats.
func (client *Client) Capabilities(ctx context.Context) (rkcapi.Capabilities, error) {
	var output rkcapi.Capabilities
	return output, client.get(ctx, "/api/v1/capabilities", nil, &output)
}

// Context retrieves a bounded, cited packet without running a model. A zero
// limit or maxBytes selects the server default; positive values are explicit.
func (client *Client) Context(ctx context.Context, query string, limit, maxBytes int) (rkcapi.ContextPacket, error) {
	values := url.Values{"q": []string{query}}
	if limit != 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if maxBytes != 0 {
		values.Set("max_bytes", strconv.Itoa(maxBytes))
	}
	var output rkcapi.ContextPacket
	return output, client.get(ctx, "/api/v1/context", values, &output)
}
