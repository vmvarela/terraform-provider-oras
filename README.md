# terraform-provider-oras

[![CI](https://github.com/vmvarela/terraform-provider-oras/actions/workflows/ci.yml/badge.svg)](https://github.com/vmvarela/terraform-provider-oras/actions/workflows/ci.yml)
[![License: MPL-2.0](https://img.shields.io/badge/License-MPL--2.0-blue.svg)](https://opensource.org/licenses/MPL-2.0)
![Status: experimental](https://img.shields.io/badge/status-experimental-orange)
![Terraform: 1.17 alpha](https://img.shields.io/badge/terraform-1.17%20alpha-blue)

Store Terraform state in an OCI-compatible registry using the ORAS protocol — tested against
ghcr.io and Zot; other registries are untested and behavior is registry-specific. If your team
already runs one, you can skip a dedicated state backend and keep state next to your container images.

Experimental: implements Terraform's `statestore.StateStore` plugin interface, available only in
Terraform 1.17 alpha builds (pinned in `.terraform-version`). Set
`TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` at runtime.

```hcl
terraform {
  required_providers {
    # Pin the exact stable version. Pre-release versions (e.g. 0.1.6-alpha)
    # additionally require an exact pin: Terraform's range operators (~>, >=, …)
    # never select pre-releases.
    oras = { source = "registry.terraform.io/vmvarela/oras", version = "0.1.5" }
  }

  state_store "oras_oci" {
    provider     = oras
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
terraform init && terraform apply
```

Full configuration reference, storage layout, and locking semantics: [`docs/index.md`](docs/index.md).
Architecture, consistency guarantees, and failure behavior: [`docs/architecture.md`](docs/architecture.md).
Credential resolution: [`docs/guides/authentication.md`](docs/guides/authentication.md).

## Workspace layout upgrade

This branch changes **all workspace identifiers**, including `default`, to
full SHA-256. Existing repositories are rejected until explicitly migrated to
a separate empty repository. Stop all writers and follow the
[workspace migration guide](docs/guides/workspace-migration.md) before upgrading.
Do not mix old and new provider versions against the same repository.

## Development

```bash
make test                           # unit tests, no external deps
TF_ORAS_ZOT_TEST=1 make test-zot    # integration: spins Zot via Docker
# Without Docker: also set TF_ORAS_ZOT_BINARY=/absolute/path/to/zot (v2.1.0)
make coverage                       # tests + coverage gate (default 80%, COVERAGE_THRESHOLD=N to change)
make lint
make install                        # build + install to ~/.terraform.d/... (VERSION= overrides the mirror dir)
make dev-override                   # generate .terraformrc.dev pointing at this checkout
export TF_CLI_CONFIG_FILE=$PWD/.terraformrc.dev
```

[`examples/main.tf`](examples/main.tf) is a runnable local example against Zot over plain HTTP.

## When to use this

Worth it if you already run an OCI registry — especially GHCR from GitHub Actions, where state
lands beside your images and `GITHUB_TOKEN` already authenticates.

If you don't, the built-in backends (S3, GCS, Azure Blob, Consul) are simpler and more mature.
