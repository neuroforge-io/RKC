# Keep source knowledge current

A workspace registers folders and optional remote Git sources, compiles each into
an immutable atlas, and keeps those atlases up to date for
[MCP clients](MCP.md). The registry and generated content stay in an owner-private
directory outside the sources. RKC never pulls into or writes to a registered
local folder.

```sh
rkc workspace add --workspace "$HOME/.local/share/rkc/workspaces/projects" \
  --id application --label "Application" /absolute/path/to/application
rkc workspace add --workspace "$HOME/.local/share/rkc/workspaces/projects" \
  --id documentation /absolute/path/to/documentation
rkc workspace sync --workspace "$HOME/.local/share/rkc/workspaces/projects"
rkc workspace list --workspace "$HOME/.local/share/rkc/workspaces/projects"
rkc workspace watch --workspace "$HOME/.local/share/rkc/workspaces/projects" \
  --interval 60s --timeout 10m
```

Use an equivalent absolute directory in PowerShell. All flags precede the optional
source alias or required source path. `sync` and `watch` accept one alias to limit
the operation. Stop a foreground watch with Ctrl+C. Each watch pass finishes before
the next delay begins; checks never overlap. The minimum polling interval is 30
seconds and the maximum is 24 hours. The per-source timeout accepts 1 second to 1
hour. A failed pass is reported and retried after the interval.

Point `rkc-mcp --workspace` at the generated **registry file**, not its directory:

```sh
rkc-mcp --workspace /absolute/private/workspace/registry.json
```

The MCP server reloads the registry when serving requests. It lists safe aliases,
snapshot identities, and freshness summaries without publishing private source
paths. See [MCP configuration and repository selection](MCP.md) for client setup.
`workspace list --json` deliberately prints the full private registry for local
automation; do not publish that output.

## What a refresh means

RKC hashes admitted regular files under the same policy used for compilation.
Uncommitted edits, untracked files, additions, and deletions count. File modification
time alone does not rebuild an atlas. Symlinks are recorded without following them;
oversized files retain their inventory disposition and metadata rather than being
read without a bound. This built-in profile disables Python and external indexers;
Git metadata inspection is explicitly unavailable. Commit-only changes with
identical admitted content do not require recompilation.

Each source is checked sequentially. Unchanged content reuses a verified active
atlas and the incremental stage cache helps changed content. A new generation must
pass integrity, inventory accounting, symbol evidence, claim citation, error, and
high-confidence secret gates. RKC checks the source again before switching the
registry pointer. Changes during compilation, failures, and cancellations leave
the last good atlas available.

The registry reports `pending`, `stale`, `current`, or `error`, along with the last
check/update time and a fixed error code. `current` means the source matched at its
last check; it does not promise continuous observation. If resource admission
blocks a pass, the prior check time remains visible. Linux work uses the existing
[resource admission policy](RESOURCE_GUARD.md), including user-systemd and cgroup
requirements. Local folder compilation is also available on supported macOS and
Windows hosts using the built-in profile.

The active and previous successful generations are retained. Older generations
are removed only after publication and only when no reader is loading them.
Unreadable, unmarked, replaced, or reader-pinned directories are preserved for a
later pass or operator inspection. Kernel writer leases prevent concurrent syncs;
process exit releases a lease without guessing that a lock file is abandoned.
The stage cache and run journal are separate from atlas retention; inspect and
prune them with the existing `cache` and `runs` tools as appropriate.

## Bound a large source explicitly

By default each source permits 100,000 encountered paths, 20 GiB of encountered
regular-file bytes, files up to 64 MiB for hashing, and text up to 2 MiB for parsing.
Use positive `--max-files`, `--max-repository-bytes`, `--max-file-bytes`, and
`--max-text-bytes` values to lower these limits. Product ceilings are 500,000 paths,
20 GiB per source, 1 GiB per file, and 8 MiB per text file. Zero never disables a
bound. A workspace supports up to 32 sources.

```sh
rkc workspace add --workspace /absolute/private/workspace --id research \
  --exclude downloads \
  --exclude-pattern '**/node_modules/**' \
  --exclude-pattern '**/*.pt' \
  --exclude-pattern '**/*.safetensors' \
  /absolute/path/to/research
```

`--exclude` accepts an exact repository-relative path and its descendants, with
up to 512 explicit exclusions including defaults. `--exclude-pattern` accepts up
to 128 slash-separated patterns: `*`, `?`, character classes, and a whole `**`
segment for zero or more path segments. Quote patterns so the shell does not
expand them. Matches are resolved using bounded filesystem metadata before file
hashing; new matching artifacts are therefore excluded on later passes too.
Matching directories are pruned and recorded as one explicit inventory exclusion.
Unmatched reports and documents remain admitted. RKC does not trust repository
ignore files or silently increase limits after a failure.

## Track a remote source separately

Remote acquisition is an explicit opt-in and currently requires protected Linux
execution. It uses a separate managed temporary checkout on every pass, then
removes that checkout. Local working trees and their dirty changes remain untouched.
No force pull, reset, project hooks, dependency installation, submodule execution,
or project code execution is requested.

```sh
rkc workspace add --workspace /absolute/private/workspace --id upstream \
  --remote --ref main https://github.com/example/project.git
rkc workspace sync --workspace /absolute/private/workspace upstream
```

Omitting `--ref` tracks the remote default branch. A fixed commit tracks that fixed
revision. HTTPS and SSH URLs must be credential-free: passwords, HTTPS usernames,
query strings, and URL fragments are rejected. SSH URLs may use the conventional
`git` username. Configure host authentication separately; RKC does not store
credentials or prompt for them in the registry. An authentication or network
failure preserves the active atlas and reports a bounded error state.

## Run a user-owned watcher

The foreground command is the portable starting point. On Linux, a user service
can restart it after a session interruption. Substitute actual absolute paths:

```ini
[Unit]
Description=Refresh my RKC source workspace

[Service]
ExecStart=/absolute/path/to/rkc workspace watch --workspace /absolute/private/workspace --interval 60s --timeout 10m
Restart=on-failure
RestartSec=30
UMask=0077
Nice=19
Environment=GOMAXPROCS=1
Environment=GOMEMLIMIT=512MiB

[Install]
WantedBy=default.target
```

RKC does not install a daemon or change service policy automatically. Keep the
workspace, service logs, and client configuration private. A watch is a polling
process, not a background task after its process has exited.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed works must preserve applicable license and `NOTICE`
terms. Third-party materials retain their own licenses and ownership._
