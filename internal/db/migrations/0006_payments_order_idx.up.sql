-- The invariant checker and refunds look up payments by order. Without this,
-- each lookup scanned the whole table (Phase 9: 22 s at 21,000 orders).
CREATE INDEX payments_order_idx ON payments (order_id, kind, status);
