# Understand analysis depth

RKC combines source text, syntax facts, compiler-produced relationships, and
explicit uncertainty in one atlas. The depth depends on the language and the
inputs you supply. A searchable file is useful evidence, but it does not imply
that every symbol or runtime behavior has been understood.

## Choose the right level

| Source | Default portable compilation | Deeper analysis |
| --- | --- | --- |
| Go | Standard-library AST: declarations, signatures, types, comments, imports, and call candidates; bounded control/value flow | Import a validated `scip-go` index for compiler-resolved definitions and references |
| JavaScript, JSX, TypeScript, TSX | Built-in tokenizer and syntax extractor: declarations, imports, exports, routes, and relationship candidates | Import a validated `scip-typescript` index |
| Python | Inventory and searchable source; the separate Python AST worker remains disabled in public direct workflows until its aggregate resource ceiling is proved | Import a validated `scip-python` index; installing Python alone does not enable that worker |
| Rust | Inventory and searchable source | Import a validated SCIP index produced by `rust-analyzer` |
| C, C++, CUDA | Inventory and searchable source | Import a validated `scip-clang` index; generation needs a compilation database |
| Java, Kotlin, Scala | Inventory and searchable source | Import a validated `scip-java` index using the project's build workflow |
| C#, Visual Basic | Inventory and searchable source | Import a validated `scip-dotnet` index using the solution workflow |
| Ruby | Inventory and searchable source | Import a validated `scip-ruby` index |
| Other admitted text languages | Inventory, source search, and any applicable manifest/configuration extractors | Add a producer through the [plugin contract](plugin-sdk.md), or import a compatible validated SCIP index |

Built-in syntax extractors do not type-check the project or establish complete
runtime call graphs. Framework extraction adds supported API, CLI, environment,
manifest, schema, and configuration surfaces independently of compiler indexing.
Dynamic dispatch, reflection, generated code, unavailable dependencies, and
environment-specific behavior can still leave unresolved relationships.

## Get useful results from a messy repository

Start with an ordinary compilation. It needs no model or project build:

```sh
rkc quickstart ./my-project
rkc query --dir ./my-project/.rkc "authentication"
rkc context --dir ./my-project/.rkc --format markdown "How does authentication work?"
```

Inspect **Coverage** for artifact dispositions and analysis depth. Use
**Diagnostics** to inspect parser errors and missing inputs. A malformed file
or unsupported construct should remain visible in accounting; it must not
silently become a claim of complete analysis. Use **Explore** for signatures
and source evidence, and **Graph** to distinguish declared, inferred, resolved,
and unresolved relationships.

For mixed-language monorepos, collect indexes from the relevant project roots
in your existing build environment, then import them together:

```sh
rkc quickstart \
  --scip-index ./indexes/go.scip \
  --scip-index ./indexes/typescript.scip \
  --scip-index ./indexes/python.scip \
  ./my-project
```

Index paths above are placeholders for already-produced indexes. RKC validates
source bindings, positions, symbols, and bounds before admitting them. It
rejects incompatible or mismatched inputs. An external producer's name does
not by itself authenticate the producer or establish runtime correctness.

Use `rkc scip languages` to see indexer routes, and
[compiler-grade semantic adapters](SCIP_SEMANTIC_ADAPTERS.md) for pinning,
generation, import, and exact compatibility requirements. RKC does not install
indexers automatically. Explicit index generation may execute project tooling;
ordinary import processes the index as inert data.

## Extend without changing the evidence contract

Language producers emit bounded graph fragments with stable IDs, source ranges,
diagnostics, and explicit resolution classes. Core validation admits those
records before they reach exports, search, HTTP, MCP, or optional model answers.
See [the architecture](ARCHITECTURE.md), [the data model](data-model.md), and
[adapter contribution requirements](../CONTRIBUTING.md#adapter-contributions).

Model output uses the same evidence. Choosing a more capable model does not
repair missing source facts or turn an inferred relationship into a
compiler-resolved one. Keep unsupported behavior and missing evidence visible
when sharing context with an assistant.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
