# Security model

## Assumptions

A repository is hostile input. It may contain malicious symlinks, malformed
syntax, enormous files, archive bombs, hostile HTML, prompt-injection text,
credential material, generated-code explosions, package-manager hooks, and build
scripts designed to execute during “helpful” analysis.

The default scan must therefore remain useful without executing repository code,
installing dependencies, running builds, contacting external hosts, or loading
untrusted plugins with ambient authority.

## Protected assets

- repository source and secrets;
- user credentials and Git helpers;
- host filesystem outside the materialized repository;
- process environment;
- local model prompts and responses;
- canonical snapshots and evidence integrity;
- plugin registry and lockfiles;
- multi-tenant service data and audit records;
- release artifacts and provenance.

## Trust boundaries

```text
untrusted repository bytes
  -> acquisition/inventory boundary
  -> parser/plugin boundary
  -> canonical validation boundary
  -> derived rendering/model boundary
  -> local API/MCP boundary
  -> optional service/tenant boundary
```

Each boundary validates structure and enforces resource and capability policy.
No downstream component treats repository text as instructions.

## Current controls

- repository paths are resolved and constrained to the selected root;
- Git prompts and hooks are disabled during remote acquisition;
- plaintext `git://`, URL query/fragment metadata, and inline HTTPS
  credentials are rejected; Git protocol helpers are denied by default;
- repository origins are canonicalized without userinfo, query strings, or
  fragments before identity, validation, persistence, or export;
- the default `paths-relative` publication boundary removes the absolute
  repository root and absolute atlas/store metadata while preserving portable
  repository-relative citations; `redacted` additionally removes the public
  Git origin, source reference, and repository-node origin fields before any
  atlas or durable snapshot publication;
- Git administrative data is always excluded from pipeline inventories, even
  when a programmatic caller omits the CLI exclusion defaults;
- `file://` transport is denied unless explicitly enabled;
- repository file count, aggregate bytes, text bytes, plugin output, stderr, and
  time are bounded;
- project code and package-manager lifecycle scripts are not executed;
- likely secrets become graph findings and are masked in normalized exports;
- model packets contain redacted bounded excerpts;
- `llama-cli` is invoked directly, not through a shell;
- model and plugin environments are sanitized;
- priority admission receipts expose only process IDs and a fixed workload
  class, never another process's command line, arguments, prompts, or paths;
- the Python-isolation doctor discards user-manager environment output and
  reports only a bounded reachability result and exit status;
- model output must be structured and cite packet evidence;
- generated HTML uses controlled templates and browser security headers;
- API responses are non-cacheable and same-origin resource protected;
- `serve` requires explicit `--allow-remote` acknowledgement before any
  non-loopback bind; the command workbench cannot use that exception;
- plugin artifacts and manifests are digest locked;
- canonical output is validated before publication;
- Docker reference deployment is read-only, drops capabilities, and applies
  `no-new-privileges`.

### Generated-output publication

RKC binds every destructive replacement to the exact directory inode, marker,
manifest, file path identities, sizes, and SHA-256 digests that it validated.
On Linux, replacement requires `renameat2(RENAME_EXCHANGE)`, and first
publication uses `RENAME_NOREPLACE`; RKC fails closed if either the kernel or
backing filesystem cannot provide that primitive. The target pathname therefore
has no `ENOENT` interval during Linux force replacement. A durable, owner-only
sibling `.rkc-quarantine-*/journal.json` records both directory identities and
snapshot bindings before exchange. A later publication performs a bounded scan
and completes only an unambiguous, fully revalidated interrupted transaction.
Ambiguous state is retained for operator inspection rather than deleted.

On non-Linux systems, no portable standard-library directory-exchange primitive
exists. RKC retains and fully revalidates the prior output in quarantine until
the exact staged inode is published and verified, but the two portable renames
necessarily create a bounded target-name absence interval. The exported
`safeoutput.ReplacementPlatformDescription` reports this residual. It is an
availability limitation, not authorization to delete an unverified path; all
identity and manifest checks remain fail closed.

## Current limitations

The digest-pinned built-in Python AST worker still runs as the invoking OS user.
On Linux its cgroup, environment, network-syscall, task-count, and cancellation
limits are enforced fail closed, but it does not yet have a mount/filesystem
namespace. External Python/native workers are disabled. The local HTTP server
has no authentication and is intended for loopback use. The secret scanner is
high-signal pattern detection, not a complete data-loss-prevention product.

The static `scratch` reference container has no shell, package manager, Python,
or user-systemd manager. Its documented portable scan profile therefore
disables Python explicitly with `--no-python`; it does not weaken the worker
policy or silently execute Python without a sandbox. Python AST extraction
currently requires a supported Linux host.

These limitations prohibit describing the reference release as a hardened
multi-tenant service.

## Production plugin isolation

### WASI path

The host should provide only:

- preopened read-only repository descriptors or artifact streams;
- a private bounded temporary directory when granted;
- deterministic clock/random functions when explicitly requested;
- bounded output channels;
- cancellation and fuel/CPU limits.

No ambient network, environment, process spawning, or host filesystem should be
available.

### Native worker path

On Linux, use a dedicated worker launcher with user/mount/PID/network namespaces,
read-only bind mounts, tmpfs workspace, seccomp, no-new-privileges, cgroup v2
memory/CPU/PID limits, rlimits, and a sanitized environment. Equivalent platform
containment is required on macOS and Windows.

Native workers must never inherit repository credentials or Git configuration
unless the policy explicitly grants them.

## Acquisition policy

Remote acquisition should:

1. parse and canonicalize URLs before identity or logging;
2. reject unsupported or plaintext schemes and inline secret-bearing metadata;
3. disable interactive prompts;
4. disable hooks and system/global configuration;
5. avoid LFS smudge unless explicitly authorized;
6. pin the requested commit/ref in the snapshot;
7. constrain submodules by allowlist and depth;
8. materialize into an ephemeral private directory;
9. verify the resulting worktree remains within the materialization root;
10. delete materialization unless retention is requested.

The raw acquisition operand exists only in memory while Git is invoked. Public
snapshot provenance uses one canonical, credential-free origin. Local path and
`file://` remotes are operational locations and are omitted from portable
repository identity.

## Workspace privacy modes

`workspace.privacy_mode` is enforced at the publication boundary after the
canonical scan has completed and before SQLite, filesystem snapshot state, or
atlas export begins:

- `paths-relative` is the default. Persistent snapshots omit
  `snapshot.root_path`, and publication records omit absolute repository and
  atlas locations. Repository-relative artifact/evidence paths and a canonical,
  credential-free remote origin remain available for grounding and portable
  repository identity.
- `redacted` applies the same path boundary and also removes
  `snapshot.git.origin`, `snapshot.metadata.source_reference`, and repository
  node origin fields. Repository and snapshot IDs remain opaque stable hashes,
  so graph relationships and repeatability survive without publishing the
  source location.
- `full` explicitly permits machine-local operational paths and canonical
  origin metadata in durable state. It does not disable secret scanning or
  normalized-source redaction.

Every transformed bundle is revalidated and its coverage/deterministic digest
is rebuilt before publication. The terminal may still print the path selected
by the operator; that display is not copied into non-`full` durable metadata.

Archives require bounded entry count, decompression ratio, total bytes, nesting,
and path containment before production support is enabled.

## Compiler-semantic index input

`--scip-index` accepts compiler-produced SCIP as untrusted inert data. RKC does
not invoke the compiler, package manager, language server, or repository build
that produced it. The importer rejects symbolic-link inputs, unsafe document
paths, documents absent from the inventoried source tree, malformed Protobuf,
ambiguous source encodings, invalid ranges, and bounded-resource violations.
It hashes the index before scheduling, verifies the digest while streaming it,
and hashes it again before merge so verified cache reuse cannot hide a changed
external input. The semantic digest enters snapshot identity and every imported
fact retains its compiler/indexer provenance.

Generating an index may execute project-specific build logic and is outside the
normal-scan trust boundary. Operators must run that separately authorized step
under controls appropriate to the repository. See
[`SCIP_SEMANTIC_ADAPTERS.md`](SCIP_SEMANTIC_ADAPTERS.md).

## Secret handling

Secret findings retain kind, source location, confidence, and a non-reversible
fingerprint. Raw values are not written to diagnostics or graph attributes.
Normalized export masks values while preserving byte length where practical, so
line/source maps remain valid.

Cloud or remote model providers require a separate egress policy, approved host
allowlist, secret scan, repository-owner consent, retention policy, and audit
record. Local mode denies model egress.

## Prompt injection

Repository text is inserted into a delimited untrusted-data field. It cannot
alter system policy, tool permissions, evidence requirements, output schema, or
network settings. Claims referencing evidence outside the packet are rejected.

## Local API

The reference server should bind to loopback by default. Exposing it to another
host requires a trusted reverse proxy and authentication. Production service
mode requires OIDC, organization/workspace boundaries, RBAC, rate limits,
request size limits, audit, and row-level authorization.

## Supply chain

Production releases require:

- pinned dependencies and base-image digests;
- reproducible build metadata;
- source and binary checksums;
- signed binaries and containers;
- SPDX or CycloneDX SBOMs;
- SLSA-compatible provenance;
- plugin signatures and transparency records;
- model-file digests and license metadata;
- dependency, source, container, and secret scanning;
- protected release workflow and two-person approval.

## Security release gates

A production release fails if:

- any sandbox escape fixture succeeds;
- plugin undeclared network/process/environment access succeeds;
- a path can escape repository/materialization boundaries;
- normalized public export contains a high-confidence unapproved secret;
- an unauthenticated tenant resource can be read;
- canonical claims can be changed by repository prompt text;
- plugin or model artifact digest is not verified;
- an interrupted write publishes a partial snapshot;
- release signatures, SBOM, or provenance are missing.
