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
- GHCR 405→retag fallback observed against live ghcr.io; zot delete path tested live. Other registries untested — registry-specific, not portable.

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
- Behavior on registries other than ghcr.io and zot is untested and registry-specific.

## Verification
Property-proving tests — unit (`internal/oras`): `LockContentionAndUnlockMismatch`, `LockTTL_ClearsStaleLock` (+ `DeleteUnsupportedFallback` variant), `VerifyLock` + `RivalHolder` + `UnlockedMarker`, `CleanupRunsOnCancelledContext`, `RaceConditionDetection`, `LateRivalDetectedByStabilityRead`, 3x `RetagToUnlocked_*`, `SameGenerationRaceDetectedByHolderID`, `ForeignManifestIsVerificationFailure`, `LockWithGenerationDetection`, `StaleLockCleanupRaceCondition`, `UnlockFallbackWhenDeleteUnsupported`.
Unit (`internal/statestore`): `LockUnlockWriteLifecycle`, `WriteRefusedWhenLockLost`, `LockContention`, `UnlockEmptyLockID`, `UnlockWrongLockID`, `ForgetLockIfKeepsNewerAcquisition`, `StateStoreDataConcurrentAccess`.
Integration: GHCR `LockUnlock`, zot `LockUnlock` + `LockTTLStaleClearing`.

## References
- `internal/oras/client.go`, `internal/statestore`
- Audits #17 (commit 5dd96e8) and #24 (commit 7e71465)
- `.agents/decisions/README.md` (template)
