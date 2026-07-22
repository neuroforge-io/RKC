package inventory

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

type Options struct {
	Root               string
	MaxFileBytes       int64
	MaxTextBytes       int64
	MaxRepositoryBytes int64
	MaxFiles           int
	Excludes           []string
}

type Result struct {
	Artifacts   []model.Artifact
	Diagnostics []model.Diagnostic
	Digest      string
}

func Scan(opts Options) (Result, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return Result{}, fmt.Errorf("resolve root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("stat repository root: %w", err)
	}
	if !rootInfo.IsDir() {
		return Result{}, fmt.Errorf("repository root is not a directory: %s", root)
	}
	marked, err := hasReservedMarker(root)
	if err != nil {
		return Result{}, fmt.Errorf("inspect reserved RKC marker at repository root: %w", err)
	}
	if marked {
		return Result{}, errors.New("refusing to inventory an RKC-generated output tree")
	}
	if opts.MaxTextBytes <= 0 {
		opts.MaxTextBytes = 2 * 1024 * 1024
	}
	excludes := make(map[string]struct{}, len(opts.Excludes))
	for _, value := range opts.Excludes {
		value = filepath.ToSlash(strings.TrimSpace(value))
		value = strings.TrimPrefix(value, "./")
		value = strings.TrimSuffix(value, "/")
		if value != "" {
			excludes[value] = struct{}{}
		}
	}

	var artifacts []model.Artifact
	var diagnostics []model.Diagnostic
	var encountered int
	var encounteredBytes int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		encountered++
		if opts.MaxFiles > 0 && encountered > opts.MaxFiles {
			return fmt.Errorf("repository path limit exceeded: %d > %d", encountered, opts.MaxFiles)
		}
		if walkErr != nil {
			diagnostics = append(diagnostics, diagnostic("error", "RKC1001", walkErr.Error(), rel, "inventory"))
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Only caller-supplied exclusions are authoritative. Repository markers
		// are hostile input and cannot silently suppress an otherwise inventoried
		// subtree. Known output paths are explicitly excluded by the caller.
		if reason, excluded := exclusionReason(rel, excludes); excluded {
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			}
			artifacts = append(artifacts, model.Artifact{
				ID:                model.StableID("artifact", rel),
				Path:              rel,
				Kind:              kind,
				Status:            "excluded",
				DispositionReason: reason,
				ExclusionReason:   reason,
			})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() == safeoutput.MarkerName {
			return fmt.Errorf("reserved RKC marker appeared in untrusted repository subtree %q; explicitly exclude a known generated output", rel)
		}
		if entry.IsDir() {
			marked, markerErr := hasReservedMarker(path)
			if markerErr != nil {
				return fmt.Errorf("inspect reserved RKC marker in untrusted repository subtree %q: %w", rel, markerErr)
			}
			if marked {
				return fmt.Errorf("reserved RKC marker appeared in untrusted repository subtree %q; explicitly exclude a known generated output", rel)
			}
		}

		info, err := entry.Info()
		if err != nil {
			diagnostics = append(diagnostics, diagnostic("error", "RKC1002", err.Error(), rel, "inventory"))
			return nil
		}
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				diagnostics = append(diagnostics, diagnostic("warning", "RKC1003", readErr.Error(), rel, "inventory"))
			}
			artifacts = append(artifacts, model.Artifact{
				ID:     model.StableID("artifact", rel),
				Path:   rel,
				Kind:   "symlink",
				Status: "recorded",
				Target: target,
			})
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !mode.IsRegular() {
			artifacts = append(artifacts, model.Artifact{
				ID:                model.StableID("artifact", rel),
				Path:              rel,
				Kind:              "special",
				Status:            "excluded",
				DispositionReason: "non_regular_file",
				ExclusionReason:   "non_regular_file",
			})
			return nil
		}

		encounteredBytes += info.Size()
		if opts.MaxRepositoryBytes > 0 && encounteredBytes > opts.MaxRepositoryBytes {
			return fmt.Errorf("repository byte limit exceeded: %d > %d", encounteredBytes, opts.MaxRepositoryBytes)
		}
		artifact, diag := inspectFile(path, rel, info.Size(), opts.MaxFileBytes, opts.MaxTextBytes)
		artifacts = append(artifacts, artifact)
		if diag != nil {
			diagnostics = append(diagnostics, *diag)
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("walk repository: %w", err)
	}

	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	sort.Slice(diagnostics, func(i, j int) bool { return diagnostics[i].ID < diagnostics[j].ID })
	var digestInput strings.Builder
	for _, artifact := range artifacts {
		fmt.Fprintf(&digestInput, "%s\x00%s\x00%s\x00%s\n", artifact.Path, artifact.SHA256, artifact.Status, artifact.ExclusionReason)
	}
	sum := sha256.Sum256([]byte(digestInput.String()))
	return Result{Artifacts: artifacts, Diagnostics: diagnostics, Digest: hex.EncodeToString(sum[:])}, nil
}

func hasReservedMarker(root string) (bool, error) {
	_, err := os.Lstat(filepath.Join(root, safeoutput.MarkerName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func inspectFile(absPath, relPath string, size, maxFileBytes, maxTextBytes int64) (model.Artifact, *model.Diagnostic) {
	artifact := model.Artifact{
		ID:        model.StableID("artifact", relPath),
		Path:      relPath,
		Kind:      "file",
		Language:  DetectLanguage(relPath),
		MediaType: detectMediaType(relPath),
		SizeBytes: size,
		Status:    "recorded",
	}

	if maxFileBytes > 0 && size > maxFileBytes {
		artifact.Status = "oversized"
		artifact.DispositionReason = fmt.Sprintf("file_exceeds_limit:%d", maxFileBytes)
		artifact.ExclusionReason = artifact.DispositionReason
		return artifact, nil
	}

	file, err := os.Open(absPath)
	if err != nil {
		artifact.Status = "unreadable"
		d := diagnostic("error", "RKC1004", err.Error(), relPath, "inventory")
		return artifact, &d
	}
	defer file.Close()

	hash := sha256.New()
	preview := make([]byte, 8192)
	n, previewErr := io.ReadFull(file, preview)
	if previewErr != nil && previewErr != io.EOF && previewErr != io.ErrUnexpectedEOF {
		artifact.Status = "unreadable"
		d := diagnostic("error", "RKC1005", previewErr.Error(), relPath, "inventory")
		return artifact, &d
	}
	preview = preview[:n]
	if _, err := hash.Write(preview); err != nil {
		artifact.Status = "unreadable"
		d := diagnostic("error", "RKC1006", err.Error(), relPath, "inventory")
		return artifact, &d
	}
	if _, err := io.Copy(hash, file); err != nil {
		artifact.Status = "unreadable"
		d := diagnostic("error", "RKC1007", err.Error(), relPath, "inventory")
		return artifact, &d
	}
	artifact.SHA256 = hex.EncodeToString(hash.Sum(nil))

	isText := likelyText(preview)
	artifact.Text = isText
	if !isText {
		artifact.Status = "binary"
		return artifact, nil
	}
	if size > maxTextBytes {
		artifact.Status = "oversized"
		artifact.DispositionReason = fmt.Sprintf("text_file_exceeds_limit:%d", maxTextBytes)
		artifact.ExclusionReason = artifact.DispositionReason
		return artifact, nil
	}

	full, err := os.ReadFile(absPath)
	if err != nil {
		artifact.Status = "unreadable"
		d := diagnostic("error", "RKC1008", err.Error(), relPath, "inventory")
		return artifact, &d
	}
	if !utf8.Valid(full) {
		artifact.Text = false
		artifact.Status = "binary"
		return artifact, nil
	}
	artifact.LineCount = countLines(full)
	artifact.Status = "text"
	return artifact, nil
}

func likelyText(preview []byte) bool {
	if len(preview) == 0 {
		return true
	}
	if bytes.IndexByte(preview, 0) >= 0 {
		return false
	}
	if utf8.Valid(preview) {
		return true
	}
	control := 0
	for _, b := range preview {
		if b < 9 || (b > 13 && b < 32) {
			control++
		}
	}
	return float64(control)/float64(len(preview)) < 0.03
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 16*1024*1024)
	for scanner.Scan() {
		count++
	}
	return count
}

func exclusionReason(rel string, excludes map[string]struct{}) (string, bool) {
	for pattern := range excludes {
		if rel == pattern || strings.HasPrefix(rel, pattern+"/") {
			return "policy_exclude:" + pattern, true
		}
	}
	return "", false
}

func detectMediaType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if value := mime.TypeByExtension(ext); value != "" {
		return value
	}
	return "application/octet-stream"
}

func DetectLanguage(path string) string {
	name := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	if language, ok := specialNames[name]; ok {
		return language
	}
	if language, ok := extensions[ext]; ok {
		return language
	}
	return ""
}

var specialNames = map[string]string{
	"dockerfile":       "dockerfile",
	"makefile":         "make",
	"cmakelists.txt":   "cmake",
	"jenkinsfile":      "groovy",
	"justfile":         "make",
	"vagrantfile":      "ruby",
	"requirements.txt": "requirements",
}

var extensions = map[string]string{
	".py": "python", ".pyi": "python", ".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".go": "go", ".rs": "rust", ".java": "java", ".kt": "kotlin", ".kts": "kotlin",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "csharp", ".rb": "ruby", ".php": "php", ".swift": "swift", ".scala": "scala", ".sh": "shell", ".bash": "shell",
	".ps1": "powershell", ".sql": "sql", ".graphql": "graphql", ".gql": "graphql", ".proto": "protobuf", ".tf": "hcl",
	".yaml": "yaml", ".yml": "yaml", ".json": "json", ".jsonl": "jsonl", ".toml": "toml", ".xml": "xml", ".html": "html",
	".css": "css", ".scss": "scss", ".md": "markdown", ".mdx": "mdx", ".rst": "rst", ".txt": "text", ".ipynb": "jupyter",
}

func diagnostic(severity, code, message, path, stage string) model.Diagnostic {
	return model.Diagnostic{
		ID:       model.StableID("diagnostic", severity, code, path, message),
		Severity: severity,
		Code:     code,
		Message:  message,
		Source:   &model.SourceRange{Path: path},
		Stage:    stage,
	}
}
