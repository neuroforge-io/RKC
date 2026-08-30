package rkcmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StableID returns a deterministic, namespace-qualified identifier derived
// from the ordered parts. Parts are separated before hashing, so distinct part
// boundaries cannot collapse to the same input string.
func StableID(namespace string, parts ...string) string {
	key := namespace + "\x00" + strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("rkc:%s:%s", namespace, hex.EncodeToString(sum[:12]))
}

// ContentID returns the full SHA-256 content address for data, including the
// "sha256:" algorithm prefix used by the canonical model.
func ContentID(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DigestJSON returns the lowercase SHA-256 digest of encoding/json's output for
// v. It returns an empty string when v cannot be marshaled; use CanonicalDigest
// for Bundle identity.
func DigestJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SortBundle orders all canonical collections and stable ID lists. Map keys are
// sorted by encoding/json; slices must be sorted explicitly.
func SortBundle(bundle *Bundle) {
	sort.Slice(bundle.Artifacts, func(i, j int) bool {
		if bundle.Artifacts[i].Path == bundle.Artifacts[j].Path {
			return bundle.Artifacts[i].ID < bundle.Artifacts[j].ID
		}
		return bundle.Artifacts[i].Path < bundle.Artifacts[j].Path
	})
	sort.Slice(bundle.Nodes, func(i, j int) bool { return bundle.Nodes[i].ID < bundle.Nodes[j].ID })
	sort.Slice(bundle.Edges, func(i, j int) bool { return bundle.Edges[i].ID < bundle.Edges[j].ID })
	sort.Slice(bundle.Evidence, func(i, j int) bool { return bundle.Evidence[i].ID < bundle.Evidence[j].ID })
	sort.Slice(bundle.Diagnostics, func(i, j int) bool { return bundle.Diagnostics[i].ID < bundle.Diagnostics[j].ID })
	sort.Slice(bundle.Conflicts, func(i, j int) bool { return bundle.Conflicts[i].ID < bundle.Conflicts[j].ID })
	sort.Slice(bundle.Documents, func(i, j int) bool { return bundle.Documents[i].ID < bundle.Documents[j].ID })
	sort.Slice(bundle.Claims, func(i, j int) bool { return bundle.Claims[i].ID < bundle.Claims[j].ID })
	sort.Slice(bundle.Paths, func(i, j int) bool { return bundle.Paths[i].ID < bundle.Paths[j].ID })

	for i := range bundle.Nodes {
		sort.Strings(bundle.Nodes[i].EvidenceIDs)
	}
	for i := range bundle.Edges {
		bundle.Edges[i].Resolution = NormalizeResolution(bundle.Edges[i].Resolution)
		sort.Strings(bundle.Edges[i].EvidenceIDs)
	}
	for i := range bundle.Conflicts {
		sort.Strings(bundle.Conflicts[i].CandidateIDs)
		sort.Strings(bundle.Conflicts[i].EvidenceIDs)
	}
	for i := range bundle.Claims {
		sort.Strings(bundle.Claims[i].EvidenceIDs)
	}
	for i := range bundle.Documents {
		sort.Strings(bundle.Documents[i].SubjectIDs)
		sort.Slice(bundle.Documents[i].Sections, func(a, b int) bool {
			if bundle.Documents[i].Sections[a].Ordinal == bundle.Documents[i].Sections[b].Ordinal {
				return bundle.Documents[i].Sections[a].ID < bundle.Documents[i].Sections[b].ID
			}
			return bundle.Documents[i].Sections[a].Ordinal < bundle.Documents[i].Sections[b].Ordinal
		})
	}
}

// IsCanonicalDecodedBundle reports whether a Bundle produced by encoding/json
// already satisfies every ordering, omitted-container, and operational-metadata
// rule applied by CanonicalBundle. The JSON-decoded precondition matters because
// arbitrary programmatic map values can change type during canonicalization.
// The check is a read-only linear scan for bounded trust-boundary readers.
func IsCanonicalDecodedBundle(bundle Bundle) bool {
	if bundle.Snapshot.CreatedAt != (time.Time{}) || bundle.Snapshot.RootPath != "" {
		return false
	}
	if nonNilEmptyMap(bundle.Snapshot.Policy) || nonNilEmptyMap(bundle.Snapshot.Metadata) ||
		nonNilEmptyMap(bundle.Snapshot.Tool.Attributes) || nonNilEmptySlice(bundle.Conflicts) ||
		nonNilEmptySlice(bundle.Documents) || nonNilEmptySlice(bundle.Claims) || nonNilEmptySlice(bundle.Paths) {
		return false
	}
	for _, key := range []string{"host", "pid", "duration_ms"} {
		if _, ok := bundle.Snapshot.Metadata[key]; ok {
			return false
		}
	}
	if !sort.SliceIsSorted(bundle.Artifacts, func(i, j int) bool {
		if bundle.Artifacts[i].Path == bundle.Artifacts[j].Path {
			return bundle.Artifacts[i].ID < bundle.Artifacts[j].ID
		}
		return bundle.Artifacts[i].Path < bundle.Artifacts[j].Path
	}) ||
		!sort.SliceIsSorted(bundle.Nodes, func(i, j int) bool { return bundle.Nodes[i].ID < bundle.Nodes[j].ID }) ||
		!sort.SliceIsSorted(bundle.Edges, func(i, j int) bool { return bundle.Edges[i].ID < bundle.Edges[j].ID }) ||
		!sort.SliceIsSorted(bundle.Evidence, func(i, j int) bool { return bundle.Evidence[i].ID < bundle.Evidence[j].ID }) ||
		!sort.SliceIsSorted(bundle.Diagnostics, func(i, j int) bool { return bundle.Diagnostics[i].ID < bundle.Diagnostics[j].ID }) ||
		!sort.SliceIsSorted(bundle.Conflicts, func(i, j int) bool { return bundle.Conflicts[i].ID < bundle.Conflicts[j].ID }) ||
		!sort.SliceIsSorted(bundle.Documents, func(i, j int) bool { return bundle.Documents[i].ID < bundle.Documents[j].ID }) ||
		!sort.SliceIsSorted(bundle.Claims, func(i, j int) bool { return bundle.Claims[i].ID < bundle.Claims[j].ID }) ||
		!sort.SliceIsSorted(bundle.Paths, func(i, j int) bool { return bundle.Paths[i].ID < bundle.Paths[j].ID }) {
		return false
	}
	for _, node := range bundle.Nodes {
		if nonNilEmptySlice(node.EvidenceIDs) || nonNilEmptyMap(node.Attributes) || !sort.StringsAreSorted(node.EvidenceIDs) {
			return false
		}
	}
	for _, edge := range bundle.Edges {
		if nonNilEmptySlice(edge.EvidenceIDs) || nonNilEmptyMap(edge.Attributes) ||
			edge.Resolution != NormalizeResolution(edge.Resolution) || !sort.StringsAreSorted(edge.EvidenceIDs) {
			return false
		}
	}
	for _, artifact := range bundle.Artifacts {
		if nonNilEmptyMap(artifact.Attributes) {
			return false
		}
	}
	for _, evidence := range bundle.Evidence {
		if nonNilEmptyMap(evidence.Attributes) {
			return false
		}
	}
	for _, diagnostic := range bundle.Diagnostics {
		if nonNilEmptyMap(diagnostic.Attributes) {
			return false
		}
	}
	for _, conflict := range bundle.Conflicts {
		if nonNilEmptySlice(conflict.CandidateIDs) || nonNilEmptySlice(conflict.EvidenceIDs) ||
			nonNilEmptyMap(conflict.Attributes) || !sort.StringsAreSorted(conflict.CandidateIDs) ||
			!sort.StringsAreSorted(conflict.EvidenceIDs) {
			return false
		}
	}
	for _, claim := range bundle.Claims {
		if nonNilEmptyMap(claim.Attributes) || !sort.StringsAreSorted(claim.EvidenceIDs) {
			return false
		}
	}
	for _, document := range bundle.Documents {
		if nonNilEmptySlice(document.SubjectIDs) || nonNilEmptySlice(document.Sections) || nonNilEmptyMap(document.Attributes) ||
			!sort.StringsAreSorted(document.SubjectIDs) || !sort.SliceIsSorted(document.Sections, func(i, j int) bool {
			if document.Sections[i].Ordinal == document.Sections[j].Ordinal {
				return document.Sections[i].ID < document.Sections[j].ID
			}
			return document.Sections[i].Ordinal < document.Sections[j].Ordinal
		}) {
			return false
		}
		for _, section := range document.Sections {
			if nonNilEmptySlice(section.ClaimIDs) || nonNilEmptySlice(section.EvidenceIDs) || nonNilEmptyMap(section.Attributes) {
				return false
			}
		}
	}
	for _, path := range bundle.Paths {
		if nonNilEmptySlice(path.EvidenceIDs) || nonNilEmptyMap(path.Attributes) {
			return false
		}
	}
	return true
}

func nonNilEmptySlice[T any](values []T) bool {
	return values != nil && len(values) == 0
}

func nonNilEmptyMap[K comparable, V any](values map[K]V) bool {
	return values != nil && len(values) == 0
}

// CanonicalBundle returns a deep-enough copy for deterministic serialization,
// removing machine-local and clock-derived fields while preserving provenance.
// It returns the zero bundle for a non-serializable value; trust-boundary code
// should use CanonicalJSON so it can propagate the corresponding error.
func CanonicalBundle(bundle Bundle) Bundle {
	out, _ := canonicalBundle(bundle)
	return out
}

func canonicalBundle(bundle Bundle) (Bundle, error) {
	data, err := json.Marshal(bundle)
	if err != nil {
		return Bundle{}, fmt.Errorf("encode canonical bundle: %w", err)
	}
	var out Bundle
	if err := json.Unmarshal(data, &out); err != nil {
		return Bundle{}, fmt.Errorf("clone canonical bundle: %w", err)
	}
	out.Snapshot.CreatedAt = time.Time{}
	out.Snapshot.RootPath = ""
	if out.Snapshot.Metadata != nil {
		delete(out.Snapshot.Metadata, "host")
		delete(out.Snapshot.Metadata, "pid")
		delete(out.Snapshot.Metadata, "duration_ms")
	}
	SortBundle(&out)
	return out, nil
}

// CanonicalJSON returns deterministic JSON for bundle after deep-copying it,
// removing operational snapshot fields, normalizing resolutions, and sorting
// canonical collections. The input bundle is not mutated.
func CanonicalJSON(bundle Bundle) ([]byte, error) {
	canonical, err := canonicalBundle(bundle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

// CanonicalDigest returns the lowercase SHA-256 digest of CanonicalJSON. It
// returns an empty string when the bundle cannot be represented as JSON.
func CanonicalDigest(bundle Bundle) string {
	data, err := CanonicalJSON(bundle)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SortFragment applies the same deterministic ordering rules used by a full
// bundle to a plugin or built-in extractor fragment.
func SortFragment(fragment *Fragment) {
	bundle := Bundle{
		Artifacts:   fragment.Artifacts,
		Nodes:       fragment.Nodes,
		Edges:       fragment.Edges,
		Evidence:    fragment.Evidence,
		Diagnostics: fragment.Diagnostics,
		Conflicts:   fragment.Conflicts,
		Documents:   fragment.Documents,
		Claims:      fragment.Claims,
		Paths:       fragment.Paths,
	}
	SortBundle(&bundle)
	fragment.Artifacts = bundle.Artifacts
	fragment.Nodes = bundle.Nodes
	fragment.Edges = bundle.Edges
	fragment.Evidence = bundle.Evidence
	fragment.Diagnostics = bundle.Diagnostics
	fragment.Conflicts = bundle.Conflicts
	fragment.Documents = bundle.Documents
	fragment.Claims = bundle.Claims
	fragment.Paths = bundle.Paths
}
