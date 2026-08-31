package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestPreflightPersistedIndexAcceptsBoundedCanonicalIndex(t *testing.T) {
	t.Parallel()
	index, err := BuildBounded([]Document{{
		ID: "artifact", ObjectType: "artifact", Title: "Guide", Path: "docs/guide.md",
		Body: "grounded repository evidence", Metadata: map[string]string{"digest": strings.Repeat("a", 64)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightPersistedIndex(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid index failed preflight: %v", err)
	}
}

func TestBoundedIndexAndPreflightRejectAllocationAmplifiers(t *testing.T) {
	t.Parallel()
	oversized := Document{ID: "large", Body: strings.Repeat("x", MaximumIndexedDocumentBytes+1)}
	if _, err := BuildBounded([]Document{oversized}); err == nil || !strings.Contains(err.Error(), "per-document limit") {
		t.Fatalf("oversized document passed bounded build: %v", err)
	}

	metadata := make(map[string]string, maximumDocumentMetadataFields+1)
	for index := 0; index <= maximumDocumentMetadataFields; index++ {
		metadata[strings.Repeat("k", index+1)] = "value"
	}
	index := Build([]Document{{ID: "metadata", Body: "body", Metadata: metadata}})
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightPersistedIndex(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
		t.Fatalf("metadata allocation amplifier passed preflight: %v", err)
	}
}

func TestTokenizeSkipsPathologicalTerms(t *testing.T) {
	t.Parallel()
	pathological := strings.Repeat("x", MaximumIndexedTermBytes+1)
	index := Build([]Document{{ID: "term", Body: pathological + " grounded"}})
	if _, indexed := index.Postings[pathological]; indexed {
		t.Fatal("pathological term was indexed")
	}
	if len(index.Postings["grounded"]) != 1 {
		t.Fatalf("normal term was lost: %+v", index.Postings)
	}
}

func TestPreflightPersistedIndexRejectsMalformedCanonicalStructure(t *testing.T) {
	t.Parallel()
	oversizedIdentifier := strings.Repeat("x", maximumSearchIdentifierBytes+1)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "non-object", input: `[]`, want: "one JSON object"},
		{name: "duplicate top-level field", input: `{"version":"1","version":"2"}`, want: "repeats top-level field"},
		{name: "oversized top-level identifier", input: `{"version":"` + oversizedIdentifier + `"}`, want: "version exceeds"},
		{name: "documents delimiter", input: `{"documents":[]}`, want: "documents must begin"},
		{name: "unsorted documents", input: `{"documents":{"b":{},"a":{}}}`, want: "not canonically sorted"},
		{name: "duplicate document field", input: `{"documents":{"a":{"id":"a","id":"b"}}}`, want: "document repeats field"},
		{name: "invalid document string", input: `{"documents":{"a":{"id":7}}}`, want: "cannot unmarshal"},
		{name: "oversized document identity", input: `{"documents":{"a":{"id":"` + oversizedIdentifier + `"}}}`, want: "document field"},
		{name: "unknown document field", input: `{"documents":{"a":{"unexpected":"value"}}}`, want: "unknown field"},
		{name: "metadata delimiter", input: `{"documents":{"a":{"metadata":[]}}}`, want: "metadata must begin"},
		{name: "unsorted metadata", input: `{"documents":{"a":{"metadata":{"b":"1","a":"2"}}}}`, want: "metadata keys"},
		{name: "oversized metadata value", input: `{"documents":{"a":{"metadata":{"key":"` + oversizedIdentifier + `"}}}}`, want: "metadata value"},
		{name: "postings delimiter", input: `{"postings":[]}`, want: "postings must begin"},
		{name: "oversized term", input: `{"postings":{"` + strings.Repeat("t", MaximumIndexedTermBytes+1) + `":[]}}`, want: "terms are oversized"},
		{name: "unsorted terms", input: `{"postings":{"b":[],"a":[]}}`, want: "not canonically sorted"},
		{name: "posting-list delimiter", input: `{"postings":{"term":{}}}`, want: "posting list must begin"},
		{name: "posting delimiter", input: `{"postings":{"term":[[]]}}`, want: "posting must begin"},
		{name: "duplicate posting field", input: `{"postings":{"term":[{"term_count":1,"term_count":2}]}}`, want: "posting repeats field"},
		{name: "invalid term count", input: `{"postings":{"term":[{"term_count":-1}]}}`, want: "term_count is invalid"},
		{name: "invalid field boost", input: `{"postings":{"term":[{"field_boost":"high"}]}}`, want: "field_boost is invalid"},
		{name: "posting-fields delimiter", input: `{"postings":{"term":[{"fields":{}}]}}`, want: "posting fields must begin"},
		{name: "oversized posting field", input: `{"postings":{"term":[{"fields":["` + oversizedIdentifier + `"]}]}}`, want: "posting field exceeds"},
		{name: "unknown posting field", input: `{"postings":{"term":[{"unexpected":1}]}}`, want: "unknown field"},
		{name: "document-length delimiter", input: `{"document_length":[]}`, want: "document lengths must begin"},
		{name: "unsorted document lengths", input: `{"document_length":{"b":1,"a":1}}`, want: "document-length keys"},
		{name: "negative document length", input: `{"document_length":{"a":-1}}`, want: "token occurrences"},
		{name: "invalid average length", input: `{"average_length":"many"}`, want: "average length"},
		{name: "invalid document count", input: `{"document_count":"many"}`, want: "document count"},
		{name: "unknown top-level field", input: `{"unexpected":true}`, want: "unknown top-level field"},
		{name: "unclosed top-level object", input: `{"version":"1"`, want: "object is not closed"},
		{name: "multiple values", input: `{} {}`, want: "multiple JSON values"},
		{name: "invalid trailing data", input: `{} x`, want: "trailing search index data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := PreflightPersistedIndex(bytes.NewReader([]byte(test.input)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PreflightPersistedIndex(%s) = %v, want error containing %q", test.name, err, test.want)
			}
		})
	}
}

func TestPreflightPersistedIndexEnforcesNestedResourceAccounting(t *testing.T) {
	t.Parallel()
	metadataFields := make([]string, 0, maximumDocumentMetadataFields+1)
	for index := 0; index <= maximumDocumentMetadataFields; index++ {
		metadataFields = append(metadataFields, fmt.Sprintf("%q:%q", fmt.Sprintf("field-%03d", index), "value"))
	}
	postingFields := make([]string, maximumPostingFields+1)
	for index := range postingFields {
		postingFields[index] = fmt.Sprintf("%q", fmt.Sprintf("field-%02d", index))
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "per-document text bytes",
			input: `{"documents":{"large":{"body":"` + strings.Repeat("x", MaximumIndexedDocumentBytes+1) + `"}}}`,
			want:  "indexed-text limit",
		},
		{
			name:  "metadata field count",
			input: `{"documents":{"metadata":{"metadata":{` + strings.Join(metadataFields, ",") + `}}}}`,
			want:  "field or key limit",
		},
		{
			name:  "posting field count",
			input: `{"postings":{"term":[{"fields":[` + strings.Join(postingFields, ",") + `]}]}}`,
			want:  "posting field list exceeds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := PreflightPersistedIndex(bytes.NewReader([]byte(test.input)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PreflightPersistedIndex(%s) = %v, want error containing %q", test.name, err, test.want)
			}
		})
	}

	assertDecoderError := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight helper error = %v, want error containing %q", err, want)
		}
	}
	t.Run("document count", func(t *testing.T) {
		usage := &persistedIndexPreflight{documents: MaximumIndexDocuments}
		assertDecoderError(t, preflightDocuments(json.NewDecoder(strings.NewReader(`{"a":{}}`)), usage), "document count exceeds")
	})
	t.Run("corpus text bytes", func(t *testing.T) {
		usage := &persistedIndexPreflight{indexedTextBytes: MaximumIndexedTextBytes}
		assertDecoderError(t, preflightDocuments(json.NewDecoder(strings.NewReader(`{"a":{"id":"a"}}`)), usage), "corpus indexed text exceeds")
	})
	t.Run("corpus metadata entries", func(t *testing.T) {
		usage := &persistedIndexPreflight{metadataFields: MaximumDocumentMetadataEntries}
		assertDecoderError(t, preflightDocuments(json.NewDecoder(strings.NewReader(`{"a":{"metadata":{"key":"value"}}}`)), usage), "metadata exceeds")
	})
	t.Run("term count", func(t *testing.T) {
		usage := &persistedIndexPreflight{terms: MaximumDistinctTerms}
		assertDecoderError(t, preflightPostings(json.NewDecoder(strings.NewReader(`{"term":[]}`)), usage), "term dictionary exceeds")
	})
	t.Run("term dictionary bytes", func(t *testing.T) {
		usage := &persistedIndexPreflight{termDictionaryByte: MaximumTermDictionaryBytes}
		assertDecoderError(t, preflightPostings(json.NewDecoder(strings.NewReader(`{"term":[]}`)), usage), "term dictionary exceeds")
	})
	t.Run("posting count", func(t *testing.T) {
		usage := &persistedIndexPreflight{postings: MaximumPostings}
		assertDecoderError(t, preflightPostings(json.NewDecoder(strings.NewReader(`{"term":[{}]}`)), usage), "posting count exceeds")
	})
	t.Run("posting field corpus", func(t *testing.T) {
		usage := &persistedIndexPreflight{postingFields: MaximumPostingFieldValues}
		assertDecoderError(t, preflightPosting(json.NewDecoder(strings.NewReader(`{"fields":["title"]}`)), usage), "posting fields exceed")
	})
	t.Run("token occurrence corpus", func(t *testing.T) {
		usage := &persistedIndexPreflight{tokenOccurrences: MaximumTokenOccurrences}
		assertDecoderError(t, preflightDocumentLengths(json.NewDecoder(strings.NewReader(`{"a":1}`)), usage), "token occurrences")
	})
}

func TestPreflightJSONStringAndReaderFailures(t *testing.T) {
	t.Parallel()
	if err := PreflightPersistedIndex(nil); err == nil || !strings.Contains(err.Error(), "reader is nil") {
		t.Fatalf("nil preflight reader = %v", err)
	}
	if err := preflightJSONStringSizes(&fixedReadError{err: errors.New("read failed")}); err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("read failure = %v", err)
	}
	if err := preflightJSONStringSizes(noProgressReader{}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("no-progress read = %v", err)
	}
	for _, input := range []string{`"unterminated`, `"escaped\`} {
		if err := preflightJSONStringSizes(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "unterminated") {
			t.Fatalf("unterminated JSON string %q = %v", input, err)
		}
	}
	oversized := &oversizedJSONStringReader{remaining: maximumRawJSONStringBytes + 2}
	if err := preflightJSONStringSizes(oversized); err == nil || !strings.Contains(err.Error(), "raw limit") {
		t.Fatalf("oversized raw JSON string = %v", err)
	}
	seekFailure := &seekFailureReader{Reader: *bytes.NewReader([]byte(`{}`))}
	if err := PreflightPersistedIndex(seekFailure); err == nil || !strings.Contains(err.Error(), "rewind search index") {
		t.Fatalf("seek failure = %v", err)
	}
}

func TestPreflightDecoderPrimitiveFailures(t *testing.T) {
	t.Parallel()
	if _, err := nextObjectName(json.NewDecoder(strings.NewReader(`1`))); err == nil || !strings.Contains(err.Error(), "key is not a string") {
		t.Fatalf("non-string object key = %v", err)
	}
	if _, err := nextObjectName(json.NewDecoder(strings.NewReader(``))); err == nil {
		t.Fatal("missing object key was accepted")
	}
	if err := preflightBoundedString(json.NewDecoder(strings.NewReader(`7`)), 1, "field"); err == nil {
		t.Fatal("non-string bounded field was accepted")
	}
	if err := preflightBoundedString(json.NewDecoder(strings.NewReader(`"xx"`)), 1, "field"); err == nil || !strings.Contains(err.Error(), "field exceeds") {
		t.Fatalf("oversized bounded field = %v", err)
	}
	if err := expectDelimiter(json.NewDecoder(strings.NewReader(`[]`)), '{', "object"); err == nil || !strings.Contains(err.Error(), "must begin") {
		t.Fatalf("wrong opening delimiter = %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(`[]`))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	if err := closeDelimiter(decoder, '}', "object"); err == nil || !strings.Contains(err.Error(), "must end") {
		t.Fatalf("wrong closing delimiter = %v", err)
	}
}

type fixedReadError struct {
	err error
}

func (reader *fixedReadError) Read([]byte) (int, error) {
	return 0, reader.err
}

type noProgressReader struct{}

func (noProgressReader) Read([]byte) (int, error) {
	return 0, nil
}

type oversizedJSONStringReader struct {
	remaining int64
	started   bool
}

func (reader *oversizedJSONStringReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if int64(count) > reader.remaining {
		count = int(reader.remaining)
	}
	for index := 0; index < count; index++ {
		buffer[index] = 'x'
	}
	if !reader.started {
		buffer[0] = '"'
		reader.started = true
	}
	reader.remaining -= int64(count)
	return count, nil
}

type seekFailureReader struct {
	bytes.Reader
}

func (reader *seekFailureReader) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek failed")
}
