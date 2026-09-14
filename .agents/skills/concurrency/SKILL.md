---
name: concurrency
description: Models multi-actor concurrency for distributed state operations, including A/B actors, stale ownership, race windows and ambiguous retries. Use when designing, implementing or reviewing any concurrency-sensitive change to state locking, generation handling or registry mutations.
---
# Concurrency Skill

Model at least actors A and B for every concurrency-sensitive change.

Core scenario:
A acquires generation X → expires → B takes generation Y → A continues.
Then test A write, renew and unlock. Stale A must not invalidate B.

Verification race:
A reads/verifies X → B changes X to Y → A mutates based on X.
Identify what prevents stale mutation. If nothing does, document the race.

Ambiguous network:
A sends mutation → registry applies it → response is lost → A retries.
Classify retry as safe, idempotent, conflicting or ambiguous.

Also inspect mutex scope, maps, shared clients/transports, mutable request configuration, goroutine lifetime and race-detector output. An in-process mutex does not solve a distributed race.

For designs/reviews state the invariant, actors, generations/state machine, any linearization point, race windows, stale behavior, retries and tests.
