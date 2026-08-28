# oras Provider

!> This provider requires Terraform 1.17+ alpha builds with the pluggable state storage experiment. It will not work with any stable Terraform release. The statestore plugin API is experimental and may break across alpha releases.

Stores Terraform state in any OCI-compatible registry — including GHCR, Docker Hub, Zot, or a self-hosted registry — using the ORAS protocol via OCI artifact manifests. This is an experimental, state-store-only provider that implements `statestore.StateStore` for Terraform's pluggable state storage experiment; it exposes no resources and no data sources. It requires a Terraform alpha build with the experiment enabled — set `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` in the environment or pass `-enable-pluggable-state-storage-experiment` on `terraform init` — and does not work with any Terraform stable release (including 1.16.x or 1.17.0 stable).

This provider has **no resources and no data sources** — it only exposes the `oras_oci` state store via `ProviderWithStateStores`.

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

Auth is resolved as `ORAS_TOKEN` (any registry) → `GHCR_TOKEN` / `GITHUB_TOKEN` (ghcr.io only) → anonymous.

## Argument Reference

The provider and its single state store expose the following arguments.

### Provider `oras`

Provider-level TLS settings forwarded to the OCI client.

- `insecure` - (Optional, Boolean) Skip TLS certificate verification when communicating with the registry. Defaults to `false`. Needed for registries served over plain HTTP (e.g. local Zot).
- `ca_file` - (Optional, String) Path to a PEM-encoded CA certificate bundle to trust when communicating with the registry.

### State Store `oras_oci` (`state_store "oras_oci"`)

- `url` - (Required, String) OCI registry URL in the form `oci://registry/repository` (e.g. `oci://ghcr.io/myorg/infra-tfstate`).
- `compression` - (Optional, Boolean) Enable gzip compression for state data. Defaults to `false`.
- `lock_ttl` - (Optional, String) Duration after which a stale lock is auto-cleared (e.g. `"15m"`, `"1h"`). When unset, locks do not expire automatically.
- `max_versions` - (Optional, Number) Number of old state versions to retain as `state-<workspace>-v<N>` tags. Defaults to `0` (unlimited — no pruning). When exceeded, the oldest versions are pruned asynchronously after each `Put`.
- `max_state_size` - (Optional, Number) Maximum allowed state payload size in bytes. Defaults to `268435456` (256 MiB).
