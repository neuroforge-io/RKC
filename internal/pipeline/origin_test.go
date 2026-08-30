package pipeline

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestCredentialVariantsProduceOneCanonicalRepositoryIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := initialiseOriginFixture(t)
	const canonical = "https://example.test/Owner/Repo.git"
	firstRemote := "https://alice:REMOTE_SECRET_ONE@Example.test:443/Owner/Repo.git?token=REMOTE_QUERY_ONE#REMOTE_FRAGMENT_ONE"
	setOriginFixtureRemote(t, root, firstRemote)

	opts := originFixtureOptions(root)
	opts.Origin = "https://caller:SOURCE_SECRET_ONE@EXAMPLE.test:0443/Owner/Repo.git?token=SOURCE_QUERY_ONE#SOURCE_FRAGMENT_ONE"
	first, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	oracle, _, err := scanSequential(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalOriginBundle(t, first, canonical)
	assertCanonicalOriginBundle(t, oracle, canonical)
	if first.Snapshot.ID != oracle.Snapshot.ID || first.Snapshot.RepositoryID != oracle.Snapshot.RepositoryID {
		t.Fatalf("staged and sequential identities disagree: %q/%q versus %q/%q", first.Snapshot.RepositoryID, first.Snapshot.ID, oracle.Snapshot.RepositoryID, oracle.Snapshot.ID)
	}

	secondRemote := "https://bob:REMOTE_SECRET_TWO@example.test/Owner/Repo.git?token=REMOTE_QUERY_TWO#REMOTE_FRAGMENT_TWO"
	setOriginFixtureRemote(t, root, secondRemote)
	opts.Origin = "https://service:SOURCE_SECRET_TWO@example.test/Owner/Repo.git?token=SOURCE_QUERY_TWO#SOURCE_FRAGMENT_TWO"
	second, _, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalOriginBundle(t, second, canonical)
	if second.Snapshot.RepositoryID != first.Snapshot.RepositoryID || second.Snapshot.ID != first.Snapshot.ID {
		t.Fatalf("credential-only changes altered identity: first=%q/%q digest=%q dirty=%t working=%q second=%q/%q digest=%q dirty=%t working=%q", first.Snapshot.RepositoryID, first.Snapshot.ID, first.Snapshot.ContentDigest, first.Snapshot.Git.Dirty, first.Snapshot.Git.WorkingTreeDigest, second.Snapshot.RepositoryID, second.Snapshot.ID, second.Snapshot.ContentDigest, second.Snapshot.Git.Dirty, second.Snapshot.Git.WorkingTreeDigest)
	}
	firstDigest := rkcmodel.CanonicalDigest(first)
	secondDigest := rkcmodel.CanonicalDigest(second)
	if firstDigest == "" || secondDigest == "" {
		t.Fatal("canonical bundle digest is empty")
	}
	if firstDigest != secondDigest {
		t.Fatalf("credential-only changes altered canonical bundle digest: %q != %q", firstDigest, secondDigest)
	}
}

func TestOriginDisagreementFailsWithoutDisclosure(t *testing.T) {
	const suppliedSecret = "SUPPLIED_SECRET_SENTINEL"
	const discoveredSecret = "DISCOVERED_SECRET_SENTINEL"
	supplied, err := publicSuppliedOrigin("https://alice:" + suppliedSecret + "@one.example/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := publicDiscoveredOrigin("https://bob:" + discoveredSecret + "@two.example/owner/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconcileRepositoryOrigin(supplied, discovered)
	if err == nil {
		t.Fatal("expected disagreeing origins to fail")
	}
	if strings.Contains(err.Error(), suppliedSecret) || strings.Contains(err.Error(), discoveredSecret) || strings.Contains(err.Error(), "one.example") || strings.Contains(err.Error(), "two.example") {
		t.Fatalf("origin disagreement disclosed caller data: %q", err)
	}
}

func TestLocalGitOriginsAreNotPublished(t *testing.T) {
	for _, origin := range []string{"../private/repository.git", "/srv/private/repository.git", "file:///Users/Alice/private/repository.git"} {
		canonical, err := publicDiscoveredOrigin(origin)
		if err != nil || canonical != "" {
			t.Fatalf("publicDiscoveredOrigin(%q) = %q, %v; want omitted", origin, canonical, err)
		}
	}
}

func initialiseOriginFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, arguments := range [][]string{
		{"init", "--quiet", root},
		{"-C", root, "config", "user.name", "RKC fixture"},
		{"-C", root, "config", "user.email", "rkc@example.invalid"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Origin fixture\n")
	for _, arguments := range [][]string{
		{"-C", root, "add", "README.md"},
		{"-C", root, "commit", "--quiet", "-m", "fixture"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	return root
}

func setOriginFixtureRemote(t *testing.T, root, remote string) {
	t.Helper()
	command := exec.Command("git", "-C", root, "config", "remote.origin.url", remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("set fixture origin: %v: %s", err, output)
	}
}

func originFixtureOptions(root string) Options {
	return Options{
		Root: root, StageWorkers: 1,
		DisablePlugins: true, DisableFrameworks: true, DisableSecretScan: true,
	}
}

func assertCanonicalOriginBundle(t *testing.T, bundle rkcmodel.Bundle, canonical string) {
	t.Helper()
	if bundle.Snapshot.Git.Origin != canonical || bundle.Snapshot.Metadata["source_reference"] != canonical {
		t.Fatalf("snapshot origin = %q metadata = %q, want %q", bundle.Snapshot.Git.Origin, bundle.Snapshot.Metadata["source_reference"], canonical)
	}
	wantRepositoryID := rkcmodel.StableID("repository", canonical)
	if bundle.Snapshot.RepositoryID != wantRepositoryID {
		t.Fatalf("repository ID = %q, want %q", bundle.Snapshot.RepositoryID, wantRepositoryID)
	}
	found := false
	for _, node := range bundle.Nodes {
		if node.Kind != "repository" {
			continue
		}
		found = true
		if node.QualifiedName != canonical || node.Attributes["git_origin"] != canonical {
			t.Fatalf("repository provenance = %q/%v, want %q", node.QualifiedName, node.Attributes["git_origin"], canonical)
		}
	}
	if !found {
		t.Fatal("repository node missing")
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET", "alice", "bob", "caller", "service", "QUERY", "FRAGMENT"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("bundle disclosed origin credential component %q", secret)
		}
	}
}
