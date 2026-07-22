# Security policy

The reference implementation is an architectural scaffold and has not received
a production security audit. Its Python plugin is an unsandboxed child process.
Use it only on repositories and plugin code you trust.

## Reporting

Report vulnerabilities privately to the maintainers through the repository's
security-advisory mechanism. Include affected version, reproduction steps,
impact, and any suggested mitigation. Do not include third-party source code or
credentials in the report.

## Production security invariants

A stable RKC release must enforce:

- repository path containment;
- no project-code execution by default;
- no plugin network by default;
- capability-scoped WASM plugins;
- isolated native workers;
- output schema and graph validation;
- resource limits and cancellation;
- sanitized Markdown and HTML;
- prompt-injection separation;
- secret-aware model and export policy;
- signed releases and verifiable plugin bundles;
- tenant and cache isolation;
- audit logging.

See the security section of `docs/implementation-plan.md` for the full threat
model and control set.
