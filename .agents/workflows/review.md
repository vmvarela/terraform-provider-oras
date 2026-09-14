# Review

Review independently as a production PR.

Check correctness, architecture, protocol/OCI semantics, concurrency/stale actors, errors, retries/timeouts, security, compatibility, tests, docs and unnecessary complexity.

Classify findings CRITICAL/HIGH/MEDIUM/LOW/QUESTION. For each include location, concrete problem, impact, evidence/reproduction and suggested direction.

Look for tests that mirror implementation instead of proving required behavior.
