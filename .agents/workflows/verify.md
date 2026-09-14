# Verify

Map Claim → Evidence/Test → Result → Remaining uncertainty.

As applicable run formatting, focused tests, `go test ./...`, `go test -race ./...`, vet/static analysis, configured lint, failure injection, registry integration, Terraform compatibility, docs/examples review and final diff inspection.

Never claim a command passed unless actually run. Mocks do not prove real registry semantics.

Report verified claims, commands/tests, failures, unverified assumptions, residual risks and PASS / PASS WITH LIMITATIONS / FAIL.
