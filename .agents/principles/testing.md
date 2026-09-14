# Testing Principles

Choose the smallest test level that proves the property:
- unit/table/property tests for deterministic logic;
- concurrency tests for ownership/shared state;
- Go race detector for in-process races;
- failure injection for ambiguous/error paths;
- real registry integration for registry semantics;
- compatibility tests for Terraform/framework/protocol combinations.

Concurrency changes should consider competing clients, stale owners, expiry/takeover, stale unlock, write after ownership loss, lost responses and retries.

Mocks do not prove real registry semantics. A regression test should fail if the protected bug/property is reintroduced.
