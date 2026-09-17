// Package statestore implements the OCI state store for the Terraform plugin framework.
package statestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
)

func TestParseOCIURL(t *testing.T) {
	tests := []struct {
		name       string
		rawURL     string
		wantReg    string
		wantRepo   string
		wantErr    bool
		wantErrSub string // substring expected in error message
	}{
		// Valid URLs.
		{
			name:     "simple registry and repo",
			rawURL:   "oci://ghcr.io/myorg/infra-state",
			wantReg:  "ghcr.io",
			wantRepo: "myorg/infra-state",
		},
		{
			name:     "single path segment",
			rawURL:   "oci://registry.example.com/myrepo",
			wantReg:  "registry.example.com",
			wantRepo: "myrepo",
		},
		{
			name:     "registry with port",
			rawURL:   "oci://registry.example.com:5000/myrepo",
			wantReg:  "registry.example.com:5000",
			wantRepo: "myrepo",
		},
		{
			name:     "uppercase host passes through",
			rawURL:   "oci://GHCR.IO/myorg/state",
			wantReg:  "GHCR.IO",
			wantRepo: "myorg/state",
		},
		{
			name:     "deeply nested repo path",
			rawURL:   "oci://ghcr.io/org/team/app/state",
			wantReg:  "ghcr.io",
			wantRepo: "org/team/app/state",
		},
		{
			name:     "no reference tag",
			rawURL:   "oci://ghcr.io/myorg/state",
			wantReg:  "ghcr.io",
			wantRepo: "myorg/state",
		},
		// Invalid URLs.
		{
			name:       "missing oci scheme",
			rawURL:     "https://ghcr.io/myorg/state",
			wantErr:    true,
			wantErrSub: "must start with 'oci://'",
		},
		{
			name:       "empty string",
			rawURL:     "",
			wantErr:    true,
			wantErrSub: "must start with 'oci://'",
		},
		{
			name:       "scheme only",
			rawURL:     "oci://",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "registry host only",
			rawURL:     "oci://ghcr.io",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "empty host",
			rawURL:     "oci:///myorg/state",
			wantErr:    true,
			wantErrSub: "missing the registry host",
		},
		{
			name:       "empty repository with trailing slash",
			rawURL:     "oci://ghcr.io/",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "path traversal with dotdot",
			rawURL:     "oci://ghcr.io/myorg/../../etc",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "dotdot inside segment",
			rawURL:     "oci://ghcr.io/myorg/secret..backup",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "leading dotdot escapes repo root",
			rawURL:     "oci://ghcr.io/../other",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "double slash mid repo",
			rawURL:     "oci://ghcr.io/myorg//state",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		// Double slash directly after the host yields repository "/state" —
		// caught by the leading-slash check, not the "//" check.
		{
			name:       "double slash after host",
			rawURL:     "oci://ghcr.io//state",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "trailing slash",
			rawURL:     "oci://ghcr.io/myorg/state/",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "double slash at repo end",
			rawURL:     "oci://ghcr.io/myorg/state//",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, repo, err := parseOCIURL(tt.rawURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseOCIURL(%q) = (%q, %q, nil), want error", tt.rawURL, reg, repo)
				}
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("parseOCIURL(%q) error = %q, want substring %q", tt.rawURL, err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOCIURL(%q) unexpected error: %v", tt.rawURL, err)
			}
			if reg != tt.wantReg {
				t.Errorf("parseOCIURL(%q) registry = %q, want %q", tt.rawURL, reg, tt.wantReg)
			}
			if repo != tt.wantRepo {
				t.Errorf("parseOCIURL(%q) repository = %q, want %q", tt.rawURL, repo, tt.wantRepo)
			}
		})
	}
}

// ─── tfsdk.Config helpers ─────────────────────────────────────────────────────

// strVal/int64Val/unknownStr build raw tftypes values matching the schema's
// Terraform types (String → tftypes.String, Int64 → tftypes.Number).
func strVal(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }
func int64Val(n int64) tftypes.Value {
	return tftypes.NewValue(tftypes.Number, big.NewFloat(float64(n)))
}
func unknownStr() tftypes.Value { return tftypes.NewValue(tftypes.String, tftypes.UnknownValue) }

// storeRaw builds a complete object value for the store schema; attributes
// not in overrides are null.
func storeRaw(schemaResp *fwss.SchemaResponse, overrides map[string]tftypes.Value) tftypes.Value {
	ctx := context.Background()
	attrTypes := map[string]tftypes.Type{}
	vals := map[string]tftypes.Value{}
	for name, attr := range schemaResp.Schema.GetAttributes() {
		t := attr.GetType().TerraformType(ctx)
		attrTypes[name] = t
		vals[name] = tftypes.NewValue(t, nil)
	}
	for k, v := range overrides {
		vals[k] = v
	}
	return tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, vals)
}

func storeConfig(overrides map[string]tftypes.Value) tfsdk.Config {
	s := &OCIStateStore{}
	schemaResp := &fwss.SchemaResponse{}
	s.Schema(context.Background(), fwss.SchemaRequest{}, schemaResp)
	return tfsdk.Config{Schema: schemaResp.Schema, Raw: storeRaw(schemaResp, overrides)}
}

// ─── Metadata / Schema ────────────────────────────────────────────────────────

func TestStateStoreMetadata(t *testing.T) {
	resp := &fwss.MetadataResponse{}
	(&OCIStateStore{}).Metadata(context.Background(), fwss.MetadataRequest{ProviderTypeName: "oras"}, resp)
	if resp.TypeName != "oras_oci" {
		t.Errorf("TypeName = %q, want %q", resp.TypeName, "oras_oci")
	}
}

func TestStateStoreSchema(t *testing.T) {
	resp := &fwss.SchemaResponse{}
	(&OCIStateStore{}).Schema(context.Background(), fwss.SchemaRequest{}, resp)
	for _, attr := range []string{"url", "compression", "lock_ttl", "max_versions", "max_state_size"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["url"].IsRequired() {
		t.Error("url should be required")
	}
}

// ─── ValidateConfig ───────────────────────────────────────────────────────────

func TestStateStoreValidateConfig(t *testing.T) {
	tests := []struct {
		name       string
		overrides  map[string]tftypes.Value
		wantErrSub string // empty = expect no error diagnostics
	}{
		{
			name:      "happy minimal",
			overrides: map[string]tftypes.Value{"url": strVal("oci://ghcr.io/org/repo")},
		},
		{
			name: "happy full",
			overrides: map[string]tftypes.Value{
				"url":            strVal("oci://registry.example.com:5000/repo"),
				"lock_ttl":       strVal("0s"),
				"max_versions":   int64Val(5),
				"max_state_size": int64Val(1024),
				"compression":    tftypes.NewValue(tftypes.Bool, true),
			},
		},
		{
			name:      "unknown url skipped",
			overrides: map[string]tftypes.Value{"url": unknownStr()},
		},
		{
			name:       "invalid url",
			overrides:  map[string]tftypes.Value{"url": strVal("https://ghcr.io/org/repo")},
			wantErrSub: "must start with 'oci://'",
		},
		{
			name:       "invalid lock_ttl",
			overrides:  map[string]tftypes.Value{"url": strVal("oci://ghcr.io/org/repo"), "lock_ttl": strVal("banana")},
			wantErrSub: "not a valid duration",
		},
		{
			name:       "negative lock_ttl",
			overrides:  map[string]tftypes.Value{"url": strVal("oci://ghcr.io/org/repo"), "lock_ttl": strVal("-15m")},
			wantErrSub: "negative",
		},
		{
			name:       "negative max_versions",
			overrides:  map[string]tftypes.Value{"url": strVal("oci://ghcr.io/org/repo"), "max_versions": int64Val(-1)},
			wantErrSub: "max_versions must be 0",
		},
		{
			name:       "negative max_state_size",
			overrides:  map[string]tftypes.Value{"url": strVal("oci://ghcr.io/org/repo"), "max_state_size": int64Val(-5)},
			wantErrSub: "max_state_size must be 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &fwss.ValidateConfigResponse{}
			(&OCIStateStore{}).ValidateConfig(context.Background(), fwss.ValidateConfigRequest{Config: storeConfig(tt.overrides)}, resp)
			if tt.wantErrSub == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
				}
				return
			}
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected error diagnostic containing %q, got %v", tt.wantErrSub, resp.Diagnostics)
			}
			if !strings.Contains(resp.Diagnostics[0].Detail(), tt.wantErrSub) {
				t.Errorf("detail = %q, want substring %q", resp.Diagnostics[0].Detail(), tt.wantErrSub)
			}
		})
	}
}

// ─── Initialize ───────────────────────────────────────────────────────────────

func TestStateStoreInitialize(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		resp := &fwss.InitializeResponse{}
		(&OCIStateStore{}).Initialize(context.Background(), fwss.InitializeRequest{
			Config: storeConfig(map[string]tftypes.Value{
				"url":          strVal("oci://ghcr.io/org/repo"),
				"lock_ttl":     strVal("15m"),
				"max_versions": int64Val(3),
			}),
			ProviderData: &ProviderData{Insecure: false},
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.StateStoreData == nil {
			t.Fatal("StateStoreData is nil")
		}
		ssd, ok := resp.StateStoreData.(*stateStoreData)
		if !ok {
			t.Fatalf("StateStoreData is %T, want *stateStoreData", resp.StateStoreData)
		}
		if ssd.client == nil {
			t.Fatal("stateStoreData.client is nil")
		}
	})

	tests := []struct {
		name       string
		overrides  map[string]tftypes.Value
		wantErrSub string
	}{
		{name: "invalid url", overrides: map[string]tftypes.Value{"url": strVal("oci://")}, wantErrSub: "missing the repository path"},
		{name: "null url", overrides: nil, wantErrSub: "url must be set"},
		{name: "invalid lock_ttl", overrides: map[string]tftypes.Value{"url": strVal("oci://ghcr.io/o/r"), "lock_ttl": strVal("banana")}, wantErrSub: "not a valid duration"},
		{name: "negative lock_ttl", overrides: map[string]tftypes.Value{"url": strVal("oci://ghcr.io/o/r"), "lock_ttl": strVal("-1h")}, wantErrSub: "negative"},
		{name: "negative max_versions", overrides: map[string]tftypes.Value{"url": strVal("oci://ghcr.io/o/r"), "max_versions": int64Val(-2)}, wantErrSub: "max_versions must be 0"},
		{name: "negative max_state_size", overrides: map[string]tftypes.Value{"url": strVal("oci://ghcr.io/o/r"), "max_state_size": int64Val(-1)}, wantErrSub: "max_state_size must be 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &fwss.InitializeResponse{}
			(&OCIStateStore{}).Initialize(context.Background(), fwss.InitializeRequest{Config: storeConfig(tt.overrides)}, resp)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected error diagnostic containing %q, got %v", tt.wantErrSub, resp.Diagnostics)
			}
			if !strings.Contains(resp.Diagnostics[0].Detail(), tt.wantErrSub) {
				t.Errorf("detail = %q, want substring %q", resp.Diagnostics[0].Detail(), tt.wantErrSub)
			}
			if resp.StateStoreData != nil {
				t.Error("StateStoreData must be nil on validation failure")
			}
		})
	}
}

// ─── Configure ────────────────────────────────────────────────────────────────

func TestStateStoreConfigure(t *testing.T) {
	ctx := context.Background()

	t.Run("nil data returns silently", func(t *testing.T) {
		// The framework calls Configure with nil data during offline
		// validation, before ValidateConfig: this must not diagnose.
		resp := &fwss.ConfigureResponse{}
		(&OCIStateStore{}).Configure(ctx, fwss.ConfigureRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("nil StateStoreData must return without diagnostics, got %v", resp.Diagnostics)
		}
	})

	t.Run("wrong type produces diagnostic", func(t *testing.T) {
		resp := &fwss.ConfigureResponse{}
		(&OCIStateStore{}).Configure(ctx, fwss.ConfigureRequest{StateStoreData: "nope"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected diagnostic for wrong StateStoreData type, got none")
		}
		if !strings.Contains(resp.Diagnostics[0].Detail(), "Initialize") {
			t.Errorf("detail = %q, want it to mention Initialize", resp.Diagnostics[0].Detail())
		}
	})

	t.Run("restores shared pointer", func(t *testing.T) {
		_, _, ssd := newTestStore(t)
		a := &OCIStateStore{}
		b := &OCIStateStore{}
		for _, s := range []*OCIStateStore{a, b} {
			resp := &fwss.ConfigureResponse{}
			s.Configure(ctx, fwss.ConfigureRequest{StateStoreData: ssd}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("configure: %v", resp.Diagnostics)
			}
		}
		if a.shared != ssd || b.shared != ssd {
			t.Error("Configure did not restore the shared state pointer on new instances")
		}
	})

	t.Run("direct client is wrapped", func(t *testing.T) {
		client, err := oras.NewClient("registry.example.com", "org/repo", oras.Config{})
		if err != nil {
			t.Fatalf("oras.NewClient: %v", err)
		}
		s := &OCIStateStore{}
		resp := &fwss.ConfigureResponse{}
		s.Configure(ctx, fwss.ConfigureRequest{StateStoreData: client}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("configure: %v", resp.Diagnostics)
		}
		if s.shared == nil || s.shared.client != client || s.client != client {
			t.Error("direct *oras.Client was not wrapped into shared state")
		}
	})

	t.Run("correct type sets client", func(t *testing.T) {
		s, _, _ := newTestStore(t)
		if s.client == nil {
			t.Fatal("client not set after Configure")
		}
	})
}

// ─── fakeOCIRegistry: minimal in-memory OCI registry over plain HTTP ─────────

type fakeOCIRegistry struct {
	mu       sync.Mutex
	manifest map[string][]byte // digest → manifest bytes
	tagOf    map[string]string // tag → digest
	blobs    map[string][]byte // digest → blob bytes
	uploads  int64
}

func newFakeOCIRegistry() *fakeOCIRegistry {
	return &fakeOCIRegistry{
		manifest: map[string][]byte{},
		tagOf:    map[string]string{},
		blobs:    map[string][]byte{},
	}
}

func (f *fakeOCIRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case strings.HasSuffix(rest, "/tags/list"):
		tags := make([]string, 0, len(f.tagOf))
		for tag := range f.tagOf {
			tags = append(tags, tag)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": strings.TrimSuffix(rest, "/tags/list"), "tags": tags})
	case strings.Contains(rest, "/manifests/"):
		f.serveManifest(w, r, rest)
	case strings.Contains(rest, "/blobs/uploads"):
		f.serveBlobUpload(w, r, rest)
	case strings.Contains(rest, "/blobs/"):
		f.serveBlob(w, r, rest)
	default:
		// Unknown probes (e.g. referrers discovery) → not found.
		w.WriteHeader(http.StatusNotFound)
	}
}

// TagManifest installs raw bytes under a tag, bypassing the normal push flow.
// Used to simulate a rival lock holder.
func (f *fakeOCIRegistry) TagManifest(tag string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dgst := "sha256:" + hex.EncodeToString(hashBytes(body))
	f.manifest[dgst] = body
	f.tagOf[tag] = dgst
}

func (f *fakeOCIRegistry) HasTag(tag string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tagOf[tag]
	return ok
}

func hashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func (f *fakeOCIRegistry) serveManifest(w http.ResponseWriter, r *http.Request, rest string) {
	ref := rest[strings.Index(rest, "/manifests/")+len("/manifests/"):]
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		dgst := "sha256:" + hex.EncodeToString(hashBytes(body))
		f.manifest[dgst] = body
		if !strings.HasPrefix(ref, "sha256:") {
			f.tagOf[ref] = dgst
		}
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead, http.MethodGet:
		dgst := ref
		if !strings.HasPrefix(ref, "sha256:") {
			dgst = f.tagOf[ref]
		}
		body, ok := f.manifest[dgst]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", dgst)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case http.MethodDelete:
		dgst := ref
		if !strings.HasPrefix(ref, "sha256:") {
			dgst = f.tagOf[ref]
		}
		delete(f.manifest, dgst)
		for tag, d := range f.tagOf {
			if d == dgst {
				delete(f.tagOf, tag)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeOCIRegistry) serveBlobUpload(w http.ResponseWriter, r *http.Request, rest string) {
	switch r.Method {
	case http.MethodPost:
		// Monolithic POST (?digest=) or start of a two-step upload.
		if d := r.URL.Query().Get("digest"); d != "" {
			body, _ := io.ReadAll(r.Body)
			f.blobs[d] = body
			w.WriteHeader(http.StatusCreated)
			return
		}
		f.uploads++
		repo := rest[:strings.Index(rest, "/blobs/uploads")]
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/upload-%d", repo, f.uploads))
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		d := r.URL.Query().Get("digest")
		body, _ := io.ReadAll(r.Body)
		f.blobs[d] = body
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeOCIRegistry) serveBlob(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dgst := rest[strings.Index(rest, "/blobs/")+len("/blobs/"):]
	body, ok := f.blobs[dgst]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// newTestStore runs the real Initialize→Configure flow against an in-memory
// fake registry over plain HTTP and returns the configured store, the
// registry, and the shared state pointer.
func newTestStore(t *testing.T) (*OCIStateStore, *fakeOCIRegistry, *stateStoreData) {
	t.Helper()
	ctx := context.Background()
	reg := newFakeOCIRegistry()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	initResp := &fwss.InitializeResponse{}
	(&OCIStateStore{}).Initialize(ctx, fwss.InitializeRequest{
		Config: storeConfig(map[string]tftypes.Value{
			"url": strVal("oci://" + srv.Listener.Addr().String() + "/test/repo"),
		}),
		ProviderData: &ProviderData{Insecure: true},
	}, initResp)
	if initResp.Diagnostics.HasError() {
		t.Fatalf("initialize: %v", initResp.Diagnostics)
	}
	ssd, ok := initResp.StateStoreData.(*stateStoreData)
	if !ok {
		t.Fatalf("StateStoreData is %T, want *stateStoreData", initResp.StateStoreData)
	}

	configure := func() *OCIStateStore {
		s := &OCIStateStore{}
		cfgResp := &fwss.ConfigureResponse{}
		s.Configure(ctx, fwss.ConfigureRequest{StateStoreData: ssd}, cfgResp)
		if cfgResp.Diagnostics.HasError() {
			t.Fatalf("configure: %v", cfgResp.Diagnostics)
		}
		return s
	}
	return configure(), reg, ssd
}

// rivalLockManifest builds a lock manifest body owned by holderID, as a
// rival client would publish on the lock tag.
func rivalLockManifest(t *testing.T, holderID string) []byte {
	t.Helper()
	info, err := json.Marshal(oras.LockInfo{ID: holderID, Operation: "plan", Who: "rival", Created: time.Now()})
	if err != nil {
		t.Fatalf("marshal rival info: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"artifactType": "application/vnd.terraform.lock.v1",
		"annotations": map[string]string{
			"org.terraform.workspace":       "default",
			"org.terraform.lock.id":         holderID,
			"org.terraform.lock.info":       string(info),
			"org.terraform.lock.generation": `{"generation":42,"holder_id":"` + holderID + `"}`,
		},
	})
	if err != nil {
		t.Fatalf("marshal rival manifest: %v", err)
	}
	return body
}

// ─── Lock / Unlock / Write lifecycle ──────────────────────────────────────────

// testLockTag is lockTagPrefix + workspaceTagFor("default") from internal/oras.
const testWorkspaceTag = "37a8eec1ce19687d132fe29051dca629d164e2c4958ba141d5f4133a33f0688f"
const testLockTag = "locked-" + testWorkspaceTag
const testStateTag = "state-" + testWorkspaceTag
const testVersionTagPrefix = "stver-" + testWorkspaceTag + "-v"

// testNewInstance configures an additional per-RPC instance on the same
// shared state, as Terraform does for every RPC.
func testNewInstance(t *testing.T, ssd *stateStoreData) *OCIStateStore {
	t.Helper()
	s := &OCIStateStore{}
	resp := &fwss.ConfigureResponse{}
	s.Configure(context.Background(), fwss.ConfigureRequest{StateStoreData: ssd}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", resp.Diagnostics)
	}
	return s
}

func TestStateStoreLockUnlockWriteLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _, ssd := newTestStore(t)

	// Terraform creates a fresh instance per RPC: lock on one, write on
	// another, unlock on a third — all sharing the same registry.
	locker := testNewInstance(t, ssd)
	writer := testNewInstance(t, ssd)
	unlocker := testNewInstance(t, ssd)

	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}
	if lockResp.LockID == "" {
		t.Fatal("expected non-empty LockID")
	}
	if ssd.lockIDs["default"] != lockResp.LockID {
		t.Errorf("shared lock registry = %v, want lock recorded", ssd.lockIDs)
	}

	// Write from a DIFFERENT instance with the shared lock held → success.
	writeResp := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("state-1")}, writeResp)
	if writeResp.Diagnostics.HasError() {
		t.Fatalf("write: %v", writeResp.Diagnostics)
	}

	readResp := &fwss.ReadResponse{}
	s.Read(ctx, fwss.ReadRequest{StateID: "default"}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if string(readResp.StateBytes) != "state-1" {
		t.Errorf("state = %q, want %q", readResp.StateBytes, "state-1")
	}

	// Unlock from a third instance → success and registration dropped.
	unlockResp := &fwss.UnlockResponse{}
	unlocker.Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: lockResp.LockID}, unlockResp)
	if unlockResp.Diagnostics.HasError() {
		t.Fatalf("unlock: %v", unlockResp.Diagnostics)
	}
	if len(ssd.lockIDs) != 0 {
		t.Errorf("shared lock registry not cleaned after unlock: %v", ssd.lockIDs)
	}

	// Write without a registered local lock (-lock=false path) → still succeeds.
	writeResp2 := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("state-2")}, writeResp2)
	if writeResp2.Diagnostics.HasError() {
		t.Fatalf("write without local lock: %v", writeResp2.Diagnostics)
	}
}

func TestStateStoreWriteRefusedWhenLockLost(t *testing.T) {
	ctx := context.Background()
	_, reg, ssd := newTestStore(t)

	locker := testNewInstance(t, ssd)
	writer := testNewInstance(t, ssd)

	lockResp := &fwss.LockResponse{}
	locker.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	// A rival takes over the lock tag after we acquired our lock.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	writeResp := &fwss.WriteResponse{}
	writer.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("hijacked")}, writeResp)
	if !writeResp.Diagnostics.HasError() {
		t.Fatal("expected write to be refused after lock loss, got no diagnostics")
	}
	if !strings.Contains(writeResp.Diagnostics[0].Summary(), "no longer held") {
		t.Errorf("summary = %q, want it to mention the lost lock", writeResp.Diagnostics[0].Summary())
	}

	// Nothing may have been written: the state tag must not exist.
	if reg.HasTag(testStateTag) {
		t.Error("state was written despite lost lock")
	}
}

func TestStateStoreLockContention(t *testing.T) {
	ctx := context.Background()
	s, reg, ssd := newTestStore(t)

	// A rival already holds the lock.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	lockResp := &fwss.LockResponse{}
	s.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if !lockResp.Diagnostics.HasError() {
		t.Fatal("expected lock contention diagnostic, got none")
	}
	if !strings.Contains(lockResp.Diagnostics[0].Detail(), "rival") {
		t.Errorf("detail = %q, want it to mention the holder", lockResp.Diagnostics[0].Detail())
	}
	// Failed lock must not register anything.
	if len(ssd.lockIDs) != 0 {
		t.Errorf("failed lock registered: %v", ssd.lockIDs)
	}
}

func TestStateStoreUnlockEmptyLockID(t *testing.T) {
	ctx := context.Background()
	s, reg, _ := newTestStore(t)

	// A lock exists (held by anyone); an empty LockID must not release it.
	reg.TagManifest(testLockTag, rivalLockManifest(t, "rival"))

	unlockResp := &fwss.UnlockResponse{}
	s.Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: ""}, unlockResp)
	if !unlockResp.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for empty LockID, got none")
	}
	if !reg.HasTag(testLockTag) {
		t.Error("lock tag was removed by an empty-LockID unlock")
	}
}

func TestStateStoreUnlockWrongLockID(t *testing.T) {
	ctx := context.Background()
	s, _, ssd := newTestStore(t)

	lockResp := &fwss.LockResponse{}
	s.Lock(ctx, fwss.LockRequest{StateID: "default", Operation: "apply"}, lockResp)
	if lockResp.Diagnostics.HasError() {
		t.Fatalf("lock: %v", lockResp.Diagnostics)
	}

	unlockResp := &fwss.UnlockResponse{}
	s.Unlock(ctx, fwss.UnlockRequest{StateID: "default", LockID: "wrong-id"}, unlockResp)
	if !unlockResp.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong LockID, got none")
	}
	// On error the registration must be preserved.
	if ssd.lockIDs["default"] != lockResp.LockID {
		t.Errorf("shared lock registry after failed unlock = %v, want lock preserved", ssd.lockIDs)
	}
}

// TestStateStoreForgetLockIfKeepsNewerAcquisition: a successful unlock must
// not erase the registration of a newer acquisition for the same StateID.
func TestStateStoreForgetLockIfKeepsNewerAcquisition(t *testing.T) {
	ssd := &stateStoreData{lockIDs: map[string]string{}}

	ssd.registerLock("default", "lock-v1")
	ssd.registerLock("default", "lock-v2")

	// Stale unlock for lock-v1 must not drop lock-v2.
	ssd.forgetLockIf("default", "lock-v1")
	if got := ssd.lockIDs["default"]; got != "lock-v2" {
		t.Fatalf("lock registry = %q, want newer acquisition %q preserved", got, "lock-v2")
	}

	// Matching unlock drops the entry.
	ssd.forgetLockIf("default", "lock-v2")
	if _, still := ssd.lockFor("default"); still {
		t.Error("lock registration not removed by matching unlock")
	}

	// Unlock for an unregistered StateID is a no-op.
	ssd.forgetLockIf("other", "whatever")
}

// TestStateStoreDataConcurrentAccess: register/lookup/forget must be safe
// under concurrent RPCs (race detector covers this test).
func TestStateStoreDataConcurrentAccess(t *testing.T) {
	ssd := &stateStoreData{}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		stateID := fmt.Sprintf("ws-%d", i)
		wg.Add(3)
		go func() {
			defer wg.Done()
			ssd.registerLock(stateID, "lock-"+stateID)
		}()
		go func() {
			defer wg.Done()
			if lockID, ok := ssd.lockFor(stateID); ok && lockID != "lock-"+stateID {
				t.Errorf("unexpected lock value for %q: %q", stateID, lockID)
			}
		}()
		go func() {
			defer wg.Done()
			ssd.forgetLockIf(stateID, "lock-"+stateID)
		}()
	}
	wg.Wait()
}
