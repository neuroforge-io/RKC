# Security policy

RKC has not received an independent production security audit. Normal scans do
not execute repository code. The built-in Python adapter is digest-pinned and
is enabled only when the Linux user-systemd isolation and resource controls can
be proved; unsupported hosts fail closed. It still runs with the invoking
user's filesystem authority and does not provide a mount namespace, so use it
only for repositories you trust. Third-party native workers remain disabled.

The complete implemented controls and residual boundaries are maintained in
[`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md) and
[`docs/IMPLEMENTATION_STATUS.md`](docs/IMPLEMENTATION_STATUS.md).

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

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
