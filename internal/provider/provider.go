// Package provider implements the OCI state store provider for Terraform.
// It satisfies both provider.Provider and provider.ProviderWithStateStores from
// the Terraform Plugin Framework, exposing provider-level transport and TLS
// configuration consumed by the state stores.
package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
	ocistatestore "github.com/vmvarela/terraform-provider-oras/internal/statestore"
)

// Compile-time interface checks.
var (
	_ provider.Provider                   = (*OrasProvider)(nil)
	_ provider.ProviderWithStateStores    = (*OrasProvider)(nil)
	_ provider.ProviderWithValidateConfig = (*OrasProvider)(nil)
)

// OrasProvider implements provider.Provider and provider.ProviderWithStateStores.
type OrasProvider struct{}

// New returns a provider factory function suitable for providerserver.Serve.
func New() func() provider.Provider {
	return func() provider.Provider {
		return &OrasProvider{}
	}
}

// providerModel mirrors the HCL provider block attributes.
type providerModel struct {
	PlainHTTP     types.Bool   `tfsdk:"plain_http"`
	TLSSkipVerify types.Bool   `tfsdk:"tls_skip_verify"`
	Insecure      types.Bool   `tfsdk:"insecure"`
	CAFile        types.String `tfsdk:"ca_file"`
}

// Metadata sets the provider type name used to prefix resource/state-store names.
func (p *OrasProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "oras"
	resp.Version = oras.Version
}

// Schema declares provider-level configuration attributes.
func (p *OrasProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for storing Terraform state in OCI registries via the ORAS protocol.",
		Attributes: map[string]schema.Attribute{
			"plain_http": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Use unencrypted HTTP instead of HTTPS. Defaults to `false`. Cannot be combined with `insecure`. When true, conflicts with a non-empty `ca_file` or `tls_skip_verify = true`.",
			},
			"tls_skip_verify": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Disable HTTPS certificate verification while retaining TLS. Defaults to `false`. Prefer `ca_file` for private CAs. Cannot be combined with `insecure`. When true, conflicts with a non-empty `ca_file`.",
			},
			"insecure": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Deprecated: retains legacy behavior, enabling plain HTTP and disabling TLS certificate verification when true. Defaults to `false`. Cannot be combined with `plain_http` or `tls_skip_verify`, even when false.",
				DeprecationMessage:  "Use plain_http for HTTP or tls_skip_verify for HTTPS without certificate verification. Remove insecure before setting either option.",
			},
			"ca_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to a PEM-encoded CA certificate bundle to trust when communicating with the OCI registry.",
			},
		},
	}
}

// ValidateConfig validates the provider configuration before execution.
func (p *OrasProvider) ValidateConfig(ctx context.Context, req provider.ValidateConfigRequest, resp *provider.ValidateConfigResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	validateTransportConfig(cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Validate ca_file exists and is readable if specified
	if !cfg.CAFile.IsNull() && !cfg.CAFile.IsUnknown() {
		caFile := cfg.CAFile.ValueString()
		if caFile != "" {
			f, err := os.Open(caFile)
			if err != nil {
				resp.Diagnostics.AddAttributeError(
					path.Root("ca_file"),
					"Invalid CA File",
					fmt.Sprintf("ca_file %q does not exist or is not accessible: %v", caFile, err),
				)
			} else {
				_ = f.Close()
			}
		}
	}
}

// Configure reads provider configuration, builds an HTTP client with the
// requested TLS settings, and stores a *ProviderData in resp.StateStoreData so
// that state stores can call Configure to receive it.
func (p *OrasProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	validateTransportConfig(cfg, &resp.Diagnostics)
	for name, unknown := range map[string]bool{
		"insecure": cfg.Insecure.IsUnknown(), "plain_http": cfg.PlainHTTP.IsUnknown(),
		"tls_skip_verify": cfg.TLSSkipVerify.IsUnknown(), "ca_file": cfg.CAFile.IsUnknown(),
	} {
		if unknown {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Unknown transport setting", "Transport settings must be known before configuring the state store.")
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	plainHTTP, skipVerify := cfg.PlainHTTP.ValueBool(), cfg.TLSSkipVerify.ValueBool()
	if cfg.Insecure.ValueBool() {
		// Preserve the old transport and trust behavior only for the legacy option.
		plainHTTP, skipVerify = true, true
	}
	caFile := ""
	if !cfg.CAFile.IsNull() && !cfg.CAFile.IsUnknown() {
		caFile = cfg.CAFile.ValueString()
	}

	httpClient, err := oras.BuildHTTPClient(skipVerify, caFile)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create HTTP client", err.Error())
		return
	}

	// Share the same ProviderData with state stores. The provider has zero
	// resources and data sources, so ResourceData/DataSourceData are not set.
	resp.StateStoreData = &ocistatestore.ProviderData{
		PlainHTTP:  plainHTTP,
		HTTPClient: httpClient,
	}
}

// DataSources returns no data sources; this is a state-store-only provider.
func (p *OrasProvider) DataSources(_ context.Context) []func() datasource.DataSource { return nil }

// Resources returns no managed resources; this is a state-store-only provider.
func (p *OrasProvider) Resources(_ context.Context) []func() resource.Resource { return nil }

// StateStores returns the OCI state store factory.
func (p *OrasProvider) StateStores(_ context.Context) []func() statestore.StateStore {
	return []func() statestore.StateStore{
		ocistatestore.New(),
	}
}

// validateTransportConfig rejects ambiguity instead of assigning precedence.
// Unknown values are deferred during validation and rejected by Configure.
func validateTransportConfig(cfg providerModel, diags *diag.Diagnostics) {
	legacy := !cfg.Insecure.IsNull()
	if legacy && (!cfg.PlainHTTP.IsNull() || !cfg.TLSSkipVerify.IsNull()) {
		diags.AddAttributeError(path.Root("insecure"), "Conflicting transport settings", "Remove insecure before setting plain_http or tls_skip_verify, even when their values are false. Legacy insecure behavior is preserved only when the new options are absent.")
	}
	hasCA := !cfg.CAFile.IsUnknown() && cfg.CAFile.ValueString() != ""
	if cfg.PlainHTTP.ValueBool() && (cfg.TLSSkipVerify.ValueBool() || hasCA) {
		diags.AddAttributeError(path.Root("plain_http"), "Conflicting transport settings", "plain_http = true cannot be combined with tls_skip_verify = true or a non-empty ca_file; TLS settings do not apply to HTTP.")
	}
	if cfg.TLSSkipVerify.ValueBool() && hasCA {
		diags.AddAttributeError(path.Root("tls_skip_verify"), "Conflicting TLS settings", "tls_skip_verify = true cannot be combined with a non-empty ca_file; use ca_file with certificate verification enabled.")
	}
}
