# terraform-provider-oras

Store Terraform state in any OCI-compatible registry — GHCR, Docker Hub, Zot, or your own
registry — using the ORAS protocol. If your team already runs an OCI registry, you can skip
dedicated state backends and keep state next to your container images.

This is an *experimental* provider implementing Terraform's `statestore.StateStore` plugin
interface, available in Terraform 1.17 alpha builds (pinned in `.terraform-version`). You must set
`TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` at runtime.

```hcl
terraform {
  required_providers {
    oras = {
      source  = "registry.terraform.io/vmvarela/oras"
      version = "~> 0.1"
    }
  }
}
```

## Quick start

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
export GHCR_TOKEN=ghp_xxxxxxxxxxxx
```

```hcl
terraform {
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
terraform init
terraform apply
```

That's it. Auth resolution: `ORAS_TOKEN` (any registry) → `GHCR_TOKEN`/`GITHUB_TOKEN`
(ghcr.io only) → anonymous.

## Configuration

| Attribute        | Default | Description                                    |
|------------------|---------|------------------------------------------------|
| `url`            | —       | OCI URL (`oci://registry/repo`)                |
| `compression`    | `false` | Gzip state data                                |
| `lock_ttl`       | —       | Auto-clear stale locks (e.g. `"15m"`, `"1h"`) |
| `max_versions`   | `0`     | Number of old versions to keep; `0` = unlimited|
| `max_state_size` | 256 MiB | Max state payload size (bytes)                 |

Provider-level: `insecure` (skip TLS — dev only) and `ca_file` (custom CA bundle).

## Under the hood

Each workspace maps to a set of OCI tags on the repository:

- `state-<workspace>` — current state
- `state-<workspace>-v<N>` — versioned snapshots (when `max_versions > 0`)
- `locked-<workspace>` / `unlocked-<workspace>` — lock state

Locking uses generation-based optimistic concurrency. Each `Lock` call increments
a counter on the lock manifest, so simultaneous attempts are detected reliably.
Stale locks auto-expire after `lock_ttl` (checked on each Lock call, no background
goroutines).

Version retention runs asynchronously after each `Put` — a goroutine pool (cap 3)
prunes the oldest versions when `max_versions` is exceeded. Call
`client.WaitForRetention()` in integration tests before asserting on tag state.

GHCR returns HTTP 405 on manifest deletion. The provider detects this and falls
back to the GitHub Packages API (`delete:packages` scope required).

## Examples

**Private registry (Zot):**

```hcl
terraform {
  state_store "oras_oci" {
    provider = oras

    url      = "oci://zot.example.com/infra/production"
    lock_ttl = "5m"
  }
}

provider "oras" {
  insecure = true
}
```

**Multi-workspace:**

```bash
# Each workspace gets its own tag set
terraform workspace new staging
```

```hcl
terraform {
  state_store "oras_oci" {
    provider = oras

    url          = "oci://ghcr.io/myorg/infra-tfstate"
    max_versions = 5
  }
}

provider "oras" {}
```

## Limitations

- Requires Terraform **1.17.x alpha** (or any Terraform alpha build with pluggable state storage experiments enabled). `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` is set for convenience but is no longer required in recent alphas.
  The statestore plugin API is experimental and may break across releases. **Terraform stable releases (including 1.16.0 and 1.17.0 stable) do NOT support pluggable state storage** — it is gated to alpha/dev builds only.
- GHCR needs a token with `delete:packages` scope for retention to work.
- No built-in migration tool. Use `terraform state pull` / `terraform state push`
  to move existing state into this provider.
- Max state size defaults to 256 MiB (configurable via `max_state_size`).

## Development

```bash
make test                           # unit tests (no deps)
TF_ORAS_ZOT_TEST=1 make test-zot   # integration: spins Zot via Docker
make install                        # build + install to ~/.terraform.d/...
cp .terraformrc.dev ~/.terraformrc  # dev override
```

See [`examples/main.tf`](examples/main.tf) for a runnable local example (Zot over plain HTTP).

## When to use this

Use this provider if your team already runs an OCI registry — especially if you're
on GHCR in GitHub Actions and want state to live alongside your container images.
Zot (`zot-linux-amd64:v2.1.0`) is our recommended self-hosted test target.

If you don't already use OCI registries, the built-in Terraform backends (S3, GCS,
Azure Blob, Consul) are simpler and more mature choices.
