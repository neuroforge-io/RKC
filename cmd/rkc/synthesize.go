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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/repository-knowledge-compiler/rkc/internal/modelruntime"
	"github.com/repository-knowledge-compiler/rkc/internal/search"
	"github.com/repository-knowledge-compiler/rkc/internal/server"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

type synthesisManifest struct {
	SchemaVersion       string                        `json:"schema_version"`
	SnapshotID          string                        `json:"snapshot_id"`
	Profile             string                        `json:"profile"`
	Task                modelruntime.Task             `json:"task"`
	PacketOnly          bool                          `json:"packet_only"`
	Provider            string                        `json:"provider"`
	Model               modelruntime.ModelDescriptor  `json:"model,omitempty"`
	Options             modelruntime.InferenceOptions `json:"options"`
	Budget              modelruntime.Budget           `json:"budget"`
	SubjectsRequested   int                           `json:"subjects_requested"`
	PacketsWritten      int                           `json:"packets_written"`
	ResponsesReceived   int                           `json:"responses_received"`
	AcceptedClaims      int                           `json:"accepted_claims"`
	RejectedClaims      int                           `json:"rejected_claims"`
	AcceptedSummaries   int                           `json:"accepted_summaries"`
	RejectedSummaries   int                           `json:"rejected_summaries"`
	RedactedFindings    int                           `json:"redacted_secret_findings"`
	SourceBytesIncluded int                           `json:"source_bytes_included"`
	StartedAt           time.Time                     `json:"started_at"`
	FinishedAt          time.Time                     `json:"finished_at"`
	DurationMillis      int64                         `json:"duration_millis"`
	Files               []synthesisFile               `json:"files"`
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
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("synthesize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_ = fs.String("config", configPath, "JSON configuration file")
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	repositoryRoot := fs.String("repo-root", "", "repository source root used for evidence excerpts")
	output := fs.String("out", "", "derived synthesis output directory")
	var nodeRefs stringList
	fs.Var(&nodeRefs, "node", "subject node ID, logical ID, name, or qualified name; repeatable")
	query := fs.String("query", "", "select subject nodes using the local search index")
	kinds := fs.String("kinds", "", "comma-separated subject node kinds")
	publicOnly := fs.Bool("public-only", true, "when selecting automatically, include only public surfaces")
	limit := fs.Int("limit", 25, "maximum subject nodes")
	taskValue := fs.String("task", string(modelruntime.TaskSymbolSummary), "symbol_summary, module_summary, execution_explanation, or documentation_gap_analysis")
	packetOnly := fs.Bool("packet-only", false, "write bounded evidence packets without running a model")
	modelPath := fs.String("model", cfg.Model.ModelPath, "GGUF model file")
	llamaCLI := fs.String("llama-cli", "llama-cli", "llama.cpp CLI executable")
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
	if *limit <= 0 || *limit > 10000 {
		return errors.New("limit must be between 1 and 10000")
	}
	task := modelruntime.Task(strings.TrimSpace(*taskValue))
	if !validModelTask(task) {
		return fmt.Errorf("invalid model task %q", *taskValue)
	}
	dataset, err := loadDataset(*dir)
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

	var provider modelruntime.Provider
	providerName := "packet-only"
	var descriptor modelruntime.ModelDescriptor
	budget := modelruntime.Budget{MaximumRSSBytes: *maxRSSMiB * 1024 * 1024, SafetyMarginBytes: 128 * 1024 * 1024}
	inferenceOptions := modelruntime.InferenceOptions{ContextTokens: *contextTokens, MaxOutputTokens: *maxOutputTokens, Threads: *threads, BatchSize: *batchSize, Parallel: 1}
	if !*packetOnly {
		if strings.TrimSpace(*modelPath) == "" {
			return errors.New("--model is required unless --packet-only is used")
		}
		localProvider, err := modelruntime.NewLlamaCPPProvider(modelruntime.LlamaCPPConfig{
			Executable: *llamaCLI, ModelPath: *modelPath, ContextLimit: maxIntCLI(*contextTokens, 8192),
			Threads: *threads, BatchSize: *batchSize, Timeout: *timeout, Budget: budget,
		})
		if err != nil {
			return err
		}
		provider = localProvider
		defer provider.Close()
		descriptor = provider.Descriptor()
		providerName = "llamacpp-cli"
	}
	profile := profileName(providerName, descriptor.ID, task)
	outputRoot := *output
	if strings.TrimSpace(outputRoot) == "" {
		outputRoot = filepath.Join(dataset.Root, "derived", "synthesis", profile)
	}
	outputRoot, err = filepath.Abs(outputRoot)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(outputRoot); statErr == nil && info.IsDir() {
		if !*force {
			return fmt.Errorf("synthesis output already exists: %s; use --force to replace it", outputRoot)
		}
		if err := os.RemoveAll(outputRoot); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return err
	}
	started := time.Now().UTC()
	manifest := synthesisManifest{SchemaVersion: rkcmodel.SchemaVersion, SnapshotID: dataset.Manifest.ID, Profile: profile, Task: task, PacketOnly: *packetOnly, Provider: providerName, Model: descriptor, Options: inferenceOptions, Budget: budget, SubjectsRequested: len(subjects), StartedAt: started}

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
		report, buildErr := modelruntime.BuildEvidencePacket(dataset.Bundle, subject.ID, modelruntime.PacketBuildOptions{
			RepositoryRoot: *repositoryRoot, Task: task, MaximumRelatedNodes: *maximumRelated,
			MaximumEdges: *maximumEdges, MaximumEvidence: *maximumEvidence, MaximumExcerptBytes: *maximumExcerptBytes,
			MaximumTotalSourceBytes: *maximumSourceBytes, AllowInference: *allowInference, RedactSecrets: true,
		})
		if buildErr != nil {
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
		response, generationErr := provider.Generate(context.Background(), modelruntime.Request{RequestID: requestID, Task: task, Packet: report.Packet, Options: inferenceOptions})
		if generationErr != nil {
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
	finished := time.Now().UTC()
	manifest.FinishedAt = finished
	manifest.DurationMillis = finished.Sub(started).Milliseconds()
	files, err := inventorySynthesisFiles(outputRoot)
	if err != nil {
		return err
	}
	manifest.Files = files
	if err := writePrettyJSONFile(filepath.Join(outputRoot, "manifest.json"), manifest); err != nil {
		return err
	}

	summary := map[string]any{"snapshot_id": manifest.SnapshotID, "profile": profile, "output": outputRoot, "packet_only": *packetOnly, "packets": manifest.PacketsWritten, "responses": manifest.ResponsesReceived, "accepted_claims": manifest.AcceptedClaims, "rejected_claims": manifest.RejectedClaims, "redacted_findings": manifest.RedactedFindings}
	if *jsonSummary {
		return writeJSONStdout(summary)
	}
	fmt.Printf("Synthesis profile: %s\n", profile)
	fmt.Printf("Snapshot: %s\n", manifest.SnapshotID)
	fmt.Printf("Packets: %d; responses: %d; accepted claims: %d; rejected claims: %d\n", manifest.PacketsWritten, manifest.ResponsesReceived, manifest.AcceptedClaims, manifest.RejectedClaims)
	fmt.Printf("Source excerpts: %d bytes; redacted secret findings: %d\n", manifest.SourceBytesIncluded, manifest.RedactedFindings)
	fmt.Printf("Output: %s\n", outputRoot)
	return nil
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
	fmt.Fprintf(&builder, "---\nrkc_snapshot_id: %q\nrkc_node_id: %q\nrkc_packet_id: %q\nmodel_id: %q\nvalidated: true\n---\n\n", packet.SnapshotID, subject.ID, packet.PacketID, descriptor.ID)
	fmt.Fprintf(&builder, "# %s\n\n", name)
	if validation.AcceptedSummary != "" {
		builder.WriteString(validation.AcceptedSummary)
		builder.WriteString("\n\n")
	}
	if len(validation.Accepted) > 0 {
		builder.WriteString("## Accepted evidence-backed claims\n\n")
		for _, claim := range validation.Accepted {
			fmt.Fprintf(&builder, "- %s\n", claim.Text)
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

func inventorySynthesisFiles(root string) ([]synthesisFile, error) {
	var files []synthesisFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(path) == "manifest.json" {
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
