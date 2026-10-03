#!/usr/bin/env bash
# Phase 8 done-when drill: run a flash sale (big enough not to sell out) against the Compose stack, make the
# payment provider slow halfway through, then show that the metrics explain the
# checkout latency spike (and that a trace shows where the time went).
#
# Usage: deploy/observability/latency-spike-drill.sh   (stack must be up, with
#        RATE_LIMIT_SCALE=1000 so one machine can play 200 buyers)
set -euo pipefail
cd "$(dirname "$0")/../.."
PROM=http://localhost:9090
START=$(date +%s)

go run ./tools/flashsale -base http://localhost:8080 -duration 90s -users 200 -seats 3000 -concurrency 10 > /tmp/drill-sale.log 2>&1 &
SALE=$!
sleep 30
curl -s -X PUT localhost:8081/admin/config -d '{"latency_ms":800}' >/dev/null
INJECT=$(date +%s)
echo "t=$((INJECT - START))s: payment provider latency raised to 800 ms"
wait "$SALE" || { tail -5 /tmp/drill-sale.log; exit 1; }
curl -s -X PUT localhost:8081/admin/config -d '{"latency_ms":50}' >/dev/null
tail -3 /tmp/drill-sale.log
sleep 10 # one more scrape

END=$(date +%s)
# -g: curl must not treat {id} in PromQL as a URL glob.
series() {
  curl -sg --get "$PROM/api/v1/query_range" --data-urlencode "query=$1" \
    --data-urlencode "start=$START" --data-urlencode "end=$END" --data-urlencode step=15 |
    python -c "
import json, sys
r = json.load(sys.stdin)['data']['result']
vals = r[0]['values'] if r else []
print('  '.join('  -  ' if v[1] == 'NaN' else f'{float(v[1]):5.2f}' for v in vals))"
}
echo
echo "15-second steps from sale start ->     (latency injected at t=$((INJECT - START))s)"
printf '%-24s' "checkout p99 (s)";  series 'histogram_quantile(0.99, sum by (le) (rate(ticket_http_request_duration_seconds_bucket{route="/v1/holds/{id}/checkout"}[30s])))'
printf '%-24s' "payment charge p99 (s)"; series 'histogram_quantile(0.99, sum by (le) (rate(ticket_payment_call_duration_seconds_bucket{op="charge"}[30s])))'
printf '%-24s' "hold p99 (s)";      series 'histogram_quantile(0.99, sum by (le) (rate(ticket_http_request_duration_seconds_bucket{route="/v1/events/{id}/holds"}[30s])))'
printf '%-24s' "seatmap p99 (s)";   series 'histogram_quantile(0.99, sum by (le) (rate(ticket_http_request_duration_seconds_bucket{route="/v1/events/{id}/seatmap"}[30s])))'
printf '%-24s' "db conns in use";   series 'max(max_over_time(ticket_db_pool_acquired_conns[15s]))'
printf '%-24s' "seatmap cache hit %";  series 'sum(rate(ticket_cache_lookups_total{kind="seatmap",result="hit"}[30s])) / sum(rate(ticket_cache_lookups_total{kind="seatmap"}[30s]))'

echo
echo "slowest checkout trace in Jaeger:"
curl -sg "http://localhost:16686/api/traces?service=ticket-api&limit=200&lookback=5m&minDuration=700ms" | python -c "
import json, sys
traces = [t for t in json.load(sys.stdin)['data'] if any('checkout' in s['operationName'] for s in t['spans'])]
t = max(traces, key=lambda t: max(s['duration'] for s in t['spans']))
spans = sorted(t['spans'], key=lambda s: s['startTime'])
root = spans[0]['startTime']
print('  trace', t['traceID'])
for s in spans:
    if s['duration'] > 2000 or 'checkout' in s['operationName'] or 'charge' in s['operationName'].lower() or 'POST' in s['operationName']:
        print(f\"  +{(s['startTime'] - root) / 1000:7.1f} ms  {s['duration'] / 1000:7.1f} ms  {s['operationName'][:60]}\")"
