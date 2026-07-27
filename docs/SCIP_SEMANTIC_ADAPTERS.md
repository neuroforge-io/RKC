# Compiler-grade semantic adapters

RKC imports [SCIP](https://github.com/scip-code/scip), the Apache-2.0,
language-neutral code-intelligence protocol. A compiler or language server
produces an `index.scip`; RKC validates and compiles that inert index into the
same canonical graph used by search, GraphRAG, documentation, SQLite, HTTP,
MCP, and the browser.

This boundary is deliberate. Normal RKC scans never run a package manager,
build script, compiler, language server, or repository executable. Index
generation may execute project-specific tooling and must be separately
authorized by the operator, ideally in CI or an isolated build environment.

## Import an index

```sh
rkc plan --scip-index /absolute/path/index.scip /path/to/repository

rkc quickstart \
  --scip-index /absolute/path/index.scip \
  /path/to/repository
```

For a polyglot workspace, repeat the flag:

```sh
rkc scan \
  --scip-index /indexes/go.scip \
  --scip-index /indexes/web.scip \
  --scip-index /indexes/jvm.scip \
  --no-python \
  --out /tmp/atlas \
  --state-dir /tmp/atlas-state \
  --force \
  /path/to/repository
```

The responsive workbench exposes the same path. Open **Command center**, choose
`quickstart`, `plan`, or `scan`, and include `--scip-index index.scip` in the
argument field. The preview is the exact argument vector; it is never evaluated
by a shell.

After compilation, **Coverage → Semantic parse** reports the semantic artifact
ratio. Compiler facts appear with `compiler_resolved` evidence, the indexer
name/version, the exact index SHA-256, source range, symbol role, and SCIP
symbol. Search and graph views require no special semantic mode.

## Language indexers

RKC consumes the protocol rather than coupling its graph to one compiler. The
following upstream indexers are the primary supported routes:

| Languages | Indexer | Typical project-root command |
|---|---|---|
| Python | [scip-python](https://github.com/sourcegraph/scip-python) | `scip-python index . --project-name=NAME` |
| JavaScript, TypeScript | [scip-typescript](https://github.com/sourcegraph/scip-typescript) | `scip-typescript index` or `scip-typescript index --infer-tsconfig` |
| Go | [scip-go](https://github.com/scip-code/scip-go) | `scip-go` |
| Rust | [rust-analyzer / scip-rust](https://github.com/scip-code/scip-rust) | `rust-analyzer scip .` |
| C, C++, CUDA | [scip-clang](https://github.com/sourcegraph/scip-clang) | `scip-clang --compdb-path=build/compile_commands.json` |
| Java, Kotlin, Scala | [scip-java](https://github.com/sourcegraph/scip-java) | use its Maven, Gradle, or SemanticDB integration |
| C#, Visual Basic | [scip-dotnet](https://github.com/sourcegraph/scip-dotnet) | follow its solution/project workflow |
| Ruby | [scip-ruby](https://github.com/sourcegraph/scip-ruby) | follow its project workflow |

The protocol itself accepts any language string. RKC therefore also consumes
conforming indexes for Dart, PHP, Swift, Objective-C, HTML/XML, CSS, SQL,
protobuf, Solidity, Zig, and other languages. Import support does not imply
that RKC ships or endorses a compiler indexer for every protocol language.
HTML in particular has structural rather than compiler semantics in most
toolchains; RKC will preserve any conforming producer's tag/symbol occurrences
without overstating them as type-checked facts.

Always pin the external indexer and review its license and build behavior in
production automation. RKC does not bundle these tools or absorb their license
terms.

## Imported facts

The adapter currently preserves:

- document language and exact inventoried artifact identity;
- global and document-local SCIP symbol identity;
- symbol kind, display name, signature, documentation, and enclosing symbol;
- definition, import, read, write, generated, test, and forward-definition
  roles;
- references and the smallest compiler-provided enclosing definition;
- implementation, reference-equivalence, type-definition, and
  definition-alias relationships;
- compiler diagnostics, severity, code, message, source, and position;
- legacy packed ranges and modern typed single-line/multi-line ranges;
- UTF-8 byte, UTF-16 code-unit, and UTF-32 code-point positions mapped back to
  exact repository byte offsets.

SCIP highlights function references but does not universally distinguish a
call from every other function reference. RKC therefore records conservative
`references`/`reads` edges instead of manufacturing `calls` edges. This is a
precision feature, not a missing conversion.

## Security and determinism

The importer:

- rejects missing metadata, duplicate metadata, invalid wire types, overflowing
  varints, invalid UTF-8, invalid/negative/reversed ranges, and ambiguous
  position encodings;
- requires canonical repository-relative document paths and an existing
  inventoried regular text artifact;
- rejects symbolic-link index inputs and source path components;
- caps each index at 512 MiB, the aggregate at 1 GiB, the input count at 64,
  and document/message/string/entity counts at bounded limits;
- streams top-level Protobuf fields and bounds every nested allocation;
- hashes each index before scheduling, verifies the same digest while parsing,
  and hashes it again before merge, including verified-cache hits;
- binds the semantic input digest into snapshot identity and cache keys;
- marks only documents proven present in the index as `semantic_parsed`;
- fails the requested scan rather than silently degrading when an explicitly
  supplied index is malformed or changes during compilation.

The adapter has no Protobuf runtime dependency and executes no index content.
Cold, warm-cache, and clean compilation are tested for canonical equality.

## Validate before import

The upstream SCIP CLI can independently lint and inspect an index:

```sh
scip lint /path/to/index.scip
scip stats --from /path/to/index.scip
scip print --json < /path/to/index.scip
```

RKC performs its own strict parsing and containment checks regardless of
whether these optional upstream diagnostics were run.
