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

Registry-engine integration coverage belongs to [`vmvarela/oras-state`](https://github.com/vmvarela/oras-state).
Do not duplicate Zot/GHCR engine tests in this provider.

This repository owns the Terraform StateStore adapter integration. CI starts a disposable Zot
registry and runs the pinned Terraform alpha recovery harness:

```bash
go build -o terraform-provider-oras .
python3 scripts/terraform_recovery_e2e.py \
  --provider-dir "$PWD" \
  --registry localhost:5001
```

The command above expects a disposable Zot instance on `localhost:5001` and the pinned Terraform
alpha on `PATH`. CI is the canonical end-to-end environment.

## Pull requests

- **Conventional commit titles** (`feat:`, `fix:`, `docs:`) — release-drafter
  autolabels and categorises from the title. Use `!` before the colon for
  breaking changes (e.g. `feat!:`); a "breaking" warning in the PR body alone
  is **not** enough to trigger the major bump.
- CI must be green: build + tests on all three OSes, lint, and the integration jobs.
- Add or update tests for behavior changes. Unit tests use the in-memory `fakeORASRepo`
  in `helper_test.go`; no testify, no mocks — plain test doubles.
- Changes to registry semantics, retention, locking, or tag publication belong in `oras-state`; update that dependency and keep this repository focused on adapter behavior.

Keep changes minimal. The storage layout (`state-<workspace>`, `stver-<workspace>-v<N>`,
lock tags) is load-bearing — changes there are breaking and need discussion first.
