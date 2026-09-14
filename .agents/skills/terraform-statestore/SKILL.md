---
name: terraform-statestore
description: Covers the experimental Terraform StateStore provider API, including StateStoreData/configure lifecycle, read/write/delete/list and lock/unlock semantics, with upstream protocol verification before compatibility claims. Use for Terraform-facing work in internal/statestore or changes touching state lifecycle, locking or diagnostics.
---
# Terraform StateStore Skill

Use for Terraform-facing StateStore work. The API is experimental: verify current upstream behavior before compatibility claims.

Before changes:
1. inspect local interfaces/tests;
2. identify Plugin Framework interface;
3. identify Plugin Go protocol behavior;
4. check Terraform Core when relevant;
5. record version constraints;
6. separate protocol requirements from provider choices.

Inspect as relevant: StateStore registration, metadata/schema, validation, initialization, StateStoreData/configure lifecycle, read/write/delete/list, lock/unlock, diagnostics, chunk/state-size behavior and workspace mapping.

Do not bridge lifecycle phases with global mutable state. Stale unlock must never remove a newer lock. Terraform-facing guarantees must not exceed OCI-layer guarantees.

For upstream-sensitive changes record Terraform Core, terraform-plugin-framework, terraform-plugin-go, protocol version and experimental flags. Do not upgrade dependencies merely because newer versions exist.

Prefer current upstream source/tests and official documentation over model prior knowledge.
