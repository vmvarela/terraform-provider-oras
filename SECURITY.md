# Security Policy

## Supported versions

This provider is experimental and pre-1.0. **Only the latest release is supported** —
upgrade before reporting an issue. State storage format and locking behavior may change
between releases.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private vulnerability reporting:

https://github.com/vmvarela/terraform-provider-oras/security/advisories/new

We aim to acknowledge reports within 7 days and will keep you informed of progress toward
a fix and release.

## Scope

This provider handles two sensitive classes of data:

- **Registry credentials** — `ORAS_TOKEN` / `GHCR_TOKEN` / `GITHUB_TOKEN` environment
  variables, Docker config files, and Docker credential helpers are resolved for
  authentication (see `docs/guides/authentication.md`).
- **Terraform state** — state files routinely contain secrets (resource attributes,
  sensitive values, provider credentials).

In scope: credential leakage (in logs, errors, or artifacts), state exposure (e.g. pushed
to a registry with wrong permissions or media type handling), lock bypass that could
corrupt concurrent state, and any path where state or credentials escape the intended
destination.
