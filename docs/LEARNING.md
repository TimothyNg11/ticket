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

## Phase 5: Waiting room

### Why a queue at all

Row locks keep a flash sale *correct*, but they don't keep it *fast*. If 50,000 people hit "hold" at once, Postgres must run 50,000 lock-acquiring transactions, the connection pool saturates, and everyone, winners included, waits. A waiting room turns that spike into a steady trickle: buyers line up, and an admitter lets in a fixed batch (default 500) every interval (default 10 s). The database only ever sees about one batch of buyers at a time, no matter how big the crowd.

### The queue is a sorted set

`internal/waitingroom` keeps one Redis sorted set per event: the member is the user id, and the score is the arrival time in milliseconds.

- **Joining** runs a small Lua script: read the Redis server clock (`TIME`), `ZADD NX` the user with that score, and return `ZRANK`. `NX` means rejoining never moves you, so refreshing the page can't cost you your place. Using the server's clock means every API pod stamps arrivals on the same timeline: first come, first served really is first.
- **Your position** is `ZRANK + 1`, an O(log n) lookup. The estimated wait is `ceil(position / batch) × interval`.
- **Admitting** is `ZPOPMIN key 500`: atomically remove the 500 lowest scores (the earliest arrivals) and issue each an admission pass.

### One batch per interval, however many admitters run

The admitter (`workers admitter`) ticks every second, but before admitting it must win `SET admit-gate:{event} 1 NX PX <interval>`. Only one replica can set that key, and nobody can set it again until it expires one interval later. That one key is the cross-replica schedule; no leader election is needed (`TestConcurrentAdmittersAdmitOneBatch`: 5 simultaneous admitters, exactly one batch). If Redis is down, admission simply pauses. Nobody new gets in, which is the safe failure mode for a queue.

### Passes that can't be forged, shared, or confused

Joining returns a **queue token**; once admitted, polling returns an **admission token**. Both are signed JWTs (`internal/auth/typed.go`) that bind a user id *and* an event id, and expire (admission after 15 minutes).

- *Can't be forged:* they're HMAC-signed.
- *Can't be shared:* `CheckAdmission` requires the pass's user to equal the authenticated caller, so posting your admission token in a group chat helps nobody.
- *Can't be confused:* every token the API signs carries a `typ` claim (`access`, `queue`, `admission`), and every verifier checks it. All three share a signing key, so without `typ` an admission pass (which has a valid user id as its subject) could be presented as an access token. `TestPassTypeIsEnforced` checks every pairing.

### The gate

For events with `queue_enabled`, `POST /v1/events/{id}/holds` requires an `Admission-Token` header and returns `403 ADMISSION_REQUIRED` otherwise. The gate sits in front of the database: refused requests never open a transaction. Holds still go through the normal row locks once inside. The queue controls *how many* people compete, and Postgres still decides *who wins*.

`TestQueueBurst10000` is the Phase 5 "done when": 10,000 users join simultaneously, the admitter admits one batch of 500, then all 10,000 try to hold a seat. Exactly the 500 admitted users succeed, the other 9,500 get `403`, and the database shows 500 holds, all by admitted users.

## Phase 6: Reliability

Every failure mode in the spec's reliability table now has a mechanism and a test. This phase is about the payment path, where mistakes cost money, and about moving events out of the database without losing them.

### Retries are only safe with idempotency keys

`payments.Resilient` retries calls whose outcome is unknown (timeouts, 5xx), up to three attempts. Retrying a *charge* would be reckless if the first attempt might have succeeded. That's exactly what the mock simulates: it sometimes records the charge and then fails the response. It's safe here because every charge carries `charge-<order id>` as its idempotency key, so the provider returns the original charge instead of making a new one (`TestRetryAgainstFlakyProviderChargesOnce`). Definite answers (declined, not found) are never retried, and an exhausted retry stays "unknown", never "declined" (`TestGivesUpAsUnknown`).

Backoff is exponential with **full jitter**: wait a random time between 0 and `base × 2^attempt`. Without jitter, every client that failed at the same moment retries at the same moment, in waves that can knock a recovering provider over again.

### The circuit breaker

When the provider is down, every checkout would otherwise wait out a 3-second timeout three times, holding a goroutine and a connection the whole while. With thousands of buyers that exhausts the API. The breaker (`payments.Breaker`) counts consecutive failures:

- **Closed**: calls go through. After 5 consecutive provider failures it opens.
- **Open**: calls fail immediately with `ErrCircuitOpen`, and checkout returns `503 PAYMENT_UNAVAILABLE` before creating an order. The buyer's hold survives so they can retry (`TestCircuitOpenFailsFastWithoutOrders`).
- **Half-open**: after a 10 s cooldown, exactly one probe call is allowed. Success closes the circuit; failure reopens it (`TestBreakerOpensFailsFastAndRecovers`, `TestFailedProbeReopens`).

Only provider trouble counts as failure. A decline is a healthy provider saying no.

### The reconciler: making "unknown" impossible to leave behind

Phase 2 left orders in `pending_payment` when the outcome was unknown. `workers reconciler` (`booking.Reconcile`) settles them. Every 15 s it takes pending orders untouched for 30 s and **repeats the charge with the same idempotency key**. If the original reached the provider, the provider returns it; if not, this attempt is the first. Either way exactly one charge exists afterwards, and its definite answer confirms or fails the order. The same pass retries refunds for orders that were cancelled while the provider was down.

`TestPaymentChaosNeverLosesOrDoubleCharges` is the Phase 6 "done when". Sixty buyers check out against a provider that fails 30% of calls (half of them *after* charging), declines 10%, and is slower than the timeout 20% of the time. Then the reconciler runs until nothing is pending, and the test compares the provider's ledger with the orders table:

- every confirmed order has a succeeded charge;
- no failed order has a succeeded charge (no lost money);
- every succeeded charge belongs to a confirmed order (no orphaned charges);
- nothing is left pending, and every database invariant holds.

### The transactional outbox

When an order is confirmed, someone should be notified. The naive version (commit, then publish to a queue) has two failure windows: crash after the commit but before publishing, and the notification is lost; publish but then the commit fails, and you announce an order that doesn't exist.

The outbox closes both: `booking.emit` inserts the event into the `outbox` table *inside the same transaction* as the order change, so the event exists if and only if the change committed. Separately, `workers outbox-relay` publishes unpublished rows to a Redis Stream and marks them published (`SKIP LOCKED` again lets several relays run; `TestConcurrentRelaysDontDoublePublish`).

The remaining gap is a crash between publishing and marking, which publishes the batch twice. So delivery is **at-least-once**, and consumers must be idempotent. The notifier wraps its handler in `outbox.Once`, which records each event id with `SET NX` before handling (`TestCrashAfterPublishDuplicatesButHandledOnce`: each event delivered twice, handled once).

### Consumer groups

The notifier reads the stream as a Redis Streams *consumer group*: each event goes to one member, and stays "pending" until that member acknowledges it (`XACK`) after handling succeeds. If a notifier crashes mid-event, the event isn't lost: after a minute idle, another member takes it over with `XAUTOCLAIM` (`TestFailedHandlerIsRetriedAndCrashedConsumerReclaimed`).

### Graceful shutdown

Kubernetes stops a pod by sending SIGTERM, removing it from the Service's endpoints *at roughly the same time*, and sending SIGKILL after the grace period. If the API stopped listening at SIGTERM, requests routed in the next second or two (before endpoint removal propagates) would fail. So on SIGTERM the API:

1. flips `/readyz` to 503 while staying live and still serving (`TestDrainingFailsReadinessOnly`);
2. waits `DRAIN_DELAY` (5 s) for the load balancer to stop routing to it;
3. calls `http.Server.Shutdown`, which stops accepting connections and waits for in-flight requests;
4. closes the database and Redis pools.

Workers stop at their next tick, and any transaction cut short by cancellation simply rolls back. Every job is idempotent, so the next run picks up where it left off.

## Phase 7: Kubernetes

### The shape of the deployment

`deploy/helm/ticket` packages everything; `deploy/kind/up.sh` builds a three-node local cluster and installs it. Each component maps to a Kubernetes object chosen for what it needs:

| Component | Object | Why |
| --- | --- | --- |
| API | Deployment + Service + HPA + PodDisruptionBudget | Stateless and interchangeable, so scale it horizontally and let any pod die |
| Each worker job | Its own Deployment | Scale and restart jobs independently; every job is safe as multiple replicas |
| Postgres, Redis (local) | StatefulSet + PersistentVolumeClaim | Stable identity and disk that survives restarts; managed services in the cloud |
| PgBouncer | Deployment | Stateless proxy; two replicas for availability |
| Ingress | Traefik + `Ingress` + cert-manager | TLS termination and routing; only `/v1` is exposed |

### Probes and rollouts

- **Liveness** (`/healthz`) only answers "is the process alive?". If it checked the database, a database blip would make Kubernetes restart every healthy API pod at once and turn a small incident into a full outage.
- **Readiness** (`/readyz`) answers "should this pod get traffic?" It checks Postgres and fails during shutdown (Phase 6), so a pod leaves the Service before it stops.
- **Rolling updates** use `maxUnavailable: 0`: a new pod must be ready before an old one goes, so capacity never dips during a deploy.
- **PodDisruptionBudget** `minAvailable: 2`: node drains and cluster upgrades can't take the API below two pods.
- **HPA** scales on CPU from 3 up to 20 pods, scaling up immediately and down slowly (5-minute window), because on-sales spike in seconds.

### Migrations before traffic

Each API pod runs `api migrate` as an **init container** before the server starts, so a pod can never serve with an older schema than its code expects. Several pods starting at once is safe: golang-migrate takes a Postgres advisory lock, so one migrates and the others find nothing to do. This only works if every migration is backward compatible with the previous release (expand, then contract in a later release), because old pods keep serving while new ones roll out. ADR 0008 records this.

### PgBouncer and transaction pooling

Each Postgres connection is a whole OS process with megabytes of memory; a few hundred is the practical ceiling. Twenty API pods with a 10-connection pool each, plus workers, would already be there. PgBouncer sits in between in **transaction pooling** mode: a real server connection is lent to a client only for the length of one transaction, so 2,000 client connections share 40 server connections.

Transaction pooling has a catch: anything that lives in a *session* breaks. Two things here depend on that:

- pgx uses protocol-level prepared statements. PgBouncer 1.21+ tracks those per server connection (`max_prepared_statements`), so they work.
- golang-migrate's advisory lock is session-level. That's why migrations connect straight to Postgres, not through PgBouncer.

### Security in the manifests

- Every Go container runs as a non-root user with a read-only root filesystem, no Linux capabilities, no privilege escalation, and the RuntimeDefault seccomp profile.
- No component talks to the Kubernetes API, so service account tokens aren't even mounted.
- Secrets come from a Kubernetes Secret created outside the chart. `DATABASE_URL` is assembled at runtime with `$(POSTGRES_PASSWORD)` expansion, so the password never appears in a ConfigMap or the rendered manifest. PgBouncer's auth file is likewise written from the Secret by an init container into memory.
- **NetworkPolicies** start from default-deny and then allow only specific paths: the ingress controller → API; API → Postgres (migrations only) and PgBouncer; workers → PgBouncer and Redis; API and reconciler → payments. These are verified, not just applied: a probe pod showed unlabeled pods blocked from Postgres, Redis and payments, and workers allowed to PgBouncer and Redis but not Postgres.

### A bug only the cluster could find

The first pod-kill test failed with 503s, and the cause wasn't the pod kills at all: API pods were **OOMKilled**. Argon2id deliberately uses 19 MiB per password hash. Registering hundreds of buyers meant a dozen hashes running at once in one pod, 228 MiB on top of the baseline, past the 256 MiB limit. Unit tests never saw it because they don't run under a memory limit. The fixes:

- `auth.SetHashConcurrency`: at most 4 hashes run at once per pod, and the rest queue for milliseconds (`TestHashingConcurrencyIsBounded`).
- `GOMEMLIMIT` is set from the container's memory limit (via the downward API), so the Go garbage collector works harder before the kernel kills the process.
- The readiness probe timeout went from 1 s to 3 s, so a CPU-throttled pod doesn't flap out of the load balancer.

### The test

`deploy/kind/kill-api-during-sale.sh` runs `tools/flashsale` through the ingress for 60 seconds (300 buyers, 150 seats) and deletes a random API pod every 15 seconds. The flashsale client retries transport errors and 502/503/504 with the *same* Idempotency-Key, as a well-behaved client would. Result: 3 pods deleted, 161 orders confirmed, 11 cancelled, **0 failed operations, 0 retries needed** (graceful draining moved traffic before each pod stopped), and 0 invariant violations. CI runs the same test on every pull request.

## Phase 8: Observability

The goal of this phase is concrete: during a load test, any latency spike should be explainable from the dashboard. That needs three signals, each answering a different question.

### Metrics: what is happening, in aggregate

`internal/metrics` defines every Prometheus metric in one file, so the dashboard and alerts have one list to match against. A few rules keep them useful:

- **RED per route:** request **R**ate, **E**rrors (by status), and **D**uration histograms for every endpoint (`ticket_http_requests_total`, `ticket_http_request_duration_seconds`).
- **Labels must have few values.** The route label is the chi *pattern* (`/v1/events/{id}/holds`), read after routing. Labelling with the raw path would create a new time series for every event and hold id, and eventually take Prometheus down. `TestMetricsUseRoutePatternsAndRequestIDHeader` checks for exactly that.
- **Histograms, not averages.** An average hides the slowest 1% of requests, which is exactly the experience of the people who complain. Percentiles are computed in Prometheus from the bucket counts (`histogram_quantile`), so they can be combined across pods correctly; per-pod percentiles can't be averaged.
- **Saturation as well as latency:** DB pool connections in use versus max, and how often a request had to wait for one (`ticket_db_pool_empty_acquires_total`). A pool pinned at its maximum explains latency that no single query does.
- **The domain, not just HTTP:** holds by outcome, seats sold, orders by status, payment calls by outcome, retries, circuit state, queue length, admissions, outbox lag, notifications, reconciliations, and the invariant counts.

The API serves `/metrics` on its normal port (the ingress only exposes `/v1`); every worker serves one on `:9090`.

### Logs: what happened to *this* request

Every log line is JSON, and the logger (`observability.NewLogger`) wraps the handler so any line written while serving a request automatically carries that request's `request_id` and `trace_id`. Nothing has to pass ids around by hand. Each request also gets one access-log line (route, status, duration, user), and the response carries `X-Request-Id`, so a user can quote it in a bug report and you can find every line for that request.

### Traces: where the time went

OpenTelemetry instruments the HTTP server (one span per request, renamed to the route pattern), every Postgres query (`otelpgx`), Redis (`redisotel`), and the outgoing payment call (`otelhttp`), and exports spans over OTLP to Jaeger. Trace context propagates on the payment call, so a real provider that also traces would continue the same trace. Database spans are named after the sqlc query (`db GetHoldForUpdate`), so a trace reads like the code.

### The drill (done-when)

`deploy/observability/latency-spike-drill.sh` runs a flash sale against the Compose stack, raises the payment provider's latency to 800 ms after 30 seconds, then queries Prometheus and Jaeger:

| 15 s steps → | t15 | t30 | **t45** | t60 | t75+ |
| --- | --- | --- | --- | --- | --- |
| checkout p99 (s) | 0.17 | 0.19 | **0.99** | 1.00 | 1.00 |
| payment charge p99 (s) | 0.10 | 0.10 | **0.99** | 1.00 | 1.00 |
| hold p99 (s) | 0.08 | 0.09 | 0.05 | 0.02 | 0.02 |
| DB connections in use | 3 | 4 | 2 | 1 | ≤1 |

Checkout latency jumps in the same 15-second window as the provider's latency, while holds, seat maps, and the database stay flat, so the cause isn't Postgres or the cache. The slowest checkout trace then pins it down: 801 ms of an 824 ms request is the outbound `HTTP POST` to the provider. (The p99 tops out at 1.00 because that's the edge of the histogram bucket.)

Running the drill taught two lessons of its own. The first version sold out of seats *before* the latency was injected, so the "spike" never happened and the graphs looked broken: a load test has to be shaped so the thing you're measuring actually occurs. And `curl` treats `{id}` in a URL as a glob pattern, which silently mangled every PromQL query that named a route until `curl -g` turned globbing off.

### Alerts

`deploy/helm/ticket/files/alerts.yaml` holds the rules: p99 over 300 ms for 5 minutes, 5xx rate over 1%, outbox lag over a minute, any invariant violation (fires immediately, critical), a pod restarting more than 3 times in 15 minutes, and an open payment circuit. Rules are code, so they have tests: `deploy/observability/alerts_test.yaml` feeds synthetic series to `promtool test rules` and asserts which alerts fire and with what text. CI runs it, which catches a typo in an alert expression before an incident does.

### The invariant checker

`workers invariants` runs `booking.CheckInvariants` every 30 seconds and exports each count as `ticket_invariant_violations{check=...}`. The alert on it has no `for:` delay: a double-sold seat should page at once.

### Running it

- Compose: Prometheus on `:9090`, Grafana on `:3000` (opens on the "Ticket: On-sale" dashboard), Jaeger on `:16686`.
- Kubernetes: `deploy/kind/up.sh` installs kube-prometheus-stack. The chart adds a ServiceMonitor (API), a PodMonitor (workers), a PrometheusRule (the same alerts file), and the dashboard as a ConfigMap that Grafana's sidecar loads. NetworkPolicies let the `monitoring` namespace scrape the API and workers and nothing else. The dashboard is generated by `deploy/observability/gen_dashboard.py`; CI fails if the committed JSON is stale.

## Phase 9: Load and chaos testing

This phase is a loop: run the on-sale, find what's slow *with evidence*, fix one thing, run it again. `BENCHMARKS.md` has every run, including the failures; this section covers the techniques.

### Simulating 50,000 people

`loadtest/onsale` models each buyer as a goroutine: join the queue, poll, buy if admitted, leave if sold out. Goroutines cost a few KB, so 50,000 concurrent buyers fit in one process. k6, which runs a JavaScript VM per virtual user, couldn't hold that many on one machine, so k6 is used for what it's best at: a constant-arrival-rate test of one endpoint (`loadtest/k6/seatmap.js`). Constant arrival rate matters: if the server slows down, k6 starts more virtual users to keep sending the same number of requests per second, instead of quietly sending fewer and making the server look healthier than it is.

Buyers are seeded straight into Postgres with tokens minted from the deployment's secret (`loadtest/seed`), because 50,000 registrations would measure Argon2id, which is slow on purpose, rather than the sale.

### Measure the server, not the client

Two of the biggest "bottlenecks" turned out to be in the test itself:

- The client was decompressing every polled seat map, about 750 KB each, even when nothing read it. Now bodies stay compressed unless a buyer actually parses them.
- From the Windows host, the server measured seat-map p50 at 5 ms while the client saw 2.2 s: every request crossed Docker Desktop's userspace port proxy. The generator now runs *inside* the cluster as a Job (`loadtest/job.sh`), entering through Traefik like real traffic.

The habit that caught both: always compare client-side latency with the server's own histograms (`loadtest/server-report.sh` pulls those for the exact run window). If they disagree, the gap is where the problem lives.

### Profiling instead of guessing

The API has Go's profiler on an internal-only port. `loadtest/profile-under-load.sh` takes a 30 s CPU profile mid-sale. The first profile showed the seat-map handler at **67% of CPU**, mostly JSON: decoding the cached map, then re-encoding it. The fix stores the *finished, gzip-compressed response* in Redis and writes the bytes straight out; seat-map p50 dropped from 496 ms to 67 ms.

### Failures that only appear under load

- **Readiness cascades.** `/readyz` pinged Postgres through the request pool. Under load the pool saturated, every pod's probe timed out *at the same moment*, and Kubernetes removed all six from the Service. A readiness check on a shared dependency turns "slow" into "down." Readiness now uses its own connection and only fails after 10 s of real unreachability.
- **singleflight and contexts.** Many requests waiting on one shared rebuild all inherited the *first* caller's context, so when that client disconnected every waiter failed with `context canceled`, 22,000 times. Work shared by many requests must run on a context that belongs to none of them (`context.WithoutCancel` plus its own timeout).
- **Correlated subqueries.** The invariant checker counted payments with one subquery per order: fine at 100 orders, 22 s at 21,000. Rewritten as set-based joins with an index, it takes 0.24 s.
- **Pools that wait while backends idle.** With both connection pools saturated and Postgres at 1.3 cores, the database wasn't busy, it was *waiting*. `pg_stat_activity` showed sessions on `LWLock:WALWrite`: every commit waits for the write-ahead log to reach disk. Two fixes that don't weaken order durability: idempotency bookkeeping commits asynchronously (losing one in a crash only means a retry re-runs, still deduplicated by the orders table), and `commit_delay` lets concurrent commits share one flush (group commit).

### Backpressure: telling clients when to come back

No amount of server tuning makes 50,000 people polling every 10 seconds fit on one machine: that's about 10,000 req/s of mostly pointless refreshes. Real queue pages poll on the server's schedule. The waiting room now returns `poll_after_seconds`, half the buyer's estimated wait, between 5 s and 2 minutes. Someone at position 40,000 checks every 2 minutes; someone about to be admitted checks every 5 seconds. That one change made the first run in which every seat sold with zero failed requests. The same idea applies to 503s: during a payment outage the API sends `Retry-After` matching the circuit breaker's cooldown, so clients wait it out instead of giving up or hammering.

### Chaos testing

`loadtest/chaos.sh` runs a sale while killing, in turn, an API pod, the payment service, Redis, the reconciler and the outbox relay, and making payments fail 40% of the time. Passing requires more than "the API stayed up": afterwards no order may be stuck in `pending_payment`, the outbox must be fully published, every refund must have gone through, and the invariants must hold. It took three attempts; the first two found the slow invariant query and the missing `Retry-After`.

## Phase 10: Delivery (GitOps)

### Deploys as commits

With GitOps, the cluster's desired state lives in git and a controller inside the cluster (Argo CD) continuously makes reality match it. Nobody runs `kubectl apply` or `helm upgrade` by hand. The pipeline:

1. A pull request merges to `main`.
2. CI builds and scans the images and pushes them to GHCR, tagged with the merge commit's SHA. It also runs every test, including the kind end-to-end sale with pod kills.
3. Only if *all* of that passes, the `promote` job commits the new SHA into `apps/ticket.yaml` on the `gitops` branch.
4. Argo CD polls git every 60 s, sees the change, and runs the chart's rolling update (`maxUnavailable: 0`), so the API never drops below capacity.

So every deploy is a commit with an author, a timestamp, and the exact build. **Rolling back means reverting that commit**: Argo CD puts the previous SHA back within a minute. `selfHeal` also reverts manual changes made directly in the cluster, so what's running can't quietly drift from what's in git.

### Why a separate `gitops` branch

`main` requires a reviewed pull request with passing checks, so a CI bot can't push to it. Keeping deployment state on its own branch keeps code history (`main`) separate from deploy history (`gitops`). Argo CD can't read one repository at two revisions within a single Application, so the setup uses the standard *app of apps*: a root Application watches `apps/` on `gitops`, and `apps/ticket.yaml` is itself an Application that deploys the chart from `main` with the pinned image tag.

### Rollbacks and migrations

The first GitOps sync rolled the cluster *back* to an older build than the one running, and every API pod failed to start: the newer build had migrated the schema to version 6, and the older migrator refused a database "ahead" of it. That would have broken every real rollback across a migration. The fix makes the migrator accept a newer schema (`TestMigrateToleratesNewerSchema`). That is only safe because of a rule the project already follows: migrations are backward compatible, so older code runs on the newer schema (add columns and tables now; drop old ones only in a later release, after no running code uses them).
