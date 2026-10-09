package secrets

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const redactionToken = "[REDACTED]"

// Short credential literals such as "one" and "two" can also occur inside
// unrelated paths, identifiers, and prose. Preserve the former assignment
// detector's eight-byte floor for cross-field substring propagation; short
// values remain redacted in their source spans and named credential fields.
const minimumPropagatedLiteralBytes = 8

// SensitiveLiterals returns the exact source substrings identified by Scan.
// Callers must keep the result in memory only; it exists solely to remove the
// same values from parser/plugin records before they enter the canonical graph.
func SensitiveLiterals(data []byte, findings []Finding) []string {
	seen := map[string]struct{}{}
	for _, finding := range findings {
		start, end := finding.StartByte, finding.EndByte
		if start < 0 || end > len(data) || start >= end {
			continue
		}
		value := string(data[start:end])
		if strings.TrimSpace(value) != "" {
			seen[value] = struct{}{}
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) == len(values[j]) {
			return values[i] < values[j]
		}
		return len(values[i]) > len(values[j])
	})
	return values
}

// SanitizeBundle masks detected source patterns and credential-like map fields.
// Transient literals of at least eight bytes also propagate across canonical
// strings. Short source literals never rewrite unrelated identities or prose.
// This runs before validation, search indexing, or any export.
func SanitizeBundle(bundle *rkcmodel.Bundle, literals []string) int {
	if bundle == nil {
		return 0
	}
	clean := make([]string, 0, len(literals))
	seen := map[string]struct{}{}
	for _, literal := range literals {
		if len(literal) < minimumPropagatedLiteralBytes {
			continue
		}
		if _, ok := seen[literal]; ok {
			continue
		}
		seen[literal] = struct{}{}
		clean = append(clean, literal)
	}
	sort.Slice(clean, func(i, j int) bool {
		if len(clean[i]) == len(clean[j]) {
			return clean[i] < clean[j]
		}
		return len(clean[i]) > len(clean[j])
	})
	redactions := 0
	redactReflect(reflect.ValueOf(bundle), clean, &redactions)
	return redactions
}

func redactReflect(value reflect.Value, literals []string, redactions *int) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			redactReflect(value.Elem(), literals, redactions)
		}
	case reflect.Interface:
		if value.IsNil() {
			return
		}
		copyValue := reflect.New(value.Elem().Type()).Elem()
		copyValue.Set(value.Elem())
		redactReflect(copyValue, literals, redactions)
		if value.CanSet() {
			value.Set(copyValue)
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if field.CanSet() {
				redactReflect(field, literals, redactions)
			}
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for index := 0; index < value.Len(); index++ {
			redactReflect(value.Index(index), literals, redactions)
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			redactReflect(value.Index(index), literals, redactions)
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		entries := sanitizedMapEntries(value, literals)
		rebuilt := reflect.MakeMapWithSize(value.Type(), len(entries))
		for _, entry := range entries {
			*redactions += entry.keyRedactions
			current := value.MapIndex(entry.originalKey)
			copyValue := reflect.New(current.Type()).Elem()
			copyValue.Set(current)
			if name, ok := mapKeyString(entry.originalKey); ok && !entry.keyChanged && isCredentialValueKey(name) {
				redactCredentialMapString(copyValue, redactions)
			}
			redactReflect(copyValue, literals, redactions)
			key := uniqueMapKey(rebuilt, entry.sanitizedKey)
			rebuilt.SetMapIndex(key, copyValue)
		}
		if value.CanSet() {
			value.Set(rebuilt)
		} else {
			// A directly supplied map value is mutable even though the reflect
			// value itself is not settable. Replace its entries only after the
			// complete sanitized map has been assembled.
			for _, key := range value.MapKeys() {
				value.SetMapIndex(key, reflect.Value{})
			}
			for _, key := range rebuilt.MapKeys() {
				value.SetMapIndex(key, rebuilt.MapIndex(key))
			}
		}
	case reflect.String:
		if !value.CanSet() {
			return
		}
		text, count := sanitizeText(value.String(), literals)
		*redactions += count
		value.SetString(text)
	}
}

func isCredentialValueKey(name string) bool {
	compact := strings.ToLower(strings.NewReplacer("-", "", ".", "", "_", "").Replace(name))
	switch compact {
	case "secret", "credential", "credentials", "privatekey", "secretkey", "clientsecret":
		return true
	}
	// Value-bearing suffixes admit provider-specific fields such as
	// openai_api_key and userPassword while metadata/reference suffixes such
	// as APIKeyEnv, token_count, secret_kind, and tokenizer stay untouched.
	for _, suffix := range []string{"password", "passwd", "token", "apikey", "privatekey", "secretkey", "clientsecret"} {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

// Bare plugin attributes no longer have their original source syntax. Their
// credential-like key supplies the missing context, without turning that value
// into a substring replacement rule for every other field in the repository.
func redactCredentialMapString(value reflect.Value, redactions *int) {
	if value.Kind() == reflect.Interface && !value.IsNil() {
		copyValue := reflect.New(value.Elem().Type()).Elem()
		copyValue.Set(value.Elem())
		redactCredentialMapString(copyValue, redactions)
		if value.CanSet() {
			value.Set(copyValue)
		}
		return
	}
	if value.Kind() != reflect.String || !value.CanSet() {
		return
	}
	text := value.String()
	if text == "" || text == redactionToken || isPlaceholder(text) || looksLikeReference(text) {
		return
	}
	value.SetString(redactionToken)
	*redactions++
}

type sanitizedMapEntry struct {
	originalKey   reflect.Value
	sanitizedKey  reflect.Value
	keyChanged    bool
	keyRedactions int
}

func sanitizedMapEntries(value reflect.Value, literals []string) []sanitizedMapEntry {
	keys := value.MapKeys()
	entries := make([]sanitizedMapEntry, 0, len(keys))
	for _, key := range keys {
		entry := sanitizedMapEntry{originalKey: key, sanitizedKey: key}
		if original, ok := mapKeyString(key); ok {
			text, count := sanitizeText(original, literals)
			entry.keyRedactions = count
			entry.keyChanged = text != original
			if entry.keyChanged {
				entry.sanitizedKey = mapKeyWithString(key, text)
			}
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		// Preserve legitimate, already-safe keys when a redacted key would
		// otherwise collide with one. Changed keys are then assigned stable
		// suffixes in lexical source-key order.
		if entries[i].keyChanged != entries[j].keyChanged {
			return !entries[i].keyChanged
		}
		left, leftString := mapKeyString(entries[i].originalKey)
		right, rightString := mapKeyString(entries[j].originalKey)
		if leftString != rightString {
			return leftString
		}
		if left != right {
			return left < right
		}
		return entries[i].originalKey.Type().String() < entries[j].originalKey.Type().String()
	})
	return entries
}

func uniqueMapKey(target reflect.Value, candidate reflect.Value) reflect.Value {
	base, stringKey := mapKeyString(candidate)
	if !target.MapIndex(candidate).IsValid() || !stringKey {
		return candidate
	}
	for ordinal := 2; ; ordinal++ {
		key := mapKeyWithString(candidate, base+"#"+strconv.Itoa(ordinal))
		if !target.MapIndex(key).IsValid() {
			return key
		}
	}
}

func mapKeyString(key reflect.Value) (string, bool) {
	current := key
	for current.IsValid() && current.Kind() == reflect.Interface {
		if current.IsNil() {
			return "", false
		}
		current = current.Elem()
	}
	if !current.IsValid() || current.Kind() != reflect.String {
		return "", false
	}
	return current.String(), true
}

func mapKeyWithString(key reflect.Value, text string) reflect.Value {
	if key.Kind() == reflect.String {
		updated := reflect.New(key.Type()).Elem()
		updated.SetString(text)
		return updated
	}
	concrete := key.Elem()
	updatedConcrete := reflect.New(concrete.Type()).Elem()
	updatedConcrete.SetString(text)
	updated := reflect.New(key.Type()).Elem()
	updated.Set(updatedConcrete)
	return updated
}

func sanitizeText(text string, literals []string) (string, int) {
	redactions := 0
	if findings := Scan([]byte(text)); len(findings) > 0 {
		text = string(Redact([]byte(text), findings))
		redactions += len(findings)
	}
	for _, literal := range literals {
		count := strings.Count(text, literal)
		if count == 0 {
			continue
		}
		text = strings.ReplaceAll(text, literal, redactionToken)
		redactions += count
	}
	return text, redactions
}
