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

// defaultRetentionSem limits concurrent async retention goroutines globally.
var defaultRetentionSem = make(chan struct{}, 3)

// ─── Exported types ───────────────────────────────────────────────────────────

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
	client      *Client
	stateID     string
	stateTag    string
	lockTag     string
	unlockedTag string
}

// newWorkspaceClient creates a workspaceClient for the given stateID (workspace name).
func newWorkspaceClient(c *Client, stateID string) *workspaceClient {
	wsTag := workspaceTagFor(stateID)
	return &workspaceClient{
		client:      c,
		stateID:     stateID,
		stateTag:    stateTagPrefix + wsTag,
		lockTag:     lockTagPrefix + wsTag,
		unlockedTag: unlockedTagPrefix + wsTag,
	}
}

// ─── Client ───────────────────────────────────────────────────────────────────

// Get retrieves the state for the given stateID. Returns nil if no state exists.
func (c *Client) Get(ctx context.Context, stateID string) ([]byte, error) {
	wc := newWorkspaceClient(c, stateID)
	return retryWithResult(ctx, func(ctx context.Context) ([]byte, error) {
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
	rc, err := wc.client.repoClient.inner.Fetch(ctx, layer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var r io.Reader = rc
	switch layer.MediaType {
	case mediaTypeStateLayer:
		// no decompression needed
	case mediaTypeStateLayerGzip:
		gz, err := gzip.NewReader(rc)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	default:
		return nil, fmt.Errorf("unsupported state layer media type %q", layer.MediaType)
	}

	limit := wc.client.config.MaxStateSize
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
	return retry(ctx, func(ctx context.Context) error {
		return wc.put(ctx, data)
	})
}

func (wc *workspaceClient) put(ctx context.Context, state []byte) error {
	stateToPush := state
	layerMediaType := mediaTypeStateLayer

	if wc.client.config.Compression == "gzip" {
		compressed, err := compressGzip(state)
		if err != nil {
			return fmt.Errorf("compressing state: %w", err)
		}
		stateToPush = compressed
		layerMediaType = mediaTypeStateLayerGzip
	}

	var nextVersion int
	if wc.client.config.MaxVersions > 0 {
		nextVersion = wc.currentStateVersion(ctx) + 1
	}

	layerDesc, err := oras.PushBytes(ctx, wc.client.repoClient.inner, layerMediaType, stateToPush)
	if err != nil {
		return err
	}

	manifestDesc, err := wc.packStateManifest(ctx, []ocispec.Descriptor{layerDesc}, nextVersion)
	if err != nil {
		return err
	}

	if err := wc.client.repoClient.inner.Tag(ctx, manifestDesc, wc.stateTag); err != nil {
		return err
	}

	if wc.client.config.MaxVersions <= 0 {
		return nil
	}

	newVersionTag := wc.versionTagFor(nextVersion)
	if err := wc.client.repoClient.inner.Tag(ctx, manifestDesc, newVersionTag); err != nil {
		return err
	}

	// Async retention: limits concurrent goroutines via semaphore.
	// The detached background context (context.Background) is intentional —
	// these cleanup operations must complete even if the parent context is
	// cancelled, ensuring version retention limits are enforced regardless
	// of the calling operation's lifecycle.
	sem := wc.client.retentionSem
	if sem == nil {
		sem = defaultRetentionSem
	}
	select {
	case sem <- struct{}{}:
		wc.client.retentionWg.Add(1)
		go func() {
			defer wc.client.retentionWg.Done()
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
	return retry(ctx, func(ctx context.Context) error {
		return wc.delete(ctx)
	})
}

func (wc *workspaceClient) delete(ctx context.Context) error {
	desc, err := wc.client.repoClient.inner.Resolve(ctx, wc.stateTag)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return wc.client.repoClient.inner.Delete(ctx, desc)
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
	if wc.client.config.LockTTL > 0 {
		leaseExpiry = wc.client.nowUTC().Add(wc.client.config.LockTTL).UnixNano()
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

	err = retry(ctx, func(ctx context.Context) error {
		return wc.client.repoClient.inner.Tag(ctx, manifestDesc, wc.lockTag)
	})
	if err != nil {
		if _, resolveErr := wc.client.repoClient.inner.Resolve(ctx, wc.lockTag); resolveErr == nil {
			existing, _ := wc.getLockInfo(ctx)
			return "", &LockError{Info: existing, Err: fmt.Errorf("state is locked")}
		}
		return "", err
	}

	// Post-write verification: ensure we actually hold the lock.
	verified, verifyErr := wc.getLockManifestData(ctx)
	if verifyErr != nil {
		if cleanupDesc, cleanupErr := wc.client.repoClient.inner.Resolve(ctx, wc.lockTag); cleanupErr == nil {
			if cleanupDesc.Digest == manifestDesc.Digest {
				_ = wc.client.repoClient.inner.Delete(ctx, cleanupDesc)
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

	err = retry(ctx, func(ctx context.Context) error {
		return wc.client.repoClient.inner.Delete(ctx, fm.desc)
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
		annotationWorkspace: wc.stateID,
		annotationUpdatedAt: wc.client.nowUTC().Format(time.RFC3339Nano),
	}
	if stateVersion > 0 {
		annotations[annotationStateVersion] = strconv.Itoa(stateVersion)
	}
	return oras.PackManifest(ctx, wc.client.repoClient.inner, oras.PackManifestVersion1_1, artifactTypeState, oras.PackManifestOptions{
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

	return oras.PackManifest(ctx, wc.client.repoClient.inner, oras.PackManifestVersion1_1, artifactTypeLock, oras.PackManifestOptions{
		ManifestAnnotations: map[string]string{
			annotationWorkspace: wc.stateID,
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
	if err := wc.client.repoClient.inner.Tags(ctx, "", func(page []string) error {
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
// wc.client.config.MaxVersions versions.
//
// Pre:  wc.client.config.MaxVersions > 0; versions contains the list of known
//       version numbers for this workspace.
// Post: at most wc.client.config.MaxVersions versions remain in the registry;
//       the current manifest (identified by current.Digest) is never deleted.
//
// Loop invariant (over groups): each group processed has its keep-tagged
// manifests retagged to a new digest before the old digest is deleted.
// Bounding function: len(groups) - index of processed group.
func (wc *workspaceClient) enforceVersionRetention(ctx context.Context, current ocispec.Descriptor, versions []int) error {
	if wc.client.config.MaxVersions <= 0 || len(versions) <= wc.client.config.MaxVersions {
		return nil
	}

	sort.Ints(versions)
	toDeleteCount := len(versions) - wc.client.config.MaxVersions
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
			desc, err := wc.client.repoClient.inner.Resolve(ctx, tag)
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
		if err := wc.client.repoClient.inner.Tag(ctx, newDesc, tag); err != nil {
			return err
		}
	}
	return nil
}

func (wc *workspaceClient) deleteDigestWithFallback(ctx context.Context, desc ocispec.Descriptor, fallbackTag string) error {
	err := wc.client.repoClient.inner.Delete(ctx, desc)
	if err == nil || isNotFound(err) {
		return nil
	}
	if !isDeleteUnsupported(err) {
		return err
	}

	ghErr := tryDeleteGHCRTag(ctx, wc.client.repoClient, fallbackTag)
	if errors.Is(ghErr, errNotGHCR) {
		return fmt.Errorf("registry does not support manifest deletion (HTTP 405) and no alternative deletion method is available for %q", fallbackTag)
	}
	if ghErr != nil {
		return fmt.Errorf("registry does not support manifest deletion and GHCR API fallback failed for %q: %w", fallbackTag, ghErr)
	}
	return nil
}

func (wc *workspaceClient) isLockStale(data *LockManifestData) bool {
	if wc.client.config.LockTTL <= 0 {
		return false
	}
	if data == nil || data.LeaseExpiry <= 0 {
		return false
	}
	return wc.client.nowUTC().UnixNano() > data.LeaseExpiry
}

func (wc *workspaceClient) clearLock(ctx context.Context, desc ocispec.Descriptor) error {
	err := retry(ctx, func(ctx context.Context) error {
		return wc.client.repoClient.inner.Delete(ctx, desc)
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
	desc, err := retryWithResult(ctx, func(ctx context.Context) (ocispec.Descriptor, error) {
		return wc.client.repoClient.inner.Resolve(ctx, wc.unlockedTag)
	})
	if isNotFound(err) {
		desc, err = wc.packLockManifestWithGeneration(ctx, "", "", 0, 0, "")
		if err != nil {
			return err
		}
if err := retry(ctx, func(ctx context.Context) error {
		return wc.client.repoClient.inner.Tag(ctx, desc, wc.unlockedTag)
	}); err != nil {
		return err
	}
} else if err != nil {
	return err
}
return retry(ctx, func(ctx context.Context) error {
	return wc.client.repoClient.inner.Tag(ctx, desc, wc.lockTag)
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
	return retryWithResult(ctx, func(ctx context.Context) (fetchedManifest, error) {
		return wc.fetchManifestInternal(ctx, reference)
	})
}

func (wc *workspaceClient) fetchManifestInternal(ctx context.Context, reference string) (fetchedManifest, error) {
	desc, err := wc.client.repoClient.inner.Resolve(ctx, reference)
	if err != nil {
		return fetchedManifest{}, err
	}
	rc, err := wc.client.repoClient.inner.Fetch(ctx, desc)
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
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	tagSet := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		tagSet[t] = struct{}{}
	}

	var out []string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, stateTagPrefix) {
			continue
		}
		if base, _, ok := splitStateVersionTag(tag); ok {
			if _, ok := tagSet[base]; ok {
				continue
			}
		}
		name, err := workspaceNameFromTag(ctx, repo, tag)
		if err != nil {
			slog.Debug("failed to resolve workspace name", "tag", tag, "error", err)
			continue
		}
		if name != "" {
			out = append(out, name)
		}
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
	return retryWithResult(ctx, func(ctx context.Context) (string, error) {
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
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ─── Retry helpers ────────────────────────────────────────────────────────────
//
// Retry runs operation up to 3 times with exponential backoff (1s, 2s, 4s),
// retrying only on transient errors.
func retry(ctx context.Context, operation func(context.Context) error) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = operation(ctx)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil || !isTransientError(lastErr) {
			return lastErr
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return lastErr
}

func retryWithResult[T any](ctx context.Context, operation func(context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := operation(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if ctx.Err() != nil || !isTransientError(err) {
			return zero, err
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return zero, lastErr
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
