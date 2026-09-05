# GitHub source acquisition

This internal client uses GitHub's REST API and ZIP archives. It never invokes
Git, a shell, an indexer or repository code, and does not discover environment,
CLI or stored credentials. `New` takes an optional token explicitly; a caller
must keep the client in memory and require an intentional account connection.

`Search` accepts a query and a one-based page, with 25 results per page. It
exposes GitHub's whole count, a next-page number and an incomplete flag. GitHub
limits search to the first 1,000 results; this is reported as incomplete rather
than implying a complete repository catalogue. Blank queries require the user
to enter a search. Anonymous calls support public repositories; account calls
can use GitHub's private-repository search qualifiers within their permissions.

`Materialize` requires an existing empty directory owned by the calling user.
On Unix it must have no group/other permission bits; on Windows the client
requires an identity-bound protected current-user DACL using `privatepath.CheckDir`.
Ownership or Unix mode bits alone are insufficient. The client resolves platform parent-path
aliases, refuses a symlink destination, stages into a random child, and publishes
under the canonical repository basename with a no-replace rename. Use the
returned `Checkout.Root`; do not reconstruct its path. A failed operation cleans
its staging files and preserves independently created caller files.

The default branch is resolved to a 40- or 64-character commit SHA before the
archive request. `Checkout` records the resolved repository, revision, exact
received archive SHA-256 and compressed bytes. It is an extracted snapshot,
not a Git working tree. The pipeline accepts the trusted receipt through
`Options.ArchiveProvenance` with `SkipGitInspection` and canonical `Origin`.

Limits are 128 MiB compressed, 512 MiB expanded, 32 MiB per file and 50,000 paths,
with bounded metadata, timeouts and cancellation. ZIP central-directory records
are checked before allocation. ZIP64, encrypted or multipart ZIPs, symlinks,
special files, path escapes, duplicate/case-colliding paths and unsafe Windows
names are rejected explicitly. Extraction creates owner-only ordinary files;
archive executability is not retained.

Requests and redirects allow only HTTPS `api.github.com` and
`codeload.github.com`. The account token is sent only to `api.github.com`.
Cookies and referring URLs are removed on redirects, and transport/remote error
bodies are not included in user-facing errors. Client formatting redacts the
credential. The implementation follows the official
[archive API](https://docs.github.com/en/rest/repos/contents#download-a-repository-archive-zip)
and GitHub's guidance to
[pin archives to a commit](https://docs.github.com/en/repositories/working-with-files/using-files/downloading-source-code-archives).

Tests use an injected transport and generated local ZIP fixtures; they do not
contact private repositories. Native Linux race tests and Windows/macOS compile
checks cover the portable API; cross-compilation is not a native platform smoke.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
