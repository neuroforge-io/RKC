// Package githubsource acquires source snapshots from GitHub without executing
// Git or repository code. Credentials are supplied explicitly and held in memory.
package githubsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const pageSize = 25

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var commitID = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

type User struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
}

type Repository struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

type SearchResult struct {
	Items      []Repository `json:"items"`
	Total      int          `json:"total"`
	Incomplete bool         `json:"incomplete"`
	NextPage   int          `json:"next_page"`
}

type Checkout struct {
	Root          string     `json:"root"`
	Repository    Repository `json:"repository"`
	CommitSHA     string     `json:"commit_sha"`
	ArchiveSHA256 string     `json:"archive_sha256"`
	ArchiveBytes  int64      `json:"archive_bytes"`
}

type limits struct {
	compressed, expanded, file int64
	paths                      int
}

// Client is safe for concurrent requests after construction. It discovers no
// ambient credentials, writes no credential files, and emits no request logs.
type Client struct {
	token     string
	transport http.RoundTripper
	limits    limits
}

// Formatting a client must never disclose its in-memory credential.
func (Client) String() string   { return "GitHub source client" }
func (Client) GoString() string { return "githubsource.Client{credential:redacted}" }

func New(token string) (*Client, error) {
	if len(token) > 4096 {
		return nil, errors.New("GitHub token is too long")
	}
	for _, r := range token {
		if r < 33 || r > 126 {
			return nil, errors.New("GitHub token must contain printable ASCII without spaces")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxResponseHeaderBytes = 64 * 1024
	return &Client{token: token, transport: transport, limits: limits{128 << 20, 512 << 20, 32 << 20, 50_000}}, nil
}

func (client *Client) User(ctx context.Context) (User, error) {
	if client.token == "" {
		return User{}, errors.New("connect a GitHub account first")
	}
	var user User
	if err := client.getJSON(ctx, "/user", &user); err != nil {
		return User{}, err
	}
	if !validPart(user.Login) || user.HTMLURL != "https://github.com/"+user.Login {
		return User{}, errors.New("GitHub returned an invalid account identity")
	}
	return user, nil
}

func (client *Client) Search(ctx context.Context, query string, page int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 512 || !utf8.ValidString(query) || strings.ContainsAny(query, "\r\n\x00") {
		return SearchResult{}, errors.New("enter a GitHub repository search of 1 to 512 bytes")
	}
	if page < 1 || page > 40 {
		return SearchResult{}, errors.New("GitHub search page must be between 1 and 40")
	}
	var wire struct {
		Items      []Repository `json:"items"`
		Total      int          `json:"total_count"`
		Incomplete bool         `json:"incomplete_results"`
	}
	values := url.Values{"q": {query}, "per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
	if err := client.getJSON(ctx, "/search/repositories?"+values.Encode(), &wire); err != nil {
		return SearchResult{}, err
	}
	if wire.Total < len(wire.Items) || len(wire.Items) > pageSize {
		return SearchResult{}, errors.New("GitHub returned invalid search pagination")
	}
	seen := map[string]bool{}
	for _, item := range wire.Items {
		key := strings.ToLower(item.FullName)
		if err := validateRepository(item); err != nil || seen[key] {
			return SearchResult{}, errors.New("GitHub returned an invalid repository search result")
		}
		seen[key] = true
	}
	result := SearchResult{Items: wire.Items, Total: wire.Total, Incomplete: wire.Incomplete || wire.Total > 1000}
	if result.Items == nil {
		result.Items = []Repository{}
	}
	if len(result.Items) == pageSize && page < 40 && page*pageSize < result.Total {
		result.NextPage = page + 1
	}
	return result, nil
}

func (client *Client) Repository(ctx context.Context, fullName string) (Repository, error) {
	if !validFullName(fullName) {
		return Repository{}, errors.New("choose a repository in owner/name form")
	}
	var repository Repository
	if err := client.getJSON(ctx, "/repos/"+fullName, &repository); err != nil {
		return Repository{}, err
	}
	if err := validateRepository(repository); err != nil {
		return Repository{}, err
	}
	if !strings.EqualFold(repository.FullName, fullName) {
		return Repository{}, errors.New("repository identity changed; choose its current GitHub name")
	}
	return repository, nil
}

func validPart(part string) bool {
	return repositoryPart.MatchString(part) && part != "." && part != ".."
}

func validFullName(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 2 && validPart(parts[0]) && validPart(parts[1])
}

// ValidRepositoryName accepts a bounded GitHub owner/name, never a URL or ref.
func ValidRepositoryName(name string) bool { return validFullName(name) }

func validateRepository(repository Repository) error {
	if !validFullName(repository.FullName) || repository.HTMLURL != "https://github.com/"+repository.FullName ||
		len(repository.Description) > 16*1024 || !utf8.ValidString(repository.Description) ||
		len(repository.DefaultBranch) > 1024 || !utf8.ValidString(repository.DefaultBranch) || strings.ContainsAny(repository.DefaultBranch, "\r\n\x00") {
		return errors.New("GitHub returned invalid repository metadata")
	}
	return nil
}

func (client *Client) getJSON(ctx context.Context, path string, destination any) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	response, err := client.get(ctx, "https://api.github.com"+path)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return requestError(ctx, "read GitHub response")
	}
	if len(data) > 4<<20 {
		return errors.New("GitHub metadata exceeds the response limit")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return errors.New("GitHub returned invalid JSON")
	}
	return nil
}

func allowedURL(address *url.URL) bool {
	return address != nil && address.Scheme == "https" && address.User == nil && address.Fragment == "" &&
		(address.Host == "api.github.com" || address.Host == "codeload.github.com")
}

func (client *Client) get(ctx context.Context, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !allowedURL(request.URL) {
		return nil, errors.New("GitHub request destination is not permitted")
	}
	client.headers(request)
	httpClient := &http.Client{Transport: client.transport, Timeout: 5 * time.Minute, CheckRedirect: func(next *http.Request, previous []*http.Request) error {
		if len(previous) >= 4 || !allowedURL(next.URL) {
			return errors.New("GitHub redirect is not permitted")
		}
		client.headers(next)
		return nil
	}}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, requestError(ctx, "GitHub request failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return nil, errors.New("GitHub authentication failed; reconnect with a valid token")
		case http.StatusForbidden, http.StatusTooManyRequests:
			return nil, errors.New("GitHub denied this request or its rate limit was reached")
		case http.StatusNotFound:
			return nil, errors.New("GitHub repository or revision was not found or is not accessible")
		default:
			return nil, fmt.Errorf("GitHub request returned HTTP %d", response.StatusCode)
		}
	}
	return response, nil
}

func (client *Client) headers(request *http.Request) {
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
	request.Header.Del("Referer")
	request.Header.Set("User-Agent", "RKC-GitHub-Source")
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if request.URL.Host == "api.github.com" && client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
}

func requestError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Transport errors can contain redirect URLs or private archive query tokens.
	return errors.New(message)
}
