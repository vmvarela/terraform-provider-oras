// Package provider implements the OCI state store provider for Terraform.
// It satisfies both provider.Provider and provider.ProviderWithStateStores from
// the Terraform Plugin Framework, exposing provider-level TLS configuration
// (insecure, ca_file) consumed by the state stores added in later phases.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/vmvarela/terraform-provider-oras/internal/config"
	ocistatestore "github.com/vmvarela/terraform-provider-oras/internal/statestore"
)

// Compile-time interface checks.
var (
	_ provider.Provider               = (*OrasProvider)(nil)
	_ provider.ProviderWithStateStores = (*OrasProvider)(nil)
)

// ProviderData holds provider-level configuration forwarded to state stores
// via ConfigureResponse.StateStoreData.
type ProviderData = config.ProviderData

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
	Insecure types.Bool   `tfsdk:"insecure"`
	CAFile   types.String `tfsdk:"ca_file"`
}

// Metadata sets the provider type name used to prefix resource/state-store names.
func (p *OrasProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "oras"
}

// Schema declares provider-level configuration attributes.
func (p *OrasProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for storing Terraform state in OCI registries via the ORAS protocol.",
		Attributes: map[string]schema.Attribute{
			"insecure": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Skip TLS certificate verification when communicating with the OCI registry. Defaults to `false`.",
			},
			"ca_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to a PEM-encoded CA certificate bundle to trust when communicating with the OCI registry.",
			},
		},
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

	insecure := !cfg.Insecure.IsNull() && !cfg.Insecure.IsUnknown() && cfg.Insecure.ValueBool()
	caFile := ""
	if !cfg.CAFile.IsNull() && !cfg.CAFile.IsUnknown() {
		caFile = cfg.CAFile.ValueString()
	}

	httpClient, err := newHTTPClient(insecure, caFile)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create HTTP client", err.Error())
		return
	}

	pd := &ProviderData{
		Insecure:   insecure,
		CAFile:     caFile,
		HTTPClient: httpClient,
	}

	// Share the same ProviderData with resources, data sources, and state stores.
	resp.ResourceData = pd
	resp.DataSourceData = pd
	resp.StateStoreData = pd
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

// newHTTPClient constructs an *http.Client with the specified TLS settings.
// When insecure is true, certificate verification is disabled.
// When caFile is non-empty, it is loaded as the trusted CA pool.
func newHTTPClient(insecure bool, caFile string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected type for http.DefaultTransport")
	}
	t := base.Clone()

	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{} //nolint:gosec
	}
	t.TLSClientConfig.InsecureSkipVerify = insecure //nolint:gosec // intentional per user config

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %q: no valid PEM certificates found", caFile)
		}
		t.TLSClientConfig.RootCAs = pool
	}

	return &http.Client{Transport: t}, nil
}
