---
page_title: "oras Provider — Architecture & Consistency"
description: |-
  Terraform StateStore adapter architecture, ownership boundaries, failure mapping,
  and the consistency guarantees delegated to oras-state.
---

# Architecture & Consistency

This provider is a **Terraform adapter** around the shared
[`oras-state`](https://github.com/vmvarela/oras-state) OCI state engine.

The separation is intentional: Terraform protocol behavior belongs here; portable OCI state
semantics belong in `oras-state`.

## Component chain

```text
Terraform Core 1.17 alpha
        │
        │ statestore.StateStore
        ▼
terraform-provider-oras
  internal/provider       provider configuration
  internal/credentials    Terraform/provider credential discovery
  internal/statestore     StateStore RPC adapter and diagnostics
        │
        │ github.com/vmvarela/oras-state v0.1.0
        ▼
oras-state
  state/workspace identity
  OCI storage layout
  lock lifecycle
  publication/recovery semantics
  version retention
  compression and size limits
  registry transport behavior
        │
        │ oras-go
        ▼
OCI registry
```

The provider must not grow a second registry engine. If behavior can be expressed and tested
without Terraform types or Terraform lifecycle semantics, it normally belongs in `oras-state`.

## Ownership boundaries

| Concern | Owner |
|---|---|
| Terraform StateStore schema and RPC lifecycle | `terraform-provider-oras` |
| Terraform diagnostics | `terraform-provider-oras` |
| Provider configuration and credential discovery | `terraform-provider-oras` |
| Mapping Terraform lock information to engine lock information | `terraform-provider-oras` |
| OCI state/workspace identity | `oras-state` |
| Manifest/tag layout and media types | `oras-state` |
| Registry read/write behavior | `oras-state` |
| Lock verification and takeover semantics | `oras-state` |
| Mutable-tag publication and recovery semantics | `oras-state` |
| Version retention and compression | `oras-state` |
| Registry compatibility | `oras-state` |
| Terraform/OpenTofu product integration | consuming adapter |

## StateStore lifecycle

Terraform can create fresh StateStore instances for individual RPCs. The provider therefore keeps
the shared engine client and adapter-local lock registrations in `stateStoreData`, which is passed
through the framework's StateStore data mechanism.

The local registration maps a Terraform `StateID` to the lock ID returned by the engine. A write
with a locally registered lock asks `oras-state` to verify that lock before publishing. If
Terraform did not acquire a lock, for example when using `-lock=false`, there is no local lock to
verify.

This local map is **not** a distributed lock. Distributed behavior is implemented by
`oras-state` using OCI objects.

## Configuration and credentials

The provider owns Terraform-facing configuration such as:

- `url`
- `compression`
- `lock_ttl`
- `max_versions`
- `max_state_size`
- `plain_http`
- `tls_skip_verify`
- `ca_file`

It also owns credential discovery from provider configuration and the caller environment. Resolved
credentials and transport settings are passed to `oras-state`; the shared engine does not depend
on Terraform configuration files or Terraform framework types.

See [Authentication](guides/authentication.md) for the provider-specific resolution order.

## Failure mapping

The adapter preserves engine failure semantics instead of flattening every failure into a generic
registry error.

In particular:

- `orasstate.LockError` is mapped to Terraform lock-contention diagnostics.
- `orasstate.ErrMutableTagMoved` is surfaced as a publication/concurrency conflict.
- `orasstate.PartialPublicationError` reports that current state is already visible even though a
  secondary publication step failed.
- cancellation and ordinary registry failures are propagated as StateStore operation diagnostics.

Engine-level failure injection belongs in `oras-state`. This repository tests that those engine
errors are translated correctly at the Terraform boundary.

## Consistency model

The provider does not add stronger consistency guarantees than `oras-state`.

The current engine contract is deliberately conservative:

- OCI tags are mutable.
- There is no claimed portable distributed compare-and-swap operation.
- Locking is generation-aware and verified, but it is not a proof of atomic mutual exclusion.
- verify-then-write remains a time-of-check/time-of-use boundary.
- ambiguous mutable-tag publication fails closed rather than blindly overwriting a possibly newer
  value.
- a partial publication is reported explicitly so callers do not blindly repeat an apply.

For the engine's exact concurrency and recovery contract, use the documentation and tests in
[`oras-state`](https://github.com/vmvarela/oras-state). The provider documentation should describe
only the additional Terraform-specific consequences.

## Recovery implications

A Terraform apply can modify infrastructure before state persistence fails. Positive lock leases
are not automatically renewed by this provider. When persistence is rejected or uncertain:

1. preserve any Terraform recovery state;
2. inspect the remote state before retrying;
3. do not blindly repeat an apply after a partial or ambiguous publication.

See [State write recovery](guides/state-recovery.md).

## Workspace and storage format

The storage format is implemented by `oras-state` and consumed by this provider. Workspace names
map to bounded OCI tag identifiers, and the original logical name is retained in annotations.

The provider's compatibility documentation remains responsible for telling Terraform users when a
storage-format change requires migration. See [OCI storage format](storage-format.md) and
[Workspace migration](guides/workspace-migration.md).

## Testing strategy

The repositories intentionally test different layers.

### oras-state

Owns registry-engine tests:

- unit and race tests for the OCI engine;
- Zot integration;
- GHCR integration;
- registry compatibility;
- publication, retention, locking, and recovery semantics.

### terraform-provider-oras

Owns adapter tests:

- provider and StateStore schema behavior;
- Terraform diagnostic mapping;
- adapter-local lock bookkeeping;
- Terraform alpha compatibility;
- end-to-end persistence/recovery through a disposable Zot registry.

The CI workflow is the canonical end-to-end environment for the Terraform alpha experiment.

## Compatibility

The provider requires a compatible Terraform alpha build with the pluggable state
storage experiment enabled; stable Terraform releases do not expose this
experiment. The tested baseline and unsupported versions are documented in
[Compatibility](guides/compatibility.md).

Terraform protocol/framework compatibility is tracked in this repository; OCI
registry behavior remains owned by `oras-state`.

## Design rule

When adding a change, ask one question first:

> Would this behavior still be required if the caller were OpenTofu, another Terraform adapter, or
> a standalone Go program?

If yes, the behavior probably belongs in `oras-state`. If it depends on Terraform framework
types, Terraform configuration, Terraform diagnostics, or StateStore RPC lifecycle, it belongs in
this provider.
