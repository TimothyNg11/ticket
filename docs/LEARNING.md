# Learning notes

One section per build phase: the concepts each phase introduces and why the code is shaped the way it is. Read alongside the code; file references point at the real thing.

## Phase 1: Foundation

### The database is the last line of defense

The headline promise of this project is "no seat is ever sold twice." Application code can enforce rules, but application code has bugs, runs in many copies at once, and can crash halfway through. So every rule that matters is also written into the schema, where Postgres enforces it for every writer, always.

Look at `event_seats_state_refs` in `internal/db/migrations/0002_inventory.up.sql`:

```sql
CHECK (
  (state = 'available' AND hold_id IS NULL     AND order_id IS NULL) OR
  (state = 'held'      AND hold_id IS NOT NULL AND order_id IS NULL) OR
  (state = 'sold'      AND hold_id IS NULL     AND order_id IS NOT NULL)
)
```

A seat that says "held" but has no hold is not just a bug the app might have; it is a row Postgres refuses to store. `UNIQUE (event_id, seat_id)` likewise means an event can never have two rows for the same physical seat. `TestEventSeatStateConstraint` proves both.

The same idea handles races. When two people register the same email at the same instant, both requests can pass a "does this email exist?" check. Instead of checking first, `account.Register` just inserts and lets the unique constraint pick the winner; the loser's unique-violation error (`SQLSTATE 23505`) becomes a clean `409 EMAIL_TAKEN`. `TestConcurrentRegisterSameEmail` fires 8 at once and expects exactly 1 success.

Emails are stored lower-cased (with a `CHECK (email = lower(email))` backstop), so `Alice@x.com` and `alice@x.com` are one account.

### Migrations, embedded

Every schema change is a numbered pair of files (`0001_*.up.sql` / `.down.sql`) applied by golang-migrate, which records the current version in a `schema_migrations` table. The files are compiled into the binary with `//go:embed` (`internal/db/migrate.go`), so the same container image that serves traffic can run `api migrate` as a one-off job before new pods start. There are no loose files to ship.

### sqlc instead of an ORM

Queries are plain SQL in `internal/db/queries/*.sql`. sqlc reads them along with the migrations and generates typed Go functions (`internal/db/sqlc`). You get the full power of SQL (row locks, `COPY`, keyset pagination) with compile-time checking of column names and types, and nothing is built from strings at runtime, which rules out SQL injection.

Two queries are worth a look:

- `CreateSeats :copyfrom` uses Postgres's `COPY` protocol, so a 20,000-seat venue is inserted in one round trip.
- `GetRefreshTokenForUpdate` uses `FOR UPDATE`, explained below.

### OpenAPI first

`api/openapi.yaml` is written before the handlers and is the contract. oapi-codegen generates request/response types and a "strict server" interface (`internal/httpapi/gen`), and `var _ gen.StrictServerInterface = (*Server)(nil)` in `server.go` makes the build fail if any endpoint is missing a handler. Handlers return either a typed response or an error; a single function, `writeError`, turns every error into the one error shape:

```json
{"error": {"code": "EMAIL_TAKEN", "message": "...", "request_id": "..."}}
```

Domain code returns `*apperr.Error` values that know their HTTP status and stable code. Anything else is an unexpected failure: it is logged with the request id and shown to the client only as a generic `500 INTERNAL`, so internals never leak.

### Password hashing must be slow

Passwords are hashed with Argon2id (`internal/auth/password.go`) using OWASP's minimum parameters: 19 MiB of memory and 2 passes per hash. That makes each guess expensive for an attacker who steals the database, especially on GPUs, which have little memory per core. Each hash gets a random salt, so identical passwords produce different hashes. The parameters are stored inside the hash string (`$argon2id$v=19$m=19456,t=2,p=1$salt$hash`), so they can be raised later without invalidating old hashes. Verification uses a constant-time compare.

Login also avoids revealing whether an account exists. An unknown email gets the same `INVALID_CREDENTIALS` error as a wrong password, and the code still runs a hash against a dummy value, so both cases take the same time.

### Access tokens and refresh tokens

- **Access token:** a JWT signed with HS256, valid for 15 minutes, sent on every request. The server can verify it without a database lookup. The verifier pins the algorithm to HS256, which blocks the classic `alg: none` attack (`TestVerifyRejectsAlgNone`).
- **Refresh token:** 32 random bytes, valid for 30 days, used only to get new tokens. The database stores only its SHA-256 hash, so a database leak doesn't hand out usable tokens. A fast hash is fine here, unlike for passwords, because the token is already 256 bits of randomness and can't be guessed.

**Rotation and reuse detection.** Every refresh revokes the presented token and issues a new one in the same *family*. Suppose an attacker steals a refresh token:

1. The attacker uses it first and gets a new pair. The victim's copy is now revoked.
2. The victim's app tries its (revoked) token. The server sees a revoked token being presented, which can only mean a copy exists, so it revokes the entire family. The attacker's new token dies too.

It works the same if the victim refreshes first. Either way the stolen token is useful for at most one round. `GetRefreshTokenForUpdate` locks the token row, so two simultaneous refreshes with the same token are serialized: the second one sees "revoked" and triggers the same protection. `TestRefreshRotatesAndDetectsReuse` walks through this.

The auth middleware (`internal/httpapi/authn.go`) treats a missing `Authorization` header as anonymous, but a present-and-invalid one as a hard `401`, so a forged or expired token is never quietly downgraded to "anonymous."

### Keyset pagination

`GET /v1/events` pages with an opaque cursor rather than `?page=3`. `OFFSET 10000` makes Postgres read and discard 10,000 rows, and pages shift when new events are inserted. Instead, the cursor encodes the sort key of the last item returned, `(starts_at, id)`, and the next page asks for rows *after* it:

```sql
WHERE (starts_at, id) > ($after_starts_at, $after_id) ORDER BY starts_at, id LIMIT n+1
```

The partial index `events (starts_at, id) WHERE status <> 'draft'` makes that a direct index seek. Fetching `n+1` rows tells us whether there's a next page without a separate count. The cursor is base64 so clients treat it as opaque.

### Tests against a real database

Mocking Postgres would test the mock. Instead, `internal/testutil` starts a real Postgres 16 container once per test package (Testcontainers), migrates a *template* database, and gives each test its own copy via `CREATE DATABASE t_N TEMPLATE ticket_template`, which takes a few milliseconds. Tests can't interfere with each other and can run in parallel. `go test -short` skips them when Docker isn't available.

### Containers

The root `Dockerfile` is a multi-stage build shared by every service (`--build-arg SERVICE=api|workers|payments_mock`). The first stage has the full Go toolchain and compiles a static binary. The second stage is `distroless/static:nonroot`, which contains the binary and nothing else: no shell, no package manager, and it runs as uid 65532. Fewer files means fewer vulnerabilities, and an attacker who gets code execution has no shell. Because there is no `curl` in the image for health checks, the binary checks itself (`api healthcheck`).

Compose runs `postgres`, then `migrate` (the same image with the `migrate` subcommand), then `api` only after migration succeeds (`service_completed_successfully`). Secrets come from a git-ignored `.env`. Compose refuses to start if they're missing (`${JWT_SECRET:?...}`).

### Liveness vs. readiness

`/healthz` answers "is the process alive?" and checks nothing else. `/readyz` answers "should I get traffic?" and pings Postgres. If the database blips, readiness fails and the load balancer stops sending traffic, but liveness stays green, so Kubernetes (Phase 7) doesn't pointlessly restart healthy pods.

## Phase 2: Holds and checkout

### Claiming seats: row locks and `SKIP LOCKED`

A hold runs one transaction (`internal/booking/holds.go`):

```sql
SELECT id FROM event_seats
WHERE event_id = $1 AND id = ANY($2) AND state = 'available'
ORDER BY id
FOR UPDATE SKIP LOCKED;
```

`FOR UPDATE` locks each returned row until the transaction ends, so no other transaction can claim the same seat in the meantime. That lock is what makes double-holding impossible. If fewer rows come back than were requested, some seat was taken: roll back and return `409 SEAT_UNAVAILABLE`. Otherwise insert the hold and flip the seats to `held` in the same transaction.

Why `SKIP LOCKED` instead of plain `FOR UPDATE`? With plain `FOR UPDATE`, if 500 people click the same seat, 499 transactions queue up behind the winner's lock, each holding a database connection while it waits, only to find the seat gone when the lock is released. `SKIP LOCKED` treats "someone is claiming this right now" as "unavailable" and returns immediately. Under a flash sale that turns a pile-up into fast, clean failures. `ORDER BY id` gives every transaction the same lock order, which rules out deadlocks between multi-seat holds. ADR 0004 compares the alternatives.

The "done when" test, `TestFlashSaleHolds500Parallel`, sends 500 simultaneous requests for the same 10 seats over HTTP and checks that exactly 10 succeed and each seat belongs to a different hold.

Other rules live in the schema too: `holds_one_active_per_user` (a partial unique index) stops one account from hoarding seats 8 at a time, and foreign keys from `event_seats.hold_id`/`order_id` mean a seat can't point at a hold or order that doesn't exist.

### Never hold a lock across the network

Checkout (`internal/booking/checkout.go`) is deliberately split into two transactions with the payment call between them:

1. **Transaction A:** lock the hold, check it's yours, active, and unexpired; create an order in `pending_payment`; commit.
2. **Charge** the provider, with no transaction open.
3. **Transaction B:** on success, mark the order `confirmed`, seats `sold`, the hold `converted`, and insert tickets.

If the charge happened inside a transaction, every slow payment would hold row locks and a pooled connection for seconds. A payment outage would then exhaust the connection pool and take down seat-map reads too. The partial unique index `orders_one_live_per_hold` (one non-failed order per hold) is what stops a double-clicked checkout from creating two orders while no lock is held. `TestDoubleCheckoutParallel` fires 20 parallel checkouts at one hold and expects exactly one confirmed order and one charge.

### When you don't know whether the charge happened

A payment call can end three ways:

- **Succeeded:** sell the seats.
- **Declined:** mark the order `failed` and keep the hold, so the user can retry.
- **Unknown:** a timeout or provider error. The charge may have gone through. This is the case that loses real money, so it is never treated as a decline.

For an unknown outcome the order stays `pending_payment` and the API returns `202 Accepted`. The charge's idempotency key is derived from the order id (`charge-<order id>`), so asking again, from a retry or from the Phase 6 reconciler, can only ever produce one charge. `ApplyChargeResult` only acts on orders that are still pending, so calling it twice is harmless.

The mock provider (`internal/payments/mock`) reproduces this deliberately: when it injects an error, half the time it records the charge first, so the money moved but the response failed.

### Idempotency keys

Networks drop responses. A client whose checkout timed out doesn't know whether it bought the seats, so it must be able to retry safely. Every authenticated write sends an `Idempotency-Key` header, and `internal/idempotency` handles it:

1. `INSERT` a row keyed by `(user_id, key)` with a hash of the request. The insert acts as the lock: only one request can create the row, so only one runs.
2. Run the handler, recording its response. Store the status and body, or delete the row on a 5xx so a retry can run.
3. A later request with the same key gets the stored response replayed (header `Idempotent-Replay: true`). The same key with a different body returns `422`. The same key while the first request is still running returns `409 REQUEST_IN_PROGRESS`.

If the server crashes after committing an order but before saving the response, the claim row is left without a response. After 60 seconds a retry may take it over. The orders table's own `UNIQUE (user_id, idempotency_key)` then returns the existing order instead of creating a second one: two layers, each covering the other's gap.

### The sweeper

Holds expire after 10 minutes. `workers sweeper` runs `ExpireHolds` every 15 seconds:

```sql
SELECT h.id FROM holds h
WHERE h.status = 'active' AND h.expires_at < now()
  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.hold_id = h.id AND o.status = 'pending_payment')
LIMIT 500 FOR UPDATE SKIP LOCKED;
```

- `NOT EXISTS (… pending_payment)` stops the sweeper from releasing seats while a payment for them is in flight, because the buyer may already have been charged (`TestExpireHoldsSkipsPendingPayment`).
- `SKIP LOCKED` lets several sweeper replicas run: each grabs a different batch, and none waits on another (`TestConcurrentSweepersDontDoubleProcess`).
- Expiry lives in the database (`expires_at`), not in a Redis TTL, so it survives restarts and has one source of truth.

### Cancellation and resale

Cancelling voids the tickets (kept for history), returns the seats to `available`, and refunds the charge. Because uniqueness on tickets is a *partial* index (`WHERE status = 'valid'`), the same seat can later get a new valid ticket for a new buyer, but never two valid ones at once. The refund call happens after the cancellation commits; if it fails, the order stays `cancelled` and `RetryRefund` finishes it later.

### Tickets you can't forge

A ticket's QR payload is `base64(ticket id | seat id) . HMAC-SHA256(payload)` (`internal/auth/ticket.go`). Anyone can read it, but nobody without the signing key can make one that verifies. The key (`TICKET_SIGNING_KEY`) is separate from the JWT secret so either can be rotated alone.

### Invariants

`booking.CheckInvariants` asks the database directly whether the promises hold: no seat with two valid tickets, no sold seat without a confirmed order, every confirmed order paid exactly once with tickets matching seats, and no held seat without an active hold. `TestFlashSaleEndToEnd` races 200 buyers through hold, checkout, cancel, and resale, then asserts every count is zero. Phase 8 runs the same checks on a schedule and alerts.

## Phase 4: Caching and rate limiting

### Cache-aside, and why Redis never decides who owns a seat

During an on-sale almost all traffic is reads: thousands of people refreshing the seat map. `internal/cache` puts Redis in front of those reads using **cache-aside**: look in Redis; on a miss, read Postgres and store the result. Redis only ever holds *copies*. Holds still go straight to Postgres row locks, so a stale cache entry can make a seat *look* free for a moment, but it can never let two people get it.

If Redis is unreachable, every cache method logs and falls through to Postgres (`TestRedisDownFallsBackToPostgres`). Losing the cache makes reads slower; it doesn't make them fail.

### Versioned keys instead of deleting entries

The seat map key carries a version number: `seatmap:{event}:v{n}`. Every hold, release, sale, or cancellation calls `SeatsChanged`, which runs `INCR seatmap:{event}:ver`. Readers fetch the current version first, so after a change they look up a key that doesn't exist yet and rebuild it. Old versions simply expire.

Compared with deleting the key, versioning avoids a race. If a reader builds the map from slightly old data *after* a delete, it would put stale data back under the live key. With versions, stale data lands under an old version number nobody reads anymore. The hook runs *after* the database commit, so readers never see a version bump before the change is visible.

### Stampedes

A hot key expiring means hundreds of requests miss at the same instant and all hit Postgres: a cache stampede. Two layers stop that:

- **singleflight** (in process): concurrent misses for the same key share one rebuild (`TestStampedeRebuildsOnce`: 50 concurrent cold reads, 1 query).
- **A short Redis lock** (across pods): `SET lock:<key> NX PX 2000`. The pod that gets it rebuilds; other pods serve the previous seat map from a longer-lived `:stale` copy instead of querying (`TestOtherPodRebuildingServesStale`).

TTLs get up to 20% random jitter, so keys filled at the same moment don't all expire at the same moment.

### Counters that can drift, and a job that fixes them

`GET /v1/events/{id}` includes `available_seats`, served from a Redis counter that each change adjusts (`INCRBY` with the delta the booking code reports). Counters updated "after commit" can drift: a crash or a Redis blip between the commit and the `INCRBY` loses an update. Rather than pretending that can't happen, the `workers availability` job recounts every on-sale event from Postgres each minute and overwrites the counters (`TestRecomputeFixesDrift`).

### Logging out a stateless token

JWT access tokens are verified without a database lookup, which also means they can't be "deleted". Logout therefore writes the token's id (`jti`) to `revoked:{jti}` with a TTL equal to the token's remaining life, and the auth middleware rejects revoked ids. The set stays small because entries vanish when the token would have expired anyway. If Redis is down the check passes (fails open): tokens live 15 minutes, and refusing every request during a cache outage would turn a degraded dependency into a full outage. ADR 0006 records that tradeoff.

### Token-bucket rate limiting in a Lua script

Each limit is a bucket holding up to `burst` tokens that refills at `rate` per second; every request spends one (`internal/ratelimit/bucket.lua`). Two details matter:

- **Atomicity.** Reading the tokens, refilling, spending, and writing back all happen inside one Lua script, which Redis runs without interleaving anything else. Doing the same from Go (GET, compute, SET) lets two concurrent requests both read "1 token left" and both pass. `TestAtomicUnderConcurrency` sends 100 simultaneous requests against a burst of 10 and gets exactly 10 through.
- **One clock.** The script uses Redis's `TIME`, not the caller's clock, so pods with skewed clocks agree.

Buckets: reads 20/s (burst 40) per user or IP; logins 10/min and registrations 5/min per IP (slows credential stuffing); holds 5/min per user (each hold locks inventory); other writes 60/min per user. Going over returns `429 RATE_LIMITED` with `Retry-After`. If Redis is down, an in-process limiter takes over at a quarter of the allowance per pod, so protection degrades rather than disappearing (`TestFallsBackWhenRedisDown`).

Per-IP limits need the real client IP. Behind the ingress, `X-Forwarded-For` lists every hop, and anything left of the last entry was supplied by the client and can be forged. With `TRUST_PROXY=true` the API uses the right-most entry, which the ingress itself appended (`TestClientIP`).
