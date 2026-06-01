# terraform-provider-orastate

A Terraform provider that stores Terraform state in any OCI-compatible registry using the ORAS (OCI Registry As Storage) protocol. If your team already runs GitHub Container Registry, Docker Hub, Zot, or another OCI registry, you can skip the dedicated S3/GCS/Azure backend and consolidate state storage onto infrastructure you already operate. The provider implements the experimental `statestore.StateStore` plugin interface introduced in Terraform 1.16.

```hcl
terraform {
  required_providers {
    orastate = {
      source  = "registry.terraform.io/vmvarela/orastate"
      version = "~> 0.1"
    }
  }
}

provider "orastate" {
  # insecure = true   # skip TLS (dev only)
  # ca_file  = "/path/to/ca.pem"
}
```

## Quick start with GHCR

```hcl
terraform {
  state_store "orastate_oci" {
    url          = "oci://ghcr.io/myorg/infra-tfstate"
    compression  = true
    lock_ttl     = "15m"
    max_versions = 10
  }
}
```

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
export GHCR_TOKEN=ghp_...

terraform init
terraform apply
```

Only one environment variable is required to enable the feature: `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1`. Authentication is resolved in this order: `ORAS_TOKEN` → `GHCR_TOKEN` / `GITHUB_TOKEN` (for ghcr.io) → anonymous.

## How it works

State is stored as an OCI image manifest. Each workspace maps to a set of tags on the repository:

- `state-<workspace>` — current state
- `state-<workspace>-v<N>` — versioned snapshots (when `max_versions > 0`)
- `locked-<workspace>` — lock manifest
- `unlocked-<workspace>` — release marker on registries that don't support deletion

Layering uses one of two media types: `application/vnd.terraform.statefile.v1` (raw) or `application/vnd.terraform.statefile.v1+gzip` (compressed).

Locking uses generation-based optimistic concurrency. Each lock attempt increments a generation counter on the lock manifest, so simultaneous attempts are detected reliably. Stale locks are automatically cleared after the configured `lock_ttl` (checked on each Lock call, not via background goroutines).

Version retention runs asynchronously after each `Put` — a goroutine pool (capped at 3 concurrent jobs) prunes the oldest versions when `max_versions` is exceeded. Call `client.WaitForRetention()` in integration tests before asserting on tag state.

GHCR has a notable quirk: it returns HTTP 405 on manifest deletion. When the provider detects this, it falls back to the GitHub Packages API (`delete:packages` scope required on the token) to delete the version.

## Provider configuration

| Attribute  | Description                            |
|------------|----------------------------------------|
| `insecure` | Skip TLS verification (dev only)       |
| `ca_file`  | Path to a PEM-encoded CA bundle        |

## State store configuration

| Attribute      | Default   | Description                                     |
|----------------|-----------|-------------------------------------------------|
| `url`          | —         | OCI URL in the form `oci://registry/repository` |
| `compression`  | `false`   | Enable gzip compression for state data          |
| `lock_ttl`     | —         | Lock TTL (e.g., `15m`, `1h`); clears stale locks|
| `max_versions` | `0`       | Max state versions to retain; `0` = unlimited   |
| `max_state_size` | 256 MiB | Max state size in bytes                          |

## Development

```bash
go build -o terraform-provider-orastate .
make install   # builds + copies to ~/.terraform.d/plugins/.../darwin_arm64/
cp .terraformrc.dev ~/.terraformrc
```

The `.terraformrc.dev` file sets up a dev override to the local checkout so Terraform picks up the provider directly.

## Tests

```bash
make test              # unit tests with race detector (no external deps)
TF_ORAS_ZOT_TEST=1 make test-zot  # integration tests with Zot (Docker required)
```

Unit tests use `fakeORASRepo` — an in-memory repository implementation (`internal/oras/helper_test.go`) with no external dependencies. Integration tests spin a Zot container per test case via Docker.

## Limitations

- Requires Terraform 1.16.0-alpha20260513+ with the `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` environment variable. The statestore plugin API is experimental and may change.
- GHCR requires a token with `delete:packages` scope for version retention to work (fallback to the GitHub API).
- No built-in migration tool from existing state backends — migrate with `terraform state pull` / `terraform state push`.
- If you don't already use OCI registries, standard Terraform backends (S3, GCS, Azure Blob) are simpler choices.

## When to use this

This provider makes sense when your team is already committed to an OCI registry workflow — especially if you're using GHCR in GitHub Actions and want state to live next to your container images. For self-hosted registries, Zot (`zot-linux-amd64:v2.1.0`) is the recommended test target. If you're starting from scratch without an OCI registry, the built-in Terraform backends will serve you better.
