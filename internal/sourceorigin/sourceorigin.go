// Package sourceorigin canonicalizes repository origins without retaining
// credentials or other URL metadata that can contain secrets.
package sourceorigin

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxOriginBytes = 4096

var (
	errEmpty       = errors.New("repository origin is empty")
	errOversized   = errors.New("repository origin exceeds the size limit")
	errInvalidText = errors.New("repository origin contains invalid text")
	errMalformed   = errors.New("repository origin is malformed")
	errTransport   = errors.New("repository origin uses an unsupported transport")
)

// Normalize returns a stable, credential-free representation of a repository
// origin. HTTPS, SSH, Git, and file transports remain distinct. URL userinfo,
// query strings, and fragments are discarded because each can contain secrets.
// Errors intentionally contain no caller-controlled text.
func Normalize(raw string) (string, error) {
	if raw == "" {
		return "", errEmpty
	}
	if len(raw) > maxOriginBytes {
		return "", errOversized
	}
	if !utf8.ValidString(raw) || containsControl(raw) {
		return "", errInvalidText
	}

	if normalized, recognized, err := normalizeSCP(raw); recognized {
		if err == nil && len(normalized) > maxOriginBytes {
			return "", errOversized
		}
		return normalized, err
	}
	return normalizeURL(raw)
}

// IsCanonical reports whether origin is already a valid Normalize result.
func IsCanonical(origin string) bool {
	normalized, err := Normalize(origin)
	return err == nil && normalized == origin
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func normalizeSCP(raw string) (string, bool, error) {
	if strings.Contains(raw, "://") {
		return "", false, nil
	}
	at := strings.IndexByte(raw, '@')
	if at <= 0 {
		return "", false, nil
	}

	remainder := raw[at+1:]
	if strings.IndexByte(remainder, '@') >= 0 {
		return "", true, errMalformed
	}
	host, path, ok := splitSCPRemainder(remainder)
	if !ok || !validSCPUser(raw[:at]) {
		return "", true, errMalformed
	}

	pathPrefix := "/"
	if strings.HasPrefix(path, "/") {
		pathPrefix = ""
	}
	normalized, err := normalizeURL("ssh://" + host + pathPrefix + path)
	return normalized, true, err
}

func splitSCPRemainder(remainder string) (host, path string, ok bool) {
	if strings.HasPrefix(remainder, "[") {
		closing := strings.IndexByte(remainder, ']')
		if closing <= 1 || len(remainder) <= closing+2 || remainder[closing+1] != ':' {
			return "", "", false
		}
		return remainder[:closing+1], remainder[closing+2:], true
	}
	separator := strings.IndexByte(remainder, ':')
	if separator <= 0 || separator == len(remainder)-1 {
		return "", "", false
	}
	return remainder[:separator], remainder[separator+1:], true
}

func validSCPUser(user string) bool {
	for index := 0; index < len(user); index++ {
		character := user[index]
		if !isUnreserved(character) && !strings.ContainsRune("!$&'()*+,;=", rune(character)) {
			return false
		}
	}
	return user != ""
}

func normalizeURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" {
		return "", errMalformed
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !supportedScheme(scheme) {
		if parsed.Scheme == "" {
			return "", errMalformed
		}
		return "", errTransport
	}
	rawPath, ok := preservedPath(raw)
	if !ok || !validPath(rawPath) {
		return "", errMalformed
	}
	authority, err := canonicalAuthority(parsed, scheme)
	if err != nil {
		return "", err
	}
	return scheme + "://" + authority + rawPath, nil
}

func supportedScheme(scheme string) bool {
	switch scheme {
	case "https", "ssh", "git", "file":
		return true
	default:
		return false
	}
}

func preservedPath(raw string) (string, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || len(raw) < colon+3 || raw[colon+1:colon+3] != "//" {
		return "", false
	}
	authorityStart := colon + 3
	delimiter := strings.IndexAny(raw[authorityStart:], "/?#")
	if delimiter < 0 {
		return "", true
	}
	delimiter += authorityStart
	if raw[delimiter] != '/' {
		return "", true
	}
	end := len(raw)
	if suffix := strings.IndexAny(raw[delimiter:], "?#"); suffix >= 0 {
		end = delimiter + suffix
	}
	return raw[delimiter:end], true
}

func validPath(path string) bool {
	if _, err := url.PathUnescape(path); err != nil {
		return false
	}
	for _, character := range path {
		if unicode.IsSpace(character) || character == '\\' {
			return false
		}
	}
	return true
}

func canonicalAuthority(parsed *url.URL, scheme string) (string, error) {
	hostname := parsed.Hostname()
	if hostname == "" {
		if scheme == "file" && parsed.Host == "" {
			return "", nil
		}
		return "", errMalformed
	}
	if !validHostname(hostname) || explicitEmptyPort(parsed.Host) {
		return "", errMalformed
	}

	port := parsed.Port()
	if scheme == "file" && port != "" {
		return "", errMalformed
	}
	if port != "" {
		value, err := parsePort(port)
		if err != nil {
			return "", errMalformed
		}
		port = strconv.Itoa(value)
		if (scheme == "https" && value == 443) || (scheme == "ssh" && value == 22) {
			port = ""
		}
	}

	hostname = strings.ToLower(hostname)
	if strings.Contains(hostname, ":") {
		hostname = strings.ReplaceAll(hostname, "%", "%25")
		hostname = "[" + hostname + "]"
	}
	if port != "" {
		return hostname + ":" + port, nil
	}
	return hostname, nil
}

func validHostname(hostname string) bool {
	for _, character := range hostname {
		if unicode.IsSpace(character) || unicode.IsControl(character) || character == '/' || character == '\\' || character == '@' {
			return false
		}
	}
	return true
}

func explicitEmptyPort(host string) bool {
	if strings.HasPrefix(host, "[") {
		closing := strings.LastIndexByte(host, ']')
		return closing >= 0 && host[closing+1:] == ":"
	}
	return strings.HasSuffix(host, ":")
}

func parsePort(port string) (int, error) {
	for index := 0; index < len(port); index++ {
		if port[index] < '0' || port[index] > '9' {
			return 0, errMalformed
		}
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return 0, errMalformed
	}
	return value, nil
}

func isUnreserved(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		strings.ContainsRune("-._~", rune(character))
}
