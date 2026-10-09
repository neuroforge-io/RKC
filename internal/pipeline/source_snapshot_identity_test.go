package pipeline

import (
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSnapshotIdentityBindsSourceProducerContract(t *testing.T) {
	t.Parallel()
	options := Options{ConfigDigest: "config", PolicyDigest: "policy", PluginLockDigest: "plugins", ToolchainDigest: "toolchain"}
	// The checkout and externally pinned inputs can be identical across an
	// upgrade. A source-producer migration must still distinguish snapshots
	// whose document and section identities use different recipes.
	originalInputs := []string{"repository", "commit", "inventory", "config", "policy", "plugins", "toolchain", rkcmodel.SchemaVersion}
	legacyID := rkcmodel.StableID("snapshot", originalInputs...)
	recipeInputs := append(append([]string(nil), originalInputs...), "builtin-source-documents/v1", docparse.SourcePluginID)
	oldProducerID := rkcmodel.StableID("snapshot", append(append([]string(nil), recipeInputs...), "0.1.0")...)
	currentProducerID := rkcmodel.StableID("snapshot", append(append([]string(nil), recipeInputs...), docparse.SourcePluginVersion)...)
	for _, admission := range []string{"enabled", "frameworks disabled", "external plugins disabled"} {
		t.Run(admission, func(t *testing.T) {
			current := options
			current.DisableFrameworks = admission == "frameworks disabled"
			current.DisablePlugins = admission == "external plugins disabled"
			id := stableSnapshotID("repository", "commit", "inventory", "", "", "", current)
			if id == legacyID || id == oldProducerID || id != currentProducerID {
				t.Fatalf("source producer contract omitted or conditional: got %s; legacy %s; old producer %s; current producer %s", id, legacyID, oldProducerID, currentProducerID)
			}
		})
	}
}
