---
page_title: "oras Provider"
description: |-
  Stores Terraform state in any OCI-compatible registry using the ORAS protocol.
---

# oras Provider

~> **Experimental:** Requires a Terraform 1.17+ alpha build with the pluggable state storage experiment enabled. It will not work with any stable Terraform release, and the statestore plugin API may break across alpha releases.

Implements Terraform's `statestore.StateStore` interface to keep state in an OCI registry — GHCR, Docker Hub, Harbor, Zot, or self-hosted — as OCI artifact manifests.

No resources, no data sources. The provider exists solely to expose the `oras_oci` state store.

## Requirements

- Terraform 1.17+ alpha (see `.terraform-version`)
- `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1`, or `terraform init -enable-pluggable-state-storage-experiment`
- If a pre-release version is published (e.g. `0.1.6-alpha`), pin it exactly (`version = "0.1.6-alpha"`): Terraform's range operators (`~>`, `>=`, …) never select pre-release versions

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

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
export GHCR_TOKEN=ghp_xxxxxxxxxxxx
terraform init
terraform apply
```

For a local registry over plain HTTP (e.g. Zot), set `insecure`:

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

A runnable version lives in [`examples/main.tf`](../examples/main.tf).

## State Store Arguments

| Argument         | Required | Default               | Description |
|------------------|:--------:|-----------------------|-------------|
| `url`            | ✓        | —                     | `oci://<registry>/<repository>`; registry may include a port |
| `compression`    |          | `false`               | Gzip the state layer |
| `lock_ttl`       |          | —                     | Go duration (`15m`, `1h`); stale locks past this lease are cleared on the next `Lock`. Unset means locks never expire |
| `max_versions`   |          | `0` (unlimited)       | Versions retained per workspace. `1` keeps only the current state |
| `max_state_size` |          | `268435456` (256 MiB) | Hard read limit; guards against a corrupted or malicious layer |

## Provider Arguments

| Argument   | Required | Default | Description |
|------------|:--------:|---------|-------------|
| `insecure` |          | `false` | Skip TLS verification and use plain HTTP (local registries) |
| `ca_file`  |          | —       | PEM-encoded CA bundle for self-signed registries |

## Authentication

Resolved in priority order: `ORAS_TOKEN` (any registry) → `GHCR_TOKEN` / `GITHUB_TOKEN` (ghcr.io only) → configured credentials (`.terraformrc` `oci_credentials` blocks, Docker config files, credential helpers) → anonymous.

See the [Authentication guide](/guides/authentication).

## Storage Layout

Each workspace maps to its own tags:

| Tag | Purpose |
|-----|---------|
| `state-<workspace>` | Current state |
| `stver-<workspace>-v<N>` | Versioned snapshots (when `max_versions > 0`) |
| `locked-<workspace>` / `unlocked-<workspace>` | Lock state (`unlocked-` is the GHCR fallback) |

Workspace names that aren't valid OCI tags are hashed to `ws-<hash>`, with the original name preserved in the `org.terraform.workspace` annotation.

| Content | Media type |
|---------|------------|
| State layer | `application/vnd.terraform.statefile.v1` |
| State layer, gzipped | `application/vnd.terraform.statefile.v1+gzip` |
| State manifest | `application/vnd.terraform.state.v1` |
| Lock manifest | `application/vnd.terraform.lock.v1` |

## Locking

Generation-based optimistic concurrency. Each `Lock` writes a lock manifest with an incremented generation counter, then re-reads it to confirm it won the race. Stale locks expire via `lock_ttl`, checked during acquisition — there are no background goroutines.

## Version Retention

When `max_versions > 0`, pruning runs asynchronously after each write (goroutine pool capped at 3). Version tags are grouped by manifest digest so identical states aren't stored twice, and the current state manifest is never deleted.

GHCR returns HTTP 405 on manifest deletion; the provider falls back to the GitHub Packages API, which needs `delete:packages` on your token. Without that scope, writes succeed but pruning fails.

Integration tests should call `client.WaitForRetention()` before asserting on tag state.

## Limitations

- Alpha Terraform only. Stable releases (including 1.16.x and 1.17.0) do not support pluggable state storage.
- GHCR pruning requires `delete:packages`.
- No migration tool. Use `terraform state pull` and `terraform state push` to move existing state in.
