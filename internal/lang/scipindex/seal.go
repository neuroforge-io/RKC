package scipindex

import (
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

	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
)

const (
	maximumSnapshotFileBytes = int64(1 << 30)
	maximumSnapshotBytes     = int64(20 << 30)
	maximumSnapshotFiles     = 500000
)

// SourceSnapshot is a process-local admission record captured before a
// compiler indexer starts. Its unexported identities cannot be reconstructed
// from an editable sidecar and are used only to seal the index produced by
// that exact invocation.
type SourceSnapshot struct {
	root       string
	rootInfo   os.FileInfo
	identities map[string]sourceIdentity
	entries    map[string]string
}

// CaptureSourceSnapshot hashes the bounded repository before an indexer is
// launched. Generated and dependency directories use the same explicit
// exclusions as an ordinary RKC scan.
func CaptureSourceSnapshot(ctx context.Context, root string) (SourceSnapshot, error) {
	if ctx == nil {
		return SourceSnapshot{}, errors.New("SCIP source snapshot context is required")
	}
	if err := ctx.Err(); err != nil {
		return SourceSnapshot{}, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("resolve SCIP source snapshot root: %w", err)
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("inspect SCIP source snapshot root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return SourceSnapshot{}, errors.New("SCIP source snapshot root must be a real directory")
	}
	result, err := inventory.Scan(inventory.Options{
		Root: absolute, MaxFileBytes: maximumSnapshotFileBytes,
		MaxTextBytes: 2 << 20, MaxRepositoryBytes: maximumSnapshotBytes,
		MaxFiles: maximumSnapshotFiles, Excludes: inventory.DefaultExclusions(),
	})
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("inventory pre-index repository: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return SourceSnapshot{}, err
	}
	identities := make(map[string]sourceIdentity, len(result.Artifacts))
	entries := make(map[string]string, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		encoded, err := json.Marshal(artifact)
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("encode pre-index artifact identity: %w", err)
		}
		entries[artifact.Path] = string(encoded)
		if artifact.SHA256 == "" || artifact.Status == "excluded" {
			continue
		}
		identities[artifact.Path] = sourceIdentity{
			path: artifact.Path, sha256: artifact.SHA256, sizeBytes: artifact.SizeBytes,
		}
	}
	return SourceSnapshot{
		root: absolute, rootInfo: rootInfo, identities: identities, entries: entries,
	}, nil
}

// SealRepositorySources writes a canonical copy of input in which every SCIP
// Document embeds the exact repository bytes. Each document must have existed
// with the same SHA-256 and size in the pre-index SourceSnapshot. This proves
// the pinned compiler ran between two identical document-source observations;
// a portable post-hoc manifest alone is deliberately insufficient.
func SealRepositorySources(
	ctx context.Context,
	root string,
	input Input,
	snapshot SourceSnapshot,
	output io.Writer,
) error {
	if ctx == nil || output == nil {
		return errors.New("SCIP source sealing requires a context and output")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve SCIP source sealing root: %w", err)
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil || snapshot.root == "" || absolute != snapshot.root || snapshot.rootInfo == nil ||
		rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || !os.SameFile(snapshot.rootInfo, rootInfo) {
		return errors.New("SCIP repository root changed after source admission")
	}
	if err := verifyCompleteSourceSnapshot(ctx, snapshot, input.Path); err != nil {
		return err
	}
	before, err := os.Lstat(input.Path)
	if err != nil {
		return fmt.Errorf("inspect SCIP index for source sealing: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() != input.SizeBytes {
		return errors.New("SCIP index no longer matches its prepared input")
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return fmt.Errorf("open SCIP index for source sealing: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, opened) {
		return errors.New("SCIP index changed while opening for source sealing")
	}
	hasher := sha256.New()
	reader := newWireReader(
		io.TeeReader(&contextReader{ctx: ctx, reader: file}, hasher),
		input.SizeBytes,
	)
	bounded := &maximumWriter{writer: output, maximum: MaximumIndexBytes}
	metadataSeen := false
	for {
		field, wire, done, err := reader.next()
		if err != nil {
			return fmt.Errorf("decode SCIP index for source sealing: %w", err)
		}
		if done {
			break
		}
		if field == 1 {
			if metadataSeen {
				return errors.New("SCIP source sealing found duplicate metadata")
			}
			metadataSeen = true
		}
		if field == 2 {
			if !metadataSeen || wire != 2 {
				return errors.New("SCIP source sealing found invalid document ordering")
			}
			documentBytes, err := readWireBytes(reader, maximumDocumentBytes)
			if err != nil {
				return fmt.Errorf("read SCIP document for source sealing: %w", err)
			}
			sealed, err := sealDocumentSource(ctx, absolute, snapshot, documentBytes)
			if err != nil {
				return err
			}
			if err := writeWireKey(bounded, field, wire); err != nil {
				return err
			}
			if err := writeProtoVarint(bounded, uint64(len(sealed))); err != nil {
				return err
			}
			if _, err := bounded.Write(sealed); err != nil {
				return err
			}
			continue
		}
		if err := writeWireKey(bounded, field, wire); err != nil {
			return err
		}
		if err := copyWireValue(reader, bounded, wire); err != nil {
			return fmt.Errorf("copy SCIP field for source sealing: %w", err)
		}
	}
	if !metadataSeen {
		return errors.New("SCIP source sealing requires metadata")
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != input.SHA256 {
		return errors.New("SCIP index changed while sealing repository sources")
	}
	after, err := os.Lstat(input.Path)
	if err != nil || !sameFileSnapshot(before, after) {
		return errors.New("SCIP index changed while sealing repository sources")
	}
	if err := verifyCompleteSourceSnapshot(ctx, snapshot, input.Path); err != nil {
		return err
	}
	return nil
}

// verifyCompleteSourceSnapshot proves that every admitted repository artifact,
// including compiler-affecting configuration not named as a SCIP document,
// remained identical across indexing. Only the exact generated index output is
// omitted; unrelated generated or modified files fail closed.
func verifyCompleteSourceSnapshot(ctx context.Context, snapshot SourceSnapshot, generatedPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	generatedAbsolute, err := filepath.Abs(generatedPath)
	if err != nil {
		return fmt.Errorf("resolve generated SCIP output: %w", err)
	}
	generatedRelative, err := filepath.Rel(snapshot.root, generatedAbsolute)
	if err != nil {
		return fmt.Errorf("relativize generated SCIP output: %w", err)
	}
	generatedRelative = filepath.ToSlash(generatedRelative)
	if generatedRelative == "." || generatedRelative == ".." ||
		filepath.IsAbs(generatedRelative) || strings.HasPrefix(generatedRelative, "../") {
		generatedRelative = ""
	}
	excludes := inventory.DefaultExclusions()
	if generatedRelative != "" {
		excludes = append(excludes, generatedRelative)
	}
	result, err := inventory.Scan(inventory.Options{
		Root: snapshot.root, MaxFileBytes: maximumSnapshotFileBytes,
		MaxTextBytes: 2 << 20, MaxRepositoryBytes: maximumSnapshotBytes,
		MaxFiles: maximumSnapshotFiles, Excludes: excludes,
	})
	if err != nil {
		return fmt.Errorf("inventory post-index repository: %w", err)
	}
	actual := make(map[string]string, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if artifact.Path == generatedRelative {
			continue
		}
		encoded, err := json.Marshal(artifact)
		if err != nil {
			return fmt.Errorf("encode post-index artifact identity: %w", err)
		}
		actual[artifact.Path] = string(encoded)
	}
	if len(actual) != len(snapshot.entries) {
		return errors.New("repository artifact set changed during SCIP indexing")
	}
	for path, expected := range snapshot.entries {
		if actual[path] != expected {
			return fmt.Errorf("repository artifact %q changed during SCIP indexing", path)
		}
	}
	return nil
}

func sealDocumentSource(
	ctx context.Context,
	root string,
	snapshot SourceSnapshot,
	message []byte,
) ([]byte, error) {
	document, err := parseDocument(message)
	if err != nil {
		return nil, fmt.Errorf("decode SCIP document for source sealing: %w", err)
	}
	canonical, contained := classifyDocumentPath(document.path)
	if !canonical || !contained {
		return nil, fmt.Errorf("SCIP document %q cannot be source sealed", document.path)
	}
	admitted, ok := snapshot.identities[document.path]
	if !ok {
		return nil, fmt.Errorf("SCIP document %q was not present in the pre-index source snapshot", document.path)
	}
	source, err := readAdmittedSource(ctx, root, admitted)
	if err != nil {
		return nil, fmt.Errorf("seal SCIP document %q: %w", document.path, err)
	}
	if document.textPresent {
		if len(document.text) != len(source) || string(source) != document.text {
			return nil, fmt.Errorf("SCIP document %q compiler-emitted text does not match the admitted source", document.path)
		}
		return message, nil
	}
	extra := 1 + protoVarintLength(uint64(len(source))) + len(source)
	if int64(len(message)+extra) > maximumDocumentBytes {
		return nil, fmt.Errorf("SCIP document %q exceeds the %d-byte sealed-document limit", document.path, maximumDocumentBytes)
	}
	sealed := make([]byte, 0, len(message)+extra)
	sealed = append(sealed, message...)
	sealed = appendProtoVarint(sealed, uint64(5<<3|2))
	sealed = appendProtoVarint(sealed, uint64(len(source)))
	sealed = append(sealed, source...)
	return sealed, nil
}

func readAdmittedSource(ctx context.Context, root string, admitted sourceIdentity) ([]byte, error) {
	file, err := sourcepath.OpenRegular(root, admitted.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != admitted.sizeBytes || before.Size() > maximumDocumentBytes {
		return nil, errors.New("source size changed after pre-index admission")
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, before.Size()+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, after) || int64(len(data)) != admitted.sizeBytes {
		return nil, errors.New("source changed while sealing")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != admitted.sha256 {
		return nil, errors.New("source content changed after pre-index admission")
	}
	return data, nil
}

type maximumWriter struct {
	writer  io.Writer
	written int64
	maximum int64
}

func (writer *maximumWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.maximum-writer.written {
		return 0, fmt.Errorf("sealed SCIP index exceeds the %d-byte limit", writer.maximum)
	}
	written, err := writer.writer.Write(data)
	writer.written += int64(written)
	return written, err
}

func readWireBytes(reader *wireReader, limit int64) ([]byte, error) {
	length, err := reader.varint()
	if err != nil {
		return nil, err
	}
	if length > uint64(limit) || length > uint64(reader.remaining) {
		return nil, errors.New("protobuf length-delimited field exceeds its bound")
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(reader.reader, data); err != nil {
		return nil, err
	}
	reader.remaining -= int64(length)
	return data, nil
}

func copyWireValue(reader *wireReader, output io.Writer, wire int) error {
	switch wire {
	case 0:
		value, err := reader.varint()
		if err != nil {
			return err
		}
		return writeProtoVarint(output, value)
	case 1:
		return copyWireFixed(reader, output, 8)
	case 2:
		length, err := reader.varint()
		if err != nil {
			return err
		}
		if length > uint64(reader.remaining) {
			return io.ErrUnexpectedEOF
		}
		if err := writeProtoVarint(output, length); err != nil {
			return err
		}
		return copyWireFixed(reader, output, int64(length))
	case 5:
		return copyWireFixed(reader, output, 4)
	case 3, 4:
		return errors.New("protobuf groups are not supported")
	default:
		return fmt.Errorf("unsupported protobuf wire type %d", wire)
	}
}

func copyWireFixed(reader *wireReader, output io.Writer, count int64) error {
	if count < 0 || count > reader.remaining {
		return io.ErrUnexpectedEOF
	}
	written, err := io.CopyN(output, reader.reader, count)
	reader.remaining -= written
	return err
}

func writeWireKey(output io.Writer, field, wire int) error {
	return writeProtoVarint(output, uint64(field<<3|wire))
}

func writeProtoVarint(output io.Writer, value uint64) error {
	var encoded [10]byte
	length := 0
	for value >= 0x80 {
		encoded[length] = byte(value) | 0x80
		value >>= 7
		length++
	}
	encoded[length] = byte(value)
	_, err := output.Write(encoded[:length+1])
	return err
}

func appendProtoVarint(output []byte, value uint64) []byte {
	for value >= 0x80 {
		output = append(output, byte(value)|0x80)
		value >>= 7
	}
	return append(output, byte(value))
}

func protoVarintLength(value uint64) int {
	length := 1
	for value >= 0x80 {
		value >>= 7
		length++
	}
	return length
}
