package oras

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientTransportSelection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tls           bool
		config        Config
		trustedClient bool
		wantError     bool
	}{
		{name: "default rejects untrusted TLS", tls: true, wantError: true},
		{name: "TLS skip retains HTTPS", tls: true, config: Config{TLSSkipVerify: true}},
		{name: "trusted HTTPS client", tls: true, trustedClient: true},
		{name: "explicit HTTP", config: Config{PlainHTTP: true}},
		{name: "no HTTP fallback", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.TLS != nil) != tc.tls {
					t.Error("wrong protocol")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"name":"test/repo","tags":[]}`)
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			if tc.tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			if tc.trustedClient {
				tc.config.HTTPClient = server.Client()
			}
			client, err := NewClient(server.Listener.Addr().String(), "test/repo", tc.config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.repoClient.httpClient.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = client.List(ctx)
			if (err != nil) != tc.wantError {
				t.Fatalf("request error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
