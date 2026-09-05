package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/knowledgepack"
)

func runKnowledge(args []string) error {
	if len(args) == 0 {
		return errors.New("knowledge requires build or verify; run 'rkc knowledge --help'")
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Println(`Create a portable, source-cited knowledge pack from verified atlases.

Usage:
  rkc knowledge build --out PACK [options] ATLAS [ATLAS...]
  rkc knowledge verify --dir PACK [--json]

Build reads processed atlases sequentially, includes verified redacted text,
and preserves provenance, citations, source groups, and explicit limitations.
It does not read live source files or infer permission to train on them.
Run 'rkc knowledge build --help' for resource limits and publication options.`)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "build":
		return runKnowledgeBuild(ctx, args[1:])
	case "verify":
		return runKnowledgeVerify(ctx, args[1:])
	default:
		return fmt.Errorf("unknown knowledge action %q; expected build or verify", args[0])
	}
}

func runKnowledgeBuild(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("knowledge build", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	output := flags.String("out", "", "new directory for the portable knowledge pack (required)")
	force := flags.Bool("force", false, "replace only a complete, verified RKC knowledge pack")
	jsonOutput := flags.Bool("json", false, "print the pack manifest as JSON")
	maxUnits := flags.Int("max-units", 100000, "maximum units; excess fails without publishing a partial pack")
	maxUnitBytes := flags.Int("max-unit-text-bytes", 16*1024, "maximum UTF-8 text bytes per unit (256..65536); truncation is labeled")
	maxTotalBytes := flags.Int("max-total-text-bytes", 64*1024*1024, "maximum total retained text bytes; excess fails without publishing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*output) == "" || flags.NArg() < 1 {
		return errors.New("knowledge build requires --out PACK and at least one processed atlas directory")
	}
	if flags.NArg() > knowledgepack.MaximumSources {
		return fmt.Errorf("knowledge build supports at most %d atlases per pack", knowledgepack.MaximumSources)
	}
	builder, err := knowledgepack.New(knowledgepack.Options{MaxUnits: *maxUnits, MaxUnitTextBytes: *maxUnitBytes, MaxTotalTextBytes: *maxTotalBytes})
	if err != nil {
		return err
	}
	target, err := safeoutput.ResolveTarget(*output, "")
	if err != nil {
		return err
	}
	var protectedRoots []string
	for _, path := range flags.Args() {
		if err := ctx.Err(); err != nil {
			return err
		}
		dataset, err := server.Load(path)
		if err != nil {
			return fmt.Errorf("load knowledge source %q: %w", path, err)
		}
		if dataset.Integrity != server.IntegrityVerified {
			return fmt.Errorf("knowledge source %q requires a modern verified atlas", path)
		}
		if err := knowledgeDisjointOutput(target, dataset.Root); err != nil {
			return err
		}
		protectedRoots = append(protectedRoots, dataset.Root)
		bodies := map[string]string{}
		for _, document := range dataset.Search.Documents {
			if document.ObjectType == "artifact" && document.Metadata["rkc_body_kind"] == "secret-redacted-repository-text" {
				bodies[document.ID] = document.Body
			}
		}
		if err := builder.Add(ctx, knowledgepack.Input{Bundle: dataset.Bundle, ArtifactBodies: bodies, Integrity: dataset.Integrity}); err != nil {
			return fmt.Errorf("admit knowledge source %q: %w", path, err)
		}
	}
	pack, err := builder.Finish()
	if err != nil {
		return err
	}
	publication, err := safeoutput.Begin(target, protectedRoots[0], *force, "knowledge")
	if err != nil {
		return err
	}
	defer func() { _ = publication.Abort() }()
	for _, root := range protectedRoots {
		if err := knowledgeDisjointOutput(publication.Target, root); err != nil {
			return err
		}
	}
	manifest, err := knowledgepack.Write(ctx, publication.Staging, pack)
	if err != nil {
		return err
	}
	if _, err := knowledgepack.Verify(ctx, publication.Staging); err != nil {
		return fmt.Errorf("verify staged knowledge pack: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publication.Commit(manifest.PackID); err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(manifest)
	}
	fmt.Printf("Knowledge pack ready: %s\n%d sources · %d units · %d text bytes\n%d metadata-only · %d truncated · %d with unknown license\nVerify: rkc knowledge verify --dir %q\n", publication.Target, manifest.SourcesCount, manifest.UnitsCount, pack.Quality.TextBytes, pack.Quality.MetadataOnlyUnits, pack.Quality.TruncatedUnits, pack.Quality.UnknownLicenseUnits, publication.Target)
	return nil
}

func knowledgeDisjointOutput(target, root string) error {
	resolved, err := safeoutput.ResolveTarget(target, root)
	if err != nil {
		return err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil {
		return err
	}
	if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: knowledge output must be outside each input atlas", safeoutput.ErrUnsafeTarget)
	}
	return nil
}

func runKnowledgeVerify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("knowledge verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	directory := flags.String("dir", "", "knowledge pack directory (required)")
	jsonOutput := flags.Bool("json", false, "print verification and quality report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*directory) == "" || flags.NArg() != 0 {
		return errors.New("knowledge verify requires --dir PACK and accepts no positional arguments")
	}
	report, err := knowledgepack.Verify(ctx, *directory)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(report)
	}
	fmt.Printf("Verified %s\n%d sources · %d units · %d text bytes\n", report.Manifest.PackID, report.Manifest.SourcesCount, report.Manifest.UnitsCount, report.Quality.TextBytes)
	return nil
}
