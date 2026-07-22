package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const (
	writerProjectionReservedAttribute = "_rkc_projection"
	writerProjectionSourcePath        = "source_path"
	writerProjectionSourceAnchor      = "source_anchor"
	writerProjectionParentNodeID      = "parent_node_id"
)

// writerProjectBundle materializes every normalized table used as a canonical
// read/search projection. The canonical_* tables remain source truth. The v4
// schema only permits legacy_projection_status='complete', so any value that
// the legacy projection cannot represent fails the enclosing publication
// transaction instead of publishing an empty or knowingly partial projection.
func writerProjectBundle(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build writerBuildRecord,
	bundle rkcmodel.Bundle,
	coverage rkcmodel.Coverage,
) error {
	snapshot := bundle.Snapshot
	snapshotID := rkcstore.SnapshotID(snapshot.ID)
	snapshotMetadata, err := writerProjectionJSON(operation, build.id, snapshotID, "snapshot.metadata", snapshot.Metadata)
	if err != nil {
		return err
	}
	if err := writerProjectionExec(
		ctx, transaction, operation, build.id, snapshotID, "snapshot",
		`INSERT INTO snapshots(
		   snapshot_id, repository_id, parent_snapshot_id, schema_version,
		   content_digest, commit_sha, ref_name, dirty, created_at,
		   tool_name, tool_version, config_digest, status, metadata_json
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'complete', ?)`,
		snapshot.ID,
		snapshot.RepositoryID,
		writerNullableString(snapshot.ParentSnapshotID),
		snapshot.SchemaVersion,
		snapshot.ContentDigest,
		writerNullableString(snapshot.Git.Commit),
		writerNullableString(snapshot.Git.Branch),
		writerBool(snapshot.Git.Dirty),
		writerTimestamp(snapshot.CreatedAt),
		snapshot.Tool.Name,
		snapshot.Tool.Version,
		snapshot.ConfigDigest,
		snapshotMetadata,
	); err != nil {
		return err
	}

	if err := writerProjectLogicalEntities(ctx, transaction, operation, build, bundle); err != nil {
		return err
	}
	for _, artifact := range bundle.Artifacts {
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "artifact.attributes", artifact.Attributes)
		if err != nil {
			return err
		}
		exclusion := artifact.DispositionReason
		if exclusion == "" {
			exclusion = artifact.ExclusionReason
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "artifact",
			`INSERT INTO artifacts(
			   snapshot_id, artifact_id, logical_artifact_id, path, kind,
			   language, media_type, size_bytes, content_sha256, line_count,
			   is_text, status, exclusion_reason, generated_classification,
			   vendor_classification, license_expression, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			artifact.ID,
			writerNullableString(artifact.LogicalID),
			artifact.Path,
			artifact.Kind,
			writerNullableString(artifact.Language),
			writerNullableString(artifact.MediaType),
			artifact.SizeBytes,
			writerNullableString(artifact.SHA256),
			artifact.LineCount,
			writerBool(artifact.Text),
			artifact.Status,
			writerNullableString(exclusion),
			writerOptionalBool(artifact.Generated),
			writerOptionalBool(artifact.Vendored),
			writerNullableString(artifact.LicenseExpression),
			attributes,
		); err != nil {
			return err
		}
	}

	for _, evidence := range bundle.Evidence {
		attributes, err := writerProjectionAttributes(
			operation, build.id, snapshotID, "evidence.attributes",
			evidence.Attributes, writerProjectionSourceAttributes(evidence.Source),
		)
		if err != nil {
			return err
		}
		source := writerProjectionSource(evidence.Source)
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "evidence",
			`INSERT INTO evidence(
			   snapshot_id, evidence_id, kind, method, confidence, artifact_id,
			   start_byte, end_byte, start_line, start_column, end_line,
			   end_column, tool_id, tool_version, detail, input_digest,
			   attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			evidence.ID,
			evidence.Kind,
			evidence.Method,
			evidence.Confidence,
			source.artifactID,
			source.startByte,
			source.endByte,
			source.startLine,
			source.startColumn,
			source.endLine,
			source.endColumn,
			writerNullableString(evidence.Tool),
			writerNullableString(evidence.ToolVersion),
			writerNullableString(evidence.Detail),
			writerNullableString(evidence.InputDigest),
			attributes,
		); err != nil {
			return err
		}
	}

	for _, node := range bundle.Nodes {
		attributes, err := writerProjectionAttributes(
			operation, build.id, snapshotID, "node.attributes",
			node.Attributes, writerProjectionSourceAttributes(node.Source),
		)
		if err != nil {
			return err
		}
		source := writerProjectionSource(node.Source)
		artifactID := writerNullableString(node.ArtifactID)
		if node.ArtifactID == "" {
			artifactID = source.artifactID
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "node",
			`INSERT INTO nodes(
			   snapshot_id, node_id, logical_id, kind, name, qualified_name,
			   signature, language, visibility, artifact_id, start_byte,
			   end_byte, start_line, start_column, end_line, end_column,
			   semantic_hash, public_surface, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			node.ID,
			writerNullableString(node.LogicalID),
			node.Kind,
			node.Name,
			writerNullableString(node.QualifiedName),
			writerNullableString(node.Signature),
			writerNullableString(node.Language),
			writerNullableString(node.Visibility),
			artifactID,
			source.startByte,
			source.endByte,
			source.startLine,
			source.startColumn,
			source.endLine,
			source.endColumn,
			writerNullableString(node.SemanticHash),
			writerBool(node.PublicSurface),
			attributes,
		); err != nil {
			return err
		}
		for _, evidenceID := range writerSortedStrings(node.EvidenceIDs) {
			if err := writerProjectionExec(
				ctx, transaction, operation, build.id, snapshotID, "node_evidence",
				`INSERT INTO node_evidence(snapshot_id, node_id, evidence_id, role)
				 VALUES (?, ?, ?, 'supports')`,
				snapshot.ID, node.ID, evidenceID,
			); err != nil {
				return err
			}
		}
	}

	for _, edge := range bundle.Edges {
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "edge.attributes", edge.Attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "edge",
			`INSERT INTO edges(
			   snapshot_id, edge_id, kind, from_node_id, to_node_id,
			   resolution, confidence, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			edge.ID,
			edge.Kind,
			edge.From,
			edge.To,
			rkcmodel.NormalizeResolution(edge.Resolution),
			edge.Confidence,
			attributes,
		); err != nil {
			return err
		}
		for _, evidenceID := range writerSortedStrings(edge.EvidenceIDs) {
			if err := writerProjectionExec(
				ctx, transaction, operation, build.id, snapshotID, "edge_evidence",
				`INSERT INTO edge_evidence(snapshot_id, edge_id, evidence_id, role)
				 VALUES (?, ?, ?, 'supports')`,
				snapshot.ID, edge.ID, evidenceID,
			); err != nil {
				return err
			}
		}
	}

	if err := writerProjectDocuments(ctx, transaction, operation, build, bundle.Documents, bundle.Nodes); err != nil {
		return err
	}
	for _, diagnostic := range bundle.Diagnostics {
		attributes, err := writerProjectionAttributes(
			operation, build.id, snapshotID, "diagnostic.attributes",
			diagnostic.Attributes, writerProjectionSourceAttributes(diagnostic.Source),
		)
		if err != nil {
			return err
		}
		source := writerProjectionSource(diagnostic.Source)
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "diagnostic",
			`INSERT INTO diagnostics(
			   snapshot_id, diagnostic_id, severity, code, message, artifact_id,
			   start_line, start_column, stage, plugin_id, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			diagnostic.ID,
			diagnostic.Severity,
			diagnostic.Code,
			diagnostic.Message,
			source.artifactID,
			source.startLine,
			source.startColumn,
			writerNullableString(diagnostic.Stage),
			writerNullableString(diagnostic.Plugin),
			attributes,
		); err != nil {
			return err
		}
	}

	for _, conflict := range bundle.Conflicts {
		candidates, err := writerProjectionJSON(operation, build.id, snapshotID, "conflict.candidate_ids", writerSortedStrings(conflict.CandidateIDs))
		if err != nil {
			return err
		}
		evidenceIDs, err := writerProjectionJSON(operation, build.id, snapshotID, "conflict.evidence_ids", writerSortedStrings(conflict.EvidenceIDs))
		if err != nil {
			return err
		}
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "conflict.attributes", conflict.Attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "conflict",
			`INSERT INTO conflicts(
			   snapshot_id, conflict_id, kind, subject_id, preferred_id,
			   resolution, candidate_ids_json, evidence_ids_json, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			conflict.ID,
			conflict.Kind,
			conflict.SubjectID,
			writerNullableString(conflict.PreferredID),
			writerNullableString(conflict.Resolution),
			candidates,
			evidenceIDs,
			attributes,
		); err != nil {
			return err
		}
	}

	for _, claim := range bundle.Claims {
		evidenceIDs, err := writerProjectionJSON(operation, build.id, snapshotID, "claim.evidence_ids", writerSortedStrings(claim.EvidenceIDs))
		if err != nil {
			return err
		}
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "claim.attributes", claim.Attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "claim",
			`INSERT INTO claims(
			   snapshot_id, claim_id, subject_id, text, category, certainty,
			   generator, generator_version, validation, evidence_ids_json,
			   attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			claim.ID,
			claim.SubjectID,
			claim.Text,
			writerNullableString(claim.Category),
			claim.Certainty,
			claim.Generator,
			writerNullableString(claim.GeneratorVersion),
			claim.Validation,
			evidenceIDs,
			attributes,
		); err != nil {
			return err
		}
	}

	for _, path := range bundle.Paths {
		nodeIDs, err := writerProjectionJSON(operation, build.id, snapshotID, "path.node_ids", path.NodeIDs)
		if err != nil {
			return err
		}
		edgeIDs, err := writerProjectionJSON(operation, build.id, snapshotID, "path.edge_ids", path.EdgeIDs)
		if err != nil {
			return err
		}
		evidenceIDs, err := writerProjectionJSON(operation, build.id, snapshotID, "path.evidence_ids", writerSortedStrings(path.EvidenceIDs))
		if err != nil {
			return err
		}
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "path.attributes", path.Attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "execution_path",
			`INSERT INTO execution_paths(
			   snapshot_id, path_id, name, entry_node_id, exit_node_id,
			   node_ids_json, edge_ids_json, evidence_ids_json, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshot.ID,
			path.ID,
			path.Name,
			path.EntryNodeID,
			writerNullableString(path.ExitNodeID),
			nodeIDs,
			edgeIDs,
			evidenceIDs,
			attributes,
		); err != nil {
			return err
		}
	}

	coverageJSON, err := writerProjectionJSON(operation, build.id, snapshotID, "coverage", coverage)
	if err != nil {
		return err
	}
	if err := writerProjectionExec(
		ctx, transaction, operation, build.id, snapshotID, "coverage",
		`INSERT INTO coverage_records(
		   snapshot_id, content_json, deterministic_output_digest
		 ) VALUES (?, ?, ?)`,
		snapshot.ID,
		coverageJSON,
		coverage.DeterministicOutputDigest,
	); err != nil {
		return err
	}
	return writerProjectFTS(ctx, transaction, operation, build, bundle)
}

type writerLogicalEntity struct {
	id         string
	kind       string
	name       string
	basis      string
	attributes any
}

func writerProjectLogicalEntities(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build writerBuildRecord,
	bundle rkcmodel.Bundle,
) error {
	snapshotID := rkcstore.SnapshotID(bundle.Snapshot.ID)
	entities := make(map[string]writerLogicalEntity)
	add := func(entity writerLogicalEntity) error {
		if entity.id == "" {
			return nil
		}
		if previous, exists := entities[entity.id]; exists && (previous.kind != entity.kind || previous.name != entity.name) {
			return writerOperationError(
				rkcstore.CodeValidation,
				operation,
				build.id,
				snapshotID,
				"legacy_projection",
				fmt.Errorf("logical id %q maps to incompatible entities", entity.id),
			)
		}
		entities[entity.id] = entity
		return nil
	}
	for _, artifact := range bundle.Artifacts {
		if err := add(writerLogicalEntity{
			id: artifact.LogicalID, kind: artifact.Kind, name: artifact.Path,
			basis: "artifact.logical_id", attributes: artifact.Attributes,
		}); err != nil {
			return err
		}
	}
	for _, node := range bundle.Nodes {
		name := node.QualifiedName
		if name == "" {
			name = node.Name
		}
		if err := add(writerLogicalEntity{
			id: node.LogicalID, kind: node.Kind, name: name,
			basis: "node.logical_id", attributes: node.Attributes,
		}); err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(entities))
	for id := range entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entity := entities[id]
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "logical_entity.attributes", entity.attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "logical_entity",
			`INSERT INTO logical_entities(
			   repository_id, logical_id, kind, canonical_name,
			   first_snapshot_id, last_snapshot_id, identity_basis,
			   attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(repository_id, logical_id) DO UPDATE SET
			   kind = excluded.kind,
			   canonical_name = excluded.canonical_name,
			   last_snapshot_id = excluded.last_snapshot_id,
			   identity_basis = excluded.identity_basis,
			   attributes_json = excluded.attributes_json`,
			bundle.Snapshot.RepositoryID,
			entity.id,
			entity.kind,
			entity.name,
			bundle.Snapshot.ID,
			bundle.Snapshot.ID,
			entity.basis,
			attributes,
		); err != nil {
			return err
		}
	}
	return nil
}

func writerProjectDocuments(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build writerBuildRecord,
	documents []rkcmodel.Document,
	nodes []rkcmodel.Node,
) error {
	snapshotID := rkcstore.SnapshotID("")
	if len(documents) > 0 {
		// Every document belongs to the build's future snapshot; the exact ID is
		// carried by the records and supplied by the caller below.
		var id string
		if err := transaction.QueryRowContext(
			ctx,
			"SELECT snapshot_id FROM canonical_snapshots WHERE build_id = ?",
			build.id,
		).Scan(&id); err != nil {
			return writerDatabaseError(operation, "canonical_snapshot", build.id, "", err)
		}
		snapshotID = rkcstore.SnapshotID(id)
	}
	nodeIDs := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		nodeIDs[node.ID] = struct{}{}
	}
	seenSections := make(map[string]struct{})
	sectionsByDocument := make(map[string]map[string]struct{}, len(documents))
	for _, document := range documents {
		if document.Status == "stale" {
			return writerOperationError(
				rkcstore.CodeValidation,
				operation,
				build.id,
				snapshotID,
				"legacy_projection",
				errors.New("SQLite v0.4 legacy documents cannot represent canonical status stale"),
			)
		}
		documentSections := make(map[string]struct{}, len(document.Sections))
		for _, section := range document.Sections {
			if _, duplicate := documentSections[section.ID]; duplicate {
				return writerOperationError(
					rkcstore.CodeValidation,
					operation,
					build.id,
					snapshotID,
					"legacy_projection",
					fmt.Errorf("document %q contains duplicate section id %q", document.ID, section.ID),
				)
			}
			if _, duplicate := seenSections[section.ID]; duplicate {
				return writerOperationError(
					rkcstore.CodeValidation,
					operation,
					build.id,
					snapshotID,
					"legacy_projection",
					fmt.Errorf("SQLite v0.4 requires snapshot-global document section id %q", section.ID),
				)
			}
			documentSections[section.ID] = struct{}{}
			seenSections[section.ID] = struct{}{}
		}
		sectionsByDocument[document.ID] = documentSections
	}
	for _, document := range documents {
		attributes, err := writerProjectionJSON(operation, build.id, snapshotID, "document.attributes", document.Attributes)
		if err != nil {
			return err
		}
		if err := writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "document",
			`INSERT INTO documents(
			   snapshot_id, document_id, logical_document_id, kind, title, path,
			   generator, generator_version, content_sha256, status, attributes_json
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshotID,
			document.ID,
			writerNullableString(document.LogicalID),
			document.Kind,
			document.Title,
			writerNullableString(document.Path),
			document.Generator,
			document.GeneratorVersion,
			document.ContentSHA256,
			document.Status,
			attributes,
		); err != nil {
			return err
		}
		for _, section := range document.Sections {
			var reserved map[string]any
			if section.ParentID != "" {
				if _, sectionParent := sectionsByDocument[document.ID][section.ParentID]; !sectionParent {
					if _, nodeParent := nodeIDs[section.ParentID]; !nodeParent {
						return writerOperationError(
							rkcstore.CodeValidation,
							operation,
							build.id,
							snapshotID,
							"legacy_projection",
							fmt.Errorf("document %q section %q has unrepresentable parent %q", document.ID, section.ID, section.ParentID),
						)
					}
					reserved = map[string]any{writerProjectionParentNodeID: section.ParentID}
				}
			}
			attributes, err := writerProjectionAttributes(
				operation, build.id, snapshotID, "document_section.attributes",
				section.Attributes, reserved,
			)
			if err != nil {
				return err
			}
			contentDigest := writerProjectionTextDigest(section.Markdown, section.PlainText)
			if err := writerProjectionExec(
				ctx, transaction, operation, build.id, snapshotID, "document_section",
				`INSERT INTO document_sections(
				   snapshot_id, section_id, document_id, parent_section_id,
				   ordinal, heading, body_markdown, body_text, content_sha256,
				   attributes_json
				 ) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
				snapshotID,
				section.ID,
				document.ID,
				section.Ordinal,
				writerNullableString(section.Heading),
				section.Markdown,
				section.PlainText,
				contentDigest,
				attributes,
			); err != nil {
				return err
			}
			text := section.PlainText
			if text == "" {
				text = section.Markdown
			}
			chunkMetadata, err := writerProjectionJSON(
				operation,
				build.id,
				snapshotID,
				"chunk.metadata",
				map[string]any{
					"heading":      section.Heading,
					"claim_ids":    writerSortedStrings(section.ClaimIDs),
					"evidence_ids": writerSortedStrings(section.EvidenceIDs),
				},
			)
			if err != nil {
				return err
			}
			if err := writerProjectionExec(
				ctx, transaction, operation, build.id, snapshotID, "chunk",
				`INSERT INTO chunks(
				   snapshot_id, chunk_id, document_id, section_id, node_id,
				   ordinal, token_count, text, content_sha256, metadata_json
				 ) VALUES (?, ?, ?, ?, NULL, ?, NULL, ?, ?, ?)`,
				snapshotID,
				rkcmodel.StableID("chunk", string(snapshotID), document.ID, section.ID),
				document.ID,
				section.ID,
				section.Ordinal,
				text,
				writerProjectionTextDigest(text),
				chunkMetadata,
			); err != nil {
				return err
			}
			claimKey, err := writerProjectionJSON(operation, build.id, snapshotID, "section.claim_ids", writerSortedStrings(section.ClaimIDs))
			if err != nil {
				return err
			}
			for _, evidenceID := range writerSortedStrings(section.EvidenceIDs) {
				if err := writerProjectionExec(
					ctx, transaction, operation, build.id, snapshotID, "section_evidence",
					`INSERT INTO section_evidence(snapshot_id, section_id, evidence_id, claim_key)
					 VALUES (?, ?, ?, ?)`,
					snapshotID, section.ID, evidenceID, claimKey,
				); err != nil {
					return err
				}
			}
		}
		for _, section := range document.Sections {
			if section.ParentID == "" {
				continue
			}
			if _, sectionParent := sectionsByDocument[document.ID][section.ParentID]; !sectionParent {
				// Node parents are preserved in the reserved attributes namespace.
				continue
			}
			if err := writerProjectionExec(
				ctx, transaction, operation, build.id, snapshotID, "document_section_parent",
				`UPDATE document_sections SET parent_section_id = ?
				 WHERE snapshot_id = ? AND section_id = ?`,
				section.ParentID, snapshotID, section.ID,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func writerProjectFTS(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build writerBuildRecord,
	bundle rkcmodel.Bundle,
) error {
	snapshotID := rkcstore.SnapshotID(bundle.Snapshot.ID)
	insert := func(objectType, objectID, title, qualifiedName, signature, body string) error {
		return writerProjectionExec(
			ctx, transaction, operation, build.id, snapshotID, "search_fts",
			`INSERT INTO search_fts(
			   snapshot_id, object_type, object_id, title,
			   qualified_name, signature, body
			 ) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			bundle.Snapshot.ID, objectType, objectID, title, qualifiedName, signature, body,
		)
	}
	for _, artifact := range bundle.Artifacts {
		if err := insert("artifact", artifact.ID, artifact.Path, artifact.Path, "", strings.TrimSpace(artifact.Kind+" "+artifact.Language)); err != nil {
			return err
		}
	}
	for _, node := range bundle.Nodes {
		if err := insert("node", node.ID, node.Name, node.QualifiedName, node.Signature, strings.TrimSpace(node.Kind+" "+node.Language+" "+node.Visibility)); err != nil {
			return err
		}
	}
	for _, document := range bundle.Documents {
		parts := make([]string, 0, len(document.Sections))
		for _, section := range document.Sections {
			text := section.PlainText
			if text == "" {
				text = section.Markdown
			}
			parts = append(parts, section.Heading+"\n"+text)
			if err := insert("document_section", section.ID, section.Heading, document.Title, "", text); err != nil {
				return err
			}
		}
		if err := insert("document", document.ID, document.Title, document.Path, "", strings.Join(parts, "\n")); err != nil {
			return err
		}
	}
	for _, claim := range bundle.Claims {
		if err := insert("claim", claim.ID, claim.Category, claim.SubjectID, "", claim.Text); err != nil {
			return err
		}
	}
	return nil
}

type writerProjectionSourceColumns struct {
	artifactID                      any
	startByte, endByte              any
	startLine, startColumn, endLine any
	endColumn                       any
}

func writerProjectionSource(source *rkcmodel.SourceRange) writerProjectionSourceColumns {
	if source == nil {
		return writerProjectionSourceColumns{}
	}
	columns := writerProjectionSourceColumns{
		artifactID:  writerNullableString(source.ArtifactID),
		startByte:   writerOptionalInt64(source.StartByte),
		endByte:     writerOptionalInt64(source.EndByte),
		startLine:   writerOptionalInt(source.StartLine),
		startColumn: writerOptionalInt(source.StartColumn),
		endLine:     writerOptionalInt(source.EndLine),
		endColumn:   writerOptionalInt(source.EndColumn),
	}
	// A non-zero end byte makes a zero start byte an explicit and valid origin.
	if source.EndByte != 0 {
		columns.startByte = source.StartByte
	}
	// Columns are zero-based. A present line makes column zero meaningful even
	// though the canonical Go value cannot otherwise distinguish it from absent.
	if source.StartLine != 0 {
		columns.startColumn = source.StartColumn
	}
	if source.EndLine != 0 {
		columns.endColumn = source.EndColumn
	}
	return columns
}

func writerProjectionSourceAttributes(source *rkcmodel.SourceRange) map[string]any {
	if source == nil {
		return nil
	}
	return map[string]any{
		writerProjectionSourcePath:   source.Path,
		writerProjectionSourceAnchor: source.Anchor,
	}
}

func writerProjectionAttributes(
	operation string,
	build rkcstore.BuildID,
	snapshot rkcstore.SnapshotID,
	field string,
	attributes map[string]any,
	reserved map[string]any,
) (string, error) {
	if _, collision := attributes[writerProjectionReservedAttribute]; collision {
		return "", writerOperationError(
			rkcstore.CodeValidation,
			operation,
			build,
			snapshot,
			"legacy_projection."+field,
			fmt.Errorf("attribute key %q is reserved for lossless SQLite projection metadata", writerProjectionReservedAttribute),
		)
	}
	if len(reserved) == 0 {
		return writerProjectionJSON(operation, build, snapshot, field, attributes)
	}
	projected := make(map[string]any, len(attributes)+1)
	for key, value := range attributes {
		projected[key] = value
	}
	projected[writerProjectionReservedAttribute] = reserved
	return writerProjectionJSON(operation, build, snapshot, field, projected)
}

func writerProjectionJSON(
	operation string,
	build rkcstore.BuildID,
	snapshot rkcstore.SnapshotID,
	field string,
	value any,
) (string, error) {
	if value == nil {
		value = map[string]any{}
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", writerOperationError(rkcstore.CodeInvalidArgument, operation, build, snapshot, field, err)
	}
	return string(body), nil
}

func writerProjectionExec(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build rkcstore.BuildID,
	snapshot rkcstore.SnapshotID,
	field string,
	statement string,
	arguments ...any,
) error {
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, statement, arguments...); err != nil {
		if writerIsConstraintError(err) {
			return writerOperationError(
				rkcstore.CodeValidation,
				operation,
				build,
				snapshot,
				"legacy_projection."+field,
				err,
			)
		}
		return writerDatabaseError(operation, "legacy_projection."+field, build, snapshot, err)
	}
	return nil
}

func writerProjectionTextDigest(values ...string) string {
	hash := sha256.New()
	for index, value := range values {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writerBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func writerOptionalBool(value bool) any {
	if !value {
		return nil
	}
	return "true"
}

func writerOptionalInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func writerOptionalInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
