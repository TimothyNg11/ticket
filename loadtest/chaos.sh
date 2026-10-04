#!/usr/bin/env bash
# Chaos experiment (Phase 9): a two-minute flash sale while, one after another,
# an API pod, the payment service, Redis, and two workers are killed, and the
# payment provider turns slow and flaky. Passes only if, after recovery:
#   - no client operation failed (clients retry with the same Idempotency-Key)
#   - no order is left pending_payment (the reconciler settled them)
#   - the outbox drained (every event published)
#   - every invariant holds (no seat sold twice, money and seats agree)
#
# Usage: BASE=http://localhost:18080 loadtest/chaos.sh
set -euo pipefail
cd "$(dirname "$0")/.."
BASE=${BASE:-http://localhost:18080}
NS=ticket
log() { echo "[chaos t=$(($(date +%s) - START))s] $*"; }
kill_one() { kubectl -n "$NS" delete "$(kubectl -n "$NS" get pods -l "$1" -o name | shuf -n1)" --wait=false >/dev/null; }

kubectl -n "$NS" port-forward svc/payments 18081:8081 >/dev/null 2>&1 &
PF_PAY=$!
trap 'kill $PF_PAY 2>/dev/null || true' EXIT
until curl -s localhost:18081/healthz >/dev/null; do sleep 1; done
payments() { curl -s -X PUT localhost:18081/admin/config -d "$1" >/dev/null || true; }

START=$(date +%s)
go run ./tools/flashsale -base "$BASE" -duration 120s -users 300 -seats 2000 -concurrency 40 -insecure \
  > /tmp/chaos-sale.log 2>&1 &
SALE=$!

sleep 15; log "kill an API pod";                 kill_one app.kubernetes.io/name=api
sleep 15; log "kill the payment service";        kill_one app.kubernetes.io/name=payments
sleep 15
# The port-forward died with the old payments pod; reconnect to the new one.
kill $PF_PAY 2>/dev/null || true
kubectl -n "$NS" wait --for=condition=ready pod -l app.kubernetes.io/name=payments --timeout=60s >/dev/null
kubectl -n "$NS" port-forward svc/payments 18081:8081 >/dev/null 2>&1 &
PF_PAY=$!
until curl -s localhost:18081/healthz >/dev/null; do sleep 1; done
log "payments: 40% errors (half after charging), 1.5s latency"
payments '{"error_rate":0.4,"latency_ms":1500}'
sleep 15; log "kill Redis (cache, rate limits, queue, streams)"; kubectl -n "$NS" delete pod redis-0 --wait=false >/dev/null
sleep 15; log "kill the reconciler and the outbox relay"
kill_one app.kubernetes.io/name=worker-reconciler
kill_one app.kubernetes.io/name=worker-outbox-relay
sleep 15; log "payments healthy again";         payments '{"latency_ms":50}'

wait "$SALE" && SALE_OK=1 || SALE_OK=0
tail -4 /tmp/chaos-sale.log

log "waiting for the reconciler and relay to settle everything"
for _ in $(seq 40); do
  pending=$(kubectl -n "$NS" exec postgres-0 -- psql -U ticket -d ticket -tAc "SELECT count(*) FROM orders WHERE status = 'pending_payment'")
  unpublished=$(kubectl -n "$NS" exec postgres-0 -- psql -U ticket -d ticket -tAc "SELECT count(*) FROM outbox WHERE published_at IS NULL")
  unrefunded=$(kubectl -n "$NS" exec postgres-0 -- psql -U ticket -d ticket -tAc "SELECT count(*) FROM orders WHERE status = 'cancelled'")
  [ "$pending" = 0 ] && [ "$unpublished" = 0 ] && [ "$unrefunded" = 0 ] && break
  sleep 5
done
log "pending_payment=$pending outbox_unpublished=$unpublished cancelled_awaiting_refund=$unrefunded"
summary=$(kubectl -n "$NS" exec postgres-0 -- psql -U ticket -d ticket -tAc \
  "SELECT string_agg(status || '=' || n, ' ') FROM (SELECT status, count(*) n FROM orders GROUP BY status ORDER BY status) s")
log "orders: $summary"

inv=$(kubectl -n "$NS" exec postgres-0 -- psql -U ticket -d ticket -tAc "
  SELECT (SELECT count(*) FROM (SELECT event_seat_id FROM tickets WHERE status='valid' GROUP BY 1 HAVING count(*)>1) d)
       + (SELECT count(*) FROM event_seats es LEFT JOIN orders o ON o.id = es.order_id WHERE es.state='sold' AND (o.id IS NULL OR o.status <> 'confirmed'))
       + (SELECT count(*) FROM orders o WHERE o.status='confirmed' AND (SELECT count(*) FROM payments p WHERE p.order_id=o.id AND p.kind='charge' AND p.status='succeeded') <> 1)")
log "invariant violations: $inv"

if [ "$SALE_OK" = 1 ] && [ "$pending" = 0 ] && [ "$unpublished" = 0 ] && [ "$unrefunded" = 0 ] && [ "$inv" = 0 ]; then
  echo "CHAOS OK"
else
  echo "CHAOS FAIL"; exit 1
fi
