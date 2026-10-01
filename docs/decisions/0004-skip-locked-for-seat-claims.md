# 4. Claim seats with `FOR UPDATE SKIP LOCKED`

**Status:** accepted (Phase 2)

## Context

Thousands of users may try to hold the same few seats within milliseconds. Exactly one may win each seat, and losers should find out fast.

## Options

1. **Plain `SELECT … FOR UPDATE`:** correct, but losers queue behind the winner's lock while each holds a connection, then fail anyway. Latency and connection use grow with the size of the crowd.
2. **Optimistic concurrency** (`UPDATE … WHERE state = 'available' AND version = $v`, retry on 0 rows): no waiting, but every loser does a full round of work, and multi-seat holds need extra logic to stay all-or-nothing.
3. **`FOR UPDATE SKIP LOCKED`:** lock the seats that are free *and* not being claimed right now; anything locked by someone else counts as unavailable.

## Decision

Option 3, with `ORDER BY id` for a consistent lock order. If fewer rows come back than requested, roll back and return 409.

## Consequences

- Losers fail immediately instead of waiting, so connection hold time stays flat as contention grows.
- A seat being claimed by a transaction that later rolls back is briefly reported as unavailable; the user retries a moment later. Acceptable for a flash sale, and never unsafe.
- The same pattern makes the hold sweeper safe to run as many replicas.
- Correctness doesn't rest on the lock alone: the `event_seats_state_refs` check, the foreign keys, and `tickets_one_valid_per_seat` still hold even if application code is wrong.
