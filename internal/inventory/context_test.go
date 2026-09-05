package inventory

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScanContextPreservesInventoryAndRejectsCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Useful source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Root: root}
	legacy, err := Scan(opts)
	if err != nil {
		t.Fatal(err)
	}
	current, err := ScanContext(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, current) {
		t.Fatal("context changed inventory contract")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ScanContext(ctx, opts)
	if !errors.Is(err, context.Canceled) || result.Digest != "" || len(result.Artifacts) != 0 {
		t.Fatalf("canceled inventory was usable: %+v %v", result, err)
	}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	calls  int
}

func (reader *cancelAfterRead) Read(buffer []byte) (int, error) {
	reader.calls++
	n, err := reader.reader.Read(buffer)
	reader.cancel()
	return n, err
}

func TestInventoryHashReadsStopAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &cancelAfterRead{reader: strings.NewReader(strings.Repeat("a", 128<<10)), cancel: cancel}
	count, err := io.Copy(io.Discard, inventoryContextReader{ctx: ctx, reader: source})
	if !errors.Is(err, context.Canceled) || source.calls != 1 || count > 32<<10 {
		t.Fatalf("hash reader ignored cancellation: bytes=%d calls=%d err=%v", count, source.calls, err)
	}
}
