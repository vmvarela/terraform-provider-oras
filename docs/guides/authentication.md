---
page_title: "Authentication"
description: |-
  Configure authentication for the ORAS provider to access OCI registries.
---

# Authentication

Credentials are resolved in priority order. The first source that yields a credential wins.

| Priority | Method | Scope |
|----------|--------|-------|
| 1 | `ORAS_TOKEN` | Any registry |
| 2 | `GHCR_TOKEN`, then `GITHUB_TOKEN` | ghcr.io only |
| 3 | Configured credentials (below) | Any registry, path-aware |
| 4 | Anonymous | Public repositories |

There are no provider-level `username` / `password` / `token` arguments — credentials never live in
your Terraform configuration.

## Environment Variables

`ORAS_TOKEN` is the portable choice and works with every OCI registry:

```bash
export ORAS_TOKEN=your-registry-token
```

For ghcr.io, `GHCR_TOKEN` and `GITHUB_TOKEN` are also checked. Required scopes:

- `read:packages` — read state
- `write:packages` — write state
- `delete:packages` — **required for `max_versions` retention.** GHCR returns HTTP 405 on manifest
  deletion, so the provider falls back to the GitHub Packages API. Without this scope, writes
  succeed but pruning fails.

## Configured Credentials

Three sources are pooled together and matched against the registry domain and repository path. The
**most specific key wins** — more matching path segments beats domain-only, which beats the global
fallback. CLI config wins ties. Keys match exactly; there is no wildcard support.

### CLI config `oci_credentials` blocks

Read from `TF_CLI_CONFIG_FILE`, then `TERRAFORM_CONFIG`, then `~/.terraformrc`:

```hcl
oci_credentials "ghcr.io" {
  username = "your-user"
  password = "your-token"
}

oci_credentials "registry.example.com" {
  access_token = "your-token"
}

oci_credentials "ghcr.io/myorg" {          # only repos under myorg/
  docker_credentials_helper = "osxkeychain"
}

oci_default_credentials {
  docker_credentials_helper = "desktop"     # global fallback
}
```

Each block must use exactly one credential group: `username`+`password`, `access_token`, or
`docker_credentials_helper`.

### Docker config files

Searched in order:

1. `$XDG_CONFIG_HOME/containers/auth.json` (default `~/.config`)
2. `~/.docker/config.json`

Supported keys: `auths` (base64 `username:password`), `credHelpers` (per-domain), `credsStore`
(global).

### Credential helpers

Invoked as `docker-credential-<name> get` with a 30s timeout. A helper reporting "not found" is
skipped and resolution falls through. Malformed config files are logged and skipped — resolution
never fails, it degrades to anonymous.

## TLS

Credentials come from the sources above; TLS lives in the provider block:

```hcl
provider "oras" {
  insecure = true                            # plain HTTP / skip verification (local dev)
  ca_file  = "/path/to/ca-bundle.pem"        # self-signed registries
}
```

## CI Example

```yaml
env:
  TF_ENABLE_PLUGGABLE_STATE_STORAGE: "1"
  GHCR_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

The default Actions `GITHUB_TOKEN` has `read:packages` and `write:packages` but **not**
`delete:packages` — use a PAT if you need retention.

## Troubleshooting

**401 / "authentication required"** — check the variable name, the token scopes, and expiry. For
ghcr.io, confirm the token can see the package.

**405 on delete** — expected on GHCR. The Packages API fallback needs `delete:packages`.

**`x509: certificate signed by unknown authority`** — set `ca_file`, or `insecure = true` for local
plain-HTTP registries.
