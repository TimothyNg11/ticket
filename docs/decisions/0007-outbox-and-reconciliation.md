# 7. Outbox for events, reconciliation for payments

**Status:** accepted (Phase 6)

## Context

Two things happen outside the database transaction: charging the card, and announcing that an order changed. Both can fail independently of the commit.

## Decision

- **Events:** a transactional outbox. Events are rows written in the same transaction as the change; a relay publishes them to a Redis Stream and marks them sent. Consumers deduplicate on the event id.
- **Payments:** never hold a transaction across the charge (ADR 0004 context). An unknown outcome leaves the order `pending_payment`. A reconciler repeats the charge with the same idempotency key until the provider gives a definite answer.
- **Messaging:** Redis Streams with consumer groups, already in the stack, rather than Kafka. Kafka remains a stretch goal from the spec.

## Consequences

- No lost events and no events for rolled-back changes; duplicates are possible and handled.
- A buyer whose payment outcome was unknown waits up to about 45 s (30 s quiet period + 15 s interval) for the order to settle. The API returns 202 so clients know to poll.
- The design leans entirely on the provider honoring idempotency keys. Every real provider we'd use (Stripe, Adyen) does; the mock does too.
- Redis Streams keep events in memory (capped at about 1M); a Kafka-backed log would be the upgrade for durable replay.
