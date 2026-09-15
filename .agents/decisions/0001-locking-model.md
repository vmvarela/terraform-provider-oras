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
- Stale rule (takeover): stale iff `LockTTL>0` AND `LeaseExpiry>0` AND `now > LeaseExpiry`. TTL=0 means never stale (no auto-recovery from crashed holders — current design, not a recommendation).
- VerifyLock stored-lease enforcement (Fase D, 2026-09-14): after the holder ID matches, `verifyLock` rejects a positive STORED `lease_expiry` already past the verifier's local wall clock (`isStoredLeaseExpired`); `LeaseExpiry=0` remains non-expiring; the verifier's configured `LockTTL` is irrelevant to the check and config changes do not alter a stored expiry. Client-side only — registries do not enforce leases, the comparison uses the local clock (skew unmodeled), and a write verified before expiry whose lease expires during the in-flight Put still lands (W1 unchanged).
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
- TTL expiry makes a lock *clearable by rivals*; after the stored lease expires, the holder is ALSO refused at VerifyLock (Fase D, `isStoredLeaseExpired`): the expired holder can no longer start NEW writes, but expiry during an in-flight verified Put does not stop the bytes landing (`TestStateStoreWriteExpiryDuringW1Limitation`, green-proves-hole). The stored lease is enforced against the verifier's local clock — skew between holders is unmodeled.
- `-lock=false` writes bypass all lock checks; concurrent writers silently last-writer-wins.
- The 100ms `lockStabilityDelay` is an untuned constant (unconfigurable); slow or replicating registries may propagate a rival's tag after the re-read.
- In the cleanup path (`cleanupOurTag`), a failed `verifyUnlockedMarker` read-back is debug-logged only — acquisition already failed, so this affects cleanup reliability, not correctness.
- `retagToUnlocked`'s preflight `Resolve` is a single call without retry; transient network errors fail unlock/clearLock on the fallback path.
- **Mutable-tag retry is observed, not blind (2026-09-14).** `publishMutableTag`/`observeMutableTag` (`client.go`): the initial Tag PUT proceeds normally; on a transient failure the tag is OBSERVED — own digest ⇒ response-lost publication confirmed (success, no second Tag), foreign digest ⇒ `ErrMutableTagMoved` fail closed (lock tag maps to a `*LockError{holder}` contention diagnostic; state tag to an actionable "State publication conflict" diagnostic in `oci.go`), absent tag ⇒ fail closed (no Resolve(404)→Tag reapply: that is itself a clobber window — a rival can publish between the 404 and a re-Tag), ambiguous ⇒ fail closed (false failure preferred over overwriting a possibly newer state). Cancellation identity (`context.Canceled`/`context.DeadlineExceeded`) is preserved on fail-closed errors and takes priority in the statestore Write diagnostic mapping. If the state tag published but the version tag publication fails/conflicts, the write reports a PARTIAL publication (`*PartialPublicationError` → "State version publication failed" diagnostic: state is visible and readable, failed version tag untouched). **Scope:** foreground state/version publication only; asynchronous retention retags (`retagToNewManifest`) remain raw last-writer-wins — no claim that ALL version-tag writes are protected. This is NOT CAS: it only prevents blind reapplication after an ambiguous mutable-tag result; the W1/W5 windows are unchanged.
- Test-harness caveat + provider mitigation (Phase C): terraform-plugin-framework v1.19.0's `statestore.generateLockID` races on its own unsynchronized global `math/rand` source when called concurrently. The provider serializes ONLY the `fwss.NewLockInfo` call in `OCIStateStore.Lock` with a narrow package-level mutex (`newLockInfoMu`, released before any registry/network operation): this removes the in-process RNG race for this provider path (covered by `TestStateStoreConcurrentFwssLock` under `-race`, actors split across two independently configured stores), provides NO distributed locking, and does not fix the dependency itself — removal is reconsidered only after this provider adopts AND verifies an upstream synchronized framework version.
- GHCR 405 fallback evidence is simulated/unit-tested (`deleteUnsupportedRepo`) plus conditional env-gated integration (`TF_ORAS_GHCR_TEST`); not portable, and a live run does not necessarily exercise the 405 branch.
- Behavior on registries other than ghcr.io and zot is untested and registry-specific.

## Verification
Property-proving tests — unit (`internal/oras`): `LockContentionAndUnlockMismatch`, `LockTTL_ClearsStaleLock` (+ `DeleteUnsupportedFallback` variant), `VerifyLock` + `RivalHolder` + `UnlockedMarker`, `CleanupRunsOnCancelledContext`, `RaceConditionDetection`, `LateRivalDetectedByStabilityRead`, 3x `RetagToUnlocked_*`, `SameGenerationRaceDetectedByHolderID`, `ForeignManifestIsVerificationFailure`, `LockWithGenerationDetection`, `StaleLockCleanupRaceCondition`, `UnlockFallbackWhenDeleteUnsupported`.
Unit (`internal/statestore`): `LockUnlockWriteLifecycle`, `WriteRefusedWhenLockLost`, `LockContention`, `UnlockEmptyLockID`, `UnlockWrongLockID`, `ForgetLockIfKeepsNewerAcquisition`, `StateStoreDataConcurrentAccess`.
Concurrency suite (`internal/statestore/oci_concurrency_test.go`, Tests 1–14 in file order):
1. `TestStateStoreConcurrentLockSingleWinner` — one winner among simultaneous contenders.
2. `TestStateStoreConcurrentLockRivalHolder` — all contenders fail against a seeded rival; holder named.
3. `TestStateStoreLockWriteReadRoundTrip` — lock→write→read across per-RPC instances.
4. `TestStateStoreTTLExpiryTakeover` — expired lease → real stale-clear takeover → B writes within its OWN lease (1s TTL; VerifyLock now refuses expired stored leases, Fase D).
5. `TestStateStoreStaleWriteRefused` — stale holder's write refused after rival takeover.
6. `TestStateStoreStaleUnlockAfterTakeover` — stale unlock rejected; newer acquisition survives.
7. `TestStateStoreConcurrentLockUnlockMix` — mixed lock/unlock race never drops the live lock.
8. `TestStateStoreConcurrentStateWritesLWW` — exactly one LWW survivor among 8 writers.
9. `TestStateStoreWriteTOCTOULimitation` — **GREEN PROVES THE HOLE**: VerifyLock→Put is not atomic; A's bytes land under a rival's lock.
10. `TestStateStoreNoLockWriteBestEffort` — `-lock=false` writes proceed unverified (best-effort).
11. `TestStateStoreLockTagRetryFailClosed` — regression guard (was the retry-blind green-hole test, converted after the mutable-tag retry-safety change): injected transient first lock-tag publication + rival acquisition ⇒ A fails with a LockError-class diagnostic naming the rival; the rival stays holder; no blind re-tag (single lock-tag PUT).
12. `TestStateStoreTTLExpiryRefusesStaleWrite` — regression guard (Fase D; was the TTL-expiry-during-write green-hole test): after the stored lease expires, A's Write is refused at VerifyLock ("lock ... expired"), no state bytes land, registration preserved; a real stale-clear takeover still succeeds.
13. `TestStateStoreConcurrentWritesVersionRetention` — LWW/tag consistency (one survivor, ≤ max_versions resolvable `stver-*` tags) under concurrent writes; NOT allocation mutual exclusion.
14. `TestStateStoreStateRetryFailClosed` — regression guard (was the state-retry-blind green-hole test): injected transient first state-tag PUT + B's newer write in the observation gap ⇒ A fails closed with the "State publication conflict" diagnostic; final state remains B; no blind re-apply.
15. `TestStateStorePartialVersionPublicationFailClosed` — partial publication: max_versions>0, state tag publishes, version-tag publication hits transient-then-foreign ⇒ "State version publication failed" diagnostic (state visible/readable, version tag belongs to the rival, no re-apply).
16. `TestStateStoreWriteVerifyLockInterrupted` — cancelled VerifyLock ⇒ interruption diagnostic, not a false lost-lock claim.
17. `TestStateStoreConcurrentFwssLock` — concurrent real fwss Lock wrapper across two independent store configurations; provider `newLockInfoMu` workaround keeps the framework RNG race out of this path.
18. `TestStateStoreWriteExpiryDuringW1Limitation` — **GREEN PROVES THE HOLE** (Fase D residual): VerifyLock passes BEFORE stored-lease expiry, expiry occurs during the in-flight Put ⇒ bytes still land; nothing re-verifies between VerifyLock and publication (W1 unchanged).

Tests 9 and 18 are LIMITATION tests: their passing proves the documented hole still exists (if they fail, the hole closed and the documentation is stale). Tests 11, 12, 14 and 15 are positive regression guards — their passing asserts the safety property, and a failure means the corresponding behavior has regressed. They do NOT close W1/W5: Test 9 still proves the verify→Put TOCTOU, Test 18 its expiry variant, and the stability-re-read window remains; the guards cover FOREGROUND state/version publication only, not async retention retags. None of these are guarantees of the provider.
Lower-level helper evidence (`internal/oras/mutabletag_test.go`): `TestPublishMutableTagResponseLostOwnDigest` (own digest landed + transient response ⇒ success, exactly one Tag), `TestPublishMutableTagForeignDigestFailClosed` + `TestPublishMutableTagForeignVersionTag` (foreign digest ⇒ `ErrMutableTagMoved`, tag preserved, no re-tag), `TestPublishMutableTagAmbiguousResolveFailClosed` + `TestPublishMutableTagContextCancelledFailClosed` + `TestPublishMutableTagDeadlineExceededFailClosed` (ambiguous/cancelled/deadline ⇒ fail closed with cancellation identity preserved, bounded, context-cancellable), `TestPublishMutableTagAbsentTagFailClosedNoReapply` + `TestPublishMutableTagRivalDuringObservationNoReapply` (absent ⇒ fail closed with exactly one Tag — no Resolve(404)→Tag reapply; a rival landing mid-observation is detected, never overwritten), `TestPublishMutableTagNonTransientSurfaced` (non-transient ⇒ surfaced, no observation), `TestLockTagMovedMapsToContentionBothSentinels` (lock path maps a moved tag to `*LockError` carrying BOTH `errStateLocked` and `ErrMutableTagMoved` identity).
Stored-lease evidence (`internal/oras/client_test.go`): `TestClient_VerifyLock_StoredLeaseExpiry` — positive past stored lease refused, future lease ok, zero lease non-expiring, with the verifier's own LockTTL = 0 (enforcement independent of local config).
Integration: GHCR `LockUnlock`, zot `LockUnlock` + `LockTTLStaleClearing`.
GHCR caveat: the 405 fallback branch has NO deterministic new evidence beyond the simulated 405 unit tests and the env-gated live integration — a live run does not prove the branch executed; no live-registry assumption is added by Fase D.

## References
- `internal/oras/client.go`, `internal/statestore`
- Audits #17 (commit 5dd96e8) and #24 (commit 7e71465)
- `.agents/decisions/README.md` (template)
