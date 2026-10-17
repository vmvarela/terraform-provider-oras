package credentials

import (
	"context"
	"os"

	orasstate "github.com/vmvarela/oras-state"
	orasAuth "orasstate.land/oras-go/v2/registry/remote/auth"
)

// NewResolver returns the provider-owned OCI credential resolver.
//
// Precedence is intentionally the same as the provider used before extracting
// the shared state engine:
//  1. ORAS_TOKEN
//  2. GHCR_TOKEN / GITHUB_TOKEN for ghcr.io
//  3. Terraform CLI config and Docker/containers credential sources
//  4. anonymous
func NewResolver(registry, repository string) orasorasstate.CredentialFunc {
	registry = normalizeRegistryHost(registry)

	return func(ctx context.Context, _ string) (orasstate.Credential, error) {
		if token := os.Getenv("ORAS_TOKEN"); token != "" {
			return stateCredential(tokenCredential(registry, token)), nil
		}
		if registry == "ghcr.io" {
			if token := os.Getenv("GHCR_TOKEN"); token != "" {
				return stateCredential(tokenCredential(registry, token)), nil
			}
			if token := os.Getenv("GITHUB_TOKEN"); token != "" {
				return stateCredential(tokenCredential(registry, token)), nil
			}
		}

		if cred, ok := resolveConfiguredCredential(ctx, registry, repository); ok {
			return stateCredential(cred), nil
		}
		return orasstate.Credential{}, nil
	}
}

func tokenCredential(registry, token string) orasAuth.Credential {
	if registry == "ghcr.io" {
		// GHCR bearer tokens must go through the basic-auth token exchange.
		return orasAuth.Credential{Username: "x", Password: token}
	}
	return orasAuth.Credential{AccessToken: token}
}

func stateCredential(cred orasAuth.Credential) orasstate.Credential {
	return orasstate.Credential{
		Username:    cred.Username,
		Password:    cred.Password,
		AccessToken: cred.AccessToken,
	}
}
