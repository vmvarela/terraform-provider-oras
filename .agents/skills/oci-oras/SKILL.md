---
name: oci-oras
description: Distinguishes the OCI/Distribution specification, the ORAS Go implementation, registry implementations and observed registry behavior as separate truth layers, covering conditional operations and storage-format changes. Use when working in internal/oras or reasoning about OCI/ORAS semantics, tag/digest behavior or registry compatibility claims.
---
# OCI / ORAS Skill

Keep registry, repository, reference, tag, digest, descriptor, manifest and blob distinct. Tags are mutable references; digests identify content.

Keep four truth layers separate:
1. OCI/Distribution specification;
2. ORAS Go API/implementation;
3. registry implementation;
4. observed behavior of a registry/version.

Before adding behavior identify the exact operation, inspect specification and ORAS semantics, classify it as required/optional/unspecified, test target registries when correctness depends on implementation behavior, and document fallback guarantees.

Do not assume ETag/If-Match/If-None-Match provides portable CAS for OCI tags/manifests. Prove it.

Prefer explicit conditional-operation APIs in `internal/oras` over mutable shared transport state.

Treat changes to tags, media types, annotations, workspace encoding, manifests, compression or retention as storage-format changes with compatibility implications.
