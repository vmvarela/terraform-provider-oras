package oras

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	zotTestEnvVar    = "TF_ORAS_ZOT_TEST"
	zotImage         = "ghcr.io/project-zot/zot-linux-amd64:v2.1.0"
	zotContainerPort = "5000"
)

// zotMinimalConfig is a minimal zot configuration used for integration tests.
const zotMinimalConfig = `{"storage":{"rootDirectory":"/tmp/zot"},"http":{"address":"0.0.0.0","port":"5000"},"log":{"level":"error"}}`

// ─── Test helpers ─────────────────────────────────────────────────────────────

// requireZotTest skips the test unless TF_ORAS_ZOT_TEST is set.
func requireZotTest(t *testing.T) {
	t.Helper()
	if os.Getenv(zotTestEnvVar) == "" {
		t.Skipf("set %s to run zot integration tests", zotTestEnvVar)
	}
}

// freeLocalPort returns a free TCP port on localhost.
func freeLocalPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	return port
}

// startZot starts a zot registry container and registers cleanup.
func startZot(t *testing.T, port string) (containerID string) {
	t.Helper()
	configFile := filepath.Join(t.TempDir(), "zot-config.json")
	if err := os.WriteFile(configFile, []byte(zotMinimalConfig), 0644); err != nil {
		t.Fatalf("write zot config: %v", err)
	}

	containerName := fmt.Sprintf("oras-zot-test-%s", port)
	cmd := exec.Command("docker", "run", "-d", "--rm",
		"--name", containerName,
		"-p", port+":"+zotContainerPort,
		"-v", configFile+":/etc/zot/config.json:ro",
		zotImage,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, string(out))
	}
	containerID = strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	return containerID
}

// waitForZot polls the registry base endpoint until it responds 200 OK.
func waitForZot(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s/v2/", addr)
	var lastErr error

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("zot not ready at %s after 15s: %v", addr, lastErr)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return
		}
		lastErr = fmt.Errorf("unexpected status: %d", resp.StatusCode)
		time.Sleep(250 * time.Millisecond)
	}
}

// countZotVersionTags queries the registry tags list and counts entries
// matching the given prefix.
func countZotVersionTags(t *testing.T, addr, repoPath, prefix string) int {
	t.Helper()
	url := fmt.Sprintf("http://%s/v2/%s/tags/list", addr, repoPath)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode tags list: %v", err)
	}

	count := 0
	for _, tag := range result.Tags {
		if strings.HasPrefix(tag, prefix) {
			count++
		}
	}
	return count
}

// newZotClient creates a *Client configured for the local zot test registry.
// Additional opts are merged with the base config.
func newZotClient(t *testing.T, addr, repoPath string, cfg Config) *Client {
	t.Helper()
	base := Config{
		Insecure:    true,
		Compression: "none",
	}
	// Merge provided config over base
	if cfg.HTTPClient != nil {
		base.HTTPClient = cfg.HTTPClient
	}
	if cfg.CAFile != "" {
		base.CAFile = cfg.CAFile
	}
	if cfg.Username != "" || cfg.Password != "" {
		base.Username = cfg.Username
		base.Password = cfg.Password
	}
	if cfg.Token != "" {
		base.Token = cfg.Token
	}
	if cfg.Compression != "" {
		base.Compression = cfg.Compression
	}
	if cfg.LockTTL != 0 {
		base.LockTTL = cfg.LockTTL
	}
	if cfg.MaxVersions != 0 {
		base.MaxVersions = cfg.MaxVersions
	}
	if cfg.MaxStateSize != 0 {
		base.MaxStateSize = cfg.MaxStateSize
	}
	client, err := NewClient(addr, repoPath, base)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// ─── Integration tests ────────────────────────────────────────────────────────

// TestZotIntegration_StateGetPutDelete tests state CRUD lifecycle against a
// live zot registry.
func TestZotIntegration_StateGetPutDelete(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	c := newZotClient(t, addr, "ghoten-test/state", Config{})

	// Get on empty workspace must return nil.
	data, err := c.Get(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("Get on empty: %v", err)
	}
	if data != nil {
		t.Fatal("expected nil on empty workspace")
	}

	// Put state.
	stateData := []byte(`{"version":4,"serial":1}`)
	if err := c.Put(ctx, "ephemeral", stateData); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get back and verify.
	data2, err := c.Get(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if !bytes.Equal(data2, stateData) {
		t.Fatalf("Get returned %q, want %q", string(data2), string(stateData))
	}

	// Delete.
	if err := c.Delete(ctx, "ephemeral"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Get after delete must return nil.
	data3, err := c.Get(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if data3 != nil {
		t.Fatal("expected nil after delete")
	}
}

// TestZotIntegration_LockUnlock tests lock contention and unlock against a
// live zot registry.
func TestZotIntegration_LockUnlock(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	c := newZotClient(t, addr, "ghoten-test/lock", Config{})

	// First lock should succeed.
	id1, err := c.Lock(ctx, "default", LockInfo{
		ID:        "zot-lock-1",
		Operation: "zot-lock-test",
	})
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Concurrent locker with different ID must fail.
	_, err = c.Lock(ctx, "default", LockInfo{
		ID:        "zot-lock-2",
		Operation: "zot-lock-concurrent",
	})
	if err == nil {
		t.Fatal("expected concurrent lock to fail")
	}

	// Release first lock.
	if err := c.Unlock(ctx, "default", id1); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// New lock after unlock must succeed.
	id3, err := c.Lock(ctx, "default", LockInfo{
		ID:        "zot-lock-3",
		Operation: "after-unlock",
	})
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	if err := c.Unlock(ctx, "default", id3); err != nil {
		t.Fatalf("final unlock: %v", err)
	}
}

// TestZotIntegration_LockTTLStaleClearing verifies that a lock with an expired
// TTL is automatically cleared when a new locker arrives.
func TestZotIntegration_LockTTLStaleClearing(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	// Use a short TTL so we can observe expiry.
	c := newZotClient(t, addr, "ghoten-test/lock-ttl", Config{LockTTL: time.Second})

	_, err := c.Lock(ctx, "default", LockInfo{
		ID:        "stale-lock",
		Operation: "stale-lock",
	})
	if err != nil {
		t.Fatalf("initial lock: %v", err)
	}

	// Don't unlock; wait for the lock TTL to expire.
	time.Sleep(2 * time.Second)

	// A new client instance must auto-clear the stale lock.
	c2 := newZotClient(t, addr, "ghoten-test/lock-ttl", Config{LockTTL: time.Second})
	id2, err := c2.Lock(ctx, "default", LockInfo{
		ID:        "after-stale",
		Operation: "after-stale",
	})
	if err != nil {
		t.Fatalf("lock after stale TTL: %v", err)
	}
	if err := c2.Unlock(ctx, "default", id2); err != nil {
		t.Fatalf("final unlock: %v", err)
	}
}

// TestZotIntegration_Retention verifies that async retention enforces the
// configured max version limit against a live zot registry.
func TestZotIntegration_Retention(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	const maxVersions = 3
	c := newZotClient(t, addr, "ghoten-test/retention", Config{MaxVersions: maxVersions})

	// Write more states than maxVersions.
	for i := range maxVersions + 2 {
		state := []byte(fmt.Sprintf(`{"serial":%d}`, i+1))
		if err := c.Put(ctx, "default", state); err != nil {
			t.Fatalf("Put %d: %v", i+1, err)
		}
	}

	// Wait for async retention to complete.
	c.WaitForRetention()

	// Count version tags; must not exceed maxVersions.
	prefix := stateTagPrefix + "default" + stateVersionTagSeparator
	versionTags := countZotVersionTags(t, addr, "ghoten-test/retention", prefix)
	if versionTags > maxVersions {
		t.Errorf("expected ≤%d version tags, found %d", maxVersions, versionTags)
	}
}

// TestZotIntegration_Workspaces verifies multi-workspace List and coexistence
// against a live zot registry.
func TestZotIntegration_Workspaces(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	c := newZotClient(t, addr, "ghoten-test/workspaces", Config{})

	// Write state for three workspaces.
	for _, ws := range []string{"prod", "staging", "dev"} {
		state := []byte(fmt.Sprintf(`{"workspace":"%s"}`, ws))
		if err := c.Put(ctx, ws, state); err != nil {
			t.Fatalf("Put %q: %v", ws, err)
		}
	}

	// List and verify all three are present.
	listed, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := map[string]bool{"prod": false, "staging": false, "dev": false}
	for _, w := range listed {
		if _, ok := want[w]; ok {
			want[w] = true
		}
	}
	for ws, found := range want {
		if !found {
			t.Errorf("workspace %q not returned in List: %v", ws, listed)
		}
	}
}

// TestZotIntegration_Compression verifies gzip compression round-trip against
// a live zot registry.
func TestZotIntegration_Compression(t *testing.T) {
	requireZotTest(t)
	ctx := context.Background()
	port := freeLocalPort(t)
	addr := "localhost:" + port
	startZot(t, port)
	waitForZot(t, addr)

	c := newZotClient(t, addr, "ghoten-test/compression", Config{Compression: "gzip"})

	original := bytes.Repeat([]byte("hello-terraform-state"), 100)
	if err := c.Put(ctx, "default", original); err != nil {
		t.Fatalf("Put with compression: %v", err)
	}

	got, err := c.Get(ctx, "default")
	if err != nil {
		t.Fatalf("Get with compression: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Fatal("gzip round-trip mismatch")
	}
}
