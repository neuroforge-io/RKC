package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
)

const maximumPageCursorBytes = 4096

var pageGeneration atomic.Uint64
var pageSigningKey = sync.OnceValues(func() ([32]byte, error) {
	var key [32]byte
	_, err := rand.Read(key[:])
	return key, err
})

// A generation belongs to one immutable loaded dataset. Snapshot IDs alone are
// insufficient: an imported dataset can reuse an ID with different contents.
// Keep synchronization behind a pointer so trusted browser copies remain safe.
type paginationState struct {
	generation uint64
	searchIDs  func() []string
}

func newPaginationState(index *search.Index) *paginationState {
	return &paginationState{
		generation: pageGeneration.Add(1),
		searchIDs: sync.OnceValue(func() []string {
			if index == nil {
				return nil
			}
			ids := make([]string, 0, len(index.Documents))
			for id := range index.Documents {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return ids
		}),
	}
}

type pageCursor struct {
	Version    int     `json:"v"`
	Generation uint64  `json:"g"`
	Scope      string  `json:"s"`
	Mode       string  `json:"m"`
	Position   int     `json:"p"`
	Total      int     `json:"t"`
	Score      float64 `json:"r,omitempty"`
}

type pageRequest struct {
	values url.Values
	scope  string
	cursor *pageCursor
}

var errPageUnavailable = errors.New("pagination is unavailable")

var errPageCursor = errors.New("cursor is invalid for this dataset and query; restart from the first page")

// Query spelling is retained (including lexical term order). Changing a limit
// is safe; changing any other parameter requires starting a new result set.
func (dataset *Dataset) parsePageRequest(request *http.Request, mode string, allowed ...string) (pageRequest, error) {
	parsed := pageRequest{}
	if len(request.URL.RawQuery) > 32768 {
		return parsed, errors.New("query string is too large")
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return parsed, errors.New("query string is malformed")
	}
	permitted := map[string]bool{"cursor": true, "limit": true}
	for _, key := range allowed {
		permitted[key] = true
	}
	for key, entries := range values {
		if !permitted[key] || len(entries) != 1 {
			return parsed, errors.New("use only supported query parameters, each once")
		}
		if len(entries[0]) > 4096 || !utf8.ValidString(entries[0]) {
			return parsed, errors.New("query parameter exceeds 4096 UTF-8 bytes or is invalid UTF-8")
		}
	}
	cursor := values.Get("cursor")
	scopeValues := make(url.Values, len(values))
	for key, entries := range values {
		scopeValues[key] = entries
	}
	scopeValues.Del("cursor")
	scopeValues.Del("limit")
	scopeBytes := sha256.Sum256([]byte(dataset.Manifest.ID + "\x00" + request.URL.Path + "\x00" + scopeValues.Encode()))
	parsed.values, parsed.scope = values, hex.EncodeToString(scopeBytes[:])
	if cursor == "" {
		return parsed, nil
	}
	if dataset.pagination == nil {
		return parsed, errPageCursor
	}
	decoded, err := decodePageCursor(cursor)
	if err != nil {
		return parsed, err
	}
	if decoded.Generation != dataset.pagination.generation || decoded.Scope != parsed.scope || decoded.Mode != mode || decoded.Version != 1 || decoded.Position < 0 || decoded.Total < 1 || math.IsNaN(decoded.Score) || math.IsInf(decoded.Score, 0) {
		return parsed, errPageCursor
	}
	parsed.cursor = &decoded
	return parsed, nil
}

func decodePageCursor(value string) (pageCursor, error) {
	var decoded pageCursor
	if len(value) > maximumPageCursorBytes {
		return decoded, errPageCursor
	}
	payload, signature, ok := strings.Cut(value, ".")
	if !ok {
		return decoded, errPageCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != payload {
		return decoded, errPageCursor
	}
	supplied, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(supplied) != sha256.Size || base64.RawURLEncoding.EncodeToString(supplied) != signature {
		return decoded, errPageCursor
	}
	key, err := pageSigningKey()
	if err != nil {
		return decoded, errors.Join(errPageUnavailable, err)
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return decoded, errPageCursor
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return decoded, errPageCursor
	}
	return decoded, nil
}

func (dataset *Dataset) nextPageCursor(parsed pageRequest, mode string, position, total int, score float64) (string, error) {
	if dataset.pagination == nil {
		return "", errPageUnavailable
	}
	key, err := pageSigningKey()
	if err != nil {
		return "", errors.Join(errPageUnavailable, err)
	}
	raw, err := json.Marshal(pageCursor{Version: 1, Generation: dataset.pagination.generation, Scope: parsed.scope, Mode: mode, Position: position, Total: total, Score: score})
	if err != nil {
		return "", errors.Join(errPageUnavailable, err)
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// First pages count all matches without copying a filtered corpus. Subsequent
// pages resume at an authenticated canonical offset and stop after lookahead.
func collectionPage[T any](dataset *Dataset, request *http.Request, parsed pageRequest, records []T, limit int, matches func(T) bool) (rkcapi.CollectionPage[T], error) {
	page := rkcapi.CollectionPage[T]{Items: make([]T, 0, min(limit, len(records))), SnapshotID: dataset.Manifest.ID}
	start, next := 0, -1
	if parsed.cursor != nil {
		start, page.Total = parsed.cursor.Position, parsed.cursor.Total
		if start >= len(records) || page.Total > len(records) {
			return page, errPageCursor
		}
	}
	for i := start; i < len(records); i++ {
		if i%256 == 0 {
			if err := request.Context().Err(); err != nil {
				return page, err
			}
		}
		if !matches(records[i]) {
			continue
		}
		if parsed.cursor == nil {
			page.Total++
		}
		if len(page.Items) < limit {
			page.Items = append(page.Items, records[i])
			continue
		}
		if next < 0 {
			next = i
		}
		if parsed.cursor != nil {
			break
		}
	}
	page.Truncated = next >= 0
	if page.Truncated {
		var err error
		page.NextCursor, err = dataset.nextPageCursor(parsed, "collection", next, page.Total, 0)
		if err != nil {
			return page, err
		}
	}
	return page, request.Context().Err()
}

type searchPageResponse struct {
	search.LexicalPage
	SnapshotID string `json:"snapshot_id"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func (dataset *Dataset) rankedPage(request *http.Request, parsed pageRequest, query search.Query) (searchPageResponse, error) {
	page := searchPageResponse{SnapshotID: dataset.Manifest.ID}
	if err := request.Context().Err(); err != nil {
		return page, err
	}
	var after *search.Position
	if parsed.cursor != nil {
		ids := dataset.pagination.searchIDs()
		if parsed.cursor.Position >= len(ids) || parsed.cursor.Total > len(ids) {
			return page, errPageCursor
		}
		document := dataset.Search.Documents[ids[parsed.cursor.Position]]
		after = &search.Position{Score: parsed.cursor.Score, Key: document.QualifiedName + "\x00" + document.ID, ID: document.ID}
	}
	page.LexicalPage = dataset.Search.SearchPage(query, after)
	if parsed.cursor != nil && page.Total != parsed.cursor.Total {
		return page, errPageCursor
	}
	if page.Truncated && len(page.Hits) > 0 {
		last := page.Hits[len(page.Hits)-1]
		ids := dataset.pagination.searchIDs()
		position := sort.SearchStrings(ids, last.Document.ID)
		var err error
		page.NextCursor, err = dataset.nextPageCursor(parsed, "ranked", position, page.Total, last.Score)
		if err != nil {
			return page, err
		}
	}
	return page, request.Context().Err()
}

func writePageError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPageUnavailable) {
		writeProblem(w, http.StatusInternalServerError, "Cannot read page", errPageUnavailable.Error())
		return
	}
	writeProblem(w, http.StatusBadRequest, "Cannot read page", err.Error())
}
