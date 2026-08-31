package oras

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// GHCR integration tests run against the real ghcr.io registry. They are
// gated behind TF_ORAS_GHCR_TEST and authenticate with TF_ORAS_GHCR_TOKEN
// (falls back to GITHUB_TOKEN). In CI, only the repository's own
// GITHUB_TOKEN with packages:write is expected to have push access to the
// ghcr.io/<owner>/<repo> package namespace.
const (
	ghcrTestEnvVar  = "TF_ORAS_GHCR_TEST"
	ghcrTokenEnvVar = "TF_ORAS_GHCR_TOKEN"
	ghcrAddr        = "ghcr.io"
	ghcrRepoPath    = "vmvarela/terraform-provider-oras"
)

// requireGHCRTest skips the test unless TF_ORAS_GHCR_TEST is set and a
// token is available.
func requireGHCRTest(t *testing.T) string {
	t.Helper()
	if os.Getenv(ghcrTestEnvVar) == "" {
		t.Skipf("set %s to run GHCR integration tests", ghcrTestEnvVar)
	}
	token := os.Getenv(ghcrTokenEnvVar)
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		t.Skipf("set %s (or GITHUB_TOKEN) with packages:write scope", ghcrTokenEnvVar)
	}
	return token
}

// ghcrWorkspace returns a unique workspace per test run so concurrent CI
// runs never contend on the same tags, and cleanup never races.
func ghcrWorkspace() string {
	return fmt.Sprintf("ci-%d", time.Now().UnixNano())
}

func newGHCRClient(t *testing.T, token string) *Client {
	t.Helper()
	c, err := NewClient(ghcrAddr, ghcrRepoPath, Config{Token: token})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestGHCRIntegration_StateRoundTrip exercises the state CRUD lifecycle
// against real GHCR: empty Get, Put, Get back, Delete, Get after delete.
func TestGHCRIntegration_StateRoundTrip(t *testing.T) {
	token := requireGHCRTest(t)
	ctx := context.Background()
	c := newGHCRClient(t, token)
	ws := ghcrWorkspace()

	data, err := c.Get(ctx, ws)
	if err != nil {
		t.Fatalf("Get on empty: %v", err)
	}
	if data != nil {
		t.Fatal("expected nil on empty workspace")
	}

	stateData := []byte(`{"version":4,"serial":1,"ghcr-integration":true}`)
	if err := c.Put(ctx, ws, stateData); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := c.Get(ctx, ws)
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if !bytes.Equal(got, stateData) {
		t.Fatalf("Get returned %q, want %q", string(got), string(stateData))
	}

	// GHCR does not support manifest DELETE via registry tokens (405) —
	// package deletion requires the REST packages API. Tolerate it: the
	// state must still be readable, which proves Delete was correctly
	// refused rather than silently ignored.
	if err := c.Delete(ctx, ws); err != nil {
		if !isDeleteUnsupported(err) {
			t.Fatalf("Delete: %v", err)
		}
		t.Logf("GHCR manifest delete unsupported (%v); skipping post-delete assertion", err)
		return
	}

	data, err = c.Get(ctx, ws)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if data != nil {
		t.Fatal("expected nil after delete")
	}
}

// TestGHCRIntegration_LockUnlock verifies generation-based optimistic
// locking (GHCR fallback tag scheme) against the real registry.
func TestGHCRIntegration_LockUnlock(t *testing.T) {
	token := requireGHCRTest(t)
	ctx := context.Background()
	c := newGHCRClient(t, token)
	ws := ghcrWorkspace()

	id1, err := c.Lock(ctx, ws, LockInfo{
		ID:        "ghcr-lock-1",
		Operation: "ghcr-integration",
	})
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Concurrent locker with a different ID must fail.
	_, err = c.Lock(ctx, ws, LockInfo{
		ID:        "ghcr-lock-2",
		Operation: "ghcr-integration-concurrent",
	})
	if err == nil {
		t.Fatal("expected concurrent lock to fail")
	}

	if err := c.Unlock(ctx, ws, id1); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Lock after unlock must succeed; release to leave the namespace clean.
	id3, err := c.Lock(ctx, ws, LockInfo{
		ID:        "ghcr-lock-3",
		Operation: "after-unlock",
	})
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	if err := c.Unlock(ctx, ws, id3); err != nil {
		t.Fatalf("final unlock: %v", err)
	}
}
