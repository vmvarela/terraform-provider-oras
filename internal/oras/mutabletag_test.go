// Tests for publishMutableTag/observeMutableTag: OCI tags are
// last-writer-wins with no CAS, so a transient (lost/failed) response on a
// mutable-tag publication must never be blindly re-applied. These tests drive
// the helper directly through the fakeORASRepo seam with a hooking wrapper —
// deterministic counts/signals only, no sleeps to create races (the only
// waits are the helper's own bounded observation backoff, tolerated like
// lockStabilityDelay).
package oras

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	orasErrcode "oras.land/oras-go/v2/registry/remote/errcode"
)

// testObservationTimeout bounds waiting for the helper's first observation
// signal (generous, like testWaitTimeout in the statestore suite).
const testObservationTimeout = 30 * time.Second

// hookingRepo wraps an orasRepository with count-gated failure hooks for a
// watched reference, simulating response-lost publications (tag lands, then
// the response fails), absent tags, and ambiguous Resolve results.
type hookingRepo struct {
	inner orasRepository

	// watch is the only reference the hooks apply to.
	watch string
	// failTagCount: the first N Tag calls for watch fail transiently.
	failTagCount int
	// landOnFailure: a failing Tag first applies to the inner repo, then
	// returns the transient error (response-lost simulation).
	landOnFailure bool
	// failResolveCount: the first N Resolve calls for watch fail transiently.
	// A large value means "never resolvable".
	failResolveCount int
	// nonTransientTagErr, if set, is returned by the first failing Tag call
	// instead of a transient 500 (used to assert non-transient surfacing).
	nonTransientTagErr error

	// firstResolve is closed when the first watched Resolve is issued.
	firstResolve chan struct{}

	mu           sync.Mutex
	tagCalls     int
	resolveCalls int
}

func (h *hookingRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	return h.inner.Push(ctx, expected, content)
}

func (h *hookingRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	return h.inner.Fetch(ctx, target)
}

func (h *hookingRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	h.mu.Lock()
	matched := reference == h.watch
	if matched {
		h.resolveCalls++
		fail := h.resolveCalls <= h.failResolveCount
		h.mu.Unlock()
		if h.resolveCalls == 1 && h.firstResolve != nil {
			select {
			case <-h.firstResolve:
			default:
				close(h.firstResolve)
			}
		}
		if fail {
			return ocispec.Descriptor{}, transientHTTPStatus(http.StatusInternalServerError)
		}
	} else {
		h.mu.Unlock()
	}
	return h.inner.Resolve(ctx, reference)
}

func (h *hookingRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	h.mu.Lock()
	matched := reference == h.watch
	if matched {
		h.tagCalls++
	}
	fail := matched && h.tagCalls <= h.failTagCount
	land := fail && h.landOnFailure
	nonTransientErr := h.nonTransientTagErr
	h.mu.Unlock()

	if land {
		if err := h.inner.Tag(ctx, desc, reference); err != nil {
			return err
		}
	}
	if fail {
		if nonTransientErr != nil {
			return nonTransientErr
		}
		return transientHTTPStatus(http.StatusInternalServerError)
	}
	return h.inner.Tag(ctx, desc, reference)
}

func (h *hookingRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return h.inner.Delete(ctx, target)
}

func (h *hookingRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	return h.inner.Tags(ctx, last, fn)
}

// tagCallCount reports how many Tag calls hit the watched reference.
func (h *hookingRepo) tagCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tagCalls
}

// transientHTTPStatus builds the errcode error the client classifies as
// transient for the given HTTP status.
func transientHTTPStatus(status int) error {
	return &orasErrcode.ErrorResponse{StatusCode: status}
}

// testDesc builds a stable manifest descriptor for the given payload bytes.
func testDesc(payload string) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromBytes([]byte(payload)),
		Size:      int64(len(payload)),
	}
}

// newHookedWorkspace builds a workspaceClient whose inner repo is the hooked
// wrapper around a fresh fakeORASRepo.
func newHookedWorkspace(t *testing.T, h *hookingRepo) *workspaceClient {
	t.Helper()
	repo := &orasRepositoryClient{inner: h, repository: "example.com/test/repo"}
	return newRemoteClient(repo, "default")
}

// TestPublishMutableTagResponseLostOwnDigest — a transient Tag response on a
// publication that DID land must be treated as success, with NO second
// mutable Tag: the helper observes the tag, sees its own digest, and returns
// nil. Asserting the exact call count proves the observation path, not a
// blind retry.
func TestPublishMutableTagResponseLostOwnDigest(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	hook := &hookingRepo{
		inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"),
		failTagCount: 1, landOnFailure: true, firstResolve: make(chan struct{}),
	}
	wc := newHookedWorkspace(t, hook)

	if err := wc.publishMutableTag(ctx, desc, wc.stateTag); err != nil {
		t.Fatalf("response-lost publication not confirmed as success: %v", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (observation must not re-tag)", calls)
	}
	got, err := hook.inner.Resolve(ctx, wc.stateTag)
	if err != nil || got.Digest != desc.Digest {
		t.Errorf("tag = (%v, %v), want own digest %s", got.Digest, err, desc.Digest)
	}
}

// TestPublishMutableTagForeignDigestFailClosed — after a transient Tag
// failure, if the tag resolves to a FOREIGN digest, the publication must fail
// closed with ErrMutableTagMoved and must NOT re-tag (no clobbering of the
// foreign holder).
func TestPublishMutableTagForeignDigestFailClosed(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	foreign := testDesc("foreign-holder-manifest")
	hook := &hookingRepo{inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"), failTagCount: 1, firstResolve: make(chan struct{})}
	// A rival published between our pack and our (failed) tag response.
	if err := hook.inner.Tag(ctx, foreign, stateTagPrefix+workspaceTagFor("default")); err != nil {
		t.Fatalf("seed foreign tag: %v", err)
	}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved)", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (foreign tag must not be re-tagged)", calls)
	}
	got, resolveErr := hook.inner.Resolve(ctx, stateTagPrefix+workspaceTagFor("default"))
	if resolveErr != nil || got.Digest != foreign.Digest {
		t.Errorf("tag digest = %v (%v), want foreign %s preserved", got.Digest, resolveErr, foreign.Digest)
	}
}

// TestPublishMutableTagAmbiguousResolveFailClosed — if the tag cannot be
// resolved (transient resolve errors across the whole observation bound), the
// publication must fail closed with ErrMutableTagMoved and must NOT re-tag.
// Waits on the helper's own bounded backoff (~1.5s worst case).
func TestPublishMutableTagAmbiguousResolveFailClosed(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	// failResolveCount large: every observation fails transiently.
	hook := &hookingRepo{
		inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"),
		failTagCount: 1, failResolveCount: 99, firstResolve: make(chan struct{}),
	}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved)", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (ambiguous state must fail closed, not re-apply)", calls)
	}
}

// TestPublishMutableTagContextCancelledFailClosed — cancellation during the
// observation must fail closed promptly, PRESERVING cancellation identity
// (errors.Is(context.Canceled)) while retaining the moved/ambiguous
// classification (errors.Is(ErrMutableTagMoved)). Never re-tag after giving
// up observing. The cancel is armed on the helper's first observation signal,
// not on a sleep.
func TestPublishMutableTagContextCancelledFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	desc := testDesc("attempt-manifest")
	hook := &hookingRepo{
		inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"),
		failTagCount: 1, failResolveCount: 99, firstResolve: make(chan struct{}),
	}
	wc := newHookedWorkspace(t, hook)

	go func() {
		select {
		case <-hook.firstResolve:
			cancel()
		case <-time.After(testObservationTimeout):
		}
	}()

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(err, context.Canceled) preserved", err)
	}
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved) classification retained", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1", calls)
	}
}

// TestPublishMutableTagDeadlineExceededFailClosed — deadline expiry during
// the bounded observation must fail closed with context.DeadlineExceeded
// identity preserved (plus the moved/ambiguous classification).
func TestPublishMutableTagDeadlineExceededFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	desc := testDesc("attempt-manifest")
	// Ambiguous: every observation fails transiently, so the helper enters
	// its bounded backoff, where the 100ms deadline fires.
	hook := &hookingRepo{
		inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"),
		failTagCount: 1, failResolveCount: 99, firstResolve: make(chan struct{}),
	}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want errors.Is(err, context.DeadlineExceeded) preserved", err)
	}
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved) classification retained", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1", calls)
	}
}

// TestPublishMutableTagAbsentTagFailClosedNoReapply — tag ABSENT after a
// transient failure: the lost response did not publish, but the publication
// must fail closed with ErrMutableTagMoved and exactly ONE Tag call. A
// Resolve(404)→Tag re-apply is itself a clobber window (a rival can publish
// between the 404 and a re-Tag), so it is never done.
func TestPublishMutableTagAbsentTagFailClosedNoReapply(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	// failTagCount=1 without landing: the Tag failed and nothing was published.
	hook := &hookingRepo{inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"), failTagCount: 1, firstResolve: make(chan struct{})}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved) for an absent tag", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (no re-apply after a 404 observation)", calls)
	}
	if _, resolveErr := hook.inner.Resolve(ctx, wc.stateTag); !errors.Is(resolveErr, errdef.ErrNotFound) {
		t.Errorf("tag must remain absent after fail-closed publish: got resolve error %v", resolveErr)
	}
}

// TestPublishMutableTagRivalDuringObservationNoReapply — deterministic
// 404→rival→no-reapply: the first observation of the absent tag is parked on
// a gate; a rival installs its manifest while the observation is parked; the
// observation then sees a FOREIGN digest and the publication fails closed —
// the helper never re-applies over the rival (exactly one Tag call).
func TestPublishMutableTagRivalDuringObservationNoReapply(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	foreign := testDesc("rival-late-manifest")
	// failResolveCount=1: the FIRST observation blocks on a gate and then
	// resolves (after the rival installs), proving no re-apply happens
	// regardless of when the rival lands.
	fake := newFakeORASRepo()
	blocked := &gateResolveRepo{
		inner:         fake,
		watch:         stateTagPrefix + workspaceTagFor("default"),
		release:       make(chan struct{}),
		firstObserved: make(chan struct{}),
	}
	hook := &hookingRepo{inner: blocked, watch: stateTagPrefix + workspaceTagFor("default"), failTagCount: 1, firstResolve: make(chan struct{})}
	wc := newHookedWorkspace(t, hook)

	errCh := make(chan error, 1)
	go func() { errCh <- wc.publishMutableTag(ctx, desc, wc.stateTag) }()

	select {
	case <-blocked.firstObserved:
	case <-time.After(testObservationTimeout):
		t.Fatal("timed out waiting for the parked observation")
	}
	// A rival publishes while the helper is parked mid-observation.
	if err := blocked.inner.Tag(ctx, foreign, stateTagPrefix+workspaceTagFor("default")); err != nil {
		t.Fatalf("rival install: %v", err)
	}
	close(blocked.release)

	err := <-errCh
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved)", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (no re-apply over the rival)", calls)
	}
	got, resolveErr := hook.inner.Resolve(ctx, stateTagPrefix+workspaceTagFor("default"))
	if resolveErr != nil || got.Digest != foreign.Digest {
		t.Errorf("tag digest = %v (%v), want rival %s preserved", got.Digest, resolveErr, foreign.Digest)
	}
}

// gateResolveRepo blocks Resolve calls for the watched reference from
// blockFrom (1-indexed) onward on a release channel, giving the test a
// deterministic window to install a rival publication mid-observation.
type gateResolveRepo struct {
	inner         orasRepository
	watch         string
	blockFrom     int
	release       chan struct{}
	firstObserved chan struct{}

	mu           sync.Mutex
	resolveCalls int
	once         sync.Once
}

func (g *gateResolveRepo) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	return g.inner.Push(ctx, expected, content)
}

func (g *gateResolveRepo) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	return g.inner.Fetch(ctx, target)
}

func (g *gateResolveRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	if reference == g.watch {
		g.mu.Lock()
		g.resolveCalls++
		block := g.resolveCalls >= g.blockFrom
		g.mu.Unlock()
		if block {
			g.once.Do(func() { close(g.firstObserved) })
			select {
			case <-g.release:
			case <-ctx.Done():
				return ocispec.Descriptor{}, ctx.Err()
			}
		}
	}
	return g.inner.Resolve(ctx, reference)
}

func (g *gateResolveRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return g.inner.Tag(ctx, desc, reference)
}

func (g *gateResolveRepo) Delete(ctx context.Context, target ocispec.Descriptor) error {
	return g.inner.Delete(ctx, target)
}

func (g *gateResolveRepo) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	return g.inner.Tags(ctx, last, fn)
}

// TestLockTagMovedMapsToContentionBothSentinels — the full lock() flow: A's
// lock-tag publication fails transiently, and a rival holds the tag by the
// time A observes it. The returned *LockError must carry BOTH classifications
// via errors.Is — errStateLocked (contention semantics, unchanged) and
// ErrMutableTagMoved (moved/ambiguous publication) — plus the rival's holder
// info, and must never re-apply the lock tag.
func TestLockTagMovedMapsToContentionBothSentinels(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	// Block only the SECOND watched resolve (the post-failure observation);
	// the first one is A's pre-tag fetch and must see an empty tag.
	blocked := &gateResolveRepo{
		inner:         fake,
		watch:         lockTagPrefix + workspaceTagFor("default"),
		blockFrom:     2,
		release:       make(chan struct{}),
		firstObserved: make(chan struct{}),
	}
	hook := &hookingRepo{inner: blocked, watch: lockTagPrefix + workspaceTagFor("default"), failTagCount: 1, firstResolve: make(chan struct{})}
	repo := &orasRepositoryClient{inner: hook, repository: "example.com/test/repo"}
	wc := newRemoteClient(repo, "default")

	// Rival lock manifest, packed through the normal path so its body is
	// fetchable, installed directly while A is parked mid-observation.
	setupRepo := &orasRepositoryClient{inner: fake, repository: "example.com/test/repo"}
	rivalWC := newRemoteClient(setupRepo, "default")
	rivalDesc, err := rivalWC.packLockManifest(ctx, `{"ID":"rival"}`, 7, 0, "rival")
	if err != nil {
		t.Fatalf("pack rival lock manifest: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := wc.lock(ctx, &LockInfo{ID: "contender-a", Operation: "apply", Who: "contender"})
		errCh <- err
	}()

	select {
	case <-blocked.firstObserved:
	case <-time.After(testObservationTimeout):
		t.Fatal("timed out waiting for the parked observation")
	}
	if err := fake.Tag(ctx, rivalDesc, lockTagPrefix+workspaceTagFor("default")); err != nil {
		t.Fatalf("rival install: %v", err)
	}
	close(blocked.release)

	err = <-errCh
	if err == nil {
		t.Fatal("lock succeeded over a rival-held tag")
	}
	var lockErr *LockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("error = %T, want *LockError", err)
	}
	if lockErr.Info == nil || lockErr.Info.ID != "rival" {
		t.Errorf("holder info = %+v, want the rival", lockErr.Info)
	}
	if !errors.Is(err, errStateLocked) {
		t.Error("errors.Is(err, errStateLocked) = false; contention semantics must be preserved")
	}
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Error("errors.Is(err, ErrMutableTagMoved) = false; moved classification must be preserved")
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("lock-tag Tag calls = %d, want exactly 1 (no re-apply over the rival)", calls)
	}
}

// TestPublishMutableTagForeignVersionTag — the same fail-closed rule applies
// to version-tag publication: a foreign version tag is never retagged or
// deleted by a retried publication.
func TestPublishMutableTagForeignVersionTag(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	foreign := testDesc("foreign-version-manifest")
	hook := &hookingRepo{inner: newFakeORASRepo(), watch: stateVersionTagPrefix + workspaceTagFor("default") + "-v1", failTagCount: 1, firstResolve: make(chan struct{})}
	if err := hook.inner.Tag(ctx, foreign, stateVersionTagPrefix+workspaceTagFor("default")+"-v1"); err != nil {
		t.Fatalf("seed foreign version tag: %v", err)
	}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, stateVersionTagPrefix+workspaceTagFor("default")+"-v1")
	if !errors.Is(err, ErrMutableTagMoved) {
		t.Fatalf("error = %v, want errors.Is(err, ErrMutableTagMoved)", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (foreign version tag must not be retagged)", calls)
	}
	got, resolveErr := hook.inner.Resolve(ctx, stateVersionTagPrefix+workspaceTagFor("default")+"-v1")
	if resolveErr != nil || got.Digest != foreign.Digest {
		t.Errorf("version tag digest = %v (%v), want foreign %s preserved", got.Digest, resolveErr, foreign.Digest)
	}
}

// TestPublishMutableTagNonTransientSurfaced — a non-transient Tag failure
// (e.g. 401) must be surfaced directly, without observation or re-apply.
func TestPublishMutableTagNonTransientSurfaced(t *testing.T) {
	ctx := context.Background()
	desc := testDesc("attempt-manifest")
	errForbidden := errors.New("forbidden (test sentinel)")
	hook := &hookingRepo{
		inner: newFakeORASRepo(), watch: stateTagPrefix + workspaceTagFor("default"),
		failTagCount: 1, nonTransientTagErr: errForbidden, firstResolve: make(chan struct{}),
	}
	wc := newHookedWorkspace(t, hook)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, errForbidden) {
		t.Fatalf("error = %v, want the non-transient error surfaced directly", err)
	}
	if calls := hook.tagCallCount(); calls != 1 {
		t.Errorf("Tag calls = %d, want exactly 1 (no observation on non-transient failure)", calls)
	}
}
