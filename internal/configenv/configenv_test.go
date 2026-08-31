package configenv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func configenvFixture(t *testing.T, files map[string]string) (rkcmodel.Fragment, []pluginapi.FileRef) {
	return configenvFixtureWithLimits(t, files, defaultExtractionLimits)
}

func configenvFixtureWithLimits(
	t *testing.T,
	files map[string]string,
	limits extractionLimits,
) (rkcmodel.Fragment, []pluginapi.FileRef) {
	t.Helper()
	root := t.TempDir()
	var refs []pluginapi.FileRef
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, pluginapi.FileRef{
			ArtifactID: rkcmodel.StableID("artifact", path), Path: path,
			SizeBytes: int64(len(content)), Language: "go",
		})
	}
	fragment, err := extractWithLimits(context.Background(), Options{Root: root, Files: refs}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return fragment, refs
}

func nodesByKind(fragment rkcmodel.Fragment, kind string) []rkcmodel.Node {
	var result []rkcmodel.Node
	for _, node := range fragment.Nodes {
		if node.Kind == kind {
			result = append(result, node)
		}
	}
	return result
}

func TestExtractBuildTags(t *testing.T) {
	fragment, _ := configenvFixture(t, map[string]string{
		"feature.go": "//go:build linux && rkc_feature_x\n\npackage main\n",
		"plain.go":   "package main\n",
	})
	targets := nodesByKind(fragment, "build_target")
	if len(targets) != 1 {
		t.Fatalf("build targets = %d, want 1", len(targets))
	}
	target := targets[0]
	tags, _ := target.Attributes["build_tags"].([]string)
	joined := strings.Join(tags, ",")
	if !strings.Contains(joined, "linux") || !strings.Contains(joined, "rkc_feature_x") {
		t.Fatalf("build tags = %v", tags)
	}
	if target.Attributes["constraint"] == nil {
		t.Fatal("constraint attribute missing")
	}
	builds := 0
	for _, edge := range fragment.Edges {
		if edge.Kind == "builds" && edge.From == target.ID {
			builds++
		}
	}
	if builds != 1 {
		t.Fatalf("builds edges = %d, want 1", builds)
	}
}

func TestExtractWorkflow(t *testing.T) {
	workflow := `name: ci
on: [push, pull_request]
env:
  CI_MAIN: "true"
  DEPLOY_TOKEN: "super-secret-value"
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      JOB_LEVEL: "yes"
    steps:
      - name: Checkout
        uses: actions/checkout@v4
      - name: Run tests
        run: go test ./...
  deploy:
    steps:
      - run: deploy.sh
`
	fragment, _ := configenvFixture(t, map[string]string{
		".github/workflows/ci.yml": workflow,
	})
	workflows := nodesByKind(fragment, "build_target")
	if len(workflows) != 3 {
		t.Fatalf("workflow+jobs = %d, want 3", len(workflows))
	}
	envs := nodesByKind(fragment, "environment_variable")
	names := map[string]rkcmodel.Node{}
	for _, env := range envs {
		names[env.Name] = env
	}
	if len(names) != 3 {
		t.Fatalf("env vars = %v, want CI_MAIN, JOB_LEVEL, and DEPLOY_TOKEN (name-only)", names)
	}
	if _, leaked := names["CI_MAIN"].Attributes["default"]; leaked {
		t.Fatal("CI_MAIN clear-text default was recorded")
	}
	if names["CI_MAIN"].Attributes["has_default"] != true ||
		names["CI_MAIN"].Attributes["default_class"] != "boolean" {
		t.Fatalf("CI_MAIN safe default metadata = %+v", names["CI_MAIN"].Attributes)
	}
	if digest, _ := names["CI_MAIN"].Attributes["default_sha256"].(string); len(digest) != 64 {
		t.Fatalf("CI_MAIN default digest = %q, want canonical SHA-256", digest)
	}
	// Secret-like names are recorded by name only: the value must never leak.
	if token, ok := names["DEPLOY_TOKEN"]; !ok {
		t.Fatal("secret-like env name was not recorded")
	} else if token.Attributes["secret_like"] != true {
		t.Fatalf("DEPLOY_TOKEN secret_like = %v", token.Attributes["secret_like"])
	} else if _, leaked := token.Attributes["default"]; leaked {
		t.Fatal("secret-like env value was recorded")
	}
	// The job-level env must be bound to the job, not the workflow.
	foundJobEnv := false
	for _, edge := range fragment.Edges {
		if edge.Kind == "configures" {
			foundJobEnv = true
		}
	}
	if !foundJobEnv {
		t.Fatal("no configures edges emitted")
	}
	steps := nodesByKind(fragment, "config_key")
	foundRun := false
	for _, edge := range fragment.Edges {
		if edge.Kind == "configures" {
			if class, ok := edge.Attributes["command_class"].(string); ok && class == "test" {
				foundRun = true
			}
			if _, leaked := edge.Attributes["run"]; leaked {
				t.Fatal("clear-text run command was recorded on an edge")
			}
		}
	}
	if !foundRun {
		t.Fatalf("safe step run metadata missing: %+v", steps)
	}
	foundRunStep := false
	for _, step := range steps {
		if class, ok := step.Attributes["command_class"].(string); ok && class == "deploy" {
			foundRunStep = true
		}
		if _, leaked := step.Attributes["run"]; leaked {
			t.Fatal("clear-text run command was recorded on a node")
		}
	}
	if !foundRunStep {
		t.Fatal("direct - run: step did not record safe command metadata")
	}
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"super-secret-value", `"true"`, `"yes"`, "go test ./...", "deploy.sh"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("fragment disclosed private workflow scalar %q", private)
		}
	}
}

func TestExtractTerraform(t *testing.T) {
	terraform := `
variable "region" {
  default = "us-east-1"
}

resource "aws_s3_bucket" "assets" {
  bucket = "assets.example.com"
}

output "bucket_name" {
  value = aws_s3_bucket.assets.id
}
`
	fragment, _ := configenvFixture(t, map[string]string{
		"infra/main.tf": terraform,
	})
	resources := nodesByKind(fragment, "build_target")
	if len(resources) != 1 {
		t.Fatalf("terraform resources = %d, want 1", len(resources))
	}
	if resources[0].Attributes["terraform_type"] != "aws_s3_bucket" {
		t.Fatalf("terraform type = %v", resources[0].Attributes["terraform_type"])
	}
	keys := nodesByKind(fragment, "config_key")
	if len(keys) != 2 {
		t.Fatalf("terraform variable/output = %d, want 2", len(keys))
	}
}

func TestExtractBuildTagNames(t *testing.T) {
	for constraint, want := range map[string][]string{
		"linux && amd64":             {"amd64", "linux"},
		"!windows || darwin":         {"darwin"},
		"linux && (arm64 || amd64)":  {"amd64", "arm64", "linux"},
		"rkc_enterprise && !rkc_oss": {"rkc_enterprise"},
		"go1.21 && cgo":              {"cgo", "go1.21"},
	} {
		got := buildTagNames(constraint)
		joined := strings.Join(got, ",")
		if !strings.Contains(joined, want[0]) {
			t.Errorf("buildTagNames(%q) = %v, missing %q", constraint, got, want)
		}
	}
	if got := buildTagNames("linux && ignore"); len(got) != 1 {
		t.Errorf("ignore tag handling = %v", got)
	}
}

func TestExtractValidatesOptions(t *testing.T) {
	if _, err := Extract(nil, Options{Root: "/tmp"}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := Extract(context.Background(), Options{Root: ""}); err == nil {
		t.Fatal("empty root was accepted")
	}
}

func TestWorkflowSystemVariants(t *testing.T) {
	for path, want := range map[string]string{
		".gitlab-ci.yml":          "gitlab",
		"bitbucket-pipelines.yml": "bitbucket",
		"azure-pipelines.yml":     "azure",
		"buildkite.yml":           "buildkite",
		"Jenkinsfile":             "jenkins",
		".github/workflows/x.yml": "github",
	} {
		fragment, _ := configenvFixture(t, map[string]string{
			path: "name: ci\njobs:\n  build:\n    steps:\n      - run: make\n",
		})
		workflows := nodesByKind(fragment, "build_target")
		if len(workflows) == 0 {
			t.Fatalf("%s produced no workflow node", path)
		}
		if workflows[0].Attributes["ci_system"] != want {
			t.Fatalf("%s ci_system = %v, want %s", path, workflows[0].Attributes["ci_system"], want)
		}
	}
}

func TestExtractUnreadableAndUnboundedInputs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("//go:build linux\n\npackage a\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	ref := pluginapi.FileRef{ArtifactID: "a", Path: "a.go", SizeBytes: 34}
	fragment, err := Extract(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("unreadable file produced nodes: %+v", fragment.Nodes)
	}
	// A non-text path type must not be treated as a Go file.
	ref2 := pluginapi.FileRef{ArtifactID: "b", Path: "notes.txt", SizeBytes: 3}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fragment, err = Extract(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{ref2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("non-Go file produced nodes: %+v", fragment.Nodes)
	}
	_ = os.Chmod(path, 0o600)
}

func TestTerraformBoundDiagnostic(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("resource \"t\" \"r0\" {\n")
	for index := 1; index <= maximumTerraformStatements+2; index++ {
		builder.WriteString("resource \"t\" \"r" + fmt.Sprint(index) + "\" {\n}\n")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "infra"), 0o700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(root, "infra/main.tf")
	if err := os.WriteFile(full, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	fragment, err := Extract(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{{
		ArtifactID: "a", Path: "infra/main.tf", SizeBytes: int64(builder.Len()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	bounded := false
	for _, diagnostic := range fragment.Diagnostics {
		if diagnostic.Code == "RKC-CFG-3002" {
			bounded = true
		}
	}
	if !bounded {
		t.Fatal("terraform bound diagnostic missing")
	}
}

func TestWorkflowEvidenceUsesPhysicalLineNumbers(t *testing.T) {
	workflow := "# comment\n\nname: ci\n\njobs:\n  test:\n    steps:\n      # step comment\n      - run: go test ./...\n"
	fragment, _ := configenvFixture(t, map[string]string{".github/workflows/ci.yml": workflow})
	for _, evidence := range fragment.Evidence {
		if evidence.Method != "configenv.step" {
			continue
		}
		if evidence.Source == nil || evidence.Source.StartLine != 9 || evidence.Source.EndLine != 9 {
			t.Fatalf("step source = %+v, want physical line 9", evidence.Source)
		}
		return
	}
	t.Fatal("step evidence missing")
}

func TestWorkflowMultilineCommandsAreFingerprintOnly(t *testing.T) {
	workflow := "jobs:\n  test:\n    steps:\n      - name: Private operation\n        run: |\n          echo hidden-first-line\n          ./internal-release --credential hidden-second-line\n"
	fragment, _ := configenvFixture(t, map[string]string{".github/workflows/ci.yml": workflow})
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"echo hidden-first-line", "./internal-release", "hidden-second-line"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("multiline command disclosed %q", private)
		}
	}
	digest := ""
	for _, edge := range fragment.Edges {
		if value, ok := edge.Attributes["command_sha256"].(string); ok {
			digest = value
		}
	}
	if len(digest) != 64 {
		t.Fatalf("multiline command digest = %q, want canonical SHA-256", digest)
	}
	changed := strings.Replace(workflow, "hidden-second-line", "changed-second-line", 1)
	changedFragment, _ := configenvFixture(t, map[string]string{".github/workflows/ci.yml": changed})
	changedDigest := ""
	for _, edge := range changedFragment.Edges {
		if value, ok := edge.Attributes["command_sha256"].(string); ok {
			changedDigest = value
		}
	}
	if len(changedDigest) != 64 || changedDigest == digest {
		t.Fatalf("changed multiline command digest = %q; original = %q", changedDigest, digest)
	}
}

func TestWorkflowActualFileSizeBoundIgnoresDeclaredSize(t *testing.T) {
	limits := defaultExtractionLimits
	limits.configFileBytes = 64
	content := "name: ci\njobs:\n  test:\n    steps:\n      - run: " + strings.Repeat("x", 80) + "\n"
	root := t.TempDir()
	full := filepath.Join(root, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// The admitted inventory size is metadata, not authority. A stale or forged
	// small value must not bypass the opened file's actual byte bound.
	ref := pluginapi.FileRef{
		ArtifactID: rkcmodel.StableID("artifact", ".github/workflows/ci.yml"),
		Path:       ".github/workflows/ci.yml",
		SizeBytes:  1,
	}
	fragment, err := extractWithLimits(context.Background(), Options{Root: root, Files: []pluginapi.FileRef{ref}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("oversized workflow produced nodes: %+v", fragment.Nodes)
	}
	if !hasDiagnostic(fragment, "RKC-CFG-3003") {
		t.Fatal("actual file-size diagnostic missing")
	}
}

func TestWorkflowNodeBoundsAreHardAndReferentiallyClean(t *testing.T) {
	t.Run("environment nodes", func(t *testing.T) {
		limits := defaultExtractionLimits
		limits.environmentNodes = 1
		fragment, _ := configenvFixtureWithLimits(t, map[string]string{
			".github/workflows/ci.yml": "env:\n  FIRST: one\n  SECOND: two\n",
		}, limits)
		if got := len(nodesByKind(fragment, "environment_variable")); got != 1 {
			t.Fatalf("environment nodes = %d, want hard limit 1", got)
		}
		if !hasDiagnostic(fragment, "RKC-CFG-3005") {
			t.Fatal("environment-node bound diagnostic missing")
		}
		assertFragmentReferencesAcceptedNodes(t, fragment, rkcmodel.StableID("artifact", ".github/workflows/ci.yml"))
	})

	t.Run("all config nodes", func(t *testing.T) {
		limits := defaultExtractionLimits
		limits.configNodes = 2
		fragment, _ := configenvFixtureWithLimits(t, map[string]string{
			".github/workflows/ci.yml": "jobs:\n  first:\n    steps:\n      - run: make\n  second:\n    steps:\n      - run: make test\n",
		}, limits)
		if len(fragment.Nodes) != 2 {
			t.Fatalf("config nodes = %d, want hard limit 2", len(fragment.Nodes))
		}
		if !hasDiagnostic(fragment, "RKC-CFG-3001") {
			t.Fatal("config-node bound diagnostic missing")
		}
		assertFragmentReferencesAcceptedNodes(t, fragment, rkcmodel.StableID("artifact", ".github/workflows/ci.yml"))
	})
}

func TestRetainedTextBoundRejectsAmplifyingWorkflowIdentifiers(t *testing.T) {
	limits := defaultExtractionLimits
	limits.retainedTextBytes = 64
	longJob := strings.Repeat("job", 256)
	longEnv := strings.Repeat("ENV", 256)
	fragment, _ := configenvFixtureWithLimits(t, map[string]string{
		".github/workflows/ci.yml": "env:\n  " + longEnv + ": value\njobs:\n  " + longJob + ":\n    steps:\n      - run: make\n",
	}, limits)
	if !hasDiagnostic(fragment, "RKC-CFG-3006") {
		t.Fatal("per-text retained-output diagnostic missing")
	}
	encoded, err := json.Marshal(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), longJob) || strings.Contains(string(encoded), longEnv) {
		t.Fatal("oversized workflow identifier survived in canonical output")
	}
	for _, node := range fragment.Nodes {
		if _, oversized := retainedTextUsage(reflect.ValueOf(node), limits.retainedTextBytes, 0); oversized {
			t.Fatalf("node retained oversized text: %+v", node)
		}
	}
}

func TestRetainedAggregateBudgetsStopDeterministically(t *testing.T) {
	files := map[string]string{}
	for index := 0; index < 8; index++ {
		files[fmt.Sprintf(".github/workflows/%02d.yml", index)] = "name: ci\n"
	}
	for _, test := range []struct {
		name   string
		limits extractionLimits
	}{
		{name: "fact budget", limits: func() extractionLimits {
			limits := defaultExtractionLimits
			limits.retainedFacts = 3
			return limits
		}()},
		{name: "byte budget", limits: func() extractionLimits {
			limits := defaultExtractionLimits
			limits.retainedBytes = 1800
			return limits
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			fragment, _ := configenvFixtureWithLimits(t, files, test.limits)
			if !hasDiagnostic(fragment, "RKC-CFG-3007") {
				t.Fatal("aggregate retained-output diagnostic missing")
			}
			facts := len(fragment.Nodes) + len(fragment.Edges) + len(fragment.Evidence)
			if facts > test.limits.retainedFacts {
				t.Fatalf("retained facts = %d, limit %d", facts, test.limits.retainedFacts)
			}
			first, _ := configenvFixtureWithLimits(t, files, test.limits)
			left, _ := json.Marshal(fragment)
			right, _ := json.Marshal(first)
			if string(left) != string(right) {
				t.Fatal("aggregate budget output is not deterministic")
			}
		})
	}
}

func TestWorkflowLineBoundIsExplicit(t *testing.T) {
	limits := defaultExtractionLimits
	limits.workflowLines = 2
	fragment, _ := configenvFixtureWithLimits(t, map[string]string{
		".github/workflows/ci.yml": "name: ci\njobs:\n  first:\n",
	}, limits)
	if !hasDiagnostic(fragment, "RKC-CFG-3004") {
		t.Fatal("workflow-line bound diagnostic missing")
	}
}

func hasDiagnostic(fragment rkcmodel.Fragment, code string) bool {
	for _, diagnostic := range fragment.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func assertFragmentReferencesAcceptedNodes(t *testing.T, fragment rkcmodel.Fragment, externalIDs ...string) {
	t.Helper()
	nodes := make(map[string]struct{}, len(fragment.Nodes))
	evidence := make(map[string]struct{}, len(fragment.Evidence))
	external := make(map[string]struct{}, len(externalIDs))
	for _, id := range externalIDs {
		external[id] = struct{}{}
	}
	for _, node := range fragment.Nodes {
		nodes[node.ID] = struct{}{}
		for _, evidenceID := range node.EvidenceIDs {
			evidence[evidenceID] = struct{}{}
		}
	}
	for _, edge := range fragment.Edges {
		for _, evidenceID := range edge.EvidenceIDs {
			evidence[evidenceID] = struct{}{}
		}
	}
	for _, record := range fragment.Evidence {
		if _, referenced := evidence[record.ID]; !referenced {
			t.Fatalf("orphan evidence %s survived node-bound filtering", record.ID)
		}
	}
	for _, edge := range fragment.Edges {
		_, fromNode := nodes[edge.From]
		_, fromExternal := external[edge.From]
		_, toNode := nodes[edge.To]
		_, toExternal := external[edge.To]
		if (!fromNode && !fromExternal) || (!toNode && !toExternal) {
			t.Fatalf("edge %s references a rejected or unknown endpoint: %s -> %s", edge.ID, edge.From, edge.To)
		}
	}
}

func TestExtractDeterministicOrdering(t *testing.T) {
	files := map[string]string{
		"a.go":                    "//go:build linux\n\npackage a\n",
		"b.go":                    "//go:build darwin\n\npackage b\n",
		".github/workflows/w.yml": "name: ci\njobs:\n  build:\n    steps:\n      - run: make\n",
	}
	first, _ := configenvFixture(t, files)
	second, _ := configenvFixture(t, files)
	if len(first.Nodes) != len(second.Nodes) || len(first.Edges) != len(second.Edges) {
		t.Fatalf("determinism: %d/%d nodes, %d/%d edges", len(first.Nodes), len(second.Nodes), len(first.Edges), len(second.Edges))
	}
	for index := range first.Nodes {
		if first.Nodes[index].ID != second.Nodes[index].ID {
			t.Fatalf("node ordering differs at %d", index)
		}
	}
}
