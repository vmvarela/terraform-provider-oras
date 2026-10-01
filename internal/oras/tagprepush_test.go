// Tests for tagPrePushNotFoundRetry (issue #51): the pinned ORAS
// manifestStore.Tag performs a pre-push Fetch — a plain GET of the manifest
// digest — before the tag PUT, and maps a 404 from that GET to
// errdef.ErrNotFound. Issue #51 observed an intermittent digest-not-found
// during the first GHCR lock acquisition (successful rerun, unchanged
// source); read-after-write lag is the HYPOTHESIS for it, and it was not
// proven that a later PUT would succeed. The helper handles exactly the
// pinned ORAS pre-PUT GET-404 path: it retries ONLY that sentinel, bounded
// and cancellation-aware.
//
// These tests drive the real oras-go remote.Repository over httptest (not a
// fake Tag) so the pre-push GET and the tag PUT are the pinned
// implementation's actual HTTP requests.
//
// ACCEPTED RESIDUAL RACE, deliberately demonstrated by
// TestPublishMutableTagPrePushRetryOverwritesRival: OCI tags have no CAS, so
// between a 404 and the retried PUT a rival can publish its own intent under
// the tag and the retry OVERWRITES it. This retry is not concurrency-safe
// and changes W5 for the pre-PUT 404 case: what previously failed hard can
// now succeed at the cost of a stale-intent overwrite.
package oras

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	orasRemote "oras.land/oras-go/v2/registry/remote"
	orasErrcode "oras.land/oras-go/v2/registry/remote/errcode"
)

// tagRetryServer is a minimal OCI Distribution registry over httptest. It
// records every manifest request and can fail the first N digest GETs with
// 404 (simulating the hypothesized read-after-write lag) and fail tag PUTs
// with an arbitrary status, deterministically.
type tagRetryServer struct {
	t *testing.T

	mu        sync.Mutex
	manifests map[string]seededManifest // digest string → stored bytes
	tags      map[string]string         // tag → digest string
	reqs      []tagRetryReq

	// digest404Remaining: the next N digest GETs are answered 404.
	digest404Remaining int
	// onFirstDigest404, if set, is closed after the first 404 digest GET.
	onFirstDigest404 chan struct{}
	first404Fired    bool
	// gateDigest + holdDigestGET: while holdDigestGET is open, every digest
	// GET for gateDigest blocks — the deterministic gate for a rival to
	// publish between a retry's attempts. onGateDigest, if set, is closed
	// once a gated GET has ARRIVED (i.e., the caller is parked on the gate).
	gateDigest    string
	holdDigestGET chan struct{}
	onGateDigest  chan struct{}
	gateFired     bool
	// putStatus: status served for manifest PUTs (0 = 201 Created).
	putStatus int
}

type seededManifest struct {
	body      []byte
	mediaType string
}

type tagRetryReq struct {
	Method    string
	Reference string
}

func newTagRetryServer(t *testing.T) (*tagRetryServer, *httptest.Server) {
	t.Helper()
	s := &tagRetryServer{
		t:         t,
		manifests: make(map[string]seededManifest),
		tags:      make(map[string]string),
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

// setPrePush404s makes the next n digest GETs answer 404.
func (s *tagRetryServer) setPrePush404s(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digest404Remaining = n
}

// setPutStatus makes manifest PUTs answer with the given status instead of 201.
func (s *tagRetryServer) setPutStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putStatus = status
}

// seedManifest stores the manifest server-side so the pre-push digest GET can
// serve it (mirrors "the manifest was pushed moments earlier").
func (s *tagRetryServer) seedManifest(desc ocispec.Descriptor, payload string) {
	body := []byte(payload)
	if got := digest.FromBytes(body); got != desc.Digest {
		s.t.Fatalf("seed digest mismatch: descriptor %s, payload %s", desc.Digest, got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifests[desc.Digest.String()] = seededManifest{body: body, mediaType: desc.MediaType}
}

// tagDigest returns the digest the tag currently points at.
func (s *tagRetryServer) tagDigest(tag string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags[tag]
}

// requests snapshots the recorded requests.
func (s *tagRetryServer) requests() []tagRetryReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tagRetryReq(nil), s.reqs...)
}

func (s *tagRetryServer) record(method, reference string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, tagRetryReq{Method: method, Reference: reference})
}

func (s *tagRetryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v2/test/repo/manifests/"
	if !strings.HasPrefix(r.URL.Path, prefix) || strings.Contains(strings.TrimPrefix(r.URL.Path, prefix), "/") {
		http.Error(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`, http.StatusNotFound)
		return
	}
	ref := strings.TrimPrefix(r.URL.Path, prefix)
	switch r.Method {
	case http.MethodGet:
		s.serveGetManifest(w, r, ref)
	case http.MethodPut:
		s.servePutManifest(w, r, ref)
	default:
		http.Error(w, `{"errors":[{"code":"UNSUPPORTED","message":"unexpected method"}]}`, http.StatusMethodNotAllowed)
	}
}

func (s *tagRetryServer) serveGetManifest(w http.ResponseWriter, r *http.Request, ref string) {
	s.mu.Lock()
	fail404 := s.digest404Remaining > 0
	if fail404 {
		s.digest404Remaining--
	}
	fire := fail404 && s.onFirstDigest404 != nil && !s.first404Fired
	if fire {
		s.first404Fired = true
	}
	gated := s.gateDigest == ref && s.holdDigestGET != nil
	hold := s.holdDigestGET
	fireGate := gated && !fail404 && s.onGateDigest != nil && !s.gateFired
	if fireGate {
		s.gateFired = true
	}
	s.reqs = append(s.reqs, tagRetryReq{Method: r.Method, Reference: ref})
	s.mu.Unlock()

	if fire {
		close(s.onFirstDigest404)
	}
	if fireGate {
		close(s.onGateDigest)
	}
	if fail404 {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown to registry"}]}`, http.StatusNotFound)
		return
	}
	if gated {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	stored, ok := s.manifests[ref]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown to registry"}]}`, http.StatusNotFound)
		return
	}
	dgst := digest.FromBytes(stored.body)
	w.Header().Set("Content-Type", stored.mediaType)
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(stored.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(stored.body)
}

func (s *tagRetryServer) servePutManifest(w http.ResponseWriter, r *http.Request, ref string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"read failure"}]}`, http.StatusInternalServerError)
		return
	}
	s.record(r.Method, ref)

	s.mu.Lock()
	status := s.putStatus
	s.mu.Unlock()
	if status != 0 && status != http.StatusCreated {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown to registry"}]}`, status)
		return
	}

	dgst := digest.FromBytes(body)
	s.mu.Lock()
	s.manifests[dgst.String()] = seededManifest{body: body, mediaType: r.Header.Get("Content-Type")}
	s.tags[ref] = dgst.String()
	s.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.WriteHeader(http.StatusCreated)
}

// newTagRetryRepo builds the pinned real oras-go remote.Repository pointed at
// srv and returns a workspaceClient whose inner repo is that repository.
func newTagRetryRepo(t *testing.T, srv *httptest.Server) *workspaceClient {
	t.Helper()
	repoRef := strings.TrimPrefix(srv.URL, "http://") + "/test/repo"
	repo, err := orasRemote.NewRepository(repoRef)
	if err != nil {
		t.Fatalf("new remote repository %q: %v", repoRef, err)
	}
	repo.PlainHTTP = true
	return newRemoteClient(&orasRepositoryClient{inner: repo, repository: repoRef}, "default")
}

// countRequests counts recorded requests by method and reference kind
// (digest references start with "sha256:").
func countRequests(reqs []tagRetryReq, method string, digestRef bool) int {
	n := 0
	for _, req := range reqs {
		if req.Method != method {
			continue
		}
		if strings.HasPrefix(req.Reference, "sha256:") == digestRef {
			n++
		}
	}
	return n
}

// TestPublishMutableTagPrePushGet404BeforePut — pinned ORAS manifestStore.Tag
// GETs the manifest digest BEFORE any tag PUT, and after eventual GET success
// the tag PUT happens exactly once.
func TestPublishMutableTagPrePushGet404BeforePut(t *testing.T) {
	ctx := context.Background()
	payload := "pre-push-get404-manifest"
	desc := testDesc(payload)
	s, srv := newTagRetryServer(t)
	s.seedManifest(desc, payload)
	// Inject two pre-push digest GET 404s; the third GET succeeds.
	s.setPrePush404s(2)
	wc := newTagRetryRepo(t, srv)

	if err := wc.publishMutableTag(ctx, desc, wc.stateTag); err != nil {
		t.Fatalf("publish with injected pre-push 404s: %v", err)
	}

	reqs := s.requests()
	if n := countRequests(reqs, http.MethodGet, true); n != 3 {
		t.Errorf("digest GETs = %d, want 3 (bounded retry after pre-push 404)", n)
	}
	if n := countRequests(reqs, http.MethodPut, false); n != 1 {
		t.Errorf("tag PUTs = %d, want exactly 1", n)
	}
	if got, want := reqs[0].Method, http.MethodGet; got != want {
		t.Errorf("first request = %s, want %s (pre-push GET must precede any PUT)", got, want)
	}
	if got := s.tagDigest(wc.stateTag); got != desc.Digest.String() {
		t.Errorf("tag digest = %s, want %s", got, desc.Digest)
	}
}

// TestPublishMutableTagPersistentPrePush404ExhaustedNoPut — when the pre-push
// digest GET keeps returning 404, the bounded retry exhausts and the tag PUT
// is never issued (zero target PUTs).
func TestPublishMutableTagPersistentPrePush404ExhaustedNoPut(t *testing.T) {
	ctx := context.Background()
	payload := "persistent-404-manifest"
	desc := testDesc(payload)
	s, srv := newTagRetryServer(t)
	// The manifest is never stored server-side: every digest GET 404s.
	s.setPrePush404s(99)
	wc := newTagRetryRepo(t, srv)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("error = %v, want errors.Is(err, errdef.ErrNotFound)", err)
	}

	reqs := s.requests()
	if n := countRequests(reqs, http.MethodGet, true); n != tagPrePushNotFoundRetryLimit {
		t.Errorf("digest GETs = %d, want %d (bounded retries, then give up)", n, tagPrePushNotFoundRetryLimit)
	}
	if n := countRequests(reqs, http.MethodPut, false); n != 0 {
		t.Errorf("tag PUTs = %d, want 0 (a pre-push 404 must never reach the PUT)", n)
	}
}

// TestPublishMutableTagPrePushRetryCancelled — cancelling the context between
// attempts stops the retry: no further digest GET beyond the in-flight
// attempt, no tag PUT, and the cancellation identity is preserved.
func TestPublishMutableTagPrePushRetryCancelled(t *testing.T) {
	payload := "cancelled-retry-manifest"
	desc := testDesc(payload)
	s, srv := newTagRetryServer(t)
	s.setPrePush404s(99)
	first404 := make(chan struct{})
	s.onFirstDigest404 = first404
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-first404
		cancel()
	}()
	wc := newTagRetryRepo(t, srv)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(err, context.Canceled)", err)
	}

	reqs := s.requests()
	if n := countRequests(reqs, http.MethodGet, true); n >= tagPrePushNotFoundRetryLimit {
		t.Errorf("digest GETs = %d, want < %d (cancellation must stop the retry)", n, tagPrePushNotFoundRetryLimit)
	}
	if n := countRequests(reqs, http.MethodPut, false); n != 0 {
		t.Errorf("tag PUTs = %d, want 0 after cancellation", n)
	}
}

// TestPublishMutableTagPut404NotRetried — a tag PUT that itself fails with
// HTTP 404 is a remote errcode.ErrorResponse, NOT errdef.ErrNotFound: it is
// surfaced immediately without retry and without observation.
func TestPublishMutableTagPut404NotRetried(t *testing.T) {
	ctx := context.Background()
	payload := "put-404-manifest"
	desc := testDesc(payload)
	s, srv := newTagRetryServer(t)
	s.seedManifest(desc, payload)
	s.setPutStatus(http.StatusNotFound)
	wc := newTagRetryRepo(t, srv)

	err := wc.publishMutableTag(ctx, desc, wc.stateTag)
	var errResp *orasErrcode.ErrorResponse
	if !errors.As(err, &errResp) || errResp.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %v (%T), want a 404 errcode.ErrorResponse", err, err)
	}
	if errors.Is(err, errdef.ErrNotFound) {
		t.Error("errors.Is(err, errdef.ErrNotFound) = true; a PUT 404 must NOT be mistaken for the pre-push sentinel")
	}
	if errors.Is(err, ErrMutableTagMoved) {
		t.Error("errors.Is(err, ErrMutableTagMoved) = true; a PUT 404 must not enter the observation path")
	}

	reqs := s.requests()
	if n := countRequests(reqs, http.MethodGet, true); n != 1 {
		t.Errorf("digest GETs = %d, want 1", n)
	}
	if n := countRequests(reqs, http.MethodPut, false); n != 1 {
		t.Errorf("tag PUTs = %d, want exactly 1 (no retry of the failed PUT)", n)
	}
}

// TestPublishMutableTagPrePushRetryOverwritesRival — THE ACCEPTED LIMITATION,
// demonstrated deterministically: actor A's pre-push GET 404s; rival B
// publishes the lock tag while A is parked on the gate before its retry; A's
// retry GET then succeeds and A's tag PUT UNCONDITIONALLY OVERWRITES B's
// lock-tag REFERENCE. The precise claim: this demonstrates that A's retried
// unconditional PUT overwrites a rival's already-published lock-tag
// reference; it does NOT run B through the full Lock/verification/stability
// flow. This is the stale-intent window the retry accepts: OCI tags have no
// CAS, so the retry is NOT concurrency-safe, and W5 is changed for the
// pre-PUT 404 case (a publication that previously failed hard can now
// overwrite a rival).
func TestPublishMutableTagPrePushRetryOverwritesRival(t *testing.T) {
	payloadA := "stale-intent-lock-manifest"
	descA := testDesc(payloadA)
	s, srv := newTagRetryServer(t)
	s.seedManifest(descA, payloadA)
	first404 := make(chan struct{})
	atGate := make(chan struct{})
	goAhead := make(chan struct{})
	s.mu.Lock()
	s.onFirstDigest404 = first404
	s.onGateDigest = atGate
	s.gateDigest = descA.Digest.String()
	s.holdDigestGET = goAhead
	s.mu.Unlock()
	// A's FIRST digest GET 404s; every later GET of A's digest is gated on
	// goAhead, giving B a deterministic window to publish in between.
	s.setPrePush404s(1)
	wc := newTagRetryRepo(t, srv)

	// A runs on a bounded, cancellable context so a B-side failure can
	// never leave A's handler parked on the gate. Cleanup releases the gate
	// and cancels A BEFORE the httptest server closes (t.Cleanup is LIFO:
	// newTagRetryServer registered srv.Close first, so it runs last), so no
	// handler goroutine can outlive or block server shutdown.
	ctxA, cancelA := context.WithTimeout(context.Background(), testObservationTimeout)
	defer cancelA()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(goAhead) }) }
	t.Cleanup(func() {
		release()
		cancelA()
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- wc.publishMutableTag(ctxA, descA, wc.lockTag)
	}()

	// A's first pre-push GET 404s; wait until A is PARKED on the gated
	// second GET, so B's publication lands strictly between A's attempts.
	select {
	case <-first404:
	case <-time.After(testObservationTimeout):
		t.Fatal("timed out waiting for the first pre-push 404")
	}
	select {
	case <-atGate:
	case <-time.After(testObservationTimeout):
		t.Fatal("timed out waiting for A to reach the gated retry GET")
	}

	// Rival B publishes the lock AFTER A's first GET 404, through its own
	// real repository client (B's manifest bytes are seeded server-side,
	// mirroring "pushed moments earlier"; B's pre-push GET is not gated:
	// the gate is A's digest only).
	descB := testDesc("rival-lock-manifest")
	s.seedManifest(descB, "rival-lock-manifest")
	wcB := newTagRetryRepo(t, srv)
	if err := wcB.publishMutableTag(context.Background(), descB, wcB.lockTag); err != nil {
		t.Fatalf("rival publish: %v", err)
	}
	if got := s.tagDigest(wc.lockTag); got != descB.Digest.String() {
		t.Fatalf("rival lock digest = %s, want %s (rival must hold the tag before A retries)", got, descB.Digest)
	}

	// Release A's gated retry GET: it now succeeds, and A's PUT overwrites B.
	release()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("A's retried publish: %v", err)
		}
	case <-time.After(testObservationTimeout):
		t.Fatal("timed out waiting for A's retried publish to finish")
	}
	// Residual race demonstrated: A's stale intent won the unconditional
	// last-writer-wins PUT, overwriting B's already-published lock.
	if got := s.tagDigest(wc.lockTag); got != descA.Digest.String() {
		t.Errorf("lock tag digest = %s, want A's stale intent %s (the accepted overwrite did not happen as documented)", got, descA.Digest)
	}
	if n := countRequests(s.requests(), http.MethodPut, false); n != 2 {
		t.Errorf("lock-tag PUTs = %d, want 2 (B's PUT, then A's overwriting PUT)", n)
	}
}
