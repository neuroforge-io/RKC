package rkcstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type cancelAfterContext struct {
	mu        sync.Mutex
	remaining int
}

func (ctx *cancelAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelAfterContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelAfterContext) Value(any) any               { return nil }
func (ctx *cancelAfterContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.remaining > 0 {
		ctx.remaining--
		return nil
	}
	return context.Canceled
}

type uncloneable int

func (uncloneable) MarshalJSON() ([]byte, error) { return []byte(`1`), nil }
func (*uncloneable) UnmarshalJSON([]byte) error  { return errors.New("fixture decode failure") }

func signedTestCursor(store *MemoryStore, payload string) Cursor {
	mac := hmac.New(sha256.New, store.secret[:])
	_, _ = mac.Write([]byte(payload))
	return Cursor(base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func TestTypedErrorsAndCursorFailurePaths(t *testing.T) {
	store := newConformanceStore(t)
	if scopeFingerprint("a", "\x00b") == scopeFingerprint("a\x00", "b") ||
		scopeFingerprint("a") == scopeFingerprint("a", "") {
		t.Fatal("cursor scope encoding is not structurally unambiguous")
	}
	var nilFailure *ValidationFailure
	if nilFailure.Error() != "<nil>" {
		t.Fatalf("nil validation failure = %q", nilFailure.Error())
	}
	failure := &ValidationFailure{Operation: "validate", BuildID: "build"}
	if !errors.Is(failure, ErrValidation) || !strings.Contains(failure.Error(), "build") {
		t.Fatalf("validation failure classification = %v", failure)
	}

	var nilOperation *OperationError
	if nilOperation.Error() != "<nil>" || nilOperation.Unwrap() != nil || nilOperation.Is(ErrConflict) {
		t.Fatal("nil operation error contract changed")
	}
	cause := errors.New("underlying")
	operation := &OperationError{Code: CodeConflict, Operation: "commit", Field: "snapshot", Err: cause}
	if !strings.Contains(operation.Error(), "commit") || !strings.Contains(operation.Error(), "snapshot") ||
		!errors.Is(operation, ErrConflict) || !errors.Is(operation, cause) {
		t.Fatalf("operation error = %v", operation)
	}
	withoutCause := &OperationError{Code: CodeBuildClosed}
	if !errors.Is(withoutCause, ErrBuildClosed) || withoutCause.Unwrap() != ErrBuildClosed {
		t.Fatalf("sentinel-only operation error = %v", withoutCause)
	}
	unknown := &OperationError{Code: "future"}
	if unknown.Unwrap() != nil || unknown.Is(ErrConflict) {
		t.Fatalf("unknown operation code was classified: %v", unknown)
	}

	scope := scopeFingerprint("snapshot")
	validPayload := `{"v":1,"k":"nodes","s":"` + scope + `","i":"node"}`
	cases := []Cursor{
		"not-separated",
		Cursor("***." + base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))),
		Cursor(base64.RawURLEncoding.EncodeToString([]byte(validPayload)) + ".***"),
		signedTestCursor(store, `{`),
		signedTestCursor(store, validPayload+`{}`),
		signedTestCursor(store, `{"v":2,"k":"nodes","s":"`+scope+`","i":"node"}`),
		signedTestCursor(store, `{"v":1,"k":"edges","s":"`+scope+`","i":"node"}`),
		signedTestCursor(store, `{"v":1,"k":"nodes","s":"other","i":"node"}`),
		signedTestCursor(store, `{"v":1,"k":"nodes","s":"`+scope+`","i":""}`),
	}
	for _, cursor := range cases {
		if _, err := store.openCursor("query", cursor, "nodes", scope); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q error = %v", cursor, err)
		}
	}
}

func TestWriterFailurePaths(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repository := RepositoryID("repo")
	bundle := conformanceBundle("snapshot", repository, "", time.Unix(1, 0).UTC())
	build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: repository})
	if err != nil {
		t.Fatal(err)
	}

	invalidWrites := []func() error{
		func() error { return store.PutArtifacts(ctx, build, []rkcmodel.Artifact{{}}) },
		func() error { return store.PutNodes(ctx, build, []rkcmodel.Node{{}}) },
		func() error { return store.PutEdges(ctx, build, []rkcmodel.Edge{{}}) },
		func() error { return store.PutEvidence(ctx, build, []rkcmodel.Evidence{{}}) },
		func() error { return store.PutDiagnostics(ctx, build, []rkcmodel.Diagnostic{{}}) },
		func() error { return store.PutConflicts(ctx, build, []rkcmodel.Conflict{{}}) },
		func() error { return store.PutDocuments(ctx, build, []rkcmodel.Document{{}}) },
		func() error { return store.PutClaims(ctx, build, []rkcmodel.Claim{{}}) },
		func() error { return store.PutPaths(ctx, build, []rkcmodel.ExecutionPath{{}}) },
	}
	for _, write := range invalidWrites {
		if err := write(); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid write error = %v", err)
		}
	}
	if err := store.PutArtifacts(ctx, "", nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty build error = %v", err)
	}
	if err := store.PutArtifacts(ctx, "missing", nil); !errors.Is(err, ErrBuildNotFound) {
		t.Fatalf("missing build error = %v", err)
	}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{{ID: strings.Repeat("x", MaxIdentifierSize+1)}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized identifier error = %v", err)
	}
	canceled := &cancelAfterContext{remaining: 1}
	if err := store.PutArtifacts(canceled, build, nil); !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("late batch cancellation = %v", err)
	}
	if _, err := store.Validate(&cancelAfterContext{remaining: 1}, build); !errors.Is(err, ErrCanceled) {
		t.Fatalf("late validation cancellation = %v", err)
	}

	badSnapshot := bundle.Snapshot
	badSnapshot.Policy = map[string]any{"bad": make(chan int)}
	if err := store.Commit(ctx, build, badSnapshot); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unserializable snapshot error = %v", err)
	}
	mutations := []func(*rkcmodel.Snapshot){
		func(value *rkcmodel.Snapshot) { value.ID = "" },
		func(value *rkcmodel.Snapshot) { value.RepositoryID = "other" },
		func(value *rkcmodel.Snapshot) { value.ParentSnapshotID = "other" },
		func(value *rkcmodel.Snapshot) { value.SchemaVersion = "future" },
		func(value *rkcmodel.Snapshot) { value.Status = "building" },
	}
	for _, mutate := range mutations {
		candidate := bundle.Snapshot
		mutate(&candidate)
		if err := store.Commit(ctx, build, candidate); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid commit candidate %+v error = %v", candidate, err)
		}
	}
	if err := store.Abort(contextCanceled(), build, nil); !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled abort = %v", err)
	}
	if _, err := store.Recover(contextCanceled()); !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled recovery = %v", err)
	}
	if err := store.Abort(ctx, build, nil); err != nil {
		t.Fatal(err)
	}

	store.current[repository] = "missing"
	if _, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: repository, ParentSnapshotID: "missing"}); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("missing current snapshot = %v", err)
	}
	delete(store.current, repository)
	if _, err := store.BeginBuild(&cancelAfterContext{remaining: 1}, BuildOptions{RepositoryID: repository}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("late begin cancellation = %v", err)
	}

	var value uncloneable
	if _, err := cloneJSON(value); err == nil {
		t.Fatal("cloneJSON accepted a value that rejects decoding")
	}
}

func TestReaderAndPaginationFailurePaths(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repository := RepositoryID("repo")
	bundle := conformanceBundle("snapshot", repository, "", time.Unix(1, 0).UTC())
	commitBundle(t, store, bundle)

	invalidCalls := []func() error{
		func() error { _, err := store.Snapshot(ctx, ""); return err },
		func() error { _, err := store.Bundle(ctx, ""); return err },
		func() error { _, err := store.Current(ctx, ""); return err },
		func() error { _, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: " "}); return err },
		func() error { _, err := store.Artifact(ctx, "", "artifact"); return err },
		func() error { _, err := store.Node(ctx, "snapshot", ""); return err },
		func() error { _, err := store.Evidence(ctx, "snapshot", ""); return err },
		func() error { _, err := store.Coverage(ctx, ""); return err },
	}
	for _, call := range invalidCalls {
		if err := call(); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid read error = %v", err)
		}
	}

	missingCalls := []func() error{
		func() error { _, err := store.Snapshot(ctx, "missing"); return err },
		func() error { _, err := store.Bundle(ctx, "missing"); return err },
		func() error { _, err := store.Artifact(ctx, "missing", "artifact"); return err },
		func() error { _, err := store.Node(ctx, "missing", "node"); return err },
		func() error { _, err := store.Evidence(ctx, "missing", "evidence"); return err },
		func() error { _, err := store.Coverage(ctx, "missing"); return err },
		func() error { _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "missing"}); return err },
	}
	for _, call := range missingCalls {
		if err := call(); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("missing snapshot error = %v", err)
		}
	}
	if _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "snapshot", PageRequest: PageRequest{Limit: -1}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid node page = %v", err)
	}

	nodeScope := scopeFingerprint("snapshot", "", "", "", "")
	badPrimary := signedTestCursor(store, `{"v":1,"k":"nodes","s":"`+nodeScope+`","p":"bad","i":"node-a"}`)
	if _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "snapshot", PageRequest: PageRequest{Cursor: badPrimary}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("node cursor primary = %v", err)
	}
	missingNode := signedTestCursor(store, `{"v":1,"k":"nodes","s":"`+nodeScope+`","i":"missing"}`)
	if _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "snapshot", PageRequest: PageRequest{Cursor: missingNode}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("missing node cursor = %v", err)
	}
	snapshotScope := scopeFingerprint("")
	missingSnapshot := signedTestCursor(store, `{"v":1,"k":"snapshots","s":"`+snapshotScope+`","p":"1970-01-01T00:00:01Z","i":"missing"}`)
	if _, err := store.ListSnapshots(ctx, SnapshotQuery{PageRequest: PageRequest{Cursor: missingSnapshot}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("missing snapshot cursor = %v", err)
	}

	edges := []rkcmodel.Edge{{ID: "a"}, {ID: "b"}}
	edgePage, err := store.edgePage("edges", edges, 1, cursorPayload{}, "scope")
	if err != nil || len(edgePage.Items) != 1 || edgePage.Next == "" {
		t.Fatalf("edge page = %+v, %v", edgePage, err)
	}
	diagnostics := []rkcmodel.Diagnostic{{ID: "a"}, {ID: "b"}}
	diagnosticPage, err := store.diagnosticPage("diagnostics", diagnostics, 1, cursorPayload{}, "scope")
	if err != nil || len(diagnosticPage.Items) != 1 || diagnosticPage.Next == "" {
		t.Fatalf("diagnostic page = %+v, %v", diagnosticPage, err)
	}
}

func TestValidateDoesNotMutateStagedRecords(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	bundle := conformanceBundle("snapshot", "repo", "", time.Unix(1, 0).UTC())
	second := bundle.Evidence[0]
	second.ID = "evidence-z"
	bundle.Evidence = append(bundle.Evidence, second)
	bundle.Nodes[0].EvidenceIDs = []string{second.ID, bundle.Evidence[0].ID}
	build := beginAndStage(t, store, bundle, true)

	store.mu.RLock()
	before := append([]string(nil), store.builds[build].nodes["node-a"].EvidenceIDs...)
	store.mu.RUnlock()
	if _, err := store.Validate(ctx, build); err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	after := append([]string(nil), store.builds[build].nodes["node-a"].EvidenceIDs...)
	store.mu.RUnlock()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Validate mutated staged evidence IDs: before=%v after=%v", before, after)
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, 16)
	var wait sync.WaitGroup
	for worker := 0; worker < cap(errorsSeen); worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Validate(ctx, build)
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func contextCanceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
