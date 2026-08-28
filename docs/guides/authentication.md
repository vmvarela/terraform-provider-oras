---
page_title: "Authentication"
description: |-
  Configure authentication for the ORAS provider to access OCI registries.
---

# Authentication

The `oras` provider supports multiple authentication methods for accessing OCI registries. Credentials are resolved with a defined priority order, allowing flexible configuration for different environments.

## Credential Resolution Order

The provider resolves credentials in the following priority order:

| Priority | Method | Scope | Description |
|----------|--------|-------|-------------|
| 1 | `ORAS_TOKEN` | Any registry | Universal token via environment variable |
| 2 | `GHCR_TOKEN` / `GITHUB_TOKEN` | ghcr.io only | GitHub Container Registry specific tokens |
| 3 | Anonymous | Public repos | No credentials (public repositories only) |

> **Note:** The provider does not currently support provider-level `username`/`password` or `token` arguments in the Terraform configuration. Credentials must be provided via environment variables.

## Environment Variables

### `ORAS_TOKEN` (Recommended)

Universal token that works with any OCI-compatible registry:

```bash
export ORAS_TOKEN=your-registry-token
```

This is the preferred method for CI/CD pipelines and works across all registry types (GHCR, Docker Hub, Harbor, Zot, etc.).

### `GHCR_TOKEN` / `GITHUB_TOKEN`

For GitHub Container Registry (ghcr.io), you can use either:

```bash
# GHCR-specific token (recommended)
export GHCR_TOKEN=ghp_xxxxxxxxxxxx

# Or use a classic GitHub Personal Access Token
export GITHUB_TOKEN=ghp_xxxxxxxxxxxx
```

These are only checked when the registry hostname is `ghcr.io`.

#### Required GHCR Token Scopes

For full functionality including state retention (pruning old versions), the token needs:

- `read:packages` — Read state from registry
- `write:packages` — Write state to registry
- `delete:packages` — **Required** for `max_versions` retention to work (GHCR returns HTTP 405 on manifest deletion, requiring the GitHub Packages API)

Without `delete:packages`, the provider will write state successfully but version pruning will fail silently.

## Per-Registry Configuration

### GHCR (ghcr.io)

```bash
export GHCR_TOKEN=ghp_xxxxxxxxxxxx
# Or
export GITHUB_TOKEN=ghp_xxxxxxxxxxxx
```

### Docker Hub

```bash
export ORAS_TOKEN=your-docker-hub-access-token
```

### Harbor / Self-Hosted Registries

```bash
export ORAS_TOKEN=your-harbor-token
```

### Anonymous Access

For public repositories, no credentials are needed:

```bash
# No environment variables required
terraform init
```

## Provider-Level TLS Configuration

While credentials come from environment variables, TLS settings are configured in the provider block:

```hcl
provider "oras" {
  # Skip TLS verification (for local registries like Zot on HTTP)
  insecure = true

  # Custom CA bundle for private registries with self-signed certs
  ca_file  = "/path/to/ca-bundle.pem"
}
```

| Argument   | Type | Default | Description |
|------------|:----:|---------|-------------|
| `insecure` | bool | `false` | Skip TLS certificate verification |
| `ca_file`  | string | — | Path to PEM-encoded CA certificate bundle |

## CI/CD Examples

### GitHub Actions (GHCR)

```yaml
env:
  TF_ENABLE_PLUGGABLE_STATE_STORAGE: "1"
  GHCR_TOKEN: ${{ secrets.GITHUB_TOKEN }}

steps:
  - uses: actions/checkout@v4
  - uses: hashicorp/setup-terraform@v3
  - run: terraform init
  - run: terraform apply -auto-approve
```

### GitLab CI (Any Registry)

```yaml
variables:
  TF_ENABLE_PLUGGABLE_STATE_STORAGE: "1"
  ORAS_TOKEN: $CI_REGISTRY_PASSWORD

before_script:
  - terraform init

script:
  - terraform apply -auto-approve
```

### Generic CI with ORAS_TOKEN

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
export ORAS_TOKEN=$REGISTRY_TOKEN
terraform init
terraform apply
```

## Troubleshooting

### "Authentication required" / 401 Errors

1. Verify the token is set in the correct environment variable
2. Check token scopes (especially `delete:packages` for GHCR)
3. Ensure the token hasn't expired
4. For GHCR, verify the repository exists and token has access

### "Manifest deletion not supported" / 405 Errors on GHCR

This is expected behavior. The provider automatically falls back to the GitHub Packages API, but this requires the `delete:packages` scope on your token. Without it, version pruning (`max_versions`) will not work.

### TLS Certificate Errors

For registries with self-signed certificates:

```hcl
provider "oras" {
  ca_file = "/etc/ssl/certs/my-registry-ca.pem"
}
```

For local development with plain HTTP (e.g., Zot):

```hcl
provider "oras" {
  insecure = true
}
```

## Security Best Practices

1. **Never hardcode tokens** in Terraform configuration files
2. **Use short-lived tokens** in CI/CD pipelines (GitHub Actions `GITHUB_TOKEN` is ideal)
3. **Scope tokens minimally** — only grant `read:packages`, `write:packages`, and `delete:packages` as needed
4. **Rotate tokens regularly** — especially for long-running environments
5. **Use `ORAS_TOKEN`** for portability across registry providers