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
| 3 | Configured credentials | Any registry (path-aware) | CLI config `oci_credentials` blocks (`.terraformrc` / `TF_CLI_CONFIG_FILE` / `TERRAFORM_CONFIG`), Docker config files (`~/.docker/config.json`, `containers/auth.json`), and Docker credential helpers — all pooled together; the **most specific match wins** (more repository path segments beats domain-only beats global fallback), CLI config wins ties |
| 4 | Anonymous | Public repos | No credentials (public repositories only) |

Configured-credential keys match the registry domain exactly plus a segment-wise path prefix (`ghcr.io/org` matches `ghcr.io/org/app` but not `ghcr.io/orgx/...`); there is no wildcard matching.

> **Note:** The provider does not currently support provider-level `username`/`password` or `token` arguments in the Terraform configuration. Credentials must come from the sources above.

## CLI Config (`oci_credentials` blocks)

For parity with the ghoten backend, the provider reads `oci_credentials` blocks from the Terraform CLI config file (discovered via `TF_CLI_CONFIG_FILE`, then `TERRAFORM_CONFIG`, then `~/.terraformrc`):

```hcl
# ~/.terraformrc
oci_credentials "ghcr.io" {
  username = "your-user"
  password = "your-token"
}

# Or a bearer token:
oci_credentials "registry.example.com" {
  access_token = "your-token"
}

# Or delegate to a Docker credential helper:
oci_credentials "ghcr.io" {
  docker_credentials_helper = "osxkeychain"
}

oci_default_credentials {
  docker_credentials_helper = "desktop"  # global fallback for unmatched registries
}
```

The config key supports an optional repository path prefix (`"ghcr.io/myorg"`), matching only repositories under that path.

## Docker Config Files and Credential Helpers

The provider also discovers credentials ambiently, like the Docker CLI:

1. `$XDG_RUNTIME_DIR/containers/auth.json` (if set)
2. `~/.config/containers/auth.json`
3. `$XDG_CONFIG_HOME/containers/auth.json` (default `~/.config`)
4. `~/.docker/config.json`

Supported keys: `auths` (base64 `username:password` entries), `credHelpers` (per-domain helper), `credsStore` (global helper). Helpers are invoked as `docker-credential-<name> get` with a 30s timeout; a helper that reports "not found" is skipped (falling through to the next source). Malformed config files are logged and skipped — the provider never fails resolution; it falls through to anonymous.

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