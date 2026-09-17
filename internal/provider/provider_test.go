// Package provider implements the OCI state store provider for Terraform.
package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
	ocistatestore "github.com/vmvarela/terraform-provider-oras/internal/statestore"
)

// ─── tfsdk.Config helpers ─────────────────────────────────────────────────────

// providerConfig builds a provider tfsdk.Config; attributes not in overrides
// are null.
func providerConfig(t *testing.T, overrides map[string]tftypes.Value) tfsdk.Config {
	t.Helper()
	schemaResp := &provider.SchemaResponse{}
	(&OrasProvider{}).Schema(context.Background(), provider.SchemaRequest{}, schemaResp)

	ctx := context.Background()
	attrTypes := map[string]tftypes.Type{}
	vals := map[string]tftypes.Value{}
	for name, attr := range schemaResp.Schema.GetAttributes() {
		tt := attr.GetType().TerraformType(ctx)
		attrTypes[name] = tt
		vals[name] = tftypes.NewValue(tt, nil)
	}
	for k, v := range overrides {
		vals[k] = v
	}
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, vals)
	return tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}
}

// ─── Provider factory / Metadata / Schema ─────────────────────────────────────

func TestProviderFactory(t *testing.T) {
	factory := New()
	if factory == nil {
		t.Fatal("New() returned nil")
	}
	if _, ok := factory().(*OrasProvider); !ok {
		t.Fatalf("factory() = %T, want *OrasProvider", factory())
	}
}

func TestProviderMetadata(t *testing.T) {
	origVersion := oras.Version
	oras.Version = "9.9.9"
	t.Cleanup(func() { oras.Version = origVersion })

	resp := &provider.MetadataResponse{}
	(&OrasProvider{}).Metadata(context.Background(), provider.MetadataRequest{}, resp)
	if resp.TypeName != "oras" {
		t.Errorf("TypeName = %q, want %q", resp.TypeName, "oras")
	}
	if resp.Version != "9.9.9" {
		t.Errorf("Version = %q, want %q", resp.Version, "9.9.9")
	}
}

func TestProviderSchema(t *testing.T) {
	resp := &provider.SchemaResponse{}
	(&OrasProvider{}).Schema(context.Background(), provider.SchemaRequest{}, resp)
	if resp.Schema.Attributes["insecure"].(schema.BoolAttribute).DeprecationMessage == "" {
		t.Error("legacy insecure must emit a schema deprecation warning")
	}
	for _, attr := range []string{"insecure", "plain_http", "tls_skip_verify", "ca_file"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// ─── ValidateConfig ───────────────────────────────────────────────────────────

func TestProviderValidateConfig(t *testing.T) {
	t.Run("happy empty config", func(t *testing.T) {
		resp := &provider.ValidateConfigResponse{}
		(&OrasProvider{}).ValidateConfig(context.Background(), provider.ValidateConfigRequest{Config: providerConfig(t, nil)}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("missing ca_file produces diagnostic", func(t *testing.T) {
		resp := &provider.ValidateConfigResponse{}
		cfg := providerConfig(t, map[string]tftypes.Value{
			"ca_file": tftypes.NewValue(tftypes.String, "/nonexistent/oras-test-ca.pem"),
		})
		(&OrasProvider{}).ValidateConfig(context.Background(), provider.ValidateConfigRequest{Config: cfg}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected diagnostic for missing ca_file, got none")
		}
		if !strings.Contains(resp.Diagnostics[0].Detail(), "ca_file") {
			t.Errorf("detail = %q, want it to mention ca_file", resp.Diagnostics[0].Detail())
		}
	})
}

// ─── Configure ────────────────────────────────────────────────────────────────

func TestProviderConfigure(t *testing.T) {
	resp := &provider.ConfigureResponse{}
	(&OrasProvider{}).Configure(context.Background(), provider.ConfigureRequest{
		Config: providerConfig(t, map[string]tftypes.Value{
			"insecure": tftypes.NewValue(tftypes.Bool, true),
		}),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	pd, ok := resp.StateStoreData.(*ocistatestore.ProviderData)
	if !ok {
		t.Fatalf("StateStoreData is %T, want *ocistatestore.ProviderData", resp.StateStoreData)
	}
	if !pd.PlainHTTP {
		t.Error("ProviderData.PlainHTTP = false, want true")
	}
	if pd.HTTPClient == nil {
		t.Error("ProviderData.HTTPClient is nil")
	}
}

// ─── StateStores ──────────────────────────────────────────────────────────────

func TestProviderStateStores(t *testing.T) {
	stores := (&OrasProvider{}).StateStores(context.Background())
	if len(stores) != 1 {
		t.Fatalf("StateStores returned %d factories, want 1", len(stores))
	}
	ss := stores[0]()
	if ss == nil {
		t.Fatal("state store factory returned nil")
	}

	// The returned store must satisfy the state store interfaces the
	// framework dispatches on, including offline validation.
	stateStoreWithValidate, ok := any(ss).(fwss.StateStoreWithValidateConfig)
	if !ok {
		t.Error("state store does not implement fwss.StateStoreWithValidateConfig")
	}
	if stateStoreWithValidate != nil {
		// Build a store config from the store's own schema (url required).
		schemaResp := &fwss.SchemaResponse{}
		stateStoreWithValidate.Schema(context.Background(), fwss.SchemaRequest{}, schemaResp)
		ctx := context.Background()
		attrTypes := map[string]tftypes.Type{}
		vals := map[string]tftypes.Value{}
		for name, attr := range schemaResp.Schema.GetAttributes() {
			tt := attr.GetType().TerraformType(ctx)
			attrTypes[name] = tt
			vals[name] = tftypes.NewValue(tt, nil)
		}
		vals["url"] = tftypes.NewValue(tftypes.String, "oci://ghcr.io/org/repo")
		raw := tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, vals)

		validateResp := &fwss.ValidateConfigResponse{}
		stateStoreWithValidate.ValidateConfig(ctx, fwss.ValidateConfigRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}}, validateResp)
		if validateResp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics from state store ValidateConfig: %v", validateResp.Diagnostics)
		}
	}
}
