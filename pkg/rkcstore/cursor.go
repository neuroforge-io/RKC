package rkcstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

const cursorVersion = 1

type cursorPayload struct {
	Version int    `json:"v"`
	Kind    string `json:"k"`
	Scope   string `json:"s"`
	Primary string `json:"p,omitempty"`
	ID      string `json:"i"`
}

func (store *MemoryStore) sealCursor(kind, scope, primary, id string) (Cursor, error) {
	payload := cursorPayload{Version: cursorVersion, Kind: kind, Scope: scope, Primary: primary, ID: id}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, store.secret[:])
	_, _ = mac.Write(data)
	return Cursor(base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), nil
}

func (store *MemoryStore) openCursor(operation string, cursor Cursor, kind, scope string) (cursorPayload, error) {
	if cursor == "" {
		return cursorPayload{}, nil
	}
	if len(cursor) > maxCursorLen {
		return cursorPayload{}, invalidCursor(operation, "cursor exceeds the safety limit")
	}
	parts := strings.Split(string(cursor), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return cursorPayload{}, invalidCursor(operation, "malformed cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return cursorPayload{}, invalidCursor(operation, "malformed cursor payload")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return cursorPayload{}, invalidCursor(operation, "malformed cursor signature")
	}
	mac := hmac.New(sha256.New, store.secret[:])
	_, _ = mac.Write(data)
	expected := mac.Sum(nil)
	if len(signature) != len(expected) || subtle.ConstantTimeCompare(signature, expected) != 1 {
		return cursorPayload{}, invalidCursor(operation, "cursor authentication failed")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var payload cursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return cursorPayload{}, invalidCursor(operation, "invalid cursor payload")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return cursorPayload{}, invalidCursor(operation, "cursor has trailing data")
	}
	if payload.Version != cursorVersion || payload.Kind != kind || payload.Scope != scope || payload.ID == "" {
		return cursorPayload{}, invalidCursor(operation, "cursor does not belong to this query")
	}
	return payload, nil
}

func scopeFingerprint(values ...string) string {
	hash := sha256.New()
	var size [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
