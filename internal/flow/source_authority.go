package flow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// readInventoriedFlowSource keeps raw source authority separate from canonical
// display paths. It never returns a raw path in an error or emitted record.
func readInventoriedFlowSource(root string, artifact rkcmodel.Artifact,
	file pluginapi.FileRef,
) ([]byte, error) {
	if !artifact.Text || artifact.Language != "go" || file.Language != "go" ||
		file.ArtifactID != artifact.ID || rkcmodel.StableID("artifact", file.Path) != artifact.ID ||
		file.SHA256 != artifact.SHA256 || file.SizeBytes != artifact.SizeBytes ||
		file.SizeBytes < 0 || file.SizeBytes == int64(^uint64(0)>>1) {
		return nil, errors.New("flow source authority is inconsistent")
	}
	if digest, err := hex.DecodeString(file.SHA256); err != nil || len(digest) != sha256.Size {
		return nil, errors.New("flow source digest is invalid")
	}
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		return nil, errors.New("flow source cannot be opened safely")
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil || before.Size() != file.SizeBytes {
		return nil, errors.New("flow source size changed")
	}
	data, err := io.ReadAll(io.LimitReader(input, file.SizeBytes+1))
	if err != nil || int64(len(data)) != file.SizeBytes {
		return nil, errors.New("flow source read failed or changed")
	}
	after, err := input.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("flow source identity changed")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != file.SHA256 {
		return nil, errors.New("flow source digest changed")
	}
	return data, nil
}
