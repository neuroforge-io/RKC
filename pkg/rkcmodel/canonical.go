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

func StableID(namespace string, parts ...string) string {
	key := namespace + "\x00" + strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("rkc:%s:%s", namespace, hex.EncodeToString(sum[:12]))
}

func ContentID(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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

func CanonicalJSON(bundle Bundle) ([]byte, error) {
	canonical, err := canonicalBundle(bundle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

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
