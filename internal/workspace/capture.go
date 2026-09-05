package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
)

// Capture copies an already bounded inventory into a new private directory.
// Every admitted file must still match its observed size and content digest.
// Exclusions retain empty placeholders; oversized unhashed files use sparse
// placeholders so their recorded disposition remains identical without reading
// their contents. Callers must recheck the live source before accepting capture.
func Capture(ctx context.Context, source, target string, observed inventory.Result, limits Limits) (resultErr error) {
	if ctx == nil {
		return errors.New("source capture context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(observed.Artifacts) > limits.MaxFiles || limits.MaxFiles < 1 || limits.MaxRepositoryBytes < 1 {
		return errors.New("source capture exceeds inventory limits")
	}
	if err := rejectSymlinks(source); err != nil {
		return err
	}
	if err := rejectSymlinks(filepath.Dir(target)); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0700); err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(target)
		}
	}()
	var bytes int64
	for _, artifact := range observed.Artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !fs.ValidPath(artifact.Path) || artifact.Path == "." || strings.ContainsAny(artifact.Path, `\:`) {
			return errors.New("source capture artifact has an unsafe path")
		}
		if artifact.SizeBytes < 0 || artifact.SizeBytes > limits.MaxRepositoryBytes-bytes {
			return errors.New("source capture exceeds repository byte limit")
		}
		bytes += artifact.SizeBytes
		output := filepath.Join(target, filepath.FromSlash(artifact.Path))
		if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
			return err
		}
		if err := rejectSymlinks(filepath.Dir(output)); err != nil {
			return err
		}
		switch artifact.Kind {
		case "directory":
			if artifact.Status != "excluded" {
				return errors.New("unexpected captured directory disposition")
			}
			if err := os.Mkdir(output, 0700); err != nil {
				return err
			}
		case "symlink":
			if err := os.Symlink(artifact.Target, output); err != nil {
				return fmt.Errorf("capture source symlink: %w", err)
			}
		case "file":
			file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if artifact.Status == "excluded" {
				err = file.Close()
			} else if artifact.SHA256 == "" && artifact.Status == "oversized" {
				err = errors.Join(file.Truncate(artifact.SizeBytes), file.Close())
			} else {
				err = captureRegular(ctx, source, artifact.Path, artifact.SizeBytes, artifact.SHA256, file)
				err = errors.Join(err, file.Close())
			}
			if err != nil {
				return err
			}
		default:
			return errors.New("source capture requires explicit exclusion of special or unreadable files")
		}
	}
	return ctx.Err()
}

func captureRegular(ctx context.Context, root, path string, size int64, digest string, output io.Writer) error {
	input, err := sourcepath.OpenRegular(root, path)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || info.Size() != size {
		return errors.New("source changed before capture")
	}
	hash := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(output, hash), io.LimitReader(captureReader{ctx, input}, size+1), make([]byte, 32<<10))
	if err != nil {
		return err
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("source changed during capture")
	}
	return ctx.Err()
}

type captureReader struct {
	ctx    context.Context
	source io.Reader
}

func (reader captureReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > 32<<10 {
		buffer = buffer[:32<<10]
	}
	return reader.source.Read(buffer)
}
