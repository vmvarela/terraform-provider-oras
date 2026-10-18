---
page_title: "oras Provider — Compatibility"
description: |-
  Tested Terraform and registry versions for the oras state store provider,
  and which combinations are unavailable or untested.
---

# Compatibility

This provider is experimental and targets Terraform's pluggable state storage
experiment. The tested Terraform baseline is listed below; other versions are
either unavailable in Terraform itself or untested by this provider.

## Terraform

| Terraform version | Status | Notes |
|-------------------|--------|-------|
| 1.17.0-alpha20260827 | **Tested** | The pinned alpha in [`.terraform-version`](https://github.com/vmvarela/terraform-provider-oras/blob/v0.1.8/.terraform-version) and the only version exercised by CI end-to-end (against Zot 2.1.0) |
| 1.17.0-beta1 | Not supported | The official beta binary refuses the StateStore experiment because experiments are disabled in beta builds. This is a Terraform-side gate, not a provider incompatibility; no provider-side beta regression was found |
| Stable releases (including 1.16.x) | Not supported | Pluggable state storage is not available in any stable Terraform release, so the provider cannot run |
| Other alpha builds | Untested | The `statestore.StateStore` plugin API may change between alphas; other builds may work but are not verified |

Enable the experiment with either supported form:

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
# or
terraform init -enable-pluggable-state-storage-experiment
```

## Provider and framework versions

| Component | Version |
|-----------|---------|
| Published provider release | v0.1.8 |
| `terraform-plugin-framework` | v1.19.0 |
| `terraform-plugin-go` | v0.31.0 |
| Registry provider protocol metadata | 6.0 (from [`terraform-registry-manifest.json`](https://github.com/vmvarela/terraform-provider-oras/blob/v0.1.8/terraform-registry-manifest.json)) |
| StateStore RPC schema | Experimental Terraform Plugin Protocol v6.11, as implemented by the pinned Terraform alpha and `terraform-plugin-go` v0.31.0 |

## Registries

| Registry | Status | Notes |
|----------|--------|-------|
| Zot 2.1.0 | **Tested** | Used by the CI end-to-end job; the local example targets a registry at `localhost:5001` but does not pin its version |
| ghcr.io | Tested by `oras-state` | The engine's GHCR integration is live-tested upstream; pruning needs `delete:packages` |
| Other OCI registries | Untested | Behavior is registry-specific; see the [provider overview](../index.md) |

CI runs a single Terraform alpha end-to-end job — it does not exercise a matrix
of Terraform or registry versions.
