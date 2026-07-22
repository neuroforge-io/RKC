package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

func runCheck(args []string) error {
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", configPath, "JSON configuration file; omitted uses built-in defaults")
	coveragePath := fs.String("coverage", ".rkc/coverage.json", "coverage report path")
	bundlePath := fs.String("bundle", "", "canonical bundle path; empty discovers bundle.json beside coverage")
	exportManifestPath := fs.String("export-manifest", "", "export manifest path; empty discovers rkc-export-manifest.json beside coverage")
	verifyBundle := fs.Bool("verify-bundle", true, "validate graph integrity and deterministic digest when bundle exists")
	verifyFiles := fs.Bool("verify-files", true, "verify export file sizes and SHA-256 hashes when export manifest exists")
	strictVocabulary := fs.Bool("strict-vocabulary", true, "validate canonical node, edge, artifact, and evidence vocabularies")
	minInventory := fs.Float64("min-inventory-accounting", cfg.QualityGates.MinInventoryAccounting, "minimum artifact accounting ratio")
	minSyntax := fs.Float64("min-syntax-parse", 0.0, "minimum syntax parse ratio")
	minSemantic := fs.Float64("min-semantic-parse", 0.0, "minimum semantic parse ratio")
	minEvidence := fs.Float64("min-symbol-evidence", cfg.QualityGates.MinSymbolEvidence, "minimum symbol evidence ratio")
	minPublicDocs := fs.Float64("min-public-documentation", -1.0, "minimum public documentation ratio; -1 disables")
	minResolution := fs.Float64("min-edge-resolution", cfg.QualityGates.MinEdgeResolution, "minimum resolved-edge ratio")
	minClaims := fs.Float64("min-claim-citation", cfg.QualityGates.MinClaimCitation, "minimum cited-claim ratio when claims exist")
	maxErrors := fs.Int("max-errors", cfg.QualityGates.MaxErrorDiagnostics, "maximum error diagnostics")
	maxFatal := fs.Int("max-fatal", 0, "maximum fatal diagnostics")
	maxUnresolved := fs.Int("max-unresolved", cfg.QualityGates.MaxUnresolvedEdges, "maximum unresolved edges; -1 disables")
	maxHighConfidenceSecrets := fs.Int("max-high-confidence-secrets", cfg.QualityGates.MaxHighConfidenceSecrets, "maximum high-confidence secret findings; -1 disables")
	requireSecretRedaction := fs.Bool("require-secret-redaction", cfg.Security.RedactExports, "require normalized source exports to redact probable secrets")
	requireDigest := fs.Bool("require-digest", cfg.QualityGates.RequireDeterminism, "require a deterministic output digest")
	jsonOutput := fs.Bool("json", false, "print machine-readable result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configFlag != configPath {
		return errors.New("--config must be supplied only once; its values establish flag defaults")
	}
	data, err := os.ReadFile(*coveragePath)
	if err != nil {
		return err
	}
	var coverage rkcmodel.Coverage
	if err := json.Unmarshal(data, &coverage); err != nil {
		return fmt.Errorf("decode coverage: %w", err)
	}
	var failures []string
	outputRoot := filepath.Dir(*coveragePath)
	if *bundlePath == "" {
		*bundlePath = filepath.Join(outputRoot, "bundle.json")
	}
	if *exportManifestPath == "" {
		*exportManifestPath = filepath.Join(outputRoot, "rkc-export-manifest.json")
	}
	if *verifyBundle {
		if _, statErr := os.Stat(*bundlePath); statErr == nil {
			bundleData, readErr := os.ReadFile(*bundlePath)
			if readErr != nil {
				failures = append(failures, "read bundle: "+readErr.Error())
			} else {
				var bundle rkcmodel.Bundle
				if decodeErr := json.Unmarshal(bundleData, &bundle); decodeErr != nil {
					failures = append(failures, "decode bundle: "+decodeErr.Error())
				} else {
					report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: *strictVocabulary, RequireEvidence: true})
					for _, diagnostic := range report.Diagnostics {
						if diagnostic.Severity == "error" || diagnostic.Severity == "fatal" {
							failures = append(failures, diagnostic.Code+": "+diagnostic.Message)
						}
					}
					actualDigest := rkcmodel.CanonicalDigest(bundle)
					if actualDigest == "" || actualDigest != coverage.DeterministicOutputDigest {
						failures = append(failures, fmt.Sprintf("bundle digest %s does not match coverage digest %s", actualDigest, coverage.DeterministicOutputDigest))
					}
					if bundle.Snapshot.ID != coverage.SnapshotID {
						failures = append(failures, fmt.Sprintf("bundle snapshot %s does not match coverage snapshot %s", bundle.Snapshot.ID, coverage.SnapshotID))
					}
				}
			}
		} else if !os.IsNotExist(statErr) {
			failures = append(failures, "stat bundle: "+statErr.Error())
		}
	}
	if *verifyFiles {
		if _, statErr := os.Stat(*exportManifestPath); statErr == nil {
			failures = append(failures, verifyExportFiles(outputRoot, *exportManifestPath)...)
		} else if !os.IsNotExist(statErr) {
			failures = append(failures, "stat export manifest: "+statErr.Error())
		}
	}
	if *requireSecretRedaction {
		failures = append(failures, verifySecretRedactionPolicy(outputRoot)...)
	}
	checkRatio := func(name string, actual, minimum float64, enabled bool) {
		if enabled && actual+1e-12 < minimum {
			failures = append(failures, fmt.Sprintf("%s %.4f < %.4f", name, actual, minimum))
		}
	}
	checkRatio("inventory accounting", coverage.InventoryAccountingRatio, *minInventory, true)
	checkRatio("syntax parse", coverage.SyntacticParseRatio, *minSyntax, coverage.TextArtifacts > 0)
	checkRatio("semantic parse", coverage.SemanticParseRatio, *minSemantic, coverage.TextArtifacts > 0)
	checkRatio("symbol evidence", coverage.SymbolEvidenceRatio, *minEvidence, coverage.SymbolsTotal > 0)
	checkRatio("public documentation", coverage.PublicDocumentationRatio, *minPublicDocs, *minPublicDocs >= 0 && coverage.PublicSymbols > 0)
	checkRatio("edge resolution", coverage.EdgeResolutionRatio, *minResolution, coverage.EdgesTotal > 0)
	checkRatio("claim citation", coverage.ClaimCitationRatio, *minClaims, coverage.ClaimsTotal > 0)
	if coverage.DiagnosticsBySeverity["error"] > *maxErrors {
		failures = append(failures, fmt.Sprintf("errors %d > %d", coverage.DiagnosticsBySeverity["error"], *maxErrors))
	}
	if coverage.DiagnosticsBySeverity["fatal"] > *maxFatal {
		failures = append(failures, fmt.Sprintf("fatal diagnostics %d > %d", coverage.DiagnosticsBySeverity["fatal"], *maxFatal))
	}
	if *maxUnresolved >= 0 && coverage.UnresolvedEdges > *maxUnresolved {
		failures = append(failures, fmt.Sprintf("unresolved edges %d > %d", coverage.UnresolvedEdges, *maxUnresolved))
	}
	if *maxHighConfidenceSecrets >= 0 && coverage.HighConfidenceSecretFindings > *maxHighConfidenceSecrets {
		failures = append(failures, fmt.Sprintf("high-confidence secret findings %d > %d", coverage.HighConfidenceSecretFindings, *maxHighConfidenceSecrets))
	}
	if *requireDigest && strings.TrimSpace(coverage.DeterministicOutputDigest) == "" {
		failures = append(failures, "deterministic output digest is missing")
	}
	result := map[string]any{"passed": len(failures) == 0, "failures": failures, "coverage": coverage}
	if *jsonOutput {
		if err := writeJSONStdout(result); err != nil {
			return err
		}
	} else if len(failures) == 0 {
		fmt.Printf("Quality gate passed for %s: inventory=%s syntax=%s semantic=%s evidence=%s resolution=%s errors=%d unresolved=%d\n",
			coverage.SnapshotID,
			strconv.FormatFloat(coverage.InventoryAccountingRatio, 'f', 4, 64),
			strconv.FormatFloat(coverage.SyntacticParseRatio, 'f', 4, 64),
			strconv.FormatFloat(coverage.SemanticParseRatio, 'f', 4, 64),
			strconv.FormatFloat(coverage.SymbolEvidenceRatio, 'f', 4, 64),
			strconv.FormatFloat(coverage.EdgeResolutionRatio, 'f', 4, 64),
			coverage.DiagnosticsBySeverity["error"], coverage.UnresolvedEdges)
	}
	if len(failures) > 0 {
		return errors.New("quality gate failed: " + strings.Join(failures, "; "))
	}
	return nil
}

type checkExportPolicy struct {
	NormalizedSources bool `json:"normalized_sources"`
	SecretRedaction   bool `json:"secret_redaction"`
}

func verifySecretRedactionPolicy(root string) []string {
	path := filepath.Join(root, "rkc.export-policy.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"read export policy: " + err.Error()}
	}
	var policy checkExportPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return []string{"decode export policy: " + err.Error()}
	}
	if policy.NormalizedSources && !policy.SecretRedaction {
		return []string{"normalized source export was generated without secret redaction"}
	}
	return nil
}

type checkExportFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Canonical bool   `json:"canonical"`
}

type checkExportManifest struct {
	Files []checkExportFile `json:"files"`
}

func verifyExportFiles(root, manifestPath string) []string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return []string{"read export manifest: " + err.Error()}
	}
	var manifest checkExportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return []string{"decode export manifest: " + err.Error()}
	}
	var failures []string
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return []string{"resolve export root: " + err.Error()}
	}
	for _, record := range manifest.Files {
		clean := filepath.Clean(filepath.FromSlash(record.Path))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			failures = append(failures, "unsafe export path in manifest: "+record.Path)
			continue
		}
		path := filepath.Join(rootAbs, clean)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			failures = append(failures, "read exported file "+record.Path+": "+readErr.Error())
			continue
		}
		if int64(len(data)) != record.Size {
			failures = append(failures, fmt.Sprintf("exported file %s size %d does not match manifest %d", record.Path, len(data), record.Size))
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		if digest != strings.ToLower(record.SHA256) {
			failures = append(failures, fmt.Sprintf("exported file %s digest %s does not match manifest %s", record.Path, digest, record.SHA256))
		}
	}
	return failures
}
