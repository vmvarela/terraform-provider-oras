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
- `delete:packages` — **required for `max_versions` retention.** When GHCR returns HTTP 405 on manifest
  deletion, the provider falls back to the GitHub Packages API. Without this scope, writes
  succeed but pruning fails. Evidence precision: the 405 fallback branch is unit-tested against
  a simulated 405 registry; live GHCR integration is env-gated and does not necessarily prove
  the 405 branch executed. Registry-specific, not portable.

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

## Transport and TLS

HTTPS with certificate verification is the default. Credentials still come from
the sources above; transport settings live in the provider configuration.
The `oci://` state-store URL identifies the repository and does not select HTTP.

| Configuration | Registry transport | Certificate verification |
|---|---|---|
| No options (or both new options false) | HTTPS | Enabled, system trust |
| `ca_file = "/path/to/ca.pem"` | HTTPS | Enabled, supplied CA bundle |
| `tls_skip_verify = true` | HTTPS | Disabled |
| `plain_http = true` | HTTP | No TLS |

For a private CA, keep verification enabled:

```hcl
provider "oras" {
  ca_file = "/path/to/ca-bundle.pem"
}
```

For a local HTTP registry such as the Zot development example:

```hcl
provider "oras" {
  plain_http = true
}
```

For an explicit HTTPS connection without certificate verification:

```hcl
provider "oras" {
  tls_skip_verify = true
}
```

Prefer a trusted CA over disabling verification. Plain HTTP does not encrypt
state or credentials. Neither a certificate error nor `tls_skip_verify` causes
an automatic fallback to HTTP. A custom CA bundle replaces the system trust
pool, preserving the existing `ca_file` behavior.

### Upgrade from `insecure`

`insecure` is deprecated but retains its previous behavior: `true` selects HTTP
and disables TLS certificate verification in the HTTP client; `false` retains
verified HTTPS. It is not reinterpreted as an HTTPS-only verification option.
There is no removal in this change.

- For existing HTTP configurations, replace `insecure = true` with
  `plain_http = true`.
- To intentionally change from HTTP to HTTPS without verification, remove
  `insecure` and set `tls_skip_verify = true`. Confirm the registry serves HTTPS.
- For verified HTTPS, remove `insecure = false`; retain `ca_file` if needed.
- Remove `insecure` before specifying either new option. Any non-null legacy
  and new option combination is an error, even when explicitly set to false.
  No option silently takes precedence.
- `plain_http = true` conflicts with `tls_skip_verify = true` or a non-empty
  `ca_file`. `tls_skip_verify = true` also conflicts with a non-empty `ca_file`:
  supplying trust anchors while disabling verification is rejected.
- Legacy `insecure` plus `ca_file` remains accepted for compatibility. With
  `insecure = true`, the file is still loaded but does not verify HTTP traffic.
  Remove it when migrating to `plain_http`, or switch to verified HTTPS.

Unknown settings are deferred during validation, but must be resolved before
provider configuration. This change does not alter state storage or credential
precedence and requires no state migration.

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

**405 on delete** — observed when GHCR refuses manifest deletion (the 405 fallback is unit-tested
with a simulated 405 registry; live GHCR integration is env-gated and does not necessarily prove
the 405 branch). The Packages API fallback needs `delete:packages`.

**`x509: certificate signed by unknown authority`** — configure the correct `ca_file`.
For an explicit local HTTPS bypass, use `tls_skip_verify = true`; it retains HTTPS.
Use `plain_http = true` only for a registry that actually serves HTTP.
