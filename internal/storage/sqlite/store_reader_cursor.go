package sqlite

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const (
	readerCursorVersion = 1
	readerCursorKeyName = "reader_cursor_hmac_v1"
)

type readerCursorPayload struct {
	Version int    `json:"v"`
	Kind    string `json:"k"`
	Scope   string `json:"s"`
	Primary string `json:"p,omitempty"`
	ID      string `json:"i"`
}

func readerCursorKey(
	ctx context.Context,
	connection *sql.Conn,
	operation string,
) ([]byte, error) {
	read := func() (string, error) {
		var encoded string
		err := connection.QueryRowContext(
			ctx,
			"SELECT value FROM schema_meta WHERE key = ?",
			readerCursorKeyName,
		).Scan(&encoded)
		return encoded, err
	}
	encoded, err := read()
	if errors.Is(err, sql.ErrNoRows) {
		candidate := make([]byte, sha256.Size)
		if _, err := rand.Read(candidate); err != nil {
			return nil, readerStoredDataError(
				operation,
				"",
				"cursor",
				"generate cursor authentication key",
				err,
			)
		}
		if _, err := connection.ExecContext(
			ctx,
			`INSERT INTO schema_meta(key, value)
			 VALUES (?, ?)
			 ON CONFLICT(key) DO NOTHING`,
			readerCursorKeyName,
			hex.EncodeToString(candidate),
		); err != nil {
			return nil, readerStorageError(operation, "", "cursor", err)
		}
		encoded, err = read()
	}
	if err != nil {
		return nil, readerStorageError(operation, "", "cursor", err)
	}
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != sha256.Size || encoded != strings.ToLower(encoded) {
		if err == nil {
			err = fmt.Errorf("key has invalid length or case")
		}
		return nil, readerStoredDataError(
			operation,
			"",
			"cursor",
			"persisted cursor authentication key is invalid",
			err,
		)
	}
	return key, nil
}

func readerSealCursor(
	operation string,
	key []byte,
	kind string,
	scope string,
	primary string,
	id string,
) (rkcstore.Cursor, error) {
	payload := readerCursorPayload{
		Version: readerCursorVersion,
		Kind:    kind,
		Scope:   scope,
		Primary: primary,
		ID:      id,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", readerStoredDataError(
			operation,
			"",
			"cursor",
			"encode cursor payload",
			err,
		)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return rkcstore.Cursor(
		base64.RawURLEncoding.EncodeToString(data) + "." +
			base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	), nil
}

func readerOpenCursor(
	operation string,
	key []byte,
	cursor rkcstore.Cursor,
	kind string,
	scope string,
) (readerCursorPayload, error) {
	if cursor == "" {
		return readerCursorPayload{}, nil
	}
	if len(cursor) > readerMaxCursorBytes {
		return readerCursorPayload{}, readerInvalidCursor(operation, "cursor exceeds the safety limit")
	}
	parts := strings.Split(string(cursor), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return readerCursorPayload{}, readerInvalidCursor(operation, "malformed cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return readerCursorPayload{}, readerInvalidCursor(operation, "malformed cursor payload")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return readerCursorPayload{}, readerInvalidCursor(operation, "malformed cursor signature")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	expected := mac.Sum(nil)
	if len(signature) != len(expected) || subtle.ConstantTimeCompare(signature, expected) != 1 {
		return readerCursorPayload{}, readerInvalidCursor(operation, "cursor authentication failed")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var payload readerCursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return readerCursorPayload{}, readerInvalidCursor(operation, "invalid cursor payload")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return readerCursorPayload{}, readerInvalidCursor(operation, "cursor has trailing data")
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !hmac.Equal(canonical, data) {
		return readerCursorPayload{}, readerInvalidCursor(operation, "cursor payload is not canonical")
	}
	if payload.Version != readerCursorVersion || payload.Kind != kind ||
		payload.Scope != scope || payload.ID == "" {
		return readerCursorPayload{}, readerInvalidCursor(operation, "cursor does not belong to this query")
	}
	return payload, nil
}

func readerScopeFingerprint(values ...string) string {
	hash := sha256.New()
	var size [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func readerInvalidCursor(operation string, message string) error {
	return readerOperationError(
		rkcstore.CodeInvalidCursor,
		operation,
		"",
		"cursor",
		errors.New(message),
	)
}
