package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func TestWriterProjectionPreservesSourceMetadataAndZeroOrigins(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("projection-source", "repository", "")
	source := &rkcmodel.SourceRange{
		ArtifactID:  "artifact",
		Path:        "main.go",
		StartByte:   0,
		EndByte:     7,
		StartLine:   1,
		StartColumn: 0,
		EndLine:     1,
		EndColumn:   0,
		Anchor:      "symbol:Alpha",
	}
	bundle.Evidence[0].Source = source
	bundle.Evidence[0].Attributes = map[string]any{"owner": "evidence"}
	nodeSource := *source
	bundle.Nodes[0].Source = &nodeSource
	bundle.Nodes[0].Attributes = map[string]any{"owner": "node"}
	diagnosticSource := *source
	bundle.Diagnostics[0].Source = &diagnosticSource
	bundle.Diagnostics[0].Attributes = map[string]any{"owner": "diagnostic"}

	writerTestCommit(t, database, bundle)

	for _, test := range []struct {
		name  string
		query string
	}{
		{
			name: "evidence",
			query: `SELECT start_byte, start_column, end_column,
			        json_extract(attributes_json, '$._rkc_projection.source_path'),
			        json_extract(attributes_json, '$._rkc_projection.source_anchor'),
			        json_extract(attributes_json, '$.owner')
			        FROM evidence WHERE snapshot_id = 'projection-source' AND evidence_id = 'evidence'`,
		},
		{
			name: "node",
			query: `SELECT start_byte, start_column, end_column,
			        json_extract(attributes_json, '$._rkc_projection.source_path'),
			        json_extract(attributes_json, '$._rkc_projection.source_anchor'),
			        json_extract(attributes_json, '$.owner')
			        FROM nodes WHERE snapshot_id = 'projection-source' AND node_id = 'node-a'`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var startByte, startColumn, endColumn int64
			var path, anchor, owner string
			if err := database.db.QueryRow(test.query).Scan(
				&startByte, &startColumn, &endColumn, &path, &anchor, &owner,
			); err != nil {
				t.Fatal(err)
			}
			if startByte != 0 || startColumn != 0 || endColumn != 0 {
				t.Fatalf("zero origins = byte:%d start-column:%d end-column:%d", startByte, startColumn, endColumn)
			}
			if path != "main.go" || anchor != "symbol:Alpha" || owner != test.name {
				t.Fatalf("projection metadata = path:%q anchor:%q owner:%q", path, anchor, owner)
			}
		})
	}

	var diagnosticColumn int64
	var diagnosticPath, diagnosticAnchor, diagnosticOwner string
	if err := database.db.QueryRow(
		`SELECT start_column,
		        json_extract(attributes_json, '$._rkc_projection.source_path'),
		        json_extract(attributes_json, '$._rkc_projection.source_anchor'),
		        json_extract(attributes_json, '$.owner')
		 FROM diagnostics
		 WHERE snapshot_id = 'projection-source' AND diagnostic_id = 'diagnostic'`,
	).Scan(&diagnosticColumn, &diagnosticPath, &diagnosticAnchor, &diagnosticOwner); err != nil {
		t.Fatal(err)
	}
	if diagnosticColumn != 0 || diagnosticPath != "main.go" ||
		diagnosticAnchor != "symbol:Alpha" || diagnosticOwner != "diagnostic" {
		t.Fatalf(
			"diagnostic projection = column:%d path:%q anchor:%q owner:%q",
			diagnosticColumn, diagnosticPath, diagnosticAnchor, diagnosticOwner,
		)
	}
}

func TestWriterProjectionScopesSectionParentsToTheirDocument(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("projection-parents", "repository", "")
	bundle.Documents[0].Sections = []rkcmodel.DocumentSection{
		{ID: "local-root", Ordinal: 0, PlainText: "Root"},
		{ID: "local-child", ParentID: "local-root", Ordinal: 1, PlainText: "Local child"},
		{ID: "node-child", ParentID: "node-a", Ordinal: 2, PlainText: "Node child"},
	}
	bundle.Documents = append(bundle.Documents, rkcmodel.Document{
		ID: "document-two", Kind: "reference", Title: "Second",
		Generator: "writer-test", Status: "validated",
		Sections: []rkcmodel.DocumentSection{{ID: "node-a", Ordinal: 0, PlainText: "Other document"}},
	})

	writerTestCommit(t, database, bundle)

	var sectionParent, nodeParent sql.NullString
	if err := database.db.QueryRow(
		`SELECT parent_section_id,
		        json_extract(attributes_json, '$._rkc_projection.parent_node_id')
		 FROM document_sections
		 WHERE snapshot_id = 'projection-parents' AND section_id = 'local-child'`,
	).Scan(&sectionParent, &nodeParent); err != nil {
		t.Fatal(err)
	}
	if !sectionParent.Valid || sectionParent.String != "local-root" || nodeParent.Valid {
		t.Fatalf("local parent = section:%+v node:%+v", sectionParent, nodeParent)
	}

	if err := database.db.QueryRow(
		`SELECT parent_section_id,
		        json_extract(attributes_json, '$._rkc_projection.parent_node_id')
		 FROM document_sections
		 WHERE snapshot_id = 'projection-parents' AND section_id = 'node-child'`,
	).Scan(&sectionParent, &nodeParent); err != nil {
		t.Fatal(err)
	}
	if sectionParent.Valid || !nodeParent.Valid || nodeParent.String != "node-a" {
		t.Fatalf("node parent = section:%+v node:%+v", sectionParent, nodeParent)
	}
}

func TestWriterProjectionRejectsReservedAttributeCollision(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("projection-collision", "repository", "")
	bundle.Evidence[0].Attributes = map[string]any{
		writerProjectionReservedAttribute: map[string]any{"source_path": "spoofed.go"},
	}
	build := writerTestStage(t, database, bundle, true)
	if err := database.Commit(context.Background(), build, bundle.Snapshot); !errors.Is(err, rkcstore.ErrValidation) {
		t.Fatalf("Commit = %v, want reserved-attribute validation failure", err)
	}
	var canonical int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM canonical_snapshots").Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical != 0 {
		t.Fatalf("reserved-attribute collision published %d canonical snapshots", canonical)
	}
}

func TestWriterProjectionRejectsCrossDocumentSectionIDCollision(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("projection-section-collision", "repository", "")
	bundle.Documents = append(bundle.Documents, rkcmodel.Document{
		ID: "document-two", Kind: "reference", Title: "Second",
		Generator: "writer-test", Status: "validated",
		Sections: []rkcmodel.DocumentSection{{ID: "section", Ordinal: 0, PlainText: "Duplicate"}},
	})
	build := writerTestStage(t, database, bundle, true)
	if err := database.Commit(context.Background(), build, bundle.Snapshot); !errors.Is(err, rkcstore.ErrValidation) {
		t.Fatalf("Commit = %v, want schema-representation validation failure", err)
	}
}
