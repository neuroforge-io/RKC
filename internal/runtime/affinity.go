package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/gitworktree"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/sourceorigin"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	captureMaximumFileBytes       = int64(1 << 30)
	captureMaximumTextBytes       = int64(2 << 20)
	captureMaximumRepositoryBytes = int64(20 << 30)
	captureMaximumFiles           = 500000
)

var affinityIgnoredPaths = map[string]struct{}{
	// These self-generated singleton files can appear only after capture. Their
	// bytes cannot participate in their own content identity.
	".rkc-history.json": {},
	".rkc-trace.json":   {},
}

// repositoryAffinity inventories the bounded repository content and obtains
// credential-free Git identity. The content digest covers every inventory
// record except the two self-referential generated singleton outputs above.
func repositoryAffinity(ctx context.Context, root string) (TraceRepository, []rkcmodel.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return TraceRepository{}, nil, err
	}
	result, err := inventory.Scan(inventory.Options{
		Root: root, MaxFileBytes: captureMaximumFileBytes,
		MaxTextBytes: captureMaximumTextBytes, MaxRepositoryBytes: captureMaximumRepositoryBytes,
		MaxFiles: captureMaximumFiles, Excludes: inventory.DefaultExclusions(),
	})
	if err != nil {
		return TraceRepository{}, nil, fmt.Errorf("inventory trace repository: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return TraceRepository{}, nil, err
	}
	git, err := traceGitAffinity(ctx, root)
	if err != nil {
		return TraceRepository{}, nil, err
	}
	origin := git.origin
	identity := origin
	if identity == "" {
		identity = filepath.Base(filepath.Clean(root))
	}
	if identity == "" || identity == "." || identity == string(filepath.Separator) {
		return TraceRepository{}, nil, errors.New("derive trace repository identity")
	}
	digest, count := contentAffinity(result.Artifacts)
	return TraceRepository{
		RepositoryID:   rkcmodel.StableID("repository", identity),
		ContentDigest:  digest,
		ArtifactCount:  count,
		GitCommit:      git.commit,
		GitUnavailable: git.unavailable,
	}, result.Artifacts, nil
}

func contentAffinity(artifacts []rkcmodel.Artifact) (string, int) {
	ordered := append([]rkcmodel.Artifact(nil), artifacts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	count := 0
	for _, artifact := range ordered {
		if _, ignored := affinityIgnoredPaths[artifact.Path]; ignored {
			continue
		}
		// Analyzer disposition changes after inventory (for example text ->
		// syntax_parsed), so affinity deliberately covers physical identity only.
		fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%s\x00%s\n",
			artifact.Path, artifact.Kind, artifact.SizeBytes, artifact.SHA256, artifact.Target)
		count++
	}
	return hex.EncodeToString(hash.Sum(nil)), count
}

func bindTraceArtifacts(root string, observed []TraceArtifact, inventoryArtifacts []rkcmodel.Artifact) ([]TraceArtifact, error) {
	candidates := make(map[string]rkcmodel.Artifact, len(inventoryArtifacts))
	for _, artifact := range inventoryArtifacts {
		if artifact.SHA256 == "" || artifact.Status == "excluded" {
			continue
		}
		candidates[artifact.Path] = artifact
	}
	modules, err := goModuleBindings(root, inventoryArtifacts)
	if err != nil {
		return nil, err
	}
	bound := make(map[string]TraceArtifact, len(observed))
	for _, item := range observed {
		artifact, err := resolveCoverageArtifact(canonicalCoveragePath(item.Path), candidates, modules)
		if err != nil {
			return nil, err
		}
		item.Path = artifact.Path
		item.SourceSHA256 = artifact.SHA256
		item.SourceSizeBytes = artifact.SizeBytes
		current, duplicate := bound[item.Path]
		if duplicate {
			current.Statements += item.Statements
			current.ExecutedStatements += item.ExecutedStatements
			current.ExecutedRanges = append(current.ExecutedRanges, item.ExecutedRanges...)
			bound[item.Path] = current
			continue
		}
		bound[item.Path] = item
	}
	pathsOrdered := make([]string, 0, len(bound))
	for path := range bound {
		pathsOrdered = append(pathsOrdered, path)
	}
	sort.Strings(pathsOrdered)
	result := make([]TraceArtifact, 0, len(pathsOrdered))
	for _, path := range pathsOrdered {
		result = append(result, bound[path])
	}
	return result, nil
}

type goModuleBinding struct {
	module    string
	directory string
}

func goModuleBindings(root string, artifacts []rkcmodel.Artifact) ([]goModuleBinding, error) {
	var bindings []goModuleBinding
	seen := map[string]string{}
	for _, artifact := range artifacts {
		if filepath.Base(artifact.Path) != "go.mod" || artifact.SHA256 == "" || artifact.SizeBytes > captureMaximumTextBytes {
			continue
		}
		data, err := sourcepath.ReadFile(root, artifact.Path)
		if err != nil {
			return nil, errors.New("read inventoried Go module identity")
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != artifact.SizeBytes || hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return nil, errors.New("Go module identity changed after inventory")
		}
		module, err := parseGoModuleIdentity(data)
		if err != nil {
			return nil, err
		}
		if module == "" {
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(artifact.Path))
		if directory == "." {
			directory = ""
		}
		if previous, duplicate := seen[module]; duplicate && previous != directory {
			return nil, errors.New("duplicate Go module identities make coverage paths ambiguous")
		}
		seen[module] = directory
		bindings = append(bindings, goModuleBinding{module: module, directory: directory})
	}
	sort.Slice(bindings, func(i, j int) bool {
		if len(bindings[i].module) == len(bindings[j].module) {
			return bindings[i].module < bindings[j].module
		}
		return len(bindings[i].module) > len(bindings[j].module)
	})
	return bindings, nil
}

func parseGoModuleIdentity(data []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), int(captureMaximumTextBytes))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "module") || (len(line) > len("module") && line[len("module")] != ' ' && line[len("module")] != '\t') {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "`") {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return "", errors.New("Go module identity is malformed")
			}
			value = unquoted
		}
		if value == "" || len(value) > 1024 || containsControl(value) || strings.ContainsAny(value, " \\#") || strings.HasPrefix(value, "/") {
			return "", errors.New("Go module identity is invalid")
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("read Go module identity")
	}
	return "", nil
}

func resolveCoverageArtifact(observed string, candidates map[string]rkcmodel.Artifact, modules []goModuleBinding) (rkcmodel.Artifact, error) {
	var moduleMatches []rkcmodel.Artifact
	for _, binding := range modules {
		prefix := binding.module + "/"
		if !strings.HasPrefix(observed, prefix) {
			continue
		}
		path := strings.TrimPrefix(observed, prefix)
		if binding.directory != "" {
			path = binding.directory + "/" + path
		}
		if artifact, ok := candidates[path]; ok {
			moduleMatches = append(moduleMatches, artifact)
		}
	}
	if len(moduleMatches) == 1 {
		return moduleMatches[0], nil
	}
	if len(moduleMatches) > 1 {
		return rkcmodel.Artifact{}, errors.New("captured coverage path has multiple Go module bindings")
	}
	if artifact, exact := candidates[observed]; exact {
		return artifact, nil
	}
	return rkcmodel.Artifact{}, errors.New("captured coverage path has no exact repository or Go module binding")
}

func canonicalCoveragePath(value string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(value)), "./")
}

type gitAffinity struct {
	commit      string
	origin      string
	unavailable bool
}

func traceGitAffinity(ctx context.Context, root string) (gitAffinity, error) {
	if !gitworktree.AffinityEnvironmentIsPaired() {
		return gitAffinity{unavailable: true}, nil
	}
	topLevelOutput, err := traceGitOutputRaw(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return gitAffinity{}, ctx.Err()
		}
		return gitAffinity{unavailable: true}, nil
	}
	topLevel, topLevelOK := gitworktree.ParseTopLevelOutput(topLevelOutput)
	if !topLevelOK || !gitworktree.IsExactRoot(root, topLevel) {
		return gitAffinity{unavailable: true}, nil
	}
	commit, err := traceGitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return gitAffinity{}, ctx.Err()
		}
		return gitAffinity{unavailable: true}, nil
	}
	rawOrigin, err := traceGitOutput(ctx, root, "remote", "get-url", "origin")
	if err != nil && ctx.Err() != nil {
		return gitAffinity{}, ctx.Err()
	}
	origin := ""
	if rawOrigin != "" {
		canonical, normalizeErr := sourceorigin.Normalize(rawOrigin)
		if normalizeErr != nil {
			return gitAffinity{}, errors.New("configured Git origin is not safely canonicalizable")
		}
		if !strings.HasPrefix(canonical, "file://") {
			origin = canonical
		}
	}
	return gitAffinity{commit: commit, origin: origin}, nil
}

func traceGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	output, err := traceGitOutputRaw(ctx, root, args...)
	return strings.TrimSpace(string(output)), err
}

func traceGitOutputRaw(ctx context.Context, root string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-c", "core.hooksPath=/dev/null", "-C", root}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	)
	var output boundedBuffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, err
	}
	if output.truncated {
		return nil, errors.New("Git metadata exceeded the runtime evidence bound")
	}
	return output.bytes(), nil
}

func sameRepositoryAffinity(left, right TraceRepository) bool {
	return left.RepositoryID == right.RepositoryID &&
		left.ContentDigest == right.ContentDigest &&
		left.ArtifactCount == right.ArtifactCount &&
		left.GitCommit == right.GitCommit &&
		left.GitUnavailable == right.GitUnavailable
}
