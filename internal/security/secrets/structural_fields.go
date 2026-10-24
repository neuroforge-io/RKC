package secrets

import (
	"reflect"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// StructuralRedactionVersion identifies the canonical structural/payload boundary.
// Snapshot identity recipes should bind this version when its behavior changes.
const StructuralRedactionVersion = "canonical-structural-redaction/v1"

var (
	snapshotStruct   = reflect.TypeOf(rkcmodel.Snapshot{})
	artifactStruct   = reflect.TypeOf(rkcmodel.Artifact{})
	nodeStruct       = reflect.TypeOf(rkcmodel.Node{})
	edgeStruct       = reflect.TypeOf(rkcmodel.Edge{})
	evidenceStruct   = reflect.TypeOf(rkcmodel.Evidence{})
	diagnosticStruct = reflect.TypeOf(rkcmodel.Diagnostic{})
	claimStruct      = reflect.TypeOf(rkcmodel.Claim{})
	documentStruct   = reflect.TypeOf(rkcmodel.Document{})
	gitInfoStruct    = reflect.TypeOf(rkcmodel.GitInfo{})
)

// preserveStructuralString recognizes only exact canonical record types and
// fields with valid structural values. This is not a payload exemption by name
// or appearance: arbitrary structs, maps, IDs, and malformed values still pass
// through ordinary redaction. Callers disable it inside open payload maps.
func preserveStructuralString(owner reflect.Type, name, value string) bool {
	switch owner {
	case snapshotStruct:
		switch name {
		case "ContentDigest", "ConfigDigest", "PolicyDigest", "PluginLockDigest", "ToolchainDigest":
			return structuralSHA256(value)
		case "Status":
			return rkcmodel.IsKnownSnapshotStatus(value)
		}
	case artifactStruct:
		switch name {
		case "SHA256":
			return structuralSHA256(value)
		case "ContentID":
			// ContentID has its own published algorithm-prefixed recipe. No
			// arbitrary identity or other algorithm address is exempted.
			return len(value) == 71 && strings.HasPrefix(value, "sha256:") &&
				structuralSHA256(value[7:]) && value == strings.ToLower(value)
		case "Kind":
			return rkcmodel.IsKnownArtifactKind(value)
		case "Status":
			return rkcmodel.IsKnownArtifactStatus(value)
		}
	case nodeStruct:
		switch name {
		case "SemanticHash":
			return structuralSHA256(value)
		case "Kind":
			return rkcmodel.IsKnownNodeKind(value)
		}
	case edgeStruct:
		switch name {
		case "Kind":
			return rkcmodel.IsKnownEdgeKind(value)
		case "Resolution":
			return rkcmodel.IsKnownResolution(value) && value == rkcmodel.NormalizeResolution(value)
		}
	case evidenceStruct:
		switch name {
		case "InputDigest":
			return structuralSHA256(value)
		case "Kind":
			return rkcmodel.IsKnownEvidenceKind(value)
		}
	case diagnosticStruct:
		return name == "Severity" && rkcmodel.IsKnownSeverity(value)
	case claimStruct:
		switch name {
		case "Certainty":
			return rkcmodel.IsKnownClaimCertainty(value)
		case "Validation":
			return rkcmodel.IsKnownClaimValidation(value)
		}
	case documentStruct:
		switch name {
		case "ContentSHA256":
			return structuralSHA256(value)
		case "Status":
			return rkcmodel.IsKnownDocumentStatus(value)
		}
	case gitInfoStruct:
		return name == "WorkingTreeDigest" && structuralSHA256(value)
	}
	return false
}

// Match the canonical SHA-256 validator without allocating a decoded byte slice.
// Uppercase hex remains valid for typed digests, as it is at model validation.
func structuralSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		current := value[index]
		if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f' ||
			current >= 'A' && current <= 'F') {
			return false
		}
	}
	return true
}
