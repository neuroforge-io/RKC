package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repository-knowledge-compiler/rkc/internal/docparse"
	"github.com/repository-knowledge-compiler/rkc/internal/framework/envkeys"
	"github.com/repository-knowledge-compiler/rkc/internal/framework/jsonschema"
	"github.com/repository-knowledge-compiler/rkc/internal/framework/manifest"
	"github.com/repository-knowledge-compiler/rkc/internal/framework/openapi"
	"github.com/repository-knowledge-compiler/rkc/internal/framework/secretpack"
	"github.com/repository-knowledge-compiler/rkc/internal/inventory"
	"github.com/repository-knowledge-compiler/rkc/internal/lang/goast"
	"github.com/repository-knowledge-compiler/rkc/internal/lang/tssyntax"
	"github.com/repository-knowledge-compiler/rkc/internal/plugin"
	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

type Options struct {
	Root               string
	MaxFileBytes       int64
	MaxTextBytes       int64
	MaxRepositoryBytes int64
	MaxFiles           int
	Excludes           []string

	PythonInterpreter string
	PythonPlugin      string
	PluginTimeout     time.Duration
	PluginMaxOutput   int64
	PluginMaxStderr   int64

	DisablePlugins    bool
	DisablePythonAST  bool
	DisableGoAST      bool
	DisableTypeScript bool
	DisableFrameworks bool
	DisableMarkdown   bool
	DisableOpenAPI    bool
	DisableJSONSchema bool
	DisableManifests  bool
	DisableEnvKeys    bool
	DisableSecretScan bool

	ToolVersion      string
	SourceReference  string
	ConfigDigest     string
	PolicyDigest     string
	PluginLockDigest string
	ToolchainDigest  string
}

func Scan(ctx context.Context, opts Options) (rkcmodel.Bundle, rkcmodel.Coverage, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, fmt.Errorf("root is not a directory: %s", root)
	}

	inv, err := inventory.Scan(inventory.Options{Root: root, MaxFileBytes: opts.MaxFileBytes, MaxTextBytes: opts.MaxTextBytes, MaxRepositoryBytes: opts.MaxRepositoryBytes, MaxFiles: opts.MaxFiles, Excludes: opts.Excludes})
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, err
	}
	gitInfo := inspectGit(ctx, root)
	rootName := filepath.Base(root)
	repositoryIdentity := firstNonEmpty(opts.SourceReference, gitInfo.Origin, rootName)
	repositoryID := rkcmodel.StableID("repository", repositoryIdentity)
	snapshotID := rkcmodel.StableID("snapshot", repositoryIdentity, gitInfo.Commit, inv.Digest, opts.ConfigDigest, opts.PolicyDigest, opts.PluginLockDigest, opts.ToolchainDigest, rkcmodel.SchemaVersion)
	if gitInfo.Dirty {
		gitInfo.WorkingTreeDigest = inv.Digest
	}
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{
		SchemaVersion: rkcmodel.SchemaVersion, ID: snapshotID, RepositoryID: repositoryID, CreatedAt: time.Now().UTC(), Status: "committed",
		RootName: rootName, RootPath: root, ContentDigest: inv.Digest, ConfigDigest: opts.ConfigDigest, PolicyDigest: opts.PolicyDigest,
		PluginLockDigest: opts.PluginLockDigest, ToolchainDigest: opts.ToolchainDigest, Git: gitInfo,
		Tool:     rkcmodel.ToolInfo{Name: "rkc", Version: firstNonEmpty(opts.ToolVersion, "development")},
		Policy:   map[string]any{"max_file_bytes": opts.MaxFileBytes, "max_text_bytes": opts.MaxTextBytes, "max_repository_bytes": opts.MaxRepositoryBytes, "max_files": opts.MaxFiles, "excludes": uniqueSorted(opts.Excludes), "plugins": !opts.DisablePlugins, "frameworks": !opts.DisableFrameworks, "secret_scan": !opts.DisableSecretScan},
		Metadata: map[string]string{"source_reference": opts.SourceReference},
	}, Artifacts: inv.Artifacts, Diagnostics: inv.Diagnostics}
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{ID: repositoryID, LogicalID: repositoryID, Kind: "repository", Name: rootName, QualifiedName: repositoryIdentity, Visibility: "repository", Attributes: map[string]any{"snapshot_id": snapshotID, "git_commit": gitInfo.Commit, "git_origin": gitInfo.Origin}})
	for _, artifact := range bundle.Artifacts {
		bundle.Nodes = append(bundle.Nodes, artifactNode(artifact))
		bundle.Edges = append(bundle.Edges, rkcmodel.Edge{ID: rkcmodel.StableID("edge", "contains", repositoryID, artifact.ID), Kind: "contains", From: repositoryID, To: artifact.ID, Resolution: rkcmodel.ResolutionDeclared, Confidence: 1, Producer: "rkc.inventory"})
	}

	files := make([]pluginapi.FileRef, 0, len(bundle.Artifacts))
	artifactByPath := map[string]string{}
	for _, artifact := range bundle.Artifacts {
		artifactByPath[artifact.Path] = artifact.ID
		if artifact.Text && artifact.Status == "text" {
			files = append(files, pluginapi.FileRef{ArtifactID: artifact.ID, Path: artifact.Path, Language: artifact.Language, MediaType: artifact.MediaType, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes, Materialized: filepath.Join(root, filepath.FromSlash(artifact.Path))})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	parsed := map[string]struct{}{}

	if !opts.DisablePlugins {
		if !opts.DisablePythonAST {
			pythonFiles := filterFiles(files, func(file pluginapi.FileRef) bool { return file.Language == "python" })
			if len(pythonFiles) > 0 {
				legacy := make([]plugin.FileRef, 0, len(pythonFiles))
				for _, file := range pythonFiles {
					legacy = append(legacy, plugin.FileRef{ID: file.ArtifactID, Path: file.Path, Language: file.Language, SHA256: file.SHA256})
				}
				fragment, runErr := plugin.RunPython(ctx, plugin.Request{SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: snapshotID, Root: root, Files: legacy}, plugin.PythonOptions{Interpreter: opts.PythonInterpreter, Script: opts.PythonPlugin, Timeout: opts.PluginTimeout, MaxOutputBytes: opts.PluginMaxOutput, MaxStderrBytes: opts.PluginMaxStderr})
				if runErr != nil {
					bundle.Diagnostics = append(bundle.Diagnostics, adapterError("RKC-PY-2001", "rkc.python-ast", runErr))
				} else {
					mergeFragment(&bundle, fragment)
					markParsed(parsed, pythonFiles)
				}
			}
		}
		if !opts.DisableGoAST {
			goFiles := filterFiles(files, func(file pluginapi.FileRef) bool { return file.Language == "go" })
			if len(goFiles) > 0 {
				fragment, extractErr := goast.Extract(goast.Options{Root: root, SnapshotID: snapshotID, Files: goFiles})
				if extractErr != nil {
					bundle.Diagnostics = append(bundle.Diagnostics, adapterError("RKC-GO-2001", goast.PluginID, extractErr))
				} else {
					mergeFragment(&bundle, fragment)
					markParsed(parsed, goFiles)
				}
			}
		}
		if !opts.DisableTypeScript {
			tsFiles := filterFiles(files, func(file pluginapi.FileRef) bool {
				return file.Language == "typescript" || file.Language == "javascript"
			})
			if len(tsFiles) > 0 {
				fragment, extractErr := tssyntax.Extract(tssyntax.Options{Root: root, SnapshotID: snapshotID, Files: tsFiles})
				if extractErr != nil {
					bundle.Diagnostics = append(bundle.Diagnostics, adapterError("RKC-TS-2001", tssyntax.PluginID, extractErr))
				} else {
					mergeFragment(&bundle, fragment)
					markParsed(parsed, tsFiles)
				}
			}
		}
	}

	if !opts.DisableFrameworks {
		if !opts.DisableMarkdown {
			candidates := filterFiles(files, func(file pluginapi.FileRef) bool { return file.Language == "markdown" || file.Language == "mdx" })
			if len(candidates) > 0 {
				fragment, extractErr := docparse.Extract(docparse.Options{Root: root, SnapshotID: snapshotID, Files: candidates, Artifacts: artifactByPath})
				handleFragment(&bundle, fragment, extractErr, "RKC-DOC-2001", docparse.PluginID)
				if extractErr == nil {
					markParsed(parsed, candidates)
				}
			}
		}
		jsonFiles := filterFiles(files, func(file pluginapi.FileRef) bool { return file.Language == "json" })
		if !opts.DisableOpenAPI && len(jsonFiles) > 0 {
			fragment, extractErr := openapi.Extract(openapi.Options{Root: root, Files: jsonFiles})
			handleFragment(&bundle, fragment, extractErr, "RKC-API-2001", openapi.PluginID)
		}
		if !opts.DisableJSONSchema && len(jsonFiles) > 0 {
			fragment, extractErr := jsonschema.Extract(jsonschema.Options{Root: root, Files: jsonFiles})
			handleFragment(&bundle, fragment, extractErr, "RKC-SCH-2001", jsonschema.PluginID)
		}
		if !opts.DisableManifests {
			fragment, extractErr := manifest.Extract(manifest.Options{Root: root, Files: files})
			handleFragment(&bundle, fragment, extractErr, "RKC-MAN-2001", manifest.PluginID)
		}
		if !opts.DisableEnvKeys {
			candidates := filterFiles(files, func(file pluginapi.FileRef) bool { return envkeys.IsCandidate(file.Path) })
			fragment, extractErr := envkeys.Extract(envkeys.Options{Root: root, Files: candidates})
			handleFragment(&bundle, fragment, extractErr, "RKC-CFG-2001", envkeys.PluginID)
			if extractErr == nil {
				markParsed(parsed, candidates)
			}
		}
	}
	if !opts.DisableSecretScan {
		fragment, extractErr := secretpack.Extract(secretpack.Options{Root: root, Files: files})
		handleFragment(&bundle, fragment, extractErr, "RKC-SEC-2001", secretpack.PluginID)
	}

	for i := range bundle.Artifacts {
		if _, ok := parsed[bundle.Artifacts[i].ID]; ok && bundle.Artifacts[i].Status == "text" {
			bundle.Artifacts[i].Status = "syntax_parsed"
		}
	}
	updateArtifactNodes(&bundle)
	dedupeBundle(&bundle)
	resolveHeuristicEdges(&bundle)
	dedupeBundle(&bundle)
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true})
	bundle.Diagnostics = append(bundle.Diagnostics, report.Diagnostics...)
	dedupeBundle(&bundle)
	rkcmodel.SortBundle(&bundle)
	coverage := rkcmodel.BuildCoverage(bundle)
	return bundle, coverage, nil
}

func artifactNode(artifact rkcmodel.Artifact) rkcmodel.Node {
	return rkcmodel.Node{ID: artifact.ID, LogicalID: firstNonEmpty(artifact.LogicalID, rkcmodel.StableID("logical", "artifact", artifact.Path)), Kind: artifact.Kind, Name: filepath.Base(artifact.Path), QualifiedName: artifact.Path, Language: artifact.Language, Visibility: "repository", ArtifactID: artifact.ID, Attributes: map[string]any{"status": artifact.Status, "size_bytes": artifact.SizeBytes, "sha256": artifact.SHA256, "media_type": artifact.MediaType, "disposition_reason": firstNonEmpty(artifact.DispositionReason, artifact.ExclusionReason)}}
}
func filterFiles(files []pluginapi.FileRef, predicate func(pluginapi.FileRef) bool) []pluginapi.FileRef {
	out := make([]pluginapi.FileRef, 0)
	for _, file := range files {
		if predicate(file) {
			out = append(out, file)
		}
	}
	return out
}
func markParsed(target map[string]struct{}, files []pluginapi.FileRef) {
	for _, file := range files {
		target[file.ArtifactID] = struct{}{}
	}
}
func handleFragment(bundle *rkcmodel.Bundle, fragment rkcmodel.Fragment, err error, code, pluginID string) {
	if err != nil {
		bundle.Diagnostics = append(bundle.Diagnostics, adapterError(code, pluginID, err))
		return
	}
	mergeFragment(bundle, fragment)
}
func adapterError(code, pluginID string, err error) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", code, pluginID, err.Error()), Severity: "error", Code: code, Message: err.Error(), Stage: "analysis", Plugin: pluginID}
}
func mergeFragment(bundle *rkcmodel.Bundle, fragment rkcmodel.Fragment) {
	bundle.Artifacts = append(bundle.Artifacts, fragment.Artifacts...)
	bundle.Nodes = append(bundle.Nodes, fragment.Nodes...)
	bundle.Edges = append(bundle.Edges, fragment.Edges...)
	bundle.Evidence = append(bundle.Evidence, fragment.Evidence...)
	bundle.Diagnostics = append(bundle.Diagnostics, fragment.Diagnostics...)
	bundle.Conflicts = append(bundle.Conflicts, fragment.Conflicts...)
	bundle.Documents = append(bundle.Documents, fragment.Documents...)
	bundle.Claims = append(bundle.Claims, fragment.Claims...)
	bundle.Paths = append(bundle.Paths, fragment.Paths...)
}

func updateArtifactNodes(bundle *rkcmodel.Bundle) {
	byID := map[string]rkcmodel.Artifact{}
	for _, a := range bundle.Artifacts {
		byID[a.ID] = a
	}
	for i := range bundle.Nodes {
		if a, ok := byID[bundle.Nodes[i].ID]; ok {
			if bundle.Nodes[i].Attributes == nil {
				bundle.Nodes[i].Attributes = map[string]any{}
			}
			bundle.Nodes[i].Attributes["status"] = a.Status
			bundle.Nodes[i].Attributes["size_bytes"] = a.SizeBytes
			bundle.Nodes[i].Attributes["sha256"] = a.SHA256
		}
	}
}

func dedupeBundle(bundle *rkcmodel.Bundle) {
	artifacts := map[string]rkcmodel.Artifact{}
	for _, item := range bundle.Artifacts {
		if current, ok := artifacts[item.ID]; !ok || artifactRank(item.Status) > artifactRank(current.Status) {
			artifacts[item.ID] = item
		}
	}
	bundle.Artifacts = bundle.Artifacts[:0]
	for _, item := range artifacts {
		bundle.Artifacts = append(bundle.Artifacts, item)
	}
	evidence := map[string]rkcmodel.Evidence{}
	for _, item := range bundle.Evidence {
		if _, ok := evidence[item.ID]; !ok {
			evidence[item.ID] = item
		}
	}
	bundle.Evidence = bundle.Evidence[:0]
	for _, item := range evidence {
		bundle.Evidence = append(bundle.Evidence, item)
	}
	nodes := map[string]rkcmodel.Node{}
	for _, item := range bundle.Nodes {
		if current, ok := nodes[item.ID]; ok {
			current.EvidenceIDs = uniqueSorted(append(current.EvidenceIDs, item.EvidenceIDs...))
			if current.LogicalID == "" {
				current.LogicalID = item.LogicalID
			}
			if current.Signature == "" {
				current.Signature = item.Signature
			}
			if current.Source == nil {
				current.Source = item.Source
			}
			if current.ArtifactID == "" {
				current.ArtifactID = item.ArtifactID
			}
			if current.Attributes == nil {
				current.Attributes = map[string]any{}
			}
			for k, v := range item.Attributes {
				if _, exists := current.Attributes[k]; !exists {
					current.Attributes[k] = v
				}
			}
			current.PublicSurface = current.PublicSurface || item.PublicSurface
			nodes[item.ID] = current
		} else {
			item.EvidenceIDs = uniqueSorted(item.EvidenceIDs)
			nodes[item.ID] = item
		}
	}
	bundle.Nodes = bundle.Nodes[:0]
	for _, item := range nodes {
		bundle.Nodes = append(bundle.Nodes, item)
	}
	edges := map[string]rkcmodel.Edge{}
	for _, item := range bundle.Edges {
		item.Resolution = rkcmodel.NormalizeResolution(item.Resolution)
		key := item.ID
		if key == "" {
			key = rkcmodel.StableID("edge", item.Kind, item.From, item.To)
		}
		item.ID = key
		if current, ok := edges[key]; ok {
			current.EvidenceIDs = uniqueSorted(append(current.EvidenceIDs, item.EvidenceIDs...))
			if resolutionRank(item.Resolution) > resolutionRank(current.Resolution) {
				current.Resolution = item.Resolution
				current.Confidence = item.Confidence
				current.Producer = item.Producer
			}
			if current.Attributes == nil {
				current.Attributes = map[string]any{}
			}
			for k, v := range item.Attributes {
				if _, exists := current.Attributes[k]; !exists {
					current.Attributes[k] = v
				}
			}
			edges[key] = current
		} else {
			item.EvidenceIDs = uniqueSorted(item.EvidenceIDs)
			edges[key] = item
		}
	}
	bundle.Edges = bundle.Edges[:0]
	for _, item := range edges {
		bundle.Edges = append(bundle.Edges, item)
	}
	diagnostics := map[string]rkcmodel.Diagnostic{}
	for _, item := range bundle.Diagnostics {
		if _, ok := diagnostics[item.ID]; !ok {
			diagnostics[item.ID] = item
		}
	}
	bundle.Diagnostics = bundle.Diagnostics[:0]
	for _, item := range diagnostics {
		bundle.Diagnostics = append(bundle.Diagnostics, item)
	}
	documents := map[string]rkcmodel.Document{}
	for _, item := range bundle.Documents {
		documents[item.ID] = item
	}
	bundle.Documents = bundle.Documents[:0]
	for _, item := range documents {
		bundle.Documents = append(bundle.Documents, item)
	}
	claims := map[string]rkcmodel.Claim{}
	for _, item := range bundle.Claims {
		claims[item.ID] = item
	}
	bundle.Claims = bundle.Claims[:0]
	for _, item := range claims {
		bundle.Claims = append(bundle.Claims, item)
	}
	conflicts := map[string]rkcmodel.Conflict{}
	for _, item := range bundle.Conflicts {
		conflicts[item.ID] = item
	}
	bundle.Conflicts = bundle.Conflicts[:0]
	for _, item := range conflicts {
		bundle.Conflicts = append(bundle.Conflicts, item)
	}
	paths := map[string]rkcmodel.ExecutionPath{}
	for _, item := range bundle.Paths {
		paths[item.ID] = item
	}
	bundle.Paths = bundle.Paths[:0]
	for _, item := range paths {
		bundle.Paths = append(bundle.Paths, item)
	}
	rkcmodel.SortBundle(bundle)
}
func artifactRank(status string) int {
	switch status {
	case "semantic_parsed":
		return 5
	case "syntax_parsed", "parsed":
		return 4
	case "text":
		return 3
	case "recorded", "included":
		return 2
	default:
		return 1
	}
}
func resolutionRank(value string) int {
	switch rkcmodel.NormalizeResolution(value) {
	case rkcmodel.ResolutionCompilerResolved:
		return 7
	case rkcmodel.ResolutionRuntimeObserved:
		return 6
	case rkcmodel.ResolutionDeclared:
		return 5
	case rkcmodel.ResolutionSyntaxInferred:
		return 4
	case rkcmodel.ResolutionDocumentationAsserted:
		return 3
	case rkcmodel.ResolutionModelInferred:
		return 2
	default:
		return 1
	}
}

func resolveHeuristicEdges(bundle *rkcmodel.Bundle) {
	byID := map[string]rkcmodel.Node{}
	byName := map[string][]rkcmodel.Node{}
	byQualified := map[string][]rkcmodel.Node{}
	for _, node := range bundle.Nodes {
		byID[node.ID] = node
		if rkcmodel.IsSymbolKind(node.Kind) && node.Kind != "unresolved_symbol" {
			if node.Name != "" {
				byName[node.Name] = append(byName[node.Name], node)
			}
			if node.QualifiedName != "" {
				byQualified[node.QualifiedName] = append(byQualified[node.QualifiedName], node)
			}
		}
	}
	for i := range bundle.Edges {
		edge := &bundle.Edges[i]
		if rkcmodel.NormalizeResolution(edge.Resolution) != rkcmodel.ResolutionUnresolved {
			continue
		}
		target, ok := byID[edge.To]
		if !ok || target.Kind != "unresolved_symbol" {
			continue
		}
		spelling := target.Name
		if value, ok := edge.Attributes["spelling"].(string); ok && strings.TrimSpace(value) != "" {
			spelling = value
		}
		candidates := byQualified[spelling]
		if len(candidates) != 1 {
			name := spelling
			if index := strings.LastIndexAny(spelling, "./:#"); index >= 0 {
				name = spelling[index+1:]
			}
			candidates = byName[name]
		}
		if len(candidates) == 1 && candidates[0].ID != edge.From {
			edge.To = candidates[0].ID
			edge.Resolution = rkcmodel.ResolutionSyntaxInferred
			edge.Confidence = maxFloat(edge.Confidence, 0.65)
			if edge.Attributes == nil {
				edge.Attributes = map[string]any{}
			}
			edge.Attributes["resolver"] = "unique_name_match"
			edge.ID = rkcmodel.StableID("edge", edge.Kind, edge.From, edge.To)
		}
	}
	referenced := map[string]struct{}{}
	for _, edge := range bundle.Edges {
		referenced[edge.From] = struct{}{}
		referenced[edge.To] = struct{}{}
	}
	filtered := bundle.Nodes[:0]
	for _, node := range bundle.Nodes {
		if node.Kind == "unresolved_symbol" {
			if _, ok := referenced[node.ID]; !ok {
				continue
			}
		}
		filtered = append(filtered, node)
	}
	bundle.Nodes = filtered
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func uniqueSorted(values []string) []string {
	set := map[string]struct{}{}
	for _, v := range values {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func inspectGit(ctx context.Context, root string) rkcmodel.GitInfo {
	info := rkcmodel.GitInfo{}
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		info.Unavailable = true
		return info
	}
	info.Commit = commit
	info.Branch, _ = gitOutput(ctx, root, "branch", "--show-current")
	info.Origin, _ = gitOutput(ctx, root, "remote", "get-url", "origin")
	status, _ := gitOutput(ctx, root, "status", "--porcelain")
	info.Dirty = status != ""
	return info
}
func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmdArgs := append([]string{"-c", "core.hooksPath=/dev/null", "-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	return strings.TrimSpace(string(output)), err
}
