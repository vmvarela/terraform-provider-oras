package provider

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	ocistatestore "github.com/vmvarela/terraform-provider-oras/internal/statestore"
)

func TestTransportConfiguration(t *testing.T) {
	b := func(v bool) tftypes.Value { return tftypes.NewValue(tftypes.Bool, v) }
	for _, tc := range []struct {
		name    string
		values  map[string]tftypes.Value
		invalid bool
	}{
		{"defaults", nil, false},
		{"explicit secure", map[string]tftypes.Value{"plain_http": b(false), "tls_skip_verify": b(false)}, false},
		{"http", map[string]tftypes.Value{"plain_http": b(true)}, false},
		{"https skip verification", map[string]tftypes.Value{"tls_skip_verify": b(true)}, false},
		{"legacy false", map[string]tftypes.Value{"insecure": b(false)}, false},
		{"legacy true", map[string]tftypes.Value{"insecure": b(true)}, false},
		{"legacy false conflicts with new false", map[string]tftypes.Value{"insecure": b(false), "plain_http": b(false)}, true},
		{"legacy true conflicts with new true", map[string]tftypes.Value{"insecure": b(true), "plain_http": b(true)}, true},
		{"legacy conflicts with tls option", map[string]tftypes.Value{"insecure": b(true), "tls_skip_verify": b(false)}, true},
		{"http with tls bypass", map[string]tftypes.Value{"plain_http": b(true), "tls_skip_verify": b(true)}, true},
		{"http with CA", map[string]tftypes.Value{"plain_http": b(true), "ca_file": tftypes.NewValue(tftypes.String, "not-read.pem")}, true},
		{"tls bypass with CA", map[string]tftypes.Value{"tls_skip_verify": b(true), "ca_file": tftypes.NewValue(tftypes.String, "not-read.pem")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := providerConfig(t, tc.values)
			validation := &provider.ValidateConfigResponse{}
			(&OrasProvider{}).ValidateConfig(context.Background(), provider.ValidateConfigRequest{Config: cfg}, validation)
			if validation.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("validation error = %v, want %v", validation.Diagnostics.HasError(), tc.invalid)
			}
			configured := &provider.ConfigureResponse{}
			(&OrasProvider{}).Configure(context.Background(), provider.ConfigureRequest{Config: cfg}, configured)
			if configured.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("configure error = %v, want %v", configured.Diagnostics.HasError(), tc.invalid)
			}
			if tc.invalid && configured.StateStoreData != nil {
				t.Fatal("invalid configuration produced a client")
			}
		})
	}
}

func TestUnknownTransportConfiguration(t *testing.T) {
	for _, name := range []string{"insecure", "plain_http", "tls_skip_verify", "ca_file"} {
		t.Run(name, func(t *testing.T) {
			var typ tftypes.Type = tftypes.Bool
			if name == "ca_file" {
				typ = tftypes.String
			}
			cfg := providerConfig(t, map[string]tftypes.Value{name: tftypes.NewValue(typ, tftypes.UnknownValue)})
			validation := &provider.ValidateConfigResponse{}
			(&OrasProvider{}).ValidateConfig(context.Background(), provider.ValidateConfigRequest{Config: cfg}, validation)
			if validation.Diagnostics.HasError() {
				t.Fatal("validation must defer unknown values")
			}
			configured := &provider.ConfigureResponse{}
			(&OrasProvider{}).Configure(context.Background(), provider.ConfigureRequest{Config: cfg}, configured)
			if !configured.Diagnostics.HasError() || configured.StateStoreData != nil {
				t.Fatal("configure silently defaulted an unknown transport setting")
			}
		})
	}
}

// Exercise Provider Configure -> StateStore Initialize/Configure -> ORAS -> real
// HTTP/TLS requests. No production registry credentials or state are used.
func TestTransportRequests(t *testing.T) {
	t.Setenv("ORAS_TOKEN", "transport-test-token")
	for _, tc := range []struct {
		name      string
		tls       bool
		options   map[string]tftypes.Value
		ca        string
		wantError bool
	}{
		{name: "default rejects untrusted TLS", tls: true, wantError: true},
		{name: "custom CA verifies TLS", tls: true, ca: "server"},
		{name: "wrong CA rejects TLS", tls: true, ca: "other", wantError: true},
		{name: "skip verification retains HTTPS", tls: true, options: map[string]tftypes.Value{"tls_skip_verify": tftypes.NewValue(tftypes.Bool, true)}},
		{name: "explicit HTTP", options: map[string]tftypes.Value{"plain_http": tftypes.NewValue(tftypes.Bool, true)}},
		{name: "default never falls back to HTTP", wantError: true},
		{name: "skip verification never falls back to HTTP", options: map[string]tftypes.Value{"tls_skip_verify": tftypes.NewValue(tftypes.Bool, true)}, wantError: true},
		{name: "legacy true retains HTTP", options: map[string]tftypes.Value{"insecure": tftypes.NewValue(tftypes.Bool, true)}},
		{name: "legacy false retains verified HTTPS", tls: true, ca: "server", options: map[string]tftypes.Value{"insecure": tftypes.NewValue(tftypes.Bool, false)}},
		{name: "legacy HTTP with CA remains accepted", ca: "server", options: map[string]tftypes.Value{"insecure": tftypes.NewValue(tftypes.Bool, true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if (r.TLS != nil) != tc.tls {
					t.Error("unexpected transport protocol")
				}
				if r.Header.Get("Authorization") == "" {
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+scheme+`://`+r.Host+`/token",service="fixture"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer transport-test-token" {
					t.Error("configured credential was not propagated")
				}
				if r.URL.Path != "/v2/test/repo/tags/list" {
					t.Error("unexpected registry operation")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"name":"test/repo","tags":[]}`)
			})
			srv := httptest.NewUnstartedServer(handler)
			srv.Config.ErrorLog = log.New(io.Discard, "", 0)
			if tc.tls {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			options := tc.options
			if options == nil {
				options = map[string]tftypes.Value{}
			}
			if tc.ca != "" {
				cert := srv.Certificate()
				if cert == nil || tc.ca == "other" {
					other := httptest.NewTLSServer(http.NotFoundHandler())
					defer other.Close()
					cert = other.Certificate()
					if tc.ca == "other" {
						// Use a different trust anchor: the fixture certificate is shared across
						// httptest servers, so a mutated copy cannot verify the server signature.
						raw := append([]byte(nil), cert.Raw...)
						raw[len(raw)-1] ^= 1
						caPath := filepath.Join(t.TempDir(), "ca.pem")
						if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
							t.Fatal(err)
						}
						options["ca_file"] = tftypes.NewValue(tftypes.String, caPath)
					}
				}
				if tc.ca != "other" {
					caPath := filepath.Join(t.TempDir(), "ca.pem")
					if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
						t.Fatal(err)
					}
					options["ca_file"] = tftypes.NewValue(tftypes.String, caPath)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			configured := &provider.ConfigureResponse{}
			(&OrasProvider{}).Configure(ctx, provider.ConfigureRequest{Config: providerConfig(t, options)}, configured)
			if configured.Diagnostics.HasError() {
				t.Fatalf("configure: %v", configured.Diagnostics)
			}
			pd := configured.StateStoreData.(*ocistatestore.ProviderData)
			wantSkip := false
			if legacy, ok := options["insecure"]; ok {
				_ = legacy.As(&wantSkip)
			}
			if skip, ok := options["tls_skip_verify"]; ok {
				_ = skip.As(&wantSkip)
			}
			transport := pd.HTTPClient.Transport.(*http.Transport)
			if transport.TLSClientConfig.InsecureSkipVerify != wantSkip {
				t.Fatal("certificate verification differs from configured mode")
			}
			defer pd.HTTPClient.CloseIdleConnections()
			store := &ocistatestore.OCIStateStore{}
			schema := &fwss.SchemaResponse{}
			store.Schema(ctx, fwss.SchemaRequest{}, schema)
			attrTypes, values := map[string]tftypes.Type{}, map[string]tftypes.Value{}
			for name, attr := range schema.Schema.GetAttributes() {
				typ := attr.GetType().TerraformType(ctx)
				attrTypes[name], values[name] = typ, tftypes.NewValue(typ, nil)
			}
			values["url"] = tftypes.NewValue(tftypes.String, "oci://"+srv.Listener.Addr().String()+"/test/repo")
			cfg := tfsdk.Config{Schema: schema.Schema, Raw: tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, values)}
			initialized := &fwss.InitializeResponse{}
			store.Initialize(ctx, fwss.InitializeRequest{Config: cfg, ProviderData: pd}, initialized)
			if initialized.Diagnostics.HasError() {
				t.Fatalf("initialize: %v", initialized.Diagnostics)
			}
			ready := &fwss.ConfigureResponse{}
			store.Configure(ctx, fwss.ConfigureRequest{StateStoreData: initialized.StateStoreData}, ready)
			if ready.Diagnostics.HasError() {
				t.Fatalf("state store configure: %v", ready.Diagnostics)
			}
			states := &fwss.GetStatesResponse{}
			store.GetStates(ctx, fwss.GetStatesRequest{}, states)
			if states.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("request error = %v, want %v", states.Diagnostics.HasError(), tc.wantError)
			}
			if tc.wantError && requests.Load() != 0 {
				t.Fatal("failed TLS verification/fallback sent an application request")
			}
			if !tc.wantError && requests.Load() == 0 {
				t.Fatal("no registry request observed")
			}
			for _, d := range states.Diagnostics {
				if strings.Contains(d.Detail(), "transport-test-token") {
					t.Fatal("diagnostic leaked a credential")
				}
			}
		})
	}
}
