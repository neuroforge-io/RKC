package scipindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// Inspection is the digest-bound structural summary of one SCIP index. It is
// produced without a repository inventory, so it cannot verify artifact
// binding; the full Extract path performs that. Both the scip verify command
// and the post-generation check use it to prove that an index is well-formed
// before it is trusted or published.
type Inspection struct {
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
	Tool              string `json:"tool,omitempty"`
	ToolVersion       string `json:"tool_version,omitempty"`
	ProjectRoot       string `json:"project_root,omitempty"`
	TextEncoding      string `json:"text_encoding,omitempty"`
	Documents         int    `json:"documents"`
	Symbols           int    `json:"symbols"`
	Occurrences       int    `json:"occurrences"`
	ExternalDocuments int    `json:"external_documents_skipped"`
	InvalidDocuments  int    `json:"invalid_documents"`
}

// textEncodingName renders the metadata text-encoding enum for inspection.
func textEncodingName(encoding int32) string {
	switch encoding {
	case 0:
		return "unspecified"
	case 1:
		return "utf8"
	case 2:
		return "utf16"
	case 3:
		return "utf32"
	default:
		return fmt.Sprintf("unsupported(%d)", encoding)
	}
}

// Inspect strictly validates a prepared SCIP input without requiring a
// repository: wire format, field ordering, bounds, metadata, document paths,
// position encodings, occurrence ranges, and symbol records are all parsed
// with the same fail-closed rules as Extract. SCIP requires every document to
// be canonical and repository-relative; invalid documents fail inspection.
func Inspect(ctx context.Context, input Input) (Inspection, error) {
	if ctx == nil {
		return Inspection{}, errors.New("SCIP inspection context is required")
	}
	if input.Path == "" || input.SHA256 == "" || input.SizeBytes < 0 {
		return Inspection{}, errors.New("SCIP inspection input is invalid")
	}
	before, err := os.Lstat(input.Path)
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect SCIP index %q: %w", input.Path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() != input.SizeBytes {
		return Inspection{}, fmt.Errorf("SCIP index %q does not match its prepared input", input.Path)
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return Inspection{}, fmt.Errorf("open SCIP index %q: %w", input.Path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, opened) {
		return Inspection{}, fmt.Errorf("SCIP index %q changed while opening", input.Path)
	}
	result := Inspection{Path: input.Path, SHA256: input.SHA256, SizeBytes: input.SizeBytes}
	hasher := sha256.New()
	reader := newWireReader(
		io.TeeReader(&contextReader{ctx: ctx, reader: file}, hasher),
		input.SizeBytes,
	)
	metadataSeen := false
	firstField := true
	for {
		field, wire, done, err := reader.next()
		if err != nil {
			return Inspection{}, fmt.Errorf("decode SCIP index %q: %w", input.Path, err)
		}
		if done {
			break
		}
		if firstField && field != 1 {
			return Inspection{}, fmt.Errorf("decode SCIP index %q: metadata must be the first field", input.Path)
		}
		firstField = false
		switch field {
		case 1:
			if metadataSeen {
				return Inspection{}, fmt.Errorf("decode SCIP index %q: metadata appears more than once", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return Inspection{}, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP metadata in %q: %w", input.Path, err)
			}
			metadata, err := parseMetadata(message)
			if err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP metadata in %q: %w", input.Path, err)
			}
			if _, err := projectRootDigest(metadata.projectRoot); err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP metadata in %q: %w", input.Path, err)
			}
			result.ProjectRoot = metadata.projectRoot
			result.Tool = metadata.toolName
			result.ToolVersion = metadata.toolVersion
			result.TextEncoding = textEncodingName(metadata.textEncoding)
			metadataSeen = true
		case 2:
			if !metadataSeen {
				return Inspection{}, fmt.Errorf("decode SCIP index %q: document precedes metadata", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return Inspection{}, err
			}
			message, err := reader.bytes(maximumDocumentBytes)
			if err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP document in %q: %w", input.Path, err)
			}
			document, err := parseDocument(message)
			if err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP document in %q: %w", input.Path, err)
			}
			result.Documents++
			if result.Documents > maximumDocuments {
				return Inspection{}, fmt.Errorf("SCIP inputs exceed the %d-document limit", maximumDocuments)
			}
			result.Symbols += len(document.symbols)
			result.Occurrences += len(document.occurrences)
			if result.Symbols > maximumSymbols {
				return Inspection{}, fmt.Errorf("SCIP inputs exceed the %d-symbol limit", maximumSymbols)
			}
			if result.Occurrences > maximumOccurrences {
				return Inspection{}, fmt.Errorf("SCIP inputs exceed the %d-occurrence limit", maximumOccurrences)
			}
			canonical, contained := classifyDocumentPath(document.path)
			if !canonical || !contained {
				result.InvalidDocuments++
				return Inspection{}, fmt.Errorf(
					"SCIP document %q has a non-canonical relative path", document.path,
				)
			}
			if document.positionEncoding < 1 || document.positionEncoding > 3 {
				return Inspection{}, fmt.Errorf(
					"SCIP document %q has unsupported position_encoding %d",
					document.path, document.positionEncoding,
				)
			}
			for index, occurrence := range document.occurrences {
				if _, _, err := occurrenceRange(occurrence); err != nil {
					return Inspection{}, fmt.Errorf(
						"SCIP document %q occurrence %d has an invalid range: %w",
						document.path, index, err,
					)
				}
			}
		case 3:
			if !metadataSeen {
				return Inspection{}, fmt.Errorf("decode SCIP index %q: external symbol precedes metadata", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return Inspection{}, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP external symbol in %q: %w", input.Path, err)
			}
			if _, err := parseSymbolInformation(message); err != nil {
				return Inspection{}, fmt.Errorf("decode SCIP external symbol in %q: %w", input.Path, err)
			}
		default:
			if err := reader.skip(wire); err != nil {
				return Inspection{}, fmt.Errorf("skip SCIP field %d in %q: %w", field, input.Path, err)
			}
		}
	}
	if !metadataSeen {
		return Inspection{}, fmt.Errorf("decode SCIP index %q: metadata is missing", input.Path)
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != input.SHA256 {
		return Inspection{}, fmt.Errorf(
			"SCIP index %q digest changed: got %s, want %s",
			input.Path, actualDigest, input.SHA256,
		)
	}
	return result, nil
}
