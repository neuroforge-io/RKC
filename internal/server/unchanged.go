package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

// VerifyUnchangedExport rechecks every file in a previously verified atlas
// against its trusted export-manifest pin without decoding graph/search data.
// It is only a reuse check: new or untrusted exports still require Load's full
// canonical and derived-index validation before a caller records this pin.
func VerifyUnchangedExport(ctx context.Context, root, snapshotID, manifestSHA256 string) error {
	if ctx == nil {
		return errors.New("export verification context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshotID == "" || len(manifestSHA256) != 64 {
		return errors.New("verified snapshot and manifest pin are required")
	}
	before, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("atlas must be a real directory")
	}
	marker, err := safeoutput.ReadMarker(root)
	if err != nil {
		return err
	}
	if marker.Kind != "atlas" || marker.SnapshotID != snapshotID {
		return errors.New("atlas ownership does not match verified snapshot")
	}
	manifestPath := filepath.Join(root, exportManifestName)
	verifyPin := func() error {
		_, _, digest, err := readAndHashRegularFileContext(ctx, manifestPath, maximumExportManifestFileSize, nil)
		if err != nil {
			return err
		}
		if digest != manifestSHA256 {
			return errors.New("atlas export manifest no longer matches its verified pin")
		}
		return nil
	}
	if err := verifyPin(); err != nil {
		return err
	}
	manifest, _, err := verifyDatasetExportManifestMode(ctx, root, manifestPath, false)
	if err != nil {
		return err
	}
	if manifest.SnapshotID != snapshotID {
		return errors.New("atlas snapshot no longer matches its verified identity")
	}
	if err := verifyPin(); err != nil {
		return err
	}
	after, err := os.Lstat(root)
	if err != nil || !os.SameFile(before, after) {
		return errors.New("atlas directory changed during verification")
	}
	return ctx.Err()
}

type integrityReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader integrityReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
