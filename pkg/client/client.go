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

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type Client struct {
	baseURL *url.URL
	http    *http.Client
	token   string
}

type Option func(*Client)

func WithHTTPClient(value *http.Client) Option {
	return func(client *Client) {
		if value != nil {
			client.http = value
		}
	}
}
func WithBearerToken(value string) Option {
	return func(client *Client) { client.token = strings.TrimSpace(value) }
}

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

type Health struct {
	Status        string `json:"status"`
	SchemaVersion string `json:"schema_version,omitempty"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
}

// NodeDetails is the complete response returned by the node endpoint. Node is
// retained as a convenience method for callers that need only the canonical
// node record.
type NodeDetails struct {
	Node          rkcmodel.Node       `json:"node"`
	IncomingEdges []rkcmodel.Edge     `json:"incoming_edges"`
	OutgoingEdges []rkcmodel.Edge     `json:"outgoing_edges"`
	Evidence      []rkcmodel.Evidence `json:"evidence"`
}

// SearchDocument is the indexed projection returned by the search endpoint.
// ObjectType distinguishes nodes, artifacts, and documents without coercing
// every result into a node.
type SearchDocument struct {
	ID            string            `json:"id"`
	ObjectType    string            `json:"object_type"`
	Kind          string            `json:"kind,omitempty"`
	Language      string            `json:"language,omitempty"`
	Title         string            `json:"title"`
	QualifiedName string            `json:"qualified_name,omitempty"`
	Signature     string            `json:"signature,omitempty"`
	Path          string            `json:"path,omitempty"`
	Body          string            `json:"body,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type SearchHit struct {
	Document SearchDocument `json:"document"`
	Score    float64        `json:"score"`
	Reasons  []string       `json:"reasons"`
	Terms    []string       `json:"terms"`
}

type SearchResponse struct {
	Query        string      `json:"query"`
	Hits         []SearchHit `json:"hits"`
	Truncated    bool        `json:"truncated"`
	Mode         string      `json:"mode"`
	IndexVersion string      `json:"index_version"`

	// Count and Results are deprecated compatibility projections. Results is a
	// lossy node-shaped view; use Hits for the server's canonical documents.
	Count   int            `json:"count,omitempty"`
	Results []SearchResult `json:"results,omitempty"`
}

// SearchResult is the legacy node-shaped search projection. New callers should
// use SearchHit, which preserves object type and indexed document fields.
type SearchResult struct {
	Node    rkcmodel.Node `json:"node"`
	Score   float64       `json:"score"`
	Reasons []string      `json:"reasons,omitempty"`
}
type Neighborhood struct {
	SeedID    string          `json:"seed_id"`
	Nodes     []rkcmodel.Node `json:"nodes"`
	Edges     []rkcmodel.Edge `json:"edges"`
	DepthByID map[string]int  `json:"depth_by_id"`
	Truncated bool            `json:"truncated"`

	// Center is a deprecated alias for SeedID.
	Center string `json:"center,omitempty"`
}
type PathResponse struct {
	Found   bool            `json:"found"`
	FromID  string          `json:"from_id"`
	ToID    string          `json:"to_id"`
	NodeIDs []string        `json:"node_ids,omitempty"`
	EdgeIDs []string        `json:"edge_ids,omitempty"`
	Nodes   []rkcmodel.Node `json:"nodes,omitempty"`
	Edges   []rkcmodel.Edge `json:"edges,omitempty"`
	Depth   int             `json:"depth,omitempty"`
	Visited int             `json:"visited"`
}
type ImpactResponse struct {
	SeedID        string          `json:"seed_id"`
	ImpactedNodes []rkcmodel.Node `json:"impacted_nodes"`
	ImpactEdges   []rkcmodel.Edge `json:"impact_edges"`
	DepthByID     map[string]int  `json:"depth_by_id"`
	Truncated     bool            `json:"truncated"`

	// Root, Nodes, and Edges are deprecated compatibility aliases for SeedID,
	// ImpactedNodes, and ImpactEdges respectively.
	Root  string          `json:"root,omitempty"`
	Nodes []rkcmodel.Node `json:"nodes,omitempty"`
	Edges []rkcmodel.Edge `json:"edges,omitempty"`
}

type SearchOptions struct {
	Limit    int
	Kind     string
	Language string
}
type NeighborhoodOptions struct {
	Hops      int
	Direction string
	EdgeKinds []string
	Limit     int
}
type ImpactOptions struct {
	Direction string
	EdgeKinds []string
	MaxDepth  int
	Limit     int
}

func (client *Client) Health(ctx context.Context) (Health, error) {
	var output Health
	return output, client.get(ctx, "/api/v1/health", nil, &output)
}
func (client *Client) Manifest(ctx context.Context) (rkcmodel.Snapshot, error) {
	var output rkcmodel.Snapshot
	return output, client.get(ctx, "/api/v1/manifest", nil, &output)
}
func (client *Client) Coverage(ctx context.Context) (rkcmodel.Coverage, error) {
	var output rkcmodel.Coverage
	return output, client.get(ctx, "/api/v1/coverage", nil, &output)
}
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
	return output, nil
}
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
