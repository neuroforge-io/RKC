package main

import (
	"context"

	"github.com/neuroforge-io/RKC/internal/githubsource"
	"github.com/neuroforge-io/RKC/internal/pipeline"
)

// guiSourceProvenance is an internal compiler input supplied only after the
// server has downloaded and checked a pinned source archive. It is never
// populated from command-line flags, environment variables, or repository data.
type guiSourceProvenance struct {
	Origin  string
	Archive *pipeline.ArchiveProvenance
}

func runGUIArchive(ctx context.Context, checkout githubsource.Checkout) error {
	// Keep source-derived cache objects inside this owned acquisition lifecycle.
	// A failed or canceled private download must not leave shared stage-cache text.
	return runQuickstartWithSource(ctx, []string{"--no-git-metadata", "--clean", checkout.Root}, &guiSourceProvenance{
		Origin:  checkout.Repository.HTMLURL,
		Archive: &pipeline.ArchiveProvenance{Provider: "github", Revision: checkout.CommitSHA, ArchiveSHA256: checkout.ArchiveSHA256},
	})
}
