---
page_title: "oras Provider"
description: |-
  Stores Terraform state in any OCI-compatible registry using the ORAS protocol.
---

# oras Provider

~> **Experimental:** This provider requires Terraform 1.17+ alpha builds with the pluggable state storage experiment enabled. It will not work with any stable Terraform release. The statestore plugin API is experimental and may break across alpha releases.

The `oras` provider implements Terraform's `statestore.StateStore` plugin interface to store Terraform state in any OCI-compatible registry — including GHCR, Docker Hub, Zot, or a self-hosted registry — using the ORAS (OCI Registry As Storage) protocol via OCI artifact manifests.

This provider has **no resources and no data sources** — it only exposes the `oras_oci` state store via `ProviderWithStateStores`.

## Requirements

- **Terraform:** 1.17+ alpha build (see `.terraform-version` for pinned version)
- **Environment:** `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` (or pass `-enable-pluggable-state-storage-experiment` on `terraform init`)

## Example Usage

```hcl
terraform {
  required_providers {
    oras = {
      source  = "registry.terraform.io/vmvarela/oras"
      version = "~> 0.1"
    }
  }

  state_store "oras_oci" {
    provider = oras

    url          = "oci://ghcr.io/myorg/infra-tfstate"
    compression  = true
    lock_ttl     = "15m"
    max_versions = 10
  }
}

provider "oras" {}
```

With environment auth before `terraform init` / `terraform apply`:

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
export GHCR_TOKEN=ghp_xxxxxxxxxxxx
terraform init
terraform apply
```

For a local registry (e.g. Zot on plain HTTP), use the `insecure` provider flag:

```hcl
terraform {
  state_store "oras_oci" {
    provider = oras
    url      = "oci://localhost:5001/estado"
  }
}

provider "oras" {
  insecure = true
}
```

## Authentication

Auth is resolved with the following priority:

1. `ORAS_TOKEN` — Universal token for any OCI registry
2. `GHCR_TOKEN` / `GITHUB_TOKEN` — GitHub Container Registry specific (ghcr.io only)
3. Anonymous access (public repositories only)

See the [Authentication guide](/guides/authentication) for detailed configuration.

## State Store Configuration

The `oras_oci` state store supports the following arguments:

| Argument         | Required | Default      | Description |
|------------------|:--------:|--------------|-------------|
| `url`            | ✓        | —            | OCI registry URL in the format `oci://registry/repository` |
| `compression`    |          | `false`      | Enable gzip compression for state data |
| `lock_ttl`       |          | —            | Duration for state lock TTL (e.g., `15m`, `1h`) |
| `max_versions`   |          | `0` (unlimited) | Maximum number of state versions to retain |
| `max_state_size` |          | `268435456` (256 MiB) | Maximum allowed state size in bytes |

See the [State Store Configuration guide](/guides/state-store) for detailed documentation.

## Provider Configuration

The provider block supports TLS configuration:

| Argument   | Required | Default | Description |
|------------|:--------:|---------|-------------|
| `insecure` |          | `false` | Skip TLS certificate verification (for local registries like Zot) |
| `ca_file`  |          | —       | Path to a PEM-encoded CA certificate bundle |

## Guides

- [Authentication](/guides/authentication) — Configure credentials for your registry
- [State Store Configuration](/guides/state-store) — Detailed state store arguments and behavior
- [Local Registry (Zot)](/guides/local-registry) — Run a local OCI registry for development
- [State Migration](/guides/migration) — Migrate existing state to the ORAS backend

## Limitations

- Requires Terraform **1.17.x alpha** (or any Terraform alpha build with pluggable state storage experiments enabled). Terraform stable releases (including 1.16.x and 1.17.0 stable) do NOT support pluggable state storage — it is gated to alpha/dev builds only.
- GHCR needs a token with `delete:packages` scope for retention (pruning old versions) to work.
- No built-in migration tool. Use `terraform state pull` / `terraform state push` to move existing state into this provider.
- Max state size defaults to 256 MiB (configurable via `max_state_size`).

## Tag Scheme

Each workspace maps to a set of OCI tags on the repository:

| Tag Pattern | Purpose |
|-------------|---------|
| `state-<workspace>` | Current state |
| `state-<workspace>-v<N>` | Versioned snapshots (when `max_versions > 0`) |
| `locked-<workspace>` / `unlocked-<workspace>` | Lock state (GHCR fallback) |

Locking uses generation-based optimistic concurrency. Each `Lock` call increments a counter on the lock manifest, so simultaneous attempts are detected reliably. Stale locks auto-expire after `lock_ttl` (checked on each Lock call, no background goroutines).

Version retention runs asynchronously after each `Put` — a goroutine pool (cap 3) prunes the oldest versions when `max_versions` is exceeded.

GHCR returns HTTP 405 on manifest deletion. The provider detects this and falls back to the GitHub Packages API (`delete:packages` scope required).

## Development

```bash
make test                           # unit tests (no deps)
TF_ORAS_ZOT_TEST=1 make test-zot   # integration: spins Zot via Docker
make install                        # build + install to ~/.terraform.d/...
cp .terraformrc.dev ~/.terraformrc  # dev override
```

See [`examples/main.tf`](../examples/main.tf) for a runnable local example (Zot over plain HTTP).