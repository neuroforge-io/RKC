package rkcstore

import (
	"context"
	"encoding/json"
	"strings"
)

// MemoryOptions bounds all mutable, caller-controlled MemoryStore staging
// state. Byte limits measure the UTF-8 bytes in metadata keys and values or the
// compact JSON encoding of records. They do not attempt to estimate Go runtime
// allocator overhead.
type MemoryOptions struct {
	MaxMetadataKeys          int   `json:"max_metadata_keys"`
	MaxMetadataBytes         int64 `json:"max_metadata_bytes"`
	MaxOpenBuilds            int   `json:"max_open_builds"`
	MaxRecordBytes           int64 `json:"max_record_bytes"`
	MaxBatchBytes            int64 `json:"max_batch_bytes"`
	MaxBuildRecords          int64 `json:"max_build_records"`
	MaxBuildBytes            int64 `json:"max_build_bytes"`
	MaxClosedBuildTombstones int   `json:"max_closed_build_tombstones"`
}

// DefaultMemoryOptions returns conservative limits for the in-process
// reference backend. Callers with a deliberately larger workload can construct
// a store with explicitly reviewed limits through NewMemoryStoreWithOptions.
func DefaultMemoryOptions() MemoryOptions {
	return MemoryOptions{
		MaxMetadataKeys:          256,
		MaxMetadataBytes:         64 << 10,
		MaxOpenBuilds:            4,
		MaxRecordBytes:           4 << 20,
		MaxBatchBytes:            16 << 20,
		MaxBuildRecords:          200_000,
		MaxBuildBytes:            128 << 20,
		MaxClosedBuildTombstones: 1_024,
	}
}

func validateMemoryOptions(options MemoryOptions) error {
	const operation = "create memory store"
	positiveInts := []struct {
		field string
		value int
	}{
		{"max_metadata_keys", options.MaxMetadataKeys},
		{"max_open_builds", options.MaxOpenBuilds},
		{"max_closed_build_tombstones", options.MaxClosedBuildTombstones},
	}
	for _, candidate := range positiveInts {
		if candidate.value <= 0 {
			return invalidArgument(operation, candidate.field, "limit must be positive")
		}
	}
	positiveInt64s := []struct {
		field string
		value int64
	}{
		{"max_metadata_bytes", options.MaxMetadataBytes},
		{"max_record_bytes", options.MaxRecordBytes},
		{"max_batch_bytes", options.MaxBatchBytes},
		{"max_build_records", options.MaxBuildRecords},
		{"max_build_bytes", options.MaxBuildBytes},
	}
	for _, candidate := range positiveInt64s {
		if candidate.value <= 0 {
			return invalidArgument(operation, candidate.field, "limit must be positive")
		}
	}
	if options.MaxRecordBytes > options.MaxBatchBytes {
		return invalidArgument(operation, "max_record_bytes", "limit must not exceed max_batch_bytes")
	}
	if options.MaxBatchBytes > options.MaxBuildBytes {
		return invalidArgument(operation, "max_batch_bytes", "limit must not exceed max_build_bytes")
	}
	return nil
}

func validateMetadata(operation string, values map[string]string, options MemoryOptions) error {
	if len(values) > options.MaxMetadataKeys {
		return resourceExhausted(operation, "", "metadata", "metadata exceeds MaxMetadataKeys")
	}
	var total int64
	for key, value := range values {
		size := int64(len(key)) + int64(len(value))
		if size > options.MaxMetadataBytes-total {
			return resourceExhausted(operation, "", "metadata", "metadata exceeds MaxMetadataBytes")
		}
		total += size
	}
	return nil
}

type preparedBatch[T any] struct {
	values  []T
	records int64
	bytes   int64
}

func prepareLimitedBatch[T any](
	ctx context.Context,
	operation string,
	values []T,
	identifier func(T) string,
	options MemoryOptions,
) (preparedBatch[T], error) {
	if err := checkContext(ctx, operation); err != nil {
		return preparedBatch[T]{}, err
	}
	if len(values) > MaxBatchSize {
		return preparedBatch[T]{}, resourceExhausted(operation, "", "values", "batch exceeds MaxBatchSize")
	}
	if int64(len(values)) > options.MaxBuildRecords {
		return preparedBatch[T]{}, resourceExhausted(operation, "", "values", "batch exceeds MaxBuildRecords")
	}
	prepared := preparedBatch[T]{values: make([]T, 0, len(values)), records: int64(len(values))}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := identifier(value)
		if err := validIdentifier(operation, "record_id", id); err != nil {
			return preparedBatch[T]{}, err
		}
		if _, duplicate := seen[id]; duplicate {
			return preparedBatch[T]{}, conflict(operation, "", "", "duplicate record identifier %q in batch", id)
		}
		seen[id] = struct{}{}
		cloned, size, err := prepareLimitedRecord(operation, "values", value, options)
		if err != nil {
			return preparedBatch[T]{}, err
		}
		if size > options.MaxBatchBytes-prepared.bytes {
			return preparedBatch[T]{}, resourceExhausted(operation, "", "values", "batch exceeds MaxBatchBytes")
		}
		prepared.values = append(prepared.values, cloned)
		prepared.bytes += size
	}
	if err := checkContext(ctx, operation); err != nil {
		return preparedBatch[T]{}, err
	}
	return prepared, nil
}

func prepareLimitedRecord[T any](operation, field string, value T, options MemoryOptions) (T, int64, error) {
	var cloned T
	data, err := json.Marshal(value)
	if err != nil {
		return cloned, 0, invalidArgument(operation, field, "record is not canonically serializable: "+err.Error())
	}
	size := int64(len(data))
	if size > options.MaxRecordBytes {
		return cloned, 0, resourceExhausted(operation, "", field, "record exceeds MaxRecordBytes")
	}
	if size > options.MaxBatchBytes {
		return cloned, 0, resourceExhausted(operation, "", field, "record exceeds MaxBatchBytes")
	}
	if err := json.Unmarshal(data, &cloned); err != nil {
		return cloned, 0, invalidArgument(operation, field, "record is not canonically serializable: "+err.Error())
	}
	return cloned, size, nil
}

func ensureBuildCapacity(operation string, buildID BuildID, build *memoryBuild, records, bytes int64, options MemoryOptions) error {
	if records > options.MaxBuildRecords-build.recordCount {
		return resourceExhausted(operation, buildID, "values", "build exceeds MaxBuildRecords")
	}
	if bytes > options.MaxBuildBytes-build.recordBytes {
		return resourceExhausted(operation, buildID, "values", "build exceeds MaxBuildBytes")
	}
	return nil
}

func limitedAbortReason(reason error, maximum int64) string {
	if reason == nil {
		return ""
	}
	message := reason.Error()
	if int64(len(message)) > maximum {
		message = message[:maximum]
	}
	return strings.Clone(message)
}
