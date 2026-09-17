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
repository tags and reads reserved manifests.
This scales with retained history; optimizing it must preserve legacy detection.

## Risks / limitations
SHA-256 collisions remain theoretically possible; stored identity checks
reject observed mismatches. Checks are not atomic with writes. Old binaries,
manual registry changes and malicious writers cannot be fenced: migration
requires all writers stopped, and mixed-version use is unsupported. No new
CAS or distributed-lock guarantee is claimed. Deletion/retention remain
subject to the existing registry-specific behavior.

## Verification
Regression tests for aliasing, final tag length, lifecycle isolation, legacy
rejection, identity mismatch, failed scans and Zot integration. Exact test
results are recorded in the PR.

## References
- #39: isolation and tag bounds
- #30: storage format
- docs/guides/workspace-migration.md
