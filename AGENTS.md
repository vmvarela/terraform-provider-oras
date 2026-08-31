# AGENTS.md — terraform-provider-oras

Terraform Plugin Framework provider implementing `statestore.StateStore` for OCI registries via ORAS. Module: `github.com/vmvarela/terraform-provider-oras`.

## Quick start

```bash
go build -o terraform-provider-oras .
make test              # go test -race -count=1 ./...
make install           # build + copy to ~/.terraform.d/plugins/.../darwin_arm64/
```

## Environment

- **Go 1.26**, **Terraform 1.17.0-alpha20260827** (`.terraform-version` via tfenv)
- `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1` required at runtime for Terraform to discover the experimental state store
- Auth: `ORAS_TOKEN`, `GHCR_TOKEN`, or `GITHUB_TOKEN` env vars (checked in that priority order), then CLI config `oci_credentials` blocks + Docker config files + Docker credential helpers, then anonymous (see `resolveCredentials` in `internal/oras/auth.go`, `internal/oras/credsource.go`, `internal/oras/dockerconfig.go`)
- Dev overrides: symlink or point `.terraformrc.dev` at the repo, then `cp .terraformrc.dev ~/.terraformrc`

## Test

```bash
make test               # unit tests with race detector
TF_ORAS_ZOT_TEST=1 make test-zot   # Zot integration (requires Docker)
```

- Unit tests use `fakeORASRepo` in-memory repo (`helper_test.go`) — no external dependencies
- No testify, no mocks; manual test doubles via interface delegation
- Integration tests spin a Zot container per test via Docker (tagged `zot-linux-amd64:v2.1.0`)
- `make lint` runs `golangci-lint run ./...`

## Architecture

```
main.go → providerserver.Serve → internal/provider/ (OrasProvider)
                                     │
                                     ├─ ProviderWithStateStores returns OCIStateStore factory
                                     └─ ProviderData flows to Initialize → Configure chain
                                              ↓
internal/statestore/oci.go → ProviderData + wraps internal/oras/ client
internal/oras/             → ORAS push/pull/lock/delete via oras-go v2, auth, GHCR fallback
```

- OCI tag scheme: `state-<workspace>`, `stver-<workspace>-v<N>`, `locked-<workspace>`, `unlocked-<workspace>` (GHCR fallback)
- Lock uses generation-based optimistic concurrency; stale locks auto-cleared via TTL
- Async retention goroutines (semaphore-limited, sem=3) prune old versions on Put
- `Client.WaitForRetention()` blocks until all async retention completes — call before assertions in tests

## Release / CI

- CI: `.github/workflows/ci.yml` runs `go build ./...` + `go test -race -count=1 ./...` on ubuntu/macos/windows, plus `golangci-lint` v2.12
- Release: GoReleaser (`.goreleaser.yml`) on tags `v*`, draft prerelease, optional GPG signature
- Functional example: `examples/main.tf` (Zot local + `insecure` provider)

## Notes

- Provider has zero data sources and zero resources — state-store-only
- Media types: `application/vnd.terraform.statefile.v1` (plain), `+gzip` (compressed)
- Default max state size: 256 MiB
