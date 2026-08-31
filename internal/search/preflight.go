package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	maximumRawJSONStringBytes     = int64(MaximumIndexedDocumentBytes*6 + 1024)
	maximumSearchIdentifierBytes  = 16 * 1024
	maximumDocumentMetadataFields = 64
	maximumPostingFields          = 8
)

type persistedIndexPreflight struct {
	documents          int
	indexedTextBytes   int64
	terms              int
	termDictionaryByte int64
	postings           int
	postingFields      int
	metadataFields     int
	tokenOccurrences   int
}

// PreflightPersistedIndex verifies allocation-driving JSON structure and the
// production resource envelope without materializing the index. The caller
// must pass one stable, seekable file descriptor; the function performs a raw
// string-size pass, seeks back to the beginning, and then streams every map and
// array entry individually. The caller may decode only the same descriptor
// after this returns successfully.
func PreflightPersistedIndex(reader io.ReadSeeker) error {
	if reader == nil {
		return errors.New("search index preflight reader is nil")
	}
	if err := preflightJSONStringSizes(reader); err != nil {
		return err
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind search index after string preflight: %w", err)
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("search index must be one JSON object")
	}
	usage := persistedIndexPreflight{}
	seen := map[string]struct{}{}
	for decoder.More() {
		name, err := nextObjectName(decoder)
		if err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("search index repeats top-level field %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "version", "snapshot_id", "corpus_version":
			if err := preflightBoundedString(decoder, maximumSearchIdentifierBytes, name); err != nil {
				return err
			}
		case "documents":
			if err := preflightDocuments(decoder, &usage); err != nil {
				return err
			}
		case "postings":
			if err := preflightPostings(decoder, &usage); err != nil {
				return err
			}
		case "document_length":
			if err := preflightDocumentLengths(decoder, &usage); err != nil {
				return err
			}
		case "average_length":
			var value float64
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("decode search average length: %w", err)
			}
		case "document_count":
			var value int
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("decode search document count: %w", err)
			}
		default:
			return fmt.Errorf("search index contains unknown top-level field %q", name)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("search index object is not closed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("search index contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing search index data: %w", err)
	}
	return nil
}

func preflightJSONStringSizes(reader io.Reader) error {
	buffer := make([]byte, 32*1024)
	inString := false
	escaped := false
	var stringBytes int64
	for {
		read, err := reader.Read(buffer)
		for _, value := range buffer[:read] {
			if !inString {
				if value == '"' {
					inString = true
					stringBytes = 0
				}
				continue
			}
			if escaped {
				escaped = false
				stringBytes++
			} else {
				switch value {
				case '\\':
					escaped = true
					stringBytes++
				case '"':
					inString = false
				default:
					stringBytes++
				}
			}
			if stringBytes > maximumRawJSONStringBytes {
				return fmt.Errorf("search index JSON string exceeds the %d-byte raw limit", maximumRawJSONStringBytes)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("scan search index JSON strings: %w", err)
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
	if inString || escaped {
		return errors.New("search index contains an unterminated JSON string")
	}
	return nil
}

func preflightDocuments(decoder *json.Decoder, usage *persistedIndexPreflight) error {
	if err := expectDelimiter(decoder, '{', "documents"); err != nil {
		return err
	}
	previous := ""
	for decoder.More() {
		id, err := nextObjectName(decoder)
		if err != nil {
			return err
		}
		if len(id) > maximumSearchIdentifierBytes || (previous != "" && previous >= id) {
			return errors.New("search document keys are oversized, duplicated, or not canonically sorted")
		}
		previous = id
		usage.documents++
		if usage.documents > MaximumIndexDocuments {
			return fmt.Errorf("search document count exceeds the %d-document limit", MaximumIndexDocuments)
		}
		bytes, metadataFields, err := preflightDocument(decoder)
		if err != nil {
			return fmt.Errorf("preflight search document %q: %w", id, err)
		}
		if bytes > MaximumIndexedDocumentBytes {
			return fmt.Errorf("search document %q exceeds the %d-byte indexed-text limit", id, MaximumIndexedDocumentBytes)
		}
		if bytes > MaximumIndexedTextBytes-usage.indexedTextBytes {
			return fmt.Errorf("search corpus indexed text exceeds the %d-byte limit", MaximumIndexedTextBytes)
		}
		usage.indexedTextBytes += bytes
		if metadataFields > MaximumDocumentMetadataEntries-usage.metadataFields {
			return fmt.Errorf("search document metadata exceeds the %d-entry corpus limit", MaximumDocumentMetadataEntries)
		}
		usage.metadataFields += metadataFields
	}
	return closeDelimiter(decoder, '}', "documents")
}

func preflightDocument(decoder *json.Decoder) (int64, int, error) {
	if err := expectDelimiter(decoder, '{', "document"); err != nil {
		return 0, 0, err
	}
	seen := map[string]struct{}{}
	var total int64
	metadataFields := 0
	for decoder.More() {
		name, err := nextObjectName(decoder)
		if err != nil {
			return 0, 0, err
		}
		if _, duplicate := seen[name]; duplicate {
			return 0, 0, fmt.Errorf("document repeats field %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "id", "object_type", "kind", "language", "title", "qualified_name", "signature", "path", "body":
			var value string
			if err := decoder.Decode(&value); err != nil {
				return 0, 0, err
			}
			if name != "body" && len(value) > maximumSearchIdentifierBytes {
				return 0, 0, fmt.Errorf("document field %q exceeds %d bytes", name, maximumSearchIdentifierBytes)
			}
			if int64(len(value)) > MaximumIndexedTextBytes-total {
				return 0, 0, errors.New("document text exceeds the search resource envelope")
			}
			total += int64(len(value))
		case "metadata":
			bytes, fields, err := preflightMetadata(decoder)
			if err != nil {
				return 0, 0, err
			}
			metadataFields = fields
			if bytes > MaximumIndexedTextBytes-total {
				return 0, 0, errors.New("document metadata exceeds the search resource envelope")
			}
			total += bytes
		default:
			return 0, 0, fmt.Errorf("document contains unknown field %q", name)
		}
	}
	return total, metadataFields, closeDelimiter(decoder, '}', "document")
}

func preflightMetadata(decoder *json.Decoder) (int64, int, error) {
	if err := expectDelimiter(decoder, '{', "metadata"); err != nil {
		return 0, 0, err
	}
	previous := ""
	fields := 0
	var total int64
	for decoder.More() {
		key, err := nextObjectName(decoder)
		if err != nil {
			return 0, 0, err
		}
		if previous != "" && previous >= key {
			return 0, 0, errors.New("search metadata keys are duplicated or not canonically sorted")
		}
		previous = key
		fields++
		if fields > maximumDocumentMetadataFields || len(key) > maximumSearchIdentifierBytes {
			return 0, 0, errors.New("search document metadata exceeds its field or key limit")
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return 0, 0, err
		}
		if len(value) > maximumSearchIdentifierBytes {
			return 0, 0, errors.New("search document metadata value exceeds its byte limit")
		}
		total += int64(len(key) + len(value))
	}
	return total, fields, closeDelimiter(decoder, '}', "metadata")
}

func preflightPostings(decoder *json.Decoder, usage *persistedIndexPreflight) error {
	if err := expectDelimiter(decoder, '{', "postings"); err != nil {
		return err
	}
	previous := ""
	for decoder.More() {
		term, err := nextObjectName(decoder)
		if err != nil {
			return err
		}
		if len(term) > MaximumIndexedTermBytes || (previous != "" && previous >= term) {
			return errors.New("search terms are oversized, duplicated, or not canonically sorted")
		}
		previous = term
		usage.terms++
		if usage.terms > MaximumDistinctTerms || int64(len(term)) > MaximumTermDictionaryBytes-usage.termDictionaryByte {
			return errors.New("search term dictionary exceeds its resource envelope")
		}
		usage.termDictionaryByte += int64(len(term))
		if err := expectDelimiter(decoder, '[', "posting list"); err != nil {
			return err
		}
		for decoder.More() {
			usage.postings++
			if usage.postings > MaximumPostings {
				return fmt.Errorf("search posting count exceeds the %d-posting limit", MaximumPostings)
			}
			if err := preflightPosting(decoder, usage); err != nil {
				return err
			}
		}
		if err := closeDelimiter(decoder, ']', "posting list"); err != nil {
			return err
		}
	}
	return closeDelimiter(decoder, '}', "postings")
}

func preflightPosting(decoder *json.Decoder, usage *persistedIndexPreflight) error {
	if err := expectDelimiter(decoder, '{', "posting"); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		name, err := nextObjectName(decoder)
		if err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("posting repeats field %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "document_id":
			if err := preflightBoundedString(decoder, maximumSearchIdentifierBytes, name); err != nil {
				return err
			}
		case "term_count":
			var value int
			if err := decoder.Decode(&value); err != nil || value < 0 {
				return errors.New("posting term_count is invalid")
			}
		case "field_boost":
			var value float64
			if err := decoder.Decode(&value); err != nil {
				return errors.New("posting field_boost is invalid")
			}
		case "fields":
			if err := expectDelimiter(decoder, '[', "posting fields"); err != nil {
				return err
			}
			count := 0
			for decoder.More() {
				count++
				if count > maximumPostingFields {
					return errors.New("posting field list exceeds its limit")
				}
				if err := preflightBoundedString(decoder, maximumSearchIdentifierBytes, "posting field"); err != nil {
					return err
				}
			}
			if count > MaximumPostingFieldValues-usage.postingFields {
				return fmt.Errorf("search posting fields exceed the %d-value corpus limit", MaximumPostingFieldValues)
			}
			usage.postingFields += count
			if err := closeDelimiter(decoder, ']', "posting fields"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("posting contains unknown field %q", name)
		}
	}
	return closeDelimiter(decoder, '}', "posting")
}

func preflightDocumentLengths(decoder *json.Decoder, usage *persistedIndexPreflight) error {
	if err := expectDelimiter(decoder, '{', "document lengths"); err != nil {
		return err
	}
	previous := ""
	count := 0
	for decoder.More() {
		id, err := nextObjectName(decoder)
		if err != nil {
			return err
		}
		if len(id) > maximumSearchIdentifierBytes || (previous != "" && previous >= id) {
			return errors.New("search document-length keys are oversized, duplicated, or not canonically sorted")
		}
		previous = id
		count++
		if count > MaximumIndexDocuments {
			return errors.New("search document-length count exceeds its limit")
		}
		var value int
		if err := decoder.Decode(&value); err != nil || value < 0 || value > MaximumTokenOccurrences-usage.tokenOccurrences {
			return fmt.Errorf("search token occurrences exceed the %d-token limit", MaximumTokenOccurrences)
		}
		usage.tokenOccurrences += value
	}
	return closeDelimiter(decoder, '}', "document lengths")
}

func nextObjectName(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", err
	}
	name, ok := token.(string)
	if !ok {
		return "", errors.New("JSON object key is not a string")
	}
	return name, nil
}

func preflightBoundedString(decoder *json.Decoder, maximum int, field string) error {
	var value string
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if len(value) > maximum {
		return fmt.Errorf("search %s exceeds the %d-byte limit", field, maximum)
	}
	return nil
}

func expectDelimiter(decoder *json.Decoder, delimiter byte, label string) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim(delimiter) {
		return fmt.Errorf("search %s must begin with %q", label, delimiter)
	}
	return nil
}

func closeDelimiter(decoder *json.Decoder, delimiter byte, label string) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim(delimiter) {
		return fmt.Errorf("search %s must end with %q", label, delimiter)
	}
	return nil
}
