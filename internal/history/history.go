// Package history compiles a bounded, first-parent observation window of Git
// commits into deterministic symbol-interface deltas. It reports only facts
// observed by the Go and TypeScript syntax extractors: symbol additions,
// removals, signature changes, and file moves. It does not claim to reconstruct
// a symbol's complete lifetime when the requested commit window is truncated.
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/gitworktree"
	"github.com/neuroforge-io/RKC/internal/lang/goast"
	"github.com/neuroforge-io/RKC/internal/lang/tssyntax"
	"github.com/neuroforge-io/RKC/internal/sourceorigin"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// PluginID is the stable producer identity for imported history facts.
const PluginID = "rkc.history"

// PluginVersion identifies the semantic-delta compiler semantics.
const PluginVersion = "1.2.0"

const (
	// SchemaVersion is the canonical history format version.
	SchemaVersion = "1.1"
	// RevisionPolicyExactHead requires the compiled source revision to equal
	// the imported bundle's clean Git HEAD. Ancestor-only imports are rejected
	// because a portable bundle does not carry enough Git state to prove them.
	RevisionPolicyExactHead = "exact_head"
	// AncestryPolicyFirstParent states that every observed commit is an ancestor
	// on the source revision's exact first-parent chain.
	AncestryPolicyFirstParent = "first_parent"
	// DefaultMaxCommits bounds the default observation window.
	DefaultMaxCommits = 500
	// MaximumCommits is the largest accepted observation window.
	MaximumCommits = 10_000
	// MaximumChangedFiles bounds analyzable file changes in one commit. The
	// build fails rather than publishing a partial commit delta when exceeded.
	MaximumChangedFiles = 256
	// MaximumHistorySymbols bounds retained symbol observations.
	MaximumHistorySymbols = 131_072
	// MaximumSymbolsPerFile bounds one extractor result.
	MaximumSymbolsPerFile = 32_768
	// MaximumSignaturesPerSymbol bounds signature snapshots. A lifecycle is
	// explicitly marked when additional snapshots were omitted.
	MaximumSignaturesPerSymbol = 64
	// MaximumMaterializedFileBytes bounds one Git blob passed to an extractor.
	MaximumMaterializedFileBytes = 4 << 20
	// MaximumGitMetadataBytes bounds each metadata-producing Git command.
	MaximumGitMetadataBytes = 32 << 20
	// MaximumCompiledHistoryBytes bounds serialized and imported history files.
	MaximumCompiledHistoryBytes = 64 << 20
	// MaximumRepositoryLabelBytes bounds the private-path-free display label.
	MaximumRepositoryLabelBytes = 255
	// MaximumSourceReferenceBytes bounds a canonical credential-free remote.
	MaximumSourceReferenceBytes = 4096
	// MaximumGitPathBytes bounds any repository-relative path retained in a
	// compiled history.
	MaximumGitPathBytes = 4096
	// MaximumCommitSubjectBytes bounds one retained Git subject.
	MaximumCommitSubjectBytes = 4096
	// MaximumSymbolNameBytes bounds an extracted unqualified symbol name.
	MaximumSymbolNameBytes = 1024
	// MaximumQualifiedNameBytes bounds qualified names and refactor endpoints.
	MaximumQualifiedNameBytes = 4096
	// MaximumSignatureBytes bounds one retained interface signature.
	MaximumSignatureBytes     = 16 << 10
	maximumGitDiagnosticBytes = 4 << 10
	maximumGitDateBytes       = 64
	maximumKindBytes          = 64
)

// Options controls one history compilation.
type Options struct {
	// Repository is the Git working tree to compile.
	Repository string
	// GitExecutable overrides the Git binary (default "git").
	GitExecutable string
	// MaxCommits bounds the observation window (default 500, maximum 10,000).
	MaxCommits int
}

// History is the canonical bounded semantic-delta record.
type History struct {
	SchemaVersion string `json:"schema_version"`
	// Repository is a display label only. Absolute host paths are never
	// serialized into a compiled history.
	Repository string `json:"repository"`
	// RepositoryID is the same canonical identity used by the target bundle.
	RepositoryID string `json:"repository_id"`
	// SourceReference is a canonical credential-free remote origin when one is
	// available. Local paths and file:// origins are never serialized.
	SourceReference string `json:"source_reference,omitempty"`
	// SourceRevision is the immutable Git object compiled by this record.
	SourceRevision string `json:"source_revision"`
	// RevisionPolicy and AncestryPolicy make import compatibility explicit.
	RevisionPolicy string `json:"revision_policy"`
	AncestryPolicy string `json:"ancestry_policy"`
	// SourceID binds repository identity, revision, and both policies.
	SourceID         string          `json:"source_id"`
	Commit           string          `json:"commit"`
	CommitLimit      int             `json:"commit_limit"`
	WindowTruncated  bool            `json:"window_truncated"`
	DetailsTruncated bool            `json:"details_truncated"`
	Commits          []CommitRecord  `json:"commits"`
	Symbols          []SymbolHistory `json:"symbols"`
	Refactors        []Refactor      `json:"refactors"`
}

// CommitRecord is one first-parent commit delta in the observation window.
type CommitRecord struct {
	ID             string   `json:"id"`
	Parent         string   `json:"parent,omitempty"`
	Date           string   `json:"date"`
	Subject        string   `json:"subject"`
	ChangedFiles   []string `json:"changed_files"`
	AddedSymbols   []string `json:"added_symbols"`
	RemovedSymbols []string `json:"removed_symbols"`
	ChangedSymbols []string `json:"changed_symbols"`
}

// SymbolHistory is the lifecycle observed inside this history window. The
// first and last fields are observations, not claims about repository-wide
// creation or deletion dates.
type SymbolHistory struct {
	ID                        string              `json:"id"`
	Kind                      string              `json:"kind"`
	Name                      string              `json:"name"`
	QualifiedName             string              `json:"qualified_name"`
	Language                  string              `json:"language"`
	FirstObserved             string              `json:"first_observed_commit"`
	LastObserved              string              `json:"last_observed_commit"`
	Files                     []string            `json:"files"`
	CommitsTouching           []string            `json:"commits_touching"`
	Signatures                []SignatureSnapshot `json:"signature_history,omitempty"`
	SignatureHistoryTruncated bool                `json:"signature_history_truncated,omitempty"`
}

// SignatureSnapshot is one observed signature and file location.
type SignatureSnapshot struct {
	Commit    string `json:"commit"`
	Signature string `json:"signature"`
	File      string `json:"file"`
}

// Refactor is a conservative, unique same-signature rename or move candidate
// observed in one commit. Language is explicit so import never assumes Go.
type Refactor struct {
	Commit        string `json:"commit"`
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	From          string `json:"from"`
	To            string `json:"to"`
	QualifiedFrom string `json:"qualified_from"`
	QualifiedTo   string `json:"qualified_to"`
}

type fileChange struct {
	status  byte
	oldPath string
	newPath string
}

type symbolObservation struct {
	node     rkcmodel.Node
	language string
	path     string
}

type symbolSet map[string]symbolObservation

type commitDelta struct {
	added   map[string][]symbolObservation
	removed map[string][]symbolObservation
	changed map[string][]symbolObservation
}

// Build compiles a bounded first-parent semantic history of a repository.
func Build(ctx context.Context, options Options) (History, error) {
	if ctx == nil {
		return History{}, errors.New("history context is required")
	}
	if err := ctx.Err(); err != nil {
		return History{}, err
	}
	root, err := resolveHistoryRoot(options.Repository)
	if err != nil {
		return History{}, fmt.Errorf("history repository: %w", err)
	}
	git := strings.TrimSpace(options.GitExecutable)
	if git == "" {
		git = "git"
	}
	maxCommits := options.MaxCommits
	if maxCommits == 0 {
		maxCommits = DefaultMaxCommits
	}
	if maxCommits < 1 || maxCommits > MaximumCommits {
		return History{}, fmt.Errorf("history max commits must be between 1 and %d", MaximumCommits)
	}
	if !gitworktree.AffinityEnvironmentIsPaired() {
		return History{}, errors.New("history repository has an unpaired Git affinity environment")
	}
	topLevelBytes, err := gitOutputBounded(
		ctx,
		git,
		root,
		gitworktree.MaximumTopLevelBytes+2,
		"rev-parse",
		"--show-toplevel",
	)
	if err != nil {
		return History{}, fmt.Errorf("resolve Git work tree: %w", err)
	}
	topLevel, topLevelOK := gitworktree.ParseTopLevelOutput(topLevelBytes)
	if !topLevelOK || !gitworktree.IsExactRoot(root, topLevel) {
		return History{}, errors.New("history repository is not the exact Git work tree")
	}

	headBytes, err := gitOutputBounded(ctx, git, root, 256, "rev-parse", "HEAD")
	if err != nil {
		return History{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))
	if !validGitObjectID(head) {
		return History{}, errors.New("resolve HEAD: Git returned an invalid commit identity")
	}
	repository := repositoryLabel(root)
	if !validBoundedText(repository, MaximumRepositoryLabelBytes, false) {
		return History{}, errors.New("history repository label is invalid or out of bounds")
	}
	sourceReference, err := repositorySourceReference(ctx, git, root)
	if err != nil {
		return History{}, err
	}
	repositoryIdentity := repository
	if sourceReference != "" {
		repositoryIdentity = sourceReference
	}
	repositoryID := rkcmodel.StableID("repository", repositoryIdentity)
	sourceID := historySourceID(repositoryID, head)

	logBytes, err := gitOutputBounded(
		ctx,
		git,
		root,
		MaximumGitMetadataBytes,
		"log",
		"--first-parent",
		"-z",
		"--format=%H%x00%P%x00%aI%x00%s",
		"-n",
		strconv.Itoa(maxCommits+1),
	)
	if err != nil {
		return History{}, fmt.Errorf("enumerate commits: %w", err)
	}
	commits, err := parseGitLog(logBytes)
	if err != nil {
		return History{}, fmt.Errorf("enumerate commits: %w", err)
	}
	if len(commits) == 0 || commits[0].ID != head {
		return History{}, errors.New("enumerate commits: Git log does not begin at HEAD")
	}
	windowTruncated := len(commits) > maxCommits
	if windowTruncated {
		commits = commits[:maxCommits]
	}
	for index := 0; index+1 < len(commits); index++ {
		if commits[index].Parent != commits[index+1].ID {
			return History{}, errors.New("enumerate commits: first-parent history is not a linear chain")
		}
	}

	symbols := make(map[string]*SymbolHistory)
	fileStates := make(map[string]symbolSet)
	refactors := make([]Refactor, 0)
	detailsTruncated := false

	// Public commits remain newest-first. Analysis runs oldest-first so every
	// file state advances in commit order.
	for order := len(commits) - 1; order >= 0; order-- {
		if err := ctx.Err(); err != nil {
			return History{}, err
		}
		commit := &commits[order]
		changes, err := changesForCommit(ctx, git, root, *commit)
		if err != nil {
			return History{}, fmt.Errorf("diff commit %s: %w", commit.ID, err)
		}
		if len(changes) > MaximumChangedFiles {
			return History{}, fmt.Errorf(
				"commit %s changes %d supported source files; maximum is %d",
				commit.ID,
				len(changes),
				MaximumChangedFiles,
			)
		}
		commit.ChangedFiles = changedPaths(changes)
		delta := newCommitDelta()

		for _, change := range changes {
			before, after, err := observeFileChange(ctx, git, root, *commit, change, fileStates)
			if err != nil {
				return History{}, fmt.Errorf("analyze commit %s: %w", commit.ID, err)
			}
			mergeFileDelta(&delta, before, after)
			advanceFileState(fileStates, change, after)
		}
		reconcileMoves(&delta)
		commit.AddedSymbols = sortedObservationKeys(delta.added)
		commit.RemovedSymbols = sortedObservationKeys(delta.removed)
		commit.ChangedSymbols = sortedObservationKeys(delta.changed)

		for _, collection := range []map[string][]symbolObservation{delta.added, delta.removed, delta.changed} {
			for _, id := range sortedObservationKeys(collection) {
				truncated, err := recordLifecycle(symbols, id, commit.ID, collection[id])
				if err != nil {
					return History{}, err
				}
				detailsTruncated = detailsTruncated || truncated
			}
		}
		refactors = append(refactors, detectRefactors(commit.ID, delta.added, delta.removed)...)
	}

	result := History{
		SchemaVersion:    SchemaVersion,
		Repository:       repository,
		RepositoryID:     repositoryID,
		SourceReference:  sourceReference,
		SourceRevision:   head,
		RevisionPolicy:   RevisionPolicyExactHead,
		AncestryPolicy:   AncestryPolicyFirstParent,
		SourceID:         sourceID,
		Commit:           head,
		CommitLimit:      maxCommits,
		WindowTruncated:  windowTruncated,
		DetailsTruncated: detailsTruncated,
		Commits:          commits,
		Refactors:        refactors,
	}
	for _, entry := range symbols {
		sort.Strings(entry.Files)
		result.Symbols = append(result.Symbols, *entry)
	}
	sort.Slice(result.Symbols, func(i, j int) bool { return result.Symbols[i].ID < result.Symbols[j].ID })
	if err := validateCompiledHistory(result); err != nil {
		return History{}, fmt.Errorf("validate compiled history: %w", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return History{}, fmt.Errorf("encode compiled history: %w", err)
	}
	if len(encoded) > MaximumCompiledHistoryBytes {
		return History{}, fmt.Errorf("compiled history exceeds maximum %d bytes", MaximumCompiledHistoryBytes)
	}
	return result, nil
}

func parseGitLog(data []byte) ([]CommitRecord, error) {
	fields := bytes.Split(data, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%4 != 0 {
		return nil, errors.New("malformed NUL-delimited Git log")
	}
	commits := make([]CommitRecord, 0, len(fields)/4)
	for index := 0; index < len(fields); index += 4 {
		id := string(fields[index])
		parents := strings.Fields(string(fields[index+1]))
		date := string(fields[index+2])
		subject := string(fields[index+3])
		if !validGitObjectID(id) || !validGitDate(date) ||
			!validBoundedText(subject, MaximumCommitSubjectBytes, true) {
			return nil, errors.New("Git log contains an invalid commit record")
		}
		for _, candidate := range parents {
			if !validGitObjectID(candidate) {
				return nil, errors.New("Git log contains an invalid parent identity")
			}
		}
		parent := ""
		if len(parents) > 0 {
			parent = parents[0]
		}
		commits = append(commits, CommitRecord{
			ID:             id,
			Parent:         parent,
			Date:           date,
			Subject:        subject,
			ChangedFiles:   []string{},
			AddedSymbols:   []string{},
			RemovedSymbols: []string{},
			ChangedSymbols: []string{},
		})
	}
	return commits, nil
}

func changesForCommit(ctx context.Context, git, root string, commit CommitRecord) ([]fileChange, error) {
	arguments := []string{"diff-tree", "-r", "-M", "--name-status", "-z", "--no-commit-id"}
	if commit.Parent == "" {
		arguments = append(arguments, "--root", commit.ID)
	} else {
		arguments = append(arguments, commit.Parent, commit.ID)
	}
	data, err := gitOutputBounded(ctx, git, root, MaximumGitMetadataBytes, arguments...)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(data)
}

func parseNameStatus(data []byte) ([]fileChange, error) {
	fields := bytes.Split(data, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	changes := make([]fileChange, 0)
	for index := 0; index < len(fields); {
		statusField := string(fields[index])
		index++
		if statusField == "" {
			return nil, errors.New("empty Git name-status record")
		}
		status := statusField[0]
		if status == 'R' || status == 'C' {
			if index+1 >= len(fields) {
				return nil, errors.New("truncated Git rename/copy record")
			}
			oldPath, newPath := string(fields[index]), string(fields[index+1])
			index += 2
			if !validGitPath(oldPath) || !validGitPath(newPath) {
				return nil, errors.New("Git returned an unsafe rename/copy path")
			}
			if status == 'R' {
				oldSupported, newSupported := analyzablePath(oldPath), analyzablePath(newPath)
				switch {
				case oldSupported && newSupported && languageOf(oldPath) == languageOf(newPath):
					changes = append(changes, fileChange{status: status, oldPath: oldPath, newPath: newPath})
				case oldSupported && newSupported:
					changes = append(changes,
						fileChange{status: 'D', oldPath: oldPath, newPath: oldPath},
						fileChange{status: 'A', oldPath: newPath, newPath: newPath},
					)
				case oldSupported:
					changes = append(changes, fileChange{status: 'D', oldPath: oldPath, newPath: oldPath})
				case newSupported:
					changes = append(changes, fileChange{status: 'A', oldPath: newPath, newPath: newPath})
				}
			}
			if status == 'C' && analyzablePath(newPath) {
				changes = append(changes, fileChange{status: status, oldPath: oldPath, newPath: newPath})
			}
			continue
		}
		if index >= len(fields) {
			return nil, errors.New("truncated Git name-status record")
		}
		filePath := string(fields[index])
		index++
		if !validGitPath(filePath) {
			return nil, errors.New("Git returned an unsafe changed path")
		}
		if !analyzablePath(filePath) {
			continue
		}
		switch status {
		case 'A', 'M', 'D', 'T':
			changes = append(changes, fileChange{status: status, oldPath: filePath, newPath: filePath})
		default:
			return nil, fmt.Errorf("unsupported Git status %q for %q", statusField, filePath)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].newPath != changes[j].newPath {
			return changes[i].newPath < changes[j].newPath
		}
		if changes[i].oldPath != changes[j].oldPath {
			return changes[i].oldPath < changes[j].oldPath
		}
		return changes[i].status < changes[j].status
	})
	return changes, nil
}

func changedPaths(changes []fileChange) []string {
	unique := make(map[string]struct{})
	for _, change := range changes {
		if change.status != 'C' && analyzablePath(change.oldPath) {
			unique[change.oldPath] = struct{}{}
		}
		if analyzablePath(change.newPath) {
			unique[change.newPath] = struct{}{}
		}
	}
	paths := make([]string, 0, len(unique))
	for filePath := range unique {
		paths = append(paths, filePath)
	}
	sort.Strings(paths)
	return paths
}

func observeFileChange(
	ctx context.Context,
	git, root string,
	commit CommitRecord,
	change fileChange,
	states map[string]symbolSet,
) (symbolSet, symbolSet, error) {
	empty := func() symbolSet { return make(symbolSet) }
	loadBefore := func(filePath string) (symbolSet, error) {
		if state, ok := states[filePath]; ok {
			return cloneSymbolSet(state), nil
		}
		if commit.Parent == "" {
			return empty(), nil
		}
		return extractSymbolsAt(ctx, git, root, commit.Parent, filePath)
	}
	loadAfter := func(filePath string) (symbolSet, error) {
		return extractSymbolsAt(ctx, git, root, commit.ID, filePath)
	}

	switch change.status {
	case 'A', 'C':
		after, err := loadAfter(change.newPath)
		return empty(), after, err
	case 'D':
		before, err := loadBefore(change.oldPath)
		return before, empty(), err
	case 'M', 'T':
		before, err := loadBefore(change.oldPath)
		if err != nil {
			return nil, nil, err
		}
		after, err := loadAfter(change.newPath)
		return before, after, err
	case 'R':
		before, err := loadBefore(change.oldPath)
		if err != nil {
			return nil, nil, err
		}
		after, err := loadAfter(change.newPath)
		return before, after, err
	default:
		return nil, nil, fmt.Errorf("unsupported file change status %q", change.status)
	}
}

func advanceFileState(states map[string]symbolSet, change fileChange, after symbolSet) {
	if change.status == 'D' || change.status == 'R' {
		delete(states, change.oldPath)
	}
	if change.status != 'D' {
		states[change.newPath] = cloneSymbolSet(after)
	}
}

func cloneSymbolSet(input symbolSet) symbolSet {
	result := make(symbolSet, len(input))
	for id, observation := range input {
		result[id] = observation
	}
	return result
}

func newCommitDelta() commitDelta {
	return commitDelta{
		added:   make(map[string][]symbolObservation),
		removed: make(map[string][]symbolObservation),
		changed: make(map[string][]symbolObservation),
	}
}

func mergeFileDelta(delta *commitDelta, before, after symbolSet) {
	for id, prior := range before {
		current, exists := after[id]
		if !exists {
			delta.removed[id] = append(delta.removed[id], prior)
			continue
		}
		if prior.node.Signature != current.node.Signature || prior.path != current.path {
			delta.changed[id] = append(delta.changed[id], prior, current)
		}
	}
	for id, current := range after {
		if _, exists := before[id]; !exists {
			delta.added[id] = append(delta.added[id], current)
		}
	}
}

// A delete/add pair with the same stable identity is a move, not simultaneous
// removal and addition. This also handles Git choosing D+A instead of R.
func reconcileMoves(delta *commitDelta) {
	for id, additions := range delta.added {
		removals, exists := delta.removed[id]
		if !exists {
			continue
		}
		delta.changed[id] = append(delta.changed[id], removals...)
		delta.changed[id] = append(delta.changed[id], additions...)
		delete(delta.added, id)
		delete(delta.removed, id)
	}
}

func sortedObservationKeys(values map[string][]symbolObservation) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func recordLifecycle(
	symbols map[string]*SymbolHistory,
	id, commit string,
	observations []symbolObservation,
) (bool, error) {
	if len(observations) == 0 {
		return false, errors.New("history symbol event has no observation")
	}
	entry, exists := symbols[id]
	if !exists {
		if len(symbols) >= MaximumHistorySymbols {
			return false, fmt.Errorf("history symbol count exceeds maximum %d", MaximumHistorySymbols)
		}
		first := observations[0]
		entry = &SymbolHistory{
			ID:            id,
			Kind:          first.node.Kind,
			Name:          first.node.Name,
			QualifiedName: first.node.QualifiedName,
			Language:      first.language,
			FirstObserved: commit,
			LastObserved:  commit,
		}
		symbols[id] = entry
	}
	entry.LastObserved = commit
	if !containsString(entry.CommitsTouching, commit) {
		entry.CommitsTouching = append(entry.CommitsTouching, commit)
	}
	truncated := false
	for _, observation := range observations {
		if observation.language != entry.Language || observation.node.Kind != entry.Kind ||
			observation.node.QualifiedName != entry.QualifiedName {
			return false, fmt.Errorf("history symbol identity %q changed semantic key", id)
		}
		if !containsString(entry.Files, observation.path) {
			entry.Files = append(entry.Files, observation.path)
		}
		if observation.node.Signature == "" {
			continue
		}
		if len(entry.Signatures) > 0 {
			last := entry.Signatures[len(entry.Signatures)-1]
			if last.Signature == observation.node.Signature && last.File == observation.path {
				continue
			}
		}
		if len(entry.Signatures) >= MaximumSignaturesPerSymbol {
			entry.SignatureHistoryTruncated = true
			truncated = true
			continue
		}
		entry.Signatures = append(entry.Signatures, SignatureSnapshot{
			Commit:    commit,
			Signature: observation.node.Signature,
			File:      observation.path,
		})
	}
	return truncated, nil
}

func detectRefactors(
	commit string,
	added, removed map[string][]symbolObservation,
) []Refactor {
	type candidates struct {
		added   []symbolObservation
		removed []symbolObservation
	}
	groups := make(map[string]*candidates)
	addCandidate := func(observation symbolObservation, addition bool) {
		keySignature := signatureKey(observation.node)
		if keySignature == "" {
			return
		}
		key := observation.language + "\x00" + observation.node.Kind + "\x00" + keySignature
		group := groups[key]
		if group == nil {
			group = &candidates{}
			groups[key] = group
		}
		if addition {
			group.added = append(group.added, observation)
		} else {
			group.removed = append(group.removed, observation)
		}
	}
	for _, observations := range added {
		if len(observations) == 1 {
			addCandidate(observations[0], true)
		}
	}
	for _, observations := range removed {
		if len(observations) == 1 {
			addCandidate(observations[0], false)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Refactor, 0)
	for _, key := range keys {
		group := groups[key]
		if len(group.added) != 1 || len(group.removed) != 1 {
			continue
		}
		from, to := group.removed[0], group.added[0]
		if from.node.QualifiedName == to.node.QualifiedName {
			continue
		}
		result = append(result, Refactor{
			Commit:        commit,
			Language:      from.language,
			Kind:          from.node.Kind,
			From:          from.node.QualifiedName,
			To:            to.node.QualifiedName,
			QualifiedFrom: from.node.QualifiedName,
			QualifiedTo:   to.node.QualifiedName,
		})
	}
	return result
}

func analyzablePath(filePath string) bool {
	switch strings.ToLower(pathpkg.Ext(filePath)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx":
		return true
	default:
		return false
	}
}

func validGitPath(value string) bool {
	if !validBoundedText(value, MaximumGitPathBytes, false) || strings.Contains(value, "\\") ||
		strings.HasPrefix(value, "/") || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// extractSymbolsAt materializes one bounded blob in a private temporary tree
// and runs the deterministic syntax extractor for its supported language.
func extractSymbolsAt(ctx context.Context, git, root, revision, filePath string) (symbolSet, error) {
	if !analyzablePath(filePath) || !validGitPath(filePath) {
		return nil, fmt.Errorf("unsupported or unsafe history path %q", filePath)
	}
	spec := revision + ":" + filePath
	content, err := gitOutputBounded(ctx, git, root, MaximumMaterializedFileBytes, "cat-file", "blob", spec)
	if err != nil {
		return nil, fmt.Errorf("materialize %q: %w", filePath, err)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, fmt.Errorf("materialized file %q contains NUL bytes", filePath)
	}
	temporary, err := os.MkdirTemp("", "rkc-history-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	full := filepath.Join(temporary, filepath.FromSlash(filePath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		return nil, err
	}
	language := languageOf(filePath)
	ref := pluginapi.FileRef{
		ArtifactID: rkcmodel.StableID("artifact", filePath),
		Path:       filePath,
		SizeBytes:  int64(len(content)),
		Language:   language,
	}
	var fragment rkcmodel.Fragment
	switch language {
	case "go":
		fragment, err = goast.Extract(goast.Options{Root: temporary, Files: []pluginapi.FileRef{ref}})
	case "typescript":
		fragment, err = tssyntax.Extract(tssyntax.Options{Root: temporary, Files: []pluginapi.FileRef{ref}})
	default:
		return nil, errors.New("unsupported history language")
	}
	if err != nil {
		return nil, fmt.Errorf("extract %q: %w", filePath, err)
	}
	result := make(symbolSet)
	for _, node := range sortNodes(fragment.Nodes) {
		if !isSymbolKind(node.Kind) {
			continue
		}
		if len(result) >= MaximumSymbolsPerFile {
			return nil, fmt.Errorf("file %q exceeds maximum %d symbols", filePath, MaximumSymbolsPerFile)
		}
		// Extractors may preserve layout in multiline declarations. History
		// signatures are a control-safe semantic comparison key, not source text.
		node.Signature = strings.Join(strings.Fields(node.Signature), " ")
		id := rkcmodel.StableID("history-symbol", language, node.Kind, node.QualifiedName)
		result[id] = symbolObservation{node: node, language: language, path: filePath}
	}
	return result, nil
}

func languageOf(filePath string) string {
	switch strings.ToLower(pathpkg.Ext(filePath)) {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".js", ".jsx":
		return "typescript"
	default:
		return ""
	}
}

func isSymbolKind(kind string) bool {
	switch kind {
	case "function", "method", "class", "interface", "type", "enum", "enum_member",
		"constructor", "field", "property", "variable", "constant", "module", "test":
		return true
	default:
		return false
	}
}

func sortNodes(nodes []rkcmodel.Node) []rkcmodel.Node {
	result := append([]rkcmodel.Node(nil), nodes...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// signatureKey removes the symbol's own name. It is used only when exactly one
// removal and one addition share the resulting language/kind/signature key.
func signatureKey(node rkcmodel.Node) string {
	signature := strings.TrimSpace(node.Signature)
	if signature == "" || node.Name == "" {
		return signature
	}
	first := strings.Index(signature, node.Name)
	if first < 0 {
		return signature
	}
	return strings.TrimSpace(signature[:first]) + " " + strings.TrimSpace(signature[first+len(node.Name):])
}

type boundedDiagnostic struct {
	buffer bytes.Buffer
	limit  int
}

func (writer *boundedDiagnostic) Write(data []byte) (int, error) {
	original := len(data)
	remaining := writer.limit - writer.buffer.Len()
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		_, _ = writer.buffer.Write(data[:remaining])
	}
	return original, nil
}

func gitOutputBounded(
	ctx context.Context,
	git, root string,
	maximumBytes int64,
	arguments ...string,
) ([]byte, error) {
	if ctx == nil || maximumBytes < 1 || strings.TrimSpace(git) == "" ||
		strings.TrimSpace(root) == "" || len(arguments) == 0 {
		return nil, errors.New("bounded Git command is not configured")
	}
	command := exec.CommandContext(ctx, git, arguments...)
	command.Dir = root
	command.Env = append([]string(nil), os.Environ()...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture Git output: %w", err)
	}
	diagnostic := &boundedDiagnostic{limit: maximumGitDiagnosticBytes}
	command.Stderr = diagnostic
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Git command: %s", EscapeTerminalText(err.Error()))
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, maximumBytes+1))
	if int64(len(output)) > maximumBytes {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("git %s output exceeds %d bytes", arguments[0], maximumBytes)
	}
	waitErr := command.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, fmt.Errorf("read git %s output: %w", arguments[0], readErr)
	}
	if waitErr != nil {
		detail := EscapeTerminalText(strings.TrimSpace(diagnostic.buffer.String()))
		if detail != "" {
			return nil, fmt.Errorf(
				"git %s failed (%s): %s",
				arguments[0],
				EscapeTerminalText(waitErr.Error()),
				detail,
			)
		}
		return nil, fmt.Errorf("git %s failed: %s", arguments[0], EscapeTerminalText(waitErr.Error()))
	}
	return output, nil
}

func repositoryLabel(root string) string {
	label := filepath.Base(filepath.Clean(root))
	if label == "" || label == "." || label == string(filepath.Separator) {
		return "repository"
	}
	return label
}

func repositorySourceReference(ctx context.Context, git, root string) (string, error) {
	data, err := gitOutputBounded(ctx, git, root, 4096, "remote", "get-url", "origin")
	if err != nil {
		// A repository without an origin remains valid. Its repository identity
		// is bound to the private-path-free root label and exact commit instead.
		return "", nil
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	raw := string(data)
	if raw == "" {
		return "", nil
	}
	canonical, normalizeErr := sourceorigin.Normalize(raw)
	if normalizeErr != nil {
		// Local path remotes are operational host state, never portable source
		// identity. Omit them without retaining or reporting the raw value.
		if !strings.Contains(raw, "://") && !strings.Contains(raw, "@") {
			return "", nil
		}
		return "", errors.New("history repository origin is not canonicalizable")
	}
	if strings.HasPrefix(canonical, "file://") {
		return "", nil
	}
	return canonical, nil
}

func historySourceID(repositoryID, revision string) string {
	return rkcmodel.StableID(
		"history-source",
		repositoryID,
		revision,
		RevisionPolicyExactHead,
		AncestryPolicyFirstParent,
	)
}

func resolveHistoryRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("repository path is empty or invalid")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect repository: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("repository must be a real directory, not a symlink")
	}
	return absolute, nil
}
