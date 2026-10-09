package secrets

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// This bounded source check complements the runtime redaction positive
// controls. It prevents their checked-in spelling from tripping the strict
// self-catalogue gate; it does not certify these files contain no private data.
func TestPositiveControlSourcesHaveNoHighConfidenceCredentialPairs(t *testing.T) {
	const maximumFileBytes = 256 * 1024
	const maximumTotalBytes = 2 * 1024 * 1024
	paths := []string{
		"internal/security/secrets/json_fields_test.go",
		"internal/security/secrets/short_literals_test.go",
		"internal/security/secrets/source_fixtures_test.go",
		"internal/pipeline/short_secrets_test.go",
		"internal/pipeline/source_identity_test.go",
		"internal/pipeline/source_documents_test.go",
		"internal/groundedanswer/artifact_projection_test.go",
		"internal/search/index.go",
		"internal/server/workbench_github_test.go",
		"internal/server/workbench_github_supersession_test.go",
		"internal/modelruntime/provider_profile_test.go",
		"internal/modelruntime/api_discovery_test.go",
		"internal/modelruntime/openai_compatible_test.go",
		"internal/mcpserver/http_test.go",
		"internal/mcpserver/metadata_test.go",
		"scripts/test_smoke_gui.py",
		"fixtures/messy-data/messages.ndjson",
		"docs/DATA_INGESTION.md",
	}
	repository := filepath.Join("..", "..", "..")
	totalBytes := 0
	for _, path := range paths {
		file, err := os.Open(filepath.Join(repository, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("open declared source fixture %s: %v", path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maximumFileBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read declared source fixture %s: read=%v close=%v", path, readErr, closeErr)
		}
		if len(data) > maximumFileBytes {
			t.Fatalf("declared source fixture %s exceeds the %d-byte limit", path, maximumFileBytes)
		}
		totalBytes += len(data)
		if totalBytes > maximumTotalBytes {
			t.Fatalf("declared source fixtures exceed the %d-byte aggregate limit", maximumTotalBytes)
		}
		findings, err := ScanContext(context.Background(), data)
		if err != nil {
			t.Fatalf("scan declared source fixture %s: %v", path, err)
		}
		for _, finding := range findings {
			if finding.Confidence >= .90 {
				t.Errorf("%s:%d:%d contains a high-confidence %s pair (key=%q)",
					path, finding.StartLine, finding.StartColumn, finding.Kind, finding.KeyName)
			}
		}
	}
}
