package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
)

// scipLanguageDescriptor describes one supported compiler-indexer route. RKC
// does not bundle these tools; the operator pins and provides them. The
// default argument vector is the canonical index command; --tool-args replaces
// it exactly when a project needs different flags.
type scipLanguageDescriptor struct {
	name                      string
	aliases                   []string
	tool                      string
	defaultArgs               []string
	generatedPositionEncoding int32
	generatedEncodingVersion  string
	note                      string
}

var scipLanguageSpecs = []scipLanguageDescriptor{
	{name: "go", tool: "scip-go", defaultArgs: []string{"index", "./..."}, note: "compiler-grade Go indexing through go/packages"},
	// scip-python 0.6.6 is built on Pyright's TypeScript implementation and
	// emits UTF-16 code-unit offsets through an older SCIP schema that omits
	// Document.position_encoding. Only digest-pinned, same-process generation
	// may fill that producer invariant; inert external indexes remain strict.
	{name: "python", tool: "scip-python", defaultArgs: []string{"index", "."}, generatedPositionEncoding: 2, generatedEncodingVersion: "0.6.6", note: "pyright-backed Python indexing"},
	{name: "typescript", aliases: []string{"ts"}, tool: "scip-typescript", defaultArgs: []string{"index"}, note: "TypeScript and JavaScript indexing"},
	{name: "javascript", aliases: []string{"js"}, tool: "scip-typescript", defaultArgs: []string{"index"}, note: "TypeScript and JavaScript indexing"},
	{name: "rust", tool: "rust-analyzer", defaultArgs: []string{"scip", "."}, note: "Rust indexing through rust-analyzer"},
	{name: "c", aliases: []string{"c++", "cc", "cpp", "cuda"}, tool: "scip-clang", defaultArgs: []string{"--compdb-path=build/compile_commands.json"}, note: "C/C++/CUDA indexing; requires a compile database"},
	{name: "java", aliases: []string{"kotlin", "scala"}, tool: "scip-java", defaultArgs: []string{}, note: "JVM indexing; follow the scip-java build workflow"},
	{name: "csharp", aliases: []string{"vb", "vb.net"}, tool: "scip-dotnet", defaultArgs: []string{}, note: ".NET indexing; follow the scip-dotnet solution workflow"},
	{name: "ruby", tool: "scip-ruby", defaultArgs: []string{}, note: "Ruby indexing; follow the scip-ruby project workflow"},
}

func scipLanguageSpec(language string) (scipLanguageDescriptor, bool) {
	normalized := strings.ToLower(strings.TrimSpace(language))
	for _, spec := range scipLanguageSpecs {
		if spec.name == normalized {
			return spec, true
		}
		for _, alias := range spec.aliases {
			if alias == normalized {
				return spec, true
			}
		}
	}
	return scipLanguageDescriptor{}, false
}

// scipIndexerEntry pins one indexer binary to its exact digest. Generation
// refuses to run an unpinned or mismatched tool once an entry exists; a
// missing entry produces an explicit warning instead of silently trusting an
// unverified binary.
type scipIndexerEntry struct {
	Language string `json:"language"`
	Tool     string `json:"tool"`
	Version  string `json:"version,omitempty"`
	SHA256   string `json:"sha256"`
}

type scipIndexerLock struct {
	SchemaVersion string             `json:"schema_version"`
	Indexers      []scipIndexerEntry `json:"indexers"`
}

const scipIndexerLockSchemaVersion = "1.0"

func defaultScipIndexerLockPath() string {
	if directory, err := os.UserConfigDir(); err == nil && strings.TrimSpace(directory) != "" {
		return filepath.Join(directory, "rkc", "indexers.lock.json")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".rkc", "indexers.lock.json")
	}
	return ""
}

func runScip(args []string) error {
	if len(args) == 0 {
		return scipUsage()
	}
	switch args[0] {
	case "generate":
		return runScipGenerateWithAdmission(args[1:])
	case "verify":
		return runScipVerify(args[1:])
	case "pin":
		return runScipPin(args[1:])
	case "languages", "list":
		return runScipLanguages(args[1:])
	case "help", "--help", "-h":
		return scipUsage()
	default:
		return fmt.Errorf("unknown scip subcommand %q; run 'rkc scip help'", args[0])
	}
}

func scipUsage() error {
	_, err := fmt.Fprint(os.Stdout, `Compiler-grade semantics for RKC atlases.

  rkc scip generate --language <language> [options] <repository>
  rkc scip verify --index <path>
  rkc scip pin --language <language> --tool <path> [--version <version>]
  rkc scip languages

Generate executes an operator-provided, digest-pinned compiler indexer and
publishes a validated index.scip that scan/quickstart/open import with
--scip-index. RKC never downloads indexers. Generation runs inside the same
low-priority resource envelope as scans.

Languages: `+scipLanguageNames()+`

Examples:
  rkc scip pin --language go --tool "$(which scip-go)" --version v0.2.7
  rkc scip generate --language go .
  rkc scan --scip-generate go --no-python --out .rkc --state-dir .rkc-state .
`)
	return err
}

func scipLanguageNames() string {
	names := make([]string, 0, len(scipLanguageSpecs))
	for _, spec := range scipLanguageSpecs {
		names = append(names, spec.name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// runScipGenerateWithAdmission places compiler-index generation inside the
// same fail-closed low-priority envelope and priority-workload policy used by
// scans. Help requests resolve locally before admission.
func runScipGenerateWithAdmission(args []string) error {
	if scipGenerateHelpRequest(args) {
		return runScipGenerate(context.Background(), args)
	}
	ctx, stop := signalContext()
	defer stop()
	return runDirectCommandWithAdmission(ctx, "scip", append([]string{"generate"}, args...), func(ctx context.Context, admissionArgs []string) error {
		// The guarded local path receives the subcommand prefix; strip it
		// before the generate parser sees the repository path.
		if len(admissionArgs) == 0 || admissionArgs[0] != "generate" {
			return errors.New("scip generation admission lost its subcommand")
		}
		return runScipGenerate(ctx, admissionArgs[1:])
	})
}

func scipGenerateHelpRequest(args []string) bool {
	fs := flag.NewFlagSet("scip generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("language", "", "")
	fs.String("tool", "", "")
	fs.String("lock", "", "")
	fs.Bool("no-pin-check", false, "")
	fs.String("out", "", "")
	fs.String("output", "", "")
	fs.Var(&stringList{}, "tool-args", "")
	fs.Duration("timeout", 0, "")
	fs.Bool("json", false, "")
	return errors.Is(fs.Parse(args), flag.ErrHelp)
}

func runScipGenerate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scip generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	language := fs.String("language", "", "repository language to index (see 'rkc scip languages')")
	tool := fs.String("tool", "", "absolute indexer binary path; default looks up the canonical tool name on PATH")
	lock := fs.String("lock", defaultScipIndexerLockPath(), "operator-owned absolute indexer pin lock path (JSON)")
	noPinCheck := fs.Bool("no-pin-check", false, "run an unpinned or mismatched tool after a warning")
	out := fs.String("out", "", "publish the validated index into this directory; default <repository>/.rkc-scip")
	outputName := fs.String("output", "index.scip", "indexer output file name relative to the repository root")
	toolArgs := stringList{}
	fs.Var(&toolArgs, "tool-args", "replace the canonical indexer arguments with these exact arguments; repeatable")
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum indexer wall-clock time")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("scip generate requires exactly one repository path")
	}
	spec, ok := scipLanguageSpec(*language)
	if !ok {
		return fmt.Errorf("unsupported language %q; run 'rkc scip languages'", *language)
	}
	root, err := resolveScipRepositoryRoot(fs.Arg(0))
	if err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if err := validateSCIPOutputName(*outputName); err != nil {
		return err
	}
	lockPath, err := resolveSCIPLockPath(root, *lock)
	if err != nil {
		return err
	}
	entry, pinned, err := loadScipIndexerEntry(lockPath, spec.name)
	if err != nil {
		return err
	}
	executable, err := resolveScipTool(*tool, spec.tool)
	if err != nil {
		return err
	}
	if pinned {
		if *noPinCheck {
			fmt.Fprintf(os.Stderr, "rkc: warning: explicitly bypassing the pinned digest for %q because --no-pin-check was supplied\n", executable)
		}
	} else if !*noPinCheck {
		return fmt.Errorf("indexer %q is not pinned in %q; pin it or explicitly pass --no-pin-check", executable, lockPath)
	} else {
		fmt.Fprintf(os.Stderr, "rkc: warning: explicitly running unpinned indexer %q because --no-pin-check was supplied\n", executable)
	}
	expectedExecutableDigest := ""
	if pinned && !*noPinCheck {
		expectedExecutableDigest = entry.SHA256
	}
	privateExecutable, privateExecutableDigest, cleanupExecutable, err := stageScipExecutable(
		ctx, executable, expectedExecutableDigest,
	)
	if err != nil {
		if expectedExecutableDigest != "" {
			return fmt.Errorf(
				"indexer %q does not match the pinned executable: %w; re-pin with 'rkc scip pin' or pass --no-pin-check to run it as unverified",
				executable, err,
			)
		}
		return err
	}
	defer cleanupExecutable()
	arguments := append([]string(nil), spec.defaultArgs...)
	if len(toolArgs) > 0 {
		arguments = append([]string(nil), toolArgs...)
	}
	produced := filepath.Join(root, *outputName)
	if _, err := os.Lstat(produced); err == nil {
		return fmt.Errorf("indexer output %q already exists; RKC refuses to overwrite repository content", produced)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect indexer output %q: %w", produced, err)
	}
	// Capture the exact repository source identities before the compiler runs.
	// This process-local admission record, rather than an editable sidecar,
	// establishes that every published document existed unchanged across the
	// pinned indexer invocation.
	sourceSnapshot, err := scipindex.CaptureSourceSnapshot(ctx, root)
	if err != nil {
		return err
	}
	defer os.Remove(produced)
	if err := runScipIndexer(ctx, privateExecutable, arguments, root, *timeout); err != nil {
		return err
	}
	inputs, _, err := scipindex.PrepareInputs(ctx, []string{produced})
	if err != nil {
		return fmt.Errorf("prepared indexer output %q is invalid: %w", produced, err)
	}
	sealed, err := os.CreateTemp("", "rkc-scip-sealed-*.scip")
	if err != nil {
		return fmt.Errorf("create private sealed SCIP output: %w", err)
	}
	sealedPath := sealed.Name()
	defer os.Remove(sealedPath)
	defaultPositionEncoding := int32(0)
	if pinned && !*noPinCheck &&
		normalizeSCIPVersion(entry.Version) == spec.generatedEncodingVersion {
		defaultPositionEncoding = spec.generatedPositionEncoding
	}
	sealErr := scipindex.SealRepositorySourcesWithDefaultPositionEncoding(
		ctx, root, inputs[0], sourceSnapshot, defaultPositionEncoding, sealed,
	)
	closeErr := sealed.Close()
	if combined := errors.Join(sealErr, closeErr); combined != nil {
		return fmt.Errorf(
			"indexer output %q failed strict validation or source-sealed output close: %w",
			produced, combined,
		)
	}
	inputs, _, err = scipindex.PrepareInputs(ctx, []string{sealedPath})
	if err != nil {
		return fmt.Errorf("prepare source-sealed indexer output: %w", err)
	}
	inspection, err := scipindex.Inspect(ctx, inputs[0])
	if err != nil {
		return fmt.Errorf("source-sealed indexer output failed strict validation: %w", err)
	}
	if defaultPositionEncoding != 0 &&
		(inspection.Tool != spec.tool || normalizeSCIPVersion(inspection.ToolVersion) != spec.generatedEncodingVersion) {
		return fmt.Errorf(
			"generated position-encoding compatibility for %s requires producer %s %s; got %s %s",
			spec.name, spec.tool, spec.generatedEncodingVersion,
			inspection.Tool, inspection.ToolVersion,
		)
	}
	sourceBinding, err := scipindex.BuildSourceBinding(ctx, root, inputs[0])
	if err != nil {
		return fmt.Errorf("indexer output %q is not bound to the current repository sources: %w", produced, err)
	}
	publishDirectory := strings.TrimSpace(*out)
	if publishDirectory == "" {
		publishDirectory = filepath.Join(root, ".rkc-scip")
	}
	if err := os.MkdirAll(publishDirectory, 0o700); err != nil {
		return fmt.Errorf("create scip output directory: %w", err)
	}
	published := filepath.Join(publishDirectory, spec.name+".scip")
	if err := copyPreparedSCIPAtomic(inputs[0], published, 0o600); err != nil {
		return err
	}
	if err := appendScipManifest(publishDirectory, scipGeneratedIndex{
		Language: spec.name, Tool: inspection.Tool, ToolVersion: inspection.ToolVersion,
		ExecutableSHA256: privateExecutableDigest,
		Path:             filepath.Base(published), SHA256: inspection.SHA256, SizeBytes: inspection.SizeBytes,
		Documents: inspection.Documents, Symbols: inspection.Symbols, Occurrences: inspection.Occurrences,
		ExternalDocumentsSkipped: inspection.ExternalDocuments,
		SourceBinding:            &sourceBinding,
	}); err != nil {
		return err
	}
	publishedInputs, _, err := scipindex.PrepareInputs(ctx, []string{published})
	if err != nil {
		return fmt.Errorf("reopen published SCIP index for compiler authentication: %w", err)
	}
	if len(publishedInputs) != 1 {
		return errors.New("published SCIP index authentication lost its input")
	}
	if pinned && !*noPinCheck {
		if err := scipindex.MarkGeneratedByCurrentProcess(publishedInputs[0]); err != nil {
			return err
		}
	}
	if *jsonOutput {
		summary := map[string]any{
			"language": spec.name, "tool": inspection.Tool, "tool_version": inspection.ToolVersion,
			"index": published, "sha256": inspection.SHA256, "size_bytes": inspection.SizeBytes,
			"indexer_executable_sha256": privateExecutableDigest,
			"documents":                 inspection.Documents, "symbols": inspection.Symbols, "occurrences": inspection.Occurrences,
			"external_documents_skipped": inspection.ExternalDocuments,
		}
		return writeJSONStdout(summary)
	}
	fmt.Printf("SCIP index generated for %s\n", spec.name)
	fmt.Printf("Tool: %s %s\n", inspection.Tool, inspection.ToolVersion)
	fmt.Printf("Index: %s\n", published)
	fmt.Printf("Digest: %s (%d bytes)\n", inspection.SHA256, inspection.SizeBytes)
	fmt.Printf("Documents: %d; symbols: %d; occurrences: %d; external documents skipped: %d\n",
		inspection.Documents, inspection.Symbols, inspection.Occurrences, inspection.ExternalDocuments)
	fmt.Printf("Import: rkc scan --scip-index %s [options] %s\n", published, root)
	return nil
}

func normalizeSCIPVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func validateSCIPOutputName(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.IndexByte(value, 0) >= 0 || filepath.IsAbs(value) ||
		filepath.VolumeName(value) != "" || filepath.Clean(value) != value || filepath.Base(value) != value {
		return errors.New("--output must be one canonical repository-root file name")
	}
	return nil
}

func resolveSCIPLockPath(root, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("SCIP indexer trust requires an operator-owned lock path; configure --lock")
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("SCIP indexer lock path must be absolute and outside the analyzed repository")
	}
	cleaned := filepath.Clean(value)
	contained, err := pathWithinRoot(root, cleaned)
	if err != nil {
		return "", fmt.Errorf("validate SCIP indexer lock path: %w", err)
	}
	if contained {
		return "", errors.New("SCIP indexer lock must remain outside the analyzed repository")
	}
	return cleaned, nil
}

func pathWithinRoot(root, path string) (bool, error) {
	canonicalRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return false, err
	}
	canonicalPath, err := canonicalProspectivePath(path)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func canonicalProspectivePath(path string) (string, error) {
	current := filepath.Clean(path)
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for position := len(missing) - 1; position >= 0; position-- {
				resolved = filepath.Join(resolved, missing[position])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type scipGeneratedIndex = scipindex.ManifestIndex

const scipManifestName = scipindex.ManifestName

const maximumSCIPMetadataFileBytes = 1 << 20

func appendScipManifest(directory string, index scipGeneratedIndex) error {
	manifestPath := filepath.Join(directory, scipManifestName)
	data, err := readBoundedRegularFile(manifestPath, maximumSCIPMetadataFileBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read scip manifest: %w", err)
	}
	manifest := scipindex.Manifest{SchemaVersion: scipindex.ManifestSchemaVersion}
	if len(data) > 0 {
		if err := decodeStrictJSON(data, &manifest); err != nil {
			return fmt.Errorf("decode scip manifest: %w", err)
		}
		if manifest.SchemaVersion != "1.0" && manifest.SchemaVersion != scipindex.ManifestSchemaVersion {
			return fmt.Errorf("decode scip manifest: unsupported schema_version %q", manifest.SchemaVersion)
		}
		manifest.SchemaVersion = scipindex.ManifestSchemaVersion
	}
	if len(manifest.Indexes) > len(scipLanguageSpecs) {
		return errors.New("decode scip manifest: index count exceeds the supported language bound")
	}
	replaced := false
	for position := range manifest.Indexes {
		if manifest.Indexes[position].Language == index.Language {
			manifest.Indexes[position] = index
			replaced = true
			break
		}
	}
	if !replaced {
		manifest.Indexes = append(manifest.Indexes, index)
	}
	sort.Slice(manifest.Indexes, func(i, j int) bool { return manifest.Indexes[i].Language < manifest.Indexes[j].Language })
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	encoded = append(encoded, '\n')
	return writeAtomic(manifestPath, encoded, 0o600)
}

func runScipIndexer(ctx context.Context, executable string, arguments []string, root string, timeout time.Duration) error {
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runContext, executable, arguments...)
	command.Dir = root
	command.Env = scipIndexerEnvironment()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if runContext.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("indexer %s timed out after %s", filepath.Base(executable), timeout)
		}
		return fmt.Errorf("indexer %s failed: %w", filepath.Base(executable), err)
	}
	return nil
}

// scipIndexerEnvironment keeps the invoking environment for compiler tooling
// (module caches, package managers, language servers) while fixing parallelism
// and disabling accelerators so the indexer stays inside the guarded envelope.
func scipIndexerEnvironment() []string {
	blocked := map[string]struct{}{
		"CUDA_VISIBLE_DEVICES": {}, "HIP_VISIBLE_DEVICES": {},
		"ROCR_VISIBLE_DEVICES": {}, "GGML_VK_VISIBLE_DEVICES": {},
	}
	result := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, disable := blocked[name]; disable {
			continue
		}
		result = append(result, entry)
	}
	for name, value := range map[string]string{
		"GOMAXPROCS": "1", "OMP_NUM_THREADS": "1", "OPENBLAS_NUM_THREADS": "1",
		"MKL_NUM_THREADS": "1", "NUMEXPR_NUM_THREADS": "1", "CMAKE_BUILD_PARALLEL_LEVEL": "1",
		"CARGO_BUILD_JOBS": "1", "CUDA_VISIBLE_DEVICES": "-1", "HIP_VISIBLE_DEVICES": "-1",
		"ROCR_VISIBLE_DEVICES": "-1", "GGML_VK_VISIBLE_DEVICES": "-1",
	} {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

func resolveScipTool(explicit, canonical string) (string, error) {
	if explicit != "" {
		absolute, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve indexer tool path: %w", err)
		}
		if err := requireRegularFile(absolute); err != nil {
			return "", fmt.Errorf("indexer tool %q is not a regular file: %w", absolute, err)
		}
		return absolute, nil
	}
	if path, err := exec.LookPath(canonical); err == nil {
		absolute, absErr := filepath.Abs(path)
		if absErr != nil {
			return "", fmt.Errorf("resolve indexer tool path: %w", absErr)
		}
		return absolute, nil
	}
	return "", fmt.Errorf(
		"indexer %q was not found on PATH; install it and pin it with 'rkc scip pin --language <language> --tool <path>', or pass --tool explicitly",
		canonical,
	)
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must be a real regular file, not a symlink")
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash indexer tool: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash indexer tool: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

const maximumSCIPIndexerExecutableBytes = int64(1 << 30)

// stageScipExecutable copies the selected tool through one already-opened file
// into an owner-only directory, hashes the exact bytes copied, and executes
// only that immutable private copy. This closes the pathname replacement
// window between pin verification and execution.
func stageScipExecutable(
	ctx context.Context,
	path string,
	expectedDigest string,
) (string, string, func(), error) {
	if ctx == nil {
		return "", "", func() {}, errors.New("stage SCIP indexer: context is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", "", func() {}, fmt.Errorf("inspect SCIP indexer: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", "", func() {}, errors.New("SCIP indexer must be a real regular file, not a symlink")
	}
	if before.Size() < 0 || before.Size() > maximumSCIPIndexerExecutableBytes {
		return "", "", func() {}, fmt.Errorf(
			"SCIP indexer is %d bytes; maximum is %d",
			before.Size(), maximumSCIPIndexerExecutableBytes,
		)
	}
	source, err := os.Open(path)
	if err != nil {
		return "", "", func() {}, fmt.Errorf("open SCIP indexer: %w", err)
	}
	opened, err := source.Stat()
	if err != nil || !sameOpenedFile(before, opened) {
		_ = source.Close()
		return "", "", func() {}, errors.New("SCIP indexer changed while opening")
	}
	directory, err := os.MkdirTemp("", "rkc-scip-indexer-")
	if err != nil {
		_ = source.Close()
		return "", "", func() {}, fmt.Errorf("create private SCIP indexer directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	destinationPath := filepath.Join(directory, filepath.Base(path))
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = source.Close()
		cleanup()
		return "", "", func() {}, fmt.Errorf("create private SCIP indexer copy: %w", err)
	}
	hasher := sha256.New()
	written, copyErr := copySCIPExecutable(ctx, io.MultiWriter(destination, hasher), source, before.Size())
	closeSourceErr := source.Close()
	syncErr := destination.Sync()
	chmodErr := destination.Chmod(0o500)
	closeDestinationErr := destination.Close()
	if err := errors.Join(copyErr, closeSourceErr, syncErr, chmodErr, closeDestinationErr); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("stage private SCIP indexer: %w", err)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if written != before.Size() {
		cleanup()
		return "", "", func() {}, errors.New("SCIP indexer changed while copying")
	}
	if expectedDigest != "" && !strings.EqualFold(digest, expectedDigest) {
		cleanup()
		return "", "", func() {}, fmt.Errorf("digest %s does not match pinned %s", digest, expectedDigest)
	}
	after, err := os.Lstat(path)
	if err != nil || !sameOpenedFile(before, after) {
		cleanup()
		return "", "", func() {}, errors.New("SCIP indexer changed while staging")
	}
	return destinationPath, digest, cleanup, nil
}

func copySCIPExecutable(ctx context.Context, destination io.Writer, source io.Reader, size int64) (int64, error) {
	buffer := make([]byte, 128*1024)
	limited := io.LimitReader(source, size+1)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := limited.Read(buffer)
		if count > 0 {
			outputCount, writeErr := destination.Write(buffer[:count])
			written += int64(outputCount)
			if writeErr != nil {
				return written, writeErr
			}
			if outputCount != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func sameOpenedFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) &&
		before.Mode() == after.Mode() && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime())
}

// loadScipIndexerEntry returns the pinned entry for a language (pinned=true)
// or a missing-entry path (pinned=false). A missing lock file is not fatal;
// the explicit --no-pin-check flag also allows a deliberately mismatched tool.
func loadScipIndexerEntry(lockPath, language string) (scipIndexerEntry, bool, error) {
	data, err := readBoundedRegularFile(lockPath, maximumSCIPMetadataFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return scipIndexerEntry{}, false, nil
	}
	if err != nil {
		return scipIndexerEntry{}, false, fmt.Errorf("read indexer lock %q: %w", lockPath, err)
	}
	var lock scipIndexerLock
	if err := decodeStrictJSON(data, &lock); err != nil {
		return scipIndexerEntry{}, false, fmt.Errorf("decode indexer lock %q: %w", lockPath, err)
	}
	if err := validateSCIPIndexerLock(lock); err != nil {
		return scipIndexerEntry{}, false, fmt.Errorf("indexer lock %q: %w", lockPath, err)
	}
	for _, entry := range lock.Indexers {
		if entry.Language == language && entry.Tool != "" {
			return entry, true, nil
		}
	}
	return scipIndexerEntry{}, false, nil
}

func validateSCIPIndexerLock(lock scipIndexerLock) error {
	if lock.SchemaVersion != scipIndexerLockSchemaVersion {
		return fmt.Errorf("unsupported schema_version %q", lock.SchemaVersion)
	}
	if len(lock.Indexers) > len(scipLanguageSpecs) {
		return errors.New("indexer count exceeds the supported language bound")
	}
	seen := map[string]struct{}{}
	for _, entry := range lock.Indexers {
		spec, supported := scipLanguageSpec(entry.Language)
		if !supported || spec.name != entry.Language {
			return fmt.Errorf("invalid canonical language %q", entry.Language)
		}
		if _, duplicate := seen[entry.Language]; duplicate {
			return fmt.Errorf("duplicate language %q", entry.Language)
		}
		seen[entry.Language] = struct{}{}
		if entry.Tool == "" || filepath.Base(entry.Tool) != entry.Tool || len(entry.Tool) > 128 || strings.ContainsAny(entry.Tool, "\x00\r\n") {
			return fmt.Errorf("invalid tool name for %q", entry.Language)
		}
		if len(entry.Version) > 128 || strings.ContainsAny(entry.Version, "\x00\r\n") {
			return fmt.Errorf("invalid tool version for %q", entry.Language)
		}
		if len(entry.SHA256) != sha256.Size*2 {
			return fmt.Errorf("invalid SHA-256 for %q", entry.Language)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("invalid SHA-256 for %q", entry.Language)
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func readBoundedRegularFile(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("path must be a real regular file, not a symlink")
	}
	if before.Size() > maximum {
		return nil, fmt.Errorf("file is %d bytes; maximum is %d", before.Size(), maximum)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("file changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds the %d-byte bound", maximum)
	}
	if afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || after.Size() != before.Size() {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

func runScipVerify(args []string) error {
	fs := flag.NewFlagSet("scip verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	index := fs.String("index", "", "SCIP index path to validate")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*index) == "" {
		return errors.New("scip verify requires --index <path>")
	}
	ctx, stop := signalContext()
	defer stop()
	inputs, _, err := scipindex.PrepareInputs(ctx, []string{*index})
	if err != nil {
		return fmt.Errorf("invalid SCIP index: %w", err)
	}
	inspection, err := scipindex.Inspect(ctx, inputs[0])
	if err != nil {
		return fmt.Errorf("SCIP index %q is not valid: %w", *index, err)
	}
	if *jsonOutput {
		return writeJSONStdout(inspection)
	}
	fmt.Printf("Valid SCIP index: %s\n", *index)
	fmt.Printf("Tool: %s %s\n", inspection.Tool, inspection.ToolVersion)
	fmt.Printf("Digest: %s (%d bytes)\n", inspection.SHA256, inspection.SizeBytes)
	fmt.Printf("Documents: %d; symbols: %d; occurrences: %d; external documents skipped: %d\n",
		inspection.Documents, inspection.Symbols, inspection.Occurrences, inspection.ExternalDocuments)
	return nil
}

func runScipPin(args []string) error {
	fs := flag.NewFlagSet("scip pin", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	language := fs.String("language", "", "repository language this tool indexes")
	tool := fs.String("tool", "", "absolute indexer binary path to pin")
	version := fs.String("version", "", "indexer version to record")
	lock := fs.String("lock", defaultScipIndexerLockPath(), "operator-owned absolute indexer pin lock path (JSON)")
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("scip pin does not accept positional arguments")
	}
	spec, ok := scipLanguageSpec(*language)
	if !ok {
		return fmt.Errorf("unsupported language %q; run 'rkc scip languages'", *language)
	}
	if strings.TrimSpace(*tool) == "" {
		return errors.New("scip pin requires --tool <path>")
	}
	if strings.TrimSpace(*lock) == "" || !filepath.IsAbs(*lock) {
		return errors.New("scip pin requires an absolute operator-owned --lock path")
	}
	*lock = filepath.Clean(*lock)
	absolute, err := filepath.Abs(*tool)
	if err != nil {
		return fmt.Errorf("resolve indexer tool path: %w", err)
	}
	if err := requireRegularFile(absolute); err != nil {
		return fmt.Errorf("indexer tool %q is not a regular file: %w", absolute, err)
	}
	digest, err := sha256File(absolute)
	if err != nil {
		return err
	}
	var lockData scipIndexerLock
	if data, err := readBoundedRegularFile(*lock, maximumSCIPMetadataFileBytes); err == nil {
		if err := decodeStrictJSON(data, &lockData); err != nil {
			return fmt.Errorf("decode indexer lock %q: %w", *lock, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read indexer lock %q: %w", *lock, err)
	}
	if lockData.SchemaVersion == "" {
		lockData.SchemaVersion = scipIndexerLockSchemaVersion
	}
	if lockData.SchemaVersion != scipIndexerLockSchemaVersion {
		return fmt.Errorf("indexer lock %q has unsupported schema_version %q", *lock, lockData.SchemaVersion)
	}
	if len(lockData.Indexers) > len(scipLanguageSpecs) {
		return fmt.Errorf("indexer lock %q exceeds the supported language bound", *lock)
	}
	entry := scipIndexerEntry{
		Language: spec.name, Tool: filepath.Base(absolute), Version: strings.TrimSpace(*version),
		SHA256: digest,
	}
	replaced := false
	for position := range lockData.Indexers {
		if lockData.Indexers[position].Language == spec.name {
			lockData.Indexers[position] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		lockData.Indexers = append(lockData.Indexers, entry)
	}
	sort.Slice(lockData.Indexers, func(i, j int) bool { return lockData.Indexers[i].Language < lockData.Indexers[j].Language })
	if err := validateSCIPIndexerLock(lockData); err != nil {
		return fmt.Errorf("indexer lock %q: %w", *lock, err)
	}
	encoded, err := json.MarshalIndent(lockData, "", "  ")
	if err != nil {
		return fmt.Errorf("encode indexer lock: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeAtomic(*lock, encoded, 0o600); err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(entry)
	}
	fmt.Printf("Pinned %s indexer: %s\n", spec.name, filepath.Base(absolute))
	fmt.Printf("Digest: %s\n", digest)
	fmt.Printf("Lock: %s\n", *lock)
	return nil
}

func runScipLanguages(args []string) error {
	fs := flag.NewFlagSet("scip languages", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("scip languages does not accept positional arguments")
	}
	rows := make([]map[string]any, 0, len(scipLanguageSpecs))
	for _, spec := range scipLanguageSpecs {
		rows = append(rows, map[string]any{
			"language": spec.name, "tool": spec.tool,
			"default_args": spec.defaultArgs, "note": spec.note,
		})
	}
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"languages": rows})
	}
	fmt.Printf("%-12s %-16s %s\n", "LANGUAGE", "TOOL", "DEFAULT COMMAND")
	for _, spec := range scipLanguageSpecs {
		command := spec.tool + " " + strings.Join(spec.defaultArgs, " ")
		fmt.Printf("%-12s %-16s %s\n", spec.name, spec.tool, command)
	}
	fmt.Println()
	fmt.Println("RKC never downloads these tools; generation executes only an explicitly selected, pinned indexer.")
	return nil
}

func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func copyPreparedSCIPAtomic(source scipindex.Input, destination string, mode os.FileMode) error {
	before, err := os.Lstat(source.Path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() != source.SizeBytes {
		return errors.New("scip index source no longer matches its prepared input")
	}
	input, err := os.Open(source.Path)
	if err != nil {
		return fmt.Errorf("open scip index source: %w", err)
	}
	opened, err := input.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = input.Close()
		return errors.New("scip index source changed while opening")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		_ = input.Close()
		return fmt.Errorf("create scip index destination: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-")
	if err != nil {
		_ = input.Close()
		return fmt.Errorf("create scip index staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = input.Close()
		_ = temporary.Close()
		return fmt.Errorf("protect scip index staging file: %w", err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(input, source.SizeBytes+1))
	closeErr := input.Close()
	syncErr := temporary.Sync()
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close scip index staging file: %w", err)
	}
	if copyErr != nil || closeErr != nil || syncErr != nil {
		return fmt.Errorf("stage scip index: %v", errors.Join(copyErr, closeErr, syncErr))
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if written != source.SizeBytes || actualDigest != source.SHA256 {
		return fmt.Errorf("scip index source changed while staging: got %d bytes and %s", written, actualDigest)
	}
	after, err := os.Lstat(source.Path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || after.Size() != before.Size() {
		return errors.New("scip index source changed while staging")
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish scip index: %w", err)
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("protect staging file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish staging file: %w", err)
	}
	return nil
}

// resolveScipRepositoryRoot validates the repository as a real, absolute,
// non-symlink directory. Generation runs the indexer there.
func resolveScipRepositoryRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("repository path is empty or invalid")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect repository: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("repository must be a real directory, not a symlink")
	}
	return absolute, nil
}

// generateSCIPIndexes is the scan/quickstart integration seam: it generates
// one index per requested language inside the atlas's derived sibling
// directory and returns the validated index paths for the normal
// digest-bound SCIP import. It runs inside the already-admitted scan process.
func generateSCIPIndexes(ctx context.Context, languages []string, root, outDirectory, toolOverride, lockPath string, noPinCheck bool) ([]string, error) {
	if len(languages) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(outDirectory) == "" {
		return nil, errors.New("scip generation requires an output atlas path")
	}
	derived := generatedSCIPDirectory(outDirectory)
	if err := os.MkdirAll(derived, 0o700); err != nil {
		return nil, fmt.Errorf("create scip generation directory: %w", err)
	}
	if strings.TrimSpace(lockPath) == "" {
		lockPath = defaultScipIndexerLockPath()
	}
	paths := make([]string, 0, len(languages))
	for _, language := range languages {
		args := []string{
			"--language", language, "--out", derived,
			"--lock", lockPath, "--output", "index.scip",
		}
		if toolOverride != "" {
			args = append(args, "--tool", toolOverride)
		}
		if noPinCheck {
			args = append(args, "--no-pin-check")
		}
		args = append(args, root)
		if err := runScipGenerate(ctx, args); err != nil {
			return nil, fmt.Errorf("generate SCIP index for %s: %w", language, err)
		}
		paths = append(paths, filepath.Join(derived, scipLanguageCanonical(language)+".scip"))
	}
	return paths, nil
}

func generatedSCIPDirectory(outDirectory string) string {
	return filepath.Join(filepath.Dir(outDirectory), filepath.Base(outDirectory)+".rkc-derived", "scip")
}

func scipLanguageCanonical(language string) string {
	if spec, ok := scipLanguageSpec(language); ok {
		return spec.name
	}
	return strings.ToLower(strings.TrimSpace(language))
}
