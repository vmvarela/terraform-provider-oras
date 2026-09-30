package oras

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasRegistry "oras.land/oras-go/v2/registry"
)

func TestWorkspaceMappingRegression(t *testing.T) {
	if _, _, ok := splitStateVersionTag("stver-x-v02"); ok {
		t.Error("non-canonical version suffix accepted")
	}
	if workspaceTagFor("a/b") == workspaceTagFor("ws-c14cddc033f64b9d") {
		t.Error("distinct workspaces share an identifier")
	}
	for _, name := range []string{strings.Repeat("a", 128), strings.Repeat("a", 129), "a/b", "ws-c14cddc033f64b9d", "日本語", "default"} {
		wc := newWorkspaceClient(nil, name)
		for _, tag := range []string{wc.stateTag, wc.lockTag, wc.unlockedTag, wc.versionTagFor(1), wc.versionTagFor(1 << 30), wc.versionTagFor(int(^uint(0) >> 1))} {
			if err := (orasRegistry.Reference{Reference: tag}).ValidateReferenceAsTag(); err != nil {
				t.Errorf("workspace %q produces invalid tag %q: %v", name, tag, err)
			}
		}
	}
}

func TestWorkspaceAliasIsolation(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(&orasRepositoryClient{inner: newFakeORASRepo()})
	names := []string{"a/b", "ws-c14cddc033f64b9d"}
	for _, name := range names {
		if err := c.Put(ctx, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		data, err := c.Get(ctx, name)
		if err != nil || string(data) != name {
			t.Fatalf("workspace %q: got %q, %v", name, data, err)
		}
	}
}

// Exercise public operations; distinct workspaces may be locked concurrently.
func exerciseWorkspaceIsolation(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	names := []string{"a/b", "ws-c14cddc033f64b9d", strings.Repeat("x", 128), "日本語", "state-literal"}
	c.config.MaxVersions = 2
	for _, name := range names {
		if _, err := c.Lock(ctx, name, LockInfo{ID: name}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if err := c.Put(ctx, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			c.WaitForRetention()
		}
	}
	got, err := c.List(ctx)
	if err != nil || len(got) != len(names) {
		t.Fatalf("list = %v, %v", got, err)
	}
	for _, name := range names {
		if !slices.Contains(got, name) {
			t.Errorf("list missing %q", name)
		}
		data, err := c.Get(ctx, name)
		if err != nil || string(data) != name {
			t.Fatalf("read %q = %q, %v", name, data, err)
		}
		versions, err := newWorkspaceClient(c, name).listExistingVersions(ctx)
		if err != nil || len(versions) != 2 {
			t.Fatalf("versions for %q = %v, %v", name, versions, err)
		}
	}
	if err := c.Unlock(ctx, names[0], names[1]); err == nil {
		t.Fatal("foreign lock ID unlocked workspace")
	}
	if err := c.Unlock(ctx, names[0], names[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, names[0]); err != nil {
		t.Fatal(err)
	}
	for _, name := range names[1:] {
		if err := c.VerifyLock(ctx, name, name); err != nil {
			t.Fatal(err)
		}
		data, err := c.Get(ctx, name)
		if err != nil || string(data) != name {
			t.Fatalf("other workspace changed: %q, %v", data, err)
		}
		if err := c.Unlock(ctx, name, name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceLifecycleIsolation(t *testing.T) {
	exerciseWorkspaceIsolation(t, newTestClient(&orasRepositoryClient{inner: newFakeORASRepo()}))
}

func TestWorkspaceUnlockedMarkerIsolation(t *testing.T) {
	fake := newFakeORASRepo()
	repo := &deleteUnsupportedRepo{delegatingRepo: delegatingRepo{inner: fake}}
	c := newTestClient(&orasRepositoryClient{inner: repo})
	ctx := context.Background()
	for _, name := range []string{"a/b", "ws-c14cddc033f64b9d"} {
		if _, err := c.Lock(ctx, name, LockInfo{ID: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unlock(ctx, "a/b", "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := c.VerifyLock(ctx, "ws-c14cddc033f64b9d", "ws-c14cddc033f64b9d"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lock(ctx, "a/b", LockInfo{ID: "again"}); err != nil {
		t.Fatal(err)
	}
}

func workspaceOperations(c *Client) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"read":   func() error { _, err := c.Get(ctx, "default"); return err },
		"write":  func() error { return c.Put(ctx, "default", []byte("must not land")) },
		"lock":   func() error { _, err := c.Lock(ctx, "default", LockInfo{ID: "new"}); return err },
		"unlock": func() error { return c.Unlock(ctx, "default", "old") },
		"verify": func() error { return c.VerifyLock(ctx, "default", "old") },
		"delete": func() error { return c.Delete(ctx, "default") },
	}
}

func TestWorkspaceRejectsLegacyRepository(t *testing.T) {
	for _, tag := range []string{"state-default", "state-ws-c14cddc033f64b9d", "locked-default", "unlocked-default", "stver-default-v1"} {
		t.Run(tag, func(t *testing.T) {
			fake := newFakeORASRepo()
			c := newTestClient(&orasRepositoryClient{inner: fake})
			ctx := context.Background()
			// Also cover a mixed-format repository: never choose one layout silently.
			if err := c.Put(ctx, "default", []byte("new layout")); err != nil {
				t.Fatal(err)
			}
			desc, err := newWorkspaceClient(c, "legacy-owner").packLockManifest(ctx, "", 0, 0, "old")
			if err != nil {
				t.Fatal(err)
			}
			if err := fake.Tag(ctx, desc, tag); err != nil {
				t.Fatal(err)
			}
			ops := workspaceOperations(c)
			ops["list"] = func() error { _, err := c.List(ctx); return err }
			for name, op := range ops {
				if err := op(); err == nil || !strings.Contains(err.Error(), "legacy workspace tag") {
					t.Errorf("%s: %v", name, err)
				}
			}
			got, err := fake.Resolve(ctx, tag)
			if err != nil || got.Digest != desc.Digest {
				t.Fatal("legacy object mutated")
			}
			data, err := newWorkspaceClient(c, "default").get(ctx)
			if err != nil || string(data) != "new layout" {
				t.Fatal("new-format state mutated")
			}
		})
	}
}

func TestWorkspaceRejectsMismatchedIdentity(t *testing.T) {
	for _, kind := range []string{"state", "lock", "marker"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			fake := newFakeORASRepo()
			c := newTestClient(&orasRepositoryClient{inner: fake})
			wc := newWorkspaceClient(c, "default")
			desc, err := newWorkspaceClient(c, "other").packLockManifest(ctx, "", 0, 0, "old")
			if err != nil {
				t.Fatal(err)
			}
			tag := map[string]string{"state": wc.stateTag, "lock": wc.lockTag, "marker": wc.unlockedTag}[kind]
			if err := fake.Tag(ctx, desc, tag); err != nil {
				t.Fatal(err)
			}
			for name, op := range workspaceOperations(c) {
				if err := op(); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
					t.Errorf("%s: %v", name, err)
				}
			}
			if kind == "state" {
				if _, err := c.List(ctx); err == nil {
					t.Fatal("list guessed an identity")
				}
			}
			got, err := fake.Resolve(ctx, tag)
			if err != nil || got.Digest != desc.Digest {
				t.Fatal("foreign object mutated")
			}
		})
	}
}

type workspaceScanFailure struct{ delegatingRepo }

// Version manifests are not read by the preflight (ADR-0002 amendment): a
// mismatched version tag must be caught on use, before retention deletes a
// digest that also backs another workspace's state.
func TestWorkspaceVersionIdentityVerifiedOnUse(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	c := newTestClient(&orasRepositoryClient{inner: fake})
	c.config.MaxVersions = 1
	if err := c.Put(ctx, "other", []byte("other state")); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"d1", "d2"} {
		if err := c.Put(ctx, "default", []byte(data)); err != nil {
			t.Fatal(err)
		}
		c.WaitForRetention()
	}
	foreign, err := fake.Resolve(ctx, newWorkspaceClient(c, "other").stateTag)
	if err != nil {
		t.Fatal(err)
	}
	versionTag := newWorkspaceClient(c, "default").versionTagFor(1)
	if err := fake.Tag(ctx, foreign, versionTag); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Lock(ctx, "default", LockInfo{ID: "me"}); err != nil {
		t.Fatalf("lock blocked by an unused version tag: %v", err)
	}
	if err := c.Put(ctx, "default", []byte("d3")); err != nil {
		t.Fatal(err)
	}
	c.WaitForRetention() // retention must refuse the foreign digest
	if err := c.Unlock(ctx, "default", "me"); err != nil {
		t.Fatal(err)
	}

	if data, err := c.Get(ctx, "other"); err != nil || string(data) != "other state" {
		t.Fatalf("retention destroyed another workspace's state: %q, %v", data, err)
	}
	if got, err := fake.Resolve(ctx, versionTag); err != nil || got.Digest != foreign.Digest {
		t.Fatal("foreign version object mutated")
	}
	if data, err := c.Get(ctx, "default"); err != nil || string(data) != "d3" {
		t.Fatalf("default state = %q, %v", data, err)
	}
}

// Paths that trust version numbers must verify version manifests first:
// the preflight no longer reads them (ADR-0002 Amendment 1).
func TestWorkspaceUnverifiedVersionsNotTrusted(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*fakeORASRepo, *Client, *workspaceClient, ocispec.Descriptor) {
		t.Helper()
		fake := newFakeORASRepo()
		c := newTestClient(&orasRepositoryClient{inner: fake})
		if err := c.Put(ctx, "other", []byte("other state")); err != nil {
			t.Fatal(err)
		}
		foreign, err := fake.Resolve(ctx, newWorkspaceClient(c, "other").stateTag)
		if err != nil {
			t.Fatal(err)
		}
		return fake, c, newWorkspaceClient(c, "default"), foreign
	}
	mustTag := func(t *testing.T, fake *fakeORASRepo, desc ocispec.Descriptor, tag string) {
		t.Helper()
		if err := fake.Tag(context.Background(), desc, tag); err != nil {
			t.Fatal(err)
		}
	}
	wantState := func(t *testing.T, c *Client, ws, want string) {
		t.Helper()
		if data, err := c.Get(ctx, ws); err != nil || string(data) != want {
			t.Fatalf("%s state = %q, %v; want %q", ws, data, err, want)
		}
	}

	t.Run("destination overwrite", func(t *testing.T) {
		// No state yet: allocation restarts at v1, which holds a foreign manifest.
		fake, c, wc, foreign := setup(t)
		mustTag(t, fake, foreign, wc.versionTagFor(1))
		c.config.MaxVersions = 1
		if err := c.Put(ctx, "default", []byte("new")); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("put = %v", err)
		}
		if got, err := fake.Resolve(ctx, wc.versionTagFor(1)); err != nil || got.Digest != foreign.Digest {
			t.Fatal("foreign version tag overwritten")
		}
		wantState(t, c, "default", "")
	})

	t.Run("allocation fallback", func(t *testing.T) {
		// A state without a version annotation falls back to version tags.
		fake, c, wc, foreign := setup(t)
		if err := c.Put(ctx, "default", []byte("v0")); err != nil {
			t.Fatal(err)
		}
		mustTag(t, fake, foreign, wc.versionTagFor(7))
		c.config.MaxVersions = 1
		if err := c.Put(ctx, "default", []byte("new")); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("put = %v", err)
		}
		wantState(t, c, "default", "v0")
	})

	t.Run("allocation bound", func(t *testing.T) {
		fake, c, wc, _ := setup(t)
		if err := c.Put(ctx, "default", []byte("v0")); err != nil {
			t.Fatal(err)
		}
		own, err := fake.Resolve(ctx, wc.stateTag)
		if err != nil {
			t.Fatal(err)
		}
		mustTag(t, fake, own, wc.versionTagFor(maxStateVersion))
		// An out-of-range annotation must not wrap the allocation around.
		fm, _, err := wc.fetchManifestWithDesc(ctx, wc.stateTag)
		if err != nil {
			t.Fatal(err)
		}
		huge, err := wc.packStateManifest(ctx, fm.Layers, math.MaxInt)
		if err != nil {
			t.Fatal(err)
		}
		mustTag(t, fake, huge, wc.stateTag)
		c.config.MaxVersions = 1
		if err := c.Put(ctx, "default", []byte("new")); err == nil || !strings.Contains(err.Error(), "exceeds the supported maximum") {
			t.Fatalf("put = %v", err)
		}
		wantState(t, c, "default", "v0")
	})

	t.Run("retention cutoff", func(t *testing.T) {
		// A foreign high version must not push legitimate versions out.
		fake, c, wc, foreign := setup(t)
		c.config.MaxVersions = 2
		for _, data := range []string{"d1", "d2"} {
			if err := c.Put(ctx, "default", []byte(data)); err != nil {
				t.Fatal(err)
			}
			c.WaitForRetention()
		}
		mustTag(t, fake, foreign, wc.versionTagFor(100))
		if err := c.Put(ctx, "default", []byte("d3")); err != nil {
			t.Fatal(err)
		}
		c.WaitForRetention()
		for _, v := range []int{1, 2} {
			if _, err := fake.Resolve(ctx, wc.versionTagFor(v)); err != nil {
				t.Fatalf("legitimate version v%d pruned by a foreign cutoff: %v", v, err)
			}
		}
		wantState(t, c, "other", "other state")
		wantState(t, c, "default", "d3")
	})
}

// A corrupted workspace must not block unrelated workspaces; List, which
// reports every name, still fails closed.
func TestWorkspaceForeignIdentityMismatchIsolated(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	c := newTestClient(&orasRepositoryClient{inner: fake})
	desc, err := newWorkspaceClient(c, "impostor").packLockManifest(ctx, "", 0, 0, "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Tag(ctx, desc, newWorkspaceClient(c, "corrupted").stateTag); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "default", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lock(ctx, "default", LockInfo{ID: "old"}); err != nil {
		t.Fatal(err)
	}
	ops := workspaceOperations(c)
	for _, name := range []string{"read", "write", "verify", "unlock", "delete"} {
		if err := ops[name](); err != nil {
			t.Errorf("%s blocked by another workspace's corruption: %v", name, err)
		}
	}
	if _, err := c.List(ctx); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("list = %v", err)
	}
	if _, err := c.Get(ctx, "corrupted"); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("corrupted workspace accessible: %v", err)
	}
}

// countingRepo counts registry calls per method.
type countingRepo struct {
	delegatingRepo
	tags, resolve, fetch atomic.Int64
}

func (r *countingRepo) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	r.resolve.Add(1)
	return r.delegatingRepo.Resolve(ctx, ref)
}

func (r *countingRepo) Fetch(ctx context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	r.fetch.Add(1)
	return r.delegatingRepo.Fetch(ctx, d)
}

func (r *countingRepo) Tags(ctx context.Context, last string, fn func([]string) error) error {
	r.tags.Add(1)
	return r.delegatingRepo.Tags(ctx, last, fn)
}

// The preflight must stay bounded regardless of workspace count and retained
// history: one listing plus Resolve+Fetch of at most three own tags.
func TestWorkspacePreflightCallsBounded(t *testing.T) {
	ctx := context.Background()
	repo := &countingRepo{delegatingRepo: delegatingRepo{inner: newFakeORASRepo()}}
	c := newTestClient(&orasRepositoryClient{inner: repo})
	c.config.MaxVersions = 5
	for i := 0; i < 5; i++ {
		ws := fmt.Sprintf("ws%d", i)
		for w := 0; w < 7; w++ {
			if err := c.Put(ctx, ws, []byte(ws)); err != nil {
				t.Fatal(err)
			}
			c.WaitForRetention()
		}
		if _, err := c.Lock(ctx, ws, LockInfo{ID: ws}); err != nil {
			t.Fatal(err)
		}
	}
	repo.tags.Store(0)
	repo.resolve.Store(0)
	repo.fetch.Store(0)
	if err := newWorkspaceClient(c, "ws0").checkWorkspace(ctx); err != nil {
		t.Fatal(err)
	}
	if tags, resolve, fetch := repo.tags.Load(), repo.resolve.Load(), repo.fetch.Load(); tags != 1 || resolve > 3 || fetch > 3 {
		t.Fatalf("preflight calls: Tags=%d Resolve=%d Fetch=%d; want 1, <=3, <=3", tags, resolve, fetch)
	}
}

func (r *workspaceScanFailure) Tags(context.Context, string, func([]string) error) error {
	return errors.New("registry listing denied")
}

func TestWorkspaceScanFailsClosed(t *testing.T) {
	c := newTestClient(&orasRepositoryClient{inner: &workspaceScanFailure{delegatingRepo{newFakeORASRepo()}}})
	for name, op := range workspaceOperations(c) {
		if err := op(); err == nil || !strings.Contains(err.Error(), "listing denied") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Runs against the same real Zot fixture as the existing integration suite.
func TestZotWorkspaceIsolation(t *testing.T) {
	requireZotTest(t)
	port := freeLocalPort(t)
	startZot(t, port)
	addr := "localhost:" + port
	waitForZot(t, addr)
	c := newZotClient(t, addr, fmt.Sprintf("workspace-isolation-%d", time.Now().UnixNano()), Config{})
	exerciseWorkspaceIsolation(t, c)
	source := newZotClient(t, addr, fmt.Sprintf("workspace-legacy-%d", time.Now().UnixNano()), Config{})
	destination := newZotClient(t, addr, fmt.Sprintf("workspace-migrated-%d", time.Now().UnixNano()), Config{})
	exerciseWorkspaceMigration(t, source, destination)
}

// A legacy literal can look exactly like a new hash. Shape alone is insufficient.
func TestWorkspaceRejectsHashShapedLegacyName(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	c := newTestClient(&orasRepositoryClient{inner: fake})
	literal := workspaceTagFor("default")
	legacy := newWorkspaceClient(c, literal)
	legacy.stateTag = stateTagPrefix + literal
	if err := legacy.put(ctx, []byte("legacy literal state")); err != nil {
		t.Fatal(err)
	}
	for name, op := range workspaceOperations(c) {
		if err := op(); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.List(ctx); err == nil {
		t.Fatal("list accepted a hash-shaped legacy literal")
	}
	data, err := legacy.get(ctx)
	if err != nil || string(data) != "legacy literal state" {
		t.Fatal("legacy literal state changed")
	}
}

func TestWorkspaceRejectsMissingIdentity(t *testing.T) {
	ctx := context.Background()
	fake := newFakeORASRepo()
	c := newTestClient(&orasRepositoryClient{inner: fake})
	desc, _ := newForeignManifest(ctx, t, fake) // no original-name annotation
	tag := newWorkspaceClient(c, "default").stateTag
	if err := fake.Tag(ctx, desc, tag); err != nil {
		t.Fatal(err)
	}
	for name, op := range workspaceOperations(c) {
		if err := op(); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.List(ctx); err == nil {
		t.Fatal("list guessed missing original name")
	}
}

func exerciseWorkspaceMigration(t *testing.T, source, destination *Client) {
	t.Helper()
	ctx := context.Background()
	state := []byte(`{"version":4,"terraform_version":"1.17.0-alpha20260827","serial":7,"lineage":"known-lineage","outputs":{},"resources":[]}`)
	// Seed legacy storage explicitly, without any automatic compatibility path.
	old := newWorkspaceClient(source, "default")
	old.stateTag = "state-default"
	if err := old.put(ctx, state); err != nil {
		t.Fatal(err)
	}
	original, err := source.repoClient.inner.Resolve(ctx, old.stateTag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Get(ctx, "default"); err == nil {
		t.Fatal("new provider accepted legacy repository")
	}
	// Model a verified export using the legacy mapping, then import into an empty
	// repository. This verifies bytes/identity, not Terraform alpha CLI behavior.
	exported, err := old.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.Put(ctx, "default", exported); err != nil {
		t.Fatal(err)
	}
	restored, err := destination.Get(ctx, "default")
	if err != nil || !bytes.Equal(state, restored) {
		t.Fatalf("migration changed payload: %v", err)
	}
	after, err := source.repoClient.inner.Resolve(ctx, old.stateTag)
	if err != nil || after.Digest != original.Digest {
		t.Fatal("source changed during migration")
	}
	// Before any new infrastructure writes, the original source is still usable
	// through the old mapping for rollback.
	rollback, err := old.get(ctx)
	if err != nil || !bytes.Equal(state, rollback) {
		t.Fatal("rollback source lost")
	}
}

func TestWorkspaceMigrationPreservesSource(t *testing.T) {
	source := newTestClient(&orasRepositoryClient{inner: newFakeORASRepo()})
	destination := newTestClient(&orasRepositoryClient{inner: newFakeORASRepo()})
	exerciseWorkspaceMigration(t, source, destination)
}
