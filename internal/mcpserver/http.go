package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/server"
)

// HTTPOptions bounds an explicitly selected credential-free local transport.
// It accepts only exact loopback IP literals; no remote deployment is provided.
type HTTPOptions struct {
	Listen            string
	RequestTimeout    time.Duration
	MaximumConcurrent int
}

// HTTPServer serves a pinned atlas using stateless Streamable HTTP JSON replies.
// It allocates no MCP sessions, emits no SSE, and performs no outbound requests.
type HTTPServer struct {
	listener net.Listener
	server   *http.Server
	endpoint string
}

type httpTransport struct {
	authority string
	slots     chan struct{}
	dispatch  func(context.Context, string, json.RawMessage) (any, *rpcError)
}

// ValidateHTTPOptions checks local transport bounds without opening a listener.
func ValidateHTTPOptions(options HTTPOptions) error {
	host, port, err := net.SplitHostPort(options.Listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return errors.New("HTTP MCP listen must be 127.0.0.1:port or [::1]:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 || strconv.Itoa(number) != port {
		return errors.New("HTTP MCP port must be an integer from 0 through 65535")
	}
	if options.RequestTimeout <= 0 || options.RequestTimeout > 30*time.Second {
		return errors.New("HTTP MCP request timeout must be positive and at most 30s")
	}
	if options.MaximumConcurrent < 1 || options.MaximumConcurrent > 8 {
		return errors.New("HTTP MCP concurrency must be between 1 and 8")
	}
	return nil
}

// ListenHTTP opens a bounded local listener for one already-verified atlas.
// Workspace adapters are refused because they can change generations on reads.
func (s *Server) ListenHTTP(options HTTPOptions) (*HTTPServer, error) {
	if err := ValidateHTTPOptions(options); err != nil {
		return nil, err
	}
	if s == nil || s.dataset == nil || s.workspace != nil || s.dataset.Manifest.ID == "" ||
		(s.dataset.Integrity != server.IntegrityVerified && s.dataset.Integrity != server.IntegrityVerifiedLegacyUnmarked) {
		return nil, errors.New("HTTP MCP requires one immutable verified atlas")
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return nil, err
	}
	authority := listener.Addr().String()
	transport := &httpTransport{authority: authority, slots: make(chan struct{}, options.MaximumConcurrent), dispatch: s.handle}
	return &HTTPServer{
		listener: listener, endpoint: "http://" + authority + "/mcp",
		server: &http.Server{
			Handler:           http.TimeoutHandler(transport, options.RequestTimeout, "MCP request deadline exceeded\n"),
			ReadHeaderTimeout: min(options.RequestTimeout, 5*time.Second),
			ReadTimeout:       options.RequestTimeout, WriteTimeout: options.RequestTimeout + time.Second,
			IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 * 1024,
		},
	}, nil
}

// Endpoint is the exact bound MCP URL, including an OS-assigned port when used.
func (s *HTTPServer) Endpoint() string { return s.endpoint }

// Serve runs until cancellation or a listener error and closes all connections.
func (s *HTTPServer) Serve(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("HTTP MCP server and context are required")
	}
	if err := ctx.Err(); err != nil {
		_ = s.listener.Close()
		return nil
	}
	s.server.BaseContext = func(net.Listener) context.Context { return ctx }
	done := make(chan error, 1)
	go func() { done <- s.server.Serve(s.listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Closing cancels requests promptly; no detached request can take new work.
		_ = s.server.Close()
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *httpTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.localRequest(r) {
		http.Error(w, "local MCP authority or origin rejected", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/mcp" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery ||
		r.URL.Fragment != "" || r.URL.IsAbs() || r.URL.Host != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "MCP uses POST JSON; SSE and sessions are not offered", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("MCP-Session-Id")) != 0 {
		http.Error(w, "MCP sessions are not offered", http.StatusBadRequest)
		return
	}
	versionHeaders := r.Header.Values("MCP-Protocol-Version")
	version := r.Header.Get("MCP-Protocol-Version")
	if len(versionHeaders) > 1 || (len(versionHeaders) == 1 && !supportedHTTPVersion(version)) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	if len(versionHeaders) == 0 {
		version = "2025-03-26"
	}
	if !acceptsMCPJSON(r.Header.Values("Accept")) {
		http.Error(w, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || len(r.Header.Values("Content-Type")) != 1 || r.Header.Get("Content-Encoding") != "" {
		http.Error(w, "MCP requires uncompressed application/json", http.StatusUnsupportedMediaType)
		return
	}
	select {
	case s.slots <- struct{}{}:
		// This permit is held by the work goroutine even after TimeoutHandler
		// replies. Synchronous search cannot manufacture unbounded timed-out work.
		defer func() { <-s.slots }()
	default:
		http.Error(w, "MCP concurrency limit reached", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximumRequestBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "MCP request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "MCP body could not be read", http.StatusBadRequest)
		}
		return
	}
	var message request
	if !utf8.Valid(data) || !json.Valid(data) {
		s.reply(w, http.StatusBadRequest, errorResponse(json.RawMessage("null"), -32700, "parse error", nil))
		return
	}
	if checkHTTPJSONDepth(data) != nil || !exactHTTPObject(data, "jsonrpc", "id", "method", "params") ||
		decodeStrict(data, &message) != nil || validateRequest(message) != nil || !validHTTPID(message.ID) ||
		(len(message.Params) != 0 && !exactHTTPObject(message.Params)) {
		s.reply(w, http.StatusBadRequest, errorResponse(json.RawMessage("null"), -32600, "invalid MCP request", nil))
		return
	}
	if strings.HasPrefix(message.Method, "notifications/") {
		if hasID(message.ID) || (message.Method == "notifications/initialized" && !validInitializedParams(message.Params)) {
			http.Error(w, "invalid MCP notification", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !hasID(message.ID) {
		http.Error(w, "MCP requests require an ID", http.StatusBadRequest)
		return
	}
	if !validHTTPParams(message.Method, message.Params) {
		s.reply(w, http.StatusOK, errorResponse(message.ID, -32602, "invalid params", nil))
		return
	}
	if message.Method == "initialize" {
		requested, err := httpInitializeVersion(message.Params)
		if err != nil {
			s.reply(w, http.StatusOK, errorResponse(message.ID, -32602, "invalid initialize params", nil))
			return
		}
		if supportedHTTPVersion(requested) {
			version = requested
		} else {
			version = ProtocolVersion
		}
	}
	result, rpcErr := s.dispatch(r.Context(), message.Method, message.Params)
	if message.Method == "initialize" && rpcErr == nil {
		result.(map[string]any)["protocolVersion"] = version
	}
	if r.Context().Err() != nil {
		return
	}
	s.reply(w, http.StatusOK, response{JSONRPC: "2.0", ID: message.ID, Result: result, Error: rpcErr})
}

func (s *httpTransport) localRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || r.Host != s.authority {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && origins[0] != "http://"+s.authority) {
		return false
	}
	for key := range r.Header {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
			return false
		}
	}
	return true
}

func (s *httpTransport) reply(w http.ResponseWriter, status int, message response) {
	var output bytes.Buffer
	if err := writeResponse(&output, message); err != nil {
		http.Error(w, "MCP response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(output.Bytes())
}

func supportedHTTPVersion(version string) bool {
	return version == "2025-03-26" || version == "2025-06-18" || version == ProtocolVersion
}

func acceptsMCPJSON(values []string) bool {
	jsonOK, sseOK := false, false
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			kind, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				return false
			}
			if q, exists := params["q"]; exists {
				quality, err := strconv.ParseFloat(q, 64)
				if err != nil || !validHTTPQuality(q) {
					return false
				}
				if quality == 0 {
					continue
				}
			}
			jsonOK = jsonOK || kind == "application/json"
			sseOK = sseOK || kind == "text/event-stream"
		}
	}
	return jsonOK && sseOK
}

// An RFC quality value is 0 or 1, with at most three decimal digits; NaN,
// exponent notation and nonzero digits after 1 are not valid media ranges.
func validHTTPQuality(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || (parts[0] != "0" && parts[0] != "1") {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	if len(parts[1]) > 3 {
		return false
	}
	for _, digit := range parts[1] {
		if digit < '0' || digit > '9' || (parts[0] == "1" && digit != '0') {
			return false
		}
	}
	return true
}

func validHTTPID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if decodeStrict(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case string:
		return len(value) <= 256
	case json.Number:
		_, err := value.Int64()
		return err == nil
	default:
		return false
	}
}

func httpInitializeVersion(raw json.RawMessage) (string, error) {
	var params map[string]json.RawMessage
	if decodeStrict(raw, &params) != nil || params == nil {
		return "", errors.New("initialize object required")
	}
	var version string
	var capabilities, client map[string]json.RawMessage
	if decodeStrict(params["protocolVersion"], &version) != nil || !printableHTTPText(version, 64) || version != strings.TrimSpace(version) ||
		decodeStrict(params["capabilities"], &capabilities) != nil || capabilities == nil ||
		decodeStrict(params["clientInfo"], &client) != nil || client == nil || validateRequestMetadata(params["_meta"]) != nil {
		return "", errors.New("initialize fields required")
	}
	for _, field := range []string{"name", "version"} {
		var text string
		if decodeStrict(client[field], &text) != nil || !printableHTTPText(text, 256) {
			return "", errors.New("client identity required")
		}
	}
	return version, nil
}

func validInitializedParams(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var params map[string]json.RawMessage
	return exactHTTPObject(raw, "_meta") && decodeStrict(raw, &params) == nil && validateRequestMetadata(params["_meta"]) == nil
}

// Map keys are case-sensitive, unlike encoding/json's struct field matching.
// With no allowlist, this checks only the object and duplicate-key contract.
func exactHTTPObject(raw json.RawMessage, allowed ...string) bool {
	var object map[string]json.RawMessage
	if decodeStrict(raw, &object) != nil || object == nil {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	for key := range object {
		found := false
		for _, field := range allowed {
			found = found || key == field
		}
		if !found {
			return false
		}
	}
	return true
}

func validHTTPParams(method string, raw json.RawMessage) bool {
	if len(raw) == 0 {
		return method != "initialize" && method != "tools/call" && method != "resources/read"
	}
	switch method {
	case "initialize":
		return exactHTTPObject(raw, "protocolVersion", "capabilities", "clientInfo", "_meta")
	case "tools/call":
		return exactHTTPObject(raw, "name", "arguments", "_meta")
	case "resources/read":
		return exactHTTPObject(raw, "uri", "_meta")
	case "ping", "tools/list", "resources/list":
		if !exactHTTPObject(raw, "_meta") {
			return false
		}
		var object map[string]json.RawMessage
		return decodeStrict(raw, &object) == nil && validateRequestMetadata(object["_meta"]) == nil
	default:
		return true
	}
}

func printableHTTPText(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum && strings.IndexFunc(value, func(character rune) bool { return !unicode.IsPrint(character) }) == -1
}

func checkHTTPJSONDepth(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			if delimiter == '{' || delimiter == '[' {
				depth++
			} else {
				depth--
			}
			if depth > 64 {
				return errors.New("MCP JSON nesting exceeds 64")
			}
		}
	}
}
