package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/neuroforge-io/RKC/internal/modelassets"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type synthesisManifest struct {
	SchemaVersion        string                         `json:"schema_version"`
	Status               string                         `json:"status"`
	SnapshotID           string                         `json:"snapshot_id"`
	Profile              string                         `json:"profile"`
	Task                 modelruntime.Task              `json:"task"`
	PacketOnly           bool                           `json:"packet_only"`
	Provider             string                         `json:"provider"`
	Model                modelruntime.ModelDescriptor   `json:"model,omitempty"`
	ModelBinding         *modelassets.GenerationBinding `json:"model_binding,omitempty"`
	ResponseSchemaSHA256 string                         `json:"response_schema_sha256,omitempty"`
	Options              modelruntime.InferenceOptions  `json:"options"`
	Budget               modelruntime.Budget            `json:"budget"`
	SubjectsRequested    int                            `json:"subjects_requested"`
	PacketsWritten       int                            `json:"packets_written"`
	ResponsesReceived    int                            `json:"responses_received"`
	AcceptedClaims       int                            `json:"accepted_claims"`
	RejectedClaims       int                            `json:"rejected_claims"`
	AcceptedSummaries    int                            `json:"accepted_summaries"`
	RejectedSummaries    int                            `json:"rejected_summaries"`
	FailedSubjects       int                            `json:"failed_subjects"`
	RedactedFindings     int                            `json:"redacted_secret_findings"`
	SourceBytesIncluded  int                            `json:"source_bytes_included"`
	StartedAt            time.Time                      `json:"started_at"`
	FinishedAt           time.Time                      `json:"finished_at"`
	DurationMillis       int64                          `json:"duration_millis"`
	Files                []synthesisFile                `json:"files"`
}

type synthesisFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type synthesisRecord struct {
	SubjectID  string                       `json:"subject_id"`
	PacketID   string                       `json:"packet_id"`
	Response   *modelruntime.Response       `json:"response,omitempty"`
	Validation modelruntime.ClaimValidation `json:"validation"`
	Error      string                       `json:"error,omitempty"`
}

func runSynthesize(args []string) error {
	modelContext, stopModel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopModel()
	return runSynthesizeContext(modelContext, args)
}

func runSynthesizeContext(modelContext context.Context, args []string) error {
	if err := synthesisCancellation(modelContext); err != nil {
		return err
	}
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("synthesize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_ = fs.String("config", configPath, "JSON configuration file")
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	repositoryRoot := fs.String("repo-root", "", "repository source root used for evidence excerpts")
	output := fs.String("out", "", "synthesis output directory outside the verified dataset")
	var nodeRefs stringList
	fs.Var(&nodeRefs, "node", "subject node ID, logical ID, name, or qualified name; repeatable")
	query := fs.String("query", "", "select subject nodes using the local search index")
	kinds := fs.String("kinds", "", "comma-separated subject node kinds")
	publicOnly := fs.Bool("public-only", true, "when selecting automatically, include only public surfaces")
	limit := fs.Int("limit", 25, "maximum subject nodes")
	taskValue := fs.String("task", string(modelruntime.TaskSymbolSummary), "symbol_summary, module_summary, execution_explanation, or documentation_gap_analysis")
	packetOnly := fs.Bool("packet-only", false, "write bounded evidence packets without running a model")
	providerType := fs.String("provider", cfg.Model.Provider, "model provider: llama.cpp")
	modelPath := fs.String("model", cfg.Model.ModelPath, "GGUF model file")
	llamaCLI := fs.String("llama-cli", "llama-cli", "llama.cpp CLI executable")
	modelLock := fs.String("model-lock", defaultSynthesisModelLockPath(), "trusted RKC model supply-chain lock")
	modelAsset := fs.String("model-asset", "", "qualified generation asset ID; defaults to the lock default")
	runtimeReceipt := fs.String("runtime-receipt", "", "llama.cpp build receipt; derived from a standard runtime layout when empty")
	contextTokens := fs.Int("context", cfg.Model.ContextTokens, "model context tokens")
	maxOutputTokens := fs.Int("max-output", cfg.Model.MaxOutputTokens, "maximum generated tokens")
	maxRSSMiB := fs.Int64("max-rss-mib", cfg.Model.MaxRSSMiB, "estimated and observed process RSS limit")
	threads := fs.Int("threads", 0, "model inference threads; 0 chooses a conservative default")
	batchSize := fs.Int("batch-size", 128, "llama.cpp logical batch size")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-subject inference timeout")
	maximumRelated := fs.Int("max-related", 24, "maximum related graph nodes per packet")
	maximumEdges := fs.Int("max-edges", 64, "maximum graph edges per packet")
	maximumEvidence := fs.Int("max-evidence", 64, "maximum evidence records per packet")
	maximumExcerptBytes := fs.Int("max-excerpt-bytes", 8*1024, "maximum bytes in one source excerpt")
	maximumSourceBytes := fs.Int("max-source-bytes", 64*1024, "maximum total source bytes per packet")
	allowInference := fs.Bool("allow-inference", false, "permit explicitly marked inferred claims")
	continueOnError := fs.Bool("continue-on-error", true, "continue processing other subjects after one model failure")
	force := fs.Bool("force", false, "replace an existing synthesis profile directory")
	jsonSummary := fs.Bool("json", false, "print machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("synthesize does not accept positional arguments")
	}
	if *limit <= 0 || *limit > 10000 {
		return errors.New("limit must be between 1 and 10000")
	}
	if *contextTokens < 512 || *contextTokens > 262144 {
		return errors.New("context must be between 512 and 262144 tokens")
	}
	if *maxOutputTokens < 1 || *maxOutputTokens > *contextTokens {
		return errors.New("max-output must be positive and no larger than context")
	}
	if *maxRSSMiB < 256 || *maxRSSMiB > modelMaximumRSSMiB {
		return errors.New("max-rss-mib must be between 256 and the 2560 MiB safety ceiling")
	}
	if *threads < 0 || *threads > 64 {
		return errors.New("threads must be between 0 and 64")
	}
	if *batchSize < 1 || *batchSize > 4096 {
		return errors.New("batch-size must be between 1 and 4096")
	}
	task := modelruntime.Task(strings.TrimSpace(*taskValue))
	if !validModelTask(task) {
		return fmt.Errorf("invalid model task %q", *taskValue)
	}
	dataset, err := loadSelectedDataset(modelContext, *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	subjects, err := selectSynthesisSubjects(dataset, nodeRefs, *query, splitSet(*kinds), *publicOnly, *limit)
	if err != nil {
		return err
	}
	if len(subjects) == 0 {
		return errors.New("no eligible subject nodes selected")
	}

	var generation *qualifiedGenerationSession
	providerName := "packet-only"
	var descriptor modelruntime.ModelDescriptor
	var generationBinding *modelassets.GenerationBinding
	responseSchemaDigest := ""
	budget := modelruntime.Budget{MaximumRSSBytes: *maxRSSMiB * 1024 * 1024, SafetyMarginBytes: 128 * 1024 * 1024}
	inferenceOptions := modelruntime.InferenceOptions{ContextTokens: *contextTokens, MaxOutputTokens: *maxOutputTokens, Threads: *threads, BatchSize: *batchSize, Parallel: 1}
	if !*packetOnly {
		if *allowInference {
			return errors.New("--allow-inference is incompatible with the qualified supported-claim response contract")
		}
		generation, err = openQualifiedGenerationProvider(qualifiedGenerationRequest{
			Provider: *providerType, ModelPath: *modelPath, LlamaCLI: *llamaCLI,
			ModelLock: *modelLock, ModelAsset: *modelAsset, RuntimeReceipt: *runtimeReceipt,
			ContextTokens: *contextTokens, MaximumOutputTokens: *maxOutputTokens,
			MaximumRSSMiB: *maxRSSMiB, Threads: *threads, BatchSize: *batchSize,
			Timeout: *timeout, Temperature: cfg.Model.Temperature,
		})
		if err != nil {
			return err
		}
		defer generation.Close()
		providerName = generation.ProviderName
		descriptor = generation.Descriptor
		generationBinding = &generation.Binding
		responseSchemaDigest = generation.ResponseSchemaSHA256
		budget = generation.Budget
		inferenceOptions = generation.Inference
	}
	profile := profileName(providerName, descriptor.ID, task)
	finalOutput, err := resolveSynthesisOutput(*output, dataset.Root, *repositoryRoot, profile)
	if err != nil {
		return err
	}
	_, protectedDatasetRoot, err := synthesisDatasetIdentity(dataset.Root)
	if err != nil {
		return err
	}
	publication, err := safeoutput.Begin(finalOutput, protectedDatasetRoot, *force, "synthesis")
	if err != nil {
		return err
	}
	recheckedOutput, recheckErr := resolveSynthesisOutput(publication.Target, dataset.Root, *repositoryRoot, profile)
	if recheckErr != nil || recheckedOutput != publication.Target {
		abortErr := publication.Abort()
		if recheckErr != nil {
			if abortErr != nil {
				return fmt.Errorf("recheck synthesis output boundary: %w; staging cleanup failed: %v", recheckErr, abortErr)
			}
			return fmt.Errorf("recheck synthesis output boundary: %w", recheckErr)
		}
		if abortErr != nil {
			return fmt.Errorf("synthesis output parent changed during setup; staging cleanup failed: %w", abortErr)
		}
		return fmt.Errorf("%w: synthesis output parent changed during setup", safeoutput.ErrUnsafeTarget)
	}
	defer func() { _ = publication.Abort() }()
	finalOutput = publication.Target
	outputRoot := publication.Staging
	started := time.Now().UTC()
	manifest := synthesisManifest{SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: dataset.Manifest.ID, Profile: profile, Task: task, PacketOnly: *packetOnly, Provider: providerName, Model: descriptor, ModelBinding: generationBinding, ResponseSchemaSHA256: responseSchemaDigest, Options: inferenceOptions, Budget: budget, SubjectsRequested: len(subjects), StartedAt: started}
	claimsPath := filepath.Join(outputRoot, "claims.jsonl")
	diagnosticsPath := filepath.Join(outputRoot, "diagnostics.jsonl")
	recordsPath := filepath.Join(outputRoot, "records.jsonl")
	claimsFile, err := os.Create(claimsPath)
	if err != nil {
		return err
	}
	defer claimsFile.Close()
	diagnosticsFile, err := os.Create(diagnosticsPath)
	if err != nil {
		return err
	}
	defer diagnosticsFile.Close()
	recordsFile, err := os.Create(recordsPath)
	if err != nil {
		return err
	}
	defer recordsFile.Close()
	claimsWriter, diagnosticsWriter, recordsWriter := bufio.NewWriter(claimsFile), bufio.NewWriter(diagnosticsFile), bufio.NewWriter(recordsFile)
	defer claimsWriter.Flush()
	defer diagnosticsWriter.Flush()
	defer recordsWriter.Flush()

	for _, subject := range subjects {
		if err := synthesisCancellation(modelContext); err != nil {
			return err
		}
		report, buildErr := modelruntime.BuildEvidencePacket(dataset.Bundle, subject.ID, modelruntime.PacketBuildOptions{
			RepositoryRoot: *repositoryRoot, Task: task, MaximumRelatedNodes: *maximumRelated,
			MaximumEdges: *maximumEdges, MaximumEvidence: *maximumEvidence, MaximumExcerptBytes: *maximumExcerptBytes,
			MaximumTotalSourceBytes: *maximumSourceBytes, AllowInference: *allowInference, RedactSecrets: true,
		})
		if buildErr != nil {
			if err := synthesisCancellation(modelContext); err != nil {
				return err
			}
			manifest.FailedSubjects++
			if !*continueOnError {
				return buildErr
			}
			record := synthesisRecord{SubjectID: subject.ID, Error: buildErr.Error()}
			if err := writeJSONLine(recordsWriter, record); err != nil {
				return err
			}
			continue
		}
		manifest.RedactedFindings += report.RedactedSecretFindings
		manifest.SourceBytesIncluded += report.IncludedSourceBytes
		if err := synthesisCancellation(modelContext); err != nil {
			return err
		}
		packetPath := filepath.Join(outputRoot, "packets", safeFileKey(subject.ID)+".json")
		if err := writePrettyJSONFile(packetPath, report); err != nil {
			return err
		}
		manifest.PacketsWritten++
		if *packetOnly {
			if err := writeJSONLine(recordsWriter, synthesisRecord{SubjectID: subject.ID, PacketID: report.Packet.PacketID}); err != nil {
				return err
			}
			continue
		}

		requestID := rkcmodel.StableID("model_request", report.Packet.PacketID, descriptor.ID, string(task))
		response, generationErr := generation.Provider.Generate(modelContext, modelruntime.Request{RequestID: requestID, Task: task, Packet: report.Packet, Options: inferenceOptions})
		if err := synthesisCancellation(modelContext); err != nil {
			return err
		}
		if generationErr != nil {
			manifest.FailedSubjects++
			record := synthesisRecord{SubjectID: subject.ID, PacketID: report.Packet.PacketID, Error: generationErr.Error()}
			if err := writeJSONLine(recordsWriter, record); err != nil {
				return err
			}
			if !*continueOnError {
				return generationErr
			}
			continue
		}
		manifest.ResponsesReceived++
		validation := modelruntime.ValidateResponse(report.Packet, response, version)
		manifest.AcceptedClaims += len(validation.Accepted)
		manifest.RejectedClaims += len(validation.Rejected)
		if validation.AcceptedSummary != "" {
			manifest.AcceptedSummaries++
		}
		if len(validation.SummaryRejectedReasons) > 0 {
			manifest.RejectedSummaries++
		}
		for _, claim := range validation.Accepted {
			if err := writeJSONLine(claimsWriter, claim); err != nil {
				return err
			}
		}
		for _, diagnostic := range validation.Diagnostics {
			if err := writeJSONLine(diagnosticsWriter, diagnostic); err != nil {
				return err
			}
		}
		record := synthesisRecord{SubjectID: subject.ID, PacketID: report.Packet.PacketID, Response: &response, Validation: validation}
		if err := writeJSONLine(recordsWriter, record); err != nil {
			return err
		}
		if err := writePrettyJSONFile(filepath.Join(outputRoot, "responses", safeFileKey(subject.ID)+".json"), record); err != nil {
			return err
		}
		if validation.AcceptedSummary != "" || len(validation.Accepted) > 0 {
			if err := writeSynthesisMarkdown(filepath.Join(outputRoot, "summaries", safeFileKey(subject.ID)+".md"), subject, report.Packet, validation, descriptor); err != nil {
				return err
			}
		}
	}
	if err := synthesisCancellation(modelContext); err != nil {
		return err
	}
	if err := claimsWriter.Flush(); err != nil {
		return err
	}
	if err := diagnosticsWriter.Flush(); err != nil {
		return err
	}
	if err := recordsWriter.Flush(); err != nil {
		return err
	}
	if err := claimsFile.Sync(); err != nil {
		return err
	}
	if err := diagnosticsFile.Sync(); err != nil {
		return err
	}
	if err := recordsFile.Sync(); err != nil {
		return err
	}
	if err := claimsFile.Close(); err != nil {
		return err
	}
	if err := diagnosticsFile.Close(); err != nil {
		return err
	}
	if err := recordsFile.Close(); err != nil {
		return err
	}
	finished := time.Now().UTC()
	manifest.FinishedAt = finished
	manifest.DurationMillis = finished.Sub(started).Milliseconds()
	manifest.Status = "completed"
	if *packetOnly && manifest.PacketsWritten == 0 {
		manifest.Status = "failed"
	} else if !*packetOnly && (manifest.ResponsesReceived == 0 || manifest.AcceptedClaims+manifest.AcceptedSummaries == 0) {
		manifest.Status = "rejected"
	} else if manifest.FailedSubjects > 0 {
		manifest.Status = "partial"
	}
	files, err := inventorySynthesisFiles(outputRoot)
	if err != nil {
		return err
	}
	manifest.Files = files
	if err := writePrettyJSONFile(filepath.Join(outputRoot, "manifest.json"), manifest); err != nil {
		return err
	}
	if err := synthesisCancellation(modelContext); err != nil {
		return err
	}
	if err := publication.Commit(manifest.SnapshotID); err != nil {
		return err
	}
	if manifest.Status == "rejected" || manifest.Status == "failed" {
		return fmt.Errorf("synthesis produced no publishable output; audit retained at %s", finalOutput)
	}

	summary := map[string]any{"snapshot_id": manifest.SnapshotID, "status": manifest.Status, "profile": profile, "output": finalOutput, "packet_only": *packetOnly, "packets": manifest.PacketsWritten, "responses": manifest.ResponsesReceived, "accepted_claims": manifest.AcceptedClaims, "rejected_claims": manifest.RejectedClaims, "failed_subjects": manifest.FailedSubjects, "redacted_findings": manifest.RedactedFindings}
	if *jsonSummary {
		return writeJSONStdout(summary)
	}
	fmt.Printf("Synthesis profile: %s\n", profile)
	fmt.Printf("Snapshot: %s\n", manifest.SnapshotID)
	fmt.Printf("Packets: %d; responses: %d; accepted claims: %d; rejected claims: %d\n", manifest.PacketsWritten, manifest.ResponsesReceived, manifest.AcceptedClaims, manifest.RejectedClaims)
	fmt.Printf("Source excerpts: %d bytes; redacted secret findings: %d\n", manifest.SourceBytesIncluded, manifest.RedactedFindings)
	fmt.Printf("Output: %s\n", finalOutput)
	return nil
}

func synthesisCancellation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("synthesis cancelled; staged output was not published: %w", err)
	}
	return nil
}

// resolveSynthesisOutput keeps model-authored and packet-derived files outside
// the verified dataset tree. That separation is a hard trust boundary: a later
// RKC load or self-scan must not mistake generated prose for verified input.
func resolveSynthesisOutput(requested, datasetRoot, repositoryRoot, profile string) (string, error) {
	datasetResolved, protectedDirectory, err := synthesisDatasetIdentity(datasetRoot)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(requested)
	if target == "" {
		datasetAbsolute, err := filepath.Abs(datasetRoot)
		if err != nil {
			return "", fmt.Errorf("resolve verified dataset root: %w", err)
		}
		datasetAbsolute = filepath.Clean(datasetAbsolute)
		target = filepath.Join(
			filepath.Dir(datasetAbsolute),
			filepath.Base(datasetAbsolute)+".rkc-derived",
			"synthesis",
			profile,
		)
	}

	resolved, err := safeoutput.ResolveTarget(target, protectedDirectory)
	if err != nil {
		return "", err
	}
	inside, err := pathIsWithin(datasetResolved, resolved)
	if err != nil {
		return "", fmt.Errorf("compare synthesis output with verified dataset: %w", err)
	}
	if inside {
		return "", fmt.Errorf("%w: synthesis output must be outside verified dataset %s", safeoutput.ErrUnsafeTarget, datasetResolved)
	}
	containsDataset, err := pathIsWithin(resolved, datasetResolved)
	if err != nil {
		return "", fmt.Errorf("compare synthesis output with verified dataset: %w", err)
	}
	if containsDataset {
		return "", fmt.Errorf("%w: synthesis output cannot contain verified dataset %s", safeoutput.ErrUnsafeTarget, datasetResolved)
	}
	if strings.TrimSpace(repositoryRoot) != "" {
		resolved, err = safeoutput.ResolveTarget(resolved, repositoryRoot)
		if err != nil {
			return "", err
		}
		// ResolveTarget may canonicalize another existing parent. Recheck the
		// dataset boundary after every canonicalization step.
		inside, err = pathIsWithin(datasetResolved, resolved)
		if err != nil {
			return "", fmt.Errorf("compare synthesis output with verified dataset: %w", err)
		}
		if inside {
			return "", fmt.Errorf("%w: synthesis output must be outside verified dataset %s", safeoutput.ErrUnsafeTarget, datasetResolved)
		}
	}
	return resolved, nil
}

func synthesisDatasetIdentity(datasetRoot string) (string, string, error) {
	if strings.TrimSpace(datasetRoot) == "" {
		return "", "", errors.New("verified dataset root is empty")
	}
	datasetResolved, err := filepath.EvalSymlinks(datasetRoot)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve verified dataset root: %v", safeoutput.ErrUnsafeTarget, err)
	}
	datasetResolved, err = filepath.Abs(datasetResolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve verified dataset root: %v", safeoutput.ErrUnsafeTarget, err)
	}
	datasetInfo, err := os.Lstat(datasetResolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: inspect verified dataset root: %v", safeoutput.ErrUnsafeTarget, err)
	}
	if datasetInfo.IsDir() {
		return datasetResolved, datasetResolved, nil
	}
	if datasetInfo.Mode().IsRegular() && datasetInfo.Mode()&os.ModeSymlink == 0 {
		return datasetResolved, "", nil
	}
	return "", "", fmt.Errorf("%w: verified dataset identity is neither a regular database nor a directory", safeoutput.ErrUnsafeTarget)
}

func pathIsWithin(parent, candidate string) (bool, error) {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func selectSynthesisSubjects(dataset *server.Dataset, references []string, query string, kinds map[string]struct{}, publicOnly bool, limit int) ([]rkcmodel.Node, error) {
	selected := map[string]rkcmodel.Node{}
	for _, reference := range references {
		node, err := resolveNode(dataset, reference)
		if err != nil {
			return nil, err
		}
		if eligibleSynthesisNode(node, kinds, publicOnly && len(references) == 0) {
			selected[node.ID] = node
		}
	}
	if strings.TrimSpace(query) != "" {
		response := dataset.Search.Search(search.Query{Text: query, Kinds: kinds, ObjectTypes: map[string]struct{}{"node": {}}, Limit: limit * 4})
		for _, hit := range response.Hits {
			node, ok := dataset.NodeByID[hit.Document.ID]
			if !ok || !eligibleSynthesisNode(node, kinds, publicOnly) {
				continue
			}
			selected[node.ID] = node
			if len(selected) >= limit {
				break
			}
		}
	}
	if len(references) == 0 && strings.TrimSpace(query) == "" {
		for _, node := range dataset.Bundle.Nodes {
			if eligibleSynthesisNode(node, kinds, publicOnly) {
				selected[node.ID] = node
			}
		}
	}
	items := make([]rkcmodel.Node, 0, len(selected))
	for _, node := range selected {
		items = append(items, node)
	}
	sort.Slice(items, func(i, j int) bool {
		left := items[i].QualifiedName
		if left == "" {
			left = items[i].Name
		}
		right := items[j].QualifiedName
		if right == "" {
			right = items[j].Name
		}
		if left == right {
			return items[i].ID < items[j].ID
		}
		return left < right
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func eligibleSynthesisNode(node rkcmodel.Node, kinds map[string]struct{}, publicOnly bool) bool {
	if node.Kind == "unresolved_symbol" || node.Kind == "secret" || node.Kind == "repository" || node.Kind == "directory" {
		return false
	}
	if len(kinds) > 0 {
		if _, ok := kinds[node.Kind]; !ok {
			return false
		}
	} else if !rkcmodel.IsSymbolKind(node.Kind) && node.Kind != "module" && node.Kind != "package" && node.Kind != "project" && node.Kind != "api_endpoint" && node.Kind != "document" {
		return false
	}
	if publicOnly && !node.PublicSurface && node.Visibility != "public" {
		return false
	}
	return len(node.EvidenceIDs) > 0
}

func validModelTask(task modelruntime.Task) bool {
	switch task {
	case modelruntime.TaskSymbolSummary, modelruntime.TaskModuleSummary, modelruntime.TaskExecutionExplanation, modelruntime.TaskGapAnalysis:
		return true
	default:
		return false
	}
}

func profileName(provider, modelID string, task modelruntime.Task) string {
	parts := []string{provider}
	if modelID != "" {
		parts = append(parts, modelID)
	}
	parts = append(parts, string(task))
	return safeFileKey(strings.Join(parts, "-"))
}

func safeFileKey(value string) string {
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}
	result := strings.Trim(builder.String(), "_.-")
	if result == "" {
		result = "item"
	}
	if len(result) > 120 {
		digest := sha256.Sum256([]byte(value))
		result = result[:96] + "-" + hex.EncodeToString(digest[:8])
	}
	return result
}

func writeJSONLine(writer *bufio.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		return err
	}
	return writer.WriteByte('\n')
}

func writePrettyJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func writeSynthesisMarkdown(path string, subject rkcmodel.Node, packet modelruntime.EvidencePacket, validation modelruntime.ClaimValidation, descriptor modelruntime.ModelDescriptor) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	name := subject.QualifiedName
	if name == "" {
		name = subject.Name
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "---\nrkc_snapshot_id: %q\nrkc_node_id: %q\nrkc_packet_id: %q\nmodel_id: %q\nvalidation: %q\n---\n\n", packet.SnapshotID, subject.ID, packet.PacketID, descriptor.ID, "structure-and-citations-checked")
	fmt.Fprintf(&builder, "# %s\n\n", markdownPlain(name))
	builder.WriteString("> Model-authored text below passed structural and citation-link checks. A citation is a trace to evidence, not deterministic proof that the evidence entails the statement.\n\n")
	if validation.AcceptedSummary != "" {
		builder.WriteString(markdownPlain(validation.AcceptedSummary))
		builder.WriteString("\n\n")
	}
	if len(validation.Accepted) > 0 {
		builder.WriteString("## Structurally accepted, citation-linked claims\n\n")
		for _, claim := range validation.Accepted {
			fmt.Fprintf(&builder, "- %s\n", markdownPlain(claim.Text))
			fmt.Fprintf(&builder, "  - Evidence: `%s`\n", strings.Join(claim.EvidenceIDs, "`, `"))
			fmt.Fprintf(&builder, "  - Certainty: `%s`; category: `%s`\n", claim.Certainty, claim.Category)
		}
	}
	if len(validation.Rejected) > 0 || len(validation.SummaryRejectedReasons) > 0 {
		builder.WriteString("\n## Rejected model output\n\n")
		for _, reason := range validation.SummaryRejectedReasons {
			fmt.Fprintf(&builder, "- Summary rejected: %s\n", reason)
		}
		for _, rejected := range validation.Rejected {
			fmt.Fprintf(&builder, "- Claim rejected: %s\n", strings.Join(rejected.Reasons, "; "))
		}
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func markdownPlain(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	return strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
		"[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;",
	).Replace(value)
}

func inventorySynthesisFiles(root string) ([]synthesisFile, error) {
	var files []synthesisFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(path) == "manifest.json" || entry.Name() == safeoutput.MarkerName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		files = append(files, synthesisFile{Path: filepath.ToSlash(relative), SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(data))})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func maxIntCLI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
