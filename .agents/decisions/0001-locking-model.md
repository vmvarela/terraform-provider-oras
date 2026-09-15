# ADR-0001: Locking model (generation-based optimistic concurrency)
## Status
Accepted

## Context
No registry in use has proven conditional-write (CAS) support, so state
protection must work over plain OCI tag operations.

## Evidence
- `internal/oras/client.go` (`lockManifestData`): metadata carries `generation`, `lease_expiry`, `holder_id`.
- Audit #17 (5dd96e8): `LockError.Unwrap` so `errors.Is` matches `errStateLocked`; `cleanupOurTag` falls back to `retagToUnlocked` on GHCR HTTP 405 instead of orphaning locks; unparseable `lock_ttl` is now a diagnostic in `validateStoreModel`.
- Audit #24 (7e71465): 100ms `lockStabilityDelay` re-read after first verification (documented in code as a MITIGATION, not mutual exclusion); detached 30s-bounded cleanup context (digest-checked) so cancelled callers do not strand locks; `VerifyLock` best-effort ownership check on the Write path; `retagToUnlocked` with `expectedDigest`, preflight re-Resolve, and `verifyUnlockedMarker` read-back; `stateStoreData` lock registry (`registerLock`/`lockFor`/`forgetLockIf`) so per-RPC instances share ownership — Write refuses on lost lock, Unlock refuses empty LockID, stale unlock never drops a newer acquisition.
- Stale rule: stale iff `LockTTL>0` AND `LeaseExpiry>0` AND `now > LeaseExpiry`. TTL=0 means never stale (no auto-recovery from crashed holders — current design, not a recommendation).
- Write enforcement only when a local lock is registered; `-lock=false` writes proceed unverified (best-effort).
- GHCR 405→retag fallback is unit-tested against a simulated 405 registry (`deleteUnsupportedRepo`); live GHCR integration is env-gated (`TF_ORAS_GHCR_TEST`) and a live run does not necessarily exercise the 405 branch — see Risks/Verification for the full wording. zot delete path tested live. Other registries untested — registry-specific, not portable.

## Decision
Generation-based optimistic concurrency over OCI tags, last-writer-wins.
No CAS / If-Match is assumed or used. Lock checks are best-effort: they
detect contention after the fact (stability re-read, holder-ID checks)
rather than preventing it.

## Alternatives considered
- Pure best-effort without the stability re-read: rejected by Audit #24 — the 100ms re-read demonstrably narrows (not closes) the late-rival window (`LateRivalDetectedByStabilityRead`).
- Server-side CAS / conditional writes: rejected — no registry in use has proven conditional-operation support; per project rules such reliance must be proven first. Revisit when one does.

## Consequences
- Two writers can race and one silently wins (last-writer-wins); the stability re-read and holder-ID checks raise detection odds without guaranteeing it.
- No auto-recovery when TTL=0: locks from crashed holders persist until manually cleared (or TTL is set > 0).
- Registry quirks (e.g. GHCR 405 on delete) handled via the `retagToUnlocked` fallback rather than failing the unlock.

## Risks / limitations
- Residual takeover window after the 100ms stability read is unclosed and unbounded — timing-dependent mitigation, not a guarantee.
- The VerifyLock→Put race is un-closeable: verify-then-write is NOT atomic CAS.
- TTL staleness uses wall-clock time; clock skew between holders is unmodeled.
- Generation is tracked on the lock tag, which `Delete()` does not touch (it removes only the `state-<ws>` digest; `stver-*` version tags and lock tags survive — corrected 2026-09-14 against `client.go:375-387`, previously stated as "state/version tags"); generation resets to 1 only if the lock tag itself is deleted (manual clear or registry GC of unreferenced manifests).
- Whether default TTL should remain 0 (never stale) is an OPEN decision, not settled here.
- TTL expiry makes a lock *clearable by rivals*, not *invalid for the holder*: `VerifyLock` checks holder ID only (`client.go:565-586`), so a holder can keep writing past TTL expiry until a rival acquires the lock.
- `-lock=false` writes bypass all lock checks; concurrent writers silently last-writer-wins.
- The 100ms `lockStabilityDelay` is an untuned constant (unconfigurable); slow or replicating registries may propagate a rival's tag after the re-read.
- In the cleanup path (`cleanupOurTag`), a failed `verifyUnlockedMarker` read-back is debug-logged only — acquisition already failed, so this affects cleanup reliability, not correctness.
- `retagToUnlocked`'s preflight `Resolve` is a single call without retry; transient network errors fail unlock/clearLock on the fallback path.
- **Retries are blind.** The lock-tag PUT retry (`client.go:457-465`) re-runs the plain tag PUT without re-reading the tag, re-checking the generation, or conditioning on the holder: a rival that acquires during the 1s/2s backoff is silently clobbered, and post-verify + stability re-read then pass because the tag points at the retrier's own manifest (`TestStateStoreRetryBlindLockTagLimitation`, green-proves-hole).
- **State Put verifies ownership only before the first attempt** (`oci.go:341-353`); the operation-level retry around Put (`client.go:262-269`) never re-verifies between attempts, so a retried Put can overwrite a newer write (`TestStateStoreStateRetryBlindOverwriteLimitation`, green-proves-hole).
- Test-harness caveat (framework, not provider): terraform-plugin-framework v1.19.0's `statestore.generateLockID` races on its own unsynchronized global `math/rand` source when called concurrently; the concurrency suite avoids concurrent `fwss.NewLockInfo` calls as a workaround (`oci_concurrency_test.go:179-183`). This is not a provider guarantee or fault.
- GHCR 405 fallback evidence is simulated/unit-tested (`deleteUnsupportedRepo`) plus conditional env-gated integration (`TF_ORAS_GHCR_TEST`); not portable, and a live run does not necessarily exercise the 405 branch.
- Behavior on registries other than ghcr.io and zot is untested and registry-specific.

## Verification
Property-proving tests — unit (`internal/oras`): `LockContentionAndUnlockMismatch`, `LockTTL_ClearsStaleLock` (+ `DeleteUnsupportedFallback` variant), `VerifyLock` + `RivalHolder` + `UnlockedMarker`, `CleanupRunsOnCancelledContext`, `RaceConditionDetection`, `LateRivalDetectedByStabilityRead`, 3x `RetagToUnlocked_*`, `SameGenerationRaceDetectedByHolderID`, `ForeignManifestIsVerificationFailure`, `LockWithGenerationDetection`, `StaleLockCleanupRaceCondition`, `UnlockFallbackWhenDeleteUnsupported`.
Unit (`internal/statestore`): `LockUnlockWriteLifecycle`, `WriteRefusedWhenLockLost`, `LockContention`, `UnlockEmptyLockID`, `UnlockWrongLockID`, `ForgetLockIfKeepsNewerAcquisition`, `StateStoreDataConcurrentAccess`.
Concurrency suite (`internal/statestore/oci_concurrency_test.go`, Tests 1–14 in file order):
1. `TestStateStoreConcurrentLockSingleWinner` — one winner among simultaneous contenders.
2. `TestStateStoreConcurrentLockRivalHolder` — all contenders fail against a seeded rival; holder named.
3. `TestStateStoreLockWriteReadRoundTrip` — lock→write→read across per-RPC instances.
4. `TestStateStoreTTLExpiryTakeover` — expired lease → real stale-clear takeover → B writes.
5. `TestStateStoreStaleWriteRefused` — stale holder's write refused after rival takeover.
6. `TestStateStoreStaleUnlockAfterTakeover` — stale unlock rejected; newer acquisition survives.
7. `TestStateStoreConcurrentLockUnlockMix` — mixed lock/unlock race never drops the live lock.
8. `TestStateStoreConcurrentStateWritesLWW` — exactly one LWW survivor among 8 writers.
9. `TestStateStoreWriteTOCTOULimitation` — **GREEN PROVES THE HOLE**: VerifyLock→Put is not atomic; A's bytes land under a rival's lock.
10. `TestStateStoreNoLockWriteBestEffort` — `-lock=false` writes proceed unverified (best-effort).
11. `TestStateStoreRetryBlindLockTagLimitation` — **GREEN PROVES THE HOLE**: retried lock-tag PUT clobbers a rival acquired during the retry backoff; both clients report success.
12. `TestStateStoreTTLExpiryDuringWriteLimitation` — **GREEN PROVES THE HOLE**: expired-lease holder keeps writing until a real takeover.
13. `TestStateStoreConcurrentWritesVersionRetention` — LWW/tag consistency (one survivor, ≤ max_versions resolvable `stver-*` tags) under concurrent writes; NOT allocation mutual exclusion.
14. `TestStateStoreStateRetryBlindOverwriteLimitation` — **GREEN PROVES THE HOLE** (scenario only): a retried state Put overwrites a newer unverified write in the injected-transient scenario; the absence of a retry-time ownership check is established by source inspection (`client.go:262-269` re-runs `wc.put` without re-verifying), not proven by the test passing.

Tests 9, 11, 12 are LIMITATION tests: their passing proves the documented hole still exists (if they fail, the hole closed and the documentation is stale). Test 14 demonstrates the overwrite it enables; a Test 14 failure warrants re-evaluation of the documented behavior (and of `client.go:262-269`), not an automatic "hole closed" conclusion. None of these are guarantees of the provider.
Integration: GHCR `LockUnlock`, zot `LockUnlock` + `LockTTLStaleClearing`.

## References
- `internal/oras/client.go`, `internal/statestore`
- Audits #17 (commit 5dd96e8) and #24 (commit 7e71465)
- `.agents/decisions/README.md` (template)
