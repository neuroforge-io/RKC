package tssyntax

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
)

func TestExtractsTypeScriptDeclarationsImportsCallsAndRoutes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"example-auth","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := `import express, { Request } from "express";
import type { Session } from "./types";

export interface UserStore {
  find(username: string): Promise<string | undefined>;
}

export class AuthService implements UserStore {
  constructor(private readonly prefix: string) {}

  public async login(username: string, password?: string): Promise<string> {
    console.log(username);
    return this.prefix + username + String(password ?? "");
  }

  find(username: string): Promise<string | undefined> {
    return Promise.resolve(username);
  }
}

export const normalize = (value: string = "") => value.trim();

const app = express();
export function mount(): void {
  app.post("/sessions", normalize);
}
`
	path := filepath.Join(root, "auth.ts")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	fragment, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: []pluginapi.FileRef{{ArtifactID: "artifact:auth", Path: "auth.ts", Language: "typescript", SHA256: "hash"}}})
	if err != nil {
		t.Fatal(err)
	}
	wantNodes := map[string]string{
		"UserStore": "interface", "AuthService": "class", "constructor": "constructor", "login": "method",
		"normalize": "function", "mount": "function", "POST /sessions": "api_endpoint",
	}
	found := map[string]string{}
	for _, node := range fragment.Nodes {
		found[node.Name] = node.Kind
	}
	for name, kind := range wantNodes {
		if found[name] != kind {
			t.Fatalf("missing %s %s; got nodes=%+v", kind, name, fragment.Nodes)
		}
	}
	var imports, calls, implements, handles bool
	for _, edge := range fragment.Edges {
		switch edge.Kind {
		case "imports":
			imports = true
		case "calls":
			calls = true
		case "implements":
			implements = true
		case "handles":
			handles = true
		}
	}
	if !imports || !calls || !implements || !handles {
		t.Fatalf("expected imports=%t calls=%t implements=%t handles=%t; edges=%+v", imports, calls, implements, handles, fragment.Edges)
	}
	if len(fragment.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", fragment.Diagnostics)
	}
}

func TestLexerIgnoresCommentsAndPreservesTemplateStrings(t *testing.T) {
	file := pluginapi.FileRef{ArtifactID: "a", Path: "fixture.ts", Language: "typescript"}
	tokens, diagnostics := lex([]byte("// function fake() {}\nconst value = `hello ${name}`;\n"), file)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	var fake, template bool
	for _, token := range tokens {
		if token.text == "fake" {
			fake = true
		}
		if token.kind == tokenString && token.text == "`hello ${name}`" {
			template = true
		}
	}
	if fake || !template {
		t.Fatalf("unexpected tokenization: fake=%t template=%t tokens=%+v", fake, template, tokens)
	}
}

func TestTierOneDetailsRemainConservativeAndEvidenceBacked(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"example"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := `import { Item } from "./types";
export interface Store {
  get(id: string): Promise<Item>;
}
export enum Status {
  Ready = "ready",
  Retry = computeRetry(),
  Done,
}
export async function load(id: string): Promise<Item> {
  return fetchItem(id);
}
`
	if err := os.WriteFile(filepath.Join(root, "index.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	fragment, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: []pluginapi.FileRef{{ArtifactID: "artifact:index", Path: "index.ts", Language: "typescript", SHA256: "hash"}}})
	if err != nil {
		t.Fatal(err)
	}

	var projectEvidence, interfaceMethod, asyncFunction bool
	enumMembers := map[string]bool{}
	for _, node := range fragment.Nodes {
		switch {
		case node.Kind == "project":
			projectEvidence = len(node.EvidenceIDs) > 0 && node.ArtifactID == "artifact:index"
		case node.Kind == "method" && node.Name == "get":
			interfaceMethod = true
		case node.Kind == "function" && node.Name == "load":
			value, _ := node.Attributes["async"].(bool)
			asyncFunction = value
		case node.Kind == "enum_member":
			enumMembers[node.Name] = true
		}
	}
	if !projectEvidence || !interfaceMethod || !asyncFunction {
		t.Fatalf("projectEvidence=%t interfaceMethod=%t asyncFunction=%t nodes=%+v", projectEvidence, interfaceMethod, asyncFunction, fragment.Nodes)
	}
	for _, member := range []string{"Ready", "Retry", "Done"} {
		if !enumMembers[member] {
			t.Fatalf("missing enum member %s: %+v", member, enumMembers)
		}
	}
	for _, notMember := range []string{"ready", "computeRetry"} {
		if enumMembers[notMember] {
			t.Fatalf("initializer token incorrectly treated as enum member: %s", notMember)
		}
	}
	var relativeImportUnresolved bool
	for _, edge := range fragment.Edges {
		if edge.Kind == "imports" && edge.Resolution == "unresolved" && edge.Attributes["module"] == "./types" {
			relativeImportUnresolved = true
		}
	}
	if !relativeImportUnresolved {
		t.Fatalf("expected relative import to remain unresolved: %+v", fragment.Edges)
	}
}
