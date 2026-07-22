package secrets

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestScanDetectsSupportedCredentialShapesWithoutRetainingValues(t *testing.T) {
	t.Parallel()

	privateKey := strings.Join([]string{"-----BEGIN ", "RSA PRIVATE KEY-----\nmaterial\n-----END ", "RSA PRIVATE KEY-----"}, "")
	awsKey := "AK" + "IA" + strings.Repeat("A", 16)
	githubToken := "gh" + "p_" + strings.Repeat("b", 30)
	fineGrained := "github_" + "pat_" + strings.Repeat("C_", 10)
	slackToken := "xox" + "b-" + strings.Repeat("D", 10)
	stripeToken := "sk_" + "live_" + strings.Repeat("e", 16)
	basicPassword := "basic-" + strings.Repeat("f", 8)
	assignedSecret := strings.Repeat("g7", 11)
	data := []byte(strings.Join([]string{
		privateKey,
		awsKey,
		githubToken,
		fineGrained,
		slackToken,
		stripeToken,
		"https://user:" + basicPassword + "@example.invalid/path",
		strings.Join([]string{"client", "_secret", "=", assignedSecret}, ""),
	}, "\n"))

	findings := Scan(data)
	if len(findings) != 8 {
		t.Fatalf("Scan found %d credentials, want 8: %+v", len(findings), findings)
	}
	wantKinds := []string{"private_key", "aws_access_key", "github_token", "github_fine_grained_token", "slack_token", "stripe_live_secret", "basic_auth_password", "secret_assignment"}
	gotKinds := make([]string, len(findings))
	for i, finding := range findings {
		gotKinds[i] = finding.Kind
		if finding.StartByte < 0 || finding.EndByte <= finding.StartByte || finding.EndByte > len(data) || finding.StartLine < 1 || finding.EndLine < finding.StartLine {
			t.Errorf("invalid finding bounds: %+v", finding)
		}
		if len(finding.Fingerprint) != 16 || strings.Contains(finding.Fingerprint, string(data[finding.StartByte:finding.EndByte])) {
			t.Errorf("fingerprint is absent or retained plaintext: %+v", finding)
		}
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("finding kinds = %v, want %v", gotKinds, wantKinds)
	}
	if findings[len(findings)-1].KeyName != "client_secret" || findings[len(findings)-1].Confidence != .92 {
		t.Fatalf("assignment metadata = %+v", findings[len(findings)-1])
	}
	if findings[6].StartByte != strings.Index(string(data), basicPassword) {
		t.Fatalf("basic auth finding should cover only password: %+v", findings[6])
	}
}

func TestScanFiltersPlaceholdersReferencesAndMergesOverlappingDetectors(t *testing.T) {
	t.Parallel()

	placeholderLines := []string{
		strings.Join([]string{"pass", "word", "=change", "me"}, ""),
		strings.Join([]string{"api", "_key", "=${", "API_KEY}"}, ""),
		strings.Join([]string{"auth", "_token", "=env", ":TOKEN"}, ""),
		strings.Join([]string{"private", "_key", "=vault", ":service/key"}, ""),
		strings.Join([]string{"secret", "=file", ":/run/secret"}, ""),
		strings.Join([]string{"client", "_secret", "=secret", "://service/key"}, ""),
		"https://user:" + "change" + "me@example.invalid",
	}
	if got := Scan([]byte(strings.Join(placeholderLines, "\n"))); len(got) != 0 {
		t.Fatalf("placeholder/reference values produced findings: %+v", got)
	}

	token := "gh" + "p_" + strings.Repeat("z", 30)
	overlap := []byte(strings.Join([]string{"api", "_key", "=", token}, ""))
	findings := Scan(overlap)
	if len(findings) != 1 || findings[0].Kind != "github_token" || findings[0].Confidence != .98 {
		t.Fatalf("overlap was not merged toward higher-confidence detector: %+v", findings)
	}
	if findings[0].StartByte != strings.Index(string(overlap), token) || findings[0].EndByte != len(overlap) {
		t.Fatalf("merged bounds = %+v", findings[0])
	}
}

func TestSecretNameAndPlaceholderClassification(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"db.password", "service-passwd", "CLIENT_SECRET_NAME", "githubToken", "api-key", "private.key", "credentials"} {
		if !IsSecretName(name) {
			t.Errorf("IsSecretName(%q) = false", name)
		}
	}
	for _, name := range []string{"username", "public_key_id", "configuration", "endpoint"} {
		if IsSecretName(name) {
			t.Errorf("IsSecretName(%q) = true", name)
		}
	}
	for _, value := range []string{"", " changeme ", "'dummy'", "example-item", "dummy-value", "replace_this", "${VALUE}", "{{ value }}", "<value>", strings.Repeat("x", 8)} {
		if !IsPlaceholder(value) {
			t.Errorf("IsPlaceholder(%q) = false", value)
		}
	}
	for _, value := range []string{"x", "a-realistic-varied-value", "12345678a"} {
		if IsPlaceholder(value) {
			t.Errorf("IsPlaceholder(%q) = true", value)
		}
	}
	if !looksLikeReference("env:item") || !looksLikeReference("secret://item") || !looksLikeReference("vault:item") || !looksLikeReference("file:item") || looksLikeReference("literal:item") {
		t.Fatal("reference classification mismatch")
	}
}

func TestRedactClampsBoundsPreservesLayoutAndDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := []byte("ab\ncd\tef")
	original := append([]byte(nil), input...)
	findings := []Finding{
		{StartByte: 3, EndByte: 7, Kind: "later"},
		{StartByte: -2, EndByte: 2, Kind: "early"},
		{StartByte: 20, EndByte: 30, Kind: "outside"},
		{StartByte: 4, EndByte: 4, Kind: "empty"},
	}
	got := Redact(input, findings)
	if string(got) != "**\n**\t*f" {
		t.Fatalf("Redact() = %q", got)
	}
	if !reflect.DeepEqual(input, original) {
		t.Fatal("Redact mutated input")
	}
	if len(got) != len(input) || got[2] != '\n' || got[5] != '\t' {
		t.Fatal("Redact changed byte layout")
	}
}

func TestFindingCoordinatesFingerprintAndMergeRules(t *testing.T) {
	t.Parallel()

	data := []byte("first\nsecond\nthird")
	finding := makeFinding(data, 6, 12, "kind", .8, "key")
	if finding.StartLine != 2 || finding.StartColumn != 0 || finding.EndLine != 2 || finding.EndColumn != 6 || len(finding.Fingerprint) != 16 || finding.KeyName != "key" {
		t.Fatalf("makeFinding() = %+v", finding)
	}
	if line, column := lineColumn(data, len(data)+10); line != 3 || column != 5 {
		t.Fatalf("lineColumn past end = %d:%d", line, column)
	}
	if line, column := lineColumn(data, -1); line != 1 || column != 0 {
		t.Fatalf("lineColumn negative = %d:%d", line, column)
	}

	merged := mergeFindings([]Finding{
		{Kind: "low", Confidence: .5, StartByte: 0, EndByte: 5, EndLine: 1, EndColumn: 5, Fingerprint: "low"},
		{Kind: "short", Confidence: .1, StartByte: 0, EndByte: 3},
		{Kind: "high", Confidence: .9, StartByte: 2, EndByte: 8, EndLine: 2, EndColumn: 2, Fingerprint: "high", KeyName: "key"},
		{Kind: "adjacent", Confidence: .7, StartByte: 8, EndByte: 10},
		{Kind: "invalid", StartByte: 12, EndByte: 12},
	})
	if len(merged) != 2 {
		t.Fatalf("mergeFindings() = %+v", merged)
	}
	if merged[0].StartByte != 0 || merged[0].EndByte != 8 || merged[0].Kind != "high" || merged[0].Confidence != .9 || merged[0].Fingerprint != "high" || merged[0].KeyName != "key" || merged[0].EndLine != 2 {
		t.Errorf("overlapping merge = %+v", merged[0])
	}
	if merged[1].Kind != "adjacent" || merged[1].StartByte != 8 {
		t.Errorf("adjacent range should remain distinct: %+v", merged[1])
	}

	ordered := append([]Finding(nil), merged...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartByte < ordered[j].StartByte })
	if !reflect.DeepEqual(ordered, merged) {
		t.Fatal("merged findings are not sorted")
	}
}

func TestSensitiveLiteralsValidatesDeduplicatesAndSortsRanges(t *testing.T) {
	t.Parallel()

	data := []byte("short medium-value longest-sensitive-value   ")
	findings := []Finding{
		{StartByte: 0, EndByte: 5},
		{StartByte: 6, EndByte: 18},
		{StartByte: 19, EndByte: 42},
		{StartByte: 0, EndByte: 5},
		{StartByte: 43, EndByte: 46},
		{StartByte: -1, EndByte: 2},
		{StartByte: 1, EndByte: len(data) + 1},
		{StartByte: 4, EndByte: 4},
	}
	got := SensitiveLiterals(data, findings)
	want := []string{"longest-sensitive-value", "medium-value", "short"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SensitiveLiterals() = %q, want %q", got, want)
	}
	if got := SensitiveLiterals(nil, nil); len(got) != 0 {
		t.Fatalf("empty SensitiveLiterals() = %v", got)
	}

	equalLength := SensitiveLiterals([]byte("bbb aaa"), []Finding{{StartByte: 0, EndByte: 3}, {StartByte: 4, EndByte: 7}})
	if !reflect.DeepEqual(equalLength, []string{"aaa", "bbb"}) {
		t.Fatalf("equal-length lexical ordering = %v", equalLength)
	}
}

func TestSanitizeBundleRecursivelyRedactsStringsAndLeavesBytes(t *testing.T) {
	t.Parallel()

	literal := strings.Join([]string{"sensitive", "-value-", "123"}, "")
	token := "gh" + "p_" + strings.Repeat("q", 30)
	nilStringPointer := (*string)(nil)
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			RootName: literal + ":" + literal,
			Metadata: map[string]string{"nested": literal},
		},
		Nodes: []rkcmodel.Node{{
			ID: "node", Name: token,
			Attributes: map[string]any{
				"direct":  literal,
				"slice":   []any{literal + "!"},
				"array":   [1]string{literal},
				"map":     map[string]string{"value": literal},
				"bytes":   []byte(literal),
				"nil":     nilStringPointer,
				"nil_map": map[string]string(nil),
			},
		}},
	}
	if got := SanitizeBundle(nil, []string{literal}); got != 0 {
		t.Fatalf("nil bundle redactions = %d", got)
	}
	scannerOnly := rkcmodel.Bundle{Nodes: []rkcmodel.Node{{
		ID:   "generated",
		Name: token,
		Attributes: map[string]any{
			"generated_value": token,
			token:             "generated-key",
		},
	}}}
	if got := SanitizeBundle(&scannerOnly, nil); got != 3 {
		t.Fatalf("detector-only bundle redactions = %d, want 3", got)
	}
	if scannerOnly.Nodes[0].Name != strings.Repeat("*", len(token)) {
		t.Fatalf("plugin-generated token was not redacted: %q", scannerOnly.Nodes[0].Name)
	}
	if scannerOnly.Nodes[0].Attributes["generated_value"] != strings.Repeat("*", len(token)) {
		t.Fatalf("plugin-generated attribute was not redacted: %#v", scannerOnly.Nodes[0].Attributes)
	}
	if _, ok := scannerOnly.Nodes[0].Attributes[strings.Repeat("*", len(token))]; !ok {
		t.Fatalf("plugin-generated map key was not redacted: %#v", scannerOnly.Nodes[0].Attributes)
	}
	redactions := SanitizeBundle(&bundle, []string{"", "short", literal, literal})
	if redactions != 8 {
		t.Fatalf("SanitizeBundle redactions = %d, want 8; bundle=%+v", redactions, bundle)
	}
	if strings.Contains(bundle.Snapshot.RootName, literal) || bundle.Snapshot.RootName != redactionToken+":"+redactionToken || bundle.Snapshot.Metadata["nested"] != redactionToken {
		t.Fatalf("snapshot was not sanitized: %+v", bundle.Snapshot)
	}
	if bundle.Nodes[0].Name != strings.Repeat("*", len(token)) {
		t.Fatalf("detector-backed token redaction = %q", bundle.Nodes[0].Name)
	}
	attributes := bundle.Nodes[0].Attributes
	if attributes["direct"] != redactionToken || attributes["slice"].([]any)[0] != redactionToken+"!" ||
		attributes["array"].([1]string)[0] != redactionToken || attributes["map"].(map[string]string)["value"] != redactionToken {
		t.Fatalf("nested attributes were not sanitized: %#v", attributes)
	}
	if string(attributes["bytes"].([]byte)) != literal {
		t.Fatal("byte slice should remain untouched to avoid arbitrary binary mutation")
	}
}

func TestSanitizeBundleRedactsMapKeysWithoutDroppingCollisions(t *testing.T) {
	t.Parallel()

	firstSecret := "source-secret-alpha"
	secondSecret := "source-secret-beta"
	attributes := map[string]any{
		redactionToken:        "existing",
		redactionToken + "#2": "existing-suffix",
		firstSecret:           "first",
		secondSecret:          "second",
	}
	bundle := rkcmodel.Bundle{Nodes: []rkcmodel.Node{{ID: "node", Attributes: attributes}}}

	if got := SanitizeBundle(&bundle, []string{firstSecret, secondSecret}); got != 2 {
		t.Fatalf("map-key redactions = %d, want 2", got)
	}
	got := bundle.Nodes[0].Attributes
	want := map[string]any{
		redactionToken:        "existing",
		redactionToken + "#2": "existing-suffix",
		redactionToken + "#3": "first",
		redactionToken + "#4": "second",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collision-safe sanitized map = %#v, want %#v", got, want)
	}
	for key := range got {
		if strings.Contains(key, firstSecret) || strings.Contains(key, secondSecret) {
			t.Fatalf("sensitive map key survived sanitization: %q", key)
		}
	}
}

func TestRedactReflectHandlesInvalidNilAndUnsettableValues(t *testing.T) {
	t.Parallel()

	count := 0
	redactReflect(reflect.Value{}, []string{"x"}, &count)
	redactReflect(reflect.ValueOf((*string)(nil)), []string{"x"}, &count)
	var nilInterface any
	redactReflect(reflect.ValueOf(&nilInterface).Elem(), []string{"x"}, &count)
	redactReflect(reflect.ValueOf("x"), []string{"x"}, &count)
	if count != 0 {
		t.Fatalf("non-settable/nil values produced %d redactions", count)
	}
}
