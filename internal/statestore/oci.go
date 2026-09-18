// Package statestore implements the OCI state store for the Terraform plugin framework.
// It bridges the terraform-plugin-framework statestore.StateStore interface with the
// ORAS client in internal/oras, enabling Terraform to persist tfstate in OCI registries.
package statestore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	ssschema "github.com/hashicorp/terraform-plugin-framework/statestore/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
)

// ProviderData holds provider-level configuration forwarded to state stores
// via ConfigureResponse.StateStoreData.
type ProviderData struct {
	PlainHTTP  bool
	HTTPClient *http.Client
}

// Compile-time interface checks.
var (
	_ fwss.StateStore                   = (*OCIStateStore)(nil)
	_ fwss.StateStoreWithConfigure      = (*OCIStateStore)(nil)
	_ fwss.StateStoreWithValidateConfig = (*OCIStateStore)(nil)
)

// OCIStateStore implements fwss.StateStore using OCI registries via the ORAS protocol.
//
// Terraform creates a fresh OCIStateStore per RPC, so mutable state (the lock
// registry) must NOT live on the struct: it travels via StateStoreData.
type OCIStateStore struct {
	// shared carries the *oras.Client and the lock registry created in
	// Initialize and restored in Configure on every new instance.
	shared *stateStoreData

	// client is shorthand for shared.client, set by Configure.
	client *oras.Client
}

// stateStoreData is the payload Initialize places in
// InitializeResponse.StateStoreData and every per-RPC OCIStateStore instance
// restores in Configure. It owns the lock registry so all instances of the
// same store configuration share ownership tracking.
type stateStoreData struct {
	client *oras.Client

	mu      sync.Mutex
	lockIDs map[string]string // StateID → lock ID acquired by this configuration
}

// registerLock records a lock acquired under this store configuration.
func (d *stateStoreData) registerLock(stateID, lockID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lockIDs == nil {
		d.lockIDs = make(map[string]string)
	}
	d.lockIDs[stateID] = lockID
}

// lockFor returns the lock ID registered for stateID, if any.
func (d *stateStoreData) lockFor(stateID string) (string, bool) {
	// Read must hold the mutex: registerLock/forgetLockIf write concurrently
	// (the framework may run RPCs in parallel).
	d.mu.Lock()
	defer d.mu.Unlock()
	lockID, ok := d.lockIDs[stateID]
	return lockID, ok
}

// forgetLockIf removes the registration for stateID only if it still holds
// lockID: a newer acquisition must survive a stale unlock.
func (d *stateStoreData) forgetLockIf(stateID, lockID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lockIDs[stateID] == lockID {
		delete(d.lockIDs, stateID)
	}
}

// New returns a factory function for OCIStateStore, suitable for use in
// provider.ProviderWithStateStores.StateStores.
func New() func() fwss.StateStore {
	return func() fwss.StateStore {
		return &OCIStateStore{}
	}
}

// storeModel is the HCL schema model for the state_store "oras_oci" block.
type storeModel struct {
	URL          types.String `tfsdk:"url"`
	Compression  types.Bool   `tfsdk:"compression"`
	LockTTL      types.String `tfsdk:"lock_ttl"`
	MaxVersions  types.Int64  `tfsdk:"max_versions"`
	MaxStateSize types.Int64  `tfsdk:"max_state_size"`
}

// ─── StateStore required methods ─────────────────────────────────────────────

// Metadata sets the state store type name.
func (s *OCIStateStore) Metadata(_ context.Context, req fwss.MetadataRequest, resp *fwss.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_oci"
}

// Schema declares the HCL attributes for the state_store block.
func (s *OCIStateStore) Schema(_ context.Context, _ fwss.SchemaRequest, resp *fwss.SchemaResponse) {
	resp.Schema = ssschema.Schema{
		MarkdownDescription: "Stores Terraform state in an OCI registry using the ORAS protocol.",
		Attributes: map[string]ssschema.Attribute{
			"url": ssschema.StringAttribute{
				Required:            true,
				MarkdownDescription: "OCI registry URL in the format `oci://registry/repository`.",
			},
			"compression": ssschema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Enable gzip compression for state data. Defaults to `false`.",
			},
			"lock_ttl": ssschema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Non-renewing state lock lease duration (e.g., `15m`, `1h`), measured using local wall clocks. An apply that outlasts a positive TTL can change infrastructure and then fail to save state. Expired locks are cleared only by a subsequent eligible Lock call; there is no background renewal or reaper. Defaults to unset/`0`: locks never expire and crashed holders require manual recovery. See the state recovery guide before retrying a failed write.",
			},
			"max_versions": ssschema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Maximum number of state versions to retain. When exceeded, the oldest versions are pruned. Defaults to `0` (versioning disabled: no version tags, no allocation, no pruning).",
			},
			"max_state_size": ssschema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Maximum allowed state size in bytes. Defaults to 256 MiB.",
			},
		},
	}
}

// validateStoreModel validates the known values of the state store model,
// appending attribute diagnostics for invalid values. Unknown values are
// skipped (Terraform cannot evaluate them yet); framework-level requiredness
// (e.g. a null url) is left to the schema. It returns the parsed
// registry/repository, empty when the URL is not a known valid value.
// Shared by ValidateConfig (offline validation) and Initialize (defensive
// re-check — ValidateConfig may not have run, e.g. for API clients).
func validateStoreModel(cfg *storeModel, diags *diag.Diagnostics) (registry, repository string) {
	if cfg.URL.IsNull() || cfg.URL.IsUnknown() {
		return "", ""
	}
	registry, repository, err := parseOCIURL(cfg.URL.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("url"), "Invalid OCI URL", err.Error())
		return "", ""
	}

	if !cfg.LockTTL.IsNull() && !cfg.LockTTL.IsUnknown() {
		ttl, parseErr := time.ParseDuration(cfg.LockTTL.ValueString())
		switch {
		case parseErr != nil:
			diags.AddAttributeError(
				path.Root("lock_ttl"),
				"Invalid Lock TTL",
				fmt.Sprintf("lock_ttl %q is not a valid duration; expected a Go duration string such as \"15m\" or \"1h\".", cfg.LockTTL.ValueString()),
			)
		case ttl < 0:
			diags.AddAttributeError(
				path.Root("lock_ttl"),
				"Invalid Lock TTL",
				fmt.Sprintf("lock_ttl %q is negative; use 0 to disable lock expiration or a positive duration such as \"15m\".", cfg.LockTTL.ValueString()),
			)
		}
	}

	if !cfg.MaxVersions.IsNull() && !cfg.MaxVersions.IsUnknown() {
		if v := cfg.MaxVersions.ValueInt64(); v < 0 {
			diags.AddAttributeError(
				path.Root("max_versions"),
				"Invalid Max Versions",
				fmt.Sprintf("max_versions must be 0 (versioning disabled) or greater, got %d.", v),
			)
		}
	}

	if !cfg.MaxStateSize.IsNull() && !cfg.MaxStateSize.IsUnknown() {
		if v := cfg.MaxStateSize.ValueInt64(); v < 0 {
			diags.AddAttributeError(
				path.Root("max_state_size"),
				"Invalid Max State Size",
				fmt.Sprintf("max_state_size must be 0 (keep the 256 MiB default) or greater, got %d bytes.", v),
			)
		}
	}

	return registry, repository
}

// ValidateConfig performs offline validation of the state_store block before
// any network access: the URL must parse as oci://registry/repository,
// lock_ttl must be a non-negative duration (0 disables expiration),
// max_versions must be >= 0 (0 disables versioning) and max_state_size must
// be >= 0 (0 keeps the 256 MiB default).
func (s *OCIStateStore) ValidateConfig(ctx context.Context, req fwss.ValidateConfigRequest, resp *fwss.ValidateConfigResponse) {
	var cfg storeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateStoreModel(&cfg, &resp.Diagnostics)
}

// Initialize parses the configuration, creates the ORAS client, and stores it
// in InitializeResponse.StateStoreData for later retrieval via Configure.
//
// Data flow:
//
//	provider.ConfigureResponse.StateStoreData (*provider.ProviderData)
//	  → Initialize receives it as req.ProviderData
//	  → Initialize sets resp.StateStoreData = *oras.Client
//	  → Configure receives it as req.StateStoreData
func (s *OCIStateStore) Initialize(ctx context.Context, req fwss.InitializeRequest, resp *fwss.InitializeResponse) {
	var cfg storeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Defensive re-validation: identical rules as ValidateConfig, so
	// Initialize never builds a client from values validation would reject.
	registry, repository := validateStoreModel(&cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if registry == "" {
		// URL is required; ValidateConfig normally catches this.
		resp.Diagnostics.AddAttributeError(
			path.Root("url"),
			"Missing OCI URL",
			"url must be set to an OCI URL in the format oci://registry/repository.",
		)
		return
	}

	var orasCfg oras.Config

	// Forward provider-level transport and TLS settings to the ORAS client.
	if pd, ok := req.ProviderData.(*ProviderData); ok && pd != nil {
		orasCfg.HTTPClient = pd.HTTPClient
		// PlainHTTP sets repo.PlainHTTP, required for http:// registries
		// (e.g. local Zot). Must be passed even when a custom HTTPClient exists.
		orasCfg.PlainHTTP = pd.PlainHTTP
	}

	if !cfg.Compression.IsNull() && !cfg.Compression.IsUnknown() {
		orasCfg.Compression = cfg.Compression.ValueBool()
	}

	if !cfg.LockTTL.IsNull() && !cfg.LockTTL.IsUnknown() {
		// Already validated; parse cannot fail here.
		ttl, err := time.ParseDuration(cfg.LockTTL.ValueString())
		if err == nil {
			orasCfg.LockTTL = ttl
		}
	}

	if !cfg.MaxVersions.IsNull() && !cfg.MaxVersions.IsUnknown() {
		orasCfg.MaxVersions = int(cfg.MaxVersions.ValueInt64())
	}

	if !cfg.MaxStateSize.IsNull() && !cfg.MaxStateSize.IsUnknown() {
		orasCfg.MaxStateSize = cfg.MaxStateSize.ValueInt64()
	}

	client, err := oras.NewClient(registry, repository, orasCfg)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create OCI client", err.Error())
		return
	}

	// Wrap the client together with the shared lock registry: every per-RPC
	// OCIStateStore instance restores this pointer in Configure.
	resp.StateStoreData = &stateStoreData{client: client}
}

// ─── StateStoreWithConfigure ──────────────────────────────────────────────────

// Configure restores the shared state (client + lock registry) created in
// Initialize onto this per-RPC instance.
func (s *OCIStateStore) Configure(_ context.Context, req fwss.ConfigureRequest, resp *fwss.ConfigureResponse) {
	if req.StateStoreData == nil {
		// Silent: the framework calls Configure with nil data during offline
		// validation, before ValidateConfig has produced anything.
		return
	}

	switch data := req.StateStoreData.(type) {
	case *stateStoreData:
		s.shared = data
		s.client = data.client
	case *oras.Client:
		// Direct client (legacy path): wrap it in a per-instance registry.
		s.shared = &stateStoreData{client: data}
		s.client = data
	default:
		resp.Diagnostics.AddError(
			"Unexpected StateStore data type",
			fmt.Sprintf("Expected state store data from Initialize, got: %T. This is a provider bug.", req.StateStoreData),
		)
	}
}

// ─── State operations ─────────────────────────────────────────────────────────

// Read retrieves the state bytes for the given StateID from the OCI registry.
// Returns nil StateBytes (no error) if no state exists yet.
func (s *OCIStateStore) Read(ctx context.Context, req fwss.ReadRequest, resp *fwss.ReadResponse) {
	data, err := s.client.Get(ctx, req.StateID)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read state", err.Error())
		return
	}
	resp.StateBytes = data
}

// Write stores the state bytes for the given StateID in the OCI registry.
//
// fwss.WriteRequest carries no LockID, so ownership is checked against locks
// this instance acquired: if we hold a registered lock for the StateID, the
// remote lock must still be ours (oras.Client.VerifyLock) before writing —
// a lost lock means another client won, and we refuse to overwrite their
// state. If we hold NO local lock (e.g. Terraform's explicit -lock=false
// usage, where Lock is never called), the write proceeds unverified: this is
// best-effort ownership checking, not enforcement. OCI tags have no CAS, so
// even a passing check cannot close the verify→Put race.
func (s *OCIStateStore) Write(ctx context.Context, req fwss.WriteRequest, resp *fwss.WriteResponse) {
	localLockID, held := s.shared.lockFor(req.StateID)
	if held {
		if err := s.client.VerifyLock(ctx, req.StateID, localLockID); err != nil {
			// Cancellation/deadline first: the check was interrupted, and the
			// caller must see that rather than a lost-lock framing.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				resp.Diagnostics.AddError(
					"State write interrupted",
					fmt.Sprintf("Verifying the lock for workspace %q was interrupted before ownership could be confirmed: %v. This write did not attempt state publication. %s",
						req.StateID, err, stateRecoveryGuidance),
				)
				return
			}
			resp.Diagnostics.AddError(
				"State lock no longer held",
				fmt.Sprintf("Refusing to write state for workspace %q: the lock held by this operation is no longer valid (%v). This write did not attempt state publication; infrastructure may already have changed and another writer may have taken over. %s",
					req.StateID, err, stateRecoveryGuidance),
			)
			return
		}
	}

	if err := s.client.Put(ctx, req.StateID, req.StateBytes); err != nil {
		addPublicationDiagnostic(&resp.Diagnostics, req.StateID, err)
	}
}

const stateRecoveryGuidance = "Do not blindly re-apply. Stop other writers, securely preserve any Terraform recovery state (such as errored.tfstate), and inspect remote state before recovery. Compare lineage, serial and resource identities; only push reconciled state with a fresh lock. See docs/guides/state-recovery.md."

// A partial error confirms state publication even when its version step was
// cancelled. Other interrupted writes remain uncertain, not wholly rejected.
func addPublicationDiagnostic(diags *diag.Diagnostics, stateID string, err error) {
	var partial *oras.PartialPublicationError
	if errors.As(err, &partial) {
		diags.AddError(
			"State version publication failed",
			fmt.Sprintf("State for workspace %q was published successfully under tag %q, but publishing the version tag %q failed (%v). State publication was confirmed; inspect the current state because another writer may since have replaced it. Version publication may be incomplete. %s",
				stateID, partial.StateTag, partial.VersionTag, partial.Err, stateRecoveryGuidance),
		)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		diags.AddError(
			"State write interrupted",
			fmt.Sprintf("Writing state for workspace %q was interrupted before it could be confirmed: %v. Publication is uncertain; state may already have been written. %s",
				stateID, err, stateRecoveryGuidance),
		)
		return
	}
	if errors.Is(err, oras.ErrMutableTagMoved) {
		// A transient write failure left the state tag's ownership
		// ambiguous: the publication failed closed instead of blindly
		// re-applying over a possibly newer state. Note this does NOT
		// mean the lock was proven lost — the ordinary verify→Put
		// TOCTOU window remains (oci_concurrency_test.go, Test 9).
		diags.AddError(
			"State publication conflict",
			fmt.Sprintf("State publication for workspace %q could not be confirmed after a transient write failure (%v). Publication is uncertain; another client may have published newer state. The tag was not blindly re-applied. %s",
				stateID, err, stateRecoveryGuidance),
		)
		return
	}
	diags.AddError("Failed to write state", fmt.Sprintf("State publication for workspace %q was not confirmed (%v). Inspect remote state; do not assume nothing was written. %s", stateID, err, stateRecoveryGuidance))
}

// DeleteState removes the state for the given StateID from the OCI registry.
func (s *OCIStateStore) DeleteState(ctx context.Context, req fwss.DeleteStateRequest, resp *fwss.DeleteStateResponse) {
	if err := s.client.Delete(ctx, req.StateID); err != nil {
		resp.Diagnostics.AddError("Failed to delete state", err.Error())
	}
}

// GetStates returns all workspace (state) IDs stored in the OCI repository.
func (s *OCIStateStore) GetStates(ctx context.Context, _ fwss.GetStatesRequest, resp *fwss.GetStatesResponse) {
	states, err := s.client.List(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list states", err.Error())
		return
	}
	resp.StateIDs = states
}

// ─── Locking ──────────────────────────────────────────────────────────────────

// newLockInfoMu is a compatibility workaround, NOT distributed locking and
// not a replacement for an upstream fix: terraform-plugin-framework v1.19.0's
// statestore.generateLockID reads its own unsynchronized package-level
// *rand.Rand (rngSource), which is a data race when Lock RPCs run concurrently
// (surfaced by oci_concurrency_test.go under -race). Serializing ONLY the
// fwss.NewLockInfo call removes that in-process RNG race for this provider
// path; it provides no cross-process/distributed safety whatsoever and must
// be revisited (removed) after an upstream synchronized release.
var newLockInfoMu sync.Mutex

// Lock acquires a lock for the given workspace. Uses generation-based optimistic
// concurrency control via the ORAS client to detect simultaneous lock attempts.
func (s *OCIStateStore) Lock(ctx context.Context, req fwss.LockRequest, resp *fwss.LockResponse) {
	// Create a new LockInfo for this attempt (generates UUID, Who, Created).
	// Narrow mutex: held only for the framework call, released before any
	// registry/network operation (see newLockInfoMu).
	newLockInfoMu.Lock()
	fwLockInfo := fwss.NewLockInfo(req)
	newLockInfoMu.Unlock()

	// Map framework LockInfo → oras LockInfo.
	orasLockInfo := oras.LockInfo{
		ID:        fwLockInfo.ID,
		Operation: fwLockInfo.Operation,
		Who:       fwLockInfo.Who,
		Created:   fwLockInfo.Created,
	}

	lockID, err := s.client.Lock(ctx, req.StateID, orasLockInfo)
	if err != nil {
		var lockErr *oras.LockError
		if errors.As(err, &lockErr) && lockErr.Info != nil {
			// Map the existing holder's oras.LockInfo → fwss.LockInfo for the diagnostic.
			existingLock := fwss.LockInfo{
				ID:        lockErr.Info.ID,
				Operation: lockErr.Info.Operation,
				Who:       lockErr.Info.Who,
				Created:   lockErr.Info.Created,
			}
			resp.Diagnostics.Append(fwss.WorkspaceAlreadyLockedDiagnostic(req, existingLock))
		} else {
			resp.Diagnostics.AddError("Failed to acquire state lock", err.Error())
		}
		return
	}

	s.shared.registerLock(req.StateID, lockID)
	resp.LockID = lockID
}

// Unlock releases a lock previously acquired by Lock.
func (s *OCIStateStore) Unlock(ctx context.Context, req fwss.UnlockRequest, resp *fwss.UnlockResponse) {
	// An empty LockID must never unlock: oras.Client.Unlock treats an empty
	// ID as "any holder", so this would release someone else's lock.
	if req.LockID == "" {
		resp.Diagnostics.AddError(
			"Missing lock ID",
			fmt.Sprintf("Unlock for workspace %q was called with an empty lock ID; refusing to release the lock without verifying ownership. This may indicate the workspace was never locked or a provider bug.",
				req.StateID),
		)
		return
	}

	if err := s.client.Unlock(ctx, req.StateID, req.LockID); err != nil {
		resp.Diagnostics.AddError("Failed to release state lock", err.Error())
		return
	}

	// Success: drop the registration only if it still matches the lock we
	// just released — a newer acquisition under the same configuration must
	// survive.
	s.shared.forgetLockIf(req.StateID, req.LockID)
}

// ─── URL parsing ──────────────────────────────────────────────────────────────

// parseOCIURL splits an oci:// URL into its registry and repository components.
//
// Examples:
//
//	"oci://ghcr.io/myorg/infra-state"         → "ghcr.io", "myorg/infra-state"
//	"oci://registry.example.com:5000/myrepo"  → "registry.example.com:5000", "myrepo"
func parseOCIURL(rawURL string) (registry, repository string, err error) {
	if !strings.HasPrefix(rawURL, "oci://") {
		return "", "", fmt.Errorf("URL must start with 'oci://', got: %q", rawURL)
	}
	rest := strings.TrimPrefix(rawURL, "oci://")
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("OCI URL %q is missing the repository path", rawURL)
	}
	registry = rest[:idx]
	repository = rest[idx+1:]
	if registry == "" {
		return "", "", fmt.Errorf("OCI URL %q is missing the registry host", rawURL)
	}
	if repository == "" {
		return "", "", fmt.Errorf("OCI URL %q is missing the repository path", rawURL)
	}
	if strings.Contains(repository, "..") || strings.Contains(repository, "//") ||
		strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return "", "", fmt.Errorf("OCI URL %q contains invalid path component", rawURL)
	}
	return registry, repository, nil
}
