package sqlite

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const writerLeaseHelperPath = "RKC_SQLITE_WRITER_LEASE_HELPER_PATH"

func TestSQLiteWriterLeaseRejectsOperationsFromNonOwner(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "writer-owner.db")
	owner := openTestDatabase(t, path)
	peer := openTestDatabase(t, path)
	bundle := writerTestBundle("unowned-snapshot", "repository", "")
	build, err := owner.BeginBuild(context.Background(), rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}

	assertConflict := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, rkcstore.ErrConflict) {
			t.Fatalf("%s = %v, want ErrConflict", name, err)
		}
		var operation *rkcstore.OperationError
		if !errors.As(err, &operation) || operation.Code != rkcstore.CodeConflict ||
			operation.Field != "writer_lease" || operation.BuildID != build {
			t.Fatalf("%s operation error = %+v, want build-scoped writer lease conflict", name, operation)
		}
	}
	assertConflict("PutNodes", peer.PutNodes(context.Background(), build, []rkcmodel.Node{{ID: "node"}}))
	_, err = peer.Validate(context.Background(), build)
	assertConflict("Validate", err)
	assertConflict("Commit", peer.Commit(context.Background(), build, bundle.Snapshot))
	assertConflict("Abort", peer.Abort(context.Background(), build, errors.New("not owned")))

	var state string
	if err := peer.db.QueryRow("SELECT state FROM builds WHERE build_id = ?", build).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("non-owner operation changed build state to %q", state)
	}
	if err := owner.Abort(context.Background(), build, errors.New("owner cleanup")); err != nil {
		t.Fatal(err)
	}
	recovered, err := peer.Recover(context.Background())
	if err != nil || len(recovered.AbortedBuilds) != 0 {
		t.Fatalf("Recover after owner Abort = %+v, %v", recovered, err)
	}
}

func TestSQLiteWriterLeaseBlocksCrossProcessRecovery(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "writer-process.db")
	owner := openTestDatabase(t, path)
	build, err := owner.BeginBuild(context.Background(), rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestSQLiteWriterLeaseProcessHelper$")
	command.Env = append(os.Environ(), writerLeaseHelperPath+"="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery helper failed: %v\n%s", err, output)
	}
	if ctx.Err() != nil {
		t.Fatalf("recovery helper timed out: %v", ctx.Err())
	}
	var state string
	if err := owner.db.QueryRow("SELECT state FROM builds WHERE build_id = ?", build).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("cross-process recovery changed live build state to %q", state)
	}
	if err := owner.Abort(context.Background(), build, errors.New("owner cleanup")); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteWriterLeaseProcessHelper(t *testing.T) {
	path := os.Getenv(writerLeaseHelperPath)
	if path == "" {
		t.Skip("subprocess helper")
	}
	database, err := Open(context.Background(), testOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	result, err := database.Recover(context.Background())
	if !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("Recover = %+v, %v; want live writer conflict", result, err)
	}
	if len(result.AbortedBuilds) != 0 {
		t.Fatalf("contended Recover reported aborted builds: %+v", result)
	}
	var operation *rkcstore.OperationError
	if !errors.As(err, &operation) || operation.Code != rkcstore.CodeConflict {
		t.Fatalf("Recover operation error = %+v, want CodeConflict", operation)
	}
}

func TestSQLiteWriterLeaseFilePolicy(t *testing.T) {
	t.Run("created owner-only", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "safe.db")
		database := openTestDatabase(t, path)
		info, err := os.Lstat(path + writerLeaseSuffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != 0o600 || info.Size() != 0 {
			t.Fatalf("writer lease mode/size = %v/%d, want 0600/0", info.Mode(), info.Size())
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		directory := privateTempDir(t)
		path := filepath.Join(directory, "symlink.db")
		target := filepath.Join(directory, "target.lock")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+writerLeaseSuffix); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Open with symlink writer lease = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("permissive mode rejected", func(t *testing.T) {
		directory := privateTempDir(t)
		path := filepath.Join(directory, "permissive.db")
		if err := os.WriteFile(path+writerLeaseSuffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Open with permissive writer lease = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("nonempty file rejected", func(t *testing.T) {
		directory := privateTempDir(t)
		path := filepath.Join(directory, "nonempty.db")
		if err := os.WriteFile(path+writerLeaseSuffix, []byte("untrusted"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrUnsafePath) ||
			!strings.Contains(err.Error(), "empty owner-only regular file") {
			t.Fatalf("Open with nonempty writer lease = %v, want explicit ErrUnsafePath", err)
		}
	})

	t.Run("hard link rejected", func(t *testing.T) {
		directory := privateTempDir(t)
		path := filepath.Join(directory, "hardlink.db")
		lockPath := path + writerLeaseSuffix
		if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(lockPath, filepath.Join(directory, "alias.lock")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Open with hard-linked writer lease = %v, want ErrUnsafePath", err)
		}
	})
}

func TestSQLiteWriterLeaseRuntimeFaultIsInternal(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "runtime-fault.db")
	database := openTestDatabase(t, path)
	if err := os.Chmod(path+writerLeaseSuffix, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := database.BeginBuild(context.Background(), rkcstore.BuildOptions{RepositoryID: "repository"})
	if !errors.Is(err, rkcstore.ErrInternal) {
		t.Fatalf("BeginBuild with unsafe runtime lease = %v, want ErrInternal", err)
	}
	var operation *rkcstore.OperationError
	if !errors.As(err, &operation) || operation.Code != rkcstore.CodeInternal ||
		operation.Field != "writer_lease" {
		t.Fatalf("BeginBuild operation error = %+v, want writer lease CodeInternal", operation)
	}
}
