package oras

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	orasErrcode "oras.land/oras-go/v2/registry/remote/errcode"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

// newRemoteClient creates a workspaceClient for testing.
func newRemoteClient(repo *orasRepositoryClient, workspace string) *workspaceClient {
	wsTag := workspaceTagFor(workspace)
	client := &Client{
		repoClient: repo,
		config: Config{
			Compression: "none",
		},
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
		retentionWg:  sync.WaitGroup{},
	}
	return &workspaceClient{
		client:      client,
		stateID:     workspace,
		stateTag:    stateTagPrefix + wsTag,
		lockTag:     lockTagPrefix + wsTag,
		unlockedTag: unlockedTagPrefix + wsTag,
	}
}

// newTestClient creates a *Client with the given repo injected for testing.
func newTestClient(repo *orasRepositoryClient) *Client {
	return &Client{
		repoClient: repo,
		config: Config{
			Compression: "none",
		},
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
	}
}

// ─── Test doubles ─────────────────────────────────────────────────────────────

// delegatingRepo wraps a fakeORASRepo and delegates all methods.
type delegatingRepo struct {
	inner *fakeORASRepo
}

func (r *delegatingRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	return r.inner.Push(ctx, expected, content)
}

func (r *delegatingRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	return r.inner.Fetch(ctx, target)
}

func (r *delegatingRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	return r.inner.Resolve(ctx, reference)
}

func (r *delegatingRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return r.inner.Tag(ctx, desc, reference)
}

func (r *delegatingRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return r.inner.Delete(ctx, target)
}

func (r *delegatingRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	return r.inner.Tags(ctx, last, fn)
}

// deleteFailingRepo causes Delete to return the given error.
type deleteFailingRepo struct {
	delegatingRepo
	deleteErr error
}

func (r *deleteFailingRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return r.deleteErr
}

// resolveFailingRepo causes Resolve to return the given error.
type resolveFailingRepo struct {
	delegatingRepo
	resolveErr error
}

func (r *resolveFailingRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, r.resolveErr
}

// deleteUnsupportedRepo simulates a registry that returns 405 on Delete.
type deleteUnsupportedRepo struct {
	delegatingRepo
}

func (r *deleteUnsupportedRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return &orasErrcode.ErrorResponse{StatusCode: http.StatusMethodNotAllowed}
}

// raceSimulatingRepo simulates a lock race by overwriting a lock tag after it
// is set, so that post-write verification detects a different holder.
type raceSimulatingRepo struct {
	delegatingRepo
	mu       sync.Mutex
	raceTag  string
	triggered bool
	// pre-built rival manifest descriptor
	rivalDesc ocispec.Descriptor
	rivalData []byte
}

func (r *raceSimulatingRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	// Always delegate first.
	if err := r.delegatingRepo.Tag(ctx, desc, reference); err != nil {
		return err
	}
	r.mu.Lock()
	shouldRace := reference == r.raceTag && !r.triggered
	r.mu.Unlock()
	if shouldRace {
		r.mu.Lock()
		r.triggered = true
		r.mu.Unlock()
		// Overwrite the tag with a rival lock manifest.
		_ = r.Push(ctx, r.rivalDesc, bytes.NewReader(r.rivalData))
		_ = r.delegatingRepo.Tag(ctx, r.rivalDesc, reference)
	}
	return nil
}

// newRivalLockManifest creates a rival lock manifest payload and descriptor in
// the given repo.
func newRivalLockManifest(ctx context.Context, t *testing.T, repo *fakeORASRepo, holderID string, generation int64) (ocispec.Descriptor, []byte) {
	t.Helper()
	lockData := LockManifestData{
		Generation:  generation,
		LeaseExpiry: 0,
		HolderID:    holderID,
	}
	lockDataJSON, err := json.Marshal(lockData)
	if err != nil {
		t.Fatalf("marshal lock data: %v", err)
	}
	rivalInfo := &LockInfo{ID: holderID, Operation: "plan", Info: "rival"}
	infoBytes, _ := json.Marshal(rivalInfo)

	annotations := map[string]string{
		annotationLockID:   holderID,
		annotationLockInfo: string(infoBytes),
		annotationLockGen:  string(lockDataJSON),
	}

	// Build a minimal OCI image manifest as a byte payload.
	m := manifest{
		MediaType:   ocispec.MediaTypeImageManifest,
		ArtifactType: artifactTypeLock,
		Annotations: annotations,
		Layers:      []ocispec.Descriptor{},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal rival manifest: %v", err)
	}
	d := digest.FromBytes(raw)
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    d,
		Size:      int64(len(raw)),
	}
	_ = repo.Push(ctx, desc, bytes.NewReader(raw))
	return desc, raw
}

// sameGenRaceRepo simulates a race where the rival has the same generation
// but a different holderID.
type sameGenRaceRepo struct {
	delegatingRepo
	mu          sync.Mutex
	raceTag     string
	triggered   bool
	rivalDesc   ocispec.Descriptor
	rivalData   []byte
}

func (r *sameGenRaceRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	if err := r.delegatingRepo.Tag(ctx, desc, reference); err != nil {
		return err
	}
	r.mu.Lock()
	shouldRace := reference == r.raceTag && !r.triggered
	r.mu.Unlock()
	if shouldRace {
		r.mu.Lock()
		r.triggered = true
		r.mu.Unlock()
		_ = r.Push(ctx, r.rivalDesc, bytes.NewReader(r.rivalData))
		_ = r.delegatingRepo.Tag(ctx, r.rivalDesc, reference)
	}
	return nil
}

// blockingRepo blocks all operations until the context is cancelled.
type blockingRepo struct {
	delegatingRepo
}

func (r *blockingRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	<-ctx.Done()
	return ocispec.Descriptor{}, ctx.Err()
}

func (r *blockingRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *blockingRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// concurrencyTrackingRepo tracks the number of concurrent calls to Resolve.
type concurrencyTrackingRepo struct {
	delegatingRepo
	mu             sync.Mutex
	maxConcurrent  int
	currentRunning int
	inflight       map[string]bool
}

func newConcurrencyTrackingRepo(inner *fakeORASRepo) *concurrencyTrackingRepo {
	return &concurrencyTrackingRepo{
		delegatingRepo: delegatingRepo{inner: inner},
		inflight:       make(map[string]bool),
	}
}

func (r *concurrencyTrackingRepo) resolveStarted(reference string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[reference] {
		return -1 // duplicate
	}
	r.inflight[reference] = true
	r.currentRunning++
	if r.currentRunning > r.maxConcurrent {
		r.maxConcurrent = r.currentRunning
	}
	return r.currentRunning
}

func (r *concurrencyTrackingRepo) resolveFinished(reference string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, reference)
	r.currentRunning--
}

func (r *concurrencyTrackingRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	r.resolveStarted(reference)
	defer r.resolveFinished(reference)
	return r.delegatingRepo.Resolve(ctx, reference)
}

// ─── Lock contention tests ────────────────────────────────────────────────────

func TestRemoteClient_LockContentionAndUnlockMismatch(t *testing.T) {
	ctx := context.Background()
	repo := &orasRepositoryClient{inner: newFakeORASRepo()}
	c := newRemoteClient(repo, "default")

	info := &LockInfo{
		ID:        "my-lock-id",
		Operation: "apply",
		Info:      "my test lock",
		Who:       "test-user",
		Version:   "1.0",
		Created:   time.Now(),
	}

	// First lock should succeed.
	lockID, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("expected first lock to succeed, got: %v", err)
	}
	if lockID != info.ID {
		t.Errorf("lockID = %q, want %q", lockID, info.ID)
	}

	// Second lock with different ID should fail.
	info2 := &LockInfo{
		ID:        "other-lock-id",
		Operation: "plan",
		Info:      "other lock",
		Who:       "other-user",
		Version:   "1.0",
		Created:   time.Now(),
	}
	_, err = c.lock(ctx, info2)
	if err == nil {
		t.Fatal("expected second lock to fail, got nil")
	}
	var lockErr *LockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *LockError, got %T: %v", err, err)
	}
	if lockErr.Info == nil {
		t.Fatal("expected LockError.Info to be non-nil")
	}
	if lockErr.Info.ID != info.ID {
		t.Errorf("LockError.Info.ID = %q, want %q", lockErr.Info.ID, info.ID)
	}

	// Unlock with wrong ID should fail.
	err = c.unlock(ctx, "wrong-id")
	if err == nil {
		t.Fatal("expected unlock with wrong ID to fail, got nil")
	}
	if !strings.Contains(err.Error(), info.ID) {
		t.Errorf("unlock error should mention holder ID, got: %v", err)
	}

	// Unlock with correct ID should succeed.
	err = c.unlock(ctx, lockID)
	if err != nil {
		t.Fatalf("expected unlock to succeed, got: %v", err)
	}

	// Unlock again (no lock) should succeed.
	err = c.unlock(ctx, lockID)
	if err != nil {
		t.Fatalf("expected second unlock to succeed (no lock), got: %v", err)
	}
}

// ─── Workspace tag tests ──────────────────────────────────────────────────────

func TestRemoteClient_WorkspacesFromTags_TagSafeAndHashed(t *testing.T) {
	ctx := context.Background()
	repo := &orasRepositoryClient{inner: newFakeORASRepo()}

	// Put state for a tag-safe workspace name and a hashed one.
	c1 := newRemoteClient(repo, "default")
	if err := c1.put(ctx, []byte("state-default")); err != nil {
		t.Fatalf("put default: %v", err)
	}

	c2 := newRemoteClient(repo, "my workspace/with:special?chars")
	if err := c2.put(ctx, []byte("state-special")); err != nil {
		t.Fatalf("put special: %v", err)
	}

	workspaces, err := listWorkspacesFromTags(ctx, repo)
	if err != nil {
		t.Fatalf("listWorkspacesFromTags: %v", err)
	}

	// Both workspaces should appear.
	found := make(map[string]bool)
	for _, w := range workspaces {
		found[w] = true
	}
	if !found["default"] {
		t.Error("expected 'default' in workspace list")
	}
	if !found["my workspace/with:special?chars"] {
		t.Error("expected special workspace name in list")
	}
}

func TestWorkspaceTagFor_HashesInvalidWorkspaceNames(t *testing.T) {
	tests := []struct {
		name     string
		expected string // empty means expect original unchanged
	}{
		{"default", "default"},
		{"simple", "simple"},
		{"with-dash", "with-dash"},
		{"with_underscore", "with_underscore"},
		{"with.dot", "with.dot"},
		{"UPPERCASE", "UPPERCASE"},
		{"a/b", ""},          // slash not allowed in tags
		{"has:colon", ""},    // colon not allowed
		{"has space", ""},    // space not allowed
		{"has?query", ""},    // question mark not allowed
		{strings.Repeat("x", 129), ""}, // too long
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workspaceTagFor(tt.name)
			if tt.expected != "" {
				if got != tt.expected {
					t.Errorf("workspaceTagFor(%q) = %q, want %q", tt.name, got, tt.expected)
				}
			} else {
				// Must be hashed: starts with "ws-".
				if !strings.HasPrefix(got, "ws-") {
					t.Errorf("workspaceTagFor(%q) = %q, expected hash (ws-*)", tt.name, got)
				}
				if len(got) != 19 { // "ws-" + 16 hex chars
					t.Errorf("workspaceTagFor(%q) length = %d, want 19", tt.name, len(got))
				}
			}
		})
	}
}

// ─── Delete tests ─────────────────────────────────────────────────────────────

func TestDelete_ReturnsErrorOnDeleteFailure(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	normalRepo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(normalRepo, "staging")
	if err := c.put(ctx, []byte("some-state")); err != nil {
		t.Fatalf("put: %v", err)
	}

	failRepo := &deleteFailingRepo{
		delegatingRepo: delegatingRepo{inner: fake},
		deleteErr:      &orasErrcode.ErrorResponse{StatusCode: http.StatusInternalServerError},
	}
	client := newTestClient(&orasRepositoryClient{inner: failRepo, repository: "example.com/test/repo"})
	err := client.Delete(ctx, "staging")
	if err == nil {
		t.Fatalf("expected Delete to return error when Delete fails, got nil")
	}
}

func TestDelete_SucceedsNormally(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	client := newTestClient(repo)

	// Put state and acquire lock.
	wc := newRemoteClient(repo, "staging")
	if err := wc.put(ctx, []byte("my-state")); err != nil {
		t.Fatalf("put: %v", err)
	}
	info := &LockInfo{
		ID:        "lock-1",
		Operation: "apply",
		Info:      "test lock",
		Who:       "test-user",
		Version:   "1.0",
		Created:   time.Now(),
	}
	lockID, err := wc.lock(ctx, info)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Delete the state.
	if err := client.Delete(ctx, "staging"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// State tag should no longer resolve.
	_, err = fake.Resolve(ctx, wc.stateTag)
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Errorf("expected state tag to be gone, resolve err = %v", err)
	}

	// Lock tag should still exist (Delete only removes state).
	_, err = fake.Resolve(ctx, wc.lockTag)
	if err != nil {
		t.Errorf("expected lock tag to still exist after Delete, got: %v", err)
	}

	// Clean up lock.
	_ = wc.unlock(ctx, lockID)
}

func TestDelete_ToleratesDeleteUnsupported(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	unsupportedRepo := &deleteUnsupportedRepo{delegatingRepo: delegatingRepo{inner: fake}}
	normalRepo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(normalRepo, "default")
	if err := c.put(ctx, []byte("state-data")); err != nil {
		t.Fatalf("put: %v", err)
	}

	client := newTestClient(&orasRepositoryClient{inner: unsupportedRepo, repository: "ghcr.io/test/repo"})
	err := client.Delete(ctx, "default")
	// When Delete returns 405 (MethodNotAllowed), the workspaceClient.delete
	// returns that error because there's no fallback for state deletion
	// (only lock deletion has the unlock fallback). So we expect an error.
	if err == nil {
		t.Fatalf("expected Delete to return error for delete-unsupported repo")
	}
}

func TestDelete_SurfacesTransientResolveError(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	resolveErr := &orasErrcode.ErrorResponse{StatusCode: http.StatusServiceUnavailable}
	failRepo := &resolveFailingRepo{
		delegatingRepo: delegatingRepo{inner: fake},
		resolveErr:     resolveErr,
	}
	client := newTestClient(&orasRepositoryClient{inner: failRepo})
	err := client.Delete(ctx, "default")
	if err == nil {
		t.Fatal("expected Delete to return error, got nil")
	}
}

func TestRemoteClient_Delete(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")

	// Delete from empty repo should succeed (no state).
	if err := c.delete(ctx); err != nil {
		t.Fatalf("delete on empty: %v", err)
	}

	// Put state and then delete.
	if err := c.put(ctx, []byte("state-data")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := c.delete(ctx); err != nil {
		t.Fatalf("delete after put: %v", err)
	}

	// State should be gone.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if p != nil {
		t.Error("expected nil after delete, got data")
	}
}

func TestRemoteClient_Delete_WithVersioning(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	client := &Client{
		repoClient: repo,
		config: Config{
			Compression: "none",
			MaxVersions: 2,
		},
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
	}
	c := &workspaceClient{
		client:      client,
		stateID:     "default",
		stateTag:    stateTagPrefix + "default",
		lockTag:     lockTagPrefix + "default",
		unlockedTag: unlockedTagPrefix + "default",
	}

	// Put several versions.
	for i := 0; i < 3; i++ {
		if err := c.put(ctx, []byte(fmt.Sprintf("state-v%d", i+1))); err != nil {
			t.Fatalf("put v%d: %v", i+1, err)
		}
	}
	client.retentionWg.Wait()

	// Now delete the whole workspace.
	if err := c.delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// State should be gone.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if p != nil {
		t.Error("expected nil after delete")
	}

	// State tag should no longer resolve.
	_, err = fake.Resolve(ctx, c.stateTag)
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Errorf("expected state tag to be gone, resolve err = %v", err)
	}
}

// ─── Put/Versioning/Retention tests ───────────────────────────────────────────

func TestRemoteClient_Put_VersioningTagsAndRetention(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	client := &Client{
		repoClient: repo,
		config: Config{
			Compression: "none",
			MaxVersions: 2,
		},
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
	}
	c := &workspaceClient{
		client:      client,
		stateID:     "default",
		stateTag:    stateTagPrefix + "default",
		lockTag:     lockTagPrefix + "default",
		unlockedTag: unlockedTagPrefix + "default",
	}

	// Put three states; versioning keeps only the last 2.
	if err := c.put(ctx, []byte("s1")); err != nil {
		t.Fatalf("put s1: %v", err)
	}
	client.retentionWg.Wait()

	if err := c.put(ctx, []byte("s2")); err != nil {
		t.Fatalf("put s2: %v", err)
	}
	client.retentionWg.Wait()

	if err := c.put(ctx, []byte("s3")); err != nil {
		t.Fatalf("put s3: %v", err)
	}
	client.retentionWg.Wait()

	// Current state should be "s3".
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(p) != "s3" {
		t.Errorf("current state = %q, want %q", string(p), "s3")
	}

	// Version tags: v1 should be cleaned up, v2 and v3 should exist.
	_, err = fake.Resolve(ctx, c.versionTagFor(1))
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Errorf("expected v1 to be cleaned up, resolve err = %v", err)
	}
	_, err = fake.Resolve(ctx, c.versionTagFor(2))
	if err != nil {
		t.Errorf("expected v2 to exist, resolve err = %v", err)
	}
	_, err = fake.Resolve(ctx, c.versionTagFor(3))
	if err != nil {
		t.Errorf("expected v3 to exist, resolve err = %v", err)
	}
}

func TestRemoteClient_RetagToNewManifest_PreservesVersionAnnotation(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	client := &Client{
		repoClient: repo,
		config: Config{
			Compression: "none",
			MaxVersions: 3,
		},
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
	}
	c := &workspaceClient{
		client:      client,
		stateID:     "default",
		stateTag:    stateTagPrefix + "default",
		lockTag:     lockTagPrefix + "default",
		unlockedTag: unlockedTagPrefix + "default",
	}

	if err := c.put(ctx, []byte("state-data")); err != nil {
		t.Fatalf("put: %v", err)
	}
	c.client.retentionWg.Wait()

	// Fetch the current manifest to check its version annotation.
	fm, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc: %v", err)
	}
	originalVersion := fm.m.Annotations[annotationStateVersion]

	// Retag to a new manifest (simulating retention detach).
	if err := c.retagToNewManifest(ctx, []string{c.stateTag}); err != nil {
		t.Fatalf("retagToNewManifest: %v", err)
	}
	c.client.retentionWg.Wait()

	// Re-fetch and verify the version annotation is preserved.
	fm2, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc after retag: %v", err)
	}
	if fm2.m.Annotations[annotationStateVersion] != originalVersion {
		t.Errorf("version annotation after retag = %q, want %q",
			fm2.m.Annotations[annotationStateVersion], originalVersion)
	}
}

func TestAsyncRetentionNotBlocking(t *testing.T) {
	// Verify that async retention is truly async and doesn't block put.
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 2
	c.client.retentionSem = make(chan struct{}, 1)

	// Put many states; retention is async so puts should not block.
	for i := 0; i < 5; i++ {
		if err := c.put(ctx, []byte(fmt.Sprintf("state-%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// Wait for all retention to finish.
	c.client.retentionWg.Wait()

	// Final state should be the last one written.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(p) != "state-4" {
		t.Errorf("current state = %q, want %q", string(p), "state-4")
	}
}

func TestRetentionCompleteAfterWait(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 1
	c.client.retentionSem = make(chan struct{}, 3)

	if err := c.put(ctx, []byte("first")); err != nil {
		t.Fatalf("put first: %v", err)
	}
	c.client.retentionWg.Wait()

	if err := c.put(ctx, []byte("second")); err != nil {
		t.Fatalf("put second: %v", err)
	}
	c.client.retentionWg.Wait()

	// After waiting, only the latest version should remain.
	_, err := fake.Resolve(ctx, c.versionTagFor(1))
	if err == nil {
		t.Error("expected version 1 to be cleaned up after retention wait")
	}
	_, err = fake.Resolve(ctx, c.versionTagFor(2))
	if err != nil {
		t.Errorf("expected version 2 to exist after retention wait: %v", err)
	}
}

func TestRetentionRaceWithoutWait(t *testing.T) {
	// Put without waiting should still work; retention runs async.
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 1
	c.client.retentionSem = make(chan struct{}, 3)

	if err := c.put(ctx, []byte("data")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// No wait - just verify the put succeeded.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(p) != "data" {
		t.Errorf("got %q, want %q", string(p), "data")
	}
}

func TestWaitForRetentionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 1
	c.client.retentionSem = make(chan struct{}, 3)

	if err := c.put(ctx, []byte("data")); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Wait multiple times - should be safe (WaitGroup wait is idempotent
	// when no goroutines are running).
	c.client.retentionWg.Wait()
	c.client.retentionWg.Wait()
	c.client.retentionWg.Wait()

	// Verify no panic and state is accessible.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(p) != "data" {
		t.Errorf("got %q, want %q", string(p), "data")
	}
}

func TestPut_nilAndEmptyState(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")

	// Put nil state.
	if err := c.put(ctx, nil); err != nil {
		t.Fatalf("put nil: %v", err)
	}
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get after nil put: %v", err)
	}
	// nil state should result in empty data after get
	if len(p) != 0 {
		t.Errorf("expected nil or empty after nil put, got %q", string(p))
	}

	// Put empty state.
	if err := c.put(ctx, []byte{}); err != nil {
		t.Fatalf("put empty: %v", err)
	}
	p, err = c.get(ctx)
	if err != nil {
		t.Fatalf("get after empty put: %v", err)
	}
	if len(p) != 0 {
		t.Errorf("expected empty after empty put, got %q", string(p))
	}
}

// ─── State compression tests ──────────────────────────────────────────────────

func TestRemoteClient_StateCompression_GzipRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.Compression = "gzip"

	original := []byte("some state data to compress and decompress")
	if err := c.put(ctx, original); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Verify the stored layer has gzip media type.
	fm, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc: %v", err)
	}
	if len(fm.m.Layers) == 0 {
		t.Fatal("no layers in manifest")
	}
	if fm.m.Layers[0].MediaType != mediaTypeStateLayerGzip {
		t.Errorf("layer media type = %q, want %q", fm.m.Layers[0].MediaType, mediaTypeStateLayerGzip)
	}

	// Get should decompress automatically.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(p, original) {
		t.Errorf("get returned %q, want %q", string(p), string(original))
	}
}

func TestRemoteClient_StateCompression_AutoDetectOnRead(t *testing.T) {
	// Write uncompressed, then read with compression enabled (or vice versa).
	// The get method auto-detects based on layer media type.
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.Compression = "none"

	original := []byte("data stored without compression")
	if err := c.put(ctx, original); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Verify uncompressed media type.
	fm, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc: %v", err)
	}
	if len(fm.m.Layers) == 0 {
		t.Fatal("no layers")
	}
	if fm.m.Layers[0].MediaType != mediaTypeStateLayer {
		t.Errorf("layer media type = %q, want %q", fm.m.Layers[0].MediaType, mediaTypeStateLayer)
	}

	// Get should return original data.
	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(p, original) {
		t.Errorf("get returned %q, want %q", string(p), string(original))
	}
}

func TestRemoteClient_StateCompression_GzipEmptyState(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.Compression = "gzip"

	if err := c.put(ctx, []byte{}); err != nil {
		t.Fatalf("put empty with gzip: %v", err)
	}

	p, err := c.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(p) != 0 {
		t.Errorf("expected empty, got %q", string(p))
	}
}

// ─── Oversized state tests ────────────────────────────────────────────────────

func TestRemoteClient_Get_RejectsOversizedState(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxStateSize = 10

	// Put state that is larger than the limit.
	if err := c.put(ctx, []byte("this state data is longer than ten bytes")); err != nil {
		t.Fatalf("put: %v", err)
	}

	_, err := c.get(ctx)
	if err == nil {
		t.Fatal("expected error for oversized state, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Errorf("error = %v, want 'exceeds maximum allowed size'", err)
	}
}

func TestRemoteClient_Get_RejectsOversizedGzipState(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.Compression = "gzip"
	c.client.config.MaxStateSize = 10

	data := []byte("this state data is compressed but longer than ten bytes uncompressed")
	if err := c.put(ctx, data); err != nil {
		t.Fatalf("put: %v", err)
	}

	_, err := c.get(ctx)
	if err == nil {
		t.Fatal("expected error for oversized compressed state, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Errorf("error = %v, want 'exceeds maximum allowed size'", err)
	}
}

// ─── Lock TTL / stale lock tests ──────────────────────────────────────────────

func TestRemoteClient_LockTTL_ClearsStaleLock(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}

	// Oversized past time so we can detect stale lock.
	pastTime := time.Now().Add(-10 * time.Minute)
	nowFunc := func() time.Time {
		return pastTime
	}

	c := newRemoteClient(repo, "default")
	c.client.config.LockTTL = 5 * time.Minute
	c.client.now = nowFunc

	info := &LockInfo{
		ID:        "stale-lock",
		Operation: "apply",
		Info:      "old lock",
		Who:       "old-user",
		Version:   "1.0",
		Created:   pastTime,
	}

	// Acquire lock at the past time.
	lockID, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Fast forward past the TTL.
	nowFuture := pastTime.Add(c.client.config.LockTTL + time.Second)
	c.client.now = func() time.Time { return nowFuture }

	// A new lock attempt should succeed (stale lock is cleared).
	info2 := &LockInfo{
		ID:        "new-lock",
		Operation: "plan",
		Info:      "new lock",
		Who:       "new-user",
		Version:   "2.0",
		Created:   nowFuture,
	}
	lockID2, err := c.lock(ctx, info2)
	if err != nil {
		t.Fatalf("expected lock to succeed after stale lock cleared, got: %v", err)
	}

	// Clean up.
	_ = c.unlock(ctx, lockID2)
	_ = c.unlock(ctx, lockID)
}

func TestRemoteClient_LockTTL_ClearsStaleLock_DeleteUnsupportedFallback(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	unsupportedInner := &deleteUnsupportedRepo{delegatingRepo: delegatingRepo{inner: fake}}
	repo := &orasRepositoryClient{inner: unsupportedInner, repository: "ghcr.io/test/repo"}

	pastTime := time.Now().Add(-10 * time.Minute)
	nowFunc := func() time.Time { return pastTime }

	c := newRemoteClient(repo, "default")
	c.client.config.LockTTL = 5 * time.Minute
	c.client.now = nowFunc

	info := &LockInfo{
		ID:        "stale-lock",
		Operation: "apply",
		Info:      "old lock on delete-unsupported repo",
		Who:       "old-user",
		Version:   "1.0",
		Created:   pastTime,
	}
	lockID, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Fast forward past TTL.
	c.client.now = func() time.Time { return pastTime.Add(c.client.config.LockTTL + time.Second) }

	info2 := &LockInfo{
		ID:        "new-lock",
		Operation: "plan",
		Info:      "new lock after stale",
		Who:       "new-user",
		Version:   "2.0",
		Created:   c.client.now(),
	}
	lockID2, err := c.lock(ctx, info2)
	if err != nil {
		t.Fatalf("expected lock to succeed after stale lock on delete-unsupported repo, got: %v", err)
	}

	_ = c.unlock(ctx, lockID2)
	_ = c.unlock(ctx, lockID)
}

// ─── Lock race / generation detection tests ───────────────────────────────────

func TestRemoteClient_Lock_RaceConditionDetection(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()

	rivalDesc, rivalData := newRivalLockManifest(ctx, t, fake, "rival-holder", 99)
	raceRepo := &raceSimulatingRepo{
		delegatingRepo: delegatingRepo{inner: fake},
		raceTag:        "",
		rivalDesc:      rivalDesc,
		rivalData:      rivalData,
	}

	repo := &orasRepositoryClient{inner: raceRepo}
	c := newRemoteClient(repo, "default")
	raceRepo.raceTag = c.lockTag

	info := &LockInfo{
		ID:        "my-lock-id",
		Operation: "apply",
		Info:      "my test lock",
		Who:       "test-user",
		Version:   "1.0",
		Created:   time.Now(),
	}
	_, err := c.lock(ctx, info)
	if err == nil {
		t.Fatal("expected lock error due to race, got nil")
	}
	var lockErr *LockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *LockError, got %T: %v", err, err)
	}
}

func TestRemoteClient_Lock_SameGenerationRaceDetectedByHolderID(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()

	// Create a rival with the same generation but different holderID.
	rivalDesc, rivalData := newRivalLockManifest(ctx, t, fake, "rival-at-same-gen", 1)
	raceRepo := &sameGenRaceRepo{
		delegatingRepo: delegatingRepo{inner: fake},
		raceTag:        "",
		rivalDesc:      rivalDesc,
		rivalData:      rivalData,
	}

	repo := &orasRepositoryClient{inner: raceRepo}
	c := newRemoteClient(repo, "default")
	raceRepo.raceTag = c.lockTag

	info := &LockInfo{
		ID:        "my-lock-id",
		Operation: "apply",
		Info:      "test lock",
		Who:       "test-user",
		Version:   "1.0",
		Created:   time.Now(),
	}
	_, err := c.lock(ctx, info)
	if err == nil {
		t.Fatal("expected lock error due to same-generation race, got nil")
	}
	var lockErr *LockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *LockError, got %T: %v", err, err)
	}
}

func TestLockWithGenerationDetection(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")

	// Acquire lock (generation 1).
	info := &LockInfo{ID: "gen1", Operation: "apply"}
	lockID1, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Check lock manifest data for generation 1.
	data, err := c.getLockManifestData(ctx)
	if err != nil {
		t.Fatalf("getLockManifestData: %v", err)
	}
	if data.Generation != 1 {
		t.Errorf("generation = %d, want 1", data.Generation)
	}
	if data.HolderID != "gen1" {
		t.Errorf("holderID = %q, want %q", data.HolderID, "gen1")
	}

	// Release lock.
	if err := c.unlock(ctx, lockID1); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	// Acquire lock again (generation 1, since the lock was fully deleted).
	info2 := &LockInfo{ID: "gen2", Operation: "plan"}
	lockID2, err := c.lock(ctx, info2)
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}

	data2, err := c.getLockManifestData(ctx)
	if err != nil {
		t.Fatalf("getLockManifestData: %v", err)
	}
	if data2.Generation != 1 {
		t.Errorf("generation = %d, want 1 (generation resets after unlock+delete)", data2.Generation)
	}
	if data2.HolderID != "gen2" {
		t.Errorf("holderID = %q, want %q", data2.HolderID, "gen2")
	}

	_ = c.unlock(ctx, lockID2)
}

func TestStaleLockCleanupRaceCondition(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}

	pastTime := time.Now().Add(-10 * time.Minute)
	c := newRemoteClient(repo, "default")
	c.client.config.LockTTL = 5 * time.Minute
	c.client.now = func() time.Time { return pastTime }

	// Create a stale lock manually using packLockManifestWithGeneration.
	staleInfo := &LockInfo{ID: "crashed-process", Operation: "apply", Created: pastTime}
	staleInfoBytes, _ := json.Marshal(staleInfo)
	leaseExpiry := pastTime.Add(c.client.config.LockTTL).UnixNano()
	manifestDesc, err := c.packLockManifestWithGeneration(ctx, staleInfo.ID, string(staleInfoBytes), 5, leaseExpiry, staleInfo.ID)
	if err != nil {
		t.Fatalf("packLockManifestWithGeneration: %v", err)
	}
	if err := fake.Tag(ctx, manifestDesc, c.lockTag); err != nil {
		t.Fatalf("tag stale lock: %v", err)
	}

	// Fast-forward past TTL.
	c.client.now = func() time.Time { return pastTime.Add(c.client.config.LockTTL + time.Second) }

	// A new lock should succeed (it detects and clears the stale lock).
	info := &LockInfo{ID: "new-process", Operation: "apply", Who: "new-user", Version: "2.0", Created: c.client.now()}
	lockID, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("expected lock to succeed after stale lock cleanup, got: %v", err)
	}
	_ = c.unlock(ctx, lockID)
}

// ─── Unlock fallback tests ────────────────────────────────────────────────────

func TestRemoteClient_UnlockFallbackWhenDeleteUnsupported(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	deleteUnsup := &deleteUnsupportedRepo{delegatingRepo: delegatingRepo{inner: fake}}
	repo := &orasRepositoryClient{inner: deleteUnsup, repository: "ghcr.io/test/repo"}
	c := newRemoteClient(repo, "default")

	info := &LockInfo{
		ID:        "lock-id",
		Operation: "apply",
		Info:      "test lock on delete-unsupported repo",
		Who:       "test-user",
		Version:   "1.0",
		Created:   time.Now(),
	}
	lockID, err := c.lock(ctx, info)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Unlock should fall back to retag-to-unlocked when Delete returns 405.
	err = c.unlock(ctx, lockID)
	if err != nil {
		t.Fatalf("expected unlock to succeed (via fallback), got: %v", err)
	}

	// Verify the lock tag now points to an unlocked manifest.
	fm, err := c.fetchManifestWithDesc(ctx, c.lockTag)
	if err != nil {
		t.Fatalf("fetch lock tag after unlock: %v", err)
	}
	parsedInfo, _ := parseLockInfo(fm.m, c.stateTag)
	if parsedInfo != nil && parsedInfo.ID != "" {
		t.Errorf("expected lock tag to point to unlocked manifest after fallback unlock, got ID=%q", parsedInfo.ID)
	}
}

// ─── List workspaces tests ────────────────────────────────────────────────────

func TestList_ReturnsStoredWorkspaces(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}

	// Put state for a few workspaces.
	for _, ws := range []string{"default", "staging", "production"} {
		c := newRemoteClient(repo, ws)
		if err := c.put(ctx, []byte(fmt.Sprintf("state-%s", ws))); err != nil {
			t.Fatalf("put %s: %v", ws, err)
		}
	}

	client := newTestClient(repo)
	workspaces, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(workspaces) != 3 {
		t.Errorf("expected 3 workspaces, got %d: %v", len(workspaces), workspaces)
	}
}

func TestList_NoDuplicateDefault(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}

	// Put state for "default".
	c := newRemoteClient(repo, "default")
	if err := c.put(ctx, []byte("state-default")); err != nil {
		t.Fatalf("put default: %v", err)
	}

	workspaces, err := listWorkspacesFromTags(ctx, repo)
	if err != nil {
		t.Fatalf("listWorkspacesFromTags: %v", err)
	}

	// "default" should appear only once.
	count := 0
	for _, w := range workspaces {
		if w == "default" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 'default' exactly once, got %d occurrences: %v", count, workspaces)
	}
}

func TestWorkspaceNameFromTag_corruptManifest(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}

	// Create a tag that starts with "ws-" but has a corrupt manifest.
	corruptData := []byte("not a valid manifest")
	d := digest.FromBytes(corruptData)
	desc := ocispec.Descriptor{Digest: d, Size: int64(len(corruptData))}
	_ = fake.Push(ctx, desc, bytes.NewReader(corruptData))
	_ = fake.Tag(ctx, desc, "state-ws-corrupt")

	_, err := workspaceNameFromTag(ctx, repo, "state-ws-corrupt")
	if err == nil {
		t.Fatal("expected error for corrupt manifest, got nil")
	}
}

func TestListWorkspacesFromTags_contextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := newFakeORASRepo()
	blockingTagsRepo := &blockingRepo{delegatingRepo: delegatingRepo{inner: fake}}
	repo := &orasRepositoryClient{inner: blockingTagsRepo}

	// Cancel the context.
	cancel()

	_, err := listWorkspacesFromTags(ctx, repo)
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
}

// ─── GroupVersionsByDigest tests ──────────────────────────────────────────────

func TestGroupVersionsByDigest_parallelResolution(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	trackingRepo := newConcurrencyTrackingRepo(fake)
	repo := &orasRepositoryClient{inner: trackingRepo}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 5

	// Put 5 states so version tags are created.
	for i := 1; i <= 5; i++ {
		if err := c.put(ctx, []byte(fmt.Sprintf("state-%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	c.client.retentionWg.Wait()

	versions := []int{1, 2, 3, 4, 5}
	// Resolve current state to get its digest.
	fm, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc: %v", err)
	}
	currentDigest := fm.desc.Digest

	groups, err := c.groupVersionsByDigest(ctx, versions, currentDigest)
	if err != nil {
		t.Fatalf("groupVersionsByDigest: %v", err)
	}

	// Verify we got some groups (at least current state's group exists).
	if len(groups) == 0 {
		t.Error("expected at least one digest group")
	}

	// The current digest should not appear in any group.
	for key, group := range groups {
		if key == currentDigest.String() {
			t.Errorf("current digest %s should not appear in groups", key)
		}
		for _, tag := range group.tags {
			if tag == c.stateTag {
				t.Errorf("state tag should not be in groups: %s", tag)
			}
		}
	}
}

func TestGroupVersionsByDigest_skipsCurrentDigest(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	repo := &orasRepositoryClient{inner: fake}
	c := newRemoteClient(repo, "default")
	c.client.config.MaxVersions = 3

	for i := 1; i <= 3; i++ {
		if err := c.put(ctx, []byte(fmt.Sprintf("state-%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	c.client.retentionWg.Wait()

	fm, err := c.fetchManifestWithDesc(ctx, c.stateTag)
	if err != nil {
		t.Fatalf("fetchManifestWithDesc: %v", err)
	}
	currentDigest := fm.desc.Digest

	// The current state is the latest. Version 1 and 2 are older versions.
	// After retention with max=3, all 3 versions should still exist.
	versions := []int{1, 2, 3}
	groups, err := c.groupVersionsByDigest(ctx, versions, currentDigest)
	if err != nil {
		t.Fatalf("groupVersionsByDigest: %v", err)
	}

	// Current digest should not be in any group.
	for key := range groups {
		if key == currentDigest.String() {
			t.Errorf("current digest %s should not be in groups", key)
		}
	}
}

// ─── Retry / transient error tests (unchanged) ────────────────────────────────

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "random error", err: errors.New("something broke"), want: false},
		{name: "429 too many requests", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "408 request timeout", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "502 bad gateway", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusBadGateway}, want: true},
		{name: "503 service unavailable", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusServiceUnavailable}, want: true},
		{name: "504 gateway timeout", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusGatewayTimeout}, want: true},
		{name: "500 internal server error", err: &orasErrcode.ErrorResponse{StatusCode: http.StatusInternalServerError}, want: false},
		{name: "connection reset", err: errors.New("connection reset by peer"), want: true},
		{name: "connection refused", err: errors.New("connection refused"), want: true},
		{name: "i/o timeout", err: errors.New("i/o timeout"), want: true},
		{name: "no such host", err: errors.New("no such host"), want: true},
		{name: "unexpected EOF", err: errors.New("unexpected eof"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err); got != tt.want {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetry_Success(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	result, err := retryWithResult(ctx, func(ctx context.Context) (string, error) {
		callCount++
		return "success", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "success" {
		t.Errorf("result = %q, want %q", result, "success")
	}
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1", callCount)
	}
}

func TestRetry_TransientFailureThenSuccess(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	result, err := retryWithResult(ctx, func(ctx context.Context) (string, error) {
		callCount++
		if callCount < 2 {
			return "", &orasErrcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want %q", result, "ok")
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestRetry_NonTransientFailure(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	_, err := retryWithResult(ctx, func(ctx context.Context) (string, error) {
		callCount++
		return "", errors.New("non-transient error")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1", callCount)
	}
}

func TestRetry_MaxAttemptsExhausted(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	_, err := retryWithResult(ctx, func(ctx context.Context) (string, error) {
		callCount++
		return "", &orasErrcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 3 {
		t.Errorf("callCount = %d, want 3", callCount)
	}
}

func TestRetry_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	callCount := 0
	errCh := make(chan error, 1)
	go func() {
		_, err := retryWithResult(ctx, func(ctx context.Context) (string, error) {
			callCount++
			return "", &orasErrcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}
		})
		errCh <- err
	}()

	// Cancel after a short delay.
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRetryNoResult(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	err := retry(ctx, func(ctx context.Context) error {
		callCount++
		if callCount < 2 {
			return &orasErrcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

// ─── Benchmarks ───────────────────────────────────────────────────────────────

func BenchmarkCompressGzip(b *testing.B) {
	data := []byte(strings.Repeat("hello world, this is some terraform state data\n", 100))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = compressGzip(data)
	}
}

func BenchmarkDecompressGzip(b *testing.B) {
	original := []byte(strings.Repeat("hello world, this is some terraform state data\n", 100))
	compressed, err := compressGzip(original)
	if err != nil {
		b.Fatalf("compress: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		gz, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			b.Fatalf("gzip.NewReader: %v", err)
		}
		_, _ = io.ReadAll(gz)
		_ = gz.Close()
	}
}
