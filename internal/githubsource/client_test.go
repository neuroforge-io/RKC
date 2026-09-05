package githubsource

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}
}

func testRepository() Repository {
	return Repository{FullName: "example/source", HTMLURL: "https://github.com/example/source", DefaultBranch: "release/current", Description: "public fixture"}
}

type zipEntry struct {
	name, body string
	mode       os.FileMode
}

func zipFixture(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(file, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func fixtureClient(t *testing.T, archive []byte) *Client {
	t.Helper()
	client, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	client.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			t.Fatalf("unexpected destination %s", request.URL.Host)
		}
		switch request.URL.EscapedPath() {
		case "/repos/example/source":
			return jsonResponse(testRepository()), nil
		case "/repos/example/source/commits/release%2Fcurrent":
			return jsonResponse(map[string]string{"sha": strings.Repeat("a", 40)}), nil
		case "/repos/example/source/zipball/" + strings.Repeat("a", 40):
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(archive))}, nil
		default:
			t.Fatalf("unexpected API endpoint %s", request.URL.EscapedPath())
			return nil, errors.New("unexpected endpoint")
		}
	})
	return client
}

func TestMaterializePinsCommitAndVerifiesArchive(t *testing.T) {
	archive := zipFixture(t, zipEntry{"repo-a/", "", os.ModeDir | 0o755}, zipEntry{"repo-a/src/main.go", "package main\n", 0o755}, zipEntry{"repo-a/src/", "", os.ModeDir | 0o755}, zipEntry{"repo-a/README with spaces.md", "Example source", 0o644})
	client := fixtureClient(t, archive)
	destination := privateTempDir(t)
	checkout, err := client.Materialize(context.Background(), "example/source", destination)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	if checkout.CommitSHA != strings.Repeat("a", 40) || checkout.ArchiveSHA256 != hex.EncodeToString(digest[:]) || checkout.ArchiveBytes != int64(len(archive)) || checkout.Repository != testRepository() {
		t.Fatalf("incorrect source receipt: %+v", checkout)
	}
	data, err := os.ReadFile(filepath.Join(checkout.Root, "src/main.go"))
	if err != nil || string(data) != "package main\n" {
		t.Fatalf("source contents: %q, %v", data, err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatalf("staging leaked: %v, %v", entries, err)
	}
	if _, err := client.Materialize(context.Background(), "example/source", destination); err == nil {
		t.Fatal("overwrote an existing checkout")
	}
	if data, err = os.ReadFile(filepath.Join(checkout.Root, "src/main.go")); err != nil || string(data) != "package main\n" {
		t.Fatal("existing source changed")
	}
}

func TestArchiveRejectsUnsafePathsAndTypesWithoutPublishing(t *testing.T) {
	cases := map[string][]zipEntry{
		"parent":                   {{"repo/../escape", "bad", 0}},
		"absolute":                 {{"/repo/escape", "bad", 0}},
		"backslash":                {{`repo/a\b`, "bad", 0}},
		"drive":                    {{"repo/C:drive", "bad", 0}},
		"device":                   {{"repo/CON.txt", "bad", 0}},
		"device superscript":       {{"repo/COM¹.txt", "bad", 0}},
		"trailing dot":             {{"repo/name.", "bad", 0}},
		"trailing space":           {{"repo/name ", "bad", 0}},
		"control":                  {{"repo/name\n", "bad", 0}},
		"git metadata":             {{"repo/.GiT/config", "bad", 0}},
		"symlink":                  {{"repo/link", "../../escape", os.ModeSymlink | 0o777}},
		"fifo":                     {{"repo/pipe", "", os.ModeNamedPipe | 0o600}},
		"duplicate":                {{"repo/a", "one", 0}, {"repo/a", "two", 0}},
		"case collision":           {{"repo/Readme", "one", 0}, {"repo/README", "two", 0}},
		"unicode case collision":   {{"repo/K", "one", 0}, {"repo/K", "two", 0}},
		"parent case collision":    {{"repo/Src/a", "one", 0}, {"repo/src/b", "two", 0}},
		"file directory collision": {{"repo/src", "one", 0}, {"repo/src/b", "two", 0}},
		"multiple roots":           {{"repo/a", "one", 0}, {"other/b", "two", 0}},
		"root file":                {{"README", "one", 0}},
		"no files":                 {{"repo/", "", os.ModeDir | 0o755}},
		"component too long":       {{"repo/" + strings.Repeat("a", 256), "bad", 0}},
		"too deep":                 {{"repo/" + strings.Repeat("a/", 128) + "b", "bad", 0}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			client := fixtureClient(t, zipFixture(t, entries...))
			destination := privateTempDir(t)
			if checkout, err := client.Materialize(context.Background(), "example/source", destination); err == nil || checkout.Root != "" {
				t.Fatalf("unsafe archive admitted: %+v %v", checkout, err)
			}
			assertEmpty(t, destination)
		})
	}
}

func TestArchiveBytePathAndCRCGuards(t *testing.T) {
	archive := zipFixture(t, zipEntry{"repo/a/b.txt", "payload content", 0}, zipEntry{"repo/c.txt", "another body", 0})
	for _, name := range []string{"compressed", "expanded", "file", "paths", "crc", "invalid zip"} {
		t.Run(name, func(t *testing.T) {
			fixture := append([]byte(nil), archive...)
			if name == "crc" {
				fixture[bytes.Index(fixture, []byte("payload content"))] ^= 1
			}
			if name == "invalid zip" {
				fixture = []byte("not a zip file")
			}
			client := fixtureClient(t, fixture)
			switch name {
			case "compressed":
				client.limits.compressed = int64(len(fixture) - 1)
			case "expanded":
				client.limits.expanded = 20
			case "file":
				client.limits.file = 5
			case "paths":
				client.limits.paths = 2
			}
			destination := privateTempDir(t)
			if _, err := client.Materialize(context.Background(), "example/source", destination); err == nil {
				t.Fatal("limit or integrity failure accepted")
			}
			assertEmpty(t, destination)
		})
	}
}

func TestSearchPaginationAndExplicitAuthentication(t *testing.T) {
	client, _ := New("test-token")
	client.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("X-GitHub-Api-Version") == "" {
			t.Fatal("missing explicit account authorization")
		}
		if request.URL.Path == "/user" {
			return jsonResponse(User{"example", "https://github.com/example"}), nil
		}
		if request.URL.Path != "/search/repositories" || request.URL.Query().Get("q") != "org:example is:private" || request.URL.Query().Get("per_page") != "25" {
			t.Fatal("incorrect search contract")
		}
		count := 25
		if request.URL.Query().Get("page") == "2" {
			count = 5
		}
		items := make([]Repository, count)
		for i := range items {
			items[i] = Repository{FullName: fmt.Sprintf("example/repo%d", i), HTMLURL: fmt.Sprintf("https://github.com/example/repo%d", i), DefaultBranch: "main", Private: true}
		}
		return jsonResponse(map[string]any{"items": items, "total_count": 30, "incomplete_results": false}), nil
	})
	if user, err := client.User(context.Background()); err != nil || user.Login != "example" {
		t.Fatalf("connected user: %+v %v", user, err)
	}
	for page := 1; page <= 2; page++ {
		result, err := client.Search(context.Background(), "org:example is:private", page)
		if err != nil || result.Total != 30 || result.Incomplete || (page == 1 && result.NextPage != 2) || (page == 2 && result.NextPage != 0) {
			t.Fatalf("page %d: %+v %v", page, result, err)
		}
	}
	for _, query := range []string{"", "x\nsecret", strings.Repeat("a", 513)} {
		if _, err := client.Search(context.Background(), query, 1); err == nil {
			t.Fatal("unbounded search accepted")
		}
	}
	for _, page := range []int{-1, 0, 41} {
		if _, err := client.Search(context.Background(), "x", page); err == nil {
			t.Fatal("unbounded page accepted")
		}
	}
}

func TestCredentialRedirectPolicyAndSanitizedErrors(t *testing.T) {
	for _, destination := range []string{"https://codeload.github.com/example/source/zip/sha?private=archive-secret", "https://evil.example/archive?private=archive-secret", "http://api.github.com/archive", "https://api.github.com.evil.example/archive", "https://api.github.com:444/archive", "https://user:pass@api.github.com/archive"} {
		t.Run(destination, func(t *testing.T) {
			client, _ := New("test-token")
			calls := 0
			client.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if request.Header.Get("Authorization") != "Bearer test-token" {
						t.Fatal("missing API credential")
					}
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {destination}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if request.URL.Host != "codeload.github.com" || request.Header.Get("Authorization") != "" || request.Header.Get("Referer") != "" || request.Header.Get("Cookie") != "" {
					t.Fatal("credential or URL escaped redirect boundary")
				}
				return nil, errors.New("transport failure exposing test-token archive-secret")
			})
			_, err := client.get(context.Background(), "https://api.github.com/archive")
			if err == nil || strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "archive-secret") || strings.Contains(err.Error(), destination) {
				t.Fatalf("unsanitized redirect failure: %v", err)
			}
			wantCalls := 1
			if strings.HasPrefix(destination, "https://codeload.github.com/") {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatal("request followed an untrusted redirect")
			}
		})
	}
}

func TestCancellationAndDestinationChangeCleanStaging(t *testing.T) {
	archive := zipFixture(t, zipEntry{"repo/a", "hello", 0})
	for _, action := range []string{"cancel", "change destination", "bad revision"} {
		t.Run(action, func(t *testing.T) {
			client := fixtureClient(t, archive)
			base := client.transport
			destination := privateTempDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Path, "/zipball/") {
					if action == "cancel" {
						cancel()
					} else if action == "change destination" {
						if err := os.WriteFile(filepath.Join(destination, "keep.txt"), []byte("user data"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if action == "bad revision" && strings.Contains(request.URL.Path, "/commits/") {
					return jsonResponse(map[string]string{"sha": "main"}), nil
				}
				return base.RoundTrip(request)
			})
			if result, err := client.Materialize(ctx, "example/source", destination); err == nil || result.Root != "" {
				t.Fatalf("failed operation published: %+v %v", result, err)
			}
			if action == "change destination" {
				data, err := os.ReadFile(filepath.Join(destination, "keep.txt"))
				if err != nil || string(data) != "user data" {
					t.Fatal("caller data changed")
				}
				os.Remove(filepath.Join(destination, "keep.txt"))
			}
			assertEmpty(t, destination)
		})
	}
}

func TestInputAndMetadataFailClosed(t *testing.T) {
	connected, _ := New("CREDENTIAL_SENTINEL")
	for _, formatted := range []string{fmt.Sprint(connected), fmt.Sprintf("%+v", connected), fmt.Sprintf("%#v", connected), fmt.Sprintf("%+v", *connected), fmt.Sprintf("%#v", *connected)} {
		if strings.Contains(formatted, "CREDENTIAL_SENTINEL") {
			t.Fatal("client formatting disclosed its credential")
		}
	}
	for _, token := range []string{"token\n", "token secret", "token\t", strings.Repeat("a", 4097)} {
		if _, err := New(token); err == nil {
			t.Fatal("invalid token admitted")
		}
	}
	for _, name := range []string{"owner", "../repo", "owner/../repo", "https://github.com/owner/repo", "owner/repo?token=secret"} {
		if ValidRepositoryName(name) {
			t.Fatal("invalid repository admitted")
		}
	}
	client, _ := New("")
	if _, err := client.User(context.Background()); err == nil {
		t.Fatal("anonymous client attempted account discovery")
	}
	for _, body := range []string{`{"full_name":"example/source","html_url":"https://evil.example/","default_branch":"main"}`, `not json`, strings.Repeat(" ", (4<<20)+1)} {
		client.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "" {
				t.Fatal("anonymous client used ambient credentials")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := client.Repository(context.Background(), "example/source"); err == nil {
			t.Fatal("invalid response admitted")
		}
	}
}

func TestZIPDirectoryUnderstatementCannotBypassAllocationLimit(t *testing.T) {
	archive := zipFixture(t, zipEntry{"repo/a", "one", 0}, zipEntry{"repo/b", "two", 0})
	end := len(archive) - 22
	binary.LittleEndian.PutUint16(archive[end+8:end+10], 1)
	binary.LittleEndian.PutUint16(archive[end+10:end+12], 1)
	client := fixtureClient(t, archive)
	client.limits.paths = 1
	destination := privateTempDir(t)
	if _, err := client.Materialize(context.Background(), "example/source", destination); err == nil {
		t.Fatal("understated ZIP record count admitted")
	}
	assertEmpty(t, destination)
}

func TestPublicationNeverClobbersExistingDestination(t *testing.T) {
	directory := privateTempDir(t)
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("stage", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("source", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := publishNoReplace(root, "stage", "source"); err == nil {
		t.Fatal("concurrent destination was replaced")
	}
	if _, err := root.Stat("stage"); err != nil {
		t.Fatal("staging was lost")
	}
}

func TestDestinationPermissionsAndSymlinkFailBeforeNetwork(t *testing.T) {
	client, _ := New("")
	client.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe destination made a network request")
		return nil, errors.New("unexpected")
	})
	directory := privateTempDir(t)
	if err := os.WriteFile(filepath.Join(directory, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Materialize(context.Background(), "example/source", directory); err == nil {
		t.Fatal("nonempty destination accepted")
	}
	link := filepath.Join(privateTempDir(t), "link")
	if err := os.Symlink(directory, link); err == nil {
		if _, err := client.Materialize(context.Background(), "example/source", link); err == nil {
			t.Fatal("symlink destination accepted")
		}
	}
}

func assertEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed acquisition leaked data: %v %v", entries, err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path, err := privatepath.MkdirTemp("", "rkc-github-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(path) })
	return path
}
