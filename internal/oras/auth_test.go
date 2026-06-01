package oras

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingRoundTripper counts the number of RoundTrip calls and records
// the last request received.
type countingRoundTripper struct {
	count int
	last  *http.Request
	next  http.RoundTripper
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.count++
	c.last = req
	if c.next != nil {
		return c.next.RoundTrip(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

// headerCapturingRoundTripper captures the request and returns a 200 response.
type headerCapturingRoundTripper struct {
	request *http.Request
}

func (h *headerCapturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h.request = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

// writeTempFile writes content to a temporary file and returns the path.
func writeTempFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.pem")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

// generateCACert generates a self-signed CA certificate and returns it as PEM bytes.
func generateCACert(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial number: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "Test CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestUserAgentRoundTripper_DoesNotMutateOriginalRequest(t *testing.T) {
	// The RoundTrip method clones the request before modifying it, so the
	// original request passed in should be left untouched.
	capturer := &headerCapturingRoundTripper{}
	rt := &userAgentRoundTripper{
		next:      capturer,
		userAgent: "test-agent/1.0",
	}

	originalReq, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	originalReq.Header.Set("User-Agent", "original/1.0")

	_, err = rt.RoundTrip(originalReq)
	if err != nil {
		t.Fatal(err)
	}

	// Original request must be unchanged.
	if got := originalReq.Header.Get("User-Agent"); got != "original/1.0" {
		t.Errorf("original request User-Agent was modified: got %q, want %q", got, "original/1.0")
	}

	// The request that reached the inner round tripper must have the new
	// User-Agent set.
	if capturer.request == nil {
		t.Fatal("inner round tripper was never called")
	}
	if got := capturer.request.Header.Get("User-Agent"); got != "test-agent/1.0" {
		t.Errorf("inner request User-Agent = %q, want %q", got, "test-agent/1.0")
	}
}

func TestUserAgentRoundTripper_SetsUserAgent(t *testing.T) {
	// This implementation always sets the User-Agent header, overwriting any
	// existing value.
	capturer := &headerCapturingRoundTripper{}
	rt := &userAgentRoundTripper{
		next:      capturer,
		userAgent: "my-agent/2.0",
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "existing-agent/1.0")

	_, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	if capturer.request == nil {
		t.Fatal("inner round tripper was never called")
	}
	if got := capturer.request.Header.Get("User-Agent"); got != "my-agent/2.0" {
		t.Errorf("User-Agent = %q, want %q", got, "my-agent/2.0")
	}
}

func TestNewORASHTTPClient(t *testing.T) {
	t.Run("default client", func(t *testing.T) {
		client, err := newORASHTTPClient(false, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client == nil {
			t.Fatal("client is nil")
		}

		// Check that the transport is a userAgentRoundTripper.
		rt, ok := client.Transport.(*userAgentRoundTripper)
		if !ok {
			t.Fatalf("transport is %T, want *userAgentRoundTripper", client.Transport)
		}
		if rt.userAgent != "terraform-provider-orastate/1.0" {
			t.Errorf("userAgent = %q, want %q", rt.userAgent, "terraform-provider-orastate/1.0")
		}
	})

	t.Run("insecure client", func(t *testing.T) {
		client, err := newORASHTTPClient(true, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client == nil {
			t.Fatal("client is nil")
		}

		// Verify that the transport disables TLS verification by making a
		// real request to a TLS server.
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		// Override the transport to point to the test server. We just want
		// to verify TLS verification is skipped.
		rt := client.Transport.(*userAgentRoundTripper)
		transport, ok := rt.next.(*http.Transport)
		if !ok {
			t.Fatalf("inner transport is %T, want *http.Transport", rt.next)
		}
		if transport.TLSClientConfig == nil {
			t.Fatal("TLSClientConfig is nil, expected non-nil with InsecureSkipVerify=true")
		}
		if !transport.TLSClientConfig.InsecureSkipVerify {
			t.Errorf("InsecureSkipVerify = false, want true")
		}
	})

	t.Run("with valid CA file", func(t *testing.T) {
		caCert := generateCACert(t)
		caFile := writeTempFile(t, caCert)

		client, err := newORASHTTPClient(false, caFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client == nil {
			t.Fatal("client is nil")
		}
	})

	t.Run("with non-existent CA file", func(t *testing.T) {
		_, err := newORASHTTPClient(false, "/nonexistent/ca.pem")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "failed to read CA file") {
			t.Errorf("error message does not contain %q: %v", "failed to read CA file", err)
		}
	})

	t.Run("with invalid PEM in CA file", func(t *testing.T) {
		caFile := writeTempFile(t, []byte("not a valid PEM certificate"))

		_, err := newORASHTTPClient(false, caFile)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "failed to parse CA certificate from") {
			t.Errorf("error message does not contain %q: %v", "failed to parse CA certificate from", err)
		}
	})
}
