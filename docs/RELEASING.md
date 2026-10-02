# Release packaging and provenance

Release packaging is separate from installing the CLI, starting sessions, and
publishing a GitHub release. Building archives does none of those things. Creating
or pushing a tag and publishing artifacts are separate operator-authorized
actions.

The existing v0.1.2 and v0.1.3 releases are source-only. Their runtimes report
`0.2.0-dev` and `0.1.3`, respectively, without commit/date stamping. Keep both
tags and releases intact. The first intended packaged release is v0.1.4, after
the packaging changes are merged. Check the published assets rather than assuming
that an intended tag already has binaries.

## Artifact contract

A release produces four archives, using the full version including its leading
`v`. For a v0.1.4 release they are:

```text
cross-session-codex_v0.1.4_linux_amd64.tar.gz
cross-session-codex_v0.1.4_linux_arm64.tar.gz
cross-session-codex_v0.1.4_darwin_amd64.tar.gz
cross-session-codex_v0.1.4_darwin_arm64.tar.gz
release.json
SHA256SUMS
```

Archives contain these paths at their root (the release guide is included when
present in the source tree):

```text
cross-session-codex
README.md
docs/LAUNCH_HANDSHAKE.md
docs/RELEASING.md
```

The messaging skill is embedded in the binary and written by the existing
`install` command. There is no separate skill file in the archive.

`release.json` uses `schema_version: 1`. It identifies the project, version, and
release tag where applicable, full `source_commit`, `source_dirty`, whether this
is a `release`, commit-derived `build_date`, and `go_version`. Its `targets` record
each OS, architecture, archive name, `archive_sha256`, `binary_sha256`, and archive
size in bytes. `SHA256SUMS` covers all four archives and `release.json`; it does
not include itself.

Compare downloaded files with their published hashes before extracting or
executing them. These hashes detect disagreement with the published checksums,
not compromise of the release publisher or both files together. Pin the release
tag, source commit, and relevant hashes in an external fleet manifest when
deployment needs durable provenance.

The workflow does not Developer ID sign or notarize macOS binaries. Go may apply
ad-hoc signing; this is not an independently authenticated publisher signature.

## Runtime build metadata

The `version` command retains its existing runtime fields and adds `commit` and
`build_date`. Make-managed builds stamp the full Git commit and that commit's UTC
timestamp. `build_date` is not the wall-clock time at which a machine compiled the
binary. Native and cross-platform Make builds select their version as follows:

- A clean exact valid release tag becomes the version, including the leading `v`.
  Multiple valid tags on that commit are ambiguous and fail closed.
- Other Make builds use `v0.0.0-snapshot.<12-character-commit>`, with `.dirty` for a
  dirty working tree. They are not release artifacts.
- Plain `go build` uses development defaults. Do not interpret that output as a
  version-stamped release build.

Explicit snapshot packaging always uses a snapshot version and `release: false`,
even at a clean tagged commit. Explicit release packaging selects the requested
exact tag and sets `release: true`; it is not inferred from a native binary's
version label.

The source commit alone does not fully identify dirty source. A dirty snapshot's
label and `source_dirty` field make that limitation visible; its binary/archive
hashes identify the actual resulting bytes. Do not publish a snapshot as a release.

`capabilities` remains the compatibility contract for integrations. A companion
requiring the launch handoff checks `cli_api: 1` and `launch-handshake-v1`, not just
a version comparison.

## Local preparation without publishing

Use the pinned Go 1.27.1 toolchain, Git, and Make. Run the checks before preparing
artifacts:

```sh
make check
make build
make cross-build
```

To package a preview from the current working tree, including its dirty label:

```sh
make release-snapshot
```

The default output is `dist/snapshot`. It must not already exist. To retain a
previous result and prepare another, choose a new output path:

```sh
make release-snapshot SNAPSHOT_DIR=dist/snapshot-review-2
```

A release build requires a clean working tree and an already existing valid
release tag pointing exactly at HEAD. `vX.Y.Z` below must be replaced with that
actual tag; the command does not create or move it:

```sh
make release TAG=vX.Y.Z
```

The default output is `dist/release`, also required not to exist. An alternate
new directory can be selected explicitly:

```sh
make release TAG=vX.Y.Z RELEASE_DIR=dist/release-review-2
```

The underlying Go helper provides the same packaging operation:

```sh
GOTOOLCHAIN=go1.27.1 go run ./cmd/csc-release --tag vX.Y.Z --out dist/release-review-3
GOTOOLCHAIN=go1.27.1 go run ./cmd/csc-release --snapshot --out dist/snapshot-review-3
```

Release mode builds from a private `git archive` export of the validated commit,
so local untracked files and ignored generated output cannot silently enter its
source tree. Native Make builds and snapshot previews use the working tree.
Packaging fails closed on an invalid/mismatched tag, dirty release source, or an
existing destination. Preserve earlier output or choose another path rather than
deleting a workspace to make a build proceed.

Inspect `release.json` and verify all packaged checksums from inside the chosen
output directory:

```sh
sha256sum -c SHA256SUMS
```

On macOS use `shasum -a 256 -c SHA256SUMS`. On a matching host, extract its archive
into a fresh directory and inspect the binary's `version` and `capabilities`.
Compare the version, full commit, build date, Go version, OS, and architecture
with the release metadata. Do not run `install` merely to validate a package.
Cross-compiled binaries can only be executed on compatible hosts.

## Publishing a new tag

The release workflow runs for a new supported `v`-prefixed semantic-version tag.
Only create and push that tag after the intended source has been reviewed and
merged, and publishing has been explicitly authorized. Keep the optional
`.codex-plugin/plugin.json` version aligned with the intended release in that
reviewed source. Do not move v0.1.2 or v0.1.3 or retroactively treat their
source-only releases as artifacts produced by this workflow.

The workflow separates permissions and phases:

1. Read-only jobs check the source and build validated packages.
2. A separate publishing job, with release-write permission, refuses to proceed
   if any GitHub release already exists for that tag, even an empty draft.
3. It creates a draft, uploads the exact archives, manifest, and checksums, then
   publishes the completed release. Existing assets are never overwritten.

Inspect the workflow conclusion and the uploaded manifest/checksums before
declaring a release usable. A successful cross-build by itself is not publication,
and a tag alone does not promise downloadable binaries.

If publication fails after creating a draft, leave its state intact for operator
review. A blind rerun must not bypass the existing-release guard or overwrite
partial assets. Any recovery that changes a draft is a separate authorized
operation; choosing a fresh release version avoids mutating a published tag.

## Installation and upgrades

The [README installation example](../README.md#pinned-binary-installation) selects
an explicit tag and platform, downloads into a fresh directory, verifies both the
chosen archive and manifest, then invokes the binary's existing self-installer.
It neither pipes a downloaded script into a shell nor follows a moving `latest`
download URL.

Installation updates the managed command and optional skill; it does not restart
running workers, replace an app-server, choose a Botbus identity, or change account
authentication. Existing workers retain their executable until deliberately
restarted. Source-only historical releases still require a build from the exact
selected source revision.
