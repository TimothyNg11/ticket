# Benchmarks

Every load and chaos run, with its settings and results, in the order they were run. Each one ends with the invariant checker (no seat with two valid tickets, every sold seat tied to a confirmed order, every confirmed order paid exactly once with tickets matching seats, no held seat without an active hold). Raw output for each run is in `loadtest/results/`.

**Environment (all runs):** one Windows laptop, 16 logical CPUs. A 3-node kind cluster runs in Docker Desktop's VM (16 CPUs, 8 GB RAM) alongside everything else: API (HPA 3–6 pods, 1 CPU each), PgBouncer ×2, Postgres (2 CPU limit), Redis, Traefik, 7 workers, Prometheus/Grafana/Jaeger, and, from run 4 on, the load generator itself. Absolute numbers are bounded by sharing one machine; the before-and-after deltas are the point.

**Scenario** (`loadtest/onsale`, the spec's on-sale): 50,000 buyers rush the waiting room of an event with 5,000 seats within 30 s. The admitter lets in 500 every 10 s. Waiting buyers poll their place in line and the seat map; admitted buyers read the seat map, hold 1–4 seats (most take 2), and check out. Clients retry transport errors and 502/503/504 with the same Idempotency-Key. "Failed" means a request still failed after 5 retries.

## Summary

| # | Run | Result | Seats sold | Orders | Duration | Throughput | Failed requests | Invariant violations |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | Baseline | FAIL | 4,391 / 5,000 | 2,165 | 13 m 56 s | 275 req/s | 46,687 | 0 |
| 2 | + pre-encoded, gzipped seat map | FAIL | 2,743 | 1,390 | 9 m 34 s | 453 req/s | 60,877 | 0 |
| 3 | + readiness off the shared pool, join without idempotency writes | FAIL (harness-bound) | 1,686 | 812 | 10 m 11 s | 217 req/s | 50,609 | 0 |
| 4 | Load generator moved into the cluster; + singleflight fix, Redis pool sizing | FAIL (overloaded) | 3,028 | 1,645 | 19 m 28 s | 348 req/s | 51,310 | 0 |
| 5 | **+ server-directed polling (`poll_after_seconds`)** | **OK** | **4,999** | 2,317 | 5 m 8 s (sold out 2 m 3 s) | 670 req/s | **0** | 0 |
| 6 | Same build, re-measured with server-side breakdown | OK | 5,000 | 2,365 | 5 m 59 s (sold out 2 m 20 s) | 657 req/s | 0 | 0 |
| 7 | **+ async-commit idempotency records, group commit** | **OK** | **5,000** | 2,442 | 6 m 6 s (sold out 3 m 3 s) | 634 req/s | **0** | 0 |

Correctness held in every run, including the ones that fell over: **0 invariant violations across 50,000-buyer runs, ~2.6 million requests in total.**

## Latency (client side, p50 / p99)

Measured from the moment a request gets a connection to the end of the response, so client-side queueing for sockets is excluded (it's reported separately in the raw results as `connwait99`).

| Run | seat map | hold | checkout | queue poll |
| --- | --- | --- | --- | --- |
| 1 Baseline | 496 ms / 5.5 s | 511 ms / 4.5 s | 2.2 s / 7.3 s | 377 ms / 5.2 s |
| 2 Pre-encoded seat map | **67 ms** / 4.3 s | **72 ms** / 5.8 s | **70 ms** / 11.3 s | 185 ms / 1.3 s |
| 5 Server-directed polling | 880 ms / 6.5 s | 5.9 s / 13.6 s | 13.3 s / 24.8 s | 715 ms / 5.2 s |
| 7 WAL fixes | 2.2 s / 8.8 s | 2.8 s / 12.8 s | 5.0 s / 22.2 s | 1.9 s / 8.1 s |

Run 2's medians show the seat-map fix's effect on the server; its tails show the readiness cascade it exposed. From run 4 on, the load generator shares the cluster's CPUs, which inflates every client-side number; the server-side table below is the cleaner comparison for runs 6 → 7.

## Server side (runs 6 → 7), from Prometheus over the run window

| | Run 6 | Run 7 |
| --- | --- | --- |
| hold p50 (inside the API) | 4.44 s | **1.00 s** |
| checkout p50 | ≥ 10 s (top bucket) | **3.11 s** |
| seat map p50 | 0.62 s | 0.68 s |
| DB pool: total wait per second | 171 s/s | **50 s/s** |
| Redis pool: total wait per second | 347 s/s | 324 s/s |
| 5xx per second | 11.9 | **0.46** |
| CPU (cores): API / Postgres / Redis / Traefik / load generator | 1.9 / 1.3 / 0.4 / 1.0 / 1.5 | 2.1 / 1.3 / 0.4 / 1.0 / 1.8 |

## What each change fixed, and how it was found

1. **Pre-encoded, gzipped seat map (run 1 → 2).** A 30 s CPU profile of an API pod under load (`loadtest/profile-under-load.sh`) showed the seat-map handler at 67% of CPU: 32% decoding the cached 5,000-seat map from Redis and 26.5% re-encoding it. The cache now stores the finished, gzip-compressed response, built once per change; the handler writes the bytes. Seat-map p50: 496 → 67 ms, holds 511 → 72 ms. ADR 0009.
2. **Rebuild coalescing and bounded staleness (with 2).** Buyers lost 33,000 seat races in run 1 because a fallback copy could be 30 s old. Rebuilds are now coalesced to one per event per 250 ms, and every copy expires within 2 s.
3. **Readiness off the shared pool (2 → 3).** Run 2 turned into a flood of 503s: under load the request pool saturated, every pod's `/readyz` (a ping through that pool) timed out at once, and Kubernetes pulled all six pods from the Service. Readiness now uses its own connection and fails only after 10 s of unreachability.
4. **Queue joins skip idempotency writes (with 3).** Two Postgres writes per join × 50,000 joins in 30 s was the largest DB load; joining is idempotent by construction (`ZADD NX`).
5. **Load generator inside the cluster (3 → 4).** With the generator on the Windows host, the server measured seat-map p50 at 5 ms while the client saw 2.2 s: Docker Desktop's userspace port proxy was the bottleneck. The generator now runs as a Job through Traefik (`loadtest/job.sh`).
6. **Singleflight cancellation bug (with 4).** 22,000 seat-map 500s: a shared rebuild ran on the first caller's context, so when that client disconnected every waiting request failed. The flight now runs on a detached context (`TestSharedRebuildSurvivesLeaderDisconnect`).
7. **Server-directed polling (4 → 5).** 50,000 buyers each polling twice every ~10 s offer ~10,000 req/s, far beyond this machine. The waiting room now returns `poll_after_seconds` (half the estimated wait, 5 s–2 min): far-back buyers poll rarely, near-front buyers often. First fully passing run: every seat sold in about 2 minutes, zero failed requests.
8. **WAL-bound commits (6 → 7).** Sampling `pg_stat_activity` mid-sale showed sessions waiting on `LWLock:WALWrite`/`WALInsert`: commits were bound by log flushes on Docker Desktop's virtual disk. Idempotency bookkeeping now commits with `synchronous_commit = off` (losing one on a crash just means a retry re-runs, still deduplicated by the orders table), and Postgres batches concurrent commits (`commit_delay`). Hold p50 4.4 → 1.0 s, checkout ≥10 → 3.1 s, DB pool waits 171 → 50 s/s.

**Next bottleneck (not yet fixed):** Redis client pool waits (324 s/s) while Redis itself uses 0.4 cores, so requests queue for Redis connections inside the API. Leading hypotheses: seat-map reads move up to two ~60 KB values per request (current version and latest copy in one `MGET`), and Go scheduling in 1-CPU pods delays the release of connections. The next step is fetching the latest copy only on a miss, then profiling again.

## Seat-map read throughput (k6)

`loadtest/k6/seatmap.js`, in-cluster through Traefik, constant arrival rate in 45 s stages, 5,000-seat map, gzip.

| Arrival rate | p99 | Errors | 300 ms target |
| --- | --- | --- | --- |
| 200 req/s | **73 ms** | 0% | met |
| 500 req/s | **182 ms** | 0% | met |
| 1,000 req/s | 508 ms | 0% | missed |
| 1,500 req/s | 5.67 s | 0% | missed (k6 dropped 13,261 iterations: past the knee) |

The p99 target (300 ms for seat-map reads, set after the first baseline) holds up to **500 req/s** on this machine; the knee is between 500 and 1,000 req/s.

## Chaos (`loadtest/chaos.sh`)

Two-minute flash sale (300 buyers, 2,000 seats). In sequence: an API pod is killed (t=15 s), the payment service is killed (t=30 s), payments return 40% errors (half after charging) at 1.5 s latency (t=45 s), Redis is killed (t=60 s), the reconciler and outbox relay are killed (t=75 s), payments recover (t=90 s).

| Attempt | Result | What it found |
| --- | --- | --- |
| 1 | FAIL | The system recovered (0 pending, outbox drained, 0 violations), but the final invariant check timed out: two checks used a correlated subquery per order, and `payments` had no index on `order_id`. **22 s → 0.24 s** after rewriting them as set-based joins and adding the index (`TestInvariantsDetectEveryViolation` guards the rewrite). |
| 2 | FAIL | 81 checkouts gave up on `503 PAYMENT_UNAVAILABLE` while the breaker was open: the response didn't say when to retry. 503s now carry `Retry-After: 10` (the breaker's cooldown) and clients honor it. |
| 3 | **OK** | 2,147 confirmed, 218 cancelled, **0 failed operations** (47 retries absorbed). After recovery: 0 orders pending payment, outbox fully published, 0 refunds outstanding, **0 invariant violations across 25,413 orders**. |

## Earlier correctness checks

| Phase | Test | Result |
| --- | --- | --- |
| 2 | 500 parallel holds for the same 10 seats | exactly 10 winners |
| 5 | 10,000-user queue burst, one batch of 500 admitted | exactly those 500 could hold |
| 6 | Payment chaos: 30% errors, 10% declines, 20% timeouts | provider ledger and orders match exactly |
| 7 | 60 s flash sale, an API pod deleted every 15 s | 0 failed orders, 0 retries needed |
| 8 | 800 ms payment latency injected mid-sale | checkout p99 tracked payment p99; DB and cache flat |
