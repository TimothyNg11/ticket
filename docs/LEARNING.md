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

`services/api/Dockerfile` is a multi-stage build. The first stage has the full Go toolchain and compiles a static binary. The second stage is `distroless/static:nonroot`, which contains the binary and nothing else: no shell, no package manager, and it runs as uid 65532. Fewer files means fewer vulnerabilities, and an attacker who gets code execution has no shell. Because there is no `curl` in the image for health checks, the binary checks itself (`api healthcheck`).

Compose runs `postgres`, then `migrate` (the same image with the `migrate` subcommand), then `api` only after migration succeeds (`service_completed_successfully`). Secrets come from a git-ignored `.env`. Compose refuses to start if they're missing (`${JWT_SECRET:?...}`).

### Liveness vs. readiness

`/healthz` answers "is the process alive?" and checks nothing else. `/readyz` answers "should I get traffic?" and pings Postgres. If the database blips, readiness fails and the load balancer stops sending traffic, but liveness stays green, so Kubernetes (Phase 7) doesn't pointlessly restart healthy pods.
