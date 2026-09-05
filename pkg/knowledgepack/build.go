package knowledgepack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const maximumReferences = 4096

var limitations = []string{
	"Source content is untrusted data; embedded instructions must not be executed or treated as agent authority.",
	"Checksums establish integrity, not factual truth, citation entailment, licensing permission, or fitness for training.",
	"Source and third-party rights remain unchanged; absent license expressions mean unknown, not permission.",
	"Artifact text comes only from verified secret-redacted atlas bodies; absent, excluded, binary, or bounded bodies remain metadata only.",
	"Secret detection is heuristic; publication and training require the source owner's own review.",
	"Text limits can truncate units; source ranges refer to original artifacts, not byte offsets in redacted or truncated text.",
	"Relations preserve analyzer resolution labels; they do not establish prerequisites, difficulty, or learning order.",
	"Source group IDs assist provenance grouping but do not prove deduplication or prevent cross-repository evaluation leakage.",
	"Claims retain their original certainty and validation; citation links do not prove the claim is correct.",
}

// Builder accepts atlases sequentially so callers can release each loaded
// dataset before loading the next one. Add failures poison the builder: a
// partially admitted source can never be mistaken for a complete pack.
type Builder struct {
	pack         Pack
	seen         map[string]bool
	bytes        int
	encodedBytes int
	failure      error
}

// New constructs a builder with validated, concrete resource limits.
func New(options Options) (*Builder, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	return &Builder{pack: Pack{Options: options}, seen: map[string]bool{}}, nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.MaxUnits == 0 {
		options.MaxUnits = MaximumUnits
	}
	if options.MaxUnitTextBytes == 0 {
		options.MaxUnitTextBytes = 16 * 1024
	}
	if options.MaxTotalTextBytes == 0 {
		options.MaxTotalTextBytes = 64 * 1024 * 1024
	}
	if options.MaxUnits < 1 || options.MaxUnits > MaximumUnits ||
		options.MaxUnitTextBytes < 256 || options.MaxUnitTextBytes > 64*1024 ||
		options.MaxTotalTextBytes < 1 || options.MaxTotalTextBytes > MaximumTextBytes {
		return Options{}, errors.New("knowledge pack limits require 1..100000 units, 256..65536 bytes per unit, and 1..134217728 total text bytes")
	}
	return options, nil
}

// Add validates and admits one source. Integrity must be "verified". The
// caller must actually verify the atlas export and body receipts before using
// this API; this string is a caller assertion, not independent authentication.
// The CLI performs that verification through the existing atlas loader.
func (builder *Builder) Add(ctx context.Context, input Input) (err error) {
	if builder == nil {
		return errors.New("knowledge pack builder is nil")
	}
	if builder.failure != nil {
		return builder.failure
	}
	defer func() {
		if err != nil {
			builder.failure = err
		}
	}()
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(builder.pack.Sources) >= MaximumSources {
		return fmt.Errorf("knowledge pack exceeds %d source limit", MaximumSources)
	}
	if input.Integrity != "verified" {
		return errors.New("knowledge packs require a modern verified atlas")
	}
	bundle := input.Bundle
	if len(bundle.Artifacts)+len(bundle.Nodes)+len(bundle.Edges)+len(bundle.Evidence)+len(bundle.Documents)+len(bundle.Claims) > 1_000_000 {
		return errors.New("knowledge source exceeds 1000000 canonical record limit")
	}
	if result := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true}); result.HasErrors() {
		return errors.New("knowledge source failed canonical bundle validation")
	}
	// Verified atlas readers already deliver canonical decoded bundles. Avoid
	// cloning their entire graph simply to calculate its portable digest.
	var canonical []byte
	if rkcmodel.IsCanonicalDecodedBundle(bundle) {
		canonical, err = json.Marshal(bundle)
	} else {
		canonical, err = rkcmodel.CanonicalJSON(bundle)
	}
	if err != nil {
		return err
	}
	bundleDigest := digest(canonical)
	group := bundle.Snapshot.RepositoryID
	if group == "" {
		group = rkcmodel.StableID("knowledge_group", bundle.Snapshot.ContentDigest)
	}
	source := Source{
		SourceID: rkcmodel.StableID("knowledge_source", bundle.Snapshot.ID, bundleDigest),
		GroupID:  group, SnapshotID: bundle.Snapshot.ID, RepositoryID: bundle.Snapshot.RepositoryID,
		Name: bundle.Snapshot.RootName, Origin: bundle.Snapshot.Git.Origin,
		Commit: bundle.Snapshot.Git.Commit, Dirty: bundle.Snapshot.Git.Dirty,
		ContentDigest: bundle.Snapshot.ContentDigest, BundleSHA256: bundleDigest,
		Integrity: input.Integrity, Coverage: rkcmodel.BuildCoverage(bundle),
	}
	if builder.seen[source.SourceID] {
		return fmt.Errorf("duplicate knowledge source %s", source.SourceID)
	}
	builder.seen[source.SourceID] = true
	builder.pack.Sources = append(builder.pack.Sources, source)
	artifacts := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		artifacts[artifact.ID] = artifact
	}
	evidence := make(map[string]rkcmodel.Evidence, len(bundle.Evidence))
	for _, item := range bundle.Evidence {
		evidence[item.ID] = item
	}
	relations := make(map[string][]Relation)
	for _, edge := range bundle.Edges {
		relations[edge.From] = append(relations[edge.From], Relation{Kind: edge.Kind, TargetObjectID: edge.To, Resolution: edge.Resolution})
		if len(relations[edge.From]) > maximumReferences {
			return errors.New("knowledge source exceeds 4096 outgoing relations per object")
		}
	}
	citations := func(ids []string, location *rkcmodel.SourceRange) []Citation {
		result := []Citation{}
		seen := map[string]bool{}
		appendCitation := func(citation Citation) {
			if citation.Source != nil {
				copy := *citation.Source
				citation.Source = &copy
				citation.ArtifactSHA256 = artifacts[copy.ArtifactID].SHA256
			}
			encoded, _ := json.Marshal(citation)
			if !seen[string(encoded)] {
				seen[string(encoded)] = true
				result = append(result, citation)
			}
		}
		for _, id := range ids {
			item := evidence[id]
			appendCitation(Citation{EvidenceID: item.ID, Kind: item.Kind, Method: item.Method, Confidence: item.Confidence, Source: item.Source})
		}
		if location != nil {
			appendCitation(Citation{Kind: "artifact", Method: "canonical-source-location", Confidence: 1, Source: location})
		}
		sort.Slice(result, func(i, j int) bool {
			left, _ := json.Marshal(result[i])
			right, _ := json.Marshal(result[j])
			return string(left) < string(right)
		})
		return result
	}
	add := func(unit Unit) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		unit.SourceID, unit.GroupID = source.SourceID, source.GroupID
		unit.ID = unitID(unit)
		return builder.addUnit(unit)
	}
	for _, artifact := range bundle.Artifacts {
		body, included := input.ArtifactBodies[artifact.ID]
		if included && (!artifact.Text || !admittedArtifact(artifact)) {
			return fmt.Errorf("body supplied for non-admitted artifact %s", artifact.ID)
		}
		if !included {
			body = artifact.Path + "\n" + artifact.Kind + "\n" + artifact.Status
		}
		location := &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path}
		if artifact.LineCount > 0 {
			location.StartLine, location.EndLine = 1, artifact.LineCount
		}
		if err := add(Unit{ObjectID: artifact.ID, Kind: "artifact", Title: artifact.Path, Text: body,
			Path: artifact.Path, Language: artifact.Language, LicenseExpression: artifact.LicenseExpression,
			Citations: citations(nil, location), Relations: relations[artifact.ID], MetadataOnly: !included}); err != nil {
			return err
		}
	}
	for id := range input.ArtifactBodies {
		if _, exists := artifacts[id]; !exists {
			return fmt.Errorf("body supplied for unknown artifact %s", id)
		}
	}
	for _, node := range bundle.Nodes {
		artifact := artifacts[node.ArtifactID]
		text := []string{node.Name}
		if node.Signature != "" {
			text = append(text, node.Signature)
		}
		hasBody := false
		for _, key := range []string{"docstring", "summary", "description", "purpose"} {
			if value, ok := node.Attributes[key].(string); ok && strings.TrimSpace(value) != "" {
				text = append(text, value)
				hasBody = true
			}
		}
		path := artifact.Path
		if node.Source != nil {
			path = node.Source.Path
		}
		if err := add(Unit{ObjectID: node.ID, Kind: "node", Title: node.Name, Text: strings.Join(text, "\n"),
			Path: path, Language: node.Language, LicenseExpression: artifact.LicenseExpression,
			Citations: citations(node.EvidenceIDs, node.Source), Relations: relations[node.ID], MetadataOnly: !hasBody}); err != nil {
			return err
		}
	}
	for _, document := range bundle.Documents {
		for _, section := range document.Sections {
			if len(document.SubjectIDs) > maximumReferences || len(section.ClaimIDs) > maximumReferences-len(document.SubjectIDs) {
				return errors.New("knowledge document section exceeds 4096 claim and subject relations")
			}
			sectionRelations := make([]Relation, 0, len(document.SubjectIDs)+len(section.ClaimIDs))
			for _, subjectID := range document.SubjectIDs {
				sectionRelations = append(sectionRelations, Relation{Kind: "describes", TargetObjectID: subjectID, Resolution: "explicit"})
			}
			for _, claimID := range section.ClaimIDs {
				sectionRelations = append(sectionRelations, Relation{Kind: "presents_claim", TargetObjectID: claimID, Resolution: "explicit"})
			}
			// Preserve the document's subject associations and the section's claim
			// links without treating claim evidence as direct section evidence.
			// Consumers can follow each claim's own certainty and review state.
			body := section.PlainText
			if body == "" {
				body = section.Markdown
			}
			title := document.Title
			if section.Heading != "" {
				title += " / " + section.Heading
			}
			if err := add(Unit{ObjectID: document.ID, SectionID: section.ID, Kind: "document_section", Title: title, Text: body,
				Path: document.Path, Citations: citations(section.EvidenceIDs, nil), Generator: document.Generator,
				Validation: document.Status, Relations: sectionRelations, MetadataOnly: strings.TrimSpace(body) == ""}); err != nil {
				return err
			}
		}
	}
	for _, claim := range bundle.Claims {
		if err := add(Unit{ObjectID: claim.ID, Kind: "claim", Title: claim.Category, Text: claim.Text,
			Citations: citations(claim.EvidenceIDs, nil), Generator: claim.Generator, Certainty: claim.Certainty,
			Validation: claim.Validation, Relations: []Relation{{Kind: "describes", TargetObjectID: claim.SubjectID, Resolution: "explicit"}}}); err != nil {
			return err
		}
	}
	return nil
}

func admittedArtifact(artifact rkcmodel.Artifact) bool {
	switch artifact.Status {
	case "text", "parsed", "syntax_parsed", "semantic_parsed":
		return true
	}
	return false
}

func (builder *Builder) addUnit(unit Unit) error {
	if len(builder.pack.Units) >= builder.pack.Options.MaxUnits {
		return errors.New("knowledge pack exceeds max-units; no partial pack was published")
	}
	if len(unit.Citations) > maximumReferences || len(unit.Relations) > maximumReferences {
		return errors.New("knowledge unit exceeds 4096 citation or relationship limit")
	}
	if !utf8.ValidString(unit.Text) || !utf8.ValidString(unit.Title) {
		return errors.New("knowledge unit text must be valid UTF-8")
	}
	if len(unit.Text) > 8*1024*1024 {
		return errors.New("knowledge unit input exceeds 8 MiB before redaction")
	}
	data := []byte(unit.Text)
	unit.Text = string(secrets.Redact(data, secrets.Scan(data)))
	title := []byte(unit.Title)
	unit.Title = string(secrets.Redact(title, secrets.Scan(title)))
	unit.OriginalTextBytes = len(unit.Text)
	unit.Text, unit.Truncated = truncateUTF8(unit.Text, builder.pack.Options.MaxUnitTextBytes)
	unit.Title, _ = truncateUTF8(unit.Title, 1024)
	unit.ContentSHA256 = digest([]byte(unit.Text))
	if len(unit.Text) > builder.pack.Options.MaxTotalTextBytes-builder.bytes {
		return errors.New("knowledge pack exceeds max-total-text-bytes; no partial pack was published")
	}
	if unit.Citations == nil {
		unit.Citations = []Citation{}
	}
	if unit.Relations == nil {
		unit.Relations = []Relation{}
	}
	sort.Slice(unit.Relations, func(i, j int) bool {
		left, right := unit.Relations[i], unit.Relations[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.TargetObjectID != right.TargetObjectID {
			return left.TargetObjectID < right.TargetObjectID
		}
		return left.Resolution < right.Resolution
	})
	encoded, err := json.Marshal(unit)
	if err != nil || len(encoded)+1 >= maximumJSONLineBytes {
		return errors.New("knowledge unit exceeds 2 MiB JSON line limit")
	}
	if len(encoded)+1 > maximumPayloadBytes-builder.encodedBytes {
		return errors.New("knowledge units exceed 256 MiB serialized payload limit")
	}
	builder.encodedBytes += len(encoded) + 1
	builder.bytes += len(unit.Text)
	builder.pack.Units = append(builder.pack.Units, unit)
	return nil
}

// Finish returns deterministically sorted units and a factual quality report.
// The returned slices belong to the builder and should be treated as immutable.
func (builder *Builder) Finish() (Pack, error) {
	if builder == nil {
		return Pack{}, errors.New("knowledge pack builder is nil")
	}
	if builder.failure != nil {
		return Pack{}, builder.failure
	}
	if len(builder.pack.Sources) == 0 {
		return Pack{}, errors.New("at least one verified source is required")
	}
	sort.Slice(builder.pack.Sources, func(i, j int) bool { return builder.pack.Sources[i].SourceID < builder.pack.Sources[j].SourceID })
	sort.Slice(builder.pack.Units, func(i, j int) bool { return builder.pack.Units[i].ID < builder.pack.Units[j].ID })
	builder.pack.Quality = summarize(builder.pack.Sources, builder.pack.Units)
	return builder.pack, nil
}

// Build is a convenience wrapper. For large atlas sets prefer New/Add/Finish
// to release input datasets between additions.
func Build(ctx context.Context, inputs []Input, options Options) (Pack, error) {
	builder, err := New(options)
	if err != nil {
		return Pack{}, err
	}
	for _, input := range inputs {
		if err := builder.Add(ctx, input); err != nil {
			return Pack{}, err
		}
	}
	return builder.Finish()
}

func summarize(sources []Source, units []Unit) Quality {
	report := Quality{SchemaVersion: SchemaVersion, SourcesCount: len(sources), UnitsCount: len(units),
		UnitsByKind: map[string]int{}, Limitations: append([]string(nil), limitations...)}
	for _, unit := range units {
		report.UnitsByKind[unit.Kind]++
		report.TextBytes += len(unit.Text)
		if unit.MetadataOnly {
			report.MetadataOnlyUnits++
		}
		if unit.Truncated {
			report.TruncatedUnits++
		}
		if len(unit.Citations) == 0 {
			report.UncitedUnits++
		}
		if unit.LicenseExpression == "" {
			report.UnknownLicenseUnits++
		}
	}
	return report
}

func unitID(unit Unit) string {
	return rkcmodel.StableID("knowledge_unit", unit.SourceID, unit.Kind, unit.ObjectID, unit.SectionID)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func truncateUTF8(value string, maximum int) (string, bool) {
	if len(value) <= maximum {
		return value, false
	}
	end := maximum
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	// Retaining a short substring would pin the complete source body in memory
	// across sequential atlas additions, defeating the retained-text budget.
	return strings.Clone(value[:end]), true
}
