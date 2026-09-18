---
page_title: "oras Provider — Architecture & Consistency"
description: |-
  How the oras provider stores state in OCI registries: storage layout, locking model, consistency guarantees, failure behavior, and OCI limitations.
---

# Architecture & Consistency

~> This page documents how the provider **actually behaves**, including race
windows and limitations. It complements the [configuration reference](/index).
The design is formalized in [ADR-0001](../.agents/decisions/0001-locking-model.md)
(generation-based optimistic concurrency).

## 1. Component chain

```
Terraform Core (1.17 alpha, pluggable state storage)
        │  statestore plugin protocol (gRPC, per-RPC instances)
        ▼
internal/statestore  (OCIStateStore: Terraform-specific concerns)
        │  *oras.Client
        ▼
internal/oras        (tag scheme, lock lifecycle, retries, GHCR fallbacks)
        │  oras-go v2.6.2 (go.mod:12)
        ▼
ORAS Go library      (plain OCI REST calls: PUT/GET/DELETE manifests, tags)
        ▼
OCI registry         (ghcr.io, zot tested; others untested)
```

Provider wiring: `provider.Configure` builds the HTTP client and stores a
`*statestore.ProviderData` (`provider.go:99-124`); state-store `Initialize`
turns the `state_store` block into an `oras.Client` (`oci.go:226-288`).

## 2. StateStore lifecycle & StateStoreData

Terraform creates a **fresh `OCIStateStore` per RPC** (`oci.go:42-49`), so
mutable state cannot live on the struct. Instead, `Initialize` produces a
`*stateStoreData` — the client plus a **lock registry** (`StateID → lockID`,
mutex-guarded, `oci.go:55-90`) — and every per-RPC instance restores it in
`Configure` (`oci.go:294-315`). All instances of one store configuration share
ownership tracking; a legacy `*oras.Client` branch wraps it in a per-instance
registry (`oci.go:305-308`).

- `Write` verifies ownership only when a local lock is registered
  (`oci.go:341-353`); no registration ⇒ unverified write (e.g. `-lock=false`).
- `Lock` maps an existing-holder `*oras.LockError` to Terraform's
  "already locked" diagnostic (`oci.go:393-403`).
- `Unlock` refuses an empty LockID (it would release someone else's lock,
  `oci.go:417-424`) and drops the registration only if it still matches
  (`forgetLockIf`, `oci.go:84-90`).

## 3. Configuration

Argument tables: see the [provider reference](/index). Effective defaults and
validation (beyond the table):

| Setting | Effective behavior | Validation |
|---|---|---|
| `lock_ttl` unset / `0` | `lease_expiry` is never written (`client.go:441-444`); locks are **never stale** | non-negative Go duration (`oci.go:162-178`) |
| `max_versions` `0` | No version tags, no allocation, no pruning | `>= 0` (`oci.go:180-188`) |
| `max_state_size` `0` | 256 MiB default applies to **reads and writes** (`client.go:204-209`, 272) | `>= 0` (`oci.go:190-198`) |
| `compression` | Gzip layer, media type `+gzip` (`client.go:279-286`) | — |

Validation runs in `ValidateConfig` and is defensively re-run in `Initialize`
(`oci.go:152-201`, 235) — an unparseable `lock_ttl` is an error, never a silent
zero. Provider-level `plain_http` selects the transport independently of
`tls_skip_verify`/`ca_file`, which configure the shared HTTP client. Both flow
through `ProviderData` from provider `Configure` to state store `Initialize`.
Legacy `insecure` retains its old behavior and is deprecated; see the
[transport upgrade instructions](guides/authentication.md#upgrade-from-insecure).

## 4. Workspace → OCI mapping

Every workspace name maps to the full lowercase SHA-256 digest of its exact
UTF-8 bytes (no normalization). All generated tags are bounded independently
of the name length:

| Tag | Pattern |
|---|---|
| Current state | `state-<sha256>` |
| Version snapshot | `stver-<sha256>-v<N>` |
| Lock | `locked-<sha256>` |
| Unlocked marker | `unlocked-<sha256>` |

The exact original name is required in `org.terraform.workspace` on every
state and lock manifest. Before every public operation, reserved tags and
annotations are checked together across the repository. Legacy, mixed or
ambiguous layouts are rejected. Listing never guesses a missing name.
Migration requires stopped writers and a separate empty repository; see
[workspace migration](guides/workspace-migration.md) and
[ADR-0002](../.agents/decisions/0002-workspace-identifiers.md).

These preflights add tag enumeration and metadata reads proportional to
retained history. They do not make subsequent writes atomic or fence old
binaries racing after validation. The limitations below still apply.

## 5. Storage: current & historical

State and lock manifests are OCI image manifests packed with
`PackManifest v1_1` (`client.go:646,665`):

| Object | Artifact type | Annotations | Layers |
|---|---|---|---|
| State (`client.go:638-650`) | `application/vnd.terraform.state.v1` | workspace, `updated_at` (always), `state.version` (when > 0) | single layer, `application/vnd.terraform.statefile.v1` (`+gzip` when compressed) |
| Lock (`client.go:654-673`) | `application/vnd.terraform.lock.v1` | workspace, lock ID, lock info (JSON), generation data | none |

The lock's generation metadata (`generation`, `lease_expiry`, `holder_id`) is
a JSON blob in the `org.terraform.lock.generation` annotation
(key `client.go:48`, struct `client.go:135-140`, written `client.go:670`).

Versioning (`max_versions > 0` only, `client.go:288-295`):
- Current version read: state-manifest `state.version` annotation preferred;
  fall back to max `stver-*` tag; missing state manifest ⇒ 0
  (`client.go:683-708`).
- Allocation: `next = current + 1`; the new manifest is tagged both
  `state-<ws>` (`client.go:307`) and `stver-<ws>-v<N>` (`client.go:315-318`).
- Deletion (`DeleteState`) resolves and deletes only the `state-<ws>` digest
  (`client.go:375-387`); `stver-*` version tags and lock tags are left untouched.

## 6. Versioning & allocation race

The two-layer race (no guard, no test):

1. **Concurrent read**: two writers both read current version N and both
   allocate `v(N+1)` (`client.go:289-295`).
2. **Non-atomic tag**: even with distinct numbers, `state-<ws>` and
   `stver-…-v<N>` tags are plain PUTs (`client.go:307,315-318`) — the second
   writer's tags win silently (last-writer-wins), losing one version pointer
   and one writer's state.

Concurrent writes are only protected by the (itself racy) lock path
(§10). There is no test demonstrating allocation mutual exclusion
because the implementation does not provide it. What *is* tested:
`TestStateStoreConcurrentWritesVersionRetention`
(`oci_concurrency_test.go:946`) runs eight concurrent writers with
`max_versions > 0` and asserts what remains true under the
read→tag race — exactly one readable payload (LWW on `state-<ws>`), at
most `max_versions` surviving `stver-*` tags, and every surviving tag
resolving to a manifest. That demonstrates **LWW/tag consistency, not
allocation mutual exclusion**: writers may still pick the same version
number or skip numbers.

Test-harness caveat (a framework fact, not a provider property):
terraform-plugin-framework v1.19.0's `statestore.generateLockID` races on its
own unsynchronized package-level `math/rand` source when Lock RPCs run
concurrently. **Provider-local mitigation (Phase C):** `OCIStateStore.Lock`
serializes ONLY the `fwss.NewLockInfo` call with a narrow package-level mutex
(`newLockInfoMu`, released before any registry/network operation). This
removes the in-process RNG race for this provider path — concurrent
`fwss.Lock` wrapper calls are covered by
`TestStateStoreConcurrentFwssLock` under `-race` — but it is NOT distributed
locking and does not fix the dependency itself; removal is reconsidered only
after this provider adopts AND verifies an upstream synchronized framework
version. Same-workspace contention in the suite is
still driven at the `oras` layer for determinism.

## 7. Lock acquisition

`lock()` (`client.go:409-542`) performs at most **one stale-clear followed by
one tag+verify attempt** (transport-level retries only):

```
1. read lock tag            (client.go:415-422)
2. held & not stale         → LockError{holder}   (427-429)
   held & stale (TTL>0)     → clearLock           (430, 911-922)
3. generation = prev + 1                          (436-439)
   lease = now + TTL iff TTL > 0                   (441-444)
4. publish lock tag via publishMutableTag          (457-465)
5. post-verify: re-read, check gen + holder       (494-507)
   → mismatch: report contention; cleanupOurTag
     (digest-guarded delete / 405→retagToUnlocked,
     detached 30s context; 471-492)
6. wait fixed 100 ms, stability re-read           (509-539)
   → digest/gen/holder no longer ours: contention,
     do NOT clear a rival's tag
```

The 100 ms `lockStabilityDelay` is a **fixed, unconfigurable constant**
(`client.go:66`) — a mitigation that narrows the late-rival window; it cannot
close it (residual window after the re-read is unbounded).

**Observed (not blind) lock-tag retry.** The lock-tag publication goes
through `publishMutableTag` (`client.go`): the initial Tag PUT proceeds
normally; on a **transient** failure the helper **observes** the tag before
ever re-tagging — own digest ⇒ response-lost publication confirmed (success,
no second Tag); foreign digest ⇒ `ErrMutableTagMoved` mapped to a
`*LockError{holder}` contention diagnostic, no re-tag; absent tag ⇒ fail
closed (a Resolve(404)→Tag reapply is itself a clobber window); ambiguous
(transient resolve error or bound exhausted) ⇒ fail closed. This is NOT CAS —
the W1/W5 windows below are unchanged — it only prevents a blind re-application
after an ambiguous mutable-tag result
(`TestStateStoreLockTagRetryFailClosed`, regression guard since the
retry-safety change). **Scope:** the observed-publication guarantee applies to
FOREGROUND state/version publication only; asynchronous retention retags
(`retagToNewManifest`, §11) remain raw last-writer-wins and are out of scope
for this guarantee.

## 8. Release

`unlock()` (`client.go:588-619`):
- Tag missing or holder-less ⇒ success (idempotent).
- LockID mismatch ⇒ error, nothing deleted (`client.go:604-605`).
- Deletes **own digest** with retry; on HTTP 405 ⇒ `retagToUnlocked`
  (`client.go:618,930-965`): re-resolve the `unlocked-` marker, preflight
  digest-guarded re-Resolve of the lock tag (`client.go:951-957`), tag the
  marker, read-back verify (`client.go:967-982`).
- Empty LockID never reaches here (refused in §2).

A **stale unlock** is rejected on ID mismatch; `forgetLockIf` keeps a newer
registration (`oci.go:84-90`) — a stale unlocker never drops a newer
acquisition.

## 9. Expiration & takeover

Stale rule (`isLockStale`, `client.go:901-909`): stale **iff** configured
`LockTTL > 0` **AND** the stored `lease_expiry > 0` **AND** `now > expiry`.
Checked only on the next `Lock` — **there is no background reaper**.

Consequences:
- TTL = 0 (or unset): a lock held by a crashed process is a **permanent
  orphan** until manually cleared. This is current design, not a
  recommendation (ADR-0001, open decision).
- Enabling TTL later does **not** clear TTL=0-era locks: their manifests have
  `lease_expiry = 0`, so `isLockStale` still returns false.
- Wall-clock based; holder clock skew is unmodeled (ADR-0001).

## 10. Optimistic concurrency semantics

**Best-effort, generation-based; last-writer-wins.** Not atomic CAS — see
ADR-0001 and §14. `VerifyLock` compares **holder ID**, then enforces the
manifest's **STORED** `lease_expiry` (`client.go` verifyLock +
`isStoredLeaseExpired`, Fase D): a positive stored lease already past the
**verifier's local wall clock** is refused ("lock ... expired"); `LeaseExpiry
= 0` remains non-expiring; the verifier's own configured `LockTTL` is
irrelevant to this check (config changes do not alter a stored expiry).
Takeover staleness (`isLockStale`, §9) remains a separate, config-TTL-based
check on the next `Lock`. Two hard limits remain: the expiry comparison is
**local-wall-clock** (holder/rival clock skew is unmodeled) and the check is
**client-side only** — registries do not enforce leases, and a write verified
before expiry whose lease expires during the in-flight Put still lands
(W1 unchanged, `TestStateStoreWriteExpiryDuringW1Limitation`).

Leases are not renewed. A long apply can modify infrastructure and subsequently
fail persistence. [State write recovery](guides/state-recovery.md) documents the
pinned Terraform/Zot experiment, recovery artifacts and operator procedure.
Write diagnostics distinguish rejection before publication, uncertainty after
Put starts, and confirmed state publication with failed version publication.
The partial outcome remains partial even if its version step was cancelled;
no diagnostic recommends blindly repeating an apply.

All verify→write windows are non-atomic (tags are mutable, unordered, plain
PUTs). Additionally, `Write` verifies ownership **once, before the first Put
attempt** (`oci.go:341-353`); the Put path keeps that property: content-addressed
steps are retried with one stable manifest digest, but the mutable state/version
tag publication is **observed, not blind** — a transient tag failure fails
closed on a foreign/ambiguous/absent tag (`ErrMutableTagMoved` → actionable
"State publication conflict" diagnostic in `oci.go`) instead of re-running the
whole `put` with a fresh timestamp/digest. If the state tag published but the
VERSION tag publication fails/conflicts, the write reports a **partial
publication** (`*oras.PartialPublicationError` → "State version publication
failed" diagnostic stating the state is already visible; the failed version
tag is left untouched). The retried publication is still not ownership-checked
(no VerifyLock inside the publication), so the W1 TOCTOU below is unchanged;
what changed is that an ambiguous transient result can no longer be blindly
re-applied over a newer write
(`TestStateStoreStateRetryFailClosed`,
`TestStateStorePartialVersionPublicationFailClosed`). **Scope:** foreground
state/version publication only; async retention retags remain raw LWW.
Enumerated (window IDs
`[W1]`–`[W5]` are referenced by §17's sequence diagram):

| Window | Where | Outcome if a rival moves in between |
|---|---|---|
| [W1] Write `VerifyLock` → `Put` | `oci.go:343` → `oci.go:353` | Silent last-writer-wins (F7) |
| [W2] Lock read → clearLock → tag | `client.go:415` → 430 → 457 | Rival re-locks in between |
| [W3] Retag preflight Resolve → `Tag` | `client.go:951` → 959 | Rival re-locks after the check |
| [W4] Retention tag list → digest grouping → enforce | `client.go:338` → 763 → 744-789 (404 skip: 803-809) | Tags left for a later prune; gone tags skipped |
| [W5] Lock tag publication → post-verify → stability re-read | `client.go` publishMutableTag → 494 → 526 | Rival wins just after the final read (F1); window unclosed |

## 11. Retention / pruning

- Runs only when `max_versions > 0`; **asynchronously** after each write,
  goroutine pool capped at 3 (`client.go:320-360`, `auth.go:143`), on a
  **detached 30-second-bounded context** (`client.go:336`).
- **The current digest is never deleted** (`client.go:817-819`).
- Tags are grouped **by digest** (`groupVersionsByDigest`, `client.go:791-836`)
  so one shared manifest is deleted once, not once per tag. This is a
  *deletion strategy*, **not write dedupe**: every write stamps a fresh
  `updated_at` (`client.go:641`), so identical state bytes produce a new
  digest.
- Digests whose manifest is being deleted get keep-tags retagged onto a fresh
  manifest first (`retagToNewManifest`, `client.go:849-880`).
- GHCR returns 405 for manifest deletion; the fallback uses the GitHub
  Packages API, which requires **`delete:packages`** on the token (`ghcr.go:52-98`,
  `client.go:882-899`). Without it: writes succeed, pruning fails with a warn log
  (`client.go:340,354`). Evidence precision: the 405 fallback branch is
  unit-tested against a **simulated** 405 registry (`deleteUnsupportedRepo`,
  `client_test.go`); live GHCR integration exists but is **conditional**
  (env-gated behind `TF_ORAS_GHCR_TEST`, `ghcr_integration_test.go:12-16`) —
  neither is a portable guarantee, and a live run does not necessarily prove
  the 405 branch executed. Registry-specific, not portable.
- `WaitForRetention()` (`client.go:630-634`) exists **for tests only**; the
  provider has no shutdown hook, so in-flight prunes are undrained on process
  exit (missed prunes self-heal on later writes; a lost prune never un-writes
  state).

## 12. Registry-unavailable behavior

- Every RPC returns an operation diagnostic on failure (e.g. `oci.go:324,354`).
- `Lock` distinguishes "already locked" (mapped to Terraform's
  already-locked diagnostic, `oci.go:393-403`) from transport errors.
- Transient failures retry **3 attempts, 1s then 2s backoff**, with
  context cancellation preserved (`client.go:1196-1277`). Idempotency classes:
  - `PushBytes`: content-addressed ⇒ idempotent.
  - `Tag`: re-tagging the same digest is idempotent, but **mutable-tag
    publications are no longer retried blindly**: `publishMutableTag`
    observes the tag after a transient failure — own digest ⇒ response-lost
    success, foreign digest ⇒ fail closed (`ErrMutableTagMoved`), absent tag
    ⇒ fail closed (no Resolve(404)→Tag reapply: that is itself a clobber
    window), ambiguous ⇒ fail closed (§7, §10, F10,
    `TestStateStoreLockTagRetryFailClosed`,
    `TestStateStoreStateRetryFailClosed`). The same helper covers version-tag
    publication, so a foreign version tag is never retagged; a version-tag
    failure after a successful state publication surfaces as a PARTIAL
    publication error. **Scope:** foreground state/version publication only;
    async retention retags (`retagToNewManifest`) remain raw LWW.
  - `Delete`: 404 treated as success ⇒ idempotent.
- Operations without a caller deadline get a 10-minute default timeout
  (`client.go:180,184-189`).

## 13. Crash / restart

- The lock manifest (generation, lease, holder) **persists in the registry**;
  the process-local lock registry (`oci.go:55-60`) is lost.
- After restart, the provider holds no registered lock ⇒ writes proceed
  **unverified** (`oci.go:341-351` no-op branch).
- New `Lock` either contends (TTL = 0: orphan blocks until manual clear) or
  clears the stale lock (TTL > 0, requires stored `lease_expiry > 0`).
- No background reaper exists.

## 14. OCI limitations

- **No portable CAS.** The OCI Distribution spec defines no `If-Match` for
  manifest PUT. The provider sets no conditional headers (grep for
  `If-Match`/`If-None-Match`/`ETag` in non-test source: zero header uses — the
  single hit is a comment at `client.go:510`), and oras-go v2.6.2's manifest
  `push`/`Tag` likewise issue plain GET/PUT with no conditional headers
  (oras-go `registry/remote/repository.go`, v2.6.2).
- **Tags are mutable and unordered.** The spec defines tags as mutable
  pointers with unspecified overwrite ordering. The practical outcome is
  last-writer-wins; this is observed behavior, not a spec guarantee.
- **Delete is optional.** A registry MAY refuse (MUST answer 400/405); GHCR's
  405 is conformant. All fallbacks (`retagToUnlocked`, GHCR Packages API) are
  registry-specific, not portable.
- **Tested only against ghcr.io and zot 2.1.0.** Other registries are
  untested; registry-specific behavior stays registry-specific.
- The 100 ms stability delay is unconfigurable (§7); replication lag can
  exceed it.
- `retagToUnlocked`'s preflight `Resolve` is a single call without retry
  (`client.go:951`) — a transient network error fails the fallback unlock.

## 15. Failure scenarios

Scenario IDs `F1`–`F13` follow the adversarial analysis's original order of
first definition, grouped here by operation rather than renumbered.

**Lock**

| # | Scenario | Behavior | Evidence |
|---|---|---|---|
| F1 | Two clients acquire concurrently | Both read gen X, both tag X+1; post-verify and the 100 ms stability re-read detect the loser post-hoc; residual window after re-read unclosed | `client.go:415-422,494-539` |
| F3 | Expired takeover | Next Lock sees `now > expiry`, clearLock, tags gen+1; requires stored LeaseExpiry > 0 (TTL = 0 never clearable) | `client.go:409-465,901-909` |
| F4 | Stale owner after takeover / expired lease | VerifyLock checks holder ID, then the STORED lease_expiry (Fase D): after stored expiry the pre-takeover holder is refused at VerifyLock; before expiry it keeps passing VerifyLock until expiry or a rival's tag lands; expiry during an in-flight verified Put still lands (W1) | `client.go` verifyLock/isStoredLeaseExpired; `TestStateStoreTTLExpiryRefusesStaleWrite`, `TestStateStoreWriteExpiryDuringW1Limitation` |
| F9 | Registry unavailable before op | RPC returns an error diagnostic; Lock maps already-locked vs transport errors distinctly | `oci.go:393-405` |

**Write**

| # | Scenario | Behavior | Evidence |
|---|---|---|---|
| F6 | Write while holding locally-known lock | lockFor → VerifyLock → Put; passes only if the registry tag still points at the holder | `oci.go:341-353` |
| F7 | Ownership change between verify and write | VerifyLock → Put is **not atomic**; rival retags in between ⇒ silent last-writer-wins | `oci.go:343,353` |
| F8 | `-lock=false` | No Lock call ⇒ no registration ⇒ VerifyLock skipped ⇒ unverified Put | `oci.go:341-351` |
| F10 | Mutation applied, response lost | PushBytes idempotent (content-addressed); same-digest Tag idempotent; mutable-tag publications go through **observed retry** (`publishMutableTag`): a transient result is followed by observation — own digest ⇒ success without a second Tag, foreign digest ⇒ fail closed (`ErrMutableTagMoved`; lock tag maps to a `*LockError{holder}` contention diagnostic, state tag to an actionable "State publication conflict" diagnostic), absent tag ⇒ fail closed (no Resolve(404)→Tag reapply), ambiguous ⇒ fail closed (conservative: false failure preferred over overwriting a possibly newer state); a version-tag failure after a successful state publication ⇒ `*PartialPublicationError` ("State version publication failed", state stays visible, foreign version tag untouched); Delete 404-as-success; transport retries 3×, 1s/2s for reads/pushes only. NOT CAS: W1/W5 windows unchanged. **Scope:** foreground state/version publication only; async retention retags remain raw LWW | `client.go` publishMutableTag/observeMutableTag, `oci.go` Write; `TestStateStoreLockTagRetryFailClosed`, `TestStateStoreStateRetryFailClosed`, `TestStateStorePartialVersionPublicationFailClosed`, `internal/oras/mutabletag_test.go` |

**Unlock**

| # | Scenario | Behavior | Evidence |
|---|---|---|---|
| F2 | Normal release | Delete own digest; 405 ⇒ retagToUnlocked with preflight digest guard + read-back | `client.go:588-619,930-982` |
| F5 | Stale unlock | Old LockID ⇒ mismatch error, nothing deleted; `forgetLockIf` keeps the newer registration | `client.go:604`, `oci.go:84-90` |

**Crash / restart**

| # | Scenario | Behavior | Evidence |
|---|---|---|---|
| F11 | Crash while holding lock | Registry keeps the lock manifest; process map lost; TTL = 0 ⇒ permanent orphan; TTL > 0 ⇒ clearable on next Lock | `oci.go:55-60`, `client.go:901-909` |
| F12 | Restart after crash | No local lock ⇒ writes proceed unverified; new Lock contends (TTL = 0) or clears stale (TTL > 0, stored LeaseExpiry > 0) | `oci.go:341-351`, `client.go:409-465` |

**Prune**

| # | Scenario | Behavior | Evidence |
|---|---|---|---|
| F13 | Pruning failure after write | Write already tagged before async prune; failure (e.g. GHCR without `delete:packages`) leaves extra versions, never un-writes state; current digest never deleted | `client.go:307-318,320-360,817-819` |

## 16. Guarantee tiers

| Tier | What is guaranteed | Examples |
|---|---|---|
| **In-process (Go-enforced)** | Local registry invariants under the mutex | Shared lock registry across per-RPC instances (`oci.go:55-90`); empty-LockID unlock refused (`oci.go:417-424`); stale unlock never drops newer acquisition (`oci.go:84-90`) |
| **OCI-object-enforced** | Content addressing | Digest comparisons on fetch/retag preflight return the bytes that digest names; deletes target a digest resolved moments earlier (`client.go:480,955`) |
| **Best-effort (detect after the fact)** | Contention *detection*, not prevention | Generation + holder checks, 100 ms stability re-read, VerifyLock, preflight digest guards, read-back verification |
| **Registry-specific** | Valid only for demonstrated registries | GHCR 405 → retagToUnlocked (405 branch simulated in unit tests; live GHCR integration env-gated, see §11); GHCR Packages API delete (`delete:packages`); zot delete path; untested outside ghcr.io + zot 2.1.0 |
| **Not guaranteed (portably or otherwise)** | Explicit non-goals | Mutual exclusion; atomic verify→write; portable CAS/`If-Match`; spec-portable LWW tag ordering; allocation-race-free concurrent writes; TTL-based recovery of TTL = 0 locks |

## 17. Lock lifecycle & write-path sequence

The TOCTOU windows from §10, in sequence. Windows are marked `[Wn]` — a rival
PUT landing inside one is not prevented, only detected post-hoc (or, after the
stability read, not at all).

```
Lock (§7)

  Terraform Core ── Lock ─▶ statestore ── Lock(StateID) ─▶ oras.Client

  1. GET  lock tag          → gen X, holder (or 404)        client.go:415-422
  2. held & not stale       → contention error              client.go:427-429
     held & stale (TTL > 0) → clearLock, continue           client.go:430,911-922
  3. PUT  lock manifest     → observed publication           client.go publishMutableTag
      [W2] rival PUT between the clear and this tag can win
  4. GET  re-read           → gen/holder must be ours       client.go:494-507
  5. wait 100 ms, re-read   → digest/gen/holder must be ours client.go:515-539
      [W5] rival PUT after this read still wins — window unclosed (F1)

  Terraform Core ◀─ LockID ─ statestore

Write (§2, §10)

  Terraform Core ── Write ─▶ statestore

  1. lockFor → VerifyLock: GET lock tag, holder must be ours  oci.go:341-343, client.go:565-586
      [W1] rival retag between VerifyLock and Put → silent last-writer-wins
  2. oras.Client ── PushBytes + PUT state tag ─▶ registry     client.go:297-309

Unlock (§8)

  Terraform Core ── Unlock ─▶ statestore ── Unlock ─▶ oras.Client

  1. GET  lock tag, holder must match LockID                  client.go:589-606
  2. DELETE own digest, with retry                            client.go:608-614
  3. 405 → retagToUnlocked: resolve marker, preflight digest  client.go:618,930-965
     check (951-957), PUT marker onto lock tag, read-back
     verify (967-982)
```
