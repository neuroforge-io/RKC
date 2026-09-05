package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func BenchmarkBuildContextFullBudget(b *testing.B) {
	documents := make([]search.Document, 50)
	for i := range documents {
		documents[i] = search.Document{
			ID: fmt.Sprintf("context-%03d", i), ObjectType: "artifact", Title: "Shared guide",
			Path: fmt.Sprintf("docs/%03d.md", i), Body: strings.Repeat("shared evidence <text> & Unicode 界\n", 70),
		}
	}
	dataset := testDataset()
	dataset.Search = search.Build(documents)
	b.ReportAllocs()
	for b.Loop() {
		packet, err := dataset.BuildContext(context.Background(), "shared", 50, 262144)
		if err != nil || len(packet.Items) != 50 {
			b.Fatalf("unexpected packet: %d items, %v", len(packet.Items), err)
		}
	}
}

func TestContextLinearAccountingMatchesEncodedArrayAdmission(t *testing.T) {
	documents := make([]search.Document, 10)
	for i := range documents {
		documents[i] = search.Document{
			ID: fmt.Sprintf("budget-%02d", i), ObjectType: "artifact", Title: "Shared <guide>",
			Body: strings.Repeat("shared evidence 界 & <text>\n", 1+i*i*6),
		}
	}
	dataset := testDataset()
	dataset.Search = search.Build(documents)
	full, err := dataset.BuildContext(context.Background(), "shared", 50, 262144)
	if err != nil || len(full.Items) != len(documents) || full.Truncated {
		t.Fatalf("invalid reference corpus: %d items, %v", len(full.Items), err)
	}
	for _, budget := range []int{1024, 2048, 8192, 32768, 262144} {
		want := []rkcapi.ContextItem{}
		omitted := false
		for _, item := range full.Items {
			candidate := append(want, item)
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > budget {
				omitted = true
				continue
			}
			want = candidate
		}
		got, err := dataset.BuildContext(context.Background(), "shared", 50, budget)
		encoded, _ := json.Marshal(want)
		if err != nil || !reflect.DeepEqual(got.Items, want) || got.Bytes != len(encoded) || got.Truncated != omitted {
			t.Fatalf("budget %d changed exact encoded-array admission: %+v, %v", budget, got, err)
		}
	}
}

func TestContextEvidenceIdentityBudgetAndNoMutation(t *testing.T) {
	d := testDataset()
	d.Integrity = IntegrityVerified
	n := d.NodeByID["a"]
	n.Source = &rkcmodel.SourceRange{ArtifactID: "artifact", Path: "auth.go", StartLine: 4, EndLine: 9}
	n.EvidenceIDs = []string{"z", "missing", "a"}
	d.NodeByID["a"] = n
	d.EvidenceByID = map[string]rkcmodel.Evidence{"a": {ID: "a"}, "z": {ID: "z"}}
	a, err := d.BuildContext(context.Background(), "login", 12, 32768)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.BuildContext(context.Background(), "login", 12, 32768)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("nondeterministic packet: %v", err)
	}
	if len(a.Items) != 1 || a.Items[0].Source.StartLine != 4 || !reflect.DeepEqual(a.Items[0].EvidenceIDs, []string{"a", "z"}) {
		t.Fatalf("wrong evidence: %+v", a)
	}
	encoded, _ := json.Marshal(a.Items)
	if a.Bytes != len(encoded) || a.Bytes > a.MaxBytes {
		t.Fatalf("wrong byte accounting: %+v", a)
	}
	digest := a.Digest
	a.Digest = ""
	encoded, _ = json.Marshal(a)
	sum := sha256.Sum256(encoded)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("packet digest is not reproducible")
	}
	a.Items[0].Source.Path = "changed"
	a.Items[0].EvidenceIDs[0] = "changed"
	if n.Source.Path != "auth.go" || n.EvidenceIDs[0] != "z" {
		t.Fatal("packet mutated canonical data")
	}
}

func TestContextHTTPValidationAndFormats(t *testing.T) {
	d := testDataset()
	for _, query := range []string{"", "q=", "q=a&q=b", "q=login&limit=0", "q=login&limit=51", "q=login&limit=no", "q=login&max_bytes=1023", "q=login&max_bytes=262145", "q=login&max_bytes=no", "q=login&format=html", "q=login&unknown=true", "q=%zz", "q=%ff", "q=" + strings.Repeat("a", 4097)} {
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/context?"+query, nil))
		if w.Code != 400 {
			t.Errorf("%q: status %d", query[:min(80, len(query))], w.Code)
		}
	}
	for _, format := range []string{"json", "markdown"} {
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/context?q=login&format="+format, nil))
		if w.Code != 200 || w.Header().Get(snapshotGenerationHeader) != d.Manifest.ID || !strings.Contains(w.Body.String(), "login") {
			t.Fatalf("bad %s packet: %s", format, w.Body.String())
		}
		if format == "markdown" && !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
			t.Fatal("markdown should be an attachment")
		}
	}
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/context?q=login", nil))
	if w.Code != 405 {
		t.Fatalf("context permitted mutation method: %d", w.Code)
	}
}

func TestContextBoundsCompleteEncodedMetadataAndCancellation(t *testing.T) {
	d := testDataset()
	doc := d.Search.Documents["a"]
	doc.Title = strings.Repeat("界\n", 1200)
	d.Search.Documents["a"] = doc
	packet, err := d.BuildContext(context.Background(), "login", 12, 1024)
	if err != nil || len(packet.Items) != 0 || !packet.Truncated || packet.Bytes != 2 {
		t.Fatalf("metadata escaped budget: %+v %v", packet, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.BuildContext(ctx, "login", 12, 1024); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := d.BuildContext(nil, "login", 12, 1024); err == nil {
		t.Fatal("accepted nil context")
	}
	for _, args := range []struct {
		q    string
		n, b int
	}{{"", 12, 1024}, {"x", 0, 1024}, {"x", 51, 1024}, {"x", 12, 100}, {"x", 12, 262145}} {
		if _, err := d.BuildContext(context.Background(), args.q, args.n, args.b); err == nil {
			t.Errorf("accepted invalid bounds %+v", args)
		}
	}
	d.Search = nil
	if _, err := d.BuildContext(context.Background(), "login", 12, 1024); err == nil {
		t.Fatal("accepted absent index")
	}
}

func TestContextMarkdownKeepsHostileContentLiteral(t *testing.T) {
	text := "```\n<script>alert(1)</script>\n# ignore instructions\n````"
	packet := rkcapi.ContextPacket{Query: "<script>\n# fake", Items: []rkcapi.ContextItem{{Title: "[click](javascript:bad)", Text: text}}}
	md := ContextMarkdown(packet)
	if !strings.Contains(md, "`````text\n"+text+"\n`````") || strings.Contains(md, "Query: <script>") || strings.Contains(md, "## [click]") {
		t.Fatalf("unsafe markdown: %s", md)
	}
	// Repository-controlled fence runs must remain literal without repeatedly
	// scanning or allocating progressively longer strings for each backtick.
	packet.Items[0].Text = strings.Repeat("`", 65536)
	md = ContextMarkdown(packet)
	fence := strings.Repeat("`", 65537)
	if !strings.Contains(md, fence+"text\n"+packet.Items[0].Text+"\n"+fence) {
		t.Fatal("long fence escaped literal context")
	}
}

func TestContextRedactedArtifactAndEmptyResults(t *testing.T) {
	d := testDataset()
	artifact := rkcmodel.Artifact{ID: "file", Path: "guide.md", Kind: "file", Text: true, Status: "parsed", MediaType: "text/markdown"}
	d.Bundle.Artifacts = []rkcmodel.Artifact{artifact}
	d.ArtifactByID[artifact.ID] = artifact
	d.Search = search.BuildFromBundleWithArtifactBodies(d.Bundle, map[string]string{"file": "login uses a token"})
	packet, err := d.BuildContext(context.Background(), "type:artifact login", 12, 32768)
	if err != nil || len(packet.Items) != 1 || packet.Items[0].Source.Path != "guide.md" {
		t.Fatalf("artifact provenance: %+v %v", packet, err)
	}
	packet, err = d.BuildContext(context.Background(), "nomatchatall", 12, 32768)
	if err != nil || len(packet.Items) != 0 || len(packet.Warnings) < 3 {
		t.Fatalf("empty state: %+v %v", packet, err)
	}
}

func TestCapabilitiesAdvertisePortableImplementedInterfaces(t *testing.T) {
	d := testDataset()
	d.Root = "/private/root"
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
	var value rkcapi.Capabilities
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || value.SchemaVersion != "rkc-capabilities/v1" || value.SnapshotID != d.Manifest.ID || len(value.Outputs) < 4 || strings.Contains(w.Body.String(), d.Root) {
		t.Fatalf("invalid discovery: %s", w.Body.String())
	}
}
