# Phase 2: Holds and Checkout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. Every task is RED → GREEN: write the listed tests, watch them fail, implement, watch them pass, commit.

**Goal:** Users hold up to 8 seats for 10 minutes, check out through a mock payment service, get HMAC-signed tickets, and can cancel for a refund. Holds expire automatically. Retried requests are idempotent. Done when 500 parallel hold requests for the same 10 seats produce exactly 10 winners.

**Architecture:** New domain package `internal/booking` (holds, checkout, orders, cancel, sweeper) on top of the Phase 1 stack. Seat ownership is decided only by Postgres row locks (`FOR UPDATE SKIP LOCKED`) plus constraints. Payments go through `internal/payments` (an interface, an HTTP client, and the mock provider's handler), and `services/payments_mock` serves that handler. A generic idempotency middleware (`internal/idempotency`) stores responses keyed by `(user_id, Idempotency-Key)`. A new `services/workers` binary runs the hold sweeper.

**Spec:** `docs/superpowers/specs/2026-10-01-ticketing-system-design.md` (Core flows 2–4, Data model, Idempotency, Phase 2).

**Plan style (ruling):** From Phase 2 on, plans fix the design, interfaces, SQL, and the exact tests; code is written test-first during execution rather than pre-written here, because the executor is the same agent (Native) and the code would otherwise be written twice.

## Global Constraints

- Everything in Phase 1's Global Constraints still holds.
- Seat state moves only `available → held → sold`, `held → available` (release/expiry), `sold → available` (cancellation).
- At most 8 seats per hold; hold TTL 10 minutes (`HOLD_TTL`); sweeper every 15 s (`SWEEP_INTERVAL`).
- Payment calls: 3 s timeout. Retries and the circuit breaker are Phase 6.
- Every authenticated, non-admin `POST`/`DELETE` under `/v1` requires an `Idempotency-Key` header (1–64 chars); stored 24 h.
- QR payload is HMAC-SHA256 signed with `TICKET_SIGNING_KEY` (≥ 32 bytes, separate from `JWT_SECRET`).

## Review Focus

1. **Two users race for the same seat:** exactly one hold wins; the other gets 409 `SEAT_UNAVAILABLE` naming the seat → concurrency test (Task 3, Task 7).
2. **Double-clicked checkout** (same hold, many parallel checkouts with different keys): exactly one order confirmed, one charge → Task 5.
3. **Retried request with the same Idempotency-Key** (sequential and concurrent): same response replayed, no second order; same key with a different body → 422 → Task 2, Task 5.
4. **Hold expires while payment is in flight:** the sweeper must not release seats that have a `pending_payment` order → Task 4.
5. **Cancel then resell:** a cancelled seat can be held and sold again and gets a new valid ticket; the old ticket is void → Task 6.

---

## Schema (migration `0003_booking`)

```sql
CREATE TABLE holds (
  id uuid PK DEFAULT gen_random_uuid(), event_id uuid NOT NULL REFERENCES events, user_id uuid NOT NULL REFERENCES users,
  expires_at timestamptz NOT NULL, status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','converted','expired','released')),
  created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX holds_sweep_idx ON holds (status, expires_at);
-- One active hold per user per event: stops one account hoarding seats 8 at a time.
CREATE UNIQUE INDEX holds_one_active_per_user ON holds (event_id, user_id) WHERE status = 'active';

CREATE TABLE orders (
  id uuid PK, user_id uuid NOT NULL REFERENCES users, event_id uuid NOT NULL REFERENCES events, hold_id uuid NOT NULL REFERENCES holds,
  total_cents int NOT NULL CHECK (total_cents > 0),
  status text NOT NULL CHECK (status IN ('pending_payment','confirmed','failed','cancelled','refunded')),
  idempotency_key text NOT NULL, created_at, updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, idempotency_key));
CREATE UNIQUE INDEX orders_one_live_per_hold ON orders (hold_id) WHERE status <> 'failed';
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);

CREATE TABLE payments (
  id uuid PK, order_id uuid NOT NULL REFERENCES orders, kind text NOT NULL CHECK (kind IN ('charge','refund')),
  provider_ref text, amount_cents int NOT NULL, status text NOT NULL CHECK (status IN ('succeeded','declined')),
  idempotency_key text NOT NULL UNIQUE, created_at);

CREATE TABLE tickets (
  id uuid PK, order_id uuid NOT NULL REFERENCES orders, event_seat_id uuid NOT NULL REFERENCES event_seats,
  status text NOT NULL DEFAULT 'valid' CHECK (status IN ('valid','void')), qr_token text NOT NULL, created_at);
CREATE UNIQUE INDEX tickets_one_valid_per_seat ON tickets (event_seat_id) WHERE status = 'valid';

CREATE TABLE idempotency_keys (
  user_id uuid NOT NULL REFERENCES users, key text NOT NULL, request_hash text NOT NULL,
  response_status int, response_body bytea, created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
  PRIMARY KEY (user_id, key));
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);

ALTER TABLE event_seats ADD FOREIGN KEY (hold_id) REFERENCES holds, ADD FOREIGN KEY (order_id) REFERENCES orders;
```

**Ruling:** `orders` is unique on `(user_id, idempotency_key)`, not on `idempotency_key` alone as the spec table says. Keys are client-chosen, so a global unique would let user A's key `abc` block user B. The spec's Idempotency section already scopes keys per user.

## API additions (`api/openapi.yaml`)

| Operation | Request | Success | Errors |
| --- | --- | --- | --- |
| `POST /v1/events/{id}/holds` `createHold` | `{seat_ids: uuid[1..8], unique}` | 201 `Hold{id,event_id,seat_ids,expires_at,status}` | 404 event, 422 `NOT_ON_SALE`, 409 `SEAT_UNAVAILABLE` (message lists ids), 409 `HOLD_EXISTS` |
| `DELETE /v1/holds/{id}` `releaseHold` | – | 204 | 404 (not found or not owner), 409 `HOLD_NOT_ACTIVE`, 409 `CHECKOUT_IN_PROGRESS` |
| `POST /v1/holds/{id}/checkout` `checkout` | – | 201 `Order` (confirmed) or 202 `Order` (pending_payment, outcome unknown) | 404, 409 `HOLD_NOT_ACTIVE`/`HOLD_EXPIRED`/`CHECKOUT_IN_PROGRESS`, 422 `PAYMENT_DECLINED`, 503 `PAYMENT_UNAVAILABLE` |
| `GET /v1/orders` `listOrders` | `?limit` (≤ 50) | 200 `{items: Order[]}` newest first | 401 |
| `GET /v1/orders/{id}` `getOrder` | – | 200 `Order` | 404 (not found or not owner) |
| `POST /v1/orders/{id}/cancel` `cancelOrder` | – | 200 `Order` (refunded or cancelled) | 404, 409 `NOT_CANCELLABLE`, 422 `EVENT_STARTED` |

`Order{id, event_id, hold_id, status, total_cents, created_at, tickets: Ticket[]}`; `Ticket{id, event_seat_id, status, qr_token}`. Not-owner returns 404 rather than 403 so other users' order ids can't be probed.

## Domain interfaces

```go
// internal/payments
type Status string // "succeeded" | "declined"
type Result struct { Status Status; Ref string }
var ErrUnknownOutcome = errors.New("payments: outcome unknown") // timeout, 5xx, network: charge may or may not have happened
type Client interface {
    Charge(ctx context.Context, idempotencyKey string, amountCents int) (Result, error)
    Refund(ctx context.Context, idempotencyKey, chargeRef string, amountCents int) (Result, error)
    GetCharge(ctx context.Context, idempotencyKey string) (Result, error) // for Phase 6 reconciler; ErrNotFound if never seen
}
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient
// internal/payments/mock: provider behavior, used by services/payments_mock and by tests via httptest
type Config struct { DeclineRate, ErrorRate float64; Latency time.Duration } // adjustable at runtime: PUT /admin/config
func NewHandler(cfg Config, rnd func() float64) http.Handler // POST /charges, GET /charges/{key}, POST /refunds; idempotent by key

// internal/auth/ticket.go
func NewTicketSigner(key []byte) *TicketSigner
func (s *TicketSigner) Sign(ticketID, eventSeatID uuid.UUID) string      // base64url(payload) + "." + base64url(hmac)
func (s *TicketSigner) Verify(token string) (ticketID, eventSeatID uuid.UUID, err error)

// internal/booking
func New(pool *pgxpool.Pool, pay payments.Client, signer *auth.TicketSigner, holdTTL time.Duration) *Service
func (s *Service) CreateHold(ctx, userID, eventID uuid.UUID, seatIDs []uuid.UUID) (Hold, error)
func (s *Service) ReleaseHold(ctx, userID, holdID uuid.UUID) error
func (s *Service) Checkout(ctx, userID, holdID uuid.UUID, idemKey string) (Order, error) // Order.Status tells 201 vs 202
func (s *Service) GetOrder(ctx, userID, orderID uuid.UUID) (Order, error)
func (s *Service) ListOrders(ctx, userID uuid.UUID, limit int) ([]Order, error)
func (s *Service) CancelOrder(ctx, userID, orderID uuid.UUID) (Order, error)
func (s *Service) ExpireHolds(ctx context.Context, batch int) (int, error) // sweeper step; returns holds expired

// internal/idempotency
func Middleware(pool *pgxpool.Pool, userFrom func(context.Context) (uuid.UUID, bool), log *slog.Logger) func(http.Handler) http.Handler
```

## Key algorithms

**CreateHold** (one transaction):
1. Event must exist and be public (404), `status = 'on_sale'` and `on_sale_at <= now()` (422 `NOT_ON_SALE`).
2. `SELECT id FROM event_seats WHERE event_id = $1 AND id = ANY($2) AND state = 'available' ORDER BY id FOR UPDATE SKIP LOCKED`. `SKIP LOCKED` means a seat another transaction is grabbing right now counts as unavailable instead of making us wait. Under a flash sale, waiting would only lead to failing anyway, after holding a connection longer. `ORDER BY id` gives a consistent lock order.
3. If fewer rows than requested: roll back, 409 `SEAT_UNAVAILABLE` listing the missing ids.
4. Insert the hold. A unique violation on `holds_one_active_per_user` → 409 `HOLD_EXISTS`. Update those seats to `held`, set `hold_id`, `version = version + 1`. Commit.

**Checkout:**
1. Tx A: `SELECT … FROM holds WHERE id = $1 FOR UPDATE`; not found or not owner → 404; not active → 409 `HOLD_NOT_ACTIVE`; `expires_at <= now()` → 409 `HOLD_EXPIRED`. If an order with `(user_id, idempotency_key)` already exists, return it (domain-level dedupe for retries that lost the middleware record). Sum seat prices, then insert an order with status `pending_payment`. A unique violation on `orders_one_live_per_hold` → 409 `CHECKOUT_IN_PROGRESS`. Commit.
2. `pay.Charge(ctx, "charge-"+orderID, total)` with a 3 s timeout, outside any transaction (never hold row locks across a network call).
3. Succeeded → Tx B: lock the order; order `confirmed`; hold `converted`; seats `sold` (`hold_id = NULL, order_id = order, version+1`); insert one valid ticket per seat with a signed QR; insert a `payments` row. 201.
4. Declined → order `failed`, insert a `payments` row with status `declined`; the hold stays active so the user can retry with a new key. 422 `PAYMENT_DECLINED`.
5. `ErrUnknownOutcome` → leave the order `pending_payment`; 202 with the order. (The Phase 6 reconciler finishes it.)

**ExpireHolds:** `SELECT id FROM holds h WHERE status = 'active' AND expires_at < now() AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.hold_id = h.id AND o.status = 'pending_payment') ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED`. Then set those seats `available` and mark the holds `expired`, in one transaction. The `NOT EXISTS` stops the sweeper from releasing seats someone is paying for. `SKIP LOCKED` lets several sweeper replicas run without blocking each other.

**CancelOrder:** Tx: lock the order (owner, else 404); status must be `confirmed` (409 `NOT_CANCELLABLE`); event `starts_at` must be in the future (422 `EVENT_STARTED`); order → `cancelled`, tickets → `void`, seats → `available` (`order_id = NULL`, `version+1`). Commit. Then `pay.Refund("refund-"+orderID, chargeRef, total)`; on success, order → `refunded` and insert a `payments` refund row. On failure it stays `cancelled` (the Phase 6 reconciler retries).

**Idempotency middleware** (applies when the method is POST/DELETE, the path starts with `/v1/` but not `/v1/auth/` or `/v1/admin/`, and the caller is authenticated):
1. Missing or overlong header → 400 `IDEMPOTENCY_KEY_REQUIRED`. Read the body; `hash = sha256(method + " " + path + "\n" + body)`.
2. `INSERT … ON CONFLICT DO NOTHING RETURNING`. If inserted → run the handler with a response recorder. Store status and body if status < 500; on 5xx delete the row so the client can retry. Then write the response.
3. On conflict, load the row: hash differs → 422 `IDEMPOTENCY_KEY_REUSED`. Status stored → replay status and body (with header `Idempotent-Replay: true`). Status null and `created_at` < 60 s ago → 409 `REQUEST_IN_PROGRESS`. Status null and older → treat as abandoned (crash mid-request): take it over (update `created_at`) and run the handler. Domain dedupe (orders' `(user_id, idempotency_key)`) prevents a duplicate order.
4. The validated key is put in the request context; `booking.Checkout` reads it via `idempotency.KeyFrom(ctx)`.

---

## Tasks

### Task 1: Schema, queries, and ticket signing
- Migration `0003_booking` (above) plus down; sqlc queries for holds/orders/payments/tickets/idempotency; regenerate.
- `internal/auth/ticket.go`.
- Tests: `TestBookingConstraints` (db): two valid tickets for one seat fail; a void ticket plus a new valid one succeed; two non-failed orders on one hold fail; a failed one plus a new one succeed; same `(user, key)` order twice fails, while different users with the same key succeed. `TestTicketSignRoundTrip`, `TestTicketVerifyRejectsTamperedPayload`, `TestTicketVerifyRejectsOtherKey`, `TestTicketVerifyRejectsGarbage`.
- Commit: "Add booking schema and signed ticket tokens".

### Task 2: Idempotency middleware
- `internal/idempotency/idempotency.go` (+ sqlc queries).
- Tests, using a tiny test handler that counts executions and returns 201: `TestReplaysStoredResponse` (2 calls → 1 execution, same body, replay header); `TestDifferentBodySameKeyIs422`; `TestMissingKeyIs400`; `TestKeysAreScopedPerUser` (same key, two users → 2 executions); `TestConcurrentSameKeyExecutesOnce` (20 goroutines → 1 execution; others replay or get 409 `REQUEST_IN_PROGRESS`); `Test5xxIsNotStored` (handler returns 503, retry executes again); `TestAbandonedInProgressIsTakenOver` (row with null status, created 2 min ago → executes); `TestSkipsAuthAndAdminAndGET`.
- Commit: "Add idempotency-key middleware".

### Task 3: Holds (create, release) through HTTP
- OpenAPI additions for holds; regenerate; `internal/booking/holds.go`; handlers; wire the middleware into the router after `authenticate`.
- Tests (booking): `TestCreateHold` (seats become held with `hold_id`, version bumped, `expires_at ≈ now+TTL`); `TestCreateHoldSeatTaken` (409 listing the seat); `TestCreateHoldRejectsSeatsFromOtherEvent` (409); `TestCreateHoldNotOnSale` (draft → 404; future `on_sale_at` → 422); `TestOneActiveHoldPerUser` (409 `HOLD_EXISTS`); `TestReleaseHold` (seats available; second release → 409; other user → 404). HTTP: `TestHoldRequiresAuthAndIdempotencyKey`, `TestHoldValidation` (0 seats, 9 seats, duplicate ids → 400), and the **done-when**: `TestFlashSaleHolds500Parallel`, where 500 distinct users each request 1 random seat of the same 10 in parallel. Assert exactly 10 return 201, the rest 409, and in the DB exactly 10 held seats, each with a distinct hold.
- Commit: "Add seat holds with SKIP LOCKED row locking".

### Task 4: Hold sweeper and workers binary
- `booking.ExpireHolds`; `services/workers/main.go` (subcommand `sweeper`: loop every `SWEEP_INTERVAL`, batch 500, graceful stop on SIGTERM).
- Tests: `TestExpireHoldsReleasesSeats` (backdate `expires_at`; seats available, hold expired, version bumped); `TestExpireHoldsSkipsPendingPayment`; `TestExpireHoldsIgnoresFresh`; `TestConcurrentSweepersDontDoubleProcess` (4 goroutines on 200 expired holds → total expired = 200, no errors).
- Commit: "Add hold expiry sweeper and workers binary".

### Task 5: Mock payment service and checkout
- `internal/payments` (client and mock handler), `services/payments_mock/main.go` (env `DECLINE_RATE`, `ERROR_RATE`, `LATENCY_MS`, `PORT`); `booking/checkout.go`; OpenAPI `checkout`; handler maps status to 201/202.
- Tests (payments): `TestMockChargeIsIdempotentByKey`; `TestMockDeclineAndErrorRates` (rnd stub); `TestMockAdminConfig`; `TestClientMapsTimeoutAndServerErrorToUnknown`; `TestClientGetCharge`. Booking (using the mock via httptest): `TestCheckoutSuccess` (order confirmed, hold converted, seats sold with `order_id`, N valid tickets whose QR verifies, one payments row); `TestCheckoutDeclined` (order failed, hold still active, retry with a new key succeeds); `TestCheckoutUnknownOutcome` (latency > timeout → order pending, seats still held, sweeper doesn't release them even after expiry); `TestCheckoutExpiredHold` (409); `TestCheckoutNotOwner` (404); `TestCheckoutSameKeyReturnsSameOrder`; `TestDoubleCheckoutParallel` (20 parallel checkouts of one hold, different keys → exactly 1 confirmed, mock saw exactly 1 charge).
- Commit: "Add mock payment service and checkout".

### Task 6: Orders and cancellation
- `booking/orders.go` (Get/List/Cancel); OpenAPI `listOrders`, `getOrder`, `cancelOrder`.
- Tests: `TestGetOrderOwnerOnly` (other user → 404); `TestListOrdersNewestFirst`; `TestCancelRefundsAndReleases` (status refunded, tickets void, seats available, refund payment row); `TestCancelThenResell` (another user holds and buys the same seat → a new valid ticket exists, the old one is void, and the unique index is satisfied); `TestCancelRefundFailsStaysCancelled`; `TestCancelTwiceIs409`; `TestCancelAfterEventStartIs422`.
- Commit: "Add order history and cancellation with refunds".

### Task 7: Invariants, Compose, docs, PR
- `booking.CheckInvariants(ctx) (Violations, error)`: seats with >1 valid ticket; sold seats without a confirmed order; confirmed orders whose valid-ticket count ≠ seat count; held seats whose hold isn't active. The Phase 8 invariant checker reuses it.
- Test `TestFlashSaleEndToEnd`: 200 users race for 10 seats (holds), winners check out in parallel, some cancel, others then buy the released seats; finally `CheckInvariants` returns none.
- Compose: add `payments` (mock) and `sweeper` services, plus the env `PAYMENTS_URL`, `TICKET_SIGNING_KEY`, `HOLD_TTL`. Smoke check extended: hold 2 seats → checkout → order confirmed with 2 tickets → cancel → refunded.
- Docs: LEARNING.md Phase 2 (row locks vs. `SKIP LOCKED`, why no locks across network calls, idempotency keys, ambiguous payment outcomes, partial unique indexes, sweeper concurrency); ADR 0004 (`SKIP LOCKED` over plain `FOR UPDATE` / optimistic versioning); README status.
- Full suite + smoke, push, PR, merge.
