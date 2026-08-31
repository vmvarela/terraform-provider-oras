# Contributing

Thanks for helping. This provider is experimental — breaking changes to the state storage
format or tag scheme are possible until 1.0.

## Prerequisites

- **Go 1.26** (see `go.mod`)
- **Terraform 1.17 alpha** — pinned in `.terraform-version`, install via tfenv:
  `tfenv install && tfenv use`
- **Docker** — required for the Zot integration tests
- `golangci-lint` (v2, see `.golangci.yml`) for `make lint`

## Build, test, lint

```bash
make build       # go build -o terraform-provider-oras .
make test        # unit tests with race detector; uses in-memory fake repo, no external deps
make lint        # golangci-lint run ./...
```

## Local dev override

Run the provider from this checkout without installing it into the plugin directory:

```bash
make dev-override
export TF_CLI_CONFIG_FILE=$PWD/.terraformrc.dev
```

`.terraformrc.dev` is gitignored and scoped to this repo — don't overwrite `~/.terraformrc`,
it may hold your `oci_credentials` blocks.

## Integration tests

Zot integration tests spin up a Zot registry container per test via Docker:

```bash
TF_ORAS_ZOT_TEST=1 make test-zot
```

GHCR integration tests run against a real registry and need a token:

```bash
TF_ORAS_GHCR_TEST=1 TF_ORAS_GHCR_TOKEN=<token> go test -race -v ./internal/oras/... -run TestGHCRIntegration
```

## Pull requests

- **Conventional commit titles** (`feat:`, `fix:`, `chore:`, `docs:`) — release-drafter
  autolabels from them.
- CI must be green: build + tests on all three OSes, lint, and the integration jobs.
- Add or update tests for behavior changes. Unit tests use the in-memory `fakeORASRepo`
  in `helper_test.go`; no testify, no mocks — plain test doubles.
- If you touched async retention or locking, call `Client.WaitForRetention()` before
  assertions.

Keep changes minimal. The storage layout (`state-<workspace>`, `stver-<workspace>-v<N>`,
lock tags) is load-bearing — changes there are breaking and need discussion first.
