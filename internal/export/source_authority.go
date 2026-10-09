package export

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

// copySourceAuthority detaches the export session from caller-owned maps. The
// registry is bounded by this snapshot's inventory, and is never serialized.
func copySourceAuthority(opts Options, artifactCount int) (Options, error) {
	if opts.SourceFiles == nil {
		if opts.SourceIdentities != nil {
			return opts, errors.New("source identity registry requires inventoried file references")
		}
		return opts, nil
	}
	if len(opts.SourceFiles) > artifactCount || len(opts.SourceIdentities) > artifactCount {
		return opts, errors.New("source authority exceeds the snapshot inventory")
	}
	files := make(map[string]pluginapi.FileRef, len(opts.SourceFiles))
	for id, file := range opts.SourceFiles {
		if file.Attributes != nil {
			attributes := make(map[string]string, len(file.Attributes))
			for name, value := range file.Attributes {
				attributes[name] = value
			}
			file.Attributes = attributes
		}
		files[id] = file
	}
	identities := make(map[string]os.FileInfo, len(opts.SourceIdentities))
	for id, identity := range opts.SourceIdentities {
		identities[id] = identity
	}
	opts.SourceFiles, opts.SourceIdentities = files, identities
	return opts, nil
}

// readSourceArtifact treats the canonical path as a display label. Only a
// private raw inventory entry can authorize reading a redacted source name.
func readSourceArtifact(opts Options, artifact model.Artifact) ([]byte, error) {
	if opts.SourceFiles == nil {
		if sourceDisplayPathRedacted(artifact.Path) {
			return nil, errors.New("redacted source path requires private scan-session authority; export metadata without a source root or rescan the repository")
		}
		data, err := readVerifiedArtifact(opts.Root, artifact)
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			return nil, errors.New("repository source filesystem access failed")
		}
		return data, err
	}
	file, exists := opts.SourceFiles[artifact.ID]
	baseline := opts.SourceIdentities[artifact.ID]
	if !exists || baseline == nil {
		return nil, errors.New("source has no private inventoried identity")
	}
	if artifact.ID == "" || file.ArtifactID != artifact.ID || model.StableID("artifact", file.Path) != artifact.ID || file.SHA256 != artifact.SHA256 || file.SizeBytes != artifact.SizeBytes {
		return nil, errors.New("private source authority does not match the canonical artifact")
	}
	if artifact.SizeBytes < 0 || artifact.SizeBytes == int64(^uint64(0)>>1) || len(artifact.SHA256) != sha256.Size*2 || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
		return nil, errors.New("artifact has no valid inventoried size and SHA-256")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return nil, errors.New("artifact has no valid inventoried SHA-256")
	}
	input, err := sourcepath.OpenRegular(opts.Root, file.Path)
	if err != nil {
		// sourcepath's detailed errors include the raw private relative path.
		return nil, errors.New("inventoried source could not be safely opened")
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(baseline, opened) || opened.Size() != artifact.SizeBytes || !opened.ModTime().Equal(baseline.ModTime()) {
		return nil, errors.New("source identity changed after inventory")
	}
	data, err := io.ReadAll(io.LimitReader(input, artifact.SizeBytes+1))
	if err != nil {
		return nil, errors.New("read inventoried source failed")
	}
	if int64(len(data)) != artifact.SizeBytes {
		return nil, errors.New("source content changed after inventory (size differs)")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, errors.New("source content changed after inventory")
	}
	final, err := input.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("source identity changed while reading")
	}
	// Reopen without following symlinks to verify the named path still reaches
	// the same inode after reading. No source contents are copied a second time.
	current, err := sourcepath.OpenRegular(opts.Root, file.Path)
	if err != nil {
		return nil, errors.New("source path changed while reading")
	}
	currentInfo, statErr := current.Stat()
	closeErr := current.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(final, currentInfo) || currentInfo.Size() != final.Size() || !currentInfo.ModTime().Equal(final.ModTime()) {
		return nil, errors.New("source path identity changed while reading")
	}
	return data, nil
}

func sourceDisplayPathRedacted(path string) bool {
	return strings.Contains(path, "[REDACTED]") || strings.Contains(path, "*")
}

// normalizedSourcePaths preserves familiar clean display paths. Redacted names
// and case-insensitive collisions use opaque artifact-derived filenames, so
// two different inputs never overwrite the same normalized envelope.
func normalizedSourcePaths(artifacts []model.Artifact) (map[string]string, error) {
	counts := make(map[string]int, len(artifacts))
	for _, artifact := range artifacts {
		if !isNormalizedTextArtifact(artifact) {
			continue
		}
		candidate, err := canonicalRelativePath(artifact.Path + ".md")
		if err != nil {
			return nil, errors.New("normalized source has an unsafe display path")
		}
		counts[strings.ToLower(candidate)]++
	}
	paths := make(map[string]string, len(counts))
	used := make(map[string]struct{}, len(counts))
	for _, artifact := range artifacts {
		if !isNormalizedTextArtifact(artifact) {
			continue
		}
		if artifact.ID == "" {
			return nil, errors.New("normalized source has no artifact identity")
		}
		if _, exists := paths[artifact.ID]; exists {
			return nil, errors.New("normalized sources contain a duplicate artifact identity")
		}
		candidate := artifact.Path + ".md"
		if counts[strings.ToLower(candidate)] > 1 || sourceDisplayPathRedacted(artifact.Path) {
			digest := sha256.Sum256([]byte(artifact.ID))
			candidate = "_redacted/" + hex.EncodeToString(digest[:]) + ".md"
		}
		key := strings.ToLower(candidate)
		if _, exists := used[key]; exists {
			return nil, errors.New("normalized source output identities collide")
		}
		used[key] = struct{}{}
		paths[artifact.ID] = candidate
	}
	return paths, nil
}
