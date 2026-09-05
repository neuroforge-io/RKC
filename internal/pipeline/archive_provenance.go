package pipeline

import (
	"errors"
	"regexp"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourceorigin"
)

// ArchiveProvenance is supplied by a trusted acquisition adapter after it has
// resolved an immutable revision and verified the received archive. It records
// that acquisition receipt; it does not assert a verified Git working tree or
// independently prove that arbitrary caller-supplied files match a remote tree.
type ArchiveProvenance struct {
	Provider      string `json:"provider"`
	Revision      string `json:"revision"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

var archiveRevisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var archiveDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var githubOriginPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

func validateArchiveOptions(options *Options) error {
	if options.ArchiveProvenance == nil {
		return nil
	}
	provenance := *options.ArchiveProvenance
	if !options.SkipGitInspection || provenance.Provider != "github" ||
		!archiveRevisionPattern.MatchString(provenance.Revision) || !archiveDigestPattern.MatchString(provenance.ArchiveSHA256) {
		return errors.New("archive provenance requires the Git-free profile, GitHub provider, immutable lowercase revision and archive SHA-256")
	}
	if !sourceorigin.IsCanonical(options.Origin) || !githubOriginPattern.MatchString(options.Origin) {
		return errors.New("archive provenance requires a canonical credential-free GitHub repository origin")
	}
	for _, part := range strings.Split(strings.TrimPrefix(options.Origin, "https://github.com/"), "/") {
		if part == "." || part == ".." {
			return errors.New("archive provenance origin is not a GitHub repository")
		}
	}
	// Isolate the run from later caller changes to its receipt value.
	options.ArchiveProvenance = &provenance
	return nil
}

func addArchiveMetadata(metadata map[string]string, provenance *ArchiveProvenance) {
	if provenance == nil {
		return
	}
	metadata["source_provider"] = provenance.Provider
	metadata["source_revision"] = provenance.Revision
	metadata["source_archive_sha256"] = provenance.ArchiveSHA256
}
