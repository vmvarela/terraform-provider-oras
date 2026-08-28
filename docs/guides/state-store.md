---
page_title: "State Store Configuration"
description: |-
  Detailed configuration reference for the oras_oci state store.
---

# State Store Configuration

The `oras_oci` state store is configured within the `terraform` block using a `state_store` block. This guide covers all available arguments and their behavior.

## Basic Configuration

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
    max_state_size = 536870912  # 512 MiB
  }
}

provider "oras" {}
```

## Argument Reference

| Argument | Required | Type | Default | Description |
|----------|:--------:|:----:|---------|-------------|
| `url` | ✓ | string | — | OCI registry URL in format `oci://registry/repository` |
| `compression` | | bool | `false` | Enable gzip compression for state data |
| `lock_ttl` | | string | — | State lock TTL duration (e.g., `15m`, `1h`) |
| `max_versions` | | number | `0` | Max state versions to retain (0 = unlimited) |
| `max_state_size` | | number | `268435456` | Max state size in bytes (256 MiB) |

### `url` (Required)

The OCI registry URL in the format `oci://<registry>/<repository>`.

**Examples:**
```hcl
# GHCR
url = "oci://ghcr.io/myorg/infra-tfstate"

# Docker Hub
url = "oci://docker.io/myuser/tfstate"

# Harbor / Self-hosted
url = "oci://harbor.company.com/library/tfstate"

# Local Zot (with port)
url = "oci://localhost:5001/estado"
```

The URL is parsed into:
- **Registry:** Hostname (and optional port) — e.g., `ghcr.io`, `localhost:5000`
- **Repository:** Path after the first `/` — e.g., `myorg/infra-tfstate`, `estado`

### `compression`

Enable gzip compression for state data before pushing to the registry.

```hcl
compression = true
```

When enabled, state layers use media type `application/vnd.terraform.statefile.v1+gzip` instead of `application/vnd.terraform.statefile.v1`. This reduces storage size and transfer time for large state files.

### `lock_ttl`

Duration after which a stale lock is automatically cleared. Uses Go duration format (e.g., `15m`, `1h`, `30s`).

```hcl
lock_ttl = "15m"
```

**Behavior:**
- Lock TTL is stored in the lock manifest as a lease expiry timestamp
- On each `Lock` call, the provider checks if the existing lock's lease has expired
- If expired, the stale lock is automatically cleared before acquiring a new one
- No background goroutines — cleanup happens synchronously during lock acquisition
- When unset, locks never expire automatically (manual intervention required if a process crashes while holding a lock)

**Recommendation:** Set to `15m`–`30m` for most workflows. Shorter TTLs reduce risk of stuck locks but increase chance of false expiry during long operations.

### `max_versions`

Maximum number of historical state versions to retain per workspace. When exceeded, the oldest versions are pruned asynchronously after each `Put`.

```hcl
max_versions = 10
```

| Value | Behavior |
|-------|----------|
| `0` (default) | Unlimited — no pruning, all versions retained |
| `1` | Keep only current state (no history) |
| `N > 1` | Keep current + N-1 historical versions |

**Version Tags:** Historical versions are stored as `state-<workspace>-v<N>` tags (e.g., `state-default-v1`, `state-default-v2`).

**Retention Mechanism:**
- Runs asynchronously in a goroutine pool (max 3 concurrent)
- Triggered after each successful `Put` when `max_versions > 0`
- Groups versions by manifest digest to avoid redundant storage
- Current state manifest is never deleted
- Call `client.WaitForRetention()` in integration tests before asserting on tag state

**GHCR Note:** GHCR returns HTTP 405 on manifest deletion. The provider falls back to the GitHub Packages API, which requires `delete:packages` scope on your token. Without this scope, pruning will fail silently.

### `max_state_size`

Maximum allowed state payload size in bytes. Protects against OOM from maliciously large or corrupted state.

```hcl
max_state_size = 536870912  # 512 MiB
```

**Default:** `268435456` (256 MiB)

When reading state, the provider enforces this limit with a hard cutoff. If state exceeds the limit, a clear error is returned suggesting to increase the limit.

## Workspace Support

Each Terraform workspace maps to its own set of tags in the repository:

```bash
# Default workspace
state-default
state-default-v1, state-default-v2, ...
locked-default / unlocked-default

# Custom workspace
terraform workspace new staging
# Creates:
state-staging
state-staging-v1, state-staging-v2, ...
locked-staging / unlocked-staging
```

Workspace names that aren't valid OCI tags (contain uppercase, special chars, etc.) are automatically hashed to `ws-<hash>` format, with the original name stored in the manifest annotation `org.terraform.workspace`.

## Locking Behavior

The provider implements distributed locking using generation-based optimistic concurrency control:

1. **Lock acquisition:** Creates a lock manifest with incremented generation counter
2. **Concurrency detection:** If lock tag exists, compares generation — mismatch means concurrent attempt
3. **Stale lock clearance:** If `lock_ttl` is set and lease expired, stale lock is auto-cleared
4. **Verification:** Post-write verification ensures the lock was actually acquired (handles race conditions)

**Lock Manifest Annotations:**
- `org.terraform.lock.id` — Lock UUID
- `org.terraform.lock.info` — JSON-encoded LockInfo (who, operation, version, etc.)
- `org.terraform.lock.generation` — JSON-encoded LockManifestData (generation, lease_expiry, holder_id)

## Version Retention Details

When `max_versions > 0`, the retention logic:

1. Lists all version tags for the workspace (`state-<ws>-v<N>`)
2. Groups tags by underlying manifest digest (deduplicates identical states)
3. Sorts versions numerically
4. Calculates how many to delete: `total_versions - max_versions`
5. For each digest group to delete:
   - Retag "keep" tags to a new manifest (preserving history)
   - Delete the old manifest digest (with GHCR fallback)

**Concurrency:** Retention runs in background with semaphore (cap 3) to avoid overwhelming the registry.

## Media Types

| Content | Media Type |
|---------|------------|
| State layer (plain) | `application/vnd.terraform.statefile.v1` |
| State layer (gzip) | `application/vnd.terraform.statefile.v1+gzip` |
| State manifest | `application/vnd.terraform.state.v1` |
| Lock manifest | `application/vnd.terraform.lock.v1` |

## Example: Production Configuration

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

    # GHCR repository for production state
    url = "oci://ghcr.io/myorg/infra-tfstate-prod"

    # Compress state to reduce storage/transfer
    compression = true

    # Auto-clear locks after 30 minutes (handles crashed applies)
    lock_ttl = "30m"

    # Keep last 20 versions for rollback capability
    max_versions = 20

    # Allow larger state files (512 MiB)
    max_state_size = 536870912
  }
}

provider "oras" {
  # Use custom CA if GHCR is behind corporate proxy with MITM
  # ca_file = "/etc/ssl/certs/corp-ca.pem"
}
```

## Example: Development Configuration

```hcl
terraform {
  state_store "oras_oci" {
    provider = oras
    url      = "oci://localhost:5001/dev-tfstate"
    # No compression, no versioning, no lock TTL for fast local iteration
  }
}

provider "oras" {
  insecure = true  # Local Zot on plain HTTP
}
```