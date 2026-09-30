# ADR-0002: Bounded workspace identifiers and explicit migration

## Status
Proposed (implemented for review in #39; maintainer acceptance pending).

## Context
The legacy mapper aliases `a/b` and literal `ws-c14cddc033f64b9d` and
validates names before prefixes and version suffixes are added.

## Decision
Hash every workspace's exact UTF-8 bytes using full SHA-256, lowercase hex.
Keep `state-`, `stver-`, `locked-`, and `unlocked-` prefixes. Historical tags
end in `-v<N>`. This experimental provider does not need a versioned tag
namespace. Preserve the original-name annotation on all manifests.

Before every public operation, enumerate all reserved repository tags and
verify both the complete tag and the original-name annotation. A 64-character
legacy literal cannot be recognized safely by shape alone: its annotation
must hash to the tag identifier. Missing/contradictory annotations fail closed.
Do not cache this check. Listing failures propagate; a missing repository is
allowed. Existing installations must migrate explicitly to an empty repository.
Reject legacy and mixed repositories, including lock-only/history-only cases,
rather than silently opening a new empty state. See the migration guide.

No dual-write mode, automatic destructive migration or new CLI is introduced.
This is a breaking experimental layout change requiring an upgrade notice.

## Alternatives considered
- Preserve literal safe names and hash the rest: leaves a reserved namespace
  problem and more compatibility branches.
- Versioned prefixes: unnecessary format versioning at this experimental stage;
  keep existing prefixes and verify original-name annotations instead.
- Automatic legacy fallback: risks splitting state and bypassing old locks.
- Reversible encoding: arbitrary-length names cannot fit bounded OCI tags.

## Consequences
Every workspace (including default) changes tags. Maximum generated tag
length is bounded independently of name length. All workspaces require the
original-name annotation for access and listing; missing or contradictory
metadata is an error rather than a guessed name. Each public operation scans
repository tags. The amendment below bounds the manifest reads.

## Amendment 1: scoped preflight (#48)
Status: Accepted by the maintainer.

Measurement (#48): the repository-wide preflight cost `1 + 2 × reserved tags`
registry calls per operation, 119 of 122 calls for a `Get` with 5 workspaces
and `max_versions = 10`. A contradictory annotation in any workspace also
blocked every other workspace.

Decision:
- Every public operation still enumerates all repository tags and rejects any
  reserved tag whose identifier is not 64 characters (legacy/mixed detection
  by name, no manifest reads).
- Only the requested workspace's `state-`, `locked-` and `unlocked-` tags
  have their original-name annotation read and hashed. This keeps the
  hash-shaped legacy literal case covered for the workspace being operated on.
- `stver-*` manifests are verified on use, wherever their numbers or digests
  are trusted: an existing destination version tag before `Put` overwrites
  it, the allocation fallback when the state lacks a version annotation, and
  retention (every version before the keep/delete cutoff; deleting a digest
  removes every tag pointing at it). A mismatch fails that write or skips
  pruning. Only canonical version numbers up to 2^30 are recognized, and
  versions beyond that are refused before writing.
- Listing keeps the full repository-wide annotation check.

Cost: one listing plus at most three Resolve+Fetch pairs, independent of the
number of workspaces and retained history (`TestWorkspacePreflightCallsBounded`).
A versioned `Put` adds one destination check. Background retention reads each
retained version manifest.

Accepted trade-offs:
- A repository whose only legacy tags are 64-character literal names of
  other workspaces is no longer rejected by operations on an unrelated
  workspace. Listing still rejects it, and so does any operation on the
  affected workspace's mutable tags.
- Corrupted history surfaces only when used: reads and locks of the same
  workspace can still succeed.

In exchange, a corrupted workspace no longer blocks unrelated workspaces
(`TestWorkspaceForeignIdentityMismatchIsolated`). No caching, CAS or fencing
is introduced. Checks remain non-atomic with writes.

## Risks / limitations
SHA-256 collisions remain theoretically possible; stored identity checks
reject observed mismatches. Checks are not atomic with writes. Old binaries,
manual registry changes and malicious writers cannot be fenced: migration
requires all writers stopped, and mixed-version use is unsupported. No new
CAS or distributed-lock guarantee is claimed. Deletion/retention remain
subject to the existing registry-specific behavior.

## Verification
Regression tests for aliasing, final tag length, lifecycle isolation, legacy
rejection, identity mismatch, failed scans and Zot integration. Amendment 1
adds `TestWorkspacePreflightCallsBounded`,
`TestWorkspaceForeignIdentityMismatchIsolated`,
`TestWorkspaceVersionIdentityVerifiedOnUse` and
`TestWorkspaceUnverifiedVersionsNotTrusted`. Exact test results are recorded
in the PR.

## References
- #39: isolation and tag bounds
- #48: scoped preflight (Amendment 1)
- #30: storage format
- docs/guides/workspace-migration.md
