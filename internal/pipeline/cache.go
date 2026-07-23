package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	stagePayloadSchemaVersion = "1.0"
	maximumStagePayloadBytes  = int64(256 << 20)
)

// StageCache stores immutable stage payloads in CAS and deterministic cache
// pointers separately. A corrupt or missing object never becomes a cache hit.
type StageCache struct {
	root     string
	metadata *scheduler.FileCache
	objects  *cas.Store
}

type stageOwnership struct {
	ArtifactIDs   []string `json:"artifact_ids,omitempty"`
	NodeIDs       []string `json:"node_ids,omitempty"`
	EdgeIDs       []string `json:"edge_ids,omitempty"`
	EvidenceIDs   []string `json:"evidence_ids,omitempty"`
	DiagnosticIDs []string `json:"diagnostic_ids,omitempty"`
	ConflictIDs   []string `json:"conflict_ids,omitempty"`
	DocumentIDs   []string `json:"document_ids,omitempty"`
	ClaimIDs      []string `json:"claim_ids,omitempty"`
	PathIDs       []string `json:"path_ids,omitempty"`
}

type stageFragmentPayload struct {
	SchemaVersion     string            `json:"schema_version"`
	StageID           string            `json:"stage_id"`
	StageVersion      string            `json:"stage_version"`
	Fragment          rkcmodel.Fragment `json:"fragment"`
	ParsedArtifactIDs []string          `json:"parsed_artifact_ids,omitempty"`
	Ownership         stageOwnership    `json:"ownership"`
}

type StageCacheEntry struct {
	CacheKey      string    `json:"cache_key"`
	StageID       string    `json:"stage_id"`
	ObjectDigest  string    `json:"object_digest"`
	MetadataBytes int64     `json:"metadata_bytes"`
	PayloadBytes  int64     `json:"payload_bytes"`
	LastAccessed  time.Time `json:"last_accessed"`
	Valid         bool      `json:"valid"`
	Issue         string    `json:"issue,omitempty"`
}

type StageCacheReport struct {
	Root           string            `json:"root"`
	Healthy        bool              `json:"healthy"`
	Entries        []StageCacheEntry `json:"entries"`
	EntryCount     int               `json:"entry_count"`
	ValidEntries   int               `json:"valid_entries"`
	InvalidEntries int               `json:"invalid_entries"`
	ObjectCount    int               `json:"object_count"`
	OrphanObjects  int               `json:"orphan_objects"`
	MetadataBytes  int64             `json:"metadata_bytes"`
	PayloadBytes   int64             `json:"payload_bytes"`
}

type StageCachePruneOptions struct {
	OlderThan time.Duration
	All       bool
	DryRun    bool
	Now       time.Time
}

type StageCachePruneReport struct {
	Root             string `json:"root"`
	DryRun           bool   `json:"dry_run"`
	EntriesSelected  int    `json:"entries_selected"`
	ObjectsSelected  int    `json:"objects_selected"`
	MetadataBytes    int64  `json:"metadata_bytes"`
	PayloadBytes     int64  `json:"payload_bytes"`
	EntriesRemaining int    `json:"entries_remaining"`
}

func OpenStageCache(root string) (*StageCache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("stage cache root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve stage cache root: %w", err)
	}
	objects, err := cas.Open(filepath.Join(absolute, "objects"))
	if err != nil {
		return nil, fmt.Errorf("open stage cache objects: %w", err)
	}
	metadata, err := scheduler.OpenFileCache(filepath.Join(absolute, "entries"))
	if err != nil {
		return nil, fmt.Errorf("open stage cache metadata: %w", err)
	}
	return &StageCache{root: absolute, metadata: metadata, objects: objects}, nil
}

func (cache *StageCache) Root() string {
	if cache == nil {
		return ""
	}
	return cache.root
}

func (cache *StageCache) Load(
	ctx context.Context,
	key string,
) (scheduler.Result, bool, error) {
	if cache == nil {
		return scheduler.Result{}, false, errors.New("stage cache is nil")
	}
	result, ok, err := cache.metadata.Load(ctx, key)
	if err != nil || !ok {
		return result, ok, err
	}
	if result.CacheKey != key || strings.TrimSpace(result.StageID) == "" {
		if err := cache.metadata.Delete(ctx, key); err != nil {
			return scheduler.Result{}, false, fmt.Errorf("discard invalid cache pointer: %w", err)
		}
		return scheduler.Result{}, false, nil
	}
	digest, err := cas.NormalizeDigest(result.ObjectDigest)
	if err != nil {
		if deleteErr := cache.metadata.Delete(ctx, key); deleteErr != nil {
			return scheduler.Result{}, false, errors.Join(err, deleteErr)
		}
		return scheduler.Result{}, false, nil
	}
	present, err := cache.objects.Has(digest)
	if err != nil {
		if errors.Is(err, cas.ErrDigestMismatch) || errors.Is(err, fs.ErrNotExist) {
			if deleteErr := cache.metadata.Delete(ctx, key); deleteErr != nil {
				return scheduler.Result{}, false, errors.Join(err, deleteErr)
			}
			if errors.Is(err, cas.ErrDigestMismatch) {
				_ = cache.objects.Delete(digest)
			}
			return scheduler.Result{}, false, nil
		}
		return scheduler.Result{}, false, fmt.Errorf("verify cached stage object: %w", err)
	}
	if !present {
		if err := cache.metadata.Delete(ctx, key); err != nil {
			return scheduler.Result{}, false, fmt.Errorf("discard missing cache pointer: %w", err)
		}
		return scheduler.Result{}, false, nil
	}
	return result, true, nil
}

func (cache *StageCache) probe(
	ctx context.Context,
	key string,
	stageID string,
) (bool, string, error) {
	if cache == nil {
		return false, "stage cache is nil", nil
	}
	result, ok, err := cache.metadata.Load(ctx, key)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "", nil
	}
	if result.CacheKey != key {
		return false, "metadata cache key mismatch", nil
	}
	if result.StageID != stageID {
		return false, "metadata stage mismatch", nil
	}
	if _, err := cas.NormalizeDigest(result.ObjectDigest); err != nil {
		return false, err.Error(), nil
	}
	payload, err := cache.readPayload(result.ObjectDigest)
	if err != nil {
		return false, err.Error(), nil
	}
	if payload.StageID != stageID {
		return false, "payload stage mismatch", nil
	}
	return true, "", nil
}

func (cache *StageCache) Store(
	ctx context.Context,
	key string,
	result scheduler.Result,
) error {
	if cache == nil {
		return errors.New("stage cache is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.CacheKey != key || strings.TrimSpace(result.StageID) == "" {
		return errors.New("stage cache result is not bound to its key and stage")
	}
	digest, err := cas.NormalizeDigest(result.ObjectDigest)
	if err != nil {
		return fmt.Errorf("stage cache object digest: %w", err)
	}
	present, err := cache.objects.Has(digest)
	if err != nil {
		return fmt.Errorf("verify stage cache object before metadata publication: %w", err)
	}
	if !present {
		return fmt.Errorf("stage cache object %s is missing", digest)
	}
	result.CacheHit = false
	result.DoNotCache = false
	return cache.metadata.Store(ctx, key, result)
}

func (cache *StageCache) Invalidate(ctx context.Context, key string) error {
	if cache == nil {
		return errors.New("stage cache is nil")
	}
	return cache.metadata.Delete(ctx, key)
}

func (cache *StageCache) putPayload(payload []byte) (string, error) {
	if int64(len(payload)) > maximumStagePayloadBytes {
		return "", fmt.Errorf(
			"stage payload is %d bytes; maximum is %d",
			len(payload), maximumStagePayloadBytes,
		)
	}
	if cache == nil {
		return cas.DigestBytes(payload), nil
	}
	object, err := cache.objects.PutBytes(payload)
	if err != nil {
		return "", err
	}
	return object.Digest, nil
}

func (cache *StageCache) readPayload(digest string) (stageFragmentPayload, error) {
	if cache == nil {
		return stageFragmentPayload{}, errors.New("stage cache is nil")
	}
	data, err := cache.objects.ReadBytesBounded(digest, true, maximumStagePayloadBytes)
	if err != nil {
		return stageFragmentPayload{}, err
	}
	var payload stageFragmentPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return stageFragmentPayload{}, fmt.Errorf("decode stage payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return stageFragmentPayload{}, errors.New("stage payload contains multiple JSON values")
		}
		return stageFragmentPayload{}, fmt.Errorf("decode trailing stage payload: %w", err)
	}
	if payload.SchemaVersion != stagePayloadSchemaVersion {
		return stageFragmentPayload{}, fmt.Errorf(
			"stage payload schema %q is unsupported",
			payload.SchemaVersion,
		)
	}
	if strings.TrimSpace(payload.StageID) == "" ||
		payload.StageVersion != pipelineStageVersion {
		return stageFragmentPayload{}, errors.New("stage payload identity is invalid")
	}
	if got := ownershipForFragment(payload.Fragment); !equalOwnership(got, payload.Ownership) {
		return stageFragmentPayload{}, errors.New("stage payload ownership does not match its fragment")
	}
	return payload, nil
}

func (cache *StageCache) Inspect(ctx context.Context, verify bool) (StageCacheReport, error) {
	if cache == nil {
		return StageCacheReport{}, errors.New("stage cache is nil")
	}
	metadataEntries, err := cache.metadata.Entries(ctx)
	if err != nil {
		return StageCacheReport{}, err
	}
	report := StageCacheReport{Root: cache.root, Healthy: true}
	referenced := make(map[string]struct{}, len(metadataEntries))
	for _, metadata := range metadataEntries {
		entry := StageCacheEntry{
			CacheKey: metadata.Key, StageID: metadata.Result.StageID,
			ObjectDigest: metadata.Result.ObjectDigest, MetadataBytes: metadata.SizeBytes,
			LastAccessed: metadata.LastAccessed, Valid: true,
		}
		report.MetadataBytes += metadata.SizeBytes
		if metadata.Result.CacheKey != metadata.Key {
			entry.Valid = false
			entry.Issue = "metadata cache key mismatch"
		} else if digest, normalizeErr := cas.NormalizeDigest(metadata.Result.ObjectDigest); normalizeErr != nil {
			entry.Valid = false
			entry.Issue = normalizeErr.Error()
		} else {
			referenced[digest] = struct{}{}
			path, pathErr := cache.objects.Path(digest)
			if pathErr != nil {
				entry.Valid = false
				entry.Issue = pathErr.Error()
			} else if info, statErr := os.Lstat(path); statErr != nil {
				entry.Valid = false
				entry.Issue = statErr.Error()
			} else if !info.Mode().IsRegular() {
				entry.Valid = false
				entry.Issue = "payload is not a regular file"
			} else {
				entry.PayloadBytes = info.Size()
				if info.Size() > maximumStagePayloadBytes {
					entry.Valid = false
					entry.Issue = fmt.Sprintf(
						"payload exceeds %d bytes",
						maximumStagePayloadBytes,
					)
				} else if verify {
					payload, payloadErr := cache.readPayload(digest)
					if payloadErr != nil {
						entry.Valid = false
						entry.Issue = payloadErr.Error()
					} else if payload.StageID != metadata.Result.StageID {
						entry.Valid = false
						entry.Issue = "payload stage does not match metadata"
					}
				}
			}
		}
		if entry.Valid {
			report.ValidEntries++
		} else {
			report.InvalidEntries++
			report.Healthy = false
		}
		report.Entries = append(report.Entries, entry)
	}
	report.EntryCount = len(report.Entries)
	seenPayloads := map[string]struct{}{}
	if err := cache.objects.Walk(func(object cas.ObjectInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		report.ObjectCount++
		if _, duplicate := seenPayloads[object.Digest]; !duplicate {
			report.PayloadBytes += object.Size
			seenPayloads[object.Digest] = struct{}{}
		}
		if _, ok := referenced[object.Digest]; !ok {
			report.OrphanObjects++
		}
		return nil
	}); err != nil {
		return StageCacheReport{}, err
	}
	return report, nil
}

func (cache *StageCache) Prune(
	ctx context.Context,
	options StageCachePruneOptions,
) (StageCachePruneReport, error) {
	if cache == nil {
		return StageCachePruneReport{}, errors.New("stage cache is nil")
	}
	if !options.All && options.OlderThan <= 0 {
		return StageCachePruneReport{}, errors.New("cache prune requires a positive age or all=true")
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	entries, err := cache.metadata.Entries(ctx)
	if err != nil {
		return StageCachePruneReport{}, err
	}
	selected := map[string]scheduler.FileCacheEntry{}
	remainingDigests := map[string]struct{}{}
	cutoff := options.Now.Add(-options.OlderThan)
	for _, entry := range entries {
		if options.All || entry.LastAccessed.Before(cutoff) {
			selected[entry.Key] = entry
			continue
		}
		if digest, err := cas.NormalizeDigest(entry.Result.ObjectDigest); err == nil {
			remainingDigests[digest] = struct{}{}
		}
	}
	report := StageCachePruneReport{
		Root: cache.root, DryRun: options.DryRun,
		EntriesSelected: len(selected), EntriesRemaining: len(entries) - len(selected),
	}
	for key, entry := range selected {
		report.MetadataBytes += entry.SizeBytes
		if !options.DryRun {
			if err := cache.metadata.Delete(ctx, key); err != nil {
				return StageCachePruneReport{}, err
			}
		}
	}
	var objects []cas.ObjectInfo
	if err := cache.objects.Walk(func(object cas.ObjectInfo) error {
		objects = append(objects, object)
		return ctx.Err()
	}); err != nil {
		return StageCachePruneReport{}, err
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Digest < objects[j].Digest })
	for _, object := range objects {
		if _, retained := remainingDigests[object.Digest]; retained {
			continue
		}
		report.ObjectsSelected++
		report.PayloadBytes += object.Size
		if !options.DryRun {
			if err := cache.objects.Delete(object.Digest); err != nil {
				return StageCachePruneReport{}, err
			}
		}
	}
	return report, nil
}

func (state *stagedScanState) recordFragment(
	stageID string,
	fragment rkcmodel.Fragment,
	files []pluginapi.FileRef,
	markSyntax bool,
) (scheduler.Result, error) {
	rkcmodel.SortFragment(&fragment)
	parsedIDs := make([]string, 0, len(files))
	if markSyntax {
		for _, file := range files {
			if strings.TrimSpace(file.ArtifactID) == "" {
				continue
			}
			parsedIDs = append(parsedIDs, file.ArtifactID)
		}
		sort.Strings(parsedIDs)
	}
	payload := stageFragmentPayload{
		SchemaVersion:     stagePayloadSchemaVersion,
		StageID:           stageID,
		StageVersion:      pipelineStageVersion,
		Fragment:          fragment,
		ParsedArtifactIDs: parsedIDs,
		Ownership:         ownershipForFragment(fragment),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return scheduler.Result{}, fmt.Errorf("encode %s stage payload: %w", stageID, err)
	}
	digest, err := state.opts.Cache.putPayload(data)
	if err != nil {
		return scheduler.Result{}, fmt.Errorf("store %s stage payload: %w", stageID, err)
	}
	state.mu.Lock()
	state.fragments[stageID] = fragment
	for _, artifactID := range parsedIDs {
		state.parsed[artifactID] = struct{}{}
	}
	state.mu.Unlock()
	return scheduler.Result{
		ObjectDigest: digest,
		Metadata: map[string]any{
			"stage": stageID, "files": len(files), "nodes": len(fragment.Nodes),
			"edges": len(fragment.Edges), "diagnostics": len(fragment.Diagnostics),
			"payload_bytes": len(data),
		},
	}, nil
}

func (state *stagedScanState) restoreFragment(
	ctx context.Context,
	stageID string,
	result scheduler.Result,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := state.opts.Cache.readPayload(result.ObjectDigest)
	if err != nil {
		return fmt.Errorf("%w: %v", scheduler.ErrCacheRejected, err)
	}
	if payload.StageID != stageID || payload.StageVersion != pipelineStageVersion {
		return fmt.Errorf(
			"%w: cached payload is for %s@%s, want %s@%s",
			scheduler.ErrCacheRejected,
			payload.StageID,
			payload.StageVersion,
			stageID,
			pipelineStageVersion,
		)
	}
	knownArtifacts := make(map[string]struct{}, len(state.bundle.Artifacts))
	for _, artifact := range state.bundle.Artifacts {
		knownArtifacts[artifact.ID] = struct{}{}
	}
	for _, artifactID := range payload.ParsedArtifactIDs {
		if _, ok := knownArtifacts[artifactID]; !ok {
			return fmt.Errorf(
				"%w: cached payload references missing artifact %s",
				scheduler.ErrCacheRejected,
				artifactID,
			)
		}
	}
	rkcmodel.SortFragment(&payload.Fragment)
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, artifactID := range payload.ParsedArtifactIDs {
		state.parsed[artifactID] = struct{}{}
	}
	state.fragments[stageID] = payload.Fragment
	return nil
}

func ownershipForFragment(fragment rkcmodel.Fragment) stageOwnership {
	ownership := stageOwnership{
		ArtifactIDs:   idsOfArtifacts(fragment.Artifacts),
		NodeIDs:       idsOfNodes(fragment.Nodes),
		EdgeIDs:       idsOfEdges(fragment.Edges),
		EvidenceIDs:   idsOfEvidence(fragment.Evidence),
		DiagnosticIDs: idsOfDiagnostics(fragment.Diagnostics),
		ConflictIDs:   idsOfConflicts(fragment.Conflicts),
		DocumentIDs:   idsOfDocuments(fragment.Documents),
		ClaimIDs:      idsOfClaims(fragment.Claims),
		PathIDs:       idsOfPaths(fragment.Paths),
	}
	return ownership
}

func equalOwnership(left, right stageOwnership) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func sortedIDs(length int, value func(int) string) []string {
	ids := make([]string, 0, length)
	for index := 0; index < length; index++ {
		if id := strings.TrimSpace(value(index)); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func idsOfArtifacts(values []rkcmodel.Artifact) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfNodes(values []rkcmodel.Node) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfEdges(values []rkcmodel.Edge) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfEvidence(values []rkcmodel.Evidence) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfDiagnostics(values []rkcmodel.Diagnostic) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfConflicts(values []rkcmodel.Conflict) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfDocuments(values []rkcmodel.Document) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfClaims(values []rkcmodel.Claim) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}

func idsOfPaths(values []rkcmodel.ExecutionPath) []string {
	return sortedIDs(len(values), func(index int) string { return values[index].ID })
}
