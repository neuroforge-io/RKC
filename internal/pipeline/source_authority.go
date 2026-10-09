package pipeline

import (
	"os"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

// publishSourceInventory keeps filesystem authority separate from canonical
// display paths, which may have been redacted after inventory. Copying the
// slice, attribute maps and identity map prevents a consumer from mutating the
// compiler's source accounting.
func publishSourceInventory(opts Options, files []pluginapi.FileRef, identities map[string]sourceFileIdentity) {
	if opts.OnSourceInventory == nil {
		return
	}
	refs := append([]pluginapi.FileRef(nil), files...)
	baseline := make(map[string]os.FileInfo, len(refs))
	for index, file := range refs {
		if file.Attributes != nil {
			attributes := make(map[string]string, len(file.Attributes))
			for name, value := range file.Attributes {
				attributes[name] = value
			}
			refs[index].Attributes = attributes
		}
		baseline[file.ArtifactID] = identities[sourceIdentityKey(file)].info
	}
	opts.OnSourceInventory(refs, baseline)
}
