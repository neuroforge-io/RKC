package rkcmodel

import (
	"encoding/json"
	"strings"
	"testing"
)

const provenanceSecretSentinel = "RKC_PROVENANCE_SECRET_SENTINEL"

func TestValidateBundleAcceptsCanonicalRepositoryProvenance(t *testing.T) {
	t.Parallel()

	bundle := validBundleWithRepositoryProvenance()
	report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() || len(report.Diagnostics) != 0 {
		t.Fatalf("canonical repository provenance rejected: %+v", report.Diagnostics)
	}

	withoutSourceReference := validBundleWithRepositoryProvenance()
	delete(withoutSourceReference.Snapshot.Metadata, "source_reference")
	report = ValidateBundle(withoutSourceReference, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() {
		t.Fatalf("optional source reference rejected: %+v", report.Diagnostics)
	}

	emptySourceReference := validBundleWithRepositoryProvenance()
	emptySourceReference.Snapshot.Metadata["source_reference"] = ""
	report = ValidateBundle(emptySourceReference, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() {
		t.Fatalf("empty optional source reference rejected: %+v", report.Diagnostics)
	}
}

func TestValidateBundlePreservesOriginlessCompatibility(t *testing.T) {
	t.Parallel()

	bundle := validBundleForTest()
	bundle.Snapshot.RepositoryID = "legacy-local-repository"
	bundle.Nodes = append(bundle.Nodes, Node{
		ID:            bundle.Snapshot.RepositoryID,
		Kind:          "repository",
		Name:          "local repository",
		QualifiedName: "machine-local-name",
		Attributes:    map[string]any{"git_origin": "legacy-local-name"},
	})

	report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() {
		t.Fatalf("originless compatibility bundle rejected: %+v", report.Diagnostics)
	}
}

func TestValidateBundleRejectsInconsistentRepositoryProvenanceWithoutDisclosure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*Bundle)
		wantCode string
	}{
		{
			name: "credentialed origin",
			mutate: func(bundle *Bundle) {
				bundle.Snapshot.Git.Origin = "https://alice:" + provenanceSecretSentinel + "@example.test/Owner/Repo.git?token=" + provenanceSecretSentinel
			},
			wantCode: "RKC-MOD-056",
		},
		{
			name: "noncanonical source reference",
			mutate: func(bundle *Bundle) {
				bundle.Snapshot.Metadata["source_reference"] = "https://alice:" + provenanceSecretSentinel + "@example.test/Owner/Repo.git#" + provenanceSecretSentinel
			},
			wantCode: "RKC-MOD-056",
		},
		{
			name: "different canonical source reference",
			mutate: func(bundle *Bundle) {
				bundle.Snapshot.Metadata["source_reference"] = "ssh://example.test/Owner/Repo.git"
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository identifier mismatch",
			mutate: func(bundle *Bundle) {
				bundle.Snapshot.RepositoryID = "rkc:repository:incorrect"
			},
			wantCode: "RKC-MOD-058",
		},
		{
			name: "repository qualified name mismatch",
			mutate: func(bundle *Bundle) {
				repositoryNode(bundle).QualifiedName = "https://example.test/Other/Repo.git"
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository qualified name missing",
			mutate: func(bundle *Bundle) {
				repositoryNode(bundle).QualifiedName = ""
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository logical identifier mismatch",
			mutate: func(bundle *Bundle) {
				repositoryNode(bundle).LogicalID = "rkc:repository:other"
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository git origin mismatch",
			mutate: func(bundle *Bundle) {
				repositoryNode(bundle).Attributes["git_origin"] = "https://example.test/Other/Repo.git"
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository git origin missing",
			mutate: func(bundle *Bundle) {
				delete(repositoryNode(bundle).Attributes, "git_origin")
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository git origin wrong type",
			mutate: func(bundle *Bundle) {
				repositoryNode(bundle).Attributes["git_origin"] = []byte(provenanceSecretSentinel)
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "repository node missing",
			mutate: func(bundle *Bundle) {
				nodes := make([]Node, 0, len(bundle.Nodes))
				for _, node := range bundle.Nodes {
					if node.Kind != "repository" {
						nodes = append(nodes, node)
					}
				}
				bundle.Nodes = nodes
			},
			wantCode: "RKC-MOD-057",
		},
		{
			name: "source reference without origin",
			mutate: func(bundle *Bundle) {
				bundle.Snapshot.Git.Origin = ""
			},
			wantCode: "RKC-MOD-057",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bundle := validBundleWithRepositoryProvenance()
			test.mutate(&bundle)
			report := ValidateBundle(bundle, ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
			if !report.HasErrors() || !reportHasDiagnosticCode(report, test.wantCode) {
				t.Fatalf("invalid provenance was not rejected with %s: %+v", test.wantCode, report.Diagnostics)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), provenanceSecretSentinel) || strings.Contains(string(encoded), "alice:") {
				t.Fatalf("validation diagnostics disclosed repository provenance: %s", encoded)
			}
		})
	}
}

func validBundleWithRepositoryProvenance() Bundle {
	const origin = "https://example.test/Owner/Repo.git"
	bundle := validBundleForTest()
	bundle.Snapshot.Git.Origin = origin
	bundle.Snapshot.RepositoryID = StableID("repository", origin)
	bundle.Snapshot.Metadata = map[string]string{"source_reference": origin}
	bundle.Nodes = append(bundle.Nodes, Node{
		ID:            bundle.Snapshot.RepositoryID,
		LogicalID:     bundle.Snapshot.RepositoryID,
		Kind:          "repository",
		Name:          "Repo",
		QualifiedName: origin,
		Attributes:    map[string]any{"git_origin": origin},
	})
	return bundle
}

func repositoryNode(bundle *Bundle) *Node {
	for index := range bundle.Nodes {
		if bundle.Nodes[index].Kind == "repository" && bundle.Nodes[index].ID == bundle.Snapshot.RepositoryID {
			return &bundle.Nodes[index]
		}
	}
	panic("repository node fixture is missing")
}

func reportHasDiagnosticCode(report ValidationReport, code string) bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
