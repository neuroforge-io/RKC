package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/neuroforge-io/RKC/internal/acquire"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/privatepath"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/internal/workspace"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func runWorkspace(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage: rkc workspace <add|list|review|sync|watch> --workspace <private-directory> [options]\n\nRegister a local folder with add --id <alias> <folder>, then sync and watch.\nRemote acquisition requires add --remote --id <alias> <https-or-ssh-url>.\nWatch polls admitted content; it never pulls or writes to local sources.")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "add":
		return runWorkspaceAdd(args[1:])
	case "list":
		return runWorkspaceList(args[1:])
	case "review":
		return runWorkspaceReview(args[1:])
	case "sync":
		return runDirectCommandWithAdmission(ctx, "workspace", args, runWorkspaceSyncContext)
	case "watch":
		return runWorkspaceWatch(ctx, args[1:])
	default:
		return errors.New("workspace expects add, list, review, sync, or watch")
	}
}

func workspaceFlags(command string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("workspace "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs, fs.String("workspace", "", "owner-private workspace directory outside every local source")
}

func workspaceRegistryPath(root string) (string, error) {
	if root == "" {
		return "", errors.New("--workspace is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if len(absolute) > 32768 {
		return "", errors.New("workspace path exceeds platform bound")
	}
	ancestor := absolute
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor || len(suffix) >= 128 {
			return "", errors.New("workspace path has no bounded existing ancestor")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
	ancestor, err = filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		ancestor = filepath.Join(ancestor, suffix[i])
	}
	return filepath.Join(ancestor, "registry.json"), nil
}

func runWorkspaceAdd(args []string) error {
	fs, root := workspaceFlags("add")
	id := fs.String("id", "", "stable lowercase source alias, for example project-docs")
	label := fs.String("label", "", "display label (defaults to alias)")
	remote := fs.Bool("remote", false, "explicitly opt into remote acquisition in separate managed checkouts")
	ref := fs.String("ref", "", "remote branch, tag, or commit; omitted tracks remote default branch")
	excludes := stringList{}
	fs.Var(&excludes, "exclude", "additional exact repository-relative path and descendants to exclude; repeatable")
	patterns := stringList{}
	fs.Var(&patterns, "exclude-pattern", "additional slash glob with whole-segment **; matched paths are explicitly inventoried as exclusions; repeatable")
	reviewFile := fs.String("secret-reviews", "", "explicit private JSON file of source-hash-bound false-positive reviews")
	limits := workspace.DefaultLimits()
	fs.IntVar(&limits.MaxFiles, "max-files", limits.MaxFiles, "maximum encountered paths (at most 500000)")
	fs.Int64Var(&limits.MaxRepositoryBytes, "max-repository-bytes", limits.MaxRepositoryBytes, "maximum encountered regular-file bytes (at most 20 GiB)")
	fs.Int64Var(&limits.MaxFileBytes, "max-file-bytes", limits.MaxFileBytes, "largest file hashed (at most 1 GiB)")
	fs.Int64Var(&limits.MaxTextBytes, "max-text-bytes", limits.MaxTextBytes, "largest file parsed (at most 8 MiB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("workspace add requires one local folder or explicitly enabled remote URL")
	}
	path, err := workspaceRegistryPath(*root)
	if err != nil {
		return err
	}
	if *label == "" {
		*label = *id
	}
	source := workspace.Source{ID: *id, Label: *label, Kind: "local", Limits: limits, ExcludePatterns: patterns}
	if *reviewFile != "" {
		source.SecretReviews, err = workspace.LoadSecretReviews(*reviewFile)
		if err != nil {
			return err
		}
	}
	if *remote {
		source.Kind, source.RemoteURL, source.Ref = "git", fs.Arg(0), *ref
	} else {
		if *ref != "" {
			return errors.New("--ref requires --remote")
		}
		source.LocalPath, err = filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		source.LocalPath, err = filepath.EvalSymlinks(source.LocalPath)
		if err != nil {
			return err
		}
		info, err := os.Stat(source.LocalPath)
		if err != nil || !info.IsDir() {
			return errors.New("workspace source must be an existing directory")
		}
		if err := workspaceDisjoint(filepath.Dir(path), source.LocalPath); err != nil {
			return err
		}
	}
	unique := map[string]bool{}
	for _, exclude := range append(inventory.DefaultExclusions(), excludes...) {
		unique[exclude] = true
	}
	source.Excludes = make([]string, 0, len(unique))
	for exclude := range unique {
		source.Excludes = append(source.Excludes, exclude)
	}
	sort.Strings(source.Excludes)
	store, err := workspace.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Add(source); err != nil {
		return err
	}
	fmt.Printf("Registered %s. Run rkc workspace sync --workspace %s\n", source.ID, quoteCommandPath(filepath.Dir(path), runtime.GOOS))
	return nil
}

func runWorkspaceList(args []string) error {
	fs, root := workspaceFlags("list")
	jsonOutput := fs.Bool("json", false, "print the private registry, including local paths; do not publish it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("workspace list does not accept positional arguments")
	}
	path, err := workspaceRegistryPath(*root)
	if err != nil {
		return err
	}
	registry, err := workspace.Load(path)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(registry)
	}
	for _, source := range registry.Sources {
		snapshotID := "not compiled"
		if source.Active != nil {
			snapshotID = source.Active.SnapshotID
		}
		checked := "never"
		if !source.Freshness.CheckedAt.IsZero() {
			checked = source.Freshness.CheckedAt.Format(time.RFC3339)
		}
		fmt.Printf("%s\t%s\tchecked=%s\t%s\t%s\n", source.ID, source.Freshness.Status, checked, snapshotID, source.Freshness.ErrorCode)
	}
	return nil
}

func runWorkspaceReview(args []string) error {
	fs, root := workspaceFlags("review")
	id := fs.String("id", "", "registered source alias")
	reviewFile := fs.String("secret-reviews", "", "private JSON file of reviewed false positives; an empty array clears reviews")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *id == "" || *reviewFile == "" {
		return errors.New("workspace review requires --id and --secret-reviews")
	}
	reviews, err := workspace.LoadSecretReviews(*reviewFile)
	if err != nil {
		return err
	}
	path, err := workspaceRegistryPath(*root)
	if err != nil {
		return err
	}
	store, err := workspace.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetSecretReviews(*id, reviews); err != nil {
		return err
	}
	fmt.Printf("Recorded %d source-bound false-positive reviews for %s; refresh is required.\n", len(reviews), *id)
	return nil
}

func runWorkspaceSyncContext(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return errors.New("protected workspace command must be sync")
	}
	fs, root := workspaceFlags("sync")
	timeout := fs.Duration("timeout", 10*time.Minute, "maximum time per source (1s to 1h)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 1 || *timeout < time.Second || *timeout > time.Hour {
		return errors.New("workspace sync accepts at most one alias and a timeout from 1s to 1h")
	}
	path, err := workspaceRegistryPath(*root)
	if err != nil {
		return err
	}
	store, err := workspace.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	selected := fs.Arg(0)
	if selected != "" {
		found := false
		for _, source := range store.Registry.Sources {
			found = found || source.ID == selected
		}
		if !found {
			return errors.New("workspace source alias is not registered")
		}
	}
	var failures []error
	for _, source := range store.Registry.Sources {
		if selected != "" && source.ID != selected {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		sourceContext, cancel := context.WithTimeout(ctx, *timeout)
		err := store.Refresh(sourceContext, source.ID, compileWorkspaceSource)
		cancel()
		if err != nil {
			failure := fmt.Errorf("source %s refresh failed: %w", source.ID, err)
			fmt.Fprintln(os.Stderr, failure)
			failures = append(failures, failure)
		} else {
			for _, current := range store.Registry.Sources {
				if current.ID == source.ID {
					fmt.Printf("Workspace source %s: %s.\n", source.ID, current.Freshness.Status)
				}
			}
		}
	}
	if err := store.Prune(); err != nil {
		failures = append(failures, fmt.Errorf("workspace generation cleanup: %w", err))
	}
	return errors.Join(failures...)
}

func runWorkspaceWatch(ctx context.Context, args []string) error {
	fs, root := workspaceFlags("watch")
	interval := fs.Duration("interval", time.Minute, "delay after each completed pass (30s to 24h)")
	timeout := fs.Duration("timeout", 10*time.Minute, "maximum time per source (1s to 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 || *interval < 30*time.Second || *interval > 24*time.Hour || *timeout < time.Second || *timeout > time.Hour {
		return errors.New("watch accepts at most one alias, interval 30s..24h, and timeout 1s..1h")
	}
	path, err := workspaceRegistryPath(*root)
	if err != nil {
		return err
	}
	if _, err := workspace.Load(path); err != nil {
		return err
	}
	syncArgs := []string{"sync", "--workspace", filepath.Dir(path), "--timeout", timeout.String()}
	if fs.NArg() == 1 {
		syncArgs = append(syncArgs, fs.Arg(0))
	}
	return watchWorkspace(ctx, *interval, func(ctx context.Context) error {
		return runDirectCommandWithAdmission(ctx, "workspace", syncArgs, runWorkspaceSyncContext)
	}, func(err error) {
		fmt.Fprintln(os.Stderr, "Workspace watch: refresh failed; last good generations remain available. Retry after the polling interval.")
	})
}

func watchWorkspace(ctx context.Context, interval time.Duration, syncPass func(context.Context) error, report func(error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := syncPass(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func workspaceDisjoint(workspaceRoot, sourceRoot string) error {
	for _, pair := range [][2]string{{workspaceRoot, sourceRoot}, {sourceRoot, workspaceRoot}} {
		within, err := pathIsWithin(pair[0], pair[1])
		if err != nil {
			return err
		}
		if within {
			return errors.New("workspace and local source directories must be disjoint")
		}
	}
	return nil
}

func compileWorkspaceSource(ctx context.Context, source workspace.Source, generations string) (*workspace.Active, error) {
	return compileWorkspaceSourceUsing(ctx, source, generations, runScanContext)
}

func compileWorkspaceSourceUsing(ctx context.Context, source workspace.Source, generations string, scan func(context.Context, []string) error) (*workspace.Active, error) {
	root := source.LocalPath
	if source.Kind == "git" {
		if runtime.GOOS != "linux" {
			return nil, errors.New("remote workspace sync requires Linux protected Git acquisition; local folder sync is portable")
		}
		acquired, err := acquire.Open(ctx, source.RemoteURL, acquire.Options{Ref: source.Ref, Depth: 1, Timeout: 5 * time.Minute, TemporaryRoot: filepath.Dir(generations), MaximumLogBytes: 64 << 10})
		if err != nil {
			return nil, &workspace.RefreshError{Code: "source_unavailable", Cause: err}
		}
		defer acquired.Cleanup()
		root = acquired.Root
	} else if err := workspaceDisjoint(filepath.Dir(generations), root); err != nil {
		return nil, err
	}
	beforeIdentity, err := os.Stat(root)
	if err != nil {
		return nil, &workspace.RefreshError{Code: "source_unavailable", Cause: err}
	}
	before, fingerprint, resolvedExcludes, err := workspaceObservation(ctx, source, root)
	if err != nil {
		return nil, &workspace.RefreshError{Code: "source_unavailable", Cause: err}
	}
	if source.Active != nil && source.Active.Fingerprint == fingerprint && source.Active.CompilerVersion == version {
		// A damaged or removed active atlas is repaired on the next pass, even
		// when source bytes have not changed.
		if err := server.VerifyUnchangedExport(ctx, source.Active.AtlasPath, source.Active.SnapshotID, source.Active.ManifestSHA256); err == nil {
			return nil, nil
		}
	}
	identity, err := privatepath.Lstat(generations)
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckDir(generations, identity); err != nil {
		return nil, err
	}
	generation, err := workspace.CreateGeneration(generations, source.ID)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(generation)
		}
	}()
	atlas := filepath.Join(generation, "atlas")
	scanRoot := root
	if source.Kind == "local" {
		captureParent := filepath.Join(generation, "capture")
		if err := os.Mkdir(captureParent, 0700); err != nil {
			return nil, err
		}
		defer os.RemoveAll(captureParent)
		scanRoot = filepath.Join(captureParent, filepath.Base(root))
		if scanRoot == captureParent {
			return nil, errors.New("cannot capture a filesystem root")
		}
		if err := workspace.Capture(ctx, root, scanRoot, before, source.Limits); err != nil {
			return nil, &workspace.RefreshError{Code: "source_changed", Cause: err}
		}
		_, captured, err := workspaceFingerprint(ctx, source, root)
		afterCaptureIdentity, statErr := os.Stat(root)
		if err != nil || statErr != nil || !os.SameFile(beforeIdentity, afterCaptureIdentity) || captured != fingerprint {
			return nil, &workspace.RefreshError{Code: "source_changed", Cause: errors.New("source changed during capture; retrying requires a stable capture")}
		}
	}
	args := []string{"--no-python", "--no-git-metadata", "--out", atlas, "--cache-dir", filepath.Join(filepath.Dir(generations), "cache"), "--runs-dir", filepath.Join(filepath.Dir(generations), "runs"), "--stage-workers", "1", "--stage-memory-mib", "512", "--max-files", strconv.Itoa(source.Limits.MaxFiles), "--max-repository-bytes", strconv.FormatInt(source.Limits.MaxRepositoryBytes, 10), "--max-file-bytes", strconv.FormatInt(source.Limits.MaxFileBytes, 10), "--max-text-bytes", strconv.FormatInt(source.Limits.MaxTextBytes, 10)}
	for _, exclude := range resolvedExcludes {
		args = append(args, "--exclude", exclude)
	}
	args = append(args, scanRoot)
	if err := scan(ctx, args); err != nil {
		return nil, err
	}
	dataset, err := server.Load(atlas)
	if err != nil {
		return nil, err
	}
	coverage := dataset.Coverage
	reviewed := workspace.CountReviewedSecrets(dataset.Bundle, source.SecretReviews)
	if dataset.Integrity != server.IntegrityVerified || coverage.DiagnosticsBySeverity["error"] > 0 || coverage.HighConfidenceSecretFindings-reviewed > 0 || coverage.InventoryAccountingRatio < 1 || coverage.SymbolEvidenceRatio < 1 || coverage.ClaimCitationRatio < 1 {
		return nil, &workspace.RefreshError{Code: "quality_failed", Cause: fmt.Errorf("new atlas failed workspace quality gates: integrity=%s errors=%d high_confidence_secrets=%d reviewed_false_positives=%d inventory_accounting=%.6f symbol_evidence=%.6f claim_citations=%.6f", dataset.Integrity, coverage.DiagnosticsBySeverity["error"], coverage.HighConfidenceSecretFindings, reviewed, coverage.InventoryAccountingRatio, coverage.SymbolEvidenceRatio, coverage.ClaimCitationRatio)}
	}
	_, after, err := workspaceFingerprint(ctx, source, root)
	afterIdentity, statErr := os.Stat(root)
	if dataset.Manifest.ContentDigest != before.Digest {
		return nil, &workspace.RefreshError{Code: "source_changed", Cause: errors.New("compiled snapshot does not match its captured inventory")}
	}
	sourceAdvanced := err != nil || statErr != nil || !os.SameFile(beforeIdentity, afterIdentity) || fingerprint != after
	manifestDigest, err := workspaceManifestDigest(atlas)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := privatepath.SyncDirectoryStable(generations, identity); err != nil {
		return nil, err
	}
	keep = true
	return &workspace.Active{AtlasPath: atlas, SnapshotID: dataset.Manifest.ID, Generation: filepath.Base(generation), ManifestSHA256: manifestDigest, Fingerprint: fingerprint, CompilerVersion: version, SourceAdvanced: sourceAdvanced, ReviewedSecretFindings: reviewed}, nil
}

func workspaceFingerprint(ctx context.Context, source workspace.Source, root string) (inventory.Result, string, error) {
	result, fingerprint, _, err := workspaceObservation(ctx, source, root)
	return result, fingerprint, err
}

func workspaceObservation(ctx context.Context, source workspace.Source, root string) (inventory.Result, string, []string, error) {
	if err := ctx.Err(); err != nil {
		return inventory.Result{}, "", nil, err
	}
	excludes, err := workspace.ResolveExclusions(ctx, root, source.Excludes, source.ExcludePatterns, source.Limits.MaxFiles)
	if err != nil {
		return inventory.Result{}, "", nil, err
	}
	result, err := inventory.ScanContext(ctx, inventory.Options{Root: root, MaxFiles: source.Limits.MaxFiles, MaxRepositoryBytes: source.Limits.MaxRepositoryBytes, MaxFileBytes: source.Limits.MaxFileBytes, MaxTextBytes: source.Limits.MaxTextBytes, Excludes: excludes})
	if err != nil {
		return result, "", nil, err
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == "error" {
			return result, "", nil, errors.New("source inventory has unreadable paths")
		}
	}
	// Include symlink targets and oversized-file metadata as well as admitted
	// file hashes. The canonical inventory digest alone omits those fields.
	fingerprint := rkcmodel.DigestJSON(struct {
		Artifacts []rkcmodel.Artifact
		Limits    workspace.Limits
		Excludes  []string
		Patterns  []string
		Reviews   []workspace.SecretReview
	}{result.Artifacts, source.Limits, source.Excludes, source.ExcludePatterns, source.SecretReviews})
	return result, fingerprint, excludes, ctx.Err()
}

func workspaceManifestDigest(atlas string) (string, error) {
	path := filepath.Join(atlas, "rkc-export-manifest.json")
	before, err := privatepath.Lstat(path)
	if err != nil {
		return "", err
	}
	const maximum = 64 << 20
	if !before.Mode().IsRegular() || before.Size() > maximum {
		return "", errors.New("workspace export manifest is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.New("workspace export manifest identity changed")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil || count != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return "", errors.New("workspace export manifest changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
