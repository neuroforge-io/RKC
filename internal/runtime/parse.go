package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
)

// parseCoverageFile parses a Go statement-coverage profile (mode: set,
// atomic, or count). Only repository-relative paths are retained; the
// caller's module path prefix is stripped when it matches the trace root.
func parseCoverageFile(path string) ([]TraceArtifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	lineNumber := 0
	seen := map[string]*TraceArtifact{}
	var order []string
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if lineNumber == 1 && (strings.HasPrefix(line, "mode: set") || strings.HasPrefix(line, "mode: atomic") || strings.HasPrefix(line, "mode: count")) {
			continue
		}
		artifact, err := parseCoverageLine(line)
		if err != nil {
			return nil, fmt.Errorf("coverage line %d: %w", lineNumber, err)
		}
		if artifact == nil {
			continue
		}
		current, exists := seen[artifact.Path]
		if !exists {
			seen[artifact.Path] = artifact
			order = append(order, artifact.Path)
			continue
		}
		current.Statements += artifact.Statements
		current.ExecutedStatements += artifact.ExecutedStatements
		current.ExecutedRanges = append(current.ExecutedRanges, artifact.ExecutedRanges...)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	result := make([]TraceArtifact, 0, len(order))
	for _, path := range order {
		result = append(result, *seen[path])
	}
	return result, nil
}

// parseCoverageLine parses one profile record:
//
//	github.com/x/y/file.go:12.34,56.78 5 3
func parseCoverageLine(line string) (*TraceArtifact, error) {
	separator := strings.IndexByte(line, ':')
	if separator <= 0 {
		return nil, fmt.Errorf("malformed coverage record %q", line)
	}
	path := line[:separator]
	remainder := line[separator+1:]
	rangeEnd := strings.IndexByte(remainder, ' ')
	if rangeEnd < 0 {
		return nil, fmt.Errorf("malformed coverage range %q", line)
	}
	span := remainder[:rangeEnd]
	rest := strings.Fields(remainder[rangeEnd+1:])
	if len(rest) != 2 {
		return nil, fmt.Errorf("malformed coverage counts %q", line)
	}
	startText, endText, ok := strings.Cut(span, ",")
	if !ok {
		return nil, fmt.Errorf("malformed coverage span %q", line)
	}
	start, err := parseCoveragePosition(startText)
	if err != nil {
		return nil, err
	}
	end, err := parseCoveragePosition(endText)
	if err != nil {
		return nil, err
	}
	statements, err := strconv.Atoi(rest[0])
	if err != nil || statements < 0 {
		return nil, fmt.Errorf("invalid statement count %q", rest[0])
	}
	executed, err := strconv.Atoi(rest[1])
	if err != nil || executed < 0 {
		return nil, fmt.Errorf("invalid executed count %q", rest[1])
	}
	if !strings.HasPrefix(path, "/") {
		// Repository-relative or module-qualified paths are retained as-is.
	} else {
		return nil, nil // Absolute toolchain paths are not repository artifacts.
	}
	artifact := &TraceArtifact{
		Path:       path,
		Statements: statements,
	}
	if executed > 0 {
		artifact.ExecutedStatements = statements
		artifact.ExecutedRanges = append(artifact.ExecutedRanges, ExecutedRange{
			StartLine: start, EndLine: end, Count: executed,
		})
	}
	return artifact, nil
}

// parseCoveragePosition parses "line.column" and returns the line.
func parseCoveragePosition(value string) (int, error) {
	lineText, _, _ := strings.Cut(value, ".")
	line, err := strconv.Atoi(lineText)
	if err != nil || line <= 0 {
		return 0, fmt.Errorf("invalid coverage position %q", value)
	}
	return line, nil
}

// goTestEvent is one JSONL event emitted by `go test -json`.
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package,omitempty"`
	Test    string  `json:"Test,omitempty"`
	Elapsed float64 `json:"Elapsed,omitempty"`
}

// parseGoTestJSON extracts terminal test results from go test -json output.
func parseGoTestJSON(output []byte) ([]TraceTest, error) {
	results := map[string]TraceTest{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // Non-JSON lines (compiler notes) are ignored.
		}
		if event.Test == "" || event.Action == "" {
			continue
		}
		switch event.Action {
		case "pass", "fail", "skip":
			packageName, packageRedacted, err := safePackageIdentity(event.Package)
			if err != nil {
				return nil, err
			}
			testName, subtestsRedacted, err := safeTestIdentity(event.Test)
			if err != nil {
				return nil, err
			}
			if math.IsNaN(event.Elapsed) || math.IsInf(event.Elapsed, 0) || event.Elapsed < 0 || event.Elapsed*1000 > float64(MaximumTestElapsedMS) {
				return nil, fmt.Errorf("go test event has an invalid elapsed duration")
			}
			test := TraceTest{
				Package: packageName, PackageRedacted: packageRedacted,
				Name: testName, SubtestsRedacted: subtestsRedacted, Status: event.Action,
			}
			if event.Elapsed > 0 {
				test.Elapsed = int64(event.Elapsed * 1000)
			}
			identity := test.Package + "\x00" + test.Name
			if current, ok := results[identity]; ok {
				test.Status = mergeTestStatus(current.Status, test.Status)
				if current.Elapsed > test.Elapsed {
					test.Elapsed = current.Elapsed
				}
			}
			results[identity] = test
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tests := make([]TraceTest, 0, len(keys))
	for _, key := range keys {
		tests = append(tests, results[key])
	}
	return tests, nil
}

func safePackageIdentity(value string) (string, bool, error) {
	if value == "" {
		return "", false, nil
	}
	if len(value) > MaximumPackageIdentifierBytes || containsControl(value) || !packageIdentityPattern.MatchString(value) {
		return "", false, errors.New("go test event has an invalid package identity")
	}
	if len(secrets.Scan([]byte(value))) > 0 {
		return redactedPackage, true, nil
	}
	return value, false, nil
}

func safeTestIdentity(value string) (string, bool, error) {
	if len(value) == 0 || len(value) > MaximumTestIdentifierBytes || containsControl(value) {
		return "", false, errors.New("go test event has an invalid test identity")
	}
	root, suffix, hasSubtest := strings.Cut(value, "/")
	if !testIdentityPattern.MatchString(root) {
		return "", false, errors.New("go test event has an invalid test identity")
	}
	if len(secrets.Scan([]byte(root))) > 0 {
		return "", false, errors.New("go test event test identity contains credential-shaped material")
	}
	if !hasSubtest {
		return root, false, nil
	}
	if suffix == "" {
		return "", false, errors.New("go test event has an invalid test identity")
	}
	return root + redactedSubtestSuffix, true, nil
}

func mergeTestStatus(left, right string) string {
	priority := map[string]int{"skip": 1, "pass": 2, "fail": 3}
	if priority[left] >= priority[right] {
		return left
	}
	return right
}
