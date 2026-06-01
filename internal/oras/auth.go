// Package oras provides OCI registry operations for the Terraform state backend.
package oras

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasRemote "oras.land/oras-go/v2/registry/remote"
	orasAuth "oras.land/oras-go/v2/registry/remote/auth"
)

// Client is the top-level ORAS client that holds the shared OCI repository
// client and configuration for state operations.
type Client struct {
	repoClient   *orasRepositoryClient
	config       clientConfig
	now          func() time.Time // injectable for testing
	retentionSem chan struct{}
	retentionWg  sync.WaitGroup
}

// clientConfig holds all configurable options for the ORAS client.
type clientConfig struct {
	insecure    bool
	caFile      string
	username    string
	password    string
	token       string
	compression string // "none" or "gzip"
	lockTTL     time.Duration
	maxVersions int
	maxStateSize int64
	retryConfig RetryConfig
}

// Option configures a Client.
type Option func(*clientConfig)

// WithInsecure configures whether to use plain HTTP instead of HTTPS.
func WithInsecure(insecure bool) Option {
	return func(c *clientConfig) {
		c.insecure = insecure
	}
}

// WithCAFile sets a custom CA certificate file for TLS verification.
func WithCAFile(caFile string) Option {
	return func(c *clientConfig) {
		c.caFile = caFile
	}
}

// WithCredentials sets explicit username and password for registry authentication.
func WithCredentials(username, password string) Option {
	return func(c *clientConfig) {
		c.username = username
		c.password = password
	}
}

// WithToken sets a bearer access token for registry authentication.
func WithToken(token string) Option {
	return func(c *clientConfig) {
		c.token = token
	}
}

// WithCompression enables or disables compression for state data.
// When enabled (true), "gzip" compression is used; when disabled (false), "none" is used.
func WithCompression(enabled bool) Option {
	return func(c *clientConfig) {
		if enabled {
			c.compression = "gzip"
		} else {
			c.compression = "none"
		}
	}
}

// WithLockTTL sets the TTL for state locks.
func WithLockTTL(ttl time.Duration) Option {
	return func(c *clientConfig) {
		c.lockTTL = ttl
	}
}

// WithMaxVersions sets the maximum number of state versions to retain.
func WithMaxVersions(n int) Option {
	return func(c *clientConfig) {
		c.maxVersions = n
	}
}

// WithMaxStateSize sets the maximum allowed state size in bytes.
func WithMaxStateSize(n int64) Option {
	return func(c *clientConfig) {
		c.maxStateSize = n
	}
}

// WithRetryConfig sets a custom retry configuration for HTTP operations.
func WithRetryConfig(cfg RetryConfig) Option {
	return func(c *clientConfig) {
		c.retryConfig = cfg
	}
}

// orasRepository is the minimal interface for OCI repository operations
// required by the state backend.
type orasRepository interface {
	Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error
	Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error)
	Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error)
	Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error
	Delete(ctx context.Context, target ocispec.Descriptor) error
	Tags(ctx context.Context, last string, fn func(tags []string) error) error
}

// orasRepositoryClient wraps the ORAS repository client with additional
// configuration for the state backend.
type orasRepositoryClient struct {
	repository string       // full reference: registry/repository
	inner      orasRepository
	token      string       // access token (for GHCR API calls)
	httpClient *http.Client
}

// accessTokenForHost returns the access token for the given host.
// The host parameter is accepted for future use but currently ignored;
// the stored token is returned directly.
// Used by ghcr.go for GitHub API calls.
func (r *orasRepositoryClient) accessTokenForHost(ctx context.Context, host string) (string, error) {
	return r.token, nil
}

// NewClient creates an ORAS client for the given registry and repository.
// The registry parameter is the registry host (e.g. "ghcr.io" or "registry.example.com:5000").
// The repository parameter is the repository name (e.g. "myorg/infra-state").
func NewClient(registry, repository string, opts ...Option) (*Client, error) {
	cfg := clientConfig{
		compression: "none",
		retryConfig: DefaultRetryConfig(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	fullRef := registry + "/" + repository

	repoClient, err := newORASRepositoryClient(registry, repository, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create ORAS repository client: %w", err)
	}

	repoClient.repository = fullRef

	return &Client{
		repoClient:   repoClient,
		config:       cfg,
		now:          time.Now,
		retentionSem: make(chan struct{}, 3),
	}, nil
}

// newORASRepositoryClient creates the underlying ORAS repository client with
// the given configuration.
func newORASRepositoryClient(registry, repository string, cfg clientConfig) (*orasRepositoryClient, error) {
	fullRef := registry + "/" + repository

	repo, err := orasRemote.NewRepository(fullRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create remote repository: %w", err)
	}

	if cfg.insecure {
		repo.PlainHTTP = true
	}

	httpClient, err := newORASHTTPClient(cfg.insecure, cfg.caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP client: %w", err)
	}

	// Resolve credentials with the following priority:
	// 1. Explicit token
	// 2. Explicit username/password
	// 3. Environment variables
	cred, resolvedToken := resolveCredentials(registry, cfg)

	repo.Client = &orasAuth.Client{
		Client:     httpClient,
		Credential: cred,
	}

	return &orasRepositoryClient{
		repository: fullRef,
		inner:      repo,
		token:      resolvedToken,
		httpClient: httpClient,
	}, nil
}

// resolveCredentials builds a credential function using the following priority:
//
//  1. Explicit token from cfg.token
//  2. Explicit username/password from cfg.username/cfg.password
//  3. ORAS_TOKEN environment variable
//  4. For ghcr.io: GHCR_TOKEN, then GITHUB_TOKEN
//  5. Anonymous access (EmptyCredential)
//
// Pre:  registry is a non-empty hostname string.
// Post: returns a non-nil CredentialFunc; the returned token is non-empty only
//       when an access token was resolved (cases 1, 3, 4 above).
func resolveCredentials(registry string, cfg clientConfig) (orasAuth.CredentialFunc, string) {
	// Priority 1: Explicit token
	if cfg.token != "" {
		return orasAuth.StaticCredential(registry, orasAuth.Credential{
			AccessToken: cfg.token,
		}), cfg.token
	}

	// Priority 2: Explicit username/password
	if cfg.username != "" || cfg.password != "" {
		return orasAuth.StaticCredential(registry, orasAuth.Credential{
			Username: cfg.username,
			Password: cfg.password,
		}), ""
	}

	// Priority 3: Environment variable fallback
	// Check ORAS_TOKEN (applies to any registry)
	if t := os.Getenv("ORAS_TOKEN"); t != "" {
		return orasAuth.StaticCredential(registry, orasAuth.Credential{
			AccessToken: t,
		}), t
	}

	// For ghcr.io, also check GHCR_TOKEN and GITHUB_TOKEN
	if registry == "ghcr.io" {
		if t := os.Getenv("GHCR_TOKEN"); t != "" {
			return orasAuth.StaticCredential(registry, orasAuth.Credential{
				AccessToken: t,
			}), t
		}
		if t := os.Getenv("GITHUB_TOKEN"); t != "" {
			return orasAuth.StaticCredential(registry, orasAuth.Credential{
				AccessToken: t,
			}), t
		}
	}

	// Fall back to anonymous access
	return orasAuth.StaticCredential(registry, orasAuth.EmptyCredential), ""
}

// newORASHTTPClient builds an HTTP client configured for OCI registry access.
// It configures TLS settings based on the insecure and caFile parameters and
// adds a User-Agent header.
func newORASHTTPClient(insecure bool, caFile string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}

	// Clone the default transport to avoid mutating the global default.
	tlsConfig := &tls.Config{}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}

	if insecure {
		tlsConfig.InsecureSkipVerify = true
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file %q: %w", caFile, err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate from %q", caFile)
		}
		if tlsConfig.RootCAs == nil {
			tlsConfig.RootCAs, _ = x509.SystemCertPool()
			if tlsConfig.RootCAs == nil {
				tlsConfig.RootCAs = x509.NewCertPool()
			}
		}
		tlsConfig.RootCAs.AppendCertsFromPEM(caCert)
	}

	transport = transport.Clone()
	transport.TLSClientConfig = tlsConfig

	client := &http.Client{
		Transport: &userAgentRoundTripper{
			next: transport,
			userAgent: "terraform-provider-oras/1.0",
		},
	}

	return client, nil
}

// userAgentRoundTripper wraps an http.RoundTripper to add a custom User-Agent
// header to every request.
type userAgentRoundTripper struct {
	next      http.RoundTripper
	userAgent string
}

// RoundTrip implements http.RoundTripper.
func (u *userAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", u.userAgent)
	return u.next.RoundTrip(req)
}
