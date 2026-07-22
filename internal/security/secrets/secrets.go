// Package secrets detects and redacts high-confidence credential material
// without retaining the credential value. It is deliberately conservative:
// findings are evidence for review, not proof that a credential is live.
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Finding struct {
	Kind        string  `json:"kind"`
	Confidence  float64 `json:"confidence"`
	StartByte   int     `json:"start_byte"`
	EndByte     int     `json:"end_byte"`
	StartLine   int     `json:"start_line"`
	StartColumn int     `json:"start_column"`
	EndLine     int     `json:"end_line"`
	EndColumn   int     `json:"end_column"`
	Fingerprint string  `json:"fingerprint"`
	KeyName     string  `json:"key_name,omitempty"`
}

type detector struct {
	kind       string
	confidence float64
	pattern    *regexp.Regexp
	group      int
}

var detectors = []detector{
	{kind: "private_key", confidence: 0.99, pattern: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
	{kind: "aws_access_key", confidence: 0.98, pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{kind: "github_token", confidence: 0.98, pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,255}\b`)},
	{kind: "github_fine_grained_token", confidence: 0.98, pattern: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,255}\b`)},
	{kind: "slack_token", confidence: 0.97, pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,255}\b`)},
	{kind: "stripe_live_secret", confidence: 0.98, pattern: regexp.MustCompile(`\bsk_live_[A-Za-z0-9]{16,255}\b`)},
	{kind: "basic_auth_password", confidence: 0.9, pattern: regexp.MustCompile(`(?i)://[^\s/:@]{1,128}:([^\s/@]{8,256})@`), group: 1},
}

var assignmentPattern = regexp.MustCompile(`(?im)\b(password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|private[_-]?key|client[_-]?secret)\b\s*[:=]\s*["']?([^\s"'#,;]{8,512})`)

var placeholderValues = map[string]struct{}{
	"changeme": {}, "change-me": {}, "replace-me": {}, "replace_me": {}, "example": {},
	"example-value": {}, "dummy": {}, "placeholder": {}, "not-a-secret": {}, "your-secret-here": {},
	"development": {}, "password": {}, "secret": {}, "test-only": {}, "localhost": {},
}

func Scan(data []byte) []Finding {
	var findings []Finding
	for _, detector := range detectors {
		matches := detector.pattern.FindAllSubmatchIndex(data, -1)
		for _, match := range matches {
			start, end := match[0], match[1]
			if detector.group > 0 {
				position := detector.group * 2
				if position+1 >= len(match) || match[position] < 0 {
					continue
				}
				start, end = match[position], match[position+1]
			}
			value := data[start:end]
			if isPlaceholder(string(value)) {
				continue
			}
			findings = append(findings, makeFinding(data, start, end, detector.kind, detector.confidence, ""))
		}
	}
	for _, match := range assignmentPattern.FindAllSubmatchIndex(data, -1) {
		if len(match) < 6 || match[4] < 0 {
			continue
		}
		key := string(data[match[2]:match[3]])
		start, end := match[4], match[5]
		value := string(data[start:end])
		if isPlaceholder(value) || looksLikeReference(value) {
			continue
		}
		confidence := 0.86
		if len(value) >= 20 {
			confidence = 0.92
		}
		findings = append(findings, makeFinding(data, start, end, "secret_assignment", confidence, key))
	}
	return mergeFindings(findings)
}

func IsSecretName(name string) bool {
	name = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(name, "-", "_"), ".", "_"))
	for _, marker := range []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "API_KEY", "PRIVATE_KEY", "CREDENTIAL", "CLIENT_SECRET"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func IsPlaceholder(value string) bool { return isPlaceholder(value) }

func Redact(data []byte, findings []Finding) []byte {
	output := append([]byte(nil), data...)
	for _, finding := range mergeFindings(append([]Finding(nil), findings...)) {
		start, end := finding.StartByte, finding.EndByte
		if start < 0 {
			start = 0
		}
		if end > len(output) {
			end = len(output)
		}
		if start >= end {
			continue
		}
		for index := start; index < end; index++ {
			switch output[index] {
			case '\n', '\r', '\t':
				// Preserve exact line and byte layout for provenance.
			default:
				output[index] = '*'
			}
		}
	}
	return output
}

func makeFinding(data []byte, start, end int, kind string, confidence float64, key string) Finding {
	startLine, startColumn := lineColumn(data, start)
	endLine, endColumn := lineColumn(data, end)
	// Fingerprints identify a finding location; they deliberately do not hash
	// the credential value, which would permit offline confirmation of
	// low-entropy passwords from public output.
	digest := sha256.Sum256([]byte(kind + ":" + strconv.Itoa(start) + ":" + strconv.Itoa(end)))
	return Finding{Kind: kind, Confidence: confidence, StartByte: start, EndByte: end, StartLine: startLine, StartColumn: startColumn, EndLine: endLine, EndColumn: endColumn, Fingerprint: hex.EncodeToString(digest[:8]), KeyName: key}
}

func lineColumn(data []byte, offset int) (line, column int) {
	line = 1
	if offset > len(data) {
		offset = len(data)
	}
	for index := 0; index < offset; index++ {
		if data[index] == '\n' {
			line++
			column = 0
		} else {
			column++
		}
	}
	return line, column
}

func mergeFindings(findings []Finding) []Finding {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].StartByte != findings[j].StartByte {
			return findings[i].StartByte < findings[j].StartByte
		}
		if findings[i].EndByte != findings[j].EndByte {
			return findings[i].EndByte > findings[j].EndByte
		}
		return findings[i].Kind < findings[j].Kind
	})
	output := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.EndByte <= finding.StartByte {
			continue
		}
		if len(output) == 0 || finding.StartByte >= output[len(output)-1].EndByte {
			output = append(output, finding)
			continue
		}
		previous := &output[len(output)-1]
		if finding.Confidence > previous.Confidence {
			previous.Kind = finding.Kind
			previous.Confidence = finding.Confidence
			previous.Fingerprint = finding.Fingerprint
			previous.KeyName = finding.KeyName
		}
		if finding.EndByte > previous.EndByte {
			previous.EndByte = finding.EndByte
			previous.EndLine = finding.EndLine
			previous.EndColumn = finding.EndColumn
		}
	}
	return output
}

func isPlaceholder(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, "'\"`"))
	lower := strings.ToLower(value)
	if _, ok := placeholderValues[lower]; ok {
		return true
	}
	if value == "" || strings.HasPrefix(value, "${") || strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "<") || strings.HasPrefix(lower, "example-") || strings.HasPrefix(lower, "dummy-") || strings.Contains(lower, "replace") {
		return true
	}
	allSame := true
	for index := 1; index < len(value); index++ {
		if value[index] != value[0] {
			allSame = false
			break
		}
	}
	return allSame && len(value) >= 8
}

func looksLikeReference(value string) bool {
	return strings.HasPrefix(value, "env:") || strings.HasPrefix(value, "secret://") || strings.HasPrefix(value, "vault:") || strings.HasPrefix(value, "file:")
}
