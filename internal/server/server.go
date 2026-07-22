package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	graphindex "github.com/repository-knowledge-compiler/rkc/internal/graph"
	"github.com/repository-knowledge-compiler/rkc/internal/model"
	"github.com/repository-knowledge-compiler/rkc/internal/search"
)

type Dataset struct {
	Root         string
	Manifest     model.Snapshot
	Coverage     model.Coverage
	Bundle       model.Bundle
	NodeByID     map[string]model.Node
	ArtifactByID map[string]model.Artifact
	EvidenceByID map[string]model.Evidence
	Graph        *graphindex.Index
	Search       *search.Index
	LoadedAt     time.Time
}

func Load(outputRoot string) (*Dataset, error) {
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return nil, err
	}
	if filepath.Base(root) == "site" {
		parent := filepath.Dir(root)
		if _, err := os.Stat(filepath.Join(parent, "rkc.manifest.json")); err == nil {
			root = parent
		}
	}
	var bundle model.Bundle
	if err := readJSON(filepath.Join(root, "bundle.json"), &bundle); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load bundle: %w", err)
		}
		bundle, err = loadLegacyBundle(root)
		if err != nil {
			return nil, err
		}
	}
	var manifest model.Snapshot
	if err := readJSON(filepath.Join(root, "rkc.manifest.json"), &manifest); err != nil {
		if bundle.Snapshot.ID == "" {
			return nil, fmt.Errorf("load manifest: %w", err)
		}
		manifest = bundle.Snapshot
	}
	bundle.Snapshot = manifest
	var coverage model.Coverage
	if err := readJSON(filepath.Join(root, "coverage.json"), &coverage); err != nil {
		coverage = model.BuildCoverage(bundle)
	}

	searchIndex, err := search.Load(filepath.Join(root, "search", "index.json"))
	if err != nil {
		searchIndex = search.BuildFromBundle(bundle)
	}
	dataset := &Dataset{
		Root: root, Manifest: manifest, Coverage: coverage, Bundle: bundle,
		NodeByID: make(map[string]model.Node, len(bundle.Nodes)), ArtifactByID: make(map[string]model.Artifact, len(bundle.Artifacts)),
		EvidenceByID: make(map[string]model.Evidence, len(bundle.Evidence)), Graph: graphindex.Build(bundle.Nodes, bundle.Edges),
		Search: searchIndex, LoadedAt: time.Now().UTC(),
	}
	for _, node := range bundle.Nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, artifact := range bundle.Artifacts {
		dataset.ArtifactByID[artifact.ID] = artifact
	}
	for _, evidence := range bundle.Evidence {
		dataset.EvidenceByID[evidence.ID] = evidence
	}
	return dataset, nil
}

func loadLegacyBundle(root string) (model.Bundle, error) {
	var bundle model.Bundle
	if err := readJSON(filepath.Join(root, "rkc.manifest.json"), &bundle.Snapshot); err != nil {
		return bundle, fmt.Errorf("load manifest: %w", err)
	}
	for path, target := range map[string]any{
		filepath.Join(root, "graph", "artifacts.jsonl"):   &bundle.Artifacts,
		filepath.Join(root, "graph", "nodes.jsonl"):       &bundle.Nodes,
		filepath.Join(root, "graph", "edges.jsonl"):       &bundle.Edges,
		filepath.Join(root, "graph", "evidence.jsonl"):    &bundle.Evidence,
		filepath.Join(root, "graph", "diagnostics.jsonl"): &bundle.Diagnostics,
	} {
		switch values := target.(type) {
		case *[]model.Artifact:
			if err := readJSONL(path, values); err != nil {
				return bundle, err
			}
		case *[]model.Node:
			if err := readJSONL(path, values); err != nil {
				return bundle, err
			}
		case *[]model.Edge:
			if err := readJSONL(path, values); err != nil {
				return bundle, err
			}
		case *[]model.Evidence:
			if err := readJSONL(path, values); err != nil {
				return bundle, err
			}
		case *[]model.Diagnostic:
			if err := readJSONL(path, values); err != nil {
				return bundle, err
			}
		}
	}
	return bundle, nil
}

func (dataset *Dataset) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", dataset.handleHealth)
	mux.HandleFunc("GET /api/v1/manifest", dataset.handleManifest)
	mux.HandleFunc("GET /api/v1/coverage", dataset.handleCoverage)
	mux.HandleFunc("GET /api/v1/artifacts", dataset.handleArtifacts)
	mux.HandleFunc("GET /api/v1/artifacts/{artifactID}", dataset.handleArtifact)
	mux.HandleFunc("GET /api/v1/nodes", dataset.handleNodes)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", dataset.handleNode)
	mux.HandleFunc("GET /api/v1/edges", dataset.handleEdges)
	mux.HandleFunc("GET /api/v1/evidence/{evidenceID}", dataset.handleEvidence)
	mux.HandleFunc("GET /api/v1/diagnostics", dataset.handleDiagnostics)
	mux.HandleFunc("GET /api/v1/search", dataset.handleSearch)
	mux.HandleFunc("GET /api/v1/graph/neighborhood", dataset.handleNeighborhood)
	mux.HandleFunc("GET /api/v1/graph/path", dataset.handlePath)
	mux.HandleFunc("GET /api/v1/graph/components", dataset.handleComponents)
	mux.HandleFunc("GET /api/v1/impact", dataset.handleImpact)
	mux.Handle("/", http.FileServer(http.Dir(filepath.Join(dataset.Root, "site"))))
	return securityHeaders(mux)
}

func (dataset *Dataset) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "schema_version": dataset.Manifest.SchemaVersion, "snapshot_id": dataset.Manifest.ID,
		"loaded_at": dataset.LoadedAt, "nodes": len(dataset.Bundle.Nodes), "edges": len(dataset.Bundle.Edges),
		"search_index_version": dataset.Search.Version,
	})
}
func (dataset *Dataset) handleManifest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dataset.Manifest)
}
func (dataset *Dataset) handleCoverage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dataset.Coverage)
}

func (dataset *Dataset) handleArtifacts(w http.ResponseWriter, request *http.Request) {
	language := request.URL.Query().Get("language")
	status := request.URL.Query().Get("status")
	pathPrefix := request.URL.Query().Get("path_prefix")
	limit := parseLimit(request, 100, 5000)
	var items []model.Artifact
	for _, artifact := range dataset.Bundle.Artifacts {
		if language != "" && artifact.Language != language {
			continue
		}
		if status != "" && artifact.Status != status {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(artifact.Path, pathPrefix) {
			continue
		}
		items = append(items, artifact)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": len(items) == limit})
}
func (dataset *Dataset) handleArtifact(w http.ResponseWriter, request *http.Request) {
	artifact, ok := dataset.ArtifactByID[request.PathValue("artifactID")]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Artifact not found", request.PathValue("artifactID"))
		return
	}
	var nodes []model.Node
	for _, node := range dataset.Bundle.Nodes {
		if node.ArtifactID == artifact.ID {
			nodes = append(nodes, node)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": artifact, "nodes": nodes})
}

func (dataset *Dataset) handleNodes(w http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	kind := request.URL.Query().Get("kind")
	language := request.URL.Query().Get("language")
	limit := parseLimit(request, 100, 1000)
	if query != "" {
		response := dataset.Search.Search(search.Query{Text: query, Kinds: optionalSet(kind), Languages: optionalSet(language), ObjectTypes: map[string]struct{}{"node": {}}, Limit: limit})
		var items []model.Node
		for _, hit := range response.Hits {
			if node, ok := dataset.NodeByID[hit.Document.ID]; ok {
				items = append(items, node)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": response.Truncated, "retrieval": response})
		return
	}
	var items []model.Node
	for _, node := range dataset.Bundle.Nodes {
		if kind != "" && node.Kind != kind {
			continue
		}
		if language != "" && node.Language != language {
			continue
		}
		items = append(items, node)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": len(items) == limit})
}

func (dataset *Dataset) handleNode(w http.ResponseWriter, request *http.Request) {
	id := request.PathValue("nodeID")
	node, ok := dataset.NodeByID[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Node not found", id)
		return
	}
	var evidence []model.Evidence
	for _, evidenceID := range node.EvidenceIDs {
		if item, ok := dataset.EvidenceByID[evidenceID]; ok {
			evidence = append(evidence, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": node, "incoming_edges": dataset.Graph.Incoming[id], "outgoing_edges": dataset.Graph.Outgoing[id], "evidence": evidence,
	})
}

func (dataset *Dataset) handleEdges(w http.ResponseWriter, request *http.Request) {
	kind := request.URL.Query().Get("kind")
	from := request.URL.Query().Get("from")
	to := request.URL.Query().Get("to")
	resolution := request.URL.Query().Get("resolution")
	limit := parseLimit(request, 100, 5000)
	var items []model.Edge
	for _, edge := range dataset.Bundle.Edges {
		if kind != "" && edge.Kind != kind {
			continue
		}
		if from != "" && edge.From != from {
			continue
		}
		if to != "" && edge.To != to {
			continue
		}
		if resolution != "" && model.NormalizeResolution(edge.Resolution) != model.NormalizeResolution(resolution) {
			continue
		}
		items = append(items, edge)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": len(items) == limit})
}

func (dataset *Dataset) handleEvidence(w http.ResponseWriter, request *http.Request) {
	evidence, ok := dataset.EvidenceByID[request.PathValue("evidenceID")]
	if !ok {
		writeProblem(w, http.StatusNotFound, "Evidence not found", request.PathValue("evidenceID"))
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

func (dataset *Dataset) handleDiagnostics(w http.ResponseWriter, request *http.Request) {
	severity := request.URL.Query().Get("severity")
	code := request.URL.Query().Get("code")
	limit := parseLimit(request, 100, 5000)
	var items []model.Diagnostic
	for _, diagnostic := range dataset.Bundle.Diagnostics {
		if severity != "" && diagnostic.Severity != severity {
			continue
		}
		if code != "" && diagnostic.Code != code {
			continue
		}
		items = append(items, diagnostic)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": len(items) == limit})
}

func (dataset *Dataset) handleSearch(w http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query == "" {
		writeProblem(w, http.StatusBadRequest, "Missing query", "q is required")
		return
	}
	kinds := firstQuery(request, "kinds", "kind")
	languages := firstQuery(request, "languages", "language")
	objectTypes := firstQuery(request, "object_types", "type")
	response := dataset.Search.Search(search.Query{
		Text: query, Kinds: setFromCSV(kinds), Languages: setFromCSV(languages),
		ObjectTypes: setFromCSV(objectTypes), PathPrefix: request.URL.Query().Get("path_prefix"), Limit: parseLimit(request, 50, 1000),
	})
	writeJSON(w, http.StatusOK, response)
}

func (dataset *Dataset) handleNeighborhood(w http.ResponseWriter, request *http.Request) {
	seed := request.URL.Query().Get("node_id")
	if seed == "" {
		writeProblem(w, http.StatusBadRequest, "Missing node", "node_id is required")
		return
	}
	result, err := dataset.Graph.Neighborhood(seed, traversalOptions(request, 1, 500))
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Seed node not found", seed)
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Graph query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handlePath(w http.ResponseWriter, request *http.Request) {
	from := request.URL.Query().Get("from")
	to := request.URL.Query().Get("to")
	if from == "" || to == "" {
		writeProblem(w, http.StatusBadRequest, "Missing endpoints", "from and to are required")
		return
	}
	result, err := dataset.Graph.ShortestPath(from, to, traversalOptions(request, 8, 10000))
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Path endpoint not found", err.Error())
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Path query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handleImpact(w http.ResponseWriter, request *http.Request) {
	seed := request.URL.Query().Get("node_id")
	if seed == "" {
		writeProblem(w, http.StatusBadRequest, "Missing node", "node_id is required")
		return
	}
	options := traversalOptions(request, 4, 5000)
	if request.URL.Query().Get("direction") == "" {
		options.Direction = graphindex.DirectionIncoming
	}
	result, err := dataset.Graph.Impact(seed, options)
	if errors.Is(err, graphindex.ErrNodeNotFound) {
		writeProblem(w, http.StatusNotFound, "Seed node not found", seed)
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Impact query failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (dataset *Dataset) handleComponents(w http.ResponseWriter, request *http.Request) {
	components := dataset.Graph.StronglyConnectedComponents(setFromCSV(request.URL.Query().Get("edge_kinds")))
	cyclicOnly := firstQuery(request, "cyclic", "cycles_only") == "true"
	limit := parseLimit(request, 100, 10000)
	var items []graphindex.Component
	for _, component := range components {
		if cyclicOnly && !component.Cyclic {
			continue
		}
		items = append(items, component)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(components), "truncated": len(items) < len(components)})
}

func traversalOptions(request *http.Request, defaultDepth, defaultNodes int) graphindex.TraverseOptions {
	direction := graphindex.Direction(request.URL.Query().Get("direction"))
	switch direction {
	case graphindex.DirectionIncoming, graphindex.DirectionOutgoing, graphindex.DirectionBoth:
	default:
		direction = graphindex.DirectionBoth
	}
	return graphindex.TraverseOptions{
		Direction: direction, EdgeKinds: setFromCSV(request.URL.Query().Get("edge_kinds")), Resolutions: setFromCSV(request.URL.Query().Get("resolutions")),
		MaxDepth:          parseBoundedInt(request.URL.Query().Get("max_depth"), parseBoundedInt(request.URL.Query().Get("hops"), defaultDepth, 0, 64), 0, 64),
		MaxNodes:          parseBoundedInt(request.URL.Query().Get("max_nodes"), parseLimit(request, defaultNodes, 100000), 1, 100000),
		IncludeUnresolved: request.URL.Query().Get("include_unresolved") == "true",
	}
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func readJSONL[T any](path string, target *[]T) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return err
		}
		*target = append(*target, value)
	}
	return scanner.Err()
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(value)
}
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail})
}
func parseLimit(request *http.Request, fallback, maximum int) int {
	return parseBoundedInt(request.URL.Query().Get("limit"), fallback, 1, maximum)
}
func parseBoundedInt(value string, fallback, minimum, maximum int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		return fallback
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}
func setFromCSV(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
func optionalSet(value string) map[string]struct{} {
	if value == "" {
		return nil
	}
	return map[string]struct{}{value: {}}
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, request)
	})
}

var ErrDatasetNotFound = errors.New("generated RKC dataset not found")

func firstQuery(request *http.Request, names ...string) string {
	for _, name := range names {
		if value := request.URL.Query().Get(name); value != "" {
			return value
		}
	}
	return ""
}
