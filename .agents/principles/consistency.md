# Consistency Principles

For every distributed transition identify:
- protected/changed object;
- owner and ownership representation;
- generation/version/digest;
- competing and stale actors;
- race windows;
- linearization point, if any;
- retry/timeout/partial-failure behavior.

Read → verify → write is not atomic.

For locks always consider simultaneous acquisition, expiry, takeover, stale write, stale unlock, renewal, crash after remote mutation, lost response and retry after ambiguous success.

HTTP conditional headers are not automatically portable OCI CAS. Before relying on them prove the registry exposes a useful validator, accepts the condition on the required operation/reference, evaluates the intended object, and atomically couples validation with mutation.
