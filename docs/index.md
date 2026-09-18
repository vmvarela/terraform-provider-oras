---
page_title: "oras Provider"
description: |-
  Stores Terraform state in an OCI-compatible registry using the ORAS protocol
  (tested against ghcr.io and Zot; other registries untested).
---

# oras Provider

~> **Experimental:** Requires a Terraform 1.17+ alpha build with the pluggable state storage experiment enabled. It will not work with any stable Terraform release, and the statestore plugin API may break across alpha releases.

Implements Terraform's `statestore.StateStore` interface to keep state in an OCI-compatible registry as OCI artifact manifests. Tested against ghcr.io and Zot; other registries are untested, and behavior is registry-specific.

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
      source = "registry.terraform.io/vmvarela/oras"
      # Pin the exact stable version. Pre-release versions (e.g. 0.1.6-alpha)
      # additionally require an exact pin: Terraform's range operators (~>, >=, …)
      # never select pre-releases.
      version = "0.1.5"
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

For a local registry over plain HTTP (e.g. Zot), set `plain_http`:

```hcl
terraform {
  state_store "oras_oci" {
    provider = oras
    url      = "oci://localhost:5001/estado"
  }
}

provider "oras" {
  plain_http = true
}
```

A runnable version lives in [`examples/main.tf`](../examples/main.tf).

## State Store Arguments

| Argument         | Required | Default               | Description |
|------------------|:--------:|-----------------------|-------------|
| `url`            | ✓        | —                     | `oci://<registry>/<repository>`; registry may include a port |
| `compression`    |          | `false`               | Gzip the state layer |
| `lock_ttl`       |          | —                     | Non-renewing lease (`15m`, `1h`), based on local clocks. Expiry can refuse state persistence after infrastructure changes. Unset/`0` means locks never expire |
| `max_versions`   |          | `0` (versioning disabled) | Versions retained per workspace. `1` keeps only the current state; `0` keeps no version tags, allocates no versions, and prunes nothing |
| `max_state_size` |          | `268435456` (256 MiB) | Hard read/write limit; guards against a corrupted or malicious layer |

## Provider Arguments

| Argument   | Required | Default | Description |
|------------|:--------:|---------|-------------|
| `plain_http` |          | `false` | Explicitly use unencrypted HTTP instead of HTTPS |
| `tls_skip_verify` |          | `false` | Disable certificate verification while retaining HTTPS |
| `insecure` |          | `false` | Deprecated: preserve legacy HTTP and disabled TLS verification; do not combine with either new option |
| `ca_file`  |          | —       | PEM-encoded CA bundle for self-signed registries |

See [transport configuration and upgrade instructions](guides/authentication.md#transport-and-tls) for conflicts and migration from `insecure`. HTTPS with certificate verification remains the default.

## Authentication

Resolved in priority order: `ORAS_TOKEN` (any registry) → `GHCR_TOKEN` / `GITHUB_TOKEN` (ghcr.io only) → configured credentials (`.terraformrc` `oci_credentials` blocks, Docker config files, credential helpers) → anonymous.

See the [Authentication guide](/guides/authentication).

## Storage Layout

Each workspace maps to its own tags:

| Tag | Purpose |
|-----|---------|
| `state-<sha256>` | Current state |
| `stver-<sha256>-v<N>` | Versioned snapshots (when `max_versions > 0`) |
| `locked-<sha256>` / `unlocked-<sha256>` | Lock state (`unlocked-` is the GHCR fallback) |

Every workspace name is encoded as its full lowercase SHA-256 digest. The original name is required in the `org.terraform.workspace` annotation on every state and lock manifest. Tags and annotations are verified together. **Existing repositories require explicit migration**; see [workspace mapping and migration](guides/workspace-migration.md).

| Content | Media type |
|---------|------------|
| State layer | `application/vnd.terraform.statefile.v1` |
| State layer, gzipped | `application/vnd.terraform.statefile.v1+gzip` |
| State manifest | `application/vnd.terraform.state.v1` |
| Lock manifest | `application/vnd.terraform.lock.v1` |

## Locking

Best-effort, generation-based optimistic concurrency. Each `Lock` writes a lock manifest with an incremented generation counter and a holder ID, immediately re-reads the lock tag to confirm it still won the race, then — after a short, context-cancellable wait — re-reads it a second time. This second read catches a rival that tagged the lock after the first verification; when ownership has moved, `Lock` reports contention instead of clearing a tag that points at someone else. Releasing a lock (or retagging it to the `unlocked-` marker on registries without manifest deletion) re-checks the tag right before overwriting it.

OCI tags are last-writer-wins: the OCI Distribution spec has no portable compare-and-swap (no `If-Match` across registries). These checks narrow the race window but cannot close it entirely — **do not treat this as strong mutual exclusion**. For most teams the practical protection is that Terraform operations are human-paced and lock acquisition is verified before every write while a lock is registered in the current process — writes proceed unverified after a restart or with `-lock=false`.

`lock_ttl` recovers orphaned locks: a lock whose stored lease has expired (`lease_expiry > 0`) is cleared on the next `Lock` attempt (no background goroutines). Unset means locks never expire, and a crashed client blocks the workspace until someone releases it. Note: a lock written while `lock_ttl` was unset has no lease timestamp, so enabling `lock_ttl` later does not clear it — it stays until manually released.

Running Terraform with `-lock=false` never calls `Lock`, so no lock is registered and writes proceed without the ownership verification described above.

Positive leases are not automatically renewed. Before retrying a failed write,
preserve any recovery snapshot and inspect remote state; do not blindly repeat
an apply. See [state write recovery](guides/state-recovery.md) for rejected,
uncertain and partial publication outcomes and the pinned Terraform experiment.

## Version Retention

When `max_versions > 0`, pruning runs asynchronously after each write (goroutine pool capped at 3). During pruning, version tags are grouped by manifest digest so a digest shared by several version tags is deleted once, not once per tag; each write stamps a fresh `updated_at` timestamp, so identical state bytes still produce a new manifest digest — digest grouping is a deletion strategy, not write deduplication. The current state manifest is never deleted.

When GHCR returns HTTP 405 on manifest deletion, the provider falls back to the GitHub Packages API, which needs `delete:packages` on your token. Without that scope, writes succeed but pruning fails. Evidence precision: the 405 fallback branch is unit-tested against a simulated 405 registry; live GHCR integration is env-gated and does not necessarily prove the 405 branch executed. Registry-specific, not portable.

Integration tests should call `client.WaitForRetention()` before asserting on tag state.

## Limitations

- Alpha Terraform only. Stable releases (including 1.16.x and 1.17.0) do not support pluggable state storage.
- GHCR pruning requires `delete:packages`.
- No migration tool. Use `terraform state pull` and `terraform state push` to move existing state in.

For storage internals, the locking model, race windows, failure behavior, and
what is (and is not) guaranteed, see the
[Architecture & Consistency page](/architecture).
