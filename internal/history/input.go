package history

import (
	"bytes"
	"context"
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
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/sourceorigin"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// Input is an absolute, regular-file history bound to its exact size and
// SHA-256 digest. The absolute path is private runtime state and is not copied
// into the compiled history document.
type Input struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// PrepareInput canonicalizes, bounds, and hashes one history file.
func PrepareInput(ctx context.Context, path string) (Input, error) {
	if ctx == nil {
		return Input{}, errors.New("history input context is required")
	}
	if err := ctx.Err(); err != nil {
		return Input{}, err
	}
	path = strings.TrimSpace(path)
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return Input{}, errors.New("history path is empty or invalid")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Input{}, fmt.Errorf("resolve history path: %w", err)
	}
	data, size, err := readBoundedRegularFile(absolute, MaximumCompiledHistoryBytes)
	if err != nil {
		return Input{}, fmt.Errorf("prepare history %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return Input{}, err
	}
	sum := sha256.Sum256(data)
	return Input{Path: absolute, SHA256: hex.EncodeToString(sum[:]), SizeBytes: size}, nil
}

// ReadCompiledFile loads and validates one prepared history input.
func ReadCompiledFile(ctx context.Context, input Input) (History, error) {
	if ctx == nil {
		return History{}, errors.New("history load context is required")
	}
	if err := ctx.Err(); err != nil {
		return History{}, err
	}
	if input.SizeBytes < 0 || input.SizeBytes > MaximumCompiledHistoryBytes ||
		len(input.SHA256) != sha256.Size*2 {
		return History{}, errors.New("prepared history identity is invalid or out of bounds")
	}
	data, size, err := readBoundedRegularFile(input.Path, MaximumCompiledHistoryBytes)
	if err != nil {
		return History{}, fmt.Errorf("read history %q: %w", input.Path, err)
	}
	if size != input.SizeBytes {
		return History{}, fmt.Errorf("history %q no longer matches its prepared input", input.Path)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != input.SHA256 {
		return History{}, fmt.Errorf("history %q digest changed", input.Path)
	}
	var compiled History
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&compiled); err != nil {
		return History{}, fmt.Errorf("decode history %q: %w", input.Path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return History{}, fmt.Errorf("decode history %q: trailing JSON content", input.Path)
	}
	if err := validateCompiledHistory(compiled); err != nil {
		return History{}, fmt.Errorf("validate history %q: %w", input.Path, err)
	}
	return compiled, nil
}

func readBoundedRegularFile(path string, maximumBytes int64) ([]byte, int64, error) {
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 || maximumBytes < 1 {
		return nil, 0, errors.New("bounded file path or limit is invalid")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, 0, errors.New("history must be a real regular file, not a symlink")
	}
	if before.Size() > maximumBytes {
		return nil, 0, fmt.Errorf("history exceeds maximum %d bytes", maximumBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return nil, 0, errors.New("history changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > maximumBytes {
		return nil, 0, fmt.Errorf("history exceeds maximum %d bytes", maximumBytes)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if after.Size() != opened.Size() || int64(len(data)) != opened.Size() {
		return nil, 0, errors.New("history changed while it was being read")
	}
	return data, opened.Size(), nil
}

func validateCompiledHistory(compiled History) error {
	if compiled.SchemaVersion != SchemaVersion {
		return fmt.Errorf("history schema_version %q is unsupported", compiled.SchemaVersion)
	}
	if !validBoundedText(compiled.Repository, MaximumRepositoryLabelBytes, false) ||
		filepath.IsAbs(compiled.Repository) ||
		strings.ContainsAny(compiled.Repository, `/\\`) {
		return errors.New("history repository label is invalid, unsafe, or path-bearing")
	}
	if compiled.SourceReference != "" &&
		(!validBoundedText(compiled.SourceReference, MaximumSourceReferenceBytes, false) ||
			!sourceorigin.IsCanonical(compiled.SourceReference) ||
			strings.HasPrefix(compiled.SourceReference, "file://")) {
		return errors.New("history source_reference is not a canonical portable origin")
	}
	repositoryIdentity := compiled.Repository
	if compiled.SourceReference != "" {
		repositoryIdentity = compiled.SourceReference
	}
	expectedRepositoryID := rkcmodel.StableID("repository", repositoryIdentity)
	if compiled.RepositoryID != expectedRepositoryID {
		return errors.New("history repository_id does not match its source identity")
	}
	if !validGitObjectID(compiled.SourceRevision) || compiled.Commit != compiled.SourceRevision {
		return errors.New("history source_revision is invalid or differs from its head")
	}
	if compiled.RevisionPolicy != RevisionPolicyExactHead {
		return errors.New("history revision_policy must require exact head affinity")
	}
	if compiled.AncestryPolicy != AncestryPolicyFirstParent {
		return errors.New("history ancestry_policy must require first-parent observations")
	}
	if compiled.SourceID != historySourceID(compiled.RepositoryID, compiled.SourceRevision) {
		return errors.New("history source_id does not match its immutable affinity")
	}
	if compiled.CommitLimit < 1 || compiled.CommitLimit > MaximumCommits {
		return fmt.Errorf("history commit_limit exceeds maximum %d", MaximumCommits)
	}
	if len(compiled.Commits) == 0 || len(compiled.Commits) > MaximumCommits ||
		(compiled.CommitLimit > 0 && len(compiled.Commits) > compiled.CommitLimit) {
		return fmt.Errorf("history commit count exceeds its bound")
	}
	if compiled.WindowTruncated && compiled.CommitLimit > 0 && len(compiled.Commits) != compiled.CommitLimit {
		return errors.New("truncated history window does not fill its declared commit limit")
	}
	if len(compiled.Symbols) > MaximumHistorySymbols || len(compiled.Refactors) > MaximumHistorySymbols {
		return errors.New("history semantic collection exceeds its bound")
	}
	commitIDs := make(map[string]struct{}, len(compiled.Commits))
	for index, commit := range compiled.Commits {
		if !validGitObjectID(commit.ID) ||
			(commit.Parent != "" && !validGitObjectID(commit.Parent)) ||
			!validGitDate(commit.Date) ||
			!validBoundedText(commit.Subject, MaximumCommitSubjectBytes, true) ||
			len(commit.ChangedFiles) > MaximumChangedFiles*2 ||
			len(commit.AddedSymbols) > MaximumHistorySymbols ||
			len(commit.RemovedSymbols) > MaximumHistorySymbols ||
			len(commit.ChangedSymbols) > MaximumHistorySymbols {
			return fmt.Errorf("history commit %q is invalid or out of bounds", commit.ID)
		}
		if _, duplicate := commitIDs[commit.ID]; duplicate {
			return fmt.Errorf("history commit %q is duplicated", commit.ID)
		}
		commitIDs[commit.ID] = struct{}{}
		if index == 0 && compiled.SourceRevision != commit.ID {
			return errors.New("history head does not match its newest observed commit")
		}
		if index+1 < len(compiled.Commits) && commit.Parent != compiled.Commits[index+1].ID {
			return errors.New("history commits do not form a first-parent chain")
		}
		for _, path := range commit.ChangedFiles {
			if !validGitPath(path) || !analyzablePath(path) {
				return fmt.Errorf("history commit %q contains unsupported path %q", commit.ID, path)
			}
		}
	}
	oldest := compiled.Commits[len(compiled.Commits)-1]
	if compiled.WindowTruncated && oldest.Parent == "" {
		return errors.New("truncated history does not identify its next first-parent ancestor")
	}
	if !compiled.WindowTruncated && oldest.Parent != "" {
		return errors.New("complete history window does not terminate at a first-parent root")
	}
	symbolIDs := make(map[string]struct{}, len(compiled.Symbols))
	for _, symbol := range compiled.Symbols {
		if !validBoundedText(symbol.ID, MaximumQualifiedNameBytes, false) ||
			!validBoundedText(symbol.Kind, maximumKindBytes, false) ||
			!validBoundedText(symbol.Name, MaximumSymbolNameBytes, false) ||
			!validBoundedText(symbol.QualifiedName, MaximumQualifiedNameBytes, false) ||
			symbol.FirstObserved == "" || symbol.LastObserved == "" ||
			(symbol.Language != "go" && symbol.Language != "typescript") ||
			len(symbol.Signatures) > MaximumSignaturesPerSymbol ||
			len(symbol.CommitsTouching) > MaximumCommits || len(symbol.Files) > MaximumCommits+1 {
			return fmt.Errorf("history symbol %q is invalid or out of bounds", symbol.ID)
		}
		expectedID := rkcmodel.StableID("history-symbol", symbol.Language, symbol.Kind, symbol.QualifiedName)
		if symbol.ID != expectedID {
			return fmt.Errorf("history symbol %q does not match its semantic identity", symbol.ID)
		}
		if _, duplicate := symbolIDs[symbol.ID]; duplicate {
			return fmt.Errorf("history symbol %q is duplicated", symbol.ID)
		}
		symbolIDs[symbol.ID] = struct{}{}
		if _, ok := commitIDs[symbol.FirstObserved]; !ok {
			return fmt.Errorf("history symbol %q first observation is outside the window", symbol.ID)
		}
		if _, ok := commitIDs[symbol.LastObserved]; !ok {
			return fmt.Errorf("history symbol %q last observation is outside the window", symbol.ID)
		}
		for _, path := range symbol.Files {
			if !validGitPath(path) || languageOf(path) != symbol.Language {
				return fmt.Errorf("history symbol %q contains unsupported path %q", symbol.ID, path)
			}
		}
		for _, snapshot := range symbol.Signatures {
			if snapshot.Commit == "" || !validGitPath(snapshot.File) ||
				languageOf(snapshot.File) != symbol.Language ||
				!validBoundedText(snapshot.Signature, MaximumSignatureBytes, false) {
				return fmt.Errorf("history symbol %q has an invalid signature snapshot", symbol.ID)
			}
			if _, ok := commitIDs[snapshot.Commit]; !ok {
				return fmt.Errorf("history symbol %q signature is outside the window", symbol.ID)
			}
		}
		for _, commitID := range symbol.CommitsTouching {
			if _, ok := commitIDs[commitID]; !ok {
				return fmt.Errorf("history symbol %q event is outside the window", symbol.ID)
			}
		}
	}
	for _, commit := range compiled.Commits {
		for _, collection := range [][]string{commit.AddedSymbols, commit.RemovedSymbols, commit.ChangedSymbols} {
			seen := make(map[string]struct{}, len(collection))
			for _, symbolID := range collection {
				if _, ok := symbolIDs[symbolID]; !ok {
					return fmt.Errorf("history commit %q references unknown symbol %q", commit.ID, symbolID)
				}
				if _, duplicate := seen[symbolID]; duplicate {
					return fmt.Errorf("history commit %q repeats symbol %q", commit.ID, symbolID)
				}
				seen[symbolID] = struct{}{}
			}
		}
	}
	for _, refactor := range compiled.Refactors {
		if !validGitObjectID(refactor.Commit) ||
			!validBoundedText(refactor.Kind, maximumKindBytes, false) ||
			!validBoundedText(refactor.QualifiedFrom, MaximumQualifiedNameBytes, false) ||
			!validBoundedText(refactor.QualifiedTo, MaximumQualifiedNameBytes, false) ||
			refactor.QualifiedFrom == refactor.QualifiedTo ||
			refactor.From != refactor.QualifiedFrom || refactor.To != refactor.QualifiedTo ||
			(refactor.Language != "go" && refactor.Language != "typescript") {
			return errors.New("history refactor is invalid or missing its language")
		}
		if _, ok := commitIDs[refactor.Commit]; !ok {
			return errors.New("history refactor commit is outside the window")
		}
		fromID := rkcmodel.StableID("history-symbol", refactor.Language, refactor.Kind, refactor.QualifiedFrom)
		toID := rkcmodel.StableID("history-symbol", refactor.Language, refactor.Kind, refactor.QualifiedTo)
		if _, ok := symbolIDs[fromID]; !ok {
			return errors.New("history refactor source is not an observed symbol")
		}
		if _, ok := symbolIDs[toID]; !ok {
			return errors.New("history refactor target is not an observed symbol")
		}
	}
	return nil
}

func validBoundedText(value string, maximumBytes int, allowEmpty bool) bool {
	if maximumBytes < 1 || len(value) > maximumBytes || !utf8.ValidString(value) {
		return false
	}
	if value == "" {
		return allowEmpty
	}
	for _, character := range value {
		if unsafeTextRune(character) {
			return false
		}
	}
	return true
}

func unsafeTextRune(character rune) bool {
	return unicode.IsControl(character) ||
		unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp)
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validGitDate(value string) bool {
	if !validBoundedText(value, maximumGitDateBytes, false) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
