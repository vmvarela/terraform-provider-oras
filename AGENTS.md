# AGENTS.md

If sources disagree, consult `.agents/` (see `.agents/README.md#authority`); this file is an index.

## Project

`terraform-provider-oras` is an experimental Terraform StateStore
provider that stores Terraform state in OCI-compatible registries
using ORAS.

## Architecture

The intended dependency direction is:

Terraform StateStore
→ internal/statestore
→ internal/oras
→ ORAS Go
→ OCI registry

Terraform-specific concerns must remain in `internal/statestore`.
OCI/registry-specific behavior must remain in `internal/oras`.

## Core principles

- Correctness over feature count.
- Evidence over assumptions.
- Prefer simple designs over clever abstractions.
- Do not claim guarantees unsupported by the underlying registry.
- Do not silently change architecture while implementing a task.
- Avoid unrelated refactoring.
- Preserve compatibility unless a deliberate change is being made.

## Consistency

Distributed operations must explicitly identify:

- ownership
- generation/version
- race windows
- failure modes
- retry behavior
- stale actors

Never describe verify-then-write as atomic CAS.

Registry-specific behavior must be treated as registry-specific
unless it is demonstrated to be portable.

## Testing

Concurrency-sensitive changes require appropriate:

- unit tests
- race detection
- adversarial scenarios
- failure injection
- integration tests

A test should demonstrate the property we actually care about,
not merely execute the implementation.

## AI-assisted development

AI-generated code and analysis are untrusted until reviewed.

The human maintainer owns:

- architecture
- protocol interpretation
- consistency guarantees
- compatibility claims
- security decisions
- final acceptance

## Project knowledge

Read the relevant files under `.agents/` before working on:

- Terraform StateStore
- OCI/ORAS behavior
- concurrency
- registry integration

## Workflow

For non-trivial work:

investigate
→ design
→ implement
→ review
→ adversarial review
→ verify

Do not skip investigation when correctness depends on external
protocols, specifications, or registry behavior.
