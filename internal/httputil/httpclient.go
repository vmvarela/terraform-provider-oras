// Package httputil provides shared HTTP client construction utilities.
package httputil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// BuildHTTPClient constructs an *http.Client with the specified TLS settings.
// When insecure is true, certificate verification is disabled.
// When caFile is non-empty, it is loaded as the trusted CA pool.
// The returned client uses the default transport with the configured TLS settings.
func BuildHTTPClient(insecure bool, caFile string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	t := transport.Clone()

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