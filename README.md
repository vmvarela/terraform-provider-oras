# terraform-provider-oras

[![CI](https://github.com/vmvarela/terraform-provider-oras/actions/workflows/ci.yml/badge.svg)](https://github.com/vmvarela/terraform-provider-oras/actions/workflows/ci.yml)
[![License: MPL-2.0](https://img.shields.io/badge/License-MPL--2.0-blue.svg)](https://opensource.org/licenses/MPL-2.0)
![Status: experimental](https://img.shields.io/badge/status-experimental-orange)
![Terraform: 1.17 alpha](https://img.shields.io/badge/terraform-1.17%20alpha-blue)

Store Terraform state in any OCI-compatible registry — GHCR, Docker Hub, Zot, Harbor, or your
own — using the ORAS protocol. If your team already runs an OCI registry, you can skip a
dedicated state backend and keep state next to your container images.

Experimental: implements Terraform's `statestore.StateStore` plugin interface, available only in
Terraform 1.17 alpha builds (pinned in `.terraform-version`). Set
`TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` at runtime.

```hcl
terraform {
  required_providers {
    oras = { source = "registry.terraform.io/vmvarela/oras", version = "~> 0.1" }
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
Credential resolution: [`docs/guides/authentication.md`](docs/guides/authentication.md).

## Development

```bash
make test                           # unit tests, no external deps
TF_ORAS_ZOT_TEST=1 make test-zot    # integration: spins Zot via Docker
make lint
make install                        # build + install to ~/.terraform.d/...
make dev-override                   # generate .terraformrc.dev pointing at this checkout
export TF_CLI_CONFIG_FILE=$PWD/.terraformrc.dev
```

[`examples/main.tf`](examples/main.tf) is a runnable local example against Zot over plain HTTP.

## When to use this

Worth it if you already run an OCI registry — especially GHCR from GitHub Actions, where state
lands beside your images and `GITHUB_TOKEN` already authenticates.

If you don't, the built-in backends (S3, GCS, Azure Blob, Consul) are simpler and more mature.
