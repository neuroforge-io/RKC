package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	journalSchemaVersion       = "rkc.scheduler-journal.v1"
	maximumJournalRecordBytes  = int64(1 << 20)
	maximumJournalFileBytes    = int64(64 << 20)
	journalTerminalSyncTimeout = 5 * time.Second
	zeroJournalDigest          = "0000000000000000000000000000000000000000000000000000000000000000"
)

// FileJournal is a durable, append-only record of one scheduler run. Each
// record is flushed before Append returns so a killed process leaves a
// replayable prefix rather than an optimistic in-memory history.
type FileJournal struct {
	gate chan struct{}

	root         string
	path         string
	file         *os.File
	rootIdentity os.FileInfo
	fileIdentity os.FileInfo
	runID        string
	sequence     uint64
	planDigest   string
	lastDigest   string
	lifecycle    journalLifecycle
	failed       error
	closed       bool
}

// JournalReport is the strictly validated replay of one run journal.
type JournalReport struct {
	Path               string          `json:"path"`
	RunID              string          `json:"run_id"`
	State              JournalState    `json:"state"`
	Terminal           bool            `json:"terminal"`
	Interrupted        bool            `json:"interrupted"`
	DiscardedTailBytes int64           `json:"discarded_tail_bytes,omitempty"`
	LastSequence       uint64          `json:"last_sequence"`
	Records            []JournalRecord `json:"records"`
}

// NewRunID returns an unguessable, path-safe identifier for one scheduler run.
func NewRunID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("generate scheduler run ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// ValidateRunID accepts only the canonical 128-bit lowercase hexadecimal
// representation produced by NewRunID.
func ValidateRunID(runID string) error {
	if !isLowerHex(runID, 32) {
		return errors.New("scheduler run ID must be 32 lowercase hexadecimal characters")
	}
	return nil
}

// ValidateJournalRootPrivacy verifies that path is a stable, non-symlink
// directory protected according to the native platform policy. Unix requires
// owner-only permission bits. Windows requires a protected DACL whose only
// access-control entry grants the current user full control.
func ValidateJournalRootPrivacy(path string) error {
	return validateJournalPathPrivacy(path, true)
}

// ValidateJournalFilePrivacy verifies that path is a stable, non-symlink
// regular file protected according to the native platform policy.
func ValidateJournalFilePrivacy(path string) error {
	return validateJournalPathPrivacy(path, false)
}

func validateJournalPathPrivacy(path string, directory bool) error {
	if path == "" || path != strings.TrimSpace(path) {
		return errors.New("scheduler journal path is required without surrounding whitespace")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve scheduler journal path: %w", err)
	}
	if err := rejectFileCacheSymlinks(absolute); err != nil {
		return fmt.Errorf("validate scheduler journal path: %w", err)
	}
	identity, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("stat scheduler journal path: %w", err)
	}
	if identity.Mode()&os.ModeSymlink != 0 ||
		identity.IsDir() != directory ||
		!directory && !identity.Mode().IsRegular() {
		if directory {
			return errors.New("scheduler journal root is not a non-symlink directory")
		}
		return errors.New("scheduler journal is not a non-symlink regular file")
	}
	if directory {
		err = validateJournalRootPrivacy(absolute, identity)
	} else {
		err = validateJournalFilePrivacy(absolute, identity)
	}
	if err != nil {
		return err
	}
	current, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(identity, current) {
		return errors.New("scheduler journal identity changed while validating privacy")
	}
	return nil
}

// OpenFileJournal atomically creates a new journal. Existing run IDs are never
// overwritten or resumed implicitly.
func OpenFileJournal(root, runID string) (*FileJournal, error) {
	if err := ValidateRunID(runID); err != nil {
		return nil, err
	}
	if root == "" || root != strings.TrimSpace(root) {
		return nil, errors.New("scheduler journal root is required without surrounding whitespace")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve scheduler journal root: %w", err)
	}
	if err := createJournalRoot(absolute); err != nil {
		return nil, fmt.Errorf("create scheduler journal root: %w", err)
	}
	if err := rejectFileCacheSymlinks(absolute); err != nil {
		return nil, fmt.Errorf("validate scheduler journal root: %w", err)
	}
	rootIdentity, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat scheduler journal root: %w", err)
	}
	if rootIdentity.Mode()&os.ModeSymlink != 0 || !rootIdentity.IsDir() {
		return nil, errors.New("scheduler journal root is not a non-symlink directory")
	}
	if err := secureJournalRoot(absolute, rootIdentity); err != nil {
		return nil, fmt.Errorf("secure scheduler journal root: %w", err)
	}
	path := filepath.Join(absolute, runID+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create scheduler journal: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	fileIdentity, err := file.Stat()
	if err != nil || !fileIdentity.Mode().IsRegular() {
		return nil, errors.New("scheduler journal is not a regular file")
	}
	if err := secureJournalFile(path, fileIdentity); err != nil {
		return nil, fmt.Errorf("secure scheduler journal file: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.Mode().IsRegular() || !os.SameFile(fileIdentity, current) {
		return nil, errors.New("scheduler journal identity changed during creation")
	}
	if err := syncStableJournalDirectory(absolute, rootIdentity); err != nil {
		return nil, fmt.Errorf("sync scheduler journal root: %w", err)
	}
	keep = true
	journal := &FileJournal{
		gate:         make(chan struct{}, 1),
		root:         absolute,
		path:         path,
		file:         file,
		rootIdentity: rootIdentity,
		fileIdentity: fileIdentity,
		runID:        runID,
		lastDigest:   zeroJournalDigest,
	}
	journal.gate <- struct{}{}
	return journal, nil
}

// Path returns the absolute bound journal pathname, or an empty string for a
// nil journal. The path is informational and does not confer file authority.
func (journal *FileJournal) Path() string {
	if journal == nil {
		return ""
	}
	return journal.path
}

// RunID returns the validated run identity, or an empty string for a nil
// journal.
func (journal *FileJournal) RunID() string {
	if journal == nil {
		return ""
	}
	return journal.runID
}

// Append persists one record and does not return until it is durable.
func (journal *FileJournal) Append(ctx context.Context, record JournalRecord) error {
	if journal == nil {
		return errors.New("scheduler journal is nil")
	}
	if journal.gate == nil {
		return errors.New("scheduler journal is not initialized")
	}
	if ctx == nil {
		return errors.New("scheduler journal context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := journal.lock(ctx); err != nil {
		return err
	}
	defer journal.unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if journal.closed {
		return errors.New("scheduler journal is closed")
	}
	if journal.failed != nil {
		return fmt.Errorf("scheduler journal previously failed: %w", journal.failed)
	}
	if err := journal.validateIdentity(); err != nil {
		journal.failed = err
		return err
	}
	if record.RunID != journal.runID {
		return errors.New("scheduler journal record run ID does not match journal")
	}
	record.SchemaVersion = journalSchemaVersion
	record.Sequence = journal.sequence + 1
	record.OccurredAt = time.Now().UTC()
	if record.Sequence == 1 {
		digest, err := JournalPlanDigest(record.Plan)
		if err != nil {
			return fmt.Errorf("digest scheduler journal plan: %w", err)
		}
		if record.PlanDigest != "" && record.PlanDigest != digest {
			return errors.New("scheduler journal plan digest does not match plan")
		}
		record.PlanDigest = digest
	} else {
		if record.PlanDigest == "" {
			record.PlanDigest = journal.planDigest
		}
		if record.PlanDigest != journal.planDigest {
			return errors.New("scheduler journal plan digest changed")
		}
	}
	if record.PreviousRecordDigest == "" {
		record.PreviousRecordDigest = journal.lastDigest
	}
	if record.PreviousRecordDigest != journal.lastDigest {
		return errors.New("scheduler journal previous record digest does not match")
	}
	if record.RecordDigest != "" {
		return errors.New("scheduler journal record digest is assigned by the journal")
	}
	digest, err := journalRecordDigest(record)
	if err != nil {
		return fmt.Errorf("digest scheduler journal record: %w", err)
	}
	record.RecordDigest = digest
	if err := validateJournalRecord(record); err != nil {
		return err
	}
	nextLifecycle := journal.lifecycle.clone()
	if err := nextLifecycle.accept(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode scheduler journal record: %w", err)
	}
	data = append(data, '\n')
	if int64(len(data)) > maximumJournalRecordBytes {
		return fmt.Errorf("scheduler journal record exceeds %d bytes", maximumJournalRecordBytes)
	}
	current, err := journal.file.Stat()
	if err != nil {
		journal.failed = err
		return fmt.Errorf("stat scheduler journal: %w", err)
	}
	if current.Size()+int64(len(data)) > maximumJournalFileBytes {
		return fmt.Errorf("scheduler journal exceeds %d bytes", maximumJournalFileBytes)
	}
	if err := writeJournalRecord(journal.file, data); err != nil {
		journal.failed = err
		return fmt.Errorf("append scheduler journal: %w", err)
	}
	if err := journal.file.Sync(); err != nil {
		journal.failed = err
		return fmt.Errorf("sync scheduler journal: %w", err)
	}
	if err := journal.validateIdentity(); err != nil {
		journal.failed = err
		return err
	}
	journal.sequence = record.Sequence
	journal.planDigest = record.PlanDigest
	journal.lastDigest = record.RecordDigest
	journal.lifecycle = nextLifecycle
	return nil
}

// Close serializes with Append, closes the journal once, and joins any earlier
// durable-write failure with the close error. Repeated calls are idempotent.
func (journal *FileJournal) Close() error {
	if journal == nil {
		return nil
	}
	if journal.gate == nil {
		return journal.failed
	}
	<-journal.gate
	defer journal.unlock()
	if journal.closed {
		return nil
	}
	journal.closed = true
	var err error
	if journal.file != nil {
		err = journal.file.Close()
	}
	return errors.Join(journal.failed, err)
}

func (journal *FileJournal) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-journal.gate:
		return nil
	}
}

func (journal *FileJournal) unlock() {
	journal.gate <- struct{}{}
}

func (journal *FileJournal) validateIdentity() error {
	if journal.file == nil || journal.rootIdentity == nil || journal.fileIdentity == nil {
		return errors.New("scheduler journal identity is missing")
	}
	root, err := os.Lstat(journal.root)
	if err != nil || root.Mode()&os.ModeSymlink != 0 || !root.IsDir() ||
		!os.SameFile(journal.rootIdentity, root) {
		return errors.New("scheduler journal root identity changed")
	}
	if err := validateJournalRootPrivacy(journal.root, root); err != nil {
		return err
	}
	opened, err := journal.file.Stat()
	if err != nil || !opened.Mode().IsRegular() ||
		!os.SameFile(journal.fileIdentity, opened) {
		return errors.New("scheduler journal file identity changed")
	}
	current, err := os.Lstat(journal.path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.Mode().IsRegular() || !os.SameFile(journal.fileIdentity, current) {
		return errors.New("scheduler journal pathname identity changed")
	}
	if err := validateJournalFilePrivacy(journal.path, current); err != nil {
		return err
	}
	return nil
}

func writeJournalRecord(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

// ReadFileJournal replays and strictly validates a journal. A non-terminal
// report is a trustworthy indication that the producing process exited before
// recording the run outcome.
func ReadFileJournal(path string) (JournalReport, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return JournalReport{}, fmt.Errorf("resolve scheduler journal: %w", err)
	}
	if err := rejectFileCacheSymlinks(absolute); err != nil {
		return JournalReport{}, fmt.Errorf("validate scheduler journal path: %w", err)
	}
	file, identity, err := openStableCacheFile(absolute)
	if err != nil {
		return JournalReport{}, fmt.Errorf("open scheduler journal: %w", err)
	}
	defer file.Close()
	if err := validateJournalFilePrivacy(absolute, identity); err != nil {
		return JournalReport{}, err
	}
	parent := filepath.Dir(absolute)
	parentIdentity, err := os.Lstat(parent)
	if err != nil || parentIdentity.Mode()&os.ModeSymlink != 0 || !parentIdentity.IsDir() {
		return JournalReport{}, errors.New("scheduler journal parent is not a non-symlink directory")
	}
	if err := validateJournalRootPrivacy(parent, parentIdentity); err != nil {
		return JournalReport{}, err
	}
	if identity.Size() > maximumJournalFileBytes {
		return JournalReport{}, fmt.Errorf("scheduler journal exceeds %d bytes", maximumJournalFileBytes)
	}
	runID := strings.TrimSuffix(filepath.Base(absolute), ".jsonl")
	if filepath.Ext(absolute) != ".jsonl" {
		return JournalReport{}, errors.New("scheduler journal must use a .jsonl filename")
	}
	if err := ValidateRunID(runID); err != nil {
		return JournalReport{}, err
	}
	report := JournalReport{Path: absolute, RunID: runID}
	var lifecycle journalLifecycle
	lastDigest := zeroJournalDigest
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				if !errors.Is(readErr, io.EOF) {
					return JournalReport{}, errors.New("scheduler journal record is not newline terminated")
				}
				if report.Terminal {
					return JournalReport{}, errors.New("scheduler journal contains a truncated record after termination")
				}
				if int64(len(line)) > maximumJournalRecordBytes {
					return JournalReport{}, fmt.Errorf(
						"scheduler journal truncated record exceeds %d bytes",
						maximumJournalRecordBytes,
					)
				}
				report.Interrupted = true
				report.DiscardedTailBytes = int64(len(line))
				break
			}
			if int64(len(line)) > maximumJournalRecordBytes {
				return JournalReport{}, fmt.Errorf(
					"scheduler journal record exceeds %d bytes",
					maximumJournalRecordBytes,
				)
			}
			record, err := decodeJournalRecord(bytes.TrimSuffix(line, []byte{'\n'}))
			if err != nil {
				return JournalReport{}, fmt.Errorf(
					"decode scheduler journal sequence %d: %w",
					len(report.Records)+1,
					err,
				)
			}
			if record.RunID != runID {
				return JournalReport{}, errors.New("scheduler journal run ID does not match its filename")
			}
			wantSequence := uint64(len(report.Records) + 1)
			if record.Sequence != wantSequence {
				return JournalReport{}, fmt.Errorf(
					"scheduler journal sequence is %d, want %d",
					record.Sequence,
					wantSequence,
				)
			}
			if report.Terminal {
				return JournalReport{}, errors.New("scheduler journal contains records after its terminal run record")
			}
			if record.PreviousRecordDigest != lastDigest {
				return JournalReport{}, errors.New("scheduler journal record hash chain is broken")
			}
			digest, err := journalRecordDigest(record)
			if err != nil {
				return JournalReport{}, fmt.Errorf("digest scheduler journal record: %w", err)
			}
			if record.RecordDigest != digest {
				return JournalReport{}, errors.New("scheduler journal record digest does not match its content")
			}
			if err := lifecycle.accept(record); err != nil {
				return JournalReport{}, fmt.Errorf(
					"validate scheduler journal sequence %d: %w",
					record.Sequence,
					err,
				)
			}
			report.Records = append(report.Records, record)
			report.LastSequence = record.Sequence
			lastDigest = record.RecordDigest
			if record.Kind == JournalKindRun {
				report.State = record.State
				report.Terminal = isTerminalRunState(record.State)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return JournalReport{}, fmt.Errorf("read scheduler journal: %w", readErr)
		}
	}
	if len(report.Records) == 0 {
		if report.Interrupted {
			return JournalReport{}, errors.New("scheduler journal has no complete run-plan record")
		}
		return JournalReport{}, errors.New("scheduler journal is empty")
	}
	if err := validateStableCacheRead(absolute, file, identity, identity.Size()); err != nil {
		return JournalReport{}, fmt.Errorf("validate scheduler journal replay: %w", err)
	}
	currentParent, err := os.Lstat(parent)
	if err != nil || currentParent.Mode()&os.ModeSymlink != 0 ||
		!currentParent.IsDir() || !os.SameFile(parentIdentity, currentParent) {
		return JournalReport{}, errors.New("scheduler journal parent identity changed while reading")
	}
	if err := validateJournalRootPrivacy(parent, currentParent); err != nil {
		return JournalReport{}, err
	}
	if err := validateJournalFilePrivacy(absolute, identity); err != nil {
		return JournalReport{}, err
	}
	return report, nil
}

func decodeJournalRecord(data []byte) (JournalRecord, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return JournalRecord{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record JournalRecord
	if err := decoder.Decode(&record); err != nil {
		return JournalRecord{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return JournalRecord{}, err
	}
	if err := validateJournalRecord(record); err != nil {
		return JournalRecord{}, err
	}
	return record, nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("scheduler journal object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("scheduler journal record contains duplicate field %q", key)
				}
				seen[key] = struct{}{}
				if err := consume(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return errors.New("scheduler journal object is not terminated")
			}
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return errors.New("scheduler journal array is not terminated")
			}
		default:
			return errors.New("scheduler journal record begins with an invalid delimiter")
		}
		return nil
	}
	if err := consume(); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("scheduler journal record contains trailing JSON content")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("scheduler journal record contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateJournalRecord(record JournalRecord) error {
	if record.SchemaVersion != journalSchemaVersion {
		return fmt.Errorf("unsupported scheduler journal schema %q", record.SchemaVersion)
	}
	if err := ValidateRunID(record.RunID); err != nil {
		return err
	}
	if record.Sequence == 0 || record.OccurredAt.IsZero() {
		return errors.New("scheduler journal sequence and occurrence time are required")
	}
	if !isLowerHex(record.PlanDigest, sha256.Size*2) {
		return errors.New("scheduler journal plan digest is invalid")
	}
	if !isLowerHex(record.PreviousRecordDigest, sha256.Size*2) {
		return errors.New("scheduler journal previous record digest is invalid")
	}
	if !isLowerHex(record.RecordDigest, sha256.Size*2) {
		return errors.New("scheduler journal record digest is invalid")
	}
	if record.Duration < 0 {
		return errors.New("scheduler journal duration cannot be negative")
	}
	if len(record.Error) > 64*1024 {
		return errors.New("scheduler journal error exceeds 65536 bytes")
	}
	switch record.Kind {
	case JournalKindRun:
		if record.Attempt != 0 || record.Resources != (ResourceRequest{}) {
			return errors.New("scheduler run journal record cannot contain attempt or resources")
		}
		if record.StageID != "" || record.StageVersion != "" ||
			record.CacheKey != "" || record.OutputDigest != "" {
			return errors.New("scheduler run journal record cannot contain stage fields")
		}
		switch record.State {
		case JournalStateRunning:
			if record.Duration != 0 || record.Error != "" {
				return errors.New("running scheduler journal record cannot contain duration or error")
			}
			if err := validateJournalPlan(record.Plan); err != nil {
				return err
			}
		case JournalStateSucceeded:
			if len(record.Plan) != 0 || record.Error != "" {
				return errors.New("successful scheduler journal record cannot contain a plan or error")
			}
		case JournalStateFailed, JournalStateCancelled:
			if len(record.Plan) != 0 || strings.TrimSpace(record.Error) == "" {
				return errors.New("failed scheduler journal record requires an error and cannot contain a plan")
			}
		default:
			return fmt.Errorf("invalid scheduler run state %q", record.State)
		}
	case JournalKindStage:
		if len(record.Plan) != 0 {
			return errors.New("scheduler stage journal record cannot contain a run plan")
		}
		if record.Attempt == 0 {
			return errors.New("scheduler stage journal record requires a positive attempt")
		}
		if record.StageID == "" || record.StageID != strings.TrimSpace(record.StageID) ||
			record.StageVersion == "" || record.StageVersion != strings.TrimSpace(record.StageVersion) {
			return errors.New("scheduler stage journal record requires stage ID and version")
		}
		if err := validateResourceRequest(record.Resources); err != nil {
			return fmt.Errorf("scheduler stage journal resources: %w", err)
		}
		switch record.State {
		case JournalStatePlanned, JournalStateQueued, JournalStateRunning:
			if record.Duration != 0 || record.Error != "" ||
				record.CacheKey != "" || record.OutputDigest != "" {
				return errors.New("non-terminal stage journal record cannot contain result fields")
			}
		case JournalStateSucceeded, JournalStateCached:
			if record.Error != "" {
				return errors.New("successful stage journal record cannot contain an error")
			}
			if err := validateJournalCacheKey(record.CacheKey); err != nil {
				return err
			}
			if err := validateJournalOutputDigest(record.OutputDigest); err != nil {
				return err
			}
		case JournalStateFailed, JournalStateCancelled:
			if strings.TrimSpace(record.Error) == "" {
				return errors.New("failed stage journal record requires an error")
			}
			if record.CacheKey != "" || record.OutputDigest != "" {
				return errors.New("failed stage journal record cannot contain successful result fields")
			}
		default:
			return fmt.Errorf("invalid scheduler stage state %q", record.State)
		}
	default:
		return fmt.Errorf("invalid scheduler journal kind %q", record.Kind)
	}
	return nil
}

func validateJournalPlan(plan []JournalStage) error {
	known := make(map[string]struct{}, len(plan))
	previous := ""
	for _, stage := range plan {
		if stage.ID == "" || stage.ID != strings.TrimSpace(stage.ID) ||
			stage.Version == "" || stage.Version != strings.TrimSpace(stage.Version) {
			return errors.New("scheduler journal plan requires stage IDs and versions")
		}
		if previous != "" && stage.ID <= previous {
			return errors.New("scheduler journal plan stages must be unique and sorted")
		}
		if err := validateResourceRequest(stage.Resources); err != nil {
			return fmt.Errorf("scheduler journal stage %s resources: %w", stage.ID, err)
		}
		if !equalStrings(stage.Dependencies, uniqueSorted(stage.Dependencies)) {
			return fmt.Errorf("scheduler journal stage %s dependencies are not unique and sorted", stage.ID)
		}
		known[stage.ID] = struct{}{}
		previous = stage.ID
	}
	for _, stage := range plan {
		for _, dependency := range stage.Dependencies {
			if dependency == stage.ID {
				return fmt.Errorf("scheduler journal stage %s depends on itself", stage.ID)
			}
			if _, ok := known[dependency]; !ok {
				return fmt.Errorf(
					"scheduler journal stage %s has unknown dependency %s",
					stage.ID,
					dependency,
				)
			}
		}
	}
	if journalPlanHasCycle(plan) {
		return errors.New("scheduler journal plan contains a dependency cycle")
	}
	return nil
}

// JournalPlanDigest binds every lifecycle record to one canonical stage plan.
func JournalPlanDigest(plan []JournalStage) (string, error) {
	normalized := append([]JournalStage{}, plan...)
	if err := validateJournalPlan(normalized); err != nil {
		return "", err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func journalRecordDigest(record JournalRecord) (string, error) {
	record.RecordDigest = ""
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateJournalCacheKey(key string) error {
	if !strings.HasPrefix(key, "stage:") ||
		!isLowerHex(strings.TrimPrefix(key, "stage:"), 64) {
		return errors.New("successful scheduler stage journal record has an invalid cache key")
	}
	return nil
}

func validateJournalOutputDigest(digest string) error {
	digest = strings.TrimPrefix(digest, "sha256:")
	if !isLowerHex(digest, sha256.Size*2) {
		return errors.New("successful scheduler stage journal record has an invalid output digest")
	}
	return nil
}

func journalPlanHasCycle(plan []JournalStage) bool {
	dependencies := make(map[string][]string, len(plan))
	for _, stage := range plan {
		dependencies[stage.ID] = stage.Dependencies
	}
	state := make(map[string]uint8, len(plan))
	var visit func(string) bool
	visit = func(stageID string) bool {
		switch state[stageID] {
		case 1:
			return true
		case 2:
			return false
		}
		state[stageID] = 1
		for _, dependency := range dependencies[stageID] {
			if visit(dependency) {
				return true
			}
		}
		state[stageID] = 2
		return false
	}
	for _, stage := range plan {
		if visit(stage.ID) {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func isTerminalRunState(state JournalState) bool {
	return state == JournalStateSucceeded ||
		state == JournalStateFailed ||
		state == JournalStateCancelled
}

type journalStageProgress struct {
	attempt     uint32
	lastAttempt uint32
	state       JournalState
	final       bool
}

type journalLifecycle struct {
	initialized  bool
	terminal     bool
	lastSequence uint64
	lastOccurred time.Time
	planDigest   string
	plan         map[string]JournalStage
	stages       map[string]journalStageProgress
}

func (lifecycle journalLifecycle) clone() journalLifecycle {
	clone := lifecycle
	if lifecycle.plan != nil {
		clone.plan = make(map[string]JournalStage, len(lifecycle.plan))
		for stageID, stage := range lifecycle.plan {
			stage.Dependencies = append([]string(nil), stage.Dependencies...)
			clone.plan[stageID] = stage
		}
	}
	if lifecycle.stages != nil {
		clone.stages = make(map[string]journalStageProgress, len(lifecycle.stages))
		for stageID, progress := range lifecycle.stages {
			clone.stages[stageID] = progress
		}
	}
	return clone
}

func (lifecycle *journalLifecycle) accept(record JournalRecord) error {
	if !lifecycle.initialized {
		if record.Sequence != 1 ||
			record.Kind != JournalKindRun ||
			record.State != JournalStateRunning {
			return errors.New("scheduler journal does not begin with a running run record")
		}
		if record.PreviousRecordDigest != zeroJournalDigest {
			return errors.New("scheduler journal genesis record has a non-zero previous digest")
		}
		digest, err := JournalPlanDigest(record.Plan)
		if err != nil {
			return err
		}
		if record.PlanDigest != digest {
			return errors.New("scheduler journal run plan digest does not match its plan")
		}
		lifecycle.initialized = true
		lifecycle.lastSequence = record.Sequence
		lifecycle.lastOccurred = record.OccurredAt
		lifecycle.planDigest = digest
		lifecycle.plan = make(map[string]JournalStage, len(record.Plan))
		lifecycle.stages = make(map[string]journalStageProgress, len(record.Plan))
		for _, stage := range record.Plan {
			stage.Dependencies = append([]string(nil), stage.Dependencies...)
			lifecycle.plan[stage.ID] = stage
		}
		return nil
	}
	if lifecycle.terminal {
		return errors.New("scheduler journal contains a record after run termination")
	}
	if record.Sequence != lifecycle.lastSequence+1 {
		return errors.New("scheduler journal lifecycle sequence is not contiguous")
	}
	if record.OccurredAt.Before(lifecycle.lastOccurred) {
		return errors.New("scheduler journal occurrence time moved backwards")
	}
	if record.PlanDigest != lifecycle.planDigest {
		return errors.New("scheduler journal plan digest changed")
	}

	switch record.Kind {
	case JournalKindRun:
		if record.State == JournalStateRunning {
			return errors.New("scheduler journal contains multiple run-start records")
		}
		if !isTerminalRunState(record.State) {
			return errors.New("scheduler journal contains a non-terminal follow-up run record")
		}
		if record.State == JournalStateSucceeded {
			for stageID := range lifecycle.plan {
				progress := lifecycle.stages[stageID]
				if !progress.final ||
					progress.state != JournalStateSucceeded &&
						progress.state != JournalStateCached {
					return fmt.Errorf(
						"scheduler run succeeded before stage %s completed successfully",
						stageID,
					)
				}
			}
		}
		lifecycle.terminal = true
	case JournalKindStage:
		if err := lifecycle.acceptStage(record); err != nil {
			return err
		}
	default:
		return errors.New("scheduler journal lifecycle has an unknown record kind")
	}
	lifecycle.lastSequence = record.Sequence
	lifecycle.lastOccurred = record.OccurredAt
	return nil
}

func (lifecycle *journalLifecycle) acceptStage(record JournalRecord) error {
	stage, ok := lifecycle.plan[record.StageID]
	if !ok {
		return fmt.Errorf("scheduler journal references unknown stage %s", record.StageID)
	}
	if record.StageVersion != stage.Version {
		return fmt.Errorf("scheduler journal stage %s version does not match plan", record.StageID)
	}
	if record.Resources != stage.Resources {
		return fmt.Errorf("scheduler journal stage %s resources do not match plan", record.StageID)
	}
	progress := lifecycle.stages[record.StageID]
	if progress.final {
		return fmt.Errorf("scheduler journal stage %s is already complete", record.StageID)
	}
	expectedAttempt := progress.lastAttempt + 1
	if progress.state == JournalStatePlanned ||
		progress.state == JournalStateQueued ||
		progress.state == JournalStateRunning {
		expectedAttempt = progress.attempt
	}
	if record.Attempt != expectedAttempt {
		return fmt.Errorf(
			"scheduler journal stage %s attempt is %d, want %d",
			record.StageID,
			record.Attempt,
			expectedAttempt,
		)
	}

	switch record.State {
	case JournalStatePlanned:
		if progress.state == JournalStatePlanned ||
			progress.state == JournalStateQueued ||
			progress.state == JournalStateRunning {
			return fmt.Errorf("scheduler journal stage %s is already active", record.StageID)
		}
	case JournalStateQueued:
		if progress.state == JournalStateQueued || progress.state == JournalStateRunning {
			return fmt.Errorf("scheduler journal stage %s has already been queued", record.StageID)
		}
		if err := lifecycle.requireDependencies(record.StageID, stage.Dependencies); err != nil {
			return err
		}
	case JournalStateRunning:
		if progress.state == JournalStateRunning {
			return fmt.Errorf("scheduler journal stage %s is already running", record.StageID)
		}
		if err := lifecycle.requireDependencies(record.StageID, stage.Dependencies); err != nil {
			return err
		}
	case JournalStateSucceeded, JournalStateCached, JournalStateFailed, JournalStateCancelled:
		if progress.state != JournalStateRunning || progress.attempt != record.Attempt {
			return fmt.Errorf(
				"scheduler journal stage %s reached a terminal state before running",
				record.StageID,
			)
		}
		progress.lastAttempt = record.Attempt
		progress.final = record.State == JournalStateSucceeded ||
			record.State == JournalStateCached
	default:
		return fmt.Errorf("scheduler journal stage %s has invalid state %q", record.StageID, record.State)
	}
	progress.attempt = record.Attempt
	progress.state = record.State
	lifecycle.stages[record.StageID] = progress
	return nil
}

func (lifecycle *journalLifecycle) requireDependencies(stageID string, dependencies []string) error {
	for _, dependency := range dependencies {
		progress := lifecycle.stages[dependency]
		if !progress.final ||
			progress.state != JournalStateSucceeded &&
				progress.state != JournalStateCached {
			return fmt.Errorf(
				"scheduler journal stage %s started before dependency %s succeeded",
				stageID,
				dependency,
			)
		}
	}
	return nil
}
