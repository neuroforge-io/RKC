package docparse

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type sourceBindingFixture struct {
	Document  rkcmodel.Document
	Artifacts map[string]rkcmodel.Artifact
	Nodes     map[string]rkcmodel.Node
	Evidence  map[string]rkcmodel.Evidence
}

func newSourceBindingFixture(t *testing.T, name, language, body string) sourceBindingFixture {
	t.Helper()
	root := t.TempDir()
	file := sourceTestFile(t, root, name, language, body)
	result, err := inventory.Scan(inventory.Options{Root: root})
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("source binding inventory: %+v, %v", result, err)
	}
	artifact := result.Artifacts[0]
	file.ArtifactID = artifact.ID
	fragment, err := ExtractSources(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{file}})
	if err != nil || len(fragment.Documents) != 1 {
		t.Fatalf("source binding extraction: %+v, %v", fragment, err)
	}
	fixture := sourceBindingFixture{Document: fragment.Documents[0], Artifacts: map[string]rkcmodel.Artifact{artifact.ID: artifact}, Nodes: map[string]rkcmodel.Node{}, Evidence: map[string]rkcmodel.Evidence{}}
	for _, node := range fragment.Nodes {
		fixture.Nodes[node.ID] = node
	}
	for _, evidence := range fragment.Evidence {
		fixture.Evidence[evidence.ID] = evidence
	}
	return fixture
}

func (fixture sourceBindingFixture) references(ctx context.Context) (*rkcmodel.SourceRange, []string) {
	return SourceDocumentReferences(ctx, fixture.Document, fixture.Artifacts, fixture.Nodes, fixture.Evidence)
}

func TestSourceBindingsTravelWithoutOriginalFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, language, body string }{
		{"notes.txt", "text", "Café research notes\r\nKeep source provenance.\r\n"},
		{"events.log", "log", "an unverified source event with no trailing newline"},
		{"guide.rst", "rst", ".. include:: forbidden-source-read\n"},
		{"records.ndjson", "jsonl", "{\"a\":9007199254740993}\n\nmalformed export line\n{\"a\":2}\n"},
		{"records.csv", "csv", "name,password,notes\r\nAlice,fictional-private,\"multi\r\nline\"\r\nBob,other-private,ready\r\n"},
		{"records.tsv", "tsv", "name\tnotes\nAlice\tfirst\nBob\tsecond\n"},
		{"broken.csv", "csv", "name,password\nAlice,fictional-private\nBob,\"unclosed\nremaining suffix"},
		{"wide.jsonl", "jsonl", "{\"note\":\"" + strings.Repeat("x", MaximumSourceSectionBytes) + "\"}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceBindingFixture(t, test.name, test.language, test.body)
			whole, ids := fixture.references(context.Background())
			if whole == nil || whole.Path != test.name || whole.StartByte != 0 || whole.EndByte != int64(len(test.body)) || len(ids) != len(fixture.Document.Sections)+1 || !sort.StringsAreSorted(ids) {
				t.Fatalf("native source references = %+v, %v", whole, ids)
			}
			encoded, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			var moved sourceBindingFixture
			if err := json.Unmarshal(encoded, &moved); err != nil {
				t.Fatal(err)
			}
			otherWhole, otherIDs := moved.references(context.Background())
			if !reflect.DeepEqual(otherWhole, whole) || !reflect.DeepEqual(otherIDs, ids) {
				t.Fatalf("JSON roundtrip lost source provenance: %+v, %v; want %+v, %v", otherWhole, otherIDs, whole, ids)
			}
			// Native Go consumers may retain int64 attributes, without any
			// string coercion or fuzzy float matching in the source boundary.
			for index := range fixture.Document.Sections {
				for _, key := range []string{"start_byte", "end_byte", "start_line", "end_line", "record_index"} {
					value, ok := fixture.Document.Sections[index].Attributes[key].(int)
					if ok {
						fixture.Document.Sections[index].Attributes[key] = int64(value)
					}
				}
			}
			intWhole, intIDs := fixture.references(context.Background())
			if !reflect.DeepEqual(intWhole, whole) || !reflect.DeepEqual(intIDs, ids) {
				t.Fatal("exact int64 source receipt lost references")
			}
		})
	}
}

func TestSourceBindingsRejectIncompleteOrForeignOwnership(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*sourceBindingFixture)
	}{
		{"foreign producer", func(f *sourceBindingFixture) { f.Document.Generator = "some.other.producer" }},
		{"foreign producer version", func(f *sourceBindingFixture) { f.Document.GeneratorVersion = "future-unknown" }},
		{"foreign evidence version", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].EvidenceIDs[0]
			e := f.Evidence[id]
			e.ToolVersion = "0.1.0"
			f.Evidence[id] = e
		}},
		{"wrong document logical identity", func(f *sourceBindingFixture) { f.Document.LogicalID = "foreign-logical-document" }},
		{"wrong subject logical identity", func(f *sourceBindingFixture) {
			id := f.Document.SubjectIDs[0]
			n := f.Nodes[id]
			n.LogicalID = "foreign-logical-subject"
			f.Nodes[id] = n
		}},
		{"wrong section logical identity", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].ID
			n := f.Nodes[id]
			n.LogicalID = "foreign-logical-section"
			f.Nodes[id] = n
		}},
		{"subject alias map key", func(f *sourceBindingFixture) {
			id := f.Document.SubjectIDs[0]
			f.Document.SubjectIDs[0] = "alias-subject"
			f.Nodes["alias-subject"] = f.Nodes[id]
		}},
		{"stale document", func(f *sourceBindingFixture) { f.Document.Status = "stale" }},
		{"ambiguous subjects", func(f *sourceBindingFixture) {
			f.Document.SubjectIDs = append(f.Document.SubjectIDs, "another-subject")
		}},
		{"no sections", func(f *sourceBindingFixture) { f.Document.Sections = nil }},
		{"missing whole node", func(f *sourceBindingFixture) { delete(f.Nodes, f.Document.SubjectIDs[0]) }},
		{"missing whole evidence", func(f *sourceBindingFixture) { delete(f.Evidence, f.Nodes[f.Document.SubjectIDs[0]].EvidenceIDs[0]) }},
		{"missing unselected section evidence", func(f *sourceBindingFixture) { delete(f.Evidence, f.Document.Sections[2].EvidenceIDs[0]) }},
		{"missing artifact", func(f *sourceBindingFixture) { f.Artifacts = nil }},
		{"binary artifact", func(f *sourceBindingFixture) {
			id := f.Document.Attributes["artifact_id"].(string)
			a := f.Artifacts[id]
			a.Text = false
			f.Artifacts[id] = a
		}},
		{"wrong artifact digest", func(f *sourceBindingFixture) { f.Document.Attributes["source_sha256"] = strings.Repeat("0", 64) }},
		{"unrecognized source format", func(f *sourceBindingFixture) {
			id := f.Document.Attributes["artifact_id"].(string)
			a := f.Artifacts[id]
			a.Language = "python"
			f.Artifacts[id] = a
		}},
		{"invalid artifact digest", func(f *sourceBindingFixture) {
			id := f.Document.Attributes["artifact_id"].(string)
			a := f.Artifacts[id]
			a.SHA256 = "not-a-digest"
			f.Artifacts[id] = a
			f.Document.Attributes["source_sha256"] = a.SHA256
		}},
		{"source column drift", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].ID
			n := f.Nodes[id]
			source := *n.Source
			source.StartColumn = 1
			n.Source = &source
			f.Nodes[id] = n
		}},
		{"foreign evidence tool", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].EvidenceIDs[0]
			e := f.Evidence[id]
			e.Tool = "foreign-tool"
			f.Evidence[id] = e
		}},
		{"foreign evidence method", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].EvidenceIDs[0]
			e := f.Evidence[id]
			e.Method = "runtime.observed"
			f.Evidence[id] = e
		}},
		{"weakened producer confidence", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].EvidenceIDs[0]
			e := f.Evidence[id]
			e.Confidence = .1
			f.Evidence[id] = e
		}},
		{"missing section source", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].ID
			n := f.Nodes[id]
			n.Source = nil
			f.Nodes[id] = n
		}},
		{"foreign section parent", func(f *sourceBindingFixture) { f.Document.Sections[0].ParentID = "another-document" }},
		{"fractional byte receipt", func(f *sourceBindingFixture) { f.Document.Sections[0].Attributes["end_byte"] = 1.5 }},
		{"string byte receipt", func(f *sourceBindingFixture) { f.Document.Sections[0].Attributes["start_byte"] = "0" }},
		{"wrong section ordinal", func(f *sourceBindingFixture) { f.Document.Sections[0].Ordinal = 1 }},
		{"unbounded source projection", func(f *sourceBindingFixture) {
			f.Document.Sections[0].PlainText = strings.Repeat("x", MaximumSourceSectionBytes+1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceBindingFixture(t, "records.jsonl", "jsonl", "{\"policy\":\"first\"}\nmalformed but retained\n{\"policy\":\"last\"}\n")
			test.mutate(&fixture)
			if whole, ids := fixture.references(context.Background()); whole != nil || len(ids) != 0 {
				t.Fatalf("%s retained citations: %+v, %v", test.name, whole, ids)
			}
		})
	}
}

// convertSourceBindingToV01 reconstructs the original producer's complete
// identity roster. Changing the version alone must never admit new IDs as old.
func convertSourceBindingToV01(fixture *sourceBindingFixture) {
	document := &fixture.Document
	document.GeneratorVersion = "0.1.0"
	document.ID = rkcmodel.StableID("document", SourcePluginID, document.Path)
	document.LogicalID = rkcmodel.StableID("logical-document", SourcePluginID, document.Path)
	oldSubject := document.SubjectIDs[0]
	whole := fixture.Nodes[oldSubject]
	delete(fixture.Nodes, oldSubject)
	whole.ID = rkcmodel.StableID("node", SourcePluginID, document.Path)
	whole.LogicalID = rkcmodel.StableID("logical", SourcePluginID, document.Path)
	document.SubjectIDs = []string{whole.ID}
	fixture.Nodes[whole.ID] = whole
	for index := range document.Sections {
		section := &document.Sections[index]
		node := fixture.Nodes[section.ID]
		delete(fixture.Nodes, section.ID)
		start, end := strconv.FormatInt(node.Source.StartByte, 10), strconv.FormatInt(node.Source.EndByte, 10)
		node.ID = rkcmodel.StableID("node", SourcePluginID, document.Path, start, end)
		node.LogicalID = rkcmodel.StableID("logical", SourcePluginID, document.Path, start, end)
		section.ID, section.ParentID = node.ID, whole.ID
		fixture.Nodes[node.ID] = node
	}
	for id, evidence := range fixture.Evidence {
		evidence.ToolVersion = "0.1.0"
		fixture.Evidence[id] = evidence
	}
}

func TestLegacySourceBindingsRequireExactV01ReceiptsAndOriginalPath(t *testing.T) {
	t.Parallel()
	fixture := newSourceBindingFixture(t, "records.jsonl", "jsonl", "{\"policy\":\"first\"}\nmalformed source\n{\"policy\":\"last\"}\n")
	convertSourceBindingToV01(&fixture)
	whole, ids := fixture.references(context.Background())
	if whole == nil || whole.Path != "records.jsonl" || len(ids) != len(fixture.Document.Sections)+1 {
		t.Fatalf("v0.1 original producer roster lost references: %+v, %v", whole, ids)
	}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*sourceBindingFixture)
	}{
		{"unchanged JSON import", func(*sourceBindingFixture) {}},
		{"unknown version", func(f *sourceBindingFixture) { f.Document.GeneratorVersion = "0.1.1" }},
		{"mixed evidence version", func(f *sourceBindingFixture) {
			id := f.Document.Sections[0].EvidenceIDs[0]
			evidence := f.Evidence[id]
			evidence.ToolVersion = SourcePluginVersion
			f.Evidence[id] = evidence
		}},
		{"original path unavailable", func(f *sourceBindingFixture) {
			id := f.Document.Attributes["artifact_id"].(string)
			artifact := f.Artifacts[id]
			artifact.Path = "[REDACTED].jsonl"
			f.Artifacts[id] = artifact
			f.Document.Path, f.Document.Title = artifact.Path, artifact.Path
			for id, node := range f.Nodes {
				source := *node.Source
				source.Path = artifact.Path
				node.Source = &source
				if node.Kind == "document" {
					node.Name, node.QualifiedName = artifact.Path, artifact.Path
				} else {
					node.QualifiedName = artifact.Path + "#bytes-" + strconv.FormatInt(source.StartByte, 10) + "-" + strconv.FormatInt(source.EndByte, 10)
				}
				f.Nodes[id] = node
			}
			for id, evidence := range f.Evidence {
				source := *evidence.Source
				source.Path = artifact.Path
				evidence.Source = &source
				f.Evidence[id] = evidence
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var imported sourceBindingFixture
			if err := json.Unmarshal(encoded, &imported); err != nil {
				t.Fatal(err)
			}
			test.mutate(&imported)
			otherWhole, otherIDs := imported.references(context.Background())
			if test.name == "unchanged JSON import" {
				if !reflect.DeepEqual(otherWhole, whole) || !reflect.DeepEqual(otherIDs, ids) {
					t.Fatalf("legacy JSON receipts lost references: %+v, %v", otherWhole, otherIDs)
				}
			} else if otherWhole != nil || len(otherIDs) != 0 {
				t.Fatalf("inconsistent legacy receipts retained references: %+v, %v", otherWhole, otherIDs)
			}
		})
	}
}

func TestSourceProducerVersionCannotRelabelIdentityFormula(t *testing.T) {
	t.Parallel()
	fixture := newSourceBindingFixture(t, "notes.txt", "text", "Fictional lantern notes.\n")
	fixture.Document.GeneratorVersion = "0.1.0"
	for id, evidence := range fixture.Evidence {
		evidence.ToolVersion = "0.1.0"
		fixture.Evidence[id] = evidence
	}
	if whole, ids := fixture.references(context.Background()); whole != nil || len(ids) != 0 {
		t.Fatalf("v0.2 IDs accepted as a v0.1 roster: %+v, %v", whole, ids)
	}
}

func TestSourceBindingsRejectNonOpaqueArtifactIdentity(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{"artifact-private-path.txt", "rkc:node:" + strings.Repeat("a", 24), "rkc:artifact:" + strings.Repeat("A", 24), "rkc:artifact:" + strings.Repeat("a", 64)} {
		t.Run(identity, func(t *testing.T) {
			root := t.TempDir()
			file := sourceTestFile(t, root, "notes.txt", "text", "Fictional lantern notes.\n")
			inventoryResult, err := inventory.Scan(inventory.Options{Root: root})
			if err != nil || len(inventoryResult.Artifacts) != 1 {
				t.Fatalf("fixture inventory: %v", err)
			}
			artifact := inventoryResult.Artifacts[0]
			artifact.ID, file.ArtifactID = identity, identity
			fragment, err := ExtractSources(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{file}})
			if err != nil || len(fragment.Documents) != 1 {
				t.Fatalf("fixture extraction: %v", err)
			}
			fixture := sourceBindingFixture{Document: fragment.Documents[0], Artifacts: map[string]rkcmodel.Artifact{identity: artifact}, Nodes: map[string]rkcmodel.Node{}, Evidence: map[string]rkcmodel.Evidence{}}
			for _, node := range fragment.Nodes {
				fixture.Nodes[node.ID] = node
			}
			for _, evidence := range fragment.Evidence {
				fixture.Evidence[evidence.ID] = evidence
			}
			if whole, ids := fixture.references(context.Background()); whole != nil || len(ids) != 0 {
				t.Fatalf("nonopaque inventoried identity gained portable references: %+v, %v", whole, ids)
			}
		})
	}
}

func TestSourceBindingRecordMetadataMustMatchProducerInterpretation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*sourceBindingFixture)
	}{
		{"missing validity", func(f *sourceBindingFixture) { delete(f.Document.Sections[1].Attributes, "record_valid") }},
		{"string validity", func(f *sourceBindingFixture) { f.Document.Sections[1].Attributes["record_valid"] = "true" }},
		{"header column projection", func(f *sourceBindingFixture) {
			f.Document.Sections[0].Attributes["projection"] = "secret-redacted-column-values"
		}},
		{"data raw projection contradiction", func(f *sourceBindingFixture) {
			f.Document.Sections[1].Attributes["projection"] = "secret-redacted-source"
		}},
		{"invalid data column projection", func(f *sourceBindingFixture) { f.Document.Sections[1].Attributes["record_valid"] = false }},
		{"fractional record index", func(f *sourceBindingFixture) { f.Document.Sections[1].Attributes["record_index"] = 1.5 }},
		{"repeated record index", func(f *sourceBindingFixture) {
			section := &f.Document.Sections[1]
			section.Attributes["record_index"] = 1
			section.Attributes["projection"] = "secret-redacted-source"
			node := f.Nodes[section.ID]
			section.Heading = fmt.Sprintf("Record 1 · Lines %d–%d", node.Source.StartLine, node.Source.EndLine)
			node.Name = section.Heading
			f.Nodes[section.ID] = node
		}},
		{"negative record index", func(f *sourceBindingFixture) { f.Document.Sections[1].Attributes["record_index"] = -1 }},
		{"missing truncation", func(f *sourceBindingFixture) { delete(f.Document.Sections[1].Attributes, "projection_truncated") }},
		{"invalid complete receipt", func(f *sourceBindingFixture) { f.Document.Attributes["complete"] = "true" }},
		{"truncated complete contradiction", func(f *sourceBindingFixture) { f.Document.Sections[1].Attributes["projection_truncated"] = true }},
		{"authenticated producer assertion", func(f *sourceBindingFixture) { f.Document.Attributes["producer_verified"] = true }},
		{"record count too small", func(f *sourceBindingFixture) { f.Document.Attributes["record_count"] = 1 }},
		{"invalid records exceed total", func(f *sourceBindingFixture) { f.Document.Attributes["invalid_record_count"] = 4 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceBindingFixture(t, "records.csv", "csv", "name,notes\nAlice,first\nBob,last\n")
			test.mutate(&fixture)
			if whole, ids := fixture.references(context.Background()); whole != nil || len(ids) != 0 {
				t.Fatalf("inconsistent record metadata retained references: %+v, %v", whole, ids)
			}
		})
	}
}

func TestSourceBindingMethodCannotChangeRecordInterpretation(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"csv", "jsonl"} {
		t.Run(language, func(t *testing.T) {
			body := "name,notes\nAlice,first\n"
			if language == "jsonl" {
				body = "{\"name\":\"Alice\"}\n"
			}
			fixture := newSourceBindingFixture(t, "records."+language, language, body)
			section := &fixture.Document.Sections[0]
			node := fixture.Nodes[section.ID]
			evidence := fixture.Evidence[section.EvidenceIDs[0]]
			evidence.Method = "source.text_chunk"
			fixture.Evidence[evidence.ID] = evidence
			section.Attributes["record_index"] = 0
			section.Heading = fmt.Sprintf("Lines %d–%d", node.Source.StartLine, node.Source.EndLine)
			node.Name = section.Heading
			fixture.Nodes[node.ID] = node
			if whole, ids := fixture.references(context.Background()); whole != nil || len(ids) != 0 {
				t.Fatalf("record relabeled as prose retained producer references: %+v, %v", whole, ids)
			}
		})
	}
}

func TestSourceBindingCancellationNeverReturnsPartialRoster(t *testing.T) {
	t.Parallel()
	fixture := newSourceBindingFixture(t, "records.jsonl", "jsonl", "{\"first\":1}\n{\"second\":2}\n{\"third\":3}\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx, &cancelAfterSourceChecks{Context: context.Background(), cancelAt: 3}} {
		if whole, ids := fixture.references(ctx); whole != nil || len(ids) != 0 {
			t.Fatalf("cancelled provenance returned a partial roster: %+v, %v", whole, ids)
		}
	}
}

type cancelAfterSourceChecks struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *cancelAfterSourceChecks) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
