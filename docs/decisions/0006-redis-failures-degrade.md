# 6. Redis failures degrade service instead of failing requests

**Status:** accepted (Phase 4)

## Context

Redis serves cached reads, availability counters, the access-token revocation list, and rate limits. It is an extra dependency, and the spec requires reads to stay up when dependencies fail.

## Decision

Every Redis use has a fallback:

| Use | When Redis is down |
| --- | --- |
| Seat map, events, availability | Read Postgres directly |
| Rate limits | In-process token buckets at a quarter of the normal allowance per pod |
| Revoked access tokens | Treated as not revoked (fail open) |

## Consequences

- A Redis outage raises database load and latency but doesn't take the API down. Phase 9 measures it.
- A logged-out access token can keep working for at most its remaining 15-minute life while Redis is down. Failing closed instead would reject every authenticated request during the outage. Refresh-token revocation lives in Postgres and is unaffected.
- Per-pod fallback limits are approximate: N pods allow N quarter-size buckets.
