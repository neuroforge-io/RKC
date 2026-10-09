package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSourceVerificationErrorsWithholdPrivateNamesAndUntrustedIDs(t *testing.T) {
	original := []byte("package original\n")
	tests := []struct {
		name        string
		mutate      func(*testing.T, pluginapi.FileRef, map[string]sourceFileIdentity)
		collectFail bool
		reason      string
	}{
		{"deleted", func(t *testing.T, file pluginapi.FileRef, _ map[string]sourceFileIdentity) {
			if err := os.Remove(file.Materialized); err != nil {
				t.Fatal(err)
			}
		}, true, "inventoried source verification failed"},
		{"modified", func(t *testing.T, file pluginapi.FileRef, _ map[string]sourceFileIdentity) {
			if err := os.WriteFile(file.Materialized, []byte("package modified\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true, "inventoried source verification failed"},
		{"replaced with identical bytes", func(t *testing.T, file pluginapi.FileRef, _ map[string]sourceFileIdentity) {
			// Keep the original inode alive so even aggressive inode reuse cannot
			// make the replacement fixture indistinguishable from its baseline.
			if err := os.Rename(file.Materialized, file.Materialized+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file.Materialized, original, 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, "identity replaced"},
		{"missing baseline", func(_ *testing.T, file pluginapi.FileRef, identities map[string]sourceFileIdentity) {
			delete(identities, sourceIdentityKey(file))
		}, false, "missing baseline identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			privateName := "private-source-secret-8271"
			file := inventoriedPipelineFile(t, root, filepath.ToSlash(filepath.Join("private", privateName+".go")), original)
			file.ArtifactID = "caller-controlled-secret-identity"
			_, identities, err := collectSensitiveLiteralsAndIdentity(root, []pluginapi.FileRef{file})
			if err != nil {
				t.Fatal("could not establish private source fixture baseline")
			}
			test.mutate(t, file, identities)
			assertPrivateFailure := func(err error, prefix, reason string) {
				t.Helper()
				if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), rkcmodel.StableID("artifact", file.Path)) {
					t.Fatalf("source mutation was not rejected with useful opaque identity: %v", err)
				}
				for _, private := range []string{privateName, file.Path, file.Materialized, root, file.ArtifactID} {
					if strings.Contains(err.Error(), private) {
						t.Fatalf("source verification exposed private identity: %v", err)
					}
				}
				var pathError *os.PathError
				if errors.As(err, &pathError) || errors.Unwrap(err) != nil {
					t.Fatal("source verification retained an unwrap-able filesystem error")
				}
			}
			assertPrivateFailure(reverifyInventoriedSources(root, []pluginapi.FileRef{file}, identities), "source changed after adapters:", test.reason)
			if test.collectFail {
				_, _, err := collectSensitiveLiteralsAndIdentity(root, []pluginapi.FileRef{file})
				assertPrivateFailure(err, "read source for canonical redaction:", "source changed after inventory or is unsafe")
			}
		})
	}
}
