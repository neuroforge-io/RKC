package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/modelassets"
	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
)

func TestParseQueryRetrievalModeStrict(t *testing.T) {
	for _, mode := range []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeSemantic, retrieval.ModeHybrid} {
		got, err := parseQueryRetrievalMode(string(mode))
		if err != nil || got != mode {
			t.Errorf("parseQueryRetrievalMode(%q) = %q, %v", mode, got, err)
		}
	}
	for _, value := range []string{"", "dense", "LEXICAL", " lexical", "hybrid\n"} {
		if got, err := parseQueryRetrievalMode(value); err == nil || got != "" {
			t.Errorf("parseQueryRetrievalMode(%q) = %q, %v; want strict refusal", value, got, err)
		}
	}
}

func TestResolveQueryVectorIndexPathConfinesDerivedStateOutsideAtlas(t *testing.T) {
	root := t.TempDir()
	atlas := filepath.Join(root, "atlas")
	if err := os.Mkdir(atlas, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "derived", "search", "vectors.json")
	got, err := resolveQueryVectorIndexPath(outside, atlas)
	if err != nil {
		t.Fatalf("resolve outside index: %v", err)
	}
	if got != outside {
		t.Fatalf("resolved path = %q, want %q", got, outside)
	}
	for _, path := range []string{
		"",
		filepath.Join(root, "vectors.bin"),
		filepath.Join(atlas, "vectors.json"),
		filepath.Join(atlas, "nested", "vectors.json"),
	} {
		if _, err := resolveQueryVectorIndexPath(path, atlas); err == nil {
			t.Errorf("resolveQueryVectorIndexPath(%q) succeeded", path)
		}
	}

	defaultPath, err := defaultQueryVectorIndexPath(atlas, "embedding-asset")
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := filepath.Join(root, "atlas.rkc-derived", "search", "embedding-asset", "vector-index.json")
	if defaultPath != wantDefault {
		t.Fatalf("default vector path = %q, want %q", defaultPath, wantDefault)
	}
	for _, input := range [][2]string{{"", "asset"}, {atlas, ""}, {atlas, " \t"}} {
		if _, err := defaultQueryVectorIndexPath(input[0], input[1]); err == nil {
			t.Errorf("defaultQueryVectorIndexPath(%q, %q) succeeded", input[0], input[1])
		}
	}
}

func TestResolveQueryVectorIndexPathRejectsSymlinksAndNonDirectories(t *testing.T) {
	root := t.TempDir()
	atlas := filepath.Join(root, "atlas")
	realDerived := filepath.Join(root, "real-derived")
	if err := os.Mkdir(atlas, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(realDerived, 0o700); err != nil {
		t.Fatal(err)
	}

	indexSymlink := filepath.Join(root, "index.json")
	if err := os.Symlink(filepath.Join(realDerived, "missing.json"), indexSymlink); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveQueryVectorIndexPath(indexSymlink, atlas); err == nil || !strings.Contains(err.Error(), "cannot be a symlink") {
		t.Fatalf("index symlink error = %v", err)
	}

	parentSymlink := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realDerived, parentSymlink); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveQueryVectorIndexPath(filepath.Join(parentSymlink, "nested", "index.json"), atlas); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("parent symlink error = %v", err)
	}

	parentFile := filepath.Join(root, "parent-file")
	if err := os.WriteFile(parentFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveQueryVectorIndexPath(filepath.Join(parentFile, "index.json"), atlas); err == nil || (!errors.Is(err, syscall.ENOTDIR) && !strings.Contains(err.Error(), "not a real directory")) {
		t.Fatalf("file parent error = %v", err)
	}

	realAtlas := filepath.Join(root, "real-atlas")
	linkedAtlas := filepath.Join(root, "linked-atlas")
	if err := os.Mkdir(realAtlas, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAtlas, linkedAtlas); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveQueryVectorIndexPath(filepath.Join(realAtlas, "derived.json"), linkedAtlas); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("resolved atlas confinement error = %v", err)
	}
}

func TestQueryEmbeddingTextUsesStableUTF8ByteBound(t *testing.T) {
	document := search.Document{
		ID: "document", ObjectType: "éclair", Kind: "function", Language: "go",
		Title: "Résumé", QualifiedName: "pkg.Résumé", Signature: "func Résumé()",
		Path: "résumé.go", Body: strings.Repeat("界", 20),
	}
	full := queryEmbeddingText(document, 4096)
	if !strings.Contains(full, "qualified_name: pkg.Résumé") || !strings.HasSuffix(full, strings.Repeat("界", 20)) {
		t.Fatalf("full embedding text omitted canonical fields: %q", full)
	}
	for _, maximum := range []int{0, 1, 6, 7, 8, 31, len(full) - 1, len(full), len(full) + 1} {
		got := queryEmbeddingText(document, maximum)
		if len(got) > maximum {
			t.Errorf("embedding text length %d exceeds maximum %d", len(got), maximum)
		}
		if !utf8.ValidString(got) {
			t.Errorf("embedding text at maximum %d is invalid UTF-8: %x", maximum, got)
		}
		if !strings.HasPrefix(full, got) {
			t.Errorf("embedding text at maximum %d is not a prefix", maximum)
		}
	}
	if got := queryEmbeddingText(document, 7); got != "type: " {
		t.Fatalf("truncation through multibyte rune = %q, want %q", got, "type: ")
	}
}

func TestQueryCorpusDigestIsCanonicalAndContentSensitive(t *testing.T) {
	documents := semanticQueryTestDocuments()
	forward := &search.Index{Documents: map[string]search.Document{
		"alpha": documents["alpha"],
		"beta":  documents["beta"],
	}}
	reverse := &search.Index{Documents: map[string]search.Document{
		"beta":  documents["beta"],
		"alpha": documents["alpha"],
	}}
	first, err := queryCorpusDigest(forward)
	if err != nil {
		t.Fatal(err)
	}
	second, err := queryCorpusDigest(reverse)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != sha256.Size*2 {
		t.Fatalf("canonical digests = %q and %q", first, second)
	}
	reverse.Documents["alpha"] = search.Document{ID: "alpha", Title: "changed"}
	changed, err := queryCorpusDigest(reverse)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("corpus digest did not bind document content")
	}
	if _, err := queryCorpusDigest(nil); err == nil {
		t.Fatal("queryCorpusDigest(nil) succeeded")
	}
}

func TestQueryVectorReceiptCanonicalizesAndRedactsLocalPaths(t *testing.T) {
	binding := semanticQueryTestBinding()
	public := queryPublicEmbeddingBinding(binding)
	if public.ExecutablePath != "" || public.ModelPath != "" || public.RuntimeReceiptPath != "" || public.LockPath != "" {
		t.Fatalf("public binding retained local paths: %+v", public)
	}
	if binding.ExecutablePath == "" || binding.ModelPath == "" || binding.RuntimeReceiptPath == "" || binding.LockPath == "" {
		t.Fatalf("redaction mutated source binding: %+v", binding)
	}
	receipt := queryVectorReceipt{
		SchemaVersion: queryVectorReceiptVersion, IndexSHA256: strings.Repeat("a", 64),
		CorpusSHA256: strings.Repeat("b", 64), MaximumTextBytes: queryVectorTextBytes,
		DocumentCount: 2, Dimensions: 2, Binding: public,
	}
	data, err := marshalQueryVectorReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' || bytes.Contains(data, []byte(binding.ModelPath)) || bytes.Contains(data, []byte(binding.ExecutablePath)) {
		t.Fatalf("canonical receipt leaked paths or lacked newline: %s", data)
	}
	var decoded queryVectorReceipt
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	remarshaled, err := marshalQueryVectorReceipt(decoded)
	if err != nil || !bytes.Equal(data, remarshaled) || !reflect.DeepEqual(decoded, receipt) {
		t.Fatalf("receipt round trip changed: err=%v\n%s\n%s", err, data, remarshaled)
	}
}

func TestPublishAndLoadQueryVectorIndexBindsCorpusAndModel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor-bound vector loading requires Linux procfs")
	}
	root := t.TempDir()
	atlas := filepath.Join(root, "atlas")
	parent := filepath.Join(root, "derived")
	if err := os.Mkdir(atlas, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := resolveQueryVectorIndexPath(filepath.Join(parent, "vector-index.json"), atlas)
	if err != nil {
		t.Fatal(err)
	}
	lexical := semanticQueryTestLexical()
	binding := semanticQueryTestBinding()
	index := semanticQueryTestVectorIndex(lexical, binding)
	if err := validateQueryVectorIndex(index, lexical, binding); err != nil {
		t.Fatalf("fixture vector index invalid: %v", err)
	}
	if err := publishQueryVectorIndex(path, index, lexical, binding); err != nil {
		t.Fatalf("publish vector index: %v", err)
	}
	for _, artifact := range []string{path, queryVectorReceiptPath(path)} {
		info, err := os.Lstat(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			t.Fatalf("published artifact %s has unsafe mode %v", artifact, info.Mode())
		}
	}
	loaded, err := loadQueryVectorIndex(path, lexical, binding)
	if err != nil {
		t.Fatalf("load published vector index: %v", err)
	}
	if !reflect.DeepEqual(loaded, index) {
		t.Fatalf("loaded index changed\n got: %+v\nwant: %+v", loaded, index)
	}

	receiptData, _, err := readBoundQueryRegular(queryVectorReceiptPath(path), maximumVectorReceiptBytes)
	if err != nil {
		t.Fatal(err)
	}
	var receipt queryVectorReceipt
	if err := json.Unmarshal(receiptData, &receipt); err != nil {
		t.Fatal(err)
	}
	corpusDigest, err := queryCorpusDigest(lexical)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CorpusSHA256 != corpusDigest || receipt.DocumentCount != len(lexical.Documents) || receipt.Dimensions != 2 || !reflect.DeepEqual(receipt.Binding, queryPublicEmbeddingBinding(binding)) {
		t.Fatalf("published receipt does not bind corpus and model: %+v", receipt)
	}
	if err := publishQueryVectorIndex(path, index, lexical, binding); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("replacement publication error = %v", err)
	}
}

func TestLoadQueryVectorIndexRejectsTampering(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor-bound vector loading requires Linux procfs")
	}
	for _, test := range []struct {
		name   string
		tamper func(t *testing.T, path string, lexical *search.Index, binding *modelassets.EmbeddingBinding)
		want   string
	}{
		{
			name: "index bytes",
			tamper: func(t *testing.T, path string, _ *search.Index, _ *modelassets.EmbeddingBinding) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 1
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "decode vector index",
		},
		{
			name: "noncanonical receipt",
			tamper: func(t *testing.T, path string, _ *search.Index, _ *modelassets.EmbeddingBinding) {
				receiptPath := queryVectorReceiptPath(path)
				data, err := os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(receiptPath, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "not canonical",
		},
		{
			name: "corpus",
			tamper: func(_ *testing.T, _ string, lexical *search.Index, _ *modelassets.EmbeddingBinding) {
				document := lexical.Documents["alpha"]
				document.Body = "changed after publication"
				lexical.Documents["alpha"] = document
			},
			want: "does not match",
		},
		{
			name: "binding",
			tamper: func(_ *testing.T, _ string, _ *search.Index, binding *modelassets.EmbeddingBinding) {
				binding.AssetID = "different-qualified-asset"
			},
			want: "does not match",
		},
		{
			name: "writable receipt",
			tamper: func(t *testing.T, path string, _ *search.Index, _ *modelassets.EmbeddingBinding) {
				if err := os.Chmod(queryVectorReceiptPath(path), 0o620); err != nil {
					t.Fatal(err)
				}
			},
			want: "bounded, non-writable regular file",
		},
		{
			name: "symlink receipt",
			tamper: func(t *testing.T, path string, _ *search.Index, _ *modelassets.EmbeddingBinding) {
				receiptPath := queryVectorReceiptPath(path)
				moved := receiptPath + ".real"
				if err := os.Rename(receiptPath, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, receiptPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "bounded, non-writable regular file",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			atlas := filepath.Join(root, "atlas")
			parent := filepath.Join(root, "derived")
			if err := os.Mkdir(atlas, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "index.json")
			lexical := semanticQueryTestLexical()
			binding := semanticQueryTestBinding()
			index := semanticQueryTestVectorIndex(lexical, binding)
			if err := publishQueryVectorIndex(path, index, lexical, binding); err != nil {
				t.Fatal(err)
			}
			test.tamper(t, path, lexical, &binding)
			if _, err := loadQueryVectorIndex(path, lexical, binding); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load tampered index error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateQueryVectorIndexRejectsDescriptorDocumentAndDigestDrift(t *testing.T) {
	lexical := semanticQueryTestLexical()
	binding := semanticQueryTestBinding()
	if err := validateQueryVectorIndex(nil, lexical, binding); err == nil {
		t.Fatal("nil semantic index succeeded")
	}
	if err := validateQueryVectorIndex(semanticQueryTestVectorIndex(lexical, binding), nil, binding); err == nil {
		t.Fatal("nil lexical index succeeded")
	}
	for _, test := range []struct {
		name   string
		mutate func(*search.VectorIndex)
		want   string
	}{
		{"provider", func(index *search.VectorIndex) { index.Descriptor.Provider = "other" }, "descriptor"},
		{"model", func(index *search.VectorIndex) { index.Descriptor.Model = "other" }, "descriptor"},
		{"digest", func(index *search.VectorIndex) { index.Descriptor.Digest = "sha256:" + strings.Repeat("0", 64) }, "descriptor"},
		{"dimensions", func(index *search.VectorIndex) { index.Descriptor.Dimensions = 0 }, "descriptor"},
		{"documents", func(index *search.VectorIndex) { delete(index.Documents, "alpha") }, "documents"},
		{"unknown vector", func(index *search.VectorIndex) { index.Vectors[0].DocumentID = "missing" }, "unknown document"},
		{"content digest", func(index *search.VectorIndex) { index.Vectors[0].ContentSHA256 = strings.Repeat("0", 64) }, "content digest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := semanticQueryTestVectorIndex(lexical, binding)
			test.mutate(index)
			if err := validateQueryVectorIndex(index, lexical, binding); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSemanticArtifactReadersFailClosedAndPreserveIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "artifact.json")
	if err := os.WriteFile(path, []byte("bound content"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, digest, err := readBoundQueryRegular(path, 64)
	if err != nil || string(data) != "bound content" {
		t.Fatalf("read bound artifact = %q, %v", data, err)
	}
	wantDigest := sha256.Sum256(data)
	if digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("bound artifact digest = %q", digest)
	}
	if _, _, err := readBoundQueryRegular(path, 2); err == nil {
		t.Fatal("oversized artifact read succeeded")
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBoundQueryRegular(path, 64); err == nil {
		t.Fatal("group/world-writable artifact read succeeded")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	file, info, err := openBoundQueryRegular(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBoundQueryRegular(path, file, info); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement identity verification = %v", err)
	}
	if _, err := digestOpenQueryRegular(file, 2); err == nil {
		t.Fatal("bounded open-file digest succeeded past limit")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := digestOpenQueryRegular(file, 64); err == nil {
		t.Fatal("closed-file digest succeeded")
	}
}

func TestPublishQueryVectorIndexCleansStagingAndRejectsUnsafeParents(t *testing.T) {
	root := t.TempDir()
	lexical := semanticQueryTestLexical()
	binding := semanticQueryTestBinding()
	invalid := semanticQueryTestVectorIndex(lexical, binding)
	invalid.Version = "unsupported"
	parent := filepath.Join(root, "derived")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "index.json")
	if err := publishQueryVectorIndex(path, invalid, lexical, binding); err == nil {
		t.Fatal("invalid vector index publication succeeded")
	}
	matches, err := filepath.Glob(filepath.Join(parent, ".rkc-vector-index-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("failed publication retained staging paths: %v", matches)
	}
	for _, artifact := range []string{path, queryVectorReceiptPath(path)} {
		if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed publication left %s: %v", artifact, err)
		}
	}

	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := publishQueryVectorIndex(filepath.Join(unsafeParent, "index.json"), semanticQueryTestVectorIndex(lexical, binding), lexical, binding); err == nil || !strings.Contains(err.Error(), "owner-controlled") {
		t.Fatalf("unsafe parent publication error = %v", err)
	}

	blockedParent := filepath.Join(root, "blocked")
	if err := os.WriteFile(blockedParent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishQueryVectorIndex(filepath.Join(blockedParent, "index.json"), semanticQueryTestVectorIndex(lexical, binding), lexical, binding); err == nil {
		t.Fatal("publication below non-directory parent succeeded")
	}
}

func TestRequireAbsentAndConditionalCleanupNeverReplaceUnrelatedArtifacts(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index.json")
	receiptPath := queryVectorReceiptPath(indexPath)
	if err := requireAbsentQueryVectorIndex(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, []byte("reserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireAbsentQueryVectorIndex(indexPath); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing receipt absence check = %v", err)
	}
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, "staged")
	unrelated := filepath.Join(root, "unrelated")
	if err := os.WriteFile(staged, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeQueryLinkIfSame(indexPath, staged)
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "keep" {
		t.Fatalf("conditional cleanup removed unrelated artifact: %q, %v", data, err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(staged, indexPath); err != nil {
		t.Fatal(err)
	}
	removeQueryLinkIfSame(indexPath, staged)
	if _, err := os.Lstat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conditional cleanup retained matching link: %v", err)
	}
	if data, err := os.ReadFile(staged); err != nil || string(data) != "staged" {
		t.Fatalf("conditional cleanup removed staged inode: %q, %v", data, err)
	}
}

func semanticQueryTestLexical() *search.Index {
	documents := semanticQueryTestDocuments()
	return &search.Index{
		Version: search.IndexVersion, Documents: documents,
		Postings: map[string][]search.Posting{}, DocumentLength: map[string]int{"alpha": 3, "beta": 2},
		AverageLength: 2.5, DocumentCount: len(documents),
	}
}

func semanticQueryTestDocuments() map[string]search.Document {
	return map[string]search.Document{
		"alpha": {
			ID: "alpha", ObjectType: "node", Kind: "function", Language: "go", Title: "Alpha",
			QualifiedName: "example.Alpha", Signature: "func Alpha() string", Path: "alpha.go",
			Body: "Alpha is the stable public entry point.", Metadata: map[string]string{"visibility": "public"},
		},
		"beta": {
			ID: "beta", ObjectType: "document", Kind: "guide", Language: "markdown", Title: "Beta guide",
			QualifiedName: "docs/beta", Path: "docs/beta.md", Body: "Beta documents the integration contract.",
		},
	}
}

func semanticQueryTestBinding() modelassets.EmbeddingBinding {
	return modelassets.EmbeddingBinding{
		AssetID: "qualified-embedding", LockSHA256: strings.Repeat("1", 64),
		RuntimeReceiptSHA256: strings.Repeat("2", 64), RuntimeProfile: "portable-cpu",
		RuntimeTag: "b10082", RuntimeRevision: strings.Repeat("3", 40), RuntimeLicense: "MIT",
		RuntimeSHA256: strings.Repeat("4", 64), ModelRevision: strings.Repeat("5", 40),
		ModelLicense: "Apache-2.0", ModelSHA256: strings.Repeat("6", 64), ModelSizeBytes: 123456,
		Quantization: "Q8_0", QuantizationBits: 8, NativeContextTokens: 32768,
		QualificationSpec: "embedding-v1", ExecutablePath: "/private/bin/llama-embedding",
		ModelPath: "/private/models/embedding.gguf", RuntimeReceiptPath: "/private/runtime/receipt.json",
		LockPath: "/private/model-lock.json",
	}
}

func semanticQueryTestVectorIndex(lexical *search.Index, binding modelassets.EmbeddingBinding) *search.VectorIndex {
	ids := []string{"alpha", "beta"}
	vectors := make([]search.VectorRecord, 0, len(ids))
	for index, id := range ids {
		digest := sha256.Sum256([]byte(queryEmbeddingText(lexical.Documents[id], queryVectorTextBytes)))
		values := []float32{1, 0}
		if index == 1 {
			values = []float32{0, 1}
		}
		vectors = append(vectors, search.VectorRecord{
			DocumentID: id, ContentSHA256: hex.EncodeToString(digest[:]), Values: values,
		})
	}
	documents := make(map[string]search.Document, len(lexical.Documents))
	for id, document := range lexical.Documents {
		documents[id] = document
	}
	return &search.VectorIndex{
		Version: search.VectorIndexVersion,
		Descriptor: search.EmbeddingDescriptor{
			Provider: "llama.cpp-embedding", Model: binding.AssetID,
			Digest: "sha256:" + binding.ModelSHA256, Dimensions: 2,
		},
		Documents: documents,
		Vectors:   vectors,
	}
}
