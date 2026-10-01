ALTER TABLE event_seats DROP CONSTRAINT event_seats_hold_fk, DROP CONSTRAINT event_seats_order_fk;
DROP INDEX event_seats_hold_idx;
DROP INDEX event_seats_order_idx;
DROP TABLE idempotency_keys;
DROP TABLE tickets;
DROP TABLE payments;
DROP TABLE orders;
DROP TABLE holds;
