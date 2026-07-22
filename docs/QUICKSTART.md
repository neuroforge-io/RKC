# Quickstart

## 1. Install prerequisites

```sh
go version       # 1.23 or newer
python3 --version # 3.11 or newer
git --version
python3 -m pip install jsonschema PyYAML
```

## 2. Verify the checkout

```sh
make verify
```

This runs formatting, vetting, Go and Python tests, contract validation,
document-link validation, plugin-lock verification, a mixed-language scan,
deterministic replay, HTTP API smoke tests, MCP smoke tests, and remote-Git
acquisition tests.

Run the race detector separately or use the logged release sequence:

```sh
make test-race
make release-verify
```

## 3. Build

```sh
make build
./bin/rkc version
./bin/rkc doctor
```

## 4. Generate configuration

```sh
./bin/rkc init --out rkc.json
```

Edit `rkc.json`, then pass it with `--config rkc.json`. Omit the option to use
safe local defaults.

## 5. Scan a repository

```sh
./bin/rkc scan \
  --config rkc.json \
  --out /tmp/my-atlas \
  --state-dir /tmp/my-atlas-state \
  --force \
  /path/to/repository
```

Remote Git repositories are materialized without prompts or hooks:

```sh
./bin/rkc scan \
  --ref main \
  --clone-depth 1 \
  --out /tmp/remote-atlas \
  --force \
  https://example.invalid/organisation/repository.git
```

Credentials should be supplied through an approved Git credential helper, not
embedded in URLs or configuration files.

## 6. Enforce quality

```sh
./bin/rkc check \
  --coverage /tmp/my-atlas/coverage.json \
  --bundle /tmp/my-atlas/bundle.json \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-edge-resolution 0.5 \
  --min-claim-citation 1 \
  --max-errors 0 \
  --max-high-confidence-secrets 0
```

Edge resolution depends on analyzer precision. The reference syntax adapters
intentionally retain unresolved relations; lower the threshold for dynamic or
unsupported codebases rather than falsifying the denominator.

## 7. Search and browse

```sh
./bin/rkc query --dir /tmp/my-atlas --limit 20 authentication
./bin/rkc serve --dir /tmp/my-atlas --addr 127.0.0.1:8787
```

The static site is also available directly under `/tmp/my-atlas/site`.

## 8. Use MCP

```sh
./bin/rkc-mcp --dir /tmp/my-atlas
```

Example initialization request:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
```

## 9. Construct model evidence packets

Packet-only mode is useful even without a model:

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --task module_summary \
  --packet-only \
  --limit 5 \
  --force
```

With `llama.cpp`:

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --model /models/coder-q4.gguf \
  --llama-cli /usr/local/bin/llama-cli \
  --context 4096 \
  --max-output 768 \
  --max-rss-mib 3584 \
  --limit 5 \
  --force
```

RKC rejects claims that cite unavailable evidence, reference unknown code
identifiers, omit certainty, or violate packet policy.

## 10. Compare snapshots

```sh
./bin/rkc diff /tmp/atlas-before /tmp/atlas-after
```

Use graph commands to inspect a changed node’s impact:

```sh
./bin/rkc impact --dir /tmp/atlas-after --node '<node-id>'
```

## 11. Produce the complete distributable

```sh
make complete-package
```

The package builder refuses to proceed without release verification,
demonstration output, and cross-compiled Linux binaries.
