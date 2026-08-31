// Package oras provides OCI registry operations for the Terraform state backend.
package oras

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/vmvarela/terraform-provider-oras/internal/httputil"
	orasRemote "oras.land/oras-go/v2/registry/remote"
	orasAuth "oras.land/oras-go/v2/registry/remote/auth"
)

// Version is the provider version, set at build time via -ldflags (e.g. -X 'oras.Version=1.0.0').
// Defaults to "dev" for development builds.
var Version = "dev"

// userAgent returns the User-Agent string for HTTP requests.
func userAgent() string {
	return "terraform-provider-oras/" + Version
}

// Config holds all configurable options for the ORAS client.
type Config struct {
	Insecure     bool
	CAFile       string
	Username     string
	Password     string
	Token        string
	Compression  bool // gzip when true
	LockTTL      time.Duration
	MaxVersions  int
	MaxStateSize int64
	HTTPClient   *http.Client
}

// Client is the top-level ORAS client that holds the shared OCI repository
// client and configuration for state operations.
type Client struct {
	repoClient   *orasRepositoryClient
	config       Config
	retentionSem chan struct{}
	retentionWg  sync.WaitGroup
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
	repository   string // full reference: registry/repository
	inner        orasRepository
	token        string              // access token (for GHCR API calls)
	resolvedCred orasAuth.Credential // resolved credential, whatever path produced it
	httpClient   *http.Client
}

// accessToken returns the access token for GHCR API calls. Falls back to the
// resolved credential (AccessToken, then Password) so that configured
// credentials (CLI config, Docker config files, helpers) also work for the
// GHCR delete fallback, not just env/explicit tokens.
func (r *orasRepositoryClient) accessToken(_ context.Context) (string, error) {
	if r.token != "" {
		return r.token, nil
	}
	if r.resolvedCred.AccessToken != "" {
		return r.resolvedCred.AccessToken, nil
	}
	return r.resolvedCred.Password, nil
}

// NewClient creates an ORAS client for the given registry and repository.
// The registry parameter is the registry host (e.g. "ghcr.io" or "registry.example.com:5000").
// The repository parameter is the repository name (e.g. "myorg/infra-state").
func NewClient(registry, repository string, cfg Config) (*Client, error) {
	repoClient, err := newORASRepositoryClient(registry, repository, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create ORAS repository client: %w", err)
	}

	return &Client{
		repoClient:   repoClient,
		config:       cfg,
		retentionSem: make(chan struct{}, 3),
	}, nil
}

// newORASRepositoryClient creates the underlying ORAS repository client with
// the given configuration.
func newORASRepositoryClient(registry, repository string, cfg Config) (*orasRepositoryClient, error) {
	fullRef := registry + "/" + repository

	repo, err := orasRemote.NewRepository(fullRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create remote repository: %w", err)
	}

	if cfg.Insecure {
		repo.PlainHTTP = true
	}

	// Use provided HTTP client if set, otherwise build one
	var httpClient *http.Client
	if cfg.HTTPClient != nil {
		httpClient = cfg.HTTPClient
	} else {
		httpClient, err = httputil.BuildHTTPClient(cfg.Insecure, cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP client: %w", err)
		}
		// Add User-Agent header for ORAS registry operations
		httpClient = &http.Client{
			Transport: &userAgentRoundTripper{
				next:      httpClient.Transport,
				userAgent: userAgent(),
			},
		}
	}

	// Resolve credentials with the following priority:
	// 1. Explicit token
	// 2. Explicit username/password
	// 3. Environment variables
	// 4. Terraform CLI config / Docker config files
	cred, resolvedToken, resolvedCred := resolveCredentials(registry, repository, cfg)

	repo.Client = &orasAuth.Client{
		Client:     httpClient,
		Credential: cred,
		Cache:      orasAuth.NewCache(),
	}

	return &orasRepositoryClient{
		repository:   fullRef,
		inner:        repo,
		token:        resolvedToken,
		resolvedCred: resolvedCred,
		httpClient:   httpClient,
	}, nil
}

// resolveCredentials builds a credential function using the following priority:
//
// Credential Resolution Order:
//  1. Explicit token from cfg.Token (set via Config.Token)
//  2. Explicit username/password from cfg.Username/cfg.Password (set via Config.Username/Config.Password)
//  3. ORAS_TOKEN environment variable (applies to any registry)
//  4. For ghcr.io only:
//     a. GHCR_TOKEN environment variable
//     b. GITHUB_TOKEN environment variable
//  5. Configured credentials (Terraform CLI config oci_credentials blocks,
//     Docker/containers config files, Docker credential helpers) resolved by
//     specificity via resolveConfiguredCredential
//  6. Anonymous access (EmptyCredential)
//
// Environment Variable Precedence:
//   - ORAS_TOKEN: Universal token for any OCI registry
//   - GHCR_TOKEN: GitHub Container Registry specific token (ghcr.io only)
//   - GITHUB_TOKEN: GitHub API token, can be used for ghcr.io (ghcr.io only)
//
// Pre:  registry is a non-empty hostname string.
// Post: returns a non-nil CredentialFunc; the returned token is non-empty only
//
//	when an access token was resolved (cases 1, 3, 4a, 4b above); the
//	returned Credential is the underlying credential regardless of which
//	path resolved it (EmptyCredential for anonymous).
//
// tokenCredential converts a bearer-style token into a credential the
// registry can actually authenticate. GHCR rejects tokens sent raw as
// Bearer — oras-go skips the exchange when AccessToken is set — and
// instead requires basic auth against its token endpoint; any non-empty
// username is accepted (Docker CLI convention). Other registries keep the
// AccessToken as-is.
func tokenCredential(registry, token string) orasAuth.Credential {
	if registry == "ghcr.io" {
		return orasAuth.Credential{Username: "x", Password: token}
	}
	return orasAuth.Credential{AccessToken: token}
}

func resolveCredentials(registry, repository string, cfg Config) (orasAuth.CredentialFunc, string, orasAuth.Credential) {
	// Priority 1: Explicit token
	if cfg.Token != "" {
		cred := tokenCredential(registry, cfg.Token)
		return orasAuth.StaticCredential(registry, cred), cfg.Token, cred
	}

	// Priority 2: Explicit username/password
	if cfg.Username != "" || cfg.Password != "" {
		cred := orasAuth.Credential{Username: cfg.Username, Password: cfg.Password}
		return orasAuth.StaticCredential(registry, cred), "", cred
	}

	// Priority 3: Environment variable fallback
	// Check ORAS_TOKEN (applies to any registry)
	if t := os.Getenv("ORAS_TOKEN"); t != "" {
		cred := tokenCredential(registry, t)
		return orasAuth.StaticCredential(registry, cred), t, cred
	}

	// For ghcr.io, also check GHCR_TOKEN and GITHUB_TOKEN
	if registry == "ghcr.io" {
		if t := os.Getenv("GHCR_TOKEN"); t != "" {
			cred := tokenCredential(registry, t)
			return orasAuth.StaticCredential(registry, cred), t, cred
		}
		if t := os.Getenv("GITHUB_TOKEN"); t != "" {
			cred := tokenCredential(registry, t)
			return orasAuth.StaticCredential(registry, cred), t, cred
		}
	}

	// Priority 4: Configured credentials (Terraform CLI config, Docker config
	// files, Docker credential helpers), matched by specificity against the
	// registry domain and repository path.
	if cred, ok := resolveConfiguredCredential(context.Background(), registry, repository); ok {
		return orasAuth.StaticCredential(registry, cred), "", cred
	}

	// Fall back to anonymous access
	return orasAuth.StaticCredential(registry, orasAuth.EmptyCredential), "", orasAuth.EmptyCredential
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
