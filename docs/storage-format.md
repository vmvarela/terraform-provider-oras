---
page_title: "oras Provider — OCI Storage Format"
description: |-
  Specification of the OCI tags, manifests, annotations and media types used to store Terraform state, history and locks.
---

# OCI storage format

This page specifies how the provider lays out Terraform state, state history
and locks in an OCI repository, independently of the implementation, so that
other tools can read it. It covers the layout introduced by
[ADR-0002](../.agents/decisions/0002-workspace-identifiers.md) (hashed
workspace identifiers). Repositories written by v0.1.6 and earlier use an
incompatible legacy layout; see [workspace migration](guides/workspace-migration.md).

There is no in-band format version: the `.v1` suffixes of the artifact and
media types identify it. The provider is experimental; layout changes are
breaking and are announced in the release notes with a migration guide.

## 1. Repository and reserved tags

One repository (`oci://<registry>/<repository>`) holds any number of
workspaces. Tags starting with `state-`, `stver-`, `locked-` or `unlocked-`
are **reserved**. All other tags are ignored, so the repository may also hold
unrelated artifacts. A reserved tag that does not follow this specification
makes the provider reject the repository (§7).

| Tag | Points at | Present when |
|---|---|---|
| `state-<id>` | Current state manifest (§3) | The workspace has state |
| `stver-<id>-v<N>` | State manifest of version `N` (§5) | `max_versions > 0` |
| `locked-<id>` | Lock manifest or unlocked marker (§6) | A lock is held, or was released on a registry without manifest deletion |
| `unlocked-<id>` | Unlocked marker (§6) | The registry refused a manifest deletion (HTTP 405) at least once |

`<N>` is a canonical decimal (no sign, no leading zeros), 1 ≤ `N` ≤ 2^30.
The longest generated tag is 82 characters.

## 2. Workspace identifier

`<id>` is the lowercase hexadecimal SHA-256 digest of the exact UTF-8 bytes of
the workspace name: always 64 characters, no normalization or truncation.
For example, `default` maps to
`37a8eec1ce19687d132fe29051dca629d164e2c4958ba141d5f4133a33f0688f`.

The name cannot be recovered from the tag. Every state and lock manifest
stores it in the `org.terraform.workspace` annotation, whose value MUST hash
to `<id>`.

## 3. State manifest

An OCI image manifest (`application/vnd.oci.image.manifest.v1+json`), as
produced by ORAS `PackManifest` v1.1:

- `artifactType`: `application/vnd.terraform.state.v1`
- `config`: the OCI empty descriptor (`application/vnd.oci.empty.v1+json`, content `{}`)
- `layers`: exactly one state layer (§4)

| Annotation | Value | Present |
|---|---|---|
| `org.terraform.workspace` | Original workspace name | Always |
| `org.terraform.state.updated_at` | Write time, RFC 3339 UTC with nanoseconds | Always |
| `org.terraform.state.version` | Version `N`, decimal | Only when `max_versions > 0` |
| `org.opencontainers.image.created` | Added by ORAS, RFC 3339 UTC | Informational |

Because `updated_at` changes on every write, each write creates a new
manifest digest even for identical state bytes. The layer blob is
content-addressed and shared.

Example (compressed state, version 2), as stored on Zot:

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.terraform.state.v1",
  "config": {
    "mediaType": "application/vnd.oci.empty.v1+json",
    "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "size": 2,
    "data": "e30="
  },
  "layers": [
    {
      "mediaType": "application/vnd.terraform.statefile.v1+gzip",
      "digest": "sha256:fbf659c963d661ffae5e012c3fc999a37b68e7895762818f3e98c1d3bf8a15d2",
      "size": 36
    }
  ],
  "annotations": {
    "org.opencontainers.image.created": "2026-09-30T16:30:42Z",
    "org.terraform.state.updated_at": "2026-09-30T16:30:42.995222Z",
    "org.terraform.state.version": "2",
    "org.terraform.workspace": "default"
  }
}
```

## 4. State layer

| Media type | Content |
|---|---|
| `application/vnd.terraform.statefile.v1` | Terraform state JSON, exactly as sent by Terraform |
| `application/vnd.terraform.statefile.v1+gzip` | The same, gzip-compressed (`compression = true`) |

Readers MUST use only `layers[0]` and MUST reject any other media type. A
state manifest without layers means "no state". The provider caps state at
`max_state_size` (default 256 MiB), measured on the uncompressed bytes.

Deleting a workspace deletes the manifest behind `state-<id>`. Every tag that
points at that manifest disappears with it, including the current
`stver-<id>-v<N>` tag. Older version tags and the lock are left in place.

## 5. Versions and retention

When `max_versions > 0`, a write:

1. Reads the current version: `org.terraform.state.version` of the manifest
   behind `state-<id>`. If that annotation is absent, it uses the highest
   version tag whose manifest belongs to the workspace, or 0 if there is no state.
2. Publishes a manifest with version `current + 1`: first `state-<id>`, then
   `stver-<id>-v<N>`, both pointing at the same digest.

Concurrent writers may pick the same number or skip numbers (see
[architecture §6](architecture.md#6-versioning--allocation-race)).

After each versioned write, retention runs asynchronously and best-effort:

- It keeps the `max_versions` highest versions and deletes the manifests
  behind older version tags.
- The manifest behind `state-<id>` is never deleted.
- If a tag to keep shares a digest with a tag to delete, the kept tag first
  moves to a fresh copy of the manifest (same layer and version, new `updated_at`).
- If any version manifest belongs to another workspace, retention stops; the
  next write retries it.

On GHCR, which answers HTTP 405 to manifest deletion, deletion falls back to
the GitHub Packages API and requires the `delete:packages` scope.

## 6. Locks

A lock is an OCI image manifest without payload:

- `artifactType`: `application/vnd.terraform.lock.v1`
- `config`: the OCI empty descriptor
- `layers`: a single OCI empty descriptor (`application/vnd.oci.empty.v1+json`)

| Annotation | Value |
|---|---|
| `org.terraform.workspace` | Original workspace name |
| `org.terraform.lock.id` | Lock ID of the holder (Terraform lock UUID) |
| `org.terraform.lock.info` | JSON object with `ID`, `Operation`, `Info`, `Who`, `Version`, `Created` (RFC 3339) and `Path` (the `state-<id>` tag) |
| `org.terraform.lock.generation` | JSON object: `generation` (integer), `lease_expiry` (Unix nanoseconds, UTC), `holder_id` (equal to the lock ID); the last two are omitted when zero or empty |
| `org.opencontainers.image.created` | Added by ORAS, informational |

Semantics:

- **Held**: `locked-<id>` resolves to a manifest with a non-empty lock ID.
  **Free**: the tag is absent, or points at the unlocked marker.
- **Expired**: `lease_expiry` is positive and in the past, by the reader's
  clock. A missing or zero `lease_expiry` never expires. An acquirer whose own
  `lock_ttl` is positive may take over an expired lock.
- **Generation**: the generation of the lock found at acquisition plus one.
  It is 1 unless an expired lock is being taken over.
- **Acquire**: publish a new lock manifest under `locked-<id>`, then re-read
  the tag immediately and again after 100 ms to confirm ownership.
- **Release**: delete the lock manifest. If the registry answers HTTP 405,
  retag `locked-<id>` to the unlocked marker instead.
- **Unlocked marker**: a lock manifest whose `lock.id` and `lock.info` are
  empty strings and whose `lock.generation` is `{"generation":0}`. It is
  published once per workspace under `unlocked-<id>` and reused.

This is best-effort optimistic concurrency over mutable tags, not
compare-and-swap. Race windows and failure behavior are described in
[architecture](architecture.md) and ADR-0001.

## 7. Reader rules

The provider applies these rules, and other readers should do the same:

1. Reject the repository if a reserved tag's identifier is not 64 characters
   or its version suffix is not canonical: that is a legacy or mixed layout.
2. Before trusting the manifest behind a workspace tag, require
   `org.terraform.workspace` and check that it hashes to `<id>`. A missing or
   contradictory annotation is an error, never a guess.
3. Require `artifactType` to be empty or the expected type.
4. Bound reads: the provider caps manifests at 1 MiB and state as in §4.
5. List workspaces from `state-<id>` tags, using the annotation rather than
   the tag.

Which checks run on each operation, and what they cost, is recorded in
[ADR-0002](../.agents/decisions/0002-workspace-identifiers.md) (Amendment 1).
