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

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
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
type SearchResponse struct {
	Query   string         `json:"query,omitempty"`
	Count   int            `json:"count,omitempty"`
	Results []SearchResult `json:"results"`
}
type SearchResult struct {
	Node    rkcmodel.Node `json:"node"`
	Score   float64       `json:"score"`
	Reasons []string      `json:"reasons,omitempty"`
}
type Neighborhood struct {
	Center    string          `json:"center"`
	Nodes     []rkcmodel.Node `json:"nodes"`
	Edges     []rkcmodel.Edge `json:"edges"`
	Truncated bool            `json:"truncated,omitempty"`
}
type PathResponse struct {
	Found   bool            `json:"found"`
	NodeIDs []string        `json:"node_ids,omitempty"`
	Edges   []rkcmodel.Edge `json:"edges,omitempty"`
}
type ImpactResponse struct {
	Root      string          `json:"root"`
	Nodes     []rkcmodel.Node `json:"nodes"`
	Edges     []rkcmodel.Edge `json:"edges"`
	Truncated bool            `json:"truncated,omitempty"`
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
	var output rkcmodel.Node
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
	return output, client.get(ctx, "/api/v1/search", values, &output)
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
	return output, client.get(ctx, "/api/v1/graph/neighborhood", values, &output)
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
	return output, client.get(ctx, "/api/v1/graph/impact", values, &output)
}

func (client *Client) get(ctx context.Context, endpoint string, query url.Values, output any) error {
	requestURL := *client.baseURL
	requestURL.Path = path.Join(strings.TrimSuffix(client.baseURL.Path, "/"), endpoint)
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
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &problem)
		message := first(problem.Error, problem.Message, strings.TrimSpace(string(body)), response.Status)
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
