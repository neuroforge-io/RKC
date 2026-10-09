package pipeline

import (
	"os"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestPrivateSourcePublicationCopiesInventoryAndBaselineMaps(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := []pluginapi.FileRef{{ArtifactID: "artifact", Path: "original.txt", Attributes: map[string]string{"receipt": "retained"}}}
	key := sourceIdentityKey(files[0])
	identities := map[string]sourceFileIdentity{key: {info: info}}
	calls := 0
	publishSourceInventory(Options{}, files, identities)
	publishSourceInventory(Options{OnSourceInventory: func(refs []pluginapi.FileRef, baseline map[string]os.FileInfo) {
		calls++
		if len(refs) != 1 || baseline["artifact"] != info {
			t.Fatalf("source authority was not bound by artifact ID: %+v", refs)
		}
		refs[0].Path = "mutated.txt"
		refs[0].Attributes["receipt"] = "mutated"
		delete(baseline, "artifact")
	}}, files, identities)
	if calls != 1 || files[0].Path != "original.txt" || files[0].Attributes["receipt"] != "retained" || identities[key].info != info {
		t.Fatal("private inventory publication shared compiler-owned state")
	}
}
