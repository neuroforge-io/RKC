package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixtureSource(t *testing.T) Source {
	t.Helper()
	return Source{ID: "sample", Label: "Sample source", Kind: "local", LocalPath: workspaceTempDir(t), Limits: DefaultLimits(), Excludes: []string{".git"}, Freshness: Freshness{Status: "pending"}}
}

func fixtureStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(workspaceTempDir(t), "workspace", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func fixtureActive(store *Store, suffix string) *Active {
	generation := "sample-" + strings.Repeat(suffix, 32)
	return &Active{AtlasPath: filepath.Join(store.root, "generations", generation, "atlas"), SnapshotID: "snapshot-" + suffix, Generation: generation, ManifestSHA256: strings.Repeat(suffix, 64), Fingerprint: strings.Repeat(suffix, 64), CompilerVersion: "test"}
}

func TestWorkspacePublicationAndLastGoodRecovery(t *testing.T) {
	store := fixtureStore(t)
	source := fixtureSource(t)
	if err := store.Add(source); err != nil {
		t.Fatal(err)
	}
	first := fixtureActive(store, "a")
	if err := store.Refresh(t.Context(), source.ID, func(_ context.Context, prior Source, generations string) (*Active, error) {
		observed, err := Load(store.Path)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Sources[0].Freshness.Status != "stale" || prior.Active != nil || generations != filepath.Join(store.root, "generations") {
			t.Fatal("refresh did not publish in-progress state without an active pointer")
		}
		return first, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Refresh(t.Context(), source.ID, func(context.Context, Source, string) (*Active, error) {
		return nil, &RefreshError{Code: "quality_failed", Cause: errors.New("private source path sentinel")}
	}); err == nil {
		t.Fatal("failure accepted")
	}
	registry, err := Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registry.Sources[0].Active, first) || registry.Sources[0].Freshness.Status != "error" || registry.Sources[0].Freshness.ErrorCode != "quality_failed" {
		t.Fatal("failed scan replaced last good generation")
	}
	data, _ := os.ReadFile(store.Path)
	if strings.Contains(string(data), "sentinel") {
		t.Fatal("private error persisted")
	}
	second := fixtureActive(store, "b")
	if err := store.Refresh(t.Context(), source.ID, func(context.Context, Source, string) (*Active, error) { return second, nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Refresh(t.Context(), source.ID, func(context.Context, Source, string) (*Active, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	registry, err = Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registry.Sources[0].Previous, first) || !reflect.DeepEqual(registry.Sources[0].Active, second) || registry.Sources[0].Freshness.Status != "current" || registry.Sources[0].Freshness.CheckedAt.IsZero() || registry.Sources[0].Freshness.UpdatedAt.IsZero() {
		t.Fatal("successful refresh or no-op lost generation history")
	}
}

func TestWorkspaceWriterLeaseAndClosedStore(t *testing.T) {
	store := fixtureStore(t)
	if other, err := Open(store.Path); err == nil {
		other.Close()
		t.Fatal("concurrent writer admitted")
	}
	if _, err := Load(store.Path); err != nil {
		t.Fatal("writer blocked reader", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*Store)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(fixtureSource(t)); err == nil {
		t.Fatal("closed writer mutated registry")
	}
	reopened, err := Open(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Registry.Sources) != 0 {
		t.Fatal("failed write persisted")
	}
}

func TestWorkspaceMalformedRegistryFailsClosed(t *testing.T) {
	store := fixtureStore(t)
	if err := store.Add(fixtureSource(t)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	valid, _ := os.ReadFile(store.Path)
	variants := map[string][]byte{
		"duplicate":  []byte(strings.Replace(string(valid), `"generation": 2`, `"generation": 2, "generation": 3`, 1)),
		"case alias": []byte(strings.Replace(string(valid), `"generation"`, `"Generation"`, 1)),
		"unknown":    []byte(strings.Replace(string(valid), `"schema_version"`, `"unexpected"`, 1)),
		"trailing":   append(append([]byte{}, valid...), []byte(`{}`)...),
		"null":       []byte(`null`),
		"oversized":  []byte(strings.Repeat(" ", maximumRegistryBytes+1)),
		"deep":       []byte(strings.Repeat("[", 18) + strings.Repeat("]", 18)),
	}
	for name, data := range variants {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(store.Path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(store.Path); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
	if err := os.WriteFile(store.Path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(store.Path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(store.Path); err == nil {
			t.Fatal("public registry accepted")
		}
	}
}

func TestWorkspaceSourceAndActiveValidation(t *testing.T) {
	store := fixtureStore(t)
	base := fixtureSource(t)
	cases := map[string]func(*Source){
		"alias": func(s *Source) { s.ID = "../escape" }, "label": func(s *Source) { s.Label = "bad\nlabel" },
		"relative local": func(s *Source) { s.LocalPath = "relative" }, "mixed kinds": func(s *Source) { s.RemoteURL = "https://example.test/repo" },
		"unknown kind": func(s *Source) { s.Kind = "command" }, "zero bound": func(s *Source) { s.Limits.MaxFiles = 0 },
		"large file bound": func(s *Source) { s.Limits.MaxFileBytes = 2 << 30 }, "text bound": func(s *Source) { s.Limits.MaxTextBytes = 9 << 20 },
		"exclude escape": func(s *Source) { s.Excludes = []string{"../outside"} }, "exclude backslash": func(s *Source) { s.Excludes = []string{`a\b`} },
		"exclude duplicate": func(s *Source) { s.Excludes = []string{"a", "a"} }, "exclude null": func(s *Source) { s.Excludes = nil },
		"unknown status": func(s *Source) { s.Freshness.Status = "freshforever" }, "unsafe error": func(s *Source) { s.Freshness.ErrorCode = "private error" },
		"no active": func(s *Source) { s.Freshness.Status = "current" },
		"pointer escape": func(s *Source) {
			s.Active = fixtureActive(store, "a")
			s.Active.AtlasPath = filepath.Join(store.root, "other")
		},
		"pointer digest": func(s *Source) { s.Active = fixtureActive(store, "a"); s.Active.ManifestSHA256 = "bad" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			source := base
			change(&source)
			registry := Registry{SchemaVersion: SchemaVersion, Generation: 1, Sources: []Source{source}}
			if err := validate(registry, store.root); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	for _, raw := range []string{"https://user:secret@example.test/repo", "https://example.test/repo?token=secret", "file:///tmp/repo", "http://example.test/repo", "ssh://other@example.test/repo"} {
		if validateRemote(raw, "") == nil {
			t.Fatal("unsafe remote accepted", raw)
		}
	}
	for _, raw := range []string{"https://example.test/repo.git", "ssh://git@example.test/owner/repo.git"} {
		if err := validateRemote(raw, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if validateRemote("https://example.test/repo", "--upload-pack=bad") == nil {
		t.Fatal("flag ref accepted")
	}
	if err := store.Add(base); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(base); err == nil {
		t.Fatal("duplicate alias accepted")
	}
	base.ID = "second"
	base.Active = fixtureActive(store, "a")
	if err := store.Add(base); err == nil {
		t.Fatal("adopted active pointer")
	}
}

func TestWorkspaceCancellationAndInvalidProducer(t *testing.T) {
	store := fixtureStore(t)
	if err := store.Add(fixtureSource(t)); err != nil {
		t.Fatal(err)
	}
	called := false
	producer := func(context.Context, Source, string) (*Active, error) {
		called = true
		return fixtureActive(store, "a"), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Refresh(ctx, "sample", producer); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled work ran", err)
	}
	if err := store.Refresh(nil, "sample", producer); err == nil {
		t.Fatal("nil context")
	}
	if err := store.Refresh(t.Context(), "sample", nil); err == nil {
		t.Fatal("nil producer")
	}
	if err := store.Refresh(t.Context(), "missing", producer); err == nil {
		t.Fatal("unknown source")
	}
	ctx, cancel = context.WithCancel(t.Context())
	if err := store.Refresh(ctx, "sample", func(context.Context, Source, string) (*Active, error) {
		cancel()
		return fixtureActive(store, "a"), nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	registry, err := Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Sources[0].Active != nil || registry.Sources[0].Freshness.ErrorCode != "canceled" {
		t.Fatal("canceled producer published")
	}
	if err := store.Refresh(t.Context(), "sample", func(context.Context, Source, string) (*Active, error) { return nil, nil }); err == nil {
		t.Fatal("missing initial generation accepted")
	}
}

func TestWorkspaceRefusesUnownedAndSymlinkPaths(t *testing.T) {
	parent := workspaceTempDir(t)
	unowned := filepath.Join(parent, "unowned")
	if err := os.Mkdir(unowned, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(unowned, "registry.json")); err == nil {
		t.Fatal("adopted existing directory")
	}
	if _, err := Open(filepath.Join(parent, "wrong.json")); err == nil {
		t.Fatal("wrong registry name")
	}
	store := fixtureStore(t)
	store.Close()
	link := filepath.Join(parent, "alias")
	if err := os.Symlink(store.root, link); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if _, err := Load(filepath.Join(link, "registry.json")); err == nil {
		t.Fatal("symlink root accepted")
	}
	if _, err := Open(filepath.Join(link, "registry.json")); err == nil {
		t.Fatal("symlink writer accepted")
	}
}

func TestWorkspaceLimitsAndSchemaValidation(t *testing.T) {
	store := fixtureStore(t)
	source := fixtureSource(t)
	registry := Registry{SchemaVersion: SchemaVersion, Generation: 1, Sources: []Source{source}}
	for _, change := range []func(*Registry){func(r *Registry) { r.SchemaVersion = "future" }, func(r *Registry) { r.Generation = 0 }, func(r *Registry) { r.Sources = nil }, func(r *Registry) { r.Sources = append(r.Sources, source) }, func(r *Registry) { r.Sources = make([]Source, MaximumSources+1) }} {
		copy := registry
		change(&copy)
		if validate(copy, store.root) == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	data, _ := json.Marshal(registry)
	var output Registry
	if err := strictJSON(data, &output); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"{", `{"sources":[}`, `{"generation":true}`, `{"generation":1,"sources":[{"id":"a","id":"b"}]}`} {
		if strictJSON([]byte(data), &output) == nil {
			t.Fatal("malformed JSON accepted")
		}
	}
}

func workspaceTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
