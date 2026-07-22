// Package safeoutput publishes RKC-generated directories without permitting a
// user-supplied --force path to become an unbounded recursive delete.
package safeoutput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	// MarkerName is deliberately hidden from normal documentation exports while
	// remaining easy for inventory scans and operators to identify.
	MarkerName                = ".rkc-generated.json"
	markerVersion             = "1.0"
	producer                  = "rkc"
	outputManifestVersion     = rkcmodel.SchemaVersion
	markerMaxSize             = 64 * 1024
	ownershipManifestMaxSize  = 64 * 1024 * 1024
	ownershipManifestMaxFiles = 100_000
	ownershipManifestMaxBytes = int64(16 * 1024 * 1024 * 1024)
	journalName               = "journal.json"
	journalVersion            = "1.0"
	journalMaxSize            = 16 * 1024
	journalScanLimit          = 128
)

var (
	ErrTargetExists               = errors.New("output directory already exists")
	ErrTargetUnowned              = errors.New("existing output is not owned by RKC")
	ErrUnsafeTarget               = errors.New("unsafe output target")
	ErrInvalidStaging             = errors.New("invalid RKC staging directory")
	errAtomicNoReplaceUnavailable = errors.New("atomic no-replace output publication is unavailable on this platform")
	exchangeOperation             = exchangePaths
	renameNoReplaceOperation      = renameNoReplacePath
)

// Marker identifies generated metadata. It is never sufficient by itself to
// authorize force replacement: a final output also needs a complete,
// digest-valid exact-kind manifest. The marker contains no wall-clock value so
// repeated output stays reproducible.
type Marker struct {
	SchemaVersion string `json:"schema_version"`
	Producer      string `json:"producer"`
	Kind          string `json:"kind"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
}

// Transaction owns one sibling staging directory and can publish it once.
type Transaction struct {
	Target    string
	Staging   string
	kind      string
	force     bool
	committed bool
	identity  os.FileInfo
}

// ReplacementPlatformDescription reports the pathname availability guarantee
// used by force replacement on the current platform. Linux requires atomic
// exchange; Windows retains the prior output in quarantine across a documented
// bounded target-missing interval; unsupported platforms fail closed.
func ReplacementPlatformDescription() string { return replacementPlatformDescription }

// Begin validates a target, verifies any existing output belongs to RKC, and
// creates a sibling staging directory so the final rename is atomic.
func Begin(target, protectedRoot string, force bool, kind string) (*Transaction, error) {
	if kind != "atlas" && kind != "synthesis" {
		return nil, fmt.Errorf("%w: invalid output kind", ErrUnsafeTarget)
	}
	resolved, err := ResolveTarget(target, protectedRoot)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(resolved)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create output parent: %w", err)
	}
	// Resolve the now-existing parent again. A symlink swap during MkdirAll must
	// not redirect publication away from the canonical path ResolveTarget saw.
	verifiedParent, err := resolveExistingParent(parent)
	if err != nil || verifiedParent != parent {
		return nil, fmt.Errorf("%w: output parent identity changed during setup", ErrUnsafeTarget)
	}
	if err := recoverInterruptedReplacements(parent, filepath.Base(resolved)); err != nil {
		return nil, fmt.Errorf("recover interrupted output publication: %w", err)
	}
	if err := checkExisting(resolved, force, kind); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(parent, ".rkc-build-")
	if err != nil {
		return nil, fmt.Errorf("create output staging directory: %w", err)
	}
	identity, err := os.Lstat(staging)
	if err != nil {
		_ = os.Remove(staging)
		return nil, fmt.Errorf("inspect output staging directory: %w", err)
	}
	transaction := &Transaction{Target: resolved, Staging: staging, kind: kind, force: force, identity: identity}
	if err := writeMarker(staging, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		// A directory without a valid marker must never be recursively removed.
		// The freshly-created directory is expected to be empty after writeMarker
		// rolls its temporary file back, so a non-recursive removal is sufficient.
		if current, statErr := os.Lstat(staging); statErr == nil && current.IsDir() && os.SameFile(identity, current) {
			_ = os.Remove(staging)
		}
		return nil, err
	}
	return transaction, nil
}

// ResolveTarget rejects filesystem roots, the protected repository itself, an
// ancestor of that repository, final-component symlinks, and parent-symlink
// aliases that would otherwise bypass those checks.
func ResolveTarget(target, protectedRoot string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("%w: output path is empty", ErrUnsafeTarget)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve output target: %w", err)
	}
	abs = filepath.Clean(abs)
	if filepath.Dir(abs) == abs {
		return "", fmt.Errorf("%w: filesystem root cannot be an output", ErrUnsafeTarget)
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: output target cannot be a symlink", ErrUnsafeTarget)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect output target: %w", statErr)
	}
	parent := filepath.Dir(abs)
	resolvedParent, err := resolveExistingParent(parent)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(abs))
	if protectedRoot != "" {
		protected, err := filepath.Abs(protectedRoot)
		if err != nil {
			return "", fmt.Errorf("resolve protected root: %w", err)
		}
		// A missing, dangling, or temporarily unresolvable protected root must
		// never silently degrade this boundary to a lexical comparison. Callers
		// supplied the root specifically to protect it, so uncertainty is fatal.
		evaluated, evalErr := filepath.EvalSymlinks(protected)
		if evalErr != nil {
			return "", fmt.Errorf("%w: resolve protected root symlinks: %v", ErrUnsafeTarget, evalErr)
		}
		protected = filepath.Clean(evaluated)
		protectedInfo, statErr := os.Lstat(protected)
		if statErr != nil || !protectedInfo.IsDir() || protectedInfo.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: protected root is not a resolved directory", ErrUnsafeTarget)
		}
		if resolved == protected || containsPath(resolved, protected) {
			return "", fmt.Errorf("%w: output cannot be the repository or its ancestor", ErrUnsafeTarget)
		}
	}
	return resolved, nil
}

func resolveExistingParent(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("resolve output parent: %w", err)
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect output parent: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve output parent: %w", err)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// ReadMarker validates and returns an RKC ownership marker.
func ReadMarker(root string) (Marker, error) {
	markerPath := filepath.Join(root, MarkerName)
	info, err := os.Lstat(markerPath)
	if err != nil {
		return Marker{}, err
	}
	if !info.Mode().IsRegular() {
		return Marker{}, errors.New("invalid RKC output marker: marker is not a regular file")
	}
	if info.Size() > markerMaxSize {
		return Marker{}, errors.New("invalid RKC output marker: marker is too large")
	}
	file, err := os.Open(markerPath)
	if err != nil {
		return Marker{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > markerMaxSize {
		return Marker{}, errors.New("invalid RKC output marker: marker identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, markerMaxSize+1))
	if err != nil {
		return Marker{}, err
	}
	if len(data) > markerMaxSize {
		return Marker{}, errors.New("invalid RKC output marker: marker is too large")
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != int64(len(data)) {
		return Marker{}, errors.New("invalid RKC output marker: marker changed while reading")
	}
	var marker Marker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return Marker{}, fmt.Errorf("decode RKC output marker: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values are not permitted")
		}
		return Marker{}, fmt.Errorf("decode RKC output marker: %w", err)
	}
	if marker.SchemaVersion != markerVersion || marker.Producer != producer || strings.TrimSpace(marker.Kind) == "" {
		return Marker{}, errors.New("invalid RKC output marker")
	}
	return marker, nil
}

// IsGenerated reports whether a directory carries any valid RKC marker,
// including a staging marker left behind by an interrupted build.
func IsGenerated(root string) bool {
	_, err := ReadMarker(root)
	return err == nil
}

// Commit replaces the target atomically after writing the final marker. If the
// final rename fails after moving an old output aside, the old output is put
// back before the error is returned.
func (transaction *Transaction) Commit(snapshotID string) error {
	if transaction == nil || transaction.committed {
		return ErrInvalidStaging
	}
	if filepath.Dir(transaction.Staging) != filepath.Dir(transaction.Target) {
		return fmt.Errorf("%w: staging is not a target sibling", ErrInvalidStaging)
	}
	if err := transaction.validateStaging("staging"); err != nil {
		return err
	}
	if err := checkExisting(transaction.Target, transaction.force, transaction.kind); err != nil {
		return err
	}
	if err := writeMarker(transaction.Staging, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: transaction.kind, SnapshotID: snapshotID}); err != nil {
		return err
	}
	if err := validateOwnedOutput(transaction.Staging, transaction.kind, transaction.identity); err != nil {
		return fmt.Errorf("%w: staged output manifest is incomplete or invalid: %v", ErrInvalidStaging, err)
	}
	if err := syncDirectory(transaction.Staging); err != nil {
		return fmt.Errorf("sync RKC staging directory: %w", err)
	}
	if err := transaction.validateStaging(transaction.kind); err != nil {
		return err
	}
	// Publication and portable rollback both require one atomic primitive that
	// never replaces a concurrently-created pathname. Platforms without such a
	// primitive fail before the target or staging directory is moved.
	if !renameNoReplaceSupported() {
		return errAtomicNoReplaceUnavailable
	}
	// Recheck after finalizing staging. If the target changes after this check,
	// the moved-aside directory is validated again before it can be replaced or
	// deleted, closing the ownership TOCTOU window.
	priorIdentity, err := inspectExisting(transaction.Target, transaction.force, transaction.kind)
	if err != nil {
		return err
	}
	if priorIdentity == nil {
		return transaction.commitNewTarget(snapshotID)
	}
	journal, err := createReplacementJournal(transaction, priorIdentity, snapshotID)
	if err != nil {
		return fmt.Errorf("create durable replacement journal: %w", err)
	}
	if replacementHasNoMissingTargetWindow() {
		return transaction.commitExchange(priorIdentity, snapshotID, journal)
	}
	return transaction.commitPortable(priorIdentity, snapshotID, journal)
}

func (transaction *Transaction) commitNewTarget(snapshotID string) error {
	if err := renameNoReplaceOperation(transaction.Staging, transaction.Target); err != nil {
		return fmt.Errorf("publish RKC output without replacement: %w", err)
	}
	if err := validatePublishedOutput(transaction.Target, transaction.kind, transaction.identity, snapshotID); err != nil {
		// Move only the exact staged inode back. If its pathname was replaced,
		// retain everything and fail closed rather than touching the replacement.
		if current, statErr := os.Lstat(transaction.Target); statErr == nil && os.SameFile(transaction.identity, current) {
			_ = renameNoReplaceOperation(transaction.Target, transaction.Staging)
		}
		return fmt.Errorf("%w: published staging identity or manifest changed: %v", ErrInvalidStaging, err)
	}
	if err := syncDirectory(filepath.Dir(transaction.Target)); err != nil {
		return fmt.Errorf("sync published output parent: %w", err)
	}
	transaction.committed = true
	return nil
}

func (transaction *Transaction) commitExchange(priorIdentity os.FileInfo, snapshotID string, journal *replacementJournal) error {
	if err := exchangeOperation(transaction.Staging, transaction.Target); err != nil {
		_ = journal.discard()
		return fmt.Errorf("atomic force replacement requires renameat2(RENAME_EXCHANGE): %w", err)
	}
	if err := syncDirectory(filepath.Dir(transaction.Target)); err != nil {
		return fmt.Errorf("sync exchanged output parent; recovery journal retained at %s: %w", journal.root, err)
	}
	if err := validatePublishedOutput(transaction.Target, transaction.kind, transaction.identity, snapshotID); err != nil {
		return transaction.rollbackExchange(priorIdentity, journal, fmt.Errorf("%w: published staging verification failed: %v", ErrInvalidStaging, err))
	}
	if err := validateOwnedOutput(transaction.Staging, transaction.kind, priorIdentity); err != nil {
		return transaction.rollbackExchange(priorIdentity, journal, fmt.Errorf("%w: displaced prior output verification failed: %v", ErrTargetUnowned, err))
	}
	if err := journal.update("exchanged"); err != nil {
		return fmt.Errorf("persist exchanged publication state; journal retained at %s: %w", journal.root, err)
	}
	return transaction.finishReplacement(priorIdentity, snapshotID, journal, transaction.Staging)
}

func (transaction *Transaction) rollbackExchange(priorIdentity os.FileInfo, journal *replacementJournal, cause error) error {
	newAtTarget, newErr := sameDirectoryIdentity(transaction.Target, transaction.identity)
	oldAtStaging, oldErr := sameDirectoryIdentity(transaction.Staging, priorIdentity)
	if newErr != nil || oldErr != nil || !newAtTarget || !oldAtStaging {
		return fmt.Errorf("%w; exact rollback identities unavailable; journal retained at %s", cause, journal.root)
	}
	if err := exchangeOperation(transaction.Target, transaction.Staging); err != nil {
		return fmt.Errorf("%w; atomic rollback failed: %v; journal retained at %s", cause, err, journal.root)
	}
	if err := validateOwnedOutput(transaction.Target, transaction.kind, priorIdentity); err != nil {
		return fmt.Errorf("%w; rolled-back prior output failed validation: %v; journal retained at %s", cause, err, journal.root)
	}
	if err := syncDirectory(filepath.Dir(transaction.Target)); err != nil {
		return fmt.Errorf("%w; rollback parent sync failed: %v; journal retained at %s", cause, err, journal.root)
	}
	if err := journal.discard(); err != nil {
		return fmt.Errorf("%w; rollback succeeded but journal cleanup failed: %v", cause, err)
	}
	return cause
}

func (transaction *Transaction) commitPortable(priorIdentity os.FileInfo, snapshotID string, journal *replacementJournal) error {
	payload := filepath.Join(journal.root, "payload")
	if err := os.Rename(transaction.Target, payload); err != nil {
		_ = journal.discard()
		return fmt.Errorf("quarantine prior RKC output: %w", err)
	}
	if err := journal.update("prior-quarantined"); err != nil {
		return fmt.Errorf("persist quarantine state; prior retained at %s: %w", payload, err)
	}
	if err := renameNoReplaceOperation(transaction.Staging, transaction.Target); err != nil {
		if restoreErr := renameNoReplaceOperation(payload, transaction.Target); restoreErr != nil {
			return fmt.Errorf("publish RKC output: %w; restore failed: %v; journal retained at %s", err, restoreErr, journal.root)
		}
		_ = journal.discard()
		return fmt.Errorf("publish RKC output: %w", err)
	}
	return transaction.finishReplacement(priorIdentity, snapshotID, journal, payload)
}

func (transaction *Transaction) finishReplacement(priorIdentity os.FileInfo, snapshotID string, journal *replacementJournal, priorPath string) error {
	if err := validatePublishedOutput(transaction.Target, transaction.kind, transaction.identity, snapshotID); err != nil {
		return fmt.Errorf("%w: published target failed exact revalidation; prior retained at %s; journal retained at %s: %v", ErrInvalidStaging, priorPath, journal.root, err)
	}
	if err := validateOwnedOutput(priorPath, transaction.kind, priorIdentity); err != nil {
		return fmt.Errorf("prior output failed exact revalidation and was retained at %s: %w", priorPath, err)
	}
	payload := filepath.Join(journal.root, "payload")
	if priorPath != payload {
		if _, err := os.Lstat(payload); err == nil {
			return fmt.Errorf("quarantine payload unexpectedly exists at %s", payload)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect quarantine payload: %w", err)
		}
		if err := os.Rename(priorPath, payload); err != nil {
			return fmt.Errorf("move prior output into durable quarantine: %w", err)
		}
	}
	if err := journal.update("prior-quarantined"); err != nil {
		return fmt.Errorf("persist final quarantine state; journal retained at %s: %w", journal.root, err)
	}
	if err := syncDirectory(filepath.Dir(transaction.Target)); err != nil {
		return fmt.Errorf("sync published output parent: %w", err)
	}
	if err := validatePublishedOutput(transaction.Target, transaction.kind, transaction.identity, snapshotID); err != nil {
		return fmt.Errorf("published target changed before prior cleanup; prior retained at %s: %w", payload, err)
	}
	quarantine := journal.quarantine(priorIdentity)
	if err := quarantine.validate(ErrTargetUnowned, transaction.kind); err != nil {
		return fmt.Errorf("quarantined prior output changed before cleanup: %w", err)
	}
	transaction.committed = true
	if err := quarantine.remove(ErrTargetUnowned, transaction.kind); err != nil {
		return fmt.Errorf("published output is valid but prior quarantine cleanup failed: %w", err)
	}
	return nil
}

func validatePublishedOutput(path, kind string, identity os.FileInfo, snapshotID string) error {
	if err := validateOwnedOutput(path, kind, identity); err != nil {
		return err
	}
	marker, err := ReadMarker(path)
	if err != nil || marker.Kind != kind || marker.SnapshotID != snapshotID {
		return errors.New("published marker does not bind the requested kind and snapshot")
	}
	matched, err := sameDirectoryIdentity(path, identity)
	if err != nil || !matched {
		return errors.New("published target is not the exact staging directory inode")
	}
	return nil
}

func sameDirectoryIdentity(path string, identity os.FileInfo) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir() && identity != nil && os.SameFile(identity, info), nil
}

// Abort removes only the uniquely created staging directory.
func (transaction *Transaction) Abort() error {
	if transaction == nil || transaction.committed || transaction.Staging == "" {
		return nil
	}
	if err := transaction.validateStaging("staging", transaction.kind); err != nil {
		return err
	}
	if err := removeOwnedDirectory(transaction.Staging, transaction.identity, ErrInvalidStaging, "staging", transaction.kind); err != nil {
		return err
	}
	transaction.Staging = ""
	return nil
}

func (transaction *Transaction) validateStaging(kinds ...string) error {
	if transaction == nil || transaction.Staging == "" || transaction.identity == nil {
		return ErrInvalidStaging
	}
	info, err := os.Lstat(transaction.Staging)
	if err != nil || !info.IsDir() || !os.SameFile(transaction.identity, info) {
		return fmt.Errorf("%w: staging directory identity changed", ErrInvalidStaging)
	}
	marker, err := ReadMarker(transaction.Staging)
	if err != nil {
		return fmt.Errorf("%w: refusing to use unmarked staging path", ErrInvalidStaging)
	}
	for _, kind := range kinds {
		if marker.Kind == kind {
			return nil
		}
	}
	return fmt.Errorf("%w: staging marker kind changed", ErrInvalidStaging)
}

func checkExisting(target string, force bool, kind string) error {
	_, err := inspectExisting(target, force, kind)
	return err
}

func inspectExisting(target string, force bool, kind string) (os.FileInfo, error) {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect output target: %w", err)
	}
	if !force {
		return nil, fmt.Errorf("%w: %s", ErrTargetExists, target)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: target is not a regular directory", ErrTargetUnowned)
	}
	if err := validateOwnedOutput(target, kind, info); err == nil {
		return info, nil
	} else {
		return nil, fmt.Errorf("%w: %s: %v", ErrTargetUnowned, target, err)
	}
}

type ownershipManifest struct {
	SchemaVersion        string                  `json:"schema_version"`
	SnapshotID           string                  `json:"snapshot_id"`
	Files                []ownershipManifestFile `json:"files"`
	TotalBytes           *int64                  `json:"total_bytes,omitempty"`
	CanonicalBytes       *int64                  `json:"canonical_bytes,omitempty"`
	CanonicalFilesDigest string                  `json:"canonical_files_digest,omitempty"`
	Status               json.RawMessage         `json:"status,omitempty"`
	Profile              json.RawMessage         `json:"profile,omitempty"`
	Task                 json.RawMessage         `json:"task,omitempty"`
	PacketOnly           json.RawMessage         `json:"packet_only,omitempty"`
	Provider             json.RawMessage         `json:"provider,omitempty"`
	Model                json.RawMessage         `json:"model,omitempty"`
	Options              json.RawMessage         `json:"options,omitempty"`
	Budget               json.RawMessage         `json:"budget,omitempty"`
	SubjectsRequested    json.RawMessage         `json:"subjects_requested,omitempty"`
	PacketsWritten       json.RawMessage         `json:"packets_written,omitempty"`
	ResponsesReceived    json.RawMessage         `json:"responses_received,omitempty"`
	AcceptedClaims       json.RawMessage         `json:"accepted_claims,omitempty"`
	RejectedClaims       json.RawMessage         `json:"rejected_claims,omitempty"`
	AcceptedSummaries    json.RawMessage         `json:"accepted_summaries,omitempty"`
	RejectedSummaries    json.RawMessage         `json:"rejected_summaries,omitempty"`
	FailedSubjects       json.RawMessage         `json:"failed_subjects,omitempty"`
	RedactedFindings     json.RawMessage         `json:"redacted_secret_findings,omitempty"`
	SourceBytesIncluded  json.RawMessage         `json:"source_bytes_included,omitempty"`
	StartedAt            json.RawMessage         `json:"started_at,omitempty"`
	FinishedAt           json.RawMessage         `json:"finished_at,omitempty"`
	DurationMillis       json.RawMessage         `json:"duration_millis,omitempty"`
}

type ownershipManifestFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
	Bytes     *int64 `json:"bytes,omitempty"`
	Canonical *bool  `json:"canonical,omitempty"`
}

// validateOwnedOutput makes marker text alone insufficient for destructive
// force replacement. The exact-kind marker must agree with a bounded manifest,
// and every manifested file must be regular, contained, complete, and digest
// valid with no unmanifested payload files.
func validateOwnedOutput(target, kind string, identity os.FileInfo) error {
	markerPath := filepath.Join(target, MarkerName)
	markerIdentity, err := os.Lstat(markerPath)
	if err != nil || !markerIdentity.Mode().IsRegular() {
		return errors.New("ownership marker identity is unavailable")
	}
	marker, err := ReadMarker(target)
	if err != nil {
		return fmt.Errorf("invalid ownership marker: %w", err)
	}
	if marker.Kind != kind || strings.TrimSpace(marker.SnapshotID) == "" {
		return errors.New("ownership marker kind or snapshot does not match")
	}
	manifestName := ""
	switch kind {
	case "atlas":
		manifestName = "rkc-export-manifest.json"
	case "synthesis":
		manifestName = "manifest.json"
	default:
		return fmt.Errorf("unsupported force-replacement kind %q", kind)
	}
	manifestPath := filepath.Join(target, manifestName)
	manifestIdentity, err := os.Lstat(manifestPath)
	if err != nil || !manifestIdentity.Mode().IsRegular() {
		return errors.New("ownership manifest identity is unavailable")
	}
	data, err := readBoundedRegular(manifestPath, ownershipManifestMaxSize)
	if err != nil {
		return fmt.Errorf("read ownership manifest: %w", err)
	}
	var manifest ownershipManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode ownership manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode ownership manifest: trailing JSON is not permitted")
	}
	if manifest.SchemaVersion != outputManifestVersion || manifest.SnapshotID != marker.SnapshotID {
		return errors.New("ownership manifest does not match marker snapshot")
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > ownershipManifestMaxFiles {
		return errors.New("ownership manifest file count is outside safety limits")
	}
	expected := make(map[string]ownershipManifestFile, len(manifest.Files))
	var expectedBytes int64
	for _, file := range manifest.Files {
		if !canonicalManifestPath(file.Path) || file.Path == MarkerName || file.Path == manifestName {
			return fmt.Errorf("ownership manifest contains unsafe path %q", file.Path)
		}
		if _, duplicate := expected[file.Path]; duplicate {
			return fmt.Errorf("ownership manifest repeats path %q", file.Path)
		}
		decoded, err := hex.DecodeString(file.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("ownership manifest has invalid digest for %q", file.Path)
		}
		size, err := manifestFileSize(kind, file)
		if err != nil {
			return fmt.Errorf("ownership manifest has invalid size for %q: %w", file.Path, err)
		}
		if size > ownershipManifestMaxBytes-expectedBytes {
			return errors.New("ownership manifest byte count exceeds safety limit")
		}
		expectedBytes += size
		expected[file.Path] = file
	}

	seen := make(map[string]struct{}, len(expected))
	pathIdentities := make(map[string]os.FileInfo, len(expected)+2)
	walked := 0
	err = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == target {
			return nil
		}
		walked++
		if walked > ownershipManifestMaxFiles+2 {
			return errors.New("output path count exceeds ownership safety limit")
		}
		relative, err := filepath.Rel(target, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			info, err := os.Lstat(path)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("output directory %q identity is invalid", relative)
			}
			pathIdentities[relative] = info
			return nil
		}
		if relative == MarkerName || relative == manifestName {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("ownership metadata %q identity is invalid", relative)
			}
			pathIdentities[relative] = info
			return nil
		}
		file, present := expected[relative]
		if !present {
			return fmt.Errorf("unmanifested output file %q", relative)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("output path %q is not a regular file", relative)
		}
		size, _ := manifestFileSize(kind, file)
		if info.Size() != size {
			return fmt.Errorf("output size mismatch for %q", relative)
		}
		digest, err := hashRegularFile(path, info, ownershipManifestMaxBytes)
		if err != nil {
			return fmt.Errorf("hash output file %q: %w", relative, err)
		}
		if !strings.EqualFold(digest, file.SHA256) {
			return fmt.Errorf("output digest mismatch for %q", relative)
		}
		seen[relative] = struct{}{}
		pathIdentities[relative] = info
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("ownership manifest references missing output files")
	}
	if current, statErr := os.Lstat(markerPath); statErr != nil || !os.SameFile(markerIdentity, current) {
		return errors.New("ownership marker pathname identity changed")
	}
	if current, statErr := os.Lstat(manifestPath); statErr != nil || !os.SameFile(manifestIdentity, current) {
		return errors.New("ownership manifest pathname identity changed")
	}
	secondManifest, err := readBoundedRegular(manifestPath, ownershipManifestMaxSize)
	if err != nil || !bytes.Equal(data, secondManifest) {
		return errors.New("ownership manifest changed during output validation")
	}
	secondMarker, err := ReadMarker(target)
	if err != nil || secondMarker != marker {
		return errors.New("ownership marker changed during output validation")
	}
	for relative, earlier := range pathIdentities {
		path := filepath.Join(target, filepath.FromSlash(relative))
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(earlier, current) {
			return fmt.Errorf("output pathname identity changed for %q", relative)
		}
		if file, present := expected[relative]; present {
			digest, err := hashRegularFile(path, current, ownershipManifestMaxBytes)
			if err != nil || !strings.EqualFold(digest, file.SHA256) {
				return fmt.Errorf("output content changed during final recheck for %q", relative)
			}
		}
	}
	final, err := os.Lstat(target)
	if err != nil || !final.IsDir() || !os.SameFile(identity, final) {
		return errors.New("output directory changed during ownership validation")
	}
	return nil
}

func manifestFileSize(kind string, file ownershipManifestFile) (int64, error) {
	var size *int64
	if kind == "atlas" && file.SizeBytes != nil && file.Bytes == nil {
		size = file.SizeBytes
	}
	if kind == "synthesis" && file.Bytes != nil && file.SizeBytes == nil {
		size = file.Bytes
	}
	if size == nil || *size < 0 || *size > ownershipManifestMaxBytes {
		return 0, errors.New("missing or out-of-range size")
	}
	return *size, nil
}

func canonicalManifestPath(path string) bool {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	native := filepath.FromSlash(path)
	clean := filepath.Clean(native)
	return clean != "." && !filepath.IsAbs(clean) && filepath.ToSlash(clean) == path &&
		clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func hashRegularFile(path string, identity os.FileInfo, maximum int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(identity, opened) || opened.Size() > maximum {
		return "", errors.New("file identity changed while opening")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != size || size > maximum {
		return "", errors.New("file changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readBoundedRegular(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maximum {
		return nil, errors.New("file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() > maximum {
		return nil, errors.New("file identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != int64(len(data)) || int64(len(data)) > maximum {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

// validateOwnedDirectory checks the exact directory inode and marker kind.
// Callers invoke it again at the final recursive-deletion boundary so an
// earlier ownership decision can never authorize deletion of a replacement.
func validateOwnedDirectory(path string, identity os.FileInfo, sentinel error, kinds ...string) error {
	if identity == nil {
		return fmt.Errorf("%w: missing directory identity", sentinel)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || !os.SameFile(identity, info) {
		return fmt.Errorf("%w: directory identity changed", sentinel)
	}
	marker, err := ReadMarker(path)
	if err != nil {
		return fmt.Errorf("%w: refusing to delete unmarked directory", sentinel)
	}
	for _, kind := range kinds {
		if marker.Kind == kind {
			// A transaction may abort its own exact staging inode after final
			// marker publication even when manifest validation caused Commit to
			// fail. Destruction of an existing/prior output always requires the
			// complete manifest and path revalidation below.
			if (kind == "atlas" || kind == "synthesis") && !errors.Is(sentinel, ErrInvalidStaging) {
				if err := validateOwnedOutput(path, kind, identity); err != nil {
					return fmt.Errorf("%w: complete ownership validation failed: %v", sentinel, err)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("%w: directory marker kind changed", sentinel)
}

func removeOwnedDirectory(path string, identity os.FileInfo, sentinel error, kinds ...string) error {
	quarantine, err := quarantineOwnedDirectory(path, identity, sentinel, kinds...)
	if err != nil {
		return err
	}
	return quarantine.remove(sentinel, kinds...)
}

type replacementJournalRecord struct {
	SchemaVersion       string `json:"schema_version"`
	TargetName          string `json:"target_name"`
	StagingName         string `json:"staging_name"`
	Kind                string `json:"kind"`
	NewSnapshot         string `json:"new_snapshot_id"`
	PriorSnapshot       string `json:"prior_snapshot_id"`
	NewIdentity         string `json:"new_identity"`
	PriorIdentity       string `json:"prior_identity"`
	NewMarkerSHA256     string `json:"new_marker_sha256"`
	NewManifestSHA256   string `json:"new_manifest_sha256"`
	PriorMarkerSHA256   string `json:"prior_marker_sha256"`
	PriorManifestSHA256 string `json:"prior_manifest_sha256"`
	Phase               string `json:"phase"`
}

type replacementJournal struct {
	root            string
	path            string
	rootIdentity    os.FileInfo
	journalIdentity os.FileInfo
	record          replacementJournalRecord
}

func createReplacementJournal(transaction *Transaction, priorIdentity os.FileInfo, snapshotID string) (*replacementJournal, error) {
	priorMarker, err := ReadMarker(transaction.Target)
	if err != nil {
		return nil, err
	}
	newToken, err := persistentIdentityToken(transaction.identity)
	if err != nil {
		return nil, err
	}
	priorToken, err := persistentIdentityToken(priorIdentity)
	if err != nil {
		return nil, err
	}
	newMarkerDigest, newManifestDigest, err := ownershipMetadataDigests(transaction.Staging, transaction.kind)
	if err != nil {
		return nil, err
	}
	priorMarkerDigest, priorManifestDigest, err := ownershipMetadataDigests(transaction.Target, transaction.kind)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(filepath.Dir(transaction.Target), ".rkc-quarantine-")
	if err != nil {
		return nil, err
	}
	rootIdentity, err := os.Lstat(root)
	if err != nil || !rootIdentity.IsDir() || rootIdentity.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(root)
		return nil, errors.New("replacement journal root is not an owner-only directory")
	}
	journal := &replacementJournal{
		root: root, path: filepath.Join(root, journalName), rootIdentity: rootIdentity,
		record: replacementJournalRecord{
			SchemaVersion: journalVersion,
			TargetName:    filepath.Base(transaction.Target), StagingName: filepath.Base(transaction.Staging),
			Kind: transaction.kind, NewSnapshot: snapshotID, PriorSnapshot: priorMarker.SnapshotID,
			NewIdentity: newToken, PriorIdentity: priorToken, Phase: "prepared",
			NewMarkerSHA256: newMarkerDigest, NewManifestSHA256: newManifestDigest,
			PriorMarkerSHA256: priorMarkerDigest, PriorManifestSHA256: priorManifestDigest,
		},
	}
	if err := journal.persist(); err != nil {
		_ = os.Remove(journal.path)
		_ = os.Remove(root)
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(transaction.Target)); err != nil {
		return nil, err
	}
	return journal, nil
}

func ownershipMetadataDigests(root, kind string) (string, string, error) {
	manifestName := ""
	switch kind {
	case "atlas":
		manifestName = "rkc-export-manifest.json"
	case "synthesis":
		manifestName = "manifest.json"
	default:
		return "", "", errors.New("unsupported ownership kind")
	}
	marker, err := readBoundedRegular(filepath.Join(root, MarkerName), markerMaxSize)
	if err != nil {
		return "", "", err
	}
	manifest, err := readBoundedRegular(filepath.Join(root, manifestName), ownershipManifestMaxSize)
	if err != nil {
		return "", "", err
	}
	markerSum := sha256.Sum256(marker)
	manifestSum := sha256.Sum256(manifest)
	return hex.EncodeToString(markerSum[:]), hex.EncodeToString(manifestSum[:]), nil
}

func validateJournalBoundOutput(path, kind string, identity os.FileInfo, snapshotID, markerDigest, manifestDigest string) error {
	if err := validatePublishedOutput(path, kind, identity, snapshotID); err != nil {
		return err
	}
	actualMarker, actualManifest, err := ownershipMetadataDigests(path, kind)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actualMarker, markerDigest) || !strings.EqualFold(actualManifest, manifestDigest) {
		return errors.New("ownership metadata digest does not match durable replacement journal")
	}
	return nil
}

func (journal *replacementJournal) update(phase string) error {
	switch phase {
	case "prepared", "exchanged", "prior-quarantined":
	default:
		return errors.New("invalid replacement journal phase")
	}
	journal.record.Phase = phase
	return journal.persist()
}

func (journal *replacementJournal) persist() error {
	if journal == nil || journal.rootIdentity == nil {
		return errors.New("missing replacement journal")
	}
	root, err := os.Lstat(journal.root)
	if err != nil || !root.IsDir() || !os.SameFile(journal.rootIdentity, root) || root.Mode().Perm()&0o077 != 0 {
		return errors.New("replacement journal root identity changed")
	}
	data, err := json.MarshalIndent(journal.record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(journal.root, ".journal-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, journal.path); err != nil {
		return err
	}
	committed = true
	journal.journalIdentity, err = os.Lstat(journal.path)
	if err != nil || !journal.journalIdentity.Mode().IsRegular() {
		return errors.New("replacement journal identity unavailable after publication")
	}
	return syncDirectory(journal.root)
}

func (journal *replacementJournal) discard() error {
	if journal == nil {
		return nil
	}
	if journal.journalIdentity != nil {
		current, err := os.Lstat(journal.path)
		if err != nil || !os.SameFile(journal.journalIdentity, current) {
			return errors.New("replacement journal identity changed before cleanup")
		}
		if err := os.Remove(journal.path); err != nil {
			return err
		}
	}
	root, err := os.Lstat(journal.root)
	if err != nil || !os.SameFile(journal.rootIdentity, root) {
		return errors.New("replacement journal root changed before cleanup")
	}
	if err := os.Remove(journal.root); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(journal.root))
}

func (journal *replacementJournal) quarantine(identity os.FileInfo) *quarantinedDirectory {
	return &quarantinedDirectory{
		root: journal.root, payload: filepath.Join(journal.root, "payload"),
		rootIdentity: journal.rootIdentity, identity: identity, journalIdentity: journal.journalIdentity,
	}
}

func persistentIdentityToken(info os.FileInfo) (string, error) {
	if info == nil || info.Sys() == nil {
		return "", errors.New("filesystem identity is unavailable")
	}
	value := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return "", errors.New("filesystem identity is unavailable")
	}
	device, deviceOK := numericIdentityField(value.FieldByName("Dev"))
	inode, inodeOK := numericIdentityField(value.FieldByName("Ino"))
	if !deviceOK || !inodeOK {
		return "", errors.New("durable filesystem identity is unsupported on this platform")
	}
	return fmt.Sprintf("%x:%x", device, inode), nil
}

func numericIdentityField(value reflect.Value) (uint64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(value.Int()), true
	default:
		return 0, false
	}
}

func recoverInterruptedReplacements(parent, targetName string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	seen := 0
	matched := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".rkc-quarantine-") {
			continue
		}
		seen++
		if seen > journalScanLimit {
			return errors.New("replacement journal scan limit exceeded")
		}
		journal, err := loadReplacementJournal(filepath.Join(parent, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if journal.record.TargetName != targetName {
			continue
		}
		matched++
		if matched > 1 {
			return errors.New("multiple interrupted publications exist for one target")
		}
		if err := recoverReplacement(parent, journal); err != nil {
			return err
		}
	}
	return nil
}

func loadReplacementJournal(root string) (*replacementJournal, error) {
	rootIdentity, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !rootIdentity.IsDir() || rootIdentity.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("replacement journal root is not an owner-only directory")
	}
	path := filepath.Join(root, journalName)
	journalIdentity, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !journalIdentity.Mode().IsRegular() {
		return nil, errors.New("replacement journal is not a regular file")
	}
	data, err := readBoundedRegular(path, journalMaxSize)
	if err != nil {
		return nil, err
	}
	var record replacementJournalRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("replacement journal contains trailing JSON")
	}
	if record.SchemaVersion != journalVersion || (record.Kind != "atlas" && record.Kind != "synthesis") ||
		!canonicalBaseName(record.TargetName) || !canonicalBaseName(record.StagingName) ||
		!strings.HasPrefix(record.StagingName, ".rkc-build-") || record.NewSnapshot == "" ||
		record.PriorSnapshot == "" || record.NewIdentity == "" || record.PriorIdentity == "" {
		return nil, errors.New("invalid replacement journal")
	}
	for _, digest := range []string{record.NewMarkerSHA256, record.NewManifestSHA256, record.PriorMarkerSHA256, record.PriorManifestSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("invalid replacement journal metadata digest")
		}
	}
	switch record.Phase {
	case "prepared", "exchanged", "prior-quarantined":
	default:
		return nil, errors.New("invalid replacement journal phase")
	}
	finalJournal, err := os.Lstat(path)
	if err != nil || !finalJournal.Mode().IsRegular() || !os.SameFile(journalIdentity, finalJournal) {
		return nil, errors.New("replacement journal identity changed while loading")
	}
	finalRoot, err := os.Lstat(root)
	if err != nil || !finalRoot.IsDir() || !os.SameFile(rootIdentity, finalRoot) {
		return nil, errors.New("replacement journal root changed while loading")
	}
	return &replacementJournal{root: root, path: path, rootIdentity: rootIdentity, journalIdentity: journalIdentity, record: record}, nil
}

func canonicalBaseName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func recoverReplacement(parent string, journal *replacementJournal) error {
	target := filepath.Join(parent, journal.record.TargetName)
	staging := filepath.Join(parent, journal.record.StagingName)
	payload := filepath.Join(journal.root, "payload")
	targetInfo, targetToken := inspectIdentityToken(target)
	stagingInfo, stagingToken := inspectIdentityToken(staging)
	payloadInfo, payloadToken := inspectIdentityToken(payload)

	if targetToken == journal.record.NewIdentity {
		if err := validateJournalBoundOutput(target, journal.record.Kind, targetInfo, journal.record.NewSnapshot, journal.record.NewMarkerSHA256, journal.record.NewManifestSHA256); err != nil {
			return fmt.Errorf("recovered target validation failed; journal retained at %s: %w", journal.root, err)
		}
		if stagingToken == journal.record.PriorIdentity {
			if err := validateJournalBoundOutput(staging, journal.record.Kind, stagingInfo, journal.record.PriorSnapshot, journal.record.PriorMarkerSHA256, journal.record.PriorManifestSHA256); err != nil {
				return err
			}
			if payloadInfo != nil {
				return errors.New("both staging and quarantine payload exist during recovery")
			}
			if err := os.Rename(staging, payload); err != nil {
				return err
			}
			payloadInfo = stagingInfo
			payloadToken = stagingToken
			if err := journal.update("prior-quarantined"); err != nil {
				return err
			}
		}
		if payloadToken == journal.record.PriorIdentity {
			if err := validateJournalBoundOutput(payload, journal.record.Kind, payloadInfo, journal.record.PriorSnapshot, journal.record.PriorMarkerSHA256, journal.record.PriorManifestSHA256); err != nil {
				return err
			}
			if err := validateJournalBoundOutput(target, journal.record.Kind, targetInfo, journal.record.NewSnapshot, journal.record.NewMarkerSHA256, journal.record.NewManifestSHA256); err != nil {
				return err
			}
			return journal.quarantine(payloadInfo).remove(ErrTargetUnowned, journal.record.Kind)
		}
		if stagingInfo == nil && payloadInfo == nil {
			return journal.discard()
		}
		return errors.New("interrupted publication prior-output identity is ambiguous")
	}

	if targetToken == journal.record.PriorIdentity && stagingToken == journal.record.NewIdentity && payloadInfo == nil {
		if err := validateJournalBoundOutput(target, journal.record.Kind, targetInfo, journal.record.PriorSnapshot, journal.record.PriorMarkerSHA256, journal.record.PriorManifestSHA256); err != nil {
			return err
		}
		if err := validateJournalBoundOutput(staging, journal.record.Kind, stagingInfo, journal.record.NewSnapshot, journal.record.NewMarkerSHA256, journal.record.NewManifestSHA256); err != nil {
			return err
		}
		if err := os.Rename(staging, payload); err != nil {
			return err
		}
		journal.record.Phase = "prior-quarantined"
		if err := journal.persist(); err != nil {
			return err
		}
		return journal.quarantine(stagingInfo).remove(ErrTargetUnowned, journal.record.Kind)
	}

	if targetInfo == nil && stagingToken == journal.record.NewIdentity && payloadToken == journal.record.PriorIdentity {
		if err := validateJournalBoundOutput(payload, journal.record.Kind, payloadInfo, journal.record.PriorSnapshot, journal.record.PriorMarkerSHA256, journal.record.PriorManifestSHA256); err != nil {
			return err
		}
		if err := renameNoReplaceOperation(payload, target); err != nil {
			return err
		}
		if err := validateJournalBoundOutput(target, journal.record.Kind, payloadInfo, journal.record.PriorSnapshot, journal.record.PriorMarkerSHA256, journal.record.PriorManifestSHA256); err != nil {
			return err
		}
		if err := os.Rename(staging, payload); err != nil {
			return err
		}
		return journal.quarantine(stagingInfo).remove(ErrTargetUnowned, journal.record.Kind)
	}
	return fmt.Errorf("interrupted publication identities are ambiguous; journal retained at %s", journal.root)
}

func inspectIdentityToken(path string) (os.FileInfo, string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return nil, ""
	}
	token, err := persistentIdentityToken(info)
	if err != nil {
		return info, ""
	}
	return info, token
}

// quarantinedDirectory binds recursive removal to the exact directory inode
// validated before an atomic rename beneath a fresh owner-only parent. A path
// replacement can make quarantine fail, but cannot redirect RemoveAll to the
// replacement path.
type quarantinedDirectory struct {
	root            string
	payload         string
	rootIdentity    os.FileInfo
	identity        os.FileInfo
	journalIdentity os.FileInfo
}

func quarantineOwnedDirectory(path string, identity os.FileInfo, sentinel error, kinds ...string) (*quarantinedDirectory, error) {
	if err := validateOwnedDirectory(path, identity, sentinel, kinds...); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(filepath.Dir(path), ".rkc-quarantine-")
	if err != nil {
		return nil, fmt.Errorf("%w: create private quarantine: %v", sentinel, err)
	}
	rootIdentity, err := os.Lstat(root)
	if err != nil || !rootIdentity.IsDir() || rootIdentity.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(root)
		return nil, fmt.Errorf("%w: quarantine is not an owner-only directory", sentinel)
	}
	payload := filepath.Join(root, "payload")
	if err := os.Rename(path, payload); err != nil {
		_ = os.Remove(root)
		return nil, fmt.Errorf("%w: move directory into quarantine: %v", sentinel, err)
	}
	quarantine := &quarantinedDirectory{
		root: root, payload: payload, rootIdentity: rootIdentity, identity: identity,
	}
	if err := quarantine.validate(sentinel, kinds...); err != nil {
		if restoreErr := quarantine.restore(path); restoreErr != nil {
			return nil, fmt.Errorf("%w; mismatched directory retained at %s after rollback failed: %v", err, payload, restoreErr)
		}
		return nil, err
	}
	return quarantine, nil
}

func (quarantine *quarantinedDirectory) validate(sentinel error, kinds ...string) error {
	if quarantine == nil || quarantine.rootIdentity == nil || quarantine.identity == nil {
		return fmt.Errorf("%w: missing quarantine identity", sentinel)
	}
	root, err := os.Lstat(quarantine.root)
	if err != nil || !root.IsDir() || root.Mode().Perm()&0o077 != 0 || !os.SameFile(quarantine.rootIdentity, root) {
		return fmt.Errorf("%w: quarantine identity changed", sentinel)
	}
	return validateOwnedDirectory(quarantine.payload, quarantine.identity, sentinel, kinds...)
}

func (quarantine *quarantinedDirectory) restore(target string) error {
	if quarantine == nil || quarantine.rootIdentity == nil || quarantine.identity == nil {
		return errors.New("missing quarantine")
	}
	root, err := os.Lstat(quarantine.root)
	if err != nil || !root.IsDir() || !os.SameFile(quarantine.rootIdentity, root) {
		return errors.New("quarantine identity changed before restore")
	}
	payload, err := os.Lstat(quarantine.payload)
	if err != nil || !payload.IsDir() || !os.SameFile(quarantine.identity, payload) {
		return errors.New("quarantined directory identity changed before restore")
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("restore target already exists: %s", target)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect restore target: %w", err)
	}
	if err := renameNoReplaceOperation(quarantine.payload, target); err != nil {
		return err
	}
	restored, err := os.Lstat(target)
	if err != nil || !os.SameFile(quarantine.identity, restored) {
		return errors.New("restored directory identity changed")
	}
	return removeEmptyQuarantine(quarantine)
}

func (quarantine *quarantinedDirectory) remove(sentinel error, kinds ...string) error {
	if err := quarantine.validate(sentinel, kinds...); err != nil {
		return err
	}
	if err := os.RemoveAll(quarantine.payload); err != nil {
		return fmt.Errorf("remove quarantined directory: %w; retained at %s", err, quarantine.payload)
	}
	return removeEmptyQuarantine(quarantine)
}

func removeEmptyQuarantine(quarantine *quarantinedDirectory) error {
	root, err := os.Lstat(quarantine.root)
	if err != nil || !root.IsDir() || !os.SameFile(quarantine.rootIdentity, root) {
		return errors.New("quarantine identity changed before final removal")
	}
	if quarantine.journalIdentity != nil {
		journalPath := filepath.Join(quarantine.root, journalName)
		journal, err := os.Lstat(journalPath)
		if err != nil || !journal.Mode().IsRegular() || !os.SameFile(quarantine.journalIdentity, journal) {
			return errors.New("quarantine journal identity changed before final removal")
		}
		if err := os.Remove(journalPath); err != nil {
			return fmt.Errorf("remove quarantine journal: %w", err)
		}
	}
	if err := os.Remove(quarantine.root); err != nil {
		return fmt.Errorf("remove empty quarantine: %w", err)
	}
	return syncDirectory(filepath.Dir(quarantine.root))
}

func writeMarker(root string, marker Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(root, ".rkc-marker-")
	if err != nil {
		return fmt.Errorf("create RKC output marker: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(root, MarkerName)); err != nil {
		return err
	}
	committed = true
	return nil
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil || relative == "." {
		return relative == "."
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
