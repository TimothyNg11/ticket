# 9. Design changes driven by load testing

**Status:** accepted (Phase 9)

Each change below was made because a 50,000-buyer on-sale simulation, profiled and graphed, showed a specific bottleneck. BENCHMARKS.md has the before-and-after numbers.

## 1. Serve the seat map as pre-encoded, gzip-compressed bytes

**Found:** a CPU profile under load showed the seat-map endpoint taking 67% of API CPU: decoding the cached 5,000-seat map from Redis (32%) and re-encoding it as the response (26.5%), plus moving ~750 KB uncompressed bodies.

**Decision:** the cache stores the finished response body, gzip-compressed, built once per version. The handler writes those bytes directly (no JSON work per request). The seat-map types carry JSON tags that match the API schema exactly, so the cached encoding *is* the response. Clients that don't accept gzip get it decompressed (rare).

## 2. Coalesce seat-map rebuilds; bound staleness to 2 s

**Found:** during heavy buying the version changes many times a second, so every read missed and rebuilt from Postgres; meanwhile readers that couldn't get the rebuild lock were served a fallback copy up to 30 s old. Buyers then picked seats that were long gone and lost thousands of seat races.

**Decision:** after a rebuild, readers of newer versions get that copy for 250 ms instead of rebuilding again (one rebuild per event per 250 ms, however fast seats change). The fallback copy now expires after 2 s, the spec's seat-map TTL. A change is visible within 250 ms and never more than 2 s; holds are still decided by row locks, so an old map can only cost a buyer a retry.

## 3. Readiness doesn't depend on the shared connection pool

**Found:** under load the request pool saturated, every API pod's `/readyz` (a ping through that pool) timed out at once, and Kubernetes removed all of them from the Service. The ingress then answered everything with 503 — a slowdown turned into an outage.

**Decision:** readiness uses a background checker on its own dedicated connection, and fails only after Postgres has been unreachable for 10 s (or the pod is draining). Readiness should say "send me traffic"; a shared dependency being slow is not a reason to remove *every* pod at once.

## 4. Joining a waiting room doesn't need an Idempotency-Key

**Found:** with 50,000 joins in 30 s, the idempotency middleware's two Postgres writes per join were the largest source of database load.

**Decision:** `POST /v1/events/{id}/queue` is exempt. It's idempotent by construction (`ZADD NX` keeps the original place), so stored responses protect nothing. This narrows the spec's "every state-changing request requires an Idempotency-Key" to every request whose repetition could change state twice: holds, checkout, release, and cancel.

## 5. Shared work runs on a detached context

**Found:** 22,000 seat-map 500s. A rebuild shared by many waiting requests (singleflight) ran on the first caller's context; when that client disconnected, every waiter failed with `context canceled`.

**Decision:** the shared flight runs on `context.WithoutCancel(ctx)` with its own 5 s timeout.

## 6. Server-directed polling and Retry-After

**Found:** 50,000 buyers polling every ~10 s offered ~10,000 req/s, beyond what one machine serves; during a payment outage, clients gave up on 503s before the circuit breaker's cooldown ended.

**Decision:** queue status includes `poll_after_seconds` (half the estimated wait, 5 s–2 min), and 503 responses carry `Retry-After: 10`. Clients wait as told. The first fully passing 50,000-buyer run came from this change.

## 7. Asynchronous commits for idempotency records; group commit

**Found:** with both connection pools saturated and Postgres mostly idle, `pg_stat_activity` showed sessions waiting on WAL flushes; idempotency bookkeeping was the most frequent commit.

**Decision:** idempotency claims and saved responses commit with `SET LOCAL synchronous_commit = off` (`db.InAsyncCommitTx`). Losing one in a crash means a retried request runs again, which the orders table's unique `(user_id, idempotency_key)` still deduplicates. Orders, payments, tickets and seats keep synchronous commits. Postgres also batches concurrent commits (`commit_delay=200µs`, `commit_siblings=5`).

## 8. Set-based invariant queries

**Found:** two invariant checks used a correlated subquery per order and `payments` had no index on `order_id`: 22 s at 21,000 orders, which timed out the chaos test and would have made the 30-second invariant job useless.

**Decision:** aggregate once with `GROUP BY` joins, and index `payments (order_id, kind, status)`. 0.24 s at the same size. `TestInvariantsDetectEveryViolation` plants each kind of violation to prove the rewrite still detects them.

## 9. Sample traces under load; report client disconnects as 499

**Found:** tracing every request OOM-killed Jaeger and cost API CPU; client timeouts during overload were logged as internal errors and counted as 5xx, hiding the real errors.

**Decision:** local clusters sample 10% of traces. A request whose client went away gets 499 (not logged as an error, not counted as 5xx).
