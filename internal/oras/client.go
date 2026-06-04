// Package oras provides an OCI registry client for storing Terraform state as
// OCI artifacts using the ORAS (OCI Registry As Storage) protocol.
//
// State is stored as OCI image manifests tagged with a workspace-derived name.
// Locking is implemented via a separate lock manifest with generation-based
// optimistic concurrency control to detect simultaneous lock attempts.
package oras

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/errdef"
	orasRegistry "oras.land/oras-go/v2/registry"
	orasErrcode "oras.land/oras-go/v2/registry/remote/errcode"
)

const (
	mediaTypeStateLayer     = "application/vnd.terraform.statefile.v1"
	mediaTypeStateLayerGzip = "application/vnd.terraform.statefile.v1+gzip"
	artifactTypeState       = "application/vnd.terraform.state.v1"
	artifactTypeLock        = "application/vnd.terraform.lock.v1"

	annotationWorkspace    = "org.terraform.workspace"
	annotationUpdatedAt    = "org.terraform.state.updated_at"
	annotationStateVersion = "org.terraform.state.version"
	annotationLockID       = "org.terraform.lock.id"
	annotationLockInfo     = "org.terraform.lock.info"
	annotationLockGen      = "org.terraform.lock.generation"
)

// defaultMaxStateSize is the default upper bound on state data read from the
// registry. It guards against OOM caused by a maliciously large or corrupted
// OCI layer (256 MiB).
const defaultMaxStateSize int64 = 256 * 1024 * 1024

// Tag naming scheme:
//   - State is stored at "state-<workspaceTag>".
//   - Lock is stored at "locked-<workspaceTag>".
//   - On registries that don't support manifest deletion (GHCR returns 405),
//     unlock retags to "unlocked-<workspaceTag>" instead.
const (
	stateTagPrefix           = "state-"
	lockTagPrefix            = "locked-"
	unlockedTagPrefix        = "unlocked-"
	stateVersionTagSeparator = "-v"
)

// gzipWriterPool pools gzip.Writer values to reduce per-Put allocations.
var gzipWriterPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	},
}

// gzipBufPool pools bytes.Buffer values used as the destination during compression.
var gzipBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// gzipReaderPool pools gzip.Reader values to reduce per-Get allocations.
var gzipReaderPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewReader(bytes.NewReader(minimalGzipStream))
		return gz
	},
}

// minimalGzipStream is the smallest valid gzip stream used to pre-warm the reader pool.
var minimalGzipStream = func() []byte {
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_ = gz.Close()
	return buf.Bytes()
}()

// defaultRetentionSem limits concurrent async retention goroutines globally.
var defaultRetentionSem = make(chan struct{}, 3)

// ─── Exported types ───────────────────────────────────────────────────────────

// RetryConfig defines retry behavior for operations against the OCI registry.
type RetryConfig struct {
	MaxAttempts       int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	BackoffMultiplier float64
}

const (
	defaultRetryMaxAttempts       = 3
	defaultRetryInitialBackoff    = time.Second
	defaultRetryMaxBackoff        = 30 * time.Second
	defaultRetryBackoffMultiplier = 2.0
)

// DefaultRetryConfig returns a RetryConfig with sensible defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:       defaultRetryMaxAttempts,
		InitialBackoff:    defaultRetryInitialBackoff,
		MaxBackoff:        defaultRetryMaxBackoff,
		BackoffMultiplier: defaultRetryBackoffMultiplier,
	}
}

// LockInfo holds information about a state lock.
type LockInfo struct {
	// ID is the unique identifier for this lock.
	ID string `json:"ID"`
	// Operation is the Terraform operation holding the lock.
	Operation string `json:"Operation"`
	// Info is additional free-form information about the lock holder.
	Info string `json:"Info"`
	// Who is the user/host holding the lock.
	Who string `json:"Who"`
	// Version is the Terraform version.
	Version string `json:"Version"`
	// Created is the time the lock was acquired.
	Created time.Time `json:"Created"`
	// Path is the state path for this lock.
	Path string `json:"Path"`
}

// LockError is returned when a state lock operation fails due to contention
// or an inconsistent read.
type LockError struct {
	// Info is the lock information of the current holder, if available.
	Info *LockInfo
	// Err is the underlying error.
	Err error
	// InconsistentRead indicates the lock state could not be reliably read,
	// typically due to a race condition or network error during verification.
	// Callers should treat this as a failed lock attempt and may retry.
	InconsistentRead bool
}

// Error implements the error interface.
func (e *LockError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Info != nil {
		return fmt.Sprintf("state locked by %s (%s)", e.Info.Who, e.Info.ID)
	}
	return "state is locked"
}

// ─── Internal types ───────────────────────────────────────────────────────────

// LockManifestData holds metadata stored in a lock manifest's annotations.
type LockManifestData struct {
	Generation  int64  `json:"generation"`
	LeaseExpiry int64  `json:"lease_expiry,omitempty"`
	HolderID    string `json:"holder_id,omitempty"`
}

// digestGroup groups version tags by their underlying manifest digest.
type digestGroup struct {
	desc ocispec.Descriptor
	tags []string
}

// manifest is the deserialized form of an OCI image manifest.
type manifest struct {
	ArtifactType string               `json:"artifactType"`
	MediaType    string               `json:"mediaType"`
	Annotations  map[string]string    `json:"annotations"`
	Layers       []ocispec.Descriptor `json:"layers"`
}

// fetchedManifest bundles a parsed manifest with its OCI descriptor.
type fetchedManifest struct {
	m    *manifest
	desc ocispec.Descriptor
}

// workspaceClient is an internal per-workspace OCI client, equivalent to
// ghoten's RemoteClient. It is created per operation on Client.
type workspaceClient struct {
	repo          *orasRepositoryClient
	workspaceName string
	stateTag      string
	lockTag       string
	unlockedTag   string
	retryConfig   RetryConfig

	stateCompression      string
	stateMaxSize          int64
	lockTTL               time.Duration
	versioningMaxVersions int

	now func() time.Time

	// retentionSem and retentionWg are shared from the parent Client.
	retentionSem chan struct{}
	retentionWg  *sync.WaitGroup
}

// newWorkspaceClient creates a workspaceClient for the given stateID (workspace name).
func newWorkspaceClient(c *Client, stateID string) *workspaceClient {
	wsTag := workspaceTagFor(stateID)
	return &workspaceClient{
		repo:                  c.repoClient,
		workspaceName:         stateID,
		stateTag:              stateTagPrefix + wsTag,
		lockTag:               lockTagPrefix + wsTag,
		unlockedTag:           unlockedTagPrefix + wsTag,
		retryConfig:           c.config.retryConfig,
		stateCompression:      c.config.compression,
		stateMaxSize:          c.config.maxStateSize,
		lockTTL:               c.config.lockTTL,
		versioningMaxVersions: c.config.maxVersions,
		now:                   c.now,
		retentionSem:          c.retentionSem,
		retentionWg:           &c.retentionWg,
	}
}

// nowUTC returns the current UTC time, using the injectable now func.
func (wc *workspaceClient) nowUTC() time.Time {
	if wc.now != nil {
		return wc.now().UTC()
	}
	return time.Now().UTC()
}

// ─── Client ───────────────────────────────────────────────────────────────────

// Get retrieves the state for the given stateID. Returns nil if no state exists.
func (c *Client) Get(ctx context.Context, stateID string) ([]byte, error) {
	wc := newWorkspaceClient(c, stateID)
	return withRetry(ctx, wc.retryConfig, func(ctx context.Context) ([]byte, error) {
		return wc.get(ctx)
	})
}

func (wc *workspaceClient) get(ctx context.Context) ([]byte, error) {
	fm, err := wc.fetchManifestWithDesc(ctx, wc.stateTag)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	m := fm.m
	if m.ArtifactType != "" && m.ArtifactType != artifactTypeState {
		return nil, fmt.Errorf("unexpected state manifest artifactType %q for %q", m.ArtifactType, wc.stateTag)
	}
	if len(m.Layers) == 0 {
		return nil, nil
	}

	layer := m.Layers[0]
	rc, err := wc.repo.inner.Fetch(ctx, layer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var r io.Reader = rc
	switch layer.MediaType {
	case mediaTypeStateLayer:
		// no decompression needed
	case mediaTypeStateLayerGzip:
		gz := gzipReaderPool.Get().(*gzip.Reader)
		if err := gz.Reset(rc); err != nil {
			gzipReaderPool.Put(gz)
			return nil, err
		}
		defer func() {
			gz.Close() //nolint:errcheck
			gzipReaderPool.Put(gz)
		}()
		r = gz
	default:
		return nil, fmt.Errorf("unsupported state layer media type %q", layer.MediaType)
	}

	limit := wc.stateMaxSize
	if limit <= 0 {
		limit = defaultMaxStateSize
	}
	lr := io.LimitReader(r, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("state size exceeds maximum allowed size of %d bytes; use the max_state_size option to increase the limit", limit)
	}

	return data, nil
}

// Put stores the state for the given stateID.
func (c *Client) Put(ctx context.Context, stateID string, data []byte) error {
	wc := newWorkspaceClient(c, stateID)
	return withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.put(ctx, data)
	})
}

func (wc *workspaceClient) put(ctx context.Context, state []byte) error {
	stateToPush := state
	layerMediaType := mediaTypeStateLayer

	if wc.stateCompression == "gzip" {
		compressed, err := compressGzip(state)
		if err != nil {
			return fmt.Errorf("compressing state: %w", err)
		}
		stateToPush = compressed
		layerMediaType = mediaTypeStateLayerGzip
	}

	var nextVersion int
	if wc.versioningMaxVersions > 0 {
		nextVersion = wc.currentStateVersion(ctx) + 1
	}

	layerDesc, err := oras.PushBytes(ctx, wc.repo.inner, layerMediaType, stateToPush)
	if err != nil {
		return err
	}

	manifestDesc, err := wc.packStateManifest(ctx, []ocispec.Descriptor{layerDesc}, nextVersion)
	if err != nil {
		return err
	}

	if err := wc.repo.inner.Tag(ctx, manifestDesc, wc.stateTag); err != nil {
		return err
	}

	if wc.versioningMaxVersions <= 0 {
		return nil
	}

	newVersionTag := wc.versionTagFor(nextVersion)
	if err := wc.repo.inner.Tag(ctx, manifestDesc, newVersionTag); err != nil {
		return err
	}

	// Async retention: limits concurrent goroutines via semaphore.
	// The detached background context (context.Background) is intentional —
	// these cleanup operations must complete even if the parent context is
	// cancelled, ensuring version retention limits are enforced regardless
	// of the calling operation's lifecycle.
	sem := wc.retentionSem
	if sem == nil {
		sem = defaultRetentionSem
	}
	select {
	case sem <- struct{}{}:
		wc.retentionWg.Add(1)
		go func() {
			defer wc.retentionWg.Done()
			defer func() { <-sem }()
			asyncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			existing, listErr := wc.listExistingVersions(asyncCtx)
			if listErr != nil {
				slog.Debug("async retention: failed to list versions", "error", listErr)
				return
			}
			found := false
			for _, v := range existing {
				if v == nextVersion {
					found = true
					break
				}
			}
			if !found {
				existing = append(existing, nextVersion)
			}
			if err := wc.enforceVersionRetention(asyncCtx, manifestDesc, existing); err != nil {
				slog.Debug("async retention cleanup failed", "error", err)
			}
		}()
	default:
		slog.Debug("async retention skipped: too many pending cleanups")
	}

	return nil
}

// Delete removes the state for the given stateID. Returns nil if no state exists.
func (c *Client) Delete(ctx context.Context, stateID string) error {
	wc := newWorkspaceClient(c, stateID)
	return withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.delete(ctx)
	})
}

func (wc *workspaceClient) delete(ctx context.Context) error {
	desc, err := wc.repo.inner.Resolve(ctx, wc.stateTag)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return wc.repo.inner.Delete(ctx, desc)
}

// Lock acquires a lock for the given stateID. Returns the lock ID on success.
func (c *Client) Lock(ctx context.Context, stateID string, info LockInfo) (string, error) {
	wc := newWorkspaceClient(c, stateID)
	return wc.lock(ctx, &info)
}

// lock acquires a distributed lock for the workspace using generation-based
// optimistic concurrency control.
//
// Pre:  ctx is non-nil; info is non-nil with a non-empty ID field.
// Post: on success, returns info.ID and the lock tag references a manifest with
//       Generation == previous_generation+1 and HolderID == info.ID.
//       On failure with an existing lock, returns *LockError with Info populated.
//
// Bounding function (termination): the function does not loop; it performs at
// most one stale-lock clear followed by one tag+verify attempt. All retries are
// delegated to withRetryNoResult with a finite MaxAttempts bound.
func (wc *workspaceClient) lock(ctx context.Context, info *LockInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("lock info is required")
	}

	var currentGen *LockManifestData
	var existingDesc ocispec.Descriptor
	lockFetched, err := wc.fetchManifestWithDesc(ctx, wc.lockTag)
	if err != nil && !isNotFound(err) {
		return "", fmt.Errorf("failed to read current lock state: %w", err)
	}
	if err == nil {
		currentGen, _ = parseLockManifestData(lockFetched.m)
		existingDesc = lockFetched.desc

		existing, parseErr := parseLockInfo(lockFetched.m, wc.stateTag)
		if parseErr != nil {
			return "", &LockError{InconsistentRead: true, Err: parseErr}
		}
		if existing != nil && existing.ID != "" {
			if wc.isLockStale(currentGen) {
				if err := wc.clearLock(ctx, existingDesc); err != nil {
					return "", err
				}
			} else {
				return "", &LockError{Info: existing, Err: fmt.Errorf("state is locked")}
			}
		}
	}

	newGeneration := int64(1)
	if currentGen != nil && currentGen.Generation > 0 {
		newGeneration = currentGen.Generation + 1
	}

	leaseExpiry := int64(0)
	if wc.lockTTL > 0 {
		leaseExpiry = wc.nowUTC().Add(wc.lockTTL).UnixNano()
	}

	info.Path = wc.stateTag
	infoBytes, err := json.Marshal(info)
	if err != nil {
		return "", err
	}

	manifestDesc, err := wc.packLockManifestWithGeneration(ctx, info.ID, string(infoBytes), newGeneration, leaseExpiry, info.ID)
	if err != nil {
		return "", err
	}

	err = withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.repo.inner.Tag(ctx, manifestDesc, wc.lockTag)
	})
	if err != nil {
		if _, resolveErr := wc.repo.inner.Resolve(ctx, wc.lockTag); resolveErr == nil {
			existing, _ := wc.getLockInfo(ctx)
			return "", &LockError{Info: existing, Err: fmt.Errorf("state is locked")}
		}
		return "", err
	}

	// Post-write verification: ensure we actually hold the lock.
	verified, verifyErr := wc.getLockManifestData(ctx)
	if verifyErr != nil {
		if cleanupDesc, cleanupErr := wc.repo.inner.Resolve(ctx, wc.lockTag); cleanupErr == nil {
			if cleanupDesc.Digest == manifestDesc.Digest {
				_ = wc.repo.inner.Delete(ctx, cleanupDesc)
			}
		}
		return "", &LockError{InconsistentRead: true, Err: fmt.Errorf("failed to verify lock acquisition: %w", verifyErr)}
	}
	if verified == nil || verified.Generation != newGeneration || verified.HolderID != info.ID {
		existing, _ := wc.getLockInfo(ctx)
		return "", &LockError{Info: existing, Err: fmt.Errorf("state is locked (lost race)")}
	}

	return info.ID, nil
}

// Unlock releases the lock for the given stateID. Returns nil if no lock exists.
func (c *Client) Unlock(ctx context.Context, stateID, lockID string) error {
	wc := newWorkspaceClient(c, stateID)
	return wc.unlock(ctx, lockID)
}

func (wc *workspaceClient) unlock(ctx context.Context, id string) error {
	fm, err := wc.fetchManifestWithDesc(ctx, wc.lockTag)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}

	existing, err := parseLockInfo(fm.m, wc.stateTag)
	if err != nil {
		return err
	}
	if existing == nil || existing.ID == "" {
		return nil
	}
	if id != "" && existing.ID != id {
		return fmt.Errorf("lock ID mismatch: held by %q", existing.ID)
	}

	err = withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.repo.inner.Delete(ctx, fm.desc)
	})
	if err == nil {
		return nil
	}
	if !isDeleteUnsupported(err) {
		return err
	}

	return wc.retagToUnlocked(ctx)
}

// List returns all workspace names stored in the OCI repository.
func (c *Client) List(ctx context.Context) ([]string, error) {
	return listWorkspacesFromTags(ctx, c.repoClient)
}

// WaitForRetention blocks until all in-flight async retention goroutines complete.
// Call this before process exit when versioning is enabled.
func (c *Client) WaitForRetention() {
	c.retentionWg.Wait()
}

// ─── workspaceClient helpers ──────────────────────────────────────────────────

func (wc *workspaceClient) packStateManifest(ctx context.Context, layers []ocispec.Descriptor, stateVersion int) (ocispec.Descriptor, error) {
	annotations := map[string]string{
		annotationWorkspace: wc.workspaceName,
		annotationUpdatedAt: wc.nowUTC().Format(time.RFC3339Nano),
	}
	if stateVersion > 0 {
		annotations[annotationStateVersion] = strconv.Itoa(stateVersion)
	}
	return oras.PackManifest(ctx, wc.repo.inner, oras.PackManifestVersion1_1, artifactTypeState, oras.PackManifestOptions{
		Layers:              layers,
		ManifestAnnotations: annotations,
	})
}

func (wc *workspaceClient) packLockManifestWithGeneration(ctx context.Context, id, infoJSON string, generation int64, leaseExpiry int64, holderID string) (ocispec.Descriptor, error) {
	lockData := LockManifestData{
		Generation:  generation,
		LeaseExpiry: leaseExpiry,
		HolderID:    holderID,
	}
	lockDataJSON, err := json.Marshal(lockData)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to marshal lock metadata: %w", err)
	}

	return oras.PackManifest(ctx, wc.repo.inner, oras.PackManifestVersion1_1, artifactTypeLock, oras.PackManifestOptions{
		ManifestAnnotations: map[string]string{
			annotationWorkspace: wc.workspaceName,
			annotationLockID:    id,
			annotationLockInfo:  infoJSON,
			annotationLockGen:   string(lockDataJSON),
		},
	})
}

func (wc *workspaceClient) versionTagFor(version int) string {
	return fmt.Sprintf("%s%s%d", wc.stateTag, stateVersionTagSeparator, version)
}

func (wc *workspaceClient) currentStateVersion(ctx context.Context) int {
	fm, err := wc.fetchManifestWithDesc(ctx, wc.stateTag)
	if err != nil {
		return 0
	}
	m := fm.m
	if v, ok := m.Annotations[annotationStateVersion]; ok {
		n, parseErr := strconv.Atoi(v)
		if parseErr == nil && n > 0 {
			return n
		}
	}
	existing, listErr := wc.listExistingVersions(ctx)
	if listErr != nil || len(existing) == 0 {
		return 0
	}
	max := 0
	for _, v := range existing {
		if v > max {
			max = v
		}
	}
	return max
}

func (wc *workspaceClient) listExistingVersions(ctx context.Context) ([]int, error) {
	var tags []string
	if err := wc.repo.inner.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	}); err != nil {
		return nil, err
	}

	var existing []int
	for _, t := range tags {
		base, v, ok := splitStateVersionTag(t)
		if !ok || base != wc.stateTag {
			continue
		}
		existing = append(existing, v)
	}
	return existing, nil
}

// enforceVersionRetention prunes old state versions, keeping at most
// wc.versioningMaxVersions versions.
//
// Pre:  wc.versioningMaxVersions > 0; versions contains the list of known
//       version numbers for this workspace.
// Post: at most wc.versioningMaxVersions versions remain in the registry;
//       the current manifest (identified by current.Digest) is never deleted.
//
// Loop invariant (over groups): each group processed has its keep-tagged
// manifests retagged to a new digest before the old digest is deleted.
// Bounding function: len(groups) - index of processed group.
func (wc *workspaceClient) enforceVersionRetention(ctx context.Context, current ocispec.Descriptor, versions []int) error {
	if wc.versioningMaxVersions <= 0 || len(versions) <= wc.versioningMaxVersions {
		return nil
	}

	sort.Ints(versions)
	toDeleteCount := len(versions) - wc.versioningMaxVersions
	deleteVersions := versions[:toDeleteCount]
	keepVersions := versions[toDeleteCount:]

	deleteTagSet := make(map[string]struct{}, len(deleteVersions))
	keepTagSet := make(map[string]struct{}, len(keepVersions))
	for _, v := range deleteVersions {
		deleteTagSet[wc.versionTagFor(v)] = struct{}{}
	}
	for _, v := range keepVersions {
		keepTagSet[wc.versionTagFor(v)] = struct{}{}
	}

	groups, err := wc.groupVersionsByDigest(ctx, versions, current.Digest)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}

	for _, g := range groups {
		tagsToDelete, tagsToKeep := classifyTags(g.tags, deleteTagSet, keepTagSet)
		if len(tagsToDelete) == 0 {
			continue
		}

		if len(tagsToKeep) > 0 {
			if err := wc.retagToNewManifest(ctx, tagsToKeep); err != nil {
				return err
			}
		}

		if err := wc.deleteDigestWithFallback(ctx, g.desc, tagsToDelete[0]); err != nil {
			return err
		}
	}

	return nil
}

func (wc *workspaceClient) groupVersionsByDigest(ctx context.Context, versions []int, currentDigest digest.Digest) (map[string]*digestGroup, error) {
	var mu sync.Mutex
	groups := make(map[string]*digestGroup)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	for _, v := range versions {
		tag := wc.versionTagFor(v)
		g.Go(func() error {
			desc, err := wc.repo.inner.Resolve(ctx, tag)
			if err != nil || desc.Digest == currentDigest {
				return nil //nolint:nilerr
			}
			key := desc.Digest.String()
			mu.Lock()
			if grp, ok := groups[key]; ok {
				grp.tags = append(grp.tags, tag)
			} else {
				groups[key] = &digestGroup{desc: desc, tags: []string{tag}}
			}
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return groups, nil
}

func classifyTags(tags []string, deleteSet, keepSet map[string]struct{}) (toDelete, toKeep []string) {
	for _, tag := range tags {
		if _, ok := deleteSet[tag]; ok {
			toDelete = append(toDelete, tag)
		} else if _, ok := keepSet[tag]; ok {
			toKeep = append(toKeep, tag)
		}
	}
	return
}

func (wc *workspaceClient) retagToNewManifest(ctx context.Context, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	slog.Debug("retention: detaching keep tags from digest", "tags", tags)

	fm, err := wc.fetchManifestWithDesc(ctx, tags[0])
	if err != nil {
		return err
	}
	m := fm.m
	if len(m.Layers) == 0 {
		return nil
	}

	preservedVersion := 0
	if v, ok := m.Annotations[annotationStateVersion]; ok {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			preservedVersion = n
		}
	}

	newDesc, err := wc.packStateManifest(ctx, m.Layers, preservedVersion)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := wc.repo.inner.Tag(ctx, newDesc, tag); err != nil {
			return err
		}
	}
	return nil
}

func (wc *workspaceClient) deleteDigestWithFallback(ctx context.Context, desc ocispec.Descriptor, fallbackTag string) error {
	err := wc.repo.inner.Delete(ctx, desc)
	if err == nil || isNotFound(err) {
		return nil
	}
	if !isDeleteUnsupported(err) {
		return err
	}

	ghErr := tryDeleteGHCRTag(ctx, wc.repo, fallbackTag)
	if errors.Is(ghErr, errNotGHCR) {
		return fmt.Errorf("registry does not support manifest deletion (HTTP 405) and no alternative deletion method is available for %q", fallbackTag)
	}
	if ghErr != nil {
		return fmt.Errorf("registry does not support manifest deletion and GHCR API fallback failed for %q: %w", fallbackTag, ghErr)
	}
	return nil
}

func (wc *workspaceClient) isLockStale(data *LockManifestData) bool {
	if wc.lockTTL <= 0 {
		return false
	}
	if data == nil || data.LeaseExpiry <= 0 {
		return false
	}
	return wc.nowUTC().UnixNano() > data.LeaseExpiry
}

func (wc *workspaceClient) clearLock(ctx context.Context, desc ocispec.Descriptor) error {
	err := withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.repo.inner.Delete(ctx, desc)
	})
	if err == nil || isNotFound(err) {
		return nil
	}
	if !isDeleteUnsupported(err) {
		return err
	}
	return wc.retagToUnlocked(ctx)
}

func (wc *workspaceClient) retagToUnlocked(ctx context.Context) error {
	desc, err := withRetry(ctx, wc.retryConfig, func(ctx context.Context) (ocispec.Descriptor, error) {
		return wc.repo.inner.Resolve(ctx, wc.unlockedTag)
	})
	if isNotFound(err) {
		desc, err = wc.packLockManifestWithGeneration(ctx, "", "", 0, 0, "")
		if err != nil {
			return err
		}
		if err := withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
			return wc.repo.inner.Tag(ctx, desc, wc.unlockedTag)
		}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return withRetryNoResult(ctx, wc.retryConfig, func(ctx context.Context) error {
		return wc.repo.inner.Tag(ctx, desc, wc.lockTag)
	})
}

func (wc *workspaceClient) getLockInfo(ctx context.Context) (*LockInfo, error) {
	fm, err := wc.fetchManifestWithDesc(ctx, wc.lockTag)
	if err != nil {
		return nil, err
	}
	return parseLockInfo(fm.m, wc.stateTag)
}

func (wc *workspaceClient) getLockManifestData(ctx context.Context) (*LockManifestData, error) {
	fm, err := wc.fetchManifestWithDesc(ctx, wc.lockTag)
	if err != nil {
		return nil, err
	}
	return parseLockManifestData(fm.m)
}

func (wc *workspaceClient) fetchManifestWithDesc(ctx context.Context, reference string) (fetchedManifest, error) {
	return withRetry(ctx, wc.retryConfig, func(ctx context.Context) (fetchedManifest, error) {
		return wc.fetchManifestInternal(ctx, reference)
	})
}

func (wc *workspaceClient) fetchManifestInternal(ctx context.Context, reference string) (fetchedManifest, error) {
	desc, err := wc.repo.inner.Resolve(ctx, reference)
	if err != nil {
		return fetchedManifest{}, err
	}
	rc, err := wc.repo.inner.Fetch(ctx, desc)
	if err != nil {
		return fetchedManifest{}, err
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fetchedManifest{}, err
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fetchedManifest{}, fmt.Errorf("decoding manifest %q: %w", reference, err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	return fetchedManifest{m: &m, desc: desc}, nil
}

// ─── Package-level helpers ────────────────────────────────────────────────────

func parseLockInfo(m *manifest, stateTag string) (*LockInfo, error) {
	if m.ArtifactType != "" && m.ArtifactType != artifactTypeLock {
		return nil, fmt.Errorf("unexpected lock manifest artifactType %q", m.ArtifactType)
	}
	if m.Annotations == nil {
		return &LockInfo{}, nil
	}
	if raw, ok := m.Annotations[annotationLockInfo]; ok && raw != "" {
		var info LockInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			return nil, fmt.Errorf("decoding lock info: %w", err)
		}
		if info.ID == "" {
			info.ID = m.Annotations[annotationLockID]
		}
		if info.Path == "" {
			info.Path = stateTag
		}
		return &info, nil
	}
	id := m.Annotations[annotationLockID]
	if id == "" {
		return &LockInfo{}, nil
	}
	return &LockInfo{ID: id, Path: stateTag}, nil
}

func parseLockManifestData(m *manifest) (*LockManifestData, error) {
	if m.ArtifactType != "" && m.ArtifactType != artifactTypeLock {
		return nil, fmt.Errorf("unexpected lock manifest artifactType %q", m.ArtifactType)
	}
	if raw, ok := m.Annotations[annotationLockGen]; ok && raw != "" {
		var data LockManifestData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, fmt.Errorf("decoding lock generation data: %w", err)
		}
		return &data, nil
	}
	return &LockManifestData{Generation: 0}, nil
}

// workspaceTagFor converts a workspace name to a valid OCI tag, hashing if necessary.
func workspaceTagFor(workspace string) string {
	ref := orasRegistry.Reference{Reference: workspace}
	if err := ref.ValidateReferenceAsTag(); err == nil {
		return workspace
	}
	h := sha256.Sum256([]byte(workspace))
	return "ws-" + hex.EncodeToString(h[:8])
}

// listWorkspacesFromTags discovers workspace names by scanning the repository's
// OCI tags.
//
// Pre:  ctx is non-nil; repo is non-nil with a valid inner repository.
// Post: returns a sorted, deduplicated list of workspace names. Names that were
//       originally hashed (ws-* tags) are resolved from their manifest
//       annotation. Returns nil slice (not error) if no workspaces exist.
func listWorkspacesFromTags(ctx context.Context, repo *orasRepositoryClient) ([]string, error) {
	var tags []string
	if err := repo.inner.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	}); err != nil {
		return nil, err
	}

	tagSet := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		tagSet[t] = struct{}{}
	}

	var relevantTags []string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, stateTagPrefix) {
			continue
		}
		if base, _, ok := splitStateVersionTag(tag); ok {
			if _, ok := tagSet[base]; ok {
				continue
			}
		}
		relevantTags = append(relevantTags, tag)
	}

	var mu sync.Mutex
	var out []string
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	for _, tag := range relevantTags {
		g.Go(func() error {
			name, err := workspaceNameFromTag(ctx, repo, tag)
			if err != nil {
				slog.Debug("failed to resolve workspace name", "tag", tag, "error", err)
				return nil // Continue instead of failing
			}
			if name != "" {
				mu.Lock()
				out = append(out, name)
				mu.Unlock()
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	sort.Strings(out)
	if len(out) == 0 {
		return out, nil
	}

	dedup := out[:1]
	for i := 1; i < len(out); i++ {
		if out[i] != out[i-1] {
			dedup = append(dedup, out[i])
		}
	}
	return dedup, nil
}

func splitStateVersionTag(tag string) (base string, version int, ok bool) {
	idx := strings.LastIndex(tag, stateVersionTagSeparator)
	if idx < 0 {
		return "", 0, false
	}
	base = tag[:idx]
	if base == "" {
		return "", 0, false
	}
	s := tag[idx+len(stateVersionTagSeparator):]
	v, err := strconv.Atoi(s)
	// Version numbers are bounded to 1<<30 to prevent overflow on 32-bit systems.
	if err != nil || v <= 0 || v > 1<<30 {
		return "", 0, false
	}
	return base, v, true
}

func workspaceNameFromTag(ctx context.Context, repo *orasRepositoryClient, stateTag string) (string, error) {
	wsTag := strings.TrimPrefix(stateTag, stateTagPrefix)
	if !strings.HasPrefix(wsTag, "ws-") {
		return wsTag, nil
	}

	// Use retry for transient errors
	return withRetry(ctx, DefaultRetryConfig(), func(ctx context.Context) (string, error) {
		desc, err := repo.inner.Resolve(ctx, stateTag)
		if err != nil {
			return "", err
		}
		rc, err := repo.inner.Fetch(ctx, desc)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()

		data, err := io.ReadAll(rc)
		if err != nil {
			return "", err
		}

		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return "", fmt.Errorf("decoding manifest for workspace tag %q: %w", stateTag, err)
		}
		if name := m.Annotations[annotationWorkspace]; name != "" {
			return name, nil
		}
		return wsTag, nil
	})
}

// compressGzip compresses data using gzip at BestSpeed level.
//
// Pre:  data may be nil or empty (both produce valid gzip output).
// Post: len(result) > 0; gzip.NewReader(bytes.NewReader(result)) succeeds and
//       decompressing result reproduces data exactly.
func compressGzip(data []byte) ([]byte, error) {
	buf := gzipBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer gzipBufPool.Put(buf)

	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(buf)
	defer gzipWriterPool.Put(gz)

	if _, err := gz.Write(data); err != nil {
		_ = gz.Close() // Close error is secondary; write error takes precedence
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// ─── Retry helpers ────────────────────────────────────────────────────────────

// withRetry executes operation with exponential backoff, retrying only on
// transient errors.
//
// Pre:  ctx is non-nil; operation is non-nil; cfg.MaxAttempts >= 1.
// Post: returns the first successful result, or the last error after
//       cfg.MaxAttempts attempts.
//
// Loop invariant: 1 <= attempt <= cfg.MaxAttempts; backoff >= cfg.InitialBackoff.
// Bounding function: cfg.MaxAttempts - attempt, which strictly decreases each
// iteration and reaches 0 when attempt == cfg.MaxAttempts.
func withRetry[T any](ctx context.Context, cfg RetryConfig, operation func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error

	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}

	backoff := cfg.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		// Check context before attempting operation to avoid unnecessary work.
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := operation(ctx)
		if err == nil {
			return result, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !isTransientError(err) {
			return zero, err
		}
		if attempt == cfg.MaxAttempts {
			break
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}

		backoff = time.Duration(float64(backoff) * cfg.BackoffMultiplier)
		if cfg.MaxBackoff > 0 && backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}

	return zero, lastErr
}

func withRetryNoResult(ctx context.Context, cfg RetryConfig, operation func(ctx context.Context) error) error {
	_, err := withRetry(ctx, cfg, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, operation(ctx)
	})
	return err
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	var errResp *orasErrcode.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusRequestTimeout,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection reset"):
		return true
	case strings.Contains(msg, "connection refused"):
		return true
	case strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "connection timeout"),
		strings.Contains(msg, "tls handshake timeout"),
		strings.Contains(msg, "context deadline exceeded"):
		return true
	case strings.Contains(msg, "temporary failure in name resolution"):
		return true
	case strings.Contains(msg, "no such host"):
		return true
	case strings.Contains(msg, "unexpected eof"),
		strings.Contains(msg, "read: eof"),
		msg == "eof":
		return true
	default:
		return false
	}
}

// ─── Error helpers ────────────────────────────────────────────────────────────

func isNotFound(err error) bool {
	if errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var resp *orasErrcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == 404
	}
	return false
}

func isDeleteUnsupported(err error) bool {
	var resp *orasErrcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == 405
	}
	return false
}
