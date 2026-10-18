// Focused adapter-boundary failure coverage for issue #29: the non-Write
// StateStore RPCs (Read, DeleteState, GetStates, Lock, Unlock) must translate
// engine failures into Terraform error diagnostics — never silent success —
// and must keep the adapter-LOCAL lock registry consistent.
//
// Failure injection is HTTP 500 (a transient 5xx for the pinned oras-state
// engine, which retries it a bounded number of times with second-scale
// backoff; 404 is deliberately avoided because the engine maps it to
// "absent", e.g. Delete on a missing state is a no-op success).
//
// Claims are limited to guaranteed local bookkeeping (stateStoreData.lockIDs):
// after a generic failure the remote effect is uncertain, so NO assertion is
// made about remote registry state. Cancellation tests use a context cancelled
// before the RPC; the engine's retry loop re-checks ctx.Err() and surfaces the
// cancellation without backoff delays, so these subtests stay fast and
// sleep-free. Write-side diagnostics are covered by recovery_test.go and
// oci_concurrency_test.go.
package statestore

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
)

// assertCancellationDetail asserts the diagnostic detail carries the
// underlying context.Canceled error, proving the cancellation itself
// propagated through the engine (not merely that some error occurred).
func assertCancellationDetail(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if !strings.Contains(diags[0].Detail(), context.Canceled.Error()) {
		t.Errorf("detail = %q, want it to carry %q", diags[0].Detail(), context.Canceled.Error())
	}
}

func assertHTTP500Detail(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if !strings.Contains(diags[0].Detail(), "response status code 500") {
		t.Errorf("detail = %q, want it to carry the HTTP 500 response", diags[0].Detail())
	}
}

// failAllGateway returns a gate that answers every HTTP request with 500.
func failAllGateway() func(*http.Request) (<-chan struct{}, int) {
	return func(*http.Request) (<-chan struct{}, int) {
		return nil, http.StatusInternalServerError
	}
}

// TestStateStoreRPCGenericFailureDiagnostics — a generic registry failure
// (HTTP 500) on Read, DeleteState, GetStates or Lock must produce an error
// diagnostic naming the failed RPC (no false success), and a failed Lock must
// not register anything in the local lock registry.
// Layer: logical (diagnostic mapping via the hookedHandler gate).
func TestStateStoreRPCGenericFailureDiagnostics(t *testing.T) {
	ctx := context.Background()
	s, _, ssd := newConcurrencyStore(t, "", failAllGateway())

	t.Run("read", func(t *testing.T) {
		resp := &fwss.ReadResponse{}
		s.Read(ctx, fwss.ReadRequest{StateID: "default"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("Read against a failing registry reported no diagnostic")
		}
		assertHTTP500Detail(t, resp.Diagnostics)
		if !strings.Contains(resp.Diagnostics[0].Summary(), "Failed to read state") {
			t.Errorf("summary = %q, want a read-failure diagnostic", resp.Diagnostics[0].Summary())
		}
		if resp.StateBytes != nil {
			t.Errorf("StateBytes = %v, want nil on failure", resp.StateBytes)
		}
	})

	t.Run("delete_state", func(t *testing.T) {
		resp := &fwss.DeleteStateResponse{}
		s.DeleteState(ctx, fwss.DeleteStateRequest{StateID: "default"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("DeleteState against a failing registry reported no diagnostic")
		}
		assertHTTP500Detail(t, resp.Diagnostics)
		if !strings.Contains(resp.Diagnostics[0].Summary(), "Failed to delete state") {
			t.Errorf("summary = %q, want a delete-failure diagnostic", resp.Diagnostics[0].Summary())
		}
	})

	t.Run("get_states", func(t *testing.T) {
		resp := &fwss.GetStatesResponse{}
		s.GetStates(ctx, fwss.GetStatesRequest{}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("GetStates against a failing registry reported no diagnostic")
		}
		assertHTTP500Detail(t, resp.Diagnostics)
		if !strings.Contains(resp.Diagnostics[0].Summary(), "Failed to list states") {
			t.Errorf("summary = %q, want a list-failure diagnostic", resp.Diagnostics[0].Summary())
		}
		if len(resp.StateIDs) != 0 {
			t.Errorf("StateIDs = %v, want empty on failure", resp.StateIDs)
		}
	})

	t.Run("lock", func(t *testing.T) {
		resp := &fwss.LockResponse{}
		s.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("Lock against a failing registry reported no diagnostic")
		}
		assertHTTP500Detail(t, resp.Diagnostics)
		if !strings.Contains(resp.Diagnostics[0].Summary(), "Failed to acquire state lock") {
			t.Errorf("summary = %q, want a lock-acquisition-failure diagnostic", resp.Diagnostics[0].Summary())
		}
		if resp.LockID != "" {
			t.Errorf("LockID = %q, want empty on failure", resp.LockID)
		}
		// Guaranteed local bookkeeping: a failed acquire registers nothing.
		if len(ssd.lockIDs) != 0 {
			t.Errorf("failed lock registered locally: %v", ssd.lockIDs)
		}
	})
}

// TestStateStoreUnlockFailureKeepsRegistration — a generic registry failure
// (HTTP 500) during Unlock must produce an error diagnostic and leave the
// local lock registration intact (forgetLockIf is only reached on success).
// Remote lock state is deliberately not asserted: after a transient failure
// the release may or may not have landed.
// Layer: logical (diagnostic mapping + local bookkeeping).
func TestStateStoreUnlockFailureKeepsRegistration(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	fail := false
	gate := func(*http.Request) (<-chan struct{}, int) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, http.StatusInternalServerError
		}
		return nil, 0
	}
	s, _, ssd := newConcurrencyStore(t, "", gate)

	lockResp := &fwss.LockResponse{}
	s.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	mu.Lock()
	fail = true
	mu.Unlock()

	unlockResp := &fwss.UnlockResponse{}
	s.Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: lockResp.LockID}, unlockResp)
	if !unlockResp.Diagnostics.HasError() {
		t.Fatal("Unlock against a failing registry reported no diagnostic")
	}
	assertHTTP500Detail(t, unlockResp.Diagnostics)
	if !strings.Contains(unlockResp.Diagnostics[0].Summary(), "Failed to release state lock") {
		t.Errorf("summary = %q, want a release-failure diagnostic", unlockResp.Diagnostics[0].Summary())
	}
	// Guaranteed local bookkeeping: a failed unlock keeps the registration.
	if id, ok := ssd.lockFor("default"); !ok || id != lockResp.LockID {
		t.Errorf("registration after failed unlock = (%q, %v), want the lock preserved", id, ok)
	}
}

// TestStateStoreRPCContextCancellation — cancellation before each non-Write
// RPC must surface as an error diagnostic (no false success) and keep the
// local lock registry consistent: a cancelled Lock registers nothing, a
// cancelled Unlock preserves the registration.
// Layer: logical (diagnostic mapping via a pre-cancelled context).
func TestStateStoreRPCContextCancellation(t *testing.T) {
	s, _, ssd := newTestStore(t)

	t.Run("read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp := &fwss.ReadResponse{}
		s.Read(ctx, fwss.ReadRequest{StateID: "cxl-read"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("cancelled Read reported no diagnostic")
		}
		assertCancellationDetail(t, resp.Diagnostics)
		if resp.StateBytes != nil {
			t.Errorf("StateBytes = %v, want nil on cancellation", resp.StateBytes)
		}
	})

	t.Run("delete_state", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp := &fwss.DeleteStateResponse{}
		s.DeleteState(ctx, fwss.DeleteStateRequest{StateID: "cxl-delete"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("cancelled DeleteState reported no diagnostic")
		}
		assertCancellationDetail(t, resp.Diagnostics)
	})

	t.Run("get_states", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp := &fwss.GetStatesResponse{}
		s.GetStates(ctx, fwss.GetStatesRequest{}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("cancelled GetStates reported no diagnostic")
		}
		assertCancellationDetail(t, resp.Diagnostics)
		if len(resp.StateIDs) != 0 {
			t.Errorf("StateIDs = %v, want empty on cancellation", resp.StateIDs)
		}
	})

	t.Run("lock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		resp := &fwss.LockResponse{}
		s.Lock(ctx, fwss.LockRequest{StateID: "cxl-lock", Operation: "apply"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("cancelled Lock reported no diagnostic")
		}
		assertCancellationDetail(t, resp.Diagnostics)
		if resp.LockID != "" {
			t.Errorf("LockID = %q, want empty on cancellation", resp.LockID)
		}
		if _, registered := ssd.lockFor("cxl-lock"); registered {
			t.Error("cancelled Lock registered a local lock")
		}
	})

	t.Run("unlock", func(t *testing.T) {
		// Acquire for real first, so the cancelled unlock has a registration
		// whose preservation can be asserted.
		lockResp := &fwss.LockResponse{}
		s.Lock(context.Background(), fwss.LockRequest{StateID: "cxl-unlock", Operation: "apply"}, lockResp)
		if lockResp.Diagnostics.HasError() {
			t.Fatalf("lock: %v", lockResp.Diagnostics)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		unlockResp := &fwss.UnlockResponse{}
		s.Unlock(ctx, fwss.UnlockRequest{StateID: "cxl-unlock", LockID: lockResp.LockID}, unlockResp)
		if !unlockResp.Diagnostics.HasError() {
			t.Fatal("cancelled Unlock reported no diagnostic")
		}
		assertCancellationDetail(t, unlockResp.Diagnostics)
		if id, ok := ssd.lockFor("cxl-unlock"); !ok || id != lockResp.LockID {
			t.Errorf("registration after cancelled unlock = (%q, %v), want the lock preserved", id, ok)
		}
	})
}
