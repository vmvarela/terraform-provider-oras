// Package statestore implements the OCI state store for the Terraform plugin framework.
// It bridges the terraform-plugin-framework statestore.StateStore interface with the
// ORAS client in internal/oras, enabling Terraform to persist tfstate in OCI registries.
package statestore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	ssschema "github.com/hashicorp/terraform-plugin-framework/statestore/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/vmvarela/terraform-provider-oras/internal/config"
	"github.com/vmvarela/terraform-provider-oras/internal/oras"
)

// Compile-time interface checks.
var (
	_ fwss.StateStore                  = (*OCIStateStore)(nil)
	_ fwss.StateStoreWithConfigure     = (*OCIStateStore)(nil)
	_ fwss.StateStoreWithValidateConfig = (*OCIStateStore)(nil)
)

// OCIStateStore implements fwss.StateStore using OCI registries via the ORAS protocol.
type OCIStateStore struct {
	client *oras.Client
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
				MarkdownDescription: "Duration for state lock TTL (e.g., `15m`, `1h`). Stale locks older than this value are automatically cleared.",
			},
			"max_versions": ssschema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Maximum number of state versions to retain. When exceeded, the oldest versions are pruned. Defaults to `0` (unlimited).",
			},
			"max_state_size": ssschema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Maximum allowed state size in bytes. Defaults to 256 MiB.",
			},
		},
	}
}

// ValidateConfig validates the state store configuration before initialization.
func (s *OCIStateStore) ValidateConfig(ctx context.Context, req fwss.ValidateConfigRequest, resp *fwss.ValidateConfigResponse) {
	var cfg storeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !cfg.URL.IsNull() && !cfg.URL.IsUnknown() {
		u := cfg.URL.ValueString()
		// Validate full URL structure
		if _, _, err := parseOCIURL(u); err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("url"),
				"Invalid OCI URL",
				err.Error(),
			)
		}
	}

	if !cfg.LockTTL.IsNull() && !cfg.LockTTL.IsUnknown() {
		if _, err := time.ParseDuration(cfg.LockTTL.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("lock_ttl"),
				"Invalid lock_ttl",
				fmt.Sprintf("lock_ttl must be a valid Go duration (e.g., '15m', '1h'): %s", err),
			)
		}
	}

	if !cfg.MaxVersions.IsNull() && !cfg.MaxVersions.IsUnknown() {
		if cfg.MaxVersions.ValueInt64() < 0 {
			resp.Diagnostics.AddAttributeError(
				path.Root("max_versions"),
				"Invalid max_versions",
				"max_versions must be >= 0",
			)
		}
	}

	if !cfg.MaxStateSize.IsNull() && !cfg.MaxStateSize.IsUnknown() {
		if cfg.MaxStateSize.ValueInt64() < 0 {
			resp.Diagnostics.AddAttributeError(
				path.Root("max_state_size"),
				"Invalid max_state_size",
				"max_state_size must be >= 0",
			)
		}
	}
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

	registry, repository, err := parseOCIURL(cfg.URL.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("url"), "Invalid OCI URL", err.Error())
		return
	}

	var opts []oras.Option

	// Forward provider-level TLS settings to the ORAS client.
	if pd, ok := req.ProviderData.(*config.ProviderData); ok && pd != nil {
		if pd.HTTPClient != nil {
			opts = append(opts, oras.WithHTTPClient(pd.HTTPClient))
		}
		// WithInsecure sets repo.PlainHTTP, required for http:// registries
		// (e.g. local Zot). Must be passed even when a custom HTTPClient exists.
		if pd.Insecure {
			opts = append(opts, oras.WithInsecure(true))
		}
		if pd.CAFile != "" {
			opts = append(opts, oras.WithCAFile(pd.CAFile))
		}
	}

	if !cfg.Compression.IsNull() && !cfg.Compression.IsUnknown() {
		opts = append(opts, oras.WithCompression(cfg.Compression.ValueBool()))
	}

	if !cfg.LockTTL.IsNull() && !cfg.LockTTL.IsUnknown() {
		// ParseDuration already validated in ValidateConfig, ignore error here.
		if ttl, err := time.ParseDuration(cfg.LockTTL.ValueString()); err == nil {
			opts = append(opts, oras.WithLockTTL(ttl))
		}
	}

	if !cfg.MaxVersions.IsNull() && !cfg.MaxVersions.IsUnknown() {
		opts = append(opts, oras.WithMaxVersions(int(cfg.MaxVersions.ValueInt64())))
	}

	if !cfg.MaxStateSize.IsNull() && !cfg.MaxStateSize.IsUnknown() {
		opts = append(opts, oras.WithMaxStateSize(cfg.MaxStateSize.ValueInt64()))
	}

	client, err := oras.NewClient(registry, repository, opts...)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create OCI client", err.Error())
		return
	}

	resp.StateStoreData = client
}

// ─── StateStoreWithConfigure ──────────────────────────────────────────────────

// Configure stores the *oras.Client (set in Initialize) on the struct so that
// Read/Write/Lock/Unlock/GetStates/DeleteState can use it.
func (s *OCIStateStore) Configure(_ context.Context, req fwss.ConfigureRequest, resp *fwss.ConfigureResponse) {
	if req.StateStoreData == nil {
		return
	}

	client, ok := req.StateStoreData.(*oras.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected StateStore data type",
			fmt.Sprintf("Expected *oras.Client, got: %T. This is a provider bug.", req.StateStoreData),
		)
		return
	}

	s.client = client
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
func (s *OCIStateStore) Write(ctx context.Context, req fwss.WriteRequest, resp *fwss.WriteResponse) {
	if err := s.client.Put(ctx, req.StateID, req.StateBytes); err != nil {
		resp.Diagnostics.AddError("Failed to write state", err.Error())
	}
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

// Lock acquires a lock for the given workspace. Uses generation-based optimistic
// concurrency control via the ORAS client to detect simultaneous lock attempts.
func (s *OCIStateStore) Lock(ctx context.Context, req fwss.LockRequest, resp *fwss.LockResponse) {
	// Create a new LockInfo for this attempt (generates UUID, Who, Created).
	fwLockInfo := fwss.NewLockInfo(req)

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

	resp.LockID = lockID
}

// Unlock releases a lock previously acquired by Lock.
func (s *OCIStateStore) Unlock(ctx context.Context, req fwss.UnlockRequest, resp *fwss.UnlockResponse) {
	if err := s.client.Unlock(ctx, req.StateID, req.LockID); err != nil {
		resp.Diagnostics.AddError("Failed to release state lock", err.Error())
	}
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

	// Substitute scheme for standard URL parsing while preserving the host:port.
	u, err := url.Parse("https://" + strings.TrimPrefix(rawURL, "oci://"))
	if err != nil {
		return "", "", fmt.Errorf("invalid OCI URL %q: %w", rawURL, err)
	}

	registry = u.Host
	if registry == "" {
		return "", "", fmt.Errorf("OCI URL %q is missing the registry host", rawURL)
	}

	repository = strings.TrimPrefix(u.Path, "/")
	if repository == "" {
		return "", "", fmt.Errorf("OCI URL %q is missing the repository path", rawURL)
	}

	// Validate repository path for invalid components
	if strings.Contains(repository, "..") {
		return "", "", fmt.Errorf("OCI URL %q contains invalid path component '..'", rawURL)
	}
	if strings.Contains(repository, "//") {
		return "", "", fmt.Errorf("OCI URL %q contains invalid path component '//'", rawURL)
	}

	return registry, repository, nil
}
