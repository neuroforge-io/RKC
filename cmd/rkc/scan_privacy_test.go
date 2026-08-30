package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/snapshot"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const privacyOriginSentinel = "https://example.test/NeuroForgeIO/private-origin-sentinel.git"

func TestWorkspacePrivacyModesEnforcePublicationBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-root-sentinel")

	t.Run("full retains operational and public provenance", func(t *testing.T) {
		bundle := privacyTestBundle(root)
		coverage, err := enforceWorkspacePrivacy(&bundle, "full")
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Snapshot.RootPath != root || bundle.Snapshot.Git.Origin != privacyOriginSentinel {
			t.Fatalf("full snapshot provenance = %q, %q", bundle.Snapshot.RootPath, bundle.Snapshot.Git.Origin)
		}
		assertPrivacyCoverage(t, bundle, coverage)
	})

	t.Run("paths relative drops machine path and retains portable origin", func(t *testing.T) {
		bundle := privacyTestBundle(root)
		coverage, err := enforceWorkspacePrivacy(&bundle, "paths-relative")
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Snapshot.RootPath != "" || bundle.Snapshot.Git.Origin != privacyOriginSentinel {
			t.Fatalf("paths-relative snapshot provenance = %q, %q", bundle.Snapshot.RootPath, bundle.Snapshot.Git.Origin)
		}
		if bundle.Artifacts[0].Path != "src/main.go" || bundle.Evidence[0].Source.Path != "src/main.go" {
			t.Fatalf("portable citations were removed: artifact=%q evidence=%+v", bundle.Artifacts[0].Path, bundle.Evidence[0].Source)
		}
		assertPrivacyCoverage(t, bundle, coverage)
	})

	t.Run("redacted removes public origin and keeps opaque identity", func(t *testing.T) {
		bundle := privacyTestBundle(root)
		bundle.Snapshot.Metadata["origin_alias"] = privacyOriginSentinel
		bundle.Nodes[0].Attributes["origin_alias"] = privacyOriginSentinel
		repositoryID := bundle.Snapshot.RepositoryID
		snapshotID := bundle.Snapshot.ID
		coverage, err := enforceWorkspacePrivacy(&bundle, "redacted")
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Snapshot.RootPath != "" || bundle.Snapshot.Git.Origin != "" {
			t.Fatalf("redacted snapshot provenance = %q, %q", bundle.Snapshot.RootPath, bundle.Snapshot.Git.Origin)
		}
		if _, present := bundle.Snapshot.Metadata["source_reference"]; present {
			t.Fatal("redacted snapshot retained source_reference")
		}
		if bundle.Snapshot.RepositoryID != repositoryID || bundle.Snapshot.ID != snapshotID {
			t.Fatalf("redaction changed opaque identities: repository=%q snapshot=%q", bundle.Snapshot.RepositoryID, bundle.Snapshot.ID)
		}
		repositoryNode := privacyRepositoryNode(t, bundle)
		if repositoryNode.QualifiedName != "" {
			t.Fatalf("redacted repository node qualified name = %q", repositoryNode.QualifiedName)
		}
		if _, present := repositoryNode.Attributes["git_origin"]; present {
			t.Fatal("redacted repository node retained git_origin")
		}
		encoded, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), privacyOriginSentinel) || strings.Contains(string(encoded), root) {
			t.Fatalf("redacted bundle retained private provenance: %s", encoded)
		}
		assertPrivacyCoverage(t, bundle, coverage)
		repeated, err := enforceWorkspacePrivacy(&bundle, "redacted")
		if err != nil || repeated.DeterministicOutputDigest != coverage.DeterministicOutputDigest {
			t.Fatalf("redacted transformation was not idempotent: digest=%q/%q err=%v", repeated.DeterministicOutputDigest, coverage.DeterministicOutputDigest, err)
		}
	})

	t.Run("errors disclose no rejected provenance", func(t *testing.T) {
		const secretSentinel = "privacy-transform-secret-sentinel"
		bundle := privacyTestBundle(root)
		bundle.Snapshot.Git.Origin = "https://alice:" + secretSentinel + "@example.test/repository.git"
		_, err := enforceWorkspacePrivacy(&bundle, "paths-relative")
		if err == nil || err.Error() != "workspace privacy transformation produced an invalid canonical bundle" {
			t.Fatalf("validation error = %v", err)
		}
		if strings.Contains(err.Error(), secretSentinel) {
			t.Fatalf("validation error disclosed provenance: %v", err)
		}
		if _, err := enforceWorkspacePrivacy(nil, "paths-relative"); err == nil || strings.Contains(err.Error(), secretSentinel) {
			t.Fatalf("nil transformation error = %v", err)
		}
		if _, err := enforceWorkspacePrivacy(&bundle, "unsupported"); err == nil || err.Error() != "workspace privacy mode is invalid" {
			t.Fatalf("mode error = %v", err)
		}
	})
}

func TestWorkspacePrivacyPublicationMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-repository-root")
	output := filepath.Join(t.TempDir(), "private-atlas-output")
	for _, targetKey := range []string{"atlas_target", "export_root"} {
		full := scanPublicationMetadata("full", targetKey, output, privacyOriginSentinel, root, false)
		if full[targetKey] != output || full["repository_root"] != root || full["repository_origin"] != privacyOriginSentinel {
			t.Fatalf("full %s metadata = %#v", targetKey, full)
		}

		relative := scanPublicationMetadata("paths-relative", targetKey, output, privacyOriginSentinel, root, false)
		if len(relative) != 1 || relative["repository_origin"] != privacyOriginSentinel {
			t.Fatalf("paths-relative %s metadata = %#v", targetKey, relative)
		}
		encoded, err := json.Marshal(relative)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), output) {
			t.Fatalf("paths-relative metadata retained an operational path: %s", encoded)
		}

		redacted := scanPublicationMetadata("redacted", targetKey, output, privacyOriginSentinel, root, false)
		if len(redacted) != 0 {
			t.Fatalf("redacted %s metadata = %#v", targetKey, redacted)
		}
	}

	temporary := scanPublicationMetadata("full", "atlas_target", output, privacyOriginSentinel, root, true)
	if _, present := temporary["repository_root"]; present {
		t.Fatalf("temporary repository root was persisted: %#v", temporary)
	}
}

func TestPathsRelativeScanPublishesNoAbsoluteOperationalPaths(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "private-repository-sentinel")
	output := filepath.Join(base, "private-output-sentinel")
	stateDirectory := filepath.Join(base, "private-state-sentinel")
	runsDirectory := filepath.Join(base, "private-runs-sentinel")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("# Privacy fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runScanContext(t.Context(), []string{
		"--out", output,
		"--state-dir", stateDirectory,
		"--runs-dir", runsDirectory,
		"--no-cache",
		"--stage-workers", "1",
		"--stage-memory-mib", "512",
		"--no-plugins",
		"--no-frameworks",
		"--no-secret-scan",
		"--include-sources=false",
		"--no-static-site",
		"--no-jsonl-graph",
		"--no-search-index",
		"--no-integrations",
		repository,
	}); err != nil {
		t.Fatal(err)
	}

	var atlasBundle rkcmodel.Bundle
	readPrivacyJSON(t, filepath.Join(output, "bundle.json"), &atlasBundle)
	if atlasBundle.Snapshot.RootPath != "" {
		t.Fatalf("atlas root path = %q", atlasBundle.Snapshot.RootPath)
	}
	assertFilesDoNotContain(t, []string{
		filepath.Join(output, "bundle.json"),
		filepath.Join(output, "rkc.manifest.json"),
		filepath.Join(output, "rkc.execution.json"),
	}, repository, output)

	store, err := snapshot.Open(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	storedBundle, storedCoverage, record, err := store.LoadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if storedBundle.Snapshot.RootPath != "" {
		t.Fatalf("stored root path = %q", storedBundle.Snapshot.RootPath)
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recordJSON), repository) || strings.Contains(string(recordJSON), output) {
		t.Fatalf("snapshot record retained an operational path: %s", recordJSON)
	}
	assertPrivacyCoverage(t, storedBundle, storedCoverage)
}

func TestRedactedSQLitePrivacyAndModeTransitions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for repository-origin privacy integration")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(base, "private-git-repository-sentinel")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("# Private origin fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initializePrivacyGitRepository(t, repository)

	redactedConfig := filepath.Join(base, "redacted.json")
	pathsRelativeConfig := filepath.Join(base, "paths-relative.json")
	writePrivacyConfiguration(t, redactedConfig, "redacted")
	writePrivacyConfiguration(t, pathsRelativeConfig, "paths-relative")

	database := filepath.Join(base, "privacy.sqlite")
	redactedOutput := filepath.Join(base, "redacted-atlas")
	runPrivacySQLiteScan(t, redactedConfig, redactedOutput, database, filepath.Join(base, "redacted-runs"), repository)

	var redactedBundle rkcmodel.Bundle
	var redactedCoverage rkcmodel.Coverage
	readPrivacyJSON(t, filepath.Join(redactedOutput, "bundle.json"), &redactedBundle)
	readPrivacyJSON(t, filepath.Join(redactedOutput, "coverage.json"), &redactedCoverage)
	if redactedBundle.Snapshot.Git.Origin != "" || redactedBundle.Snapshot.RootPath != "" {
		t.Fatalf("redacted atlas provenance = %q, %q", redactedBundle.Snapshot.Git.Origin, redactedBundle.Snapshot.RootPath)
	}
	assertPrivacyCoverage(t, redactedBundle, redactedCoverage)
	assertRegularTreeDoesNotContain(
		t,
		redactedOutput,
		privacyOriginSentinel,
		base,
		repository,
		redactedOutput,
		database,
	)

	redactedDataset, err := loadSQLiteDataset(t.Context(), database, redactedBundle.Snapshot.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if redactedDataset.Bundle.Snapshot.Git.Origin != "" || redactedDataset.Bundle.Snapshot.RootPath != "" {
		t.Fatalf("redacted SQLite provenance = %q, %q", redactedDataset.Bundle.Snapshot.Git.Origin, redactedDataset.Bundle.Snapshot.RootPath)
	}
	assertPrivacyCoverage(t, redactedDataset.Bundle, redactedDataset.Coverage)
	assertJSONValuesDoNotContain(
		t,
		[]any{redactedDataset.Manifest, redactedDataset.Bundle, redactedDataset.Coverage},
		privacyOriginSentinel,
		base,
		repository,
		redactedOutput,
	)
	assertSQLiteBytesDoNotContain(t, database, privacyOriginSentinel, base, repository, redactedOutput)

	pathsRelativeOutput := filepath.Join(base, "paths-relative-atlas")
	runPrivacySQLiteScan(t, pathsRelativeConfig, pathsRelativeOutput, database, filepath.Join(base, "paths-relative-runs"), repository)
	var pathsRelativeBundle rkcmodel.Bundle
	var pathsRelativeCoverage rkcmodel.Coverage
	readPrivacyJSON(t, filepath.Join(pathsRelativeOutput, "bundle.json"), &pathsRelativeBundle)
	readPrivacyJSON(t, filepath.Join(pathsRelativeOutput, "coverage.json"), &pathsRelativeCoverage)
	if pathsRelativeBundle.Snapshot.Git.Origin != privacyOriginSentinel || pathsRelativeBundle.Snapshot.RootPath != "" {
		t.Fatalf("paths-relative atlas provenance = %q, %q", pathsRelativeBundle.Snapshot.Git.Origin, pathsRelativeBundle.Snapshot.RootPath)
	}
	if pathsRelativeBundle.Snapshot.RepositoryID != redactedBundle.Snapshot.RepositoryID {
		t.Fatalf("privacy transition changed repository identity: %q/%q", redactedBundle.Snapshot.RepositoryID, pathsRelativeBundle.Snapshot.RepositoryID)
	}
	assertPrivacyCoverage(t, pathsRelativeBundle, pathsRelativeCoverage)
	current, err := loadSQLiteDataset(t.Context(), database, "", redactedBundle.Snapshot.RepositoryID)
	if err != nil {
		t.Fatalf("load paths-relative current after redacted snapshot: %v", err)
	}
	if current.Manifest.ID != pathsRelativeBundle.Snapshot.ID || current.Bundle.Snapshot.Git.Origin != privacyOriginSentinel {
		t.Fatalf("paths-relative transition current = %q origin=%q", current.Manifest.ID, current.Bundle.Snapshot.Git.Origin)
	}
	assertPrivacyCoverage(t, current.Bundle, current.Coverage)

	reverseDatabase := filepath.Join(base, "reverse-privacy.sqlite")
	reversePathsOutput := filepath.Join(base, "reverse-paths-relative-atlas")
	runPrivacySQLiteScan(t, pathsRelativeConfig, reversePathsOutput, reverseDatabase, filepath.Join(base, "reverse-paths-runs"), repository)
	reverseRedactedOutput := filepath.Join(base, "reverse-redacted-atlas")
	runPrivacySQLiteScan(t, redactedConfig, reverseRedactedOutput, reverseDatabase, filepath.Join(base, "reverse-redacted-runs"), repository)
	var reverseRedactedBundle rkcmodel.Bundle
	readPrivacyJSON(t, filepath.Join(reverseRedactedOutput, "bundle.json"), &reverseRedactedBundle)
	reverseCurrent, err := loadSQLiteDataset(t.Context(), reverseDatabase, "", reverseRedactedBundle.Snapshot.RepositoryID)
	if err != nil {
		t.Fatalf("load redacted current after paths-relative snapshot: %v", err)
	}
	if reverseCurrent.Manifest.ID != reverseRedactedBundle.Snapshot.ID || reverseCurrent.Bundle.Snapshot.Git.Origin != "" {
		t.Fatalf("redacted reverse transition current = %q origin=%q", reverseCurrent.Manifest.ID, reverseCurrent.Bundle.Snapshot.Git.Origin)
	}
	assertPrivacyCoverage(t, reverseCurrent.Bundle, reverseCurrent.Coverage)
}

func privacyTestBundle(root string) rkcmodel.Bundle {
	repositoryID := rkcmodel.StableID("repository", privacyOriginSentinel)
	snapshotID := rkcmodel.StableID("snapshot", "privacy-mode-test")
	artifactID := rkcmodel.StableID("artifact", snapshotID, "src/main.go")
	evidenceID := rkcmodel.StableID("evidence", artifactID, "declared")
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion,
			ID:            snapshotID,
			RepositoryID:  repositoryID,
			CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
			Status:        "committed",
			RootName:      "privacy-fixture",
			RootPath:      root,
			ContentDigest: strings.Repeat("a", 64),
			Git:           rkcmodel.GitInfo{Origin: privacyOriginSentinel},
			Tool:          rkcmodel.ToolInfo{Name: "rkc", Version: "test"},
			Metadata:      map[string]string{"source_reference": privacyOriginSentinel},
		},
		Artifacts: []rkcmodel.Artifact{{
			ID: artifactID, Path: "src/main.go", Kind: "file", Language: "go",
			Status: "syntax_parsed", Text: true,
		}},
		Nodes: []rkcmodel.Node{
			{
				ID: repositoryID, LogicalID: repositoryID, Kind: "repository",
				Name: "privacy-fixture", QualifiedName: privacyOriginSentinel,
				Visibility: "repository",
				Attributes: map[string]any{"snapshot_id": snapshotID, "git_origin": privacyOriginSentinel},
			},
		},
		Evidence: []rkcmodel.Evidence{{
			ID: evidenceID, Kind: "declared", Method: "privacy-test", Confidence: 1,
			Source: &rkcmodel.SourceRange{ArtifactID: artifactID, Path: "src/main.go", StartLine: 1, EndLine: 1},
		}},
	}
}

func privacyRepositoryNode(t *testing.T, bundle rkcmodel.Bundle) rkcmodel.Node {
	t.Helper()
	for _, node := range bundle.Nodes {
		if node.Kind == "repository" && node.ID == bundle.Snapshot.RepositoryID {
			return node
		}
	}
	t.Fatal("repository node is missing")
	return rkcmodel.Node{}
}

func assertPrivacyCoverage(t *testing.T, bundle rkcmodel.Bundle, coverage rkcmodel.Coverage) {
	t.Helper()
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	if report.HasErrors() {
		t.Fatalf("privacy bundle validation = %+v", report.Diagnostics)
	}
	if want := rkcmodel.CanonicalDigest(bundle); coverage.DeterministicOutputDigest != want {
		t.Fatalf("coverage digest = %q, want %q", coverage.DeterministicOutputDigest, want)
	}
}

func readPrivacyJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func assertFilesDoNotContain(t *testing.T, paths []string, sentinels ...string) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if forbidden, present := firstPresentPrivacySentinel(data, sentinels); present {
			t.Fatalf("%s retained private path %q", path, forbidden)
		}
	}
}

func initializePrivacyGitRepository(t *testing.T, repository string) {
	t.Helper()
	emptyHooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(emptyHooks, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"init", "--quiet", repository},
		{"-C", repository, "config", "user.name", "RKC Privacy Test"},
		{"-C", repository, "config", "user.email", "privacy-test@example.invalid"},
		{"-C", repository, "config", "commit.gpgsign", "false"},
		{"-C", repository, "config", "core.hooksPath", emptyHooks},
		{"-C", repository, "add", "--", "README.md"},
		{"-C", repository, "commit", "--quiet", "-m", "privacy fixture"},
		{"-C", repository, "remote", "add", "origin", privacyOriginSentinel},
	}
	for _, arguments := range commands {
		command := exec.CommandContext(t.Context(), "git", arguments...)
		command.Env = append(
			os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_TERMINAL_PROMPT=0",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("initialize Git privacy fixture: %v: %s", err, output)
		}
	}
}

func writePrivacyConfiguration(t *testing.T, path, mode string) {
	t.Helper()
	configuration := defaultConfiguration()
	configuration.Workspace.PrivacyMode = mode
	if err := writeInitConfiguration(path, mustJSON(t, configuration), false); err != nil {
		t.Fatal(err)
	}
}

func runPrivacySQLiteScan(t *testing.T, config, output, database, runs, repository string) {
	t.Helper()
	_, err := captureStdout(t, func() error {
		return runScanContext(t.Context(), []string{
			"--config", config,
			"--out", output,
			"--database", database,
			"--runs-dir", runs,
			"--no-cache",
			"--stage-workers", "1",
			"--stage-memory-mib", "512",
			"--no-plugins",
			"--no-frameworks",
			"--no-secret-scan",
			"--include-sources=false",
			"--no-static-site",
			"--no-jsonl-graph",
			"--no-search-index",
			"--no-integrations",
			repository,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertRegularTreeDoesNotContain(t *testing.T, root string, sentinels ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if forbidden, present := firstPresentPrivacySentinel(data, sentinels); present {
			t.Fatalf("%s retained private provenance %q", path, forbidden)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertSQLiteBytesDoNotContain(t *testing.T, database string, sentinels ...string) {
	t.Helper()
	for _, path := range []string{database, database + "-wal", database + "-shm"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if forbidden, present := firstPresentPrivacySentinel(data, sentinels); present {
			t.Fatalf("SQLite bytes retained private provenance %q", forbidden)
		}
	}
}

func assertJSONValuesDoNotContain(t *testing.T, values []any, sentinels ...string) {
	t.Helper()
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if forbidden, present := firstPresentPrivacySentinel(data, sentinels); present {
			t.Fatalf("durable value retained private provenance %q", forbidden)
		}
	}
}

func firstPresentPrivacySentinel(data []byte, sentinels []string) (string, bool) {
	text := string(data)
	for _, sentinel := range sentinels {
		if sentinel == "" {
			continue
		}
		if strings.Contains(text, sentinel) {
			return sentinel, true
		}
		encoded, err := json.Marshal(sentinel)
		if err == nil && len(encoded) >= 2 && strings.Contains(text, string(encoded[1:len(encoded)-1])) {
			return sentinel, true
		}
	}
	return "", false
}
