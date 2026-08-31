# Compatibility: terraform-provider-oras ↔ action-ghoten

Wire-level compatibility verification between this provider and the ORAS backend of
[PrisaMedia/action-ghoten](https://github.com/PrisaMedia/action-ghoten)
(`internal/backend/remote-state/oras/`). This provider's ORAS client was originally
ported from the ghoten fork; both use `oras-go v2.6.0` (identical wire stack).

**Method**: comparative static analysis of both codebases, verified claim by claim
against both code bases (not just fact sheets).

## Verdict

| Scenario | Verdict |
|---|---|
| (a) Concurrent use of the same workspace | **COMPATIBLE WITH LIMITATIONS** — no data-loss risk; operational limitations (see findings) |
| (b) Sequential migration (one writes, the other reads) | **COMPATIBLE** |
| (c) Independent parallel workspaces in the same repository | **COMPATIBLE** — disjoint tag namespaces |

## Compatibility matrix

| Dimension | Provider | Ghoten | Compatible |
|---|---|---|---|
| State tags `state-<ws>` / hash `ws-<hex(sha256[:8])>` | `internal/oras/client.go:62,865-872` | `client.go:99,1125-1132` | ✅ identical |
| Media types `application/vnd.terraform.statefile.v1` (+gzip) and artifactTypes `.state.v1` / `.lock.v1` | `client.go:38-41` | `client.go:36-39` | ✅ identical |
| 6 annotation keys (`org.terraform.*`) | `client.go:43-48` | `client.go:41-46` | ✅ identical |
| Manifest: PackManifest v1.1, 1 layer, no config, no subject | both | both | ✅ identical |
| Lock protocol (gen+1, post-write verify, holder ID check, 405→retag-to-unlocked) | `client.go:358-475,728-771` | `client.go:809-905,991-1014` | ✅ identical |
| Retention (async, sem=3, 30s ctx, keep-N, digest grouping lim 10, GHCR fallback) | `client.go:270-726` | `client.go:480-784` | ✅ same shape |
| Read limit 256 MiB, 404⇒nil/no-op | both | both | ✅ identical |
| `insecure` (PlainHTTP + InsecureSkipVerify) | `auth.go:116-118` + `httputil/httpclient.go:26` | `backend.go:436-438,476` | ✅ equivalent |
| **Version tags** | `stver-<ws>-v<N>` (`client.go:65-66,525-527`, updated for parity) | `stver-<ws>-v<N>` (`client.go:102,530-532`) | ✅ identical (updated 2026-08-31: provider switched from `state-…-v<N>` to ghoten's `stver-` prefix; splitStateVersionTag now guards on the prefix, eliminating the `-v<N>`-suffix workspace-name ambiguity) |
| Delete | state manifest only (`client.go:330-339`), no 405 fallback — leaves `stver-<ws>-v<N>` version tags dangling (retention's `groupVersionsByDigest` skips unresolvable tags) | `delete()` identical no-fallback (`client.go:786-801`); `DeleteWorkspace` additionally cleans version+lock+unlocked tags (`backend.go:306-370`) | ✅ same limitation |
| Auth | env (`ORAS_TOKEN`/`GHCR_TOKEN`/`GITHUB_TOKEN`) first, then CLI config `oci_credentials` blocks + Docker config files + Docker credential helpers (`internal/oras/credsource.go`, `dockerconfig.go`), then anonymous | cliconfig `oci_credentials` blocks + `oci_default_credentials` + Docker configs + helpers, then anonymous (`backend.go:565-589`) | ✅ same source family (env vars remain provider-only, ahead of the rest) |
| Retry | fixed 3 attempts, waits 1s/2s (`client.go:1006-1028`) | default 3 attempts, waits 1s/2s; multiplicative with 30s cap only materializes at 4+ attempts (`backend.go:208-222`) | ✅ equivalent at defaults |
| GHCR delete fallback (Packages API) | `internal/oras/ghcr.go:39-206` | `github.go:35-202` | ✅ equivalent |
| Gzip | default level | BestSpeed | ✅ level-agnostic on read |

## Findings (all LOW, no data-loss risk)

1. **Version-tag prefix divergence — RESOLVED 2026-08-31.** The provider now writes
   `stver-<ws>-v<N>` (ghoten's format) instead of `state-<ws>-v<N>`. The legacy
   provider format is no longer produced or parsed. The `stver-` prefix guard in
   `splitStateVersionTag` also eliminates the workspace-name-ending-in-`-v<N>`
   ambiguity (ghoten's original rationale, its client.go:105-111).
   Operational notes: (a) pre-existing `state-<ws>-v<N>` tags from old provider
   versions surface as phantom workspaces in workspace listing and are never pruned
   — clean them up manually if any exist; (b) both tools now share the `stver-`
   namespace, so cross-tool retention converges: whichever tool has the smaller
   `max_versions` prunes the other tool's versioned tags too (same behavior as two
   ghoten runs with different settings — intended parity).
2. **Version-number regression — accept.** The provider's `currentStateVersion`
   fallback only lists its own tags; after a ghoten write without a version
   annotation, numbering can reset (10→1). Benign: versions are just tags; each tool
   prunes its own namespace.
3. **(Correction) `insecure` equivalent** — the provider does configure
   `InsecureSkipVerify` via `httputil.BuildHTTPClient`. No action.
4. **Credential provisioning — RESOLVED 2026-08-31.** The provider now also resolves
   credentials from CLI config `oci_credentials` blocks, Docker config files
   (`auths`/`credHelpers`/`credsStore`), and Docker credential helpers — the same
   source family as ghoten — after its env vars. Provider-specific env resolution
   (`ORAS_TOKEN`/`GHCR_TOKEN`/`GITHUB_TOKEN`) remains first in the order. The
   provider's GHCR fallback still requires an access token; ghoten also accepts a
   password.
5. **No ORAS auth cache — optional.** The provider builds `orasAuth.Client` without
   `Cache` (`auth.go:144-147`); it may re-negotiate the token on every request.
   Performance only.
6. **UA omitted on registry requests — optional.** With `cfg.HTTPClient` set (normal
   flow), the provider does not send its User-Agent to the registry; only on GHCR API
   calls. Cosmetic (registries policing UA).
7. **Listing divergence with ambiguous `-v<N>` names — accept.** Both miss real
   workspaces with a `-v<N>` suffix shadowed by version tags; inherent to the tag
   scheme, not a regression.
8. **Lock TTL asymmetry — note.** Stale-clear requires the checker to have
   `lock_ttl>0` AND the lock to have `lease_expiry>0`. With `lock_ttl` configured on
   both tools, behavior is symmetric.

## Verification limitations (static analysis)

What static analysis cannot prove, and how to test it if needed:

1. **Live registry quirks** (real GHCR): 405 behavior, `artifactType`/annotation
   preservation on push, Packages API delete semantics.
2. **Cross-tool lock races**: no cross-process contention test exists. A harness
   against Zot is possible (`TF_ORAS_ZOT_TEST=1 make test-zot`) importing ghoten's
   client via `replace`.
3. **Cross-read of gzip**: write compressed with one, read with the other (both use
   standard gzip readers — level-agnostic).
4. **Version-number regression scenario** (finding 2): only constructable against a
   live registry with the exact write sequence.
5. **Eventual consistency of tag listing** under load: retention compensates (both
   append the just-written version).

Recommendation: a cross-tool integration test against Zot — ghoten writes
state+lock, the provider reads/unlocks and vice versa, plus a retention pass with
both versioning schemes — using the existing `zot_test.go` scaffolding.

