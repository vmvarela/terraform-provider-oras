// Deterministic adversarial concurrency tests for the OCI state store,
// covering issue #28. Every test documents the consistency property it
// verifies and the layer it exercises:
//
//   - Go-race:      the value is the race detector staying clean under
//     concurrent RPCs (memory-safety of shared state).
//   - logical:      in-process invariants of the local lock registry
//     (stateStoreData.registerLock/lockFor/forgetLockIf).
//   - distributed:  multi-holder contention semantics through the registry
//     (generation checks, stale clears, takeover).
//   - fake-only:    relies on fakeOCIRegistry hooks or direct tag installs;
//     documents behavior of THIS implementation against THIS
//     fake, not a portable registry guarantee.
//
// Synchronization is deterministic only: start barriers (channel close),
// hook channels, and the fake registry's serialized request handling. No
// time.Sleep is used to *create* races (the exceptions are the TTL-expiry
// tests — tests 4, 6 and 12 — where expiry itself is wall-clock and cannot be
// channel-driven). The implementation's own
// 100ms lockStabilityDelay is tolerated with generous 30s test timeouts.
//
// Limitation tests are green-proves-hole only where the hole is real and
// open: the W1 verify→Put window (Test 9) and the expiry-during-W1 window
// (Test 18: verification passes before stored-lease expiry, expiry occurs
// during the in-flight Put, bytes still land) — both documented in ADR-0001
// (.agents/decisions/0001-locking-model.md) and both must fail if the window
// is ever closed. The former retry-blind tests (Tests 11/14) and the former
// TTL-expiry-during-write test (Test 12) were converted into positive
// regression guards: Tests 11/14 assert fail-closed retry behavior
// (internal/oras publishMutableTag) and Test 12 asserts stored-lease refusal
// at VerifyLock (Fase D). Scope of the observed publication guarantees:
// FOREGROUND state/version publication only (including the partial "state
// lands, version tag fails closed" case of Test 15); asynchronous retention
// retags (retagToNewManifest) remain raw last-writer-wins — no claim is made
// that ALL version-tag writes are protected.
package statestore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
)

// testWaitTimeout is the generous bound for joining concurrent actors; it
// must comfortably exceed the implementation's own 100ms stability delay
// and (for the retry test) its 1s retry backoff.
const testWaitTimeout = 30 * time.Second

// hookedHandler wraps a fakeOCIRegistry with a test-controlled gate without
// touching the fake in oci_test.go. The gate runs BEFORE the request reaches
// the fake (and before the fake's mutex is taken), so direct fake calls such
// as TagManifest proceed while a gated request is blocked.
type hookedHandler struct {
	reg  *fakeOCIRegistry
	mu   sync.Mutex
	gate func(r *http.Request) (block <-chan struct{}, status int)
}

func (h *hookedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.gate != nil {
		h.mu.Lock()
		block, status := h.gate(r)
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if block != nil {
			select {
			case <-block:
			case <-time.After(testWaitTimeout):
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
		}
	}
	h.reg.ServeHTTP(w, r)
}

// newConcurrencyRegistry builds a fake registry (optionally gated) behind an
// httptest server and returns it with its base URL, so a test can configure
// MULTIPLE independent store configurations (separate lock registries) against
// the same registry.
func newConcurrencyRegistry(t *testing.T, gate func(*http.Request) (<-chan struct{}, int)) (*fakeOCIRegistry, string) {
	t.Helper()
	reg := newFakeOCIRegistry()
	var handler http.Handler = reg
	if gate != nil {
		handler = &hookedHandler{reg: reg, gate: gate}
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return reg, srv.Listener.Addr().String()
}

// newConfiguredStore runs the real Initialize→Configure flow against an
// existing registry endpoint, with lock_ttl and max_versions overrides
// (maxVersions <= 0 keeps the default, versioning disabled).
func newConfiguredStore(t *testing.T, lockTTL, baseURL string, maxVersions int64) (*OCIStateStore, *stateStoreData) {
	t.Helper()
	ctx := context.Background()

	overrides := map[string]tftypes.Value{
		"url": strVal("oci://" + baseURL + "/test/repo"),
	}
	if lockTTL != "" {
		overrides["lock_ttl"] = strVal(lockTTL)
	}
	if maxVersions > 0 {
		overrides["max_versions"] = int64Val(maxVersions)
	}

	initResp := &fwss.InitializeResponse{}
	(&OCIStateStore{}).Initialize(ctx, fwss.InitializeRequest{
		Config:       storeConfig(overrides),
		ProviderData: &ProviderData{Insecure: true},
	}, initResp)
	if initResp.Diagnostics.HasError() {
		t.Fatalf("initialize: %v", initResp.Diagnostics)
	}
	ssd, ok := initResp.StateStoreData.(*stateStoreData)
	if !ok {
		t.Fatalf("StateStoreData is %T, want *stateStoreData", initResp.StateStoreData)
	}

	s := &OCIStateStore{}
	cfgResp := &fwss.ConfigureResponse{}
	s.Configure(ctx, fwss.ConfigureRequest{StateStoreData: ssd}, cfgResp)
	if cfgResp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", cfgResp.Diagnostics)
	}
	return s, ssd
}

// newConcurrencyStore mirrors newTestStore but supports a request gate and a
// lock_ttl override, so adversarial tests can inject failure/blocking hooks
// and short leases. The gate, when non-nil, runs for every HTTP request.
func newConcurrencyStore(t *testing.T, lockTTL string, gate func(*http.Request) (<-chan struct{}, int)) (*OCIStateStore, *fakeOCIRegistry, *stateStoreData) {
	t.Helper()
	reg, baseURL := newConcurrencyRegistry(t, gate)
	s, ssd := newConfiguredStore(t, lockTTL, baseURL, 0)
	return s, reg, ssd
}

// waitGroup joins the actors with a generous timeout so a stuck
// implementation fails the test instead of hanging the run.
func waitGroup(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(testWaitTimeout):
		t.Fatalf("concurrent actors did not finish within %v", testWaitTimeout)
	}
}

// waitSignal waits for a one-shot hook signal.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWaitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ─── 1. Simultaneous acquisition on an empty lock tag ─────────────────────────

// TestStateStoreConcurrentLockSingleWinner — Property (issue #28 I1): when N
// clients attempt Lock simultaneously on an empty lock tag, exactly one may
// acquire it; every loser receives *oras.LockError, and the winner ends up in
// the shared local lock registry (registration is mirrored here exactly as
// OCIStateStore.Lock does after a successful acquire).
//
// The concurrent contenders are driven at the oras layer (ssd.client.Lock):
// same-workspace fwss contention is deterministic only that way. Concurrent
// fwss.Lock calls are separately covered by
// TestStateStoreConcurrentFwssLock (test 17), which relies on the
// provider-side serialization of fwss.NewLockInfo (newLockInfoMu in oci.go) —
// a compatibility workaround for terraform-plugin-framework v1.19.0's
// unsynchronized package-level *rand.Rand in statestore.generateLockID
// (surfaced by this suite under -race). The upstream defect remains
// dependency-specific; the provider mutex removes the in-process RNG race
// for this provider path only.
//
// Qualified I1: fake-serialized, no induced transients — the fake registry's
// single mutex serializes tag operations, so the winner is decided by real
// registry tag ordering, never by injected failures or retries.
// Layer: distributed (fake-serialized) + logical.
func TestStateStoreConcurrentLockSingleWinner(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	const n = 3 // flake seed: 3 contenders still prove simultaneous contention
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: all contenders released at once
			info := oras.LockInfo{
				ID:        fmt.Sprintf("contender-%d", i),
				Operation: "apply",
				Who:       "contender",
			}
			ids[i], errs[i] = ssd.client.Lock(ctx, "default", info)
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	winners := 0
	var winnerID string
	for i, err := range errs {
		if err == nil {
			winners++
			winnerID = ids[i]
			continue
		}
		var lockErr *oras.LockError
		if !errors.As(err, &lockErr) {
			t.Errorf("actor %d failed with %T, want *oras.LockError", i, err)
		}
		if ids[i] != "" {
			t.Errorf("actor %d failed but returned LockID %q", i, ids[i])
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (out of %d contenders)", winners, n)
	}

	// Winner registration: mirror of OCIStateStore.Lock's registerLock step
	// (sequential logic), so the shared-registry property is asserted on the
	// real winner of the concurrent race.
	ssd.registerLock("default", winnerID)
	if got := ssd.lockIDs["default"]; got != winnerID {
		t.Errorf("winner not registered locally: registry = %q, want %q", got, winnerID)
	}
	if !reg.HasTag(testLockTag) {
		t.Error("winner's lock tag missing from registry")
	}
	if err := ssd.client.VerifyLock(ctx, "default", winnerID); err != nil {
		t.Errorf("winner does not hold the distributed lock: %v", err)
	}
}

// ─── 2. Contention against a pre-seeded rival holder ──────────────────────────

// TestStateStoreConcurrentLockRivalHolder — Property (issue #28 I2): when a
// rival already holds the lock, ALL contenders fail; every loser's
// *oras.LockError names the holder; no contender registers a local lock; the
// rival's lock tag survives. The fwss diagnostic mapping (holder name in the
// detail) is asserted sequentially at the end. Qualified I1: fake-serialized,
// no induced transients — contention is deterministic (the rival manifest is
// installed before the start barrier).
// Layer: distributed + fake-only (rival seeded via direct TagManifest).
func TestStateStoreConcurrentLockRivalHolder(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	// A rival already holds the lock before any contender starts.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			info := oras.LockInfo{
				ID:        fmt.Sprintf("contender-%d", i),
				Operation: "apply",
				Who:       "contender",
			}
			ids[i], errs[i] = ssd.client.Lock(ctx, "default", info)
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	for i, err := range errs {
		if err == nil {
			t.Fatalf("actor %d acquired a rival-held lock", i)
		}
		var lockErr *oras.LockError
		if !errors.As(err, &lockErr) {
			t.Errorf("actor %d failed with %T, want *oras.LockError", i, err)
		} else if lockErr.Info == nil || lockErr.Info.ID != "rival" || lockErr.Info.Who != "rival" {
			t.Errorf("actor %d diagnostic does not name the holder: %+v", i, lockErr.Info)
		}
		if ids[i] != "" {
			t.Errorf("actor %d failed but returned LockID %q", i, ids[i])
		}
	}
	if len(ssd.lockIDs) != 0 {
		t.Errorf("failed locks registered locally: %v", ssd.lockIDs)
	}
	if !reg.HasTag(testLockTag) {
		t.Error("rival's lock tag was removed")
	}

	// fwss-level mapping (sequential): the framework diagnostic must surface
	// the rival holder, mirroring TestStateStoreLockContention.
	fwssLocker := testNewInstance(t, ssd)
	fwssResp := &fwss.LockResponse{}
	fwssLocker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, fwssResp)
	if !fwssResp.Diagnostics.HasError() {
		t.Fatal("fwss-level lock against rival holder was accepted")
	}
	if !strings.Contains(fwssResp.Diagnostics[0].Detail(), "rival") {
		t.Errorf("fwss detail = %q, want it to mention the holder", fwssResp.Diagnostics[0].Detail())
	}
	if len(ssd.lockIDs) != 0 {
		t.Errorf("failed fwss lock registered locally: %v", ssd.lockIDs)
	}
}

// ─── 3. Lock → Write → Read across per-RPC instances ──────────────────────────

// TestStateStoreLockWriteReadRoundTrip — Property: a lock acquired by one
// per-RPC instance authorizes a Write (verified path) and a Read on other
// instances sharing the same stateStoreData; Terraform's fresh-instance-per-
// RPC model must not lose ownership. Layer: logical + Go-race.
func TestStateStoreLockWriteReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	reader, _, ssd := newTestStore(t)

	locker := testNewInstance(t, ssd)
	writer := testNewInstance(t, ssd)

	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	writeResp := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("roundtrip-payload")}, writeResp)
	if writeResp.Diagnostics.HasError() {
		t.Fatalf("write: %v", writeResp.Diagnostics)
	}

	readResp := &fwss.ReadResponse{}
	reader.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "roundtrip-payload" {
		t.Errorf("read = %q, want %q", readResp.StateBytes, "roundtrip-payload")
	}
	if got := ssd.lockIDs["default"]; got != lockResp.LockID {
		t.Errorf("registration = %q, want %q", got, lockResp.LockID)
	}
}

// ─── 4. TTL expiry → takeover → B writes ──────────────────────────────────────

// TestStateStoreTTLExpiryTakeover — Property (issue #28 I4): with a short
// lock_ttl, a lock whose lease expired is stale; the next contender must win
// via the real stale-clear path (not a direct tag install) and must then be
// able to write while its OWN lease is still valid. The TTL must exceed the
// ~100ms lockStabilityDelay inside acquisition: post-Fase-D, VerifyLock
// refuses a holder whose STORED lease has expired, so a too-short TTL (e.g.
// 1ms) would make every post-acquisition write fail.
// Layer: distributed (TTL staleness + clearLock + stored-lease VerifyLock).
func TestStateStoreTTLExpiryTakeover(t *testing.T) {
	ctx := context.Background()
	_, _, ssd := newConcurrencyStore(t, "1s", nil)

	a := testNewInstance(t, ssd)
	aResp := &fwss.LockResponse{}
	a.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, aResp)
	if aResp.Diagnostics.HasError() {
		t.Fatalf("A lock: %v", aResp.Diagnostics)
	}

	// Expiry is wall-clock based: wait out A's 1s lease (1.1× TTL margin).
	// (Expiry cannot be channel-driven; the sleep is the tolerated
	// wall-clock wait for actual expiry.)
	time.Sleep(1100 * time.Millisecond)

	b := testNewInstance(t, ssd)
	bResp := &fwss.LockResponse{}
	b.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, bResp)
	if bResp.Diagnostics.HasError() {
		t.Fatalf("B takeover: %v", bResp.Diagnostics)
	}
	if bResp.LockID == aResp.LockID {
		t.Fatal("B acquired A's lock ID; takeover did not mint a new holder")
	}

	// B writes immediately, within its own (just-acquired) lease.
	writeResp := &fwss.WriteResponse{}
	b.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("after-takeover")}, writeResp)
	if writeResp.Diagnostics.HasError() {
		t.Fatalf("B write: %v", writeResp.Diagnostics)
	}

	readResp := &fwss.ReadResponse{}
	b.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "after-takeover" {
		t.Errorf("read = %q, want %q", readResp.StateBytes, "after-takeover")
	}
}

// ─── 5. Stale owner cannot write after takeover (issue #28 I5) ────────────────

// TestStateStoreStaleWriteRefused — Property (issue #28 I5): once the lock
// tag points at a rival, the stale holder's Write must be refused with a
// "no longer held" diagnostic, and no state bytes may land. Qualified I5:
// deterministic — the rival takeover is a direct tag install before the
// write; no transients, no timing dependence. Layer: distributed + fake-only.
func TestStateStoreStaleWriteRefused(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	locker := testNewInstance(t, ssd)
	writer := testNewInstance(t, ssd)

	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	// Rival takes over the lock tag after our acquisition.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	writeResp := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("hijacked")}, writeResp)
	if !writeResp.Diagnostics.HasError() {
		t.Fatal("stale holder's write was accepted")
	}
	if !strings.Contains(writeResp.Diagnostics[0].Summary(), "no longer held") {
		t.Errorf("summary = %q, want it to mention the lost lock", writeResp.Diagnostics[0].Summary())
	}
	if reg.HasTag(testStateTag) {
		t.Error("state bytes landed despite lost lock")
	}
	if !reg.HasTag(testLockTag) {
		t.Error("lock tag removed by the refused write")
	}
}

// ─── 6. Stale owner cannot unlock a newer lock ────────────────────────────────

// TestStateStoreStaleUnlockAfterTakeover — Property (issue #28 I3/I6): a
// stale owner's Unlock after a rival acquired via the real stale-clear path
// must be rejected remotely: the oras unlock refuses to release a lock tag
// whose holder ID differs from the caller's (stale-ID rejection at the
// registry layer), so B's remote lock tag survives AND B keeps holding it
// (confirmed via B's own client). This is NOT exercise of forgetLockIf after
// a successful unlock — forgetLockIf only runs on success (the
// keep-newer-acquisition behavior is covered by
// TestStateStoreForgetLockIfKeepsNewerAcquisition). A and B are independent
// client configurations (separate stateStoreData/lock registries) against the
// same endpoint, so a failed unlock must also leave A's local registration
// pointing at A's own lock. Layer: distributed + logical.
func TestStateStoreStaleUnlockAfterTakeover(t *testing.T) {
	ctx := context.Background()
	reg, baseURL := newConcurrencyRegistry(t, nil)
	// TTL must exceed the ~100ms acquisition delay so B's OWN stored lease is
	// still valid when we later VerifyLock B's holder (Fase D enforces stored
	// expiry at VerifyLock); 1s gives a comfortable margin.
	_, ssdA := newConfiguredStore(t, "1s", baseURL, 0)
	_, ssdB := newConfiguredStore(t, "1s", baseURL, 0)

	a := testNewInstance(t, ssdA)
	aResp := &fwss.LockResponse{}
	a.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, aResp)
	if aResp.Diagnostics.HasError() {
		t.Fatalf("A lock: %v", aResp.Diagnostics)
	}

	// Let A's lease expire (1.1× TTL margin), then B takes over through the
	// stale-clear path.
	time.Sleep(1100 * time.Millisecond)
	b := testNewInstance(t, ssdB)
	bResp := &fwss.LockResponse{}
	b.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, bResp)
	if bResp.Diagnostics.HasError() {
		t.Fatalf("B takeover: %v", bResp.Diagnostics)
	}

	// A's stale unlock must be rejected: the remote holder ID differs.
	unlockResp := &fwss.UnlockResponse{}
	a.Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: aResp.LockID}, unlockResp)
	if !unlockResp.Diagnostics.HasError() {
		t.Fatal("stale owner released a newer holder's lock")
	}

	// The remote tag survives AND still names B as holder.
	if !reg.HasTag(testLockTag) {
		t.Error("B's remote lock tag was removed by A's stale unlock")
	}
	if err := ssdB.client.VerifyLock(ctx, "default", bResp.LockID); err != nil {
		t.Errorf("B no longer holds the remote lock after A's stale unlock: %v", err)
	}

	// Local registrations: B keeps its acquisition; A's failed unlock must
	// leave A registered for A's own lock.
	if id, ok := ssdB.lockFor("default"); !ok || id != bResp.LockID {
		t.Errorf("B's registration = (%q, %v), want (%q, true): stale unlock must not drop it", id, ok, bResp.LockID)
	}
	if id, ok := ssdA.lockFor("default"); !ok || id != aResp.LockID {
		t.Errorf("A's registration = (%q, %v), want (%q, true) preserved after the failed unlock", id, ok, aResp.LockID)
	}
}

// ─── 7. Concurrent lock/unlock mix ────────────────────────────────────────────

// TestStateStoreConcurrentLockUnlockMix — Property (issue #28 I7): a mix of
// concurrent Lock attempts and Unlock attempts carrying mismatched LockIDs
// must never panic, must never let a mismatched unlock remove the remote
// lock tag, and must leave the local registry holding exactly the live
// acquisition (only a matching forgetLockIf may drop it). The holder is
// pre-acquired sequentially; lockers contend against it concurrently at the
// oras layer, while fwss-level wrong-ID Unlock actors run concurrently
// alongside them (Unlock does not touch the framework's rand source).
// Layer: Go-race + logical + distributed.
func TestStateStoreConcurrentLockUnlockMix(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	// Pre-acquired winner: its registration must survive every mismatched
	// concurrent unlock.
	winner := testNewInstance(t, ssd)
	winnerResp := &fwss.LockResponse{}
	winner.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, winnerResp)
	if winnerResp.Diagnostics.HasError() {
		t.Fatalf("winner lock: %v", winnerResp.Diagnostics)
	}

	const nLock, nUnlock = 8, 8
	instances := make([]*OCIStateStore, nLock+nUnlock)
	for i := range instances {
		instances[i] = testNewInstance(t, ssd)
	}

	lockErrs := make([]error, nLock)
	start := make(chan struct{})
	var wg sync.WaitGroup

	// Lockers contend against the live holder.
	for i := 0; i < nLock; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			info := oras.LockInfo{
				ID:        fmt.Sprintf("contender-%d", i),
				Operation: "apply",
				Who:       "contender",
			}
			_, lockErrs[i] = ssd.client.Lock(ctx, "default", info)
		}(i)
	}
	// Unlockers concurrently try mismatched IDs: never a matching one, never
	// empty (empty is refused before reaching the registry). The holder's tag
	// is always present, so every mismatched unlock must be refused. The fwss
	// Unlock path performs no lock-ID generation, so it is race-detector clean.
	for i := 0; i < nUnlock; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp := &fwss.UnlockResponse{}
			instances[nLock+i].Unlock(ctx, fwss.UnlockRequest{
				StateID: "default",
				LockID:  "stale-wrong-" + string(rune('a'+i)),
			}, resp)
			if !resp.Diagnostics.HasError() {
				t.Errorf("unlock actor %d with mismatched ID was not refused: %v", i, resp.Diagnostics)
			}
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	for i, err := range lockErrs {
		if err == nil {
			t.Errorf("contender %d acquired a held lock", i)
		}
	}

	// The remote lock tag still exists and still names the winner as holder.
	if err := ssd.client.VerifyLock(ctx, "default", winnerResp.LockID); err != nil {
		t.Errorf("remote holder is no longer the winner after concurrent unlocks: %v", err)
	}

	if !reg.HasTag(testLockTag) {
		t.Fatal("remote lock tag removed despite no matching unlock")
	}
	if got := ssd.lockIDs["default"]; got != winnerResp.LockID {
		t.Fatalf("registration = %q, want winner %q; only matching forgetLockIf may drop it", got, winnerResp.LockID)
	}

	// Deterministic epilogue: the one matching unlock drops the registration
	// and removes the remote tag.
	final := &fwss.UnlockResponse{}
	instances[0].Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: winnerResp.LockID}, final)
	if final.Diagnostics.HasError() {
		t.Fatalf("matching unlock: %v", final.Diagnostics)
	}
	if len(ssd.lockIDs) != 0 {
		t.Errorf("registration survived matching unlock: %v", ssd.lockIDs)
	}
	if reg.HasTag(testLockTag) {
		t.Error("remote lock tag survived matching unlock")
	}
}

// ─── 8. Concurrent state writes (LWW) ─────────────────────────────────────────

// TestStateStoreConcurrentStateWritesLWW — Property (issue #28 I8): eight
// concurrent writers pushing distinct payloads to the same workspace end with
// EXACTLY ONE payload readable — OCI tags are last-writer-wins, so the
// survivor is whichever write tagged last (no ordering guarantee, documented
// LWW). One writer holds the registered lock (verified path); the other seven
// exercise the -lock=false path. Layer: Go-race + distributed (LWW documented).
func TestStateStoreConcurrentStateWritesLWW(t *testing.T) {
	ctx := context.Background()
	_, _, ssd := newTestStore(t)

	// Writer 0 holds a registered local lock; writers 1..n-1 are the
	// -lock=false path (no local registration, write proceeds unverified).
	holder := testNewInstance(t, ssd)
	lockResp := &fwss.LockResponse{}
	holder.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	const n = 8
	payloads := make([]string, n)
	instances := make([]*OCIStateStore, n)
	for i := range instances {
		payloads[i] = "lww-payload-" + string(rune('a'+i))
		instances[i] = testNewInstance(t, ssd)
	}

	writeOK := make([]bool, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp := &fwss.WriteResponse{}
			instances[i].Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte(payloads[i])}, resp)
			writeOK[i] = !resp.Diagnostics.HasError()
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	for i, ok := range writeOK {
		if !ok {
			t.Fatalf("writer %d failed", i)
		}
	}

	readResp := &fwss.ReadResponse{}
	instances[0].Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	got := string(readResp.StateBytes)
	matches := 0
	for _, p := range payloads {
		if got == p {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("read back %q: matches %d of %d known payloads, want exactly one LWW survivor", got, matches, n)
	}
}

// ─── 9. TOCTOU LIMITATION: VerifyLock→Put is not atomic ───────────────────────

// TestStateStoreWriteTOCTOULimitation — LIMITATION (issue #28 I9, Gate1):
// documents the residual VerifyLock→Put window. A's VerifyLock passes; the
// hook then blocks A's state-tag manifest PUT; a rival takes over the lock
// tag while A's bytes are in flight; the hook releases; A's write LANDS while
// the rival holds the lock. GREEN PROVES THE HOLE: verify-then-write is not
// atomic CAS (OCI tags have no CAS — ADR-0001). If this test ever fails, the
// window has been closed and this documentation is stale.
// Layer: fake-only (blocking hook + direct rival tag install).
func TestStateStoreWriteTOCTOULimitation(t *testing.T) {
	ctx := context.Background()

	engaged := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := func(r *http.Request) (<-chan struct{}, int) {
		// Block only the state-tag manifest PUT (the step that makes A's
		// bytes visible), after VerifyLock has already passed.
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+testStateTag) {
			select {
			case engaged <- struct{}{}:
			default:
			}
			return release, 0
		}
		return nil, 0
	}
	_, reg, ssd := newConcurrencyStore(t, "", gate)

	locker := testNewInstance(t, ssd)
	writer := testNewInstance(t, ssd)

	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("toctou-payload")}, &fwss.WriteResponse{})
	}()

	// A's VerifyLock has passed; its state PUT is now parked in the gate.
	waitSignal(t, engaged, "A's blocked state-tag PUT")

	// While A's bytes are in flight, a rival takes over the lock tag. The
	// direct fake call proceeds because the blocked request has not reached
	// the fake's mutex yet.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival-takeover"))

	// Release A's PUT: its bytes land under the rival's lock.
	close(release)
	waitGroup(t, &wg)

	readResp := &fwss.ReadResponse{}
	writer.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "toctou-payload" {
		t.Fatalf("A's stale bytes did not land: read = %q", readResp.StateBytes)
	}
	if !reg.HasTag(testLockTag) {
		t.Error("rival's lock tag missing after takeover")
	}
	// A can no longer reproduce the write — the lock is the rival's now —
	// but the stale bytes from the race are already in place.
	if err := ssd.client.VerifyLock(ctx, "default", lockResp.LockID); err == nil {
		t.Error("VerifyLock still passes for A after rival takeover; hook scenario changed")
	}
}

// ─── 10. -lock=false: unverified write succeeds ───────────────────────────────

// TestStateStoreNoLockWriteBestEffort — Property (issue #28 I10): with no
// local lock ever acquired (Terraform's -lock=false), Write succeeds without
// any ownership verification. Documented as BEST-EFFORT: there is no
// ownership check and no registry-side enforcement; concurrent -lock=false
// writers silently last-writer-wins. Layer: logical.
func TestStateStoreNoLockWriteBestEffort(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	writer := testNewInstance(t, ssd)
	if len(ssd.lockIDs) != 0 {
		t.Fatalf("precondition: unexpected local registrations %v", ssd.lockIDs)
	}

	writeResp := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("unlocked-write")}, writeResp)
	if writeResp.Diagnostics.HasError() {
		t.Fatalf("-lock=false write rejected: %v", writeResp.Diagnostics)
	}

	readResp := &fwss.ReadResponse{}
	writer.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "unlocked-write" {
		t.Errorf("read = %q, want %q", readResp.StateBytes, "unlocked-write")
	}
	if reg.HasTag(testLockTag) {
		t.Error("a lock tag appeared during an unlocked write")
	}
}

// ─── 11. Lock-tag retry: fail closed on a rival takeover (regression guard) ───

// TestStateStoreLockTagRetryFailClosed — Property (positive regression guard;
// replaces the former TestStateStoreRetryBlindLockTagLimitation green-hole
// test after the mutable-tag retry-safety change): a transient failure on A's
// first lock-tag publication must NOT be blindly re-applied. The hook rejects
// A's first lock-tag PUT with a transient 500; while A observes the tag
// (hook blocks A's first post-failure lock-tag read), a rival installs its
// lock directly in the registry; A's observation then sees a foreign digest
// and A FAILS with a LockError-class diagnostic naming the holder, instead of
// re-tagging. B remains holder, and the lock tag was never clobbered (exactly
// one lock-tag PUT reached the fake).
// Layer: distributed + fake-only (transient injection + observation gate).
func TestStateStoreLockTagRetryFailClosed(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	lockPuts := 0
	lockTagReads := 0 // HEAD/GET on the lock tag after the first PUT failed
	firstPutFailed := false
	firstRejected := make(chan struct{}, 1)
	observing := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := func(r *http.Request) (<-chan struct{}, int) {
		isLockTag := strings.HasSuffix(r.URL.Path, "/manifests/"+testLockTag)
		if !isLockTag {
			return nil, 0
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			lockPuts++
			if lockPuts == 1 {
				firstPutFailed = true
				select {
				case firstRejected <- struct{}{}:
				default:
				}
				// Transient 500: the client must observe, not blind-retry.
				return nil, http.StatusInternalServerError
			}
		case http.MethodHead, http.MethodGet:
			if firstPutFailed {
				lockTagReads++
				if lockTagReads == 1 {
					// Park A's post-failure observation of the tag.
					select {
					case observing <- struct{}{}:
					default:
					}
					return release, 0
				}
			}
		}
		return nil, 0
	}
	_, reg, ssd := newConcurrencyStore(t, "", gate)

	a := testNewInstance(t, ssd)

	aResp := &fwss.LockResponse{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, aResp)
	}()

	// A's first lock-tag PUT was rejected; A is now parked observing the tag.
	waitSignal(t, firstRejected, "A's first lock-tag PUT rejection")
	waitSignal(t, observing, "A's parked observation of the lock tag")

	// While A observes, a rival acquires the lock (direct registry install,
	// as in the contention tests). This lands while A's re-tag is deferred.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	// Release A's observation: it must see the rival's digest and fail closed.
	close(release)
	waitGroup(t, &wg)

	// Fail-closed assertions: A loses, the rival stays holder, and A's
	// publication was never blindly re-applied.
	if !aResp.Diagnostics.HasError() {
		t.Fatalf("A's lock succeeded over a rival that acquired in the observation gap")
	}
	if aResp.LockID != "" {
		t.Errorf("A failed but returned LockID %q", aResp.LockID)
	}
	if !strings.Contains(aResp.Diagnostics[0].Detail(), "rival") {
		t.Errorf("A's diagnostic does not name the rival holder: %q", aResp.Diagnostics[0].Detail())
	}
	if len(ssd.lockIDs) != 0 {
		t.Errorf("failed lock registered locally: %v", ssd.lockIDs)
	}
	if err := ssd.client.VerifyLock(ctx, "default", "rival"); err != nil {
		t.Errorf("rival no longer holds the lock: %v", err)
	}
	mu.Lock()
	puts := lockPuts
	mu.Unlock()
	if puts != 1 {
		t.Errorf("lock-tag PUT calls = %d, want exactly 1 (A's publication must not be re-applied)", puts)
	}
	if !reg.HasTag(testLockTag) {
		t.Error("lock tag missing after rival acquisition")
	}
}

// ─── 12. Stored-lease expiry: stale holder refused at VerifyLock ──────────────

// TestStateStoreTTLExpiryRefusesStaleWrite — Property (positive regression
// guard, Fase D; replaces the former TestStateStoreTTLExpiryDuringWriteLimitation
// green-hole test): after A's STORED lease expires, A's Write is refused at
// VerifyLock (holder matches, but the manifest's own lease_expiry is past the
// verifier's clock), and NO state bytes land. The stored expiry governs — the
// verifier's configured LockTTL is irrelevant to the check. The residual
// window (verification passing BEFORE expiry, expiry occurring during the
// in-flight Put) is separately covered by Test 18
// (TestStateStoreWriteExpiryDuringW1Limitation) and stays green-proves-hole.
// Layer: distributed + fake-only (time-based stored staleness).
func TestStateStoreTTLExpiryRefusesStaleWrite(t *testing.T) {
	ctx := context.Background()
	reg, baseURL := newConcurrencyRegistry(t, nil)
	_, ssdA := newConfiguredStore(t, "50ms", baseURL, 0)
	_, ssdB := newConfiguredStore(t, "50ms", baseURL, 0)

	aLocker := testNewInstance(t, ssdA)
	aWriter := testNewInstance(t, ssdA)

	aResp := &fwss.LockResponse{}
	aLocker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, aResp)
	if aResp.Diagnostics.HasError() {
		t.Fatalf("A lock: %v", aResp.Diagnostics)
	}

	// Sleep past the 50ms stored lease (10× TTL margin). The lease is now
	// expired for everyone, but A is still the tag holder.
	time.Sleep(200 * time.Millisecond)

	// A writes past its own expiry: VerifyLock must refuse at the stored
	// lease — no state bytes may land.
	writeResp := &fwss.WriteResponse{}
	aWriter.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("expired-lease-write")}, writeResp)
	if !writeResp.Diagnostics.HasError() {
		t.Fatal("expired-lease write was accepted; stored-lease enforcement has regressed — update this regression guard")
	}
	if !strings.Contains(writeResp.Diagnostics[0].Summary(), "no longer held") {
		t.Errorf("summary = %q, want the lost-lock framing", writeResp.Diagnostics[0].Summary())
	}
	if !strings.Contains(writeResp.Diagnostics[0].Detail(), "expired") {
		t.Errorf("detail = %q, want it to name the expired stored lease", writeResp.Diagnostics[0].Detail())
	}
	if reg.HasTag(testStateTag) {
		t.Error("state bytes landed despite an expired stored lease")
	}
	// A's local registration must survive the refused write.
	if id, ok := ssdA.lockFor("default"); !ok || id != aResp.LockID {
		t.Errorf("A's registration = (%q, %v), want (%q, true) preserved", id, ok, aResp.LockID)
	}

	// A rival's real takeover is what finally stops A: B is a separate client
	// configuration whose Lock sees A's stored lease as stale and clears it.
	// (B does not write here: with a 50ms TTL its own lease also expires
	// during acquisition — write-within-lease after takeover is covered by
	// Test 4 with a 1s TTL.)
	b := testNewInstance(t, ssdB)
	bResp := &fwss.LockResponse{}
	b.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, bResp)
	if bResp.Diagnostics.HasError() {
		t.Fatalf("B takeover: %v", bResp.Diagnostics)
	}
	if bResp.LockID == aResp.LockID {
		t.Error("B acquired A's lock ID; takeover did not mint a new holder")
	}
	if !reg.HasTag(testLockTag) {
		t.Error("lock tag missing after B's takeover")
	}
}

// ─── 13. Concurrent writes with version retention enabled ─────────────────────

// TestStateStoreConcurrentWritesVersionRetention — Property (Gate 1 finding
// #6): with max_versions > 0, concurrent Put calls each run currentStateVersion
// (read the state tag / list stver tags) and then Tag the NEXT version number;
// the read→tag sequence is not synchronized, so concurrent writers can pick the
// same nextVersion or skip numbers — enforceVersionRetention then races with
// the concurrent tags it tries to prune. This test documents what stays TRUE
// under that race: no panic, exactly ONE payload readable back (LWW on the
// state tag), at most max_versions stver-* tags survive, and every surviving
// version tag still resolves to a manifest present in the fake registry.
// Layer: Go-race + fake-only (version-tag inspection via the fake's tag table).
func TestStateStoreConcurrentWritesVersionRetention(t *testing.T) {
	ctx := context.Background()
	reg, baseURL := newConcurrencyRegistry(t, nil)
	_, ssd := newConfiguredStore(t, "", baseURL, 3)

	const n = 8
	payloads := make([]string, n)
	instances := make([]*OCIStateStore, n)
	for i := range instances {
		payloads[i] = "vret-payload-" + string(rune('a'+i))
		instances[i] = testNewInstance(t, ssd)
	}

	writeOK := make([]bool, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: all writers released at once
			resp := &fwss.WriteResponse{}
			instances[i].Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte(payloads[i])}, resp)
			writeOK[i] = !resp.Diagnostics.HasError()
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	for i, ok := range writeOK {
		if !ok {
			t.Fatalf("writer %d failed", i)
		}
	}

	// Settle the async retention goroutines before inspecting tags.
	ssd.client.WaitForRetention()

	// Exactly one LWW survivor readable on the state tag.
	readResp := &fwss.ReadResponse{}
	instances[0].Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	got := string(readResp.StateBytes)
	matches := 0
	for _, p := range payloads {
		if got == p {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("read back %q: matches %d of %d known payloads, want exactly one LWW survivor", got, matches, n)
	}

	// Version-tag consistency under the fake: at most max_versions stver tags
	// survive, and each still resolves to a manifest the registry holds.
	reg.mu.Lock()
	versionTags := make([]string, 0, len(reg.tagOf))
	for tag := range reg.tagOf {
		if strings.HasPrefix(tag, testVersionTagPrefix) {
			versionTags = append(versionTags, tag)
		}
	}
	resolved := make([]string, 0, len(versionTags))
	for _, tag := range versionTags {
		dgst := reg.tagOf[tag]
		if _, present := reg.manifest[dgst]; present {
			resolved = append(resolved, tag)
		}
	}
	reg.mu.Unlock()

	if len(versionTags) > 3 {
		t.Errorf("%d version tags survived retention, want ≤ 3 (max_versions): %v", len(versionTags), versionTags)
	}
	if len(resolved) != len(versionTags) {
		t.Errorf("unresolved version tags after retention: %v (all: %v)", diffTags(versionTags, resolved), versionTags)
	}
}

// diffTags returns the elements of all not present in the sub set.
func diffTags(all, sub []string) []string {
	set := make(map[string]struct{}, len(sub))
	for _, t := range sub {
		set[t] = struct{}{}
	}
	var out []string
	for _, t := range all {
		if _, ok := set[t]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// ─── 14. State-tag retry: fail closed on a newer write (regression guard) ─────

// TestStateStoreStateRetryFailClosed — Property (positive regression guard;
// replaces the former TestStateStoreStateRetryBlindOverwriteLimitation
// green-hole test after the mutable-tag retry-safety change): A's first
// state-tag PUT gets a transient 500; while A observes the tag (hook blocks
// A's first post-failure state-tag read), B (-lock=false, separate instance,
// no local registration) writes a NEWER payload through the real Put flow;
// A's observation then sees a foreign digest and A FAILS CLOSED with an
// actionable diagnostic instead of re-pushing/re-tagging over B's newer
// state. Final state remains B's payload. The ordinary W1 verify→Put window
// (Test 9) and the TTL-expiry-during-write window (Test 12) are unaffected.
// Layer: fake-only (transient injection + observation gate; sequential
// verification).
func TestStateStoreStateRetryFailClosed(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	statePuts := 0
	stateTagReads := 0 // HEAD/GET on the state tag after the first PUT failed
	firstPutFailed := false
	firstRejected := make(chan struct{}, 1)
	observing := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := func(r *http.Request) (<-chan struct{}, int) {
		isStateTag := strings.HasSuffix(r.URL.Path, "/manifests/"+testStateTag)
		if !isStateTag {
			return nil, 0
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			statePuts++
			if statePuts == 1 {
				firstPutFailed = true
				select {
				case firstRejected <- struct{}{}:
				default:
				}
				// Transient 500: A must observe, not blind-retry.
				return nil, http.StatusInternalServerError
			}
		case http.MethodHead, http.MethodGet:
			if firstPutFailed {
				stateTagReads++
				if stateTagReads == 1 {
					// Park A's post-failure observation of the tag.
					select {
					case observing <- struct{}{}:
					default:
					}
					return release, 0
				}
			}
		}
		return nil, 0
	}
	_, reg, ssd := newConcurrencyStore(t, "", gate)

	// A is the registered lock holder: its writes take the verified path.
	locker := testNewInstance(t, ssd)
	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	aWriter := testNewInstance(t, ssd)
	bWriter := testNewInstance(t, ssd) // -lock=false path: no local registration

	aResp := &fwss.WriteResponse{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		aWriter.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("payload-a")}, aResp)
	}()

	// A's first state-tag PUT was rejected; A is parked observing the tag.
	waitSignal(t, firstRejected, "A's first state-tag PUT rejection")
	waitSignal(t, observing, "A's parked observation of the state tag")

	// While A observes, B writes a newer state through the -lock=false path.
	bResp := &fwss.WriteResponse{}
	bWriter.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("payload-b")}, bResp)
	if bResp.Diagnostics.HasError() {
		t.Fatalf("B write in the observation gap: %v", bResp.Diagnostics)
	}

	// B's write had fully landed before A's observation resumes.
	preResp := &fwss.ReadResponse{}
	bWriter.Read(ctx, fwss.ReadRequest{StateID: "default"}, preResp)
	if preResp.Diagnostics.HasError() {
		t.Fatalf("pre-resume read: %v", preResp.Diagnostics)
	}
	if string(preResp.StateBytes) != "payload-b" {
		t.Fatalf("pre-resume read = %q, want %q; hook scenario changed", preResp.StateBytes, "payload-b")
	}

	// Release A's observation: it must see B's digest and fail closed.
	close(release)
	waitGroup(t, &wg)

	// Fail-closed assertions: A loses, B's newer state remains.
	if !aResp.Diagnostics.HasError() {
		t.Fatalf("A's write succeeded over B's newer state; the publication must fail closed after an ambiguous retry — update this regression guard")
	}
	if !strings.Contains(aResp.Diagnostics[0].Summary(), "publication conflict") {
		t.Errorf("A's diagnostic summary = %q, want the actionable fail-closed diagnostic", aResp.Diagnostics[0].Summary())
	}
	finalResp := &fwss.ReadResponse{}
	aWriter.Read(ctx, fwss.ReadRequest{StateID: "default"}, finalResp)
	if finalResp.Diagnostics.HasError() {
		t.Fatalf("final read: %v", finalResp.Diagnostics)
	}
	if string(finalResp.StateBytes) != "payload-b" {
		t.Errorf("final state = %q, want %q (A must not overwrite B's newer write)", finalResp.StateBytes, "payload-b")
	}
	// A's registration must be intact throughout (B never registered).
	if id, ok := ssd.lockFor("default"); !ok || id != lockResp.LockID {
		t.Errorf("A's registration = (%q, %v), want (%q, true); B's write must not touch the registry", id, ok, lockResp.LockID)
	}
	mu.Lock()
	puts := statePuts
	mu.Unlock()
	if puts != 2 {
		t.Errorf("state-tag PUT calls = %d, want exactly 2 (A's rejected attempt + B's write); A must not re-apply", puts)
	}
	if !reg.HasTag(testStateTag) {
		t.Error("state tag missing after B's write")
	}
}

// ─── 15. Partial version publication: state lands, version tag fails closed ──

// TestStateStorePartialVersionPublicationFailClosed — Property (Gate 2 #2):
// with max_versions > 0, when the state tag publishes successfully but the
// version-tag publication hits a transient failure and then a foreign digest,
// the Write must report a PARTIAL failure ("State version publication failed")
// — NOT a whole-write rejection — and the published state must remain
// readable. The failed version tag is never re-applied (false-failure
// preference: it may belong to a rival claiming the same version number).
// Layer: distributed + fake-only (transient injection + observation gate).
func TestStateStorePartialVersionPublicationFailClosed(t *testing.T) {
	ctx := context.Background()

	const versionTag = testVersionTagPrefix + "1"
	var mu sync.Mutex
	versionPuts := 0
	versionTagReads := 0
	versionTagFailed := false
	firstRejected := make(chan struct{}, 1)
	observing := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := func(r *http.Request) (<-chan struct{}, int) {
		if !strings.HasSuffix(r.URL.Path, "/manifests/"+versionTag) {
			return nil, 0
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			versionPuts++
			if versionPuts == 1 {
				versionTagFailed = true
				select {
				case firstRejected <- struct{}{}:
				default:
				}
				// Transient 500 on the FIRST version-tag publication.
				return nil, http.StatusInternalServerError
			}
		case http.MethodHead, http.MethodGet:
			if versionTagFailed {
				versionTagReads++
				if versionTagReads == 1 {
					// Park the post-failure observation of the version tag.
					select {
					case observing <- struct{}{}:
					default:
					}
					return release, 0
				}
			}
		}
		return nil, 0
	}
	reg, baseURL := newConcurrencyRegistry(t, gate)
	_, ssd := newConfiguredStore(t, "", baseURL, 3)
	writer := testNewInstance(t, ssd)

	writeResp := &fwss.WriteResponse{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("partial-payload")}, writeResp)
	}()

	// The version tag PUT was rejected; the writer is parked observing it.
	waitSignal(t, firstRejected, "first version-tag PUT rejection")
	waitSignal(t, observing, "parked observation of the version tag")

	// The state tag already published successfully while the version tag
	// publication is ambiguous: the state must be readable NOW.
	preResp := &fwss.ReadResponse{}
	writer.Read(ctx, fwss.ReadRequest{StateID: "default"}, preResp)
	if preResp.Diagnostics.HasError() {
		t.Fatalf("pre-resume read: %v", preResp.Diagnostics)
	}
	if string(preResp.StateBytes) != "partial-payload" {
		t.Fatalf("pre-resume read = %q, want %q; hook scenario changed", preResp.StateBytes, "partial-payload")
	}

	// A rival claims the version tag while the observation is parked.
	rivalBody := rivalLockManifest(t, "rival-version")
	reg.TagManifest(versionTag, rivalBody)

	// Release: the writer observes a foreign digest → fail closed, and the
	// publication must surface as PARTIAL (state visible, version failed).
	close(release)
	waitGroup(t, &wg)

	if !writeResp.Diagnostics.HasError() {
		t.Fatal("version-tag conflict did not surface any diagnostic")
	}
	if !strings.Contains(writeResp.Diagnostics[0].Summary(), "State version publication failed") {
		t.Errorf("summary = %q, want the actionable partial-publication diagnostic", writeResp.Diagnostics[0].Summary())
	}
	if strings.Contains(writeResp.Diagnostics[0].Summary(), "Failed to write state") {
		t.Errorf("summary = %q: a partial publication must not be reported as a whole-write rejection", writeResp.Diagnostics[0].Summary())
	}
	detail := writeResp.Diagnostics[0].Detail()
	if !strings.Contains(detail, testStateTag) || !strings.Contains(detail, versionTag) {
		t.Errorf("detail = %q, want it to name both the published state tag and the failed version tag", detail)
	}

	// The state remains readable after the partial failure.
	finalResp := &fwss.ReadResponse{}
	writer.Read(ctx, fwss.ReadRequest{StateID: "default"}, finalResp)
	if finalResp.Diagnostics.HasError() {
		t.Fatalf("final read: %v", finalResp.Diagnostics)
	}
	if string(finalResp.StateBytes) != "partial-payload" {
		t.Errorf("final state = %q, want %q (state must stay visible)", finalResp.StateBytes, "partial-payload")
	}

	// The version tag belongs to the rival: the failed publication was never
	// re-applied over it.
	mu.Lock()
	puts := versionPuts
	mu.Unlock()
	if puts != 1 {
		t.Errorf("version-tag PUT calls = %d, want exactly 1 (no re-apply after a transient/foreign outcome)", puts)
	}
	reg.mu.Lock()
	dgst := reg.tagOf[versionTag]
	reg.mu.Unlock()
	wantDigest := "sha256:" + hex.EncodeToString(hashBytes(rivalBody))
	if dgst != wantDigest {
		t.Errorf("version tag digest = %q, want the rival's %q (foreign tag must be preserved)", dgst, wantDigest)
	}
}

// ─── 16. VerifyLock interruption: context cancellation before lost-lock ──────

// TestStateStoreWriteVerifyLockInterrupted — Property (Gate 2 #3): when the
// pre-write VerifyLock fails because the operation was interrupted
// (context cancelled), the diagnostic must be the interruption style
// ("State write interrupted"), NOT "State lock no longer held" — the
// ownership could not be evaluated, so a lost-lock claim would be false.
// Layer: logical (diagnostic mapping, deterministic via a cancelled context).
func TestStateStoreWriteVerifyLockInterrupted(t *testing.T) {
	ctx := context.Background()
	_, _, ssd := newTestStore(t)

	locker := testNewInstance(t, ssd)
	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	writer := testNewInstance(t, ssd)
	interrupted, cancel := context.WithCancel(ctx)
	cancel() // cancelled before the VerifyLock can run

	writeResp := &fwss.WriteResponse{}
	writer.Write(interrupted, fwss.WriteRequest{StateID: "default", StateBytes: []byte("never-written")}, writeResp)
	if !writeResp.Diagnostics.HasError() {
		t.Fatal("write with a cancelled context reported no diagnostic")
	}
	summary := writeResp.Diagnostics[0].Summary()
	if !strings.Contains(summary, "interrupted") {
		t.Errorf("summary = %q, want the interruption diagnostic", summary)
	}
	if strings.Contains(summary, "no longer held") {
		t.Errorf("summary = %q: a cancelled ownership check must not be framed as a lost lock", summary)
	}
	// The cancelled write must not register or drop anything.
	if id, ok := ssd.lockFor("default"); !ok || id != lockResp.LockID {
		t.Errorf("registration = (%q, %v), want the lock preserved", id, ok)
	}
}

// ─── 17. Concurrent fwss.Lock wrapper (framework RNG workaround) ──────────────

// TestStateStoreConcurrentFwssLock — Property: the real OCIStateStore.Lock
// wrapper may be invoked concurrently (Terraform runs RPCs in parallel), and
// independent store CONFIGURATIONS must work in parallel too: actors are
// split across TWO independently initialized stores, each with its own
// stateStoreData, *oras.Client, and fake registry endpoint. All locks on
// DISTINCT workspaces (ws-0..7) succeed, LockIDs are non-empty and distinct,
// and each group's lockFor registration matches its own lock.
//
// This exercises the provider-side serialization of fwss.NewLockInfo
// (newLockInfoMu in oci.go) — a compatibility workaround for
// terraform-plugin-framework v1.19.0's unsynchronized package-level
// *rand.Rand in statestore.generateLockID. It is NOT distributed locking, and
// the upstream defect remains dependency-specific; the provider mutex removes
// the in-process RNG race for this provider path only.
//
// Meaningful under -race: a DATA RACE report in statestore.generateLockID
// from this test means the provider mutex no longer covers the call
// (regression). No sleeps: the start barrier creates the concurrency; each
// fake registry's mutex serializes HTTP requests within its endpoint.
// Layer: Go-race + logical (distinct workspaces; no same-workspace contention).
func TestStateStoreConcurrentFwssLock(t *testing.T) {
	ctx := context.Background()

	// Two independently initialized store configurations: separate
	// stateStoreData, separate *oras.Client, separate fake registry endpoints.
	const groups, perGroup = 2, 4
	const n = groups * perGroup
	ssds := make([]*stateStoreData, groups)
	for g := 0; g < groups; g++ {
		_, baseURL := newConcurrencyRegistry(t, nil)
		_, ssd := newConfiguredStore(t, "", baseURL, 0)
		ssds[g] = ssd
	}

	stateIDs := make([]string, n)
	ids := make([]string, n)
	actorGroup := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		g := i / perGroup
		actorGroup[i] = g
		instance := testNewInstance(t, ssds[g])
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: all Lock RPCs released at once
			resp := &fwss.LockResponse{}
			stateID := fmt.Sprintf("ws-%d", i)
			instance.Lock(ctx, fwss.LockRequest{StateID: stateID, Operation: "apply"}, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("Lock(%q) failed: %v", stateID, resp.Diagnostics)
				return
			}
			if resp.LockID == "" {
				t.Errorf("Lock(%q) returned an empty LockID", stateID)
			}
			stateIDs[i] = stateID
			ids[i] = resp.LockID
		}(i)
	}
	close(start)
	waitGroup(t, &wg)

	seen := make(map[string]string, n)
	perGroupSuccess := make([]int, groups)
	for i := 0; i < n; i++ {
		if stateIDs[i] == "" {
			t.Fatalf("actor %d did not record its StateID", i)
		}
		if id, ok := seen[ids[i]]; ok {
			t.Errorf("LockID %q used by both %q and %q; want distinct", ids[i], id, stateIDs[i])
		}
		seen[ids[i]] = stateIDs[i]
		perGroupSuccess[actorGroup[i]]++
		// Each group's own lock registry must hold its own actor's lock.
		if got, ok := ssds[actorGroup[i]].lockFor(stateIDs[i]); !ok || got != ids[i] {
			t.Errorf("registration for %q (group %d) = (%q, %v), want (%q, true)", stateIDs[i], actorGroup[i], got, ok, ids[i])
		}
	}
	for g := 0; g < groups; g++ {
		if perGroupSuccess[g] != perGroup {
			t.Errorf("group %d successful locks = %d, want %d", g, perGroupSuccess[g], perGroup)
		}
	}
}

// ─── 18. Expiry-during-W1 LIMITATION: verified-then-expired write still lands ─

// TestStateStoreWriteExpiryDuringW1Limitation — LIMITATION (Fase D; keeps the
// W1 evidence after the stored-lease enforcement): the stored-lease check
// (Test 12) runs at VerifyLock. If verification passes BEFORE expiry and the
// lease expires while the state Put is in flight, the bytes STILL LAND —
// nothing re-verifies ownership between VerifyLock and the mutable tag
// publication (W1 unchanged by the TTL hardening; registries do not enforce
// leases). GREEN PROVES THE HOLE: if this test fails, W1 has been closed for
// the expiry case and the documentation must be updated.
// Deterministic sync: A locks with a 500ms stored lease; the state-tag gate
// parks A's in-flight Put (VerifyLock has already passed, lease still
// valid); the main goroutine waits out the expiry (the ONE controlled
// wall-clock wait — expiry itself is time-based) and releases. No rival; the
// residual is purely verify-then-expire-then-land.
// Layer: distributed + fake-only (time-based; HTTP gate).
func TestStateStoreWriteExpiryDuringW1Limitation(t *testing.T) {
	ctx := context.Background()

	engaged := make(chan struct{}, 1)
	release := make(chan struct{})
	gate := func(r *http.Request) (<-chan struct{}, int) {
		// Park only the state-tag manifest PUT (after VerifyLock passed).
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+testStateTag) {
			select {
			case engaged <- struct{}{}:
			default:
			}
			return release, 0
		}
		return nil, 0
	}
	_, reg, ssd := newConcurrencyStore(t, "500ms", gate)

	locker := testNewInstance(t, ssd)
	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	writer := testNewInstance(t, ssd)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("expiry-w1-payload")}, &fwss.WriteResponse{})
	}()

	// VerifyLock has passed (lease still valid); A's state PUT is parked.
	waitSignal(t, engaged, "A's parked state-tag PUT")

	// Expiry passes while the verified write is still in flight.
	time.Sleep(600 * time.Millisecond)

	// Release: nothing re-verifies the (now expired) lease, so the stale
	// bytes land.
	close(release)
	waitGroup(t, &wg)

	readResp := &fwss.ReadResponse{}
	writer.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "expiry-w1-payload" {
		t.Fatalf("verified-then-expired write did not land: read = %q; W1 may be closed for the expiry case — update the LIMITATION", readResp.StateBytes)
	}
	if !reg.HasTag(testStateTag) {
		t.Error("state tag missing after the in-flight write landed")
	}
	if !reg.HasTag(testLockTag) {
		t.Error("lock tag missing; hook scenario changed")
	}
}
