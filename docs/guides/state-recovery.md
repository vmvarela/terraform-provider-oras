---
page_title: "oras Provider — State Write Recovery"
description: |-
  Non-renewing leases, failed-write outcomes, and safe Terraform state recovery.
---

# State write recovery

A failed state write does **not** imply that infrastructure changes failed.
Do not blindly repeat an apply: the remote snapshot may predate changes already
made, or the write may have succeeded despite a lost response.

## Lease behavior

- Positive `lock_ttl` values are **non-renewing** leases. Expiry is fixed at
  acquisition; a long apply can change infrastructure and then have its next
  state write refused. Allow for the entire operation, including slow registry
  requests, when selecting a TTL. This is not a recommendation to change the default.
- Unset or `0` means no expiry. An orphaned lock needs operator intervention;
  enabling TTL later does not give an existing non-expiring lock an expiry.
- There is no background renewal or cleanup. Eligible later lock acquisitions
  attempt to clear expired locks.
- Times are compared using clients' local wall clocks. Clock skew can cause
  premature takeover or delayed expiry. The registry does not enforce leases.
- Verification and publication are separate operations, **not atomic CAS**.
  If verification passes before expiry, an in-flight write can still land after
  expiry or after another writer takes over. Tests deliberately preserve this
  limitation. `-lock=false` bypasses ownership verification.

## Classify the result before acting

| Outcome | Evidence | What is not established |
|---|---|---|
| Rejected before publication | Lock verification refused or was interrupted; this Write never called Put | Earlier writes or infrastructure changes may already have occurred; another writer may have changed remote state |
| Uncertain publication | Interrupted Put, ambiguous tag observation, or other unconfirmed write | Do not assume the state is unchanged; inspect the current snapshot and history |
| Partial publication | State publication confirmed, subsequent version-tag publication failed (including cancellation) | The current tag may since have moved; history metadata may be incomplete |

The provider does not automatically retry an uncertain mutable-tag publication
over another writer. This does not close the verification-to-write race.
Do not re-apply merely to repair missing history metadata.

## Recovery procedure

1. Stop all writers for the workspace, including automation and processes with
   expired leases. Confirm no original apply is still running before clearing
   a lock. Record the exact Terraform/provider versions, registry, repository,
   workspace, and failure phase.
2. Preserve any local recovery state **before** deleting the working directory
   or rerunning Terraform. Treat `errored.tfstate`, plans, backups and raw logs
   as secrets: use a private directory, restrictive permissions and encrypted
   storage. Never commit them or upload them as ordinary CI artifacts. A failed
   write does not guarantee that a recovery file could be created (disk, process
   and permission failures can prevent it).
3. With the same pinned Terraform and configuration, inspect remote state from
   a fresh process. For the tested alpha, `terraform state pull` retrieves it.
   Redirect output into private storage, not a CI console. Preserve the remote
   manifest digest and history as evidence before any write.
4. Compare workspace, lineage, serial, resource identities and actual
   infrastructure between remote state and the recovery snapshot. A newer
   serial alone does not prove a snapshot is complete or authoritative. If
   another writer advanced state, reconcile both sets of changes first. If no
   trustworthy recovery snapshot exists, stop and reconstruct from verified
   backups/imports; do not recreate infrastructure by blindly applying.
5. Only after reconciliation and exclusive operational control, push the
   verified recovery file with `terraform state push errored.tfstate`, using
   normal locking and a lease long enough for the recovery. **Do not use
   `-force` or `-lock=false` to suppress conflicts.** A lineage/serial or lock
   conflict is a reason to investigate, not overwrite. This is a new operation
   with a fresh lock, not renewal of the expired apply's lease.
6. Pull again from a new process and verify the intended resources, lineage and
   serial. Review a plan without applying. Resume writers only when remote
   state and infrastructure agree; retain protected backups until verified.

Changing TTL alone does not recover state already lost from the remote store.
Partial publication may need no state push if the current snapshot already
contains the intended result. Treat asynchronous retention failures separately
from foreground state/version publication.

## Reproducible experiment and scope

The CI experiment is pinned to Terraform **1.17.0-alpha20260827**, Zot **2.1.0**,
terraform-plugin-framework **1.19.0**, and terraform-plugin-go **0.31.0**
(protocol v6, `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1`). It uses an anonymous local
registry and a built-in `terraform_data` resource whose provisioner writes a
synthetic external marker. No cloud infrastructure or real credentials are used.

```sh
go build -o terraform-provider-oras .
# Start the same Zot 2.1.0 fixture used by .github/workflows/ci.yml first.
python3 scripts/terraform_recovery_e2e.py --provider-dir "$PWD" --registry localhost:5001
# Without Docker, pass --zot-binary /absolute/path/to/zot instead of --registry.
```

The harness requires the exact Terraform version on PATH (or `--terraform`),
uses separate randomly named repositories, captures command output privately,
and emits only version information and assertion results. It removes its
disposable local evidence on success; failure paths report a private directory
for local investigation. Never upload its raw logs/state. The local registry
is disposable and retains synthetic test repositories until removed.

Assertions cover successful apply/read/update/read in separate processes; an
apply exceeding a five-second lease after changing the marker; nonzero exit
and recovery artifact; unchanged remote snapshot; normal locked recovery push;
fresh-process verification and a no-change plan. CI results are recorded in the PR.
The first [verified run](https://github.com/vmvarela/terraform-provider-oras/actions/runs/35317081115)
on 2026-09-18 observed exit code **1**, `errored.tfstate` containing the changed
resource, unchanged remote state/manifest, and successful normal `state push`
followed by fresh-process reads and a plan exiting **0** (no changes).
These results are specific to this alpha, provider and Zot combination, not a
promise for other Terraform builds, registries, crashes or failure modes.

Complementary race/failure-injection tests retain competing-writer scenarios,
ambiguous publication, partial history publication, and the documented
`TestStateStoreWriteExpiryDuringW1Limitation` and
`TestStateStoreWriteTOCTOULimitation`. The real CLI experiment does not simulate
every one of those network/interleaving failures.

General command semantics: [Terraform state push reference](https://developer.hashicorp.com/terraform/cli/commands/state/push).
The pinned experiment, not stable-release documentation, establishes StateStore
compatibility for this provider.
