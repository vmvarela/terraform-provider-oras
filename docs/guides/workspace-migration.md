---
page_title: "oras Provider — Workspace Mapping and Migration"
description: |-
  Workspace identifiers, legacy detection, and explicit migration and recovery.
---

# Workspace mapping and migration

## Current mapping

Every workspace name is hashed from its exact UTF-8 bytes using SHA-256.
The identifier is the full 64-character lowercase hexadecimal digest. No
normalization, truncation, or literal-name bypass is applied. The existing
prefixes remain unchanged; this experimental provider does not introduce a
versioned tag namespace.

| Object | Tag |
|---|---|
| Current state | `state-<sha256>` |
| History | `stver-<sha256>-v<N>` |
| Lock | `locked-<sha256>` |
| Unlocked marker | `unlocked-<sha256>` |

All state and lock manifests retain `org.terraform.workspace` with the exact
original name. This annotation is required, including for unlocked markers.
Tags and annotations are checked together: a legacy literal name consisting
of 64 hex characters is **not** accepted just because its tag looks hashed.
The complete tags are OCI-valid independently of workspace-name length.

The content media types, state bytes, compression and lock protocol do not
change. SHA-256 collisions are theoretically possible; observed identity
mismatches are refused, not merged. These checks are not atomic CAS.

## Compatibility and detection

This is a **breaking storage-layout change**, including for `default`.
Use a release containing this change only with a new/explicitly migrated
repository. Existing releases use the legacy layout.

The legacy layout preserved tag-compatible names and otherwise used `ws-`
plus 16 hash characters. Before each public operation the provider enumerates
all repository tags and checks every reserved state/history/lock/marker tag
against its original-name annotation. Missing metadata, mismatched identities,
legacy tags and mixed layouts fail closed. A legacy lock or history-only
repository is also rejected. Other artifact tags are ignored.

There is no automatic fallback, in-place rewrite or dual-write mode. The
extra repository-wide metadata reads are a deliberate experimental safety
tradeoff; they scale with retained tags. Registry failures propagate rather
than being treated as an empty workspace. Concurrently deleted tags are
skipped. A missing repository (404) is treated as empty.

All writers must be stopped for migration. Checks cannot fence an old binary,
manual registry edits, or a writer racing after the check. Do not run old and
new provider versions against the same repository.

## Migration to an empty repository

1. Freeze all CI and local writers for every workspace. Record the provider
   and Terraform alpha versions and the repository URL. Resolve or explicitly
   recover old locks only after confirming no operation is running.
2. Back up the original repository, including tagged manifests and their
   referenced blobs. Inventory all current states, historical versions and
   locks. Record each manifest digest, its original-name annotation and the
   intended workspace. Protect these backups as secrets.
3. Validate the inventory. For a legacy hash/literal collision, the tag alone
   cannot identify both original states. Stop and recover the missing state
   from a verified historical snapshot or independent backup. Do not copy the
   one surviving payload to both names. A missing/contradictory annotation
   likewise requires operator reconciliation before migration.
4. Export each **verified current state** using the previously pinned provider
   and Terraform version while all writers remain stopped. Terraform's
   `state pull` may be used where supported by that alpha build; alternatively
   retrieve the state manifest's sole state layer and decompress it when its
   media type ends in `+gzip`. Verify workspace ownership, JSON, lineage,
   serial and payload checksum. Never recreate state by applying infrastructure.
5. Configure the updated provider with a **separate empty OCI repository**.
   Transfer each verified payload to its exact original workspace using the
   supported state-import/push operation for the pinned Terraform alpha.
   Do not copy legacy tags or stale lock manifests into the destination.
   This guide does not promise identical CLI syntax across experimental
   Terraform builds; validate the available migration command in a sandbox.
6. Read each destination state back and compare payload, lineage and serial
   (accounting for any intentional Terraform serialization). Check workspace
   listing and independent lock/unlock operations. Review a plan without
   applying it. No infrastructure recreation should be necessary.
7. Switch all writers to the destination together and keep the source
   read-only. Historical OCI tags remain archived at the source; destination
   version numbering starts afresh. This procedure transfers current states,
   not historical tag counters. Keep backups until recovery has been tested.

The implementation tests verify payload transfer between isolated repositories
and refusal to mutate legacy/mixed data. They do not certify a particular
Terraform alpha CLI's migration UX.

## Rollback and recovery

Before any destination writes beyond migration, stop writers and restore the
old provider/configuration pointing to the untouched source after verifying
it still represents the current infrastructure.

After any new infrastructure operation, the source snapshot may be stale.
Stop writers, export the latest verified destination state, and reconcile
lineage/serial and infrastructure before importing into an isolated legacy
recovery repository. Verify it with the previous provider before switching.
Never simply repoint to the old snapshot or retry an apply blindly.

If a partial transfer fails, keep writers stopped, preserve the source and
all exported payloads, inspect destination states and resume only missing or
verified transfers. Do not delete the source or use force options to suppress
identity/lineage conflicts. Restore missing historical data from the archived
repository. An overwritten state with no surviving backup cannot be recovered
by changing workspace names.
