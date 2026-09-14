# Architecture Principles

Terraform Core / StateStore protocol
→ `internal/statestore`
→ `internal/oras`
→ ORAS Go / OCI Distribution
→ registry

`internal/statestore` owns Terraform lifecycle, schema, requests/responses, diagnostics and Terraform lock/state semantics.

`internal/oras` owns OCI references, manifests/blobs, authentication integration, compression/storage representation, registry capabilities and OCI lock representation.

Introduce abstractions only for concrete boundaries, testability, external dependencies or genuinely required multiple implementations.
