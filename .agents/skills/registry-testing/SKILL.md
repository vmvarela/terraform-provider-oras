---
name: registry-testing
description: Designs and documents experiments against real registries (Zot, GHCR, Harbor) with conditional-write scenarios, because mocks are insufficient evidence for registry behavior. Use when correctness of an operation depends on observed registry behavior rather than specification or ORAS semantics alone.
---
# Registry Testing Skill

Use when correctness depends on real registry behavior. Mocks are insufficient evidence.

Distinguish Zot/local CI, GHCR, Harbor and any additional claimed registry.

For each experiment record product/version when known, auth mode, operation/reference, relevant request conditions, expected behavior, observed status/headers, repeatability, conclusion, confidence and limitations. Never store credentials.

Useful conditional-write experiments:
1. two clients race create-if-absent;
2. A reads X, B moves tag to Y, A conditionally updates X;
3. same setup with stale conditional delete;
4. B/C race takeover of expired lock;
5. ambiguous timeout/retry where practical.

Define the property before running the experiment. Phrase conclusions as specification-required, ORAS-supported, observed on registry/version, not established, or best-effort. Never generalize one registry result to OCI-wide behavior.
