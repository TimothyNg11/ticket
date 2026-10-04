#!/usr/bin/env bash
# Prints the server-side view of a time window from Prometheus: latency per route
# measured inside the API, saturation (DB and Redis pools), and CPU by component.
# Pairs with the client-side numbers the load tools print.
#
# Usage: loadtest/server-report.sh <start-unix> <end-unix>
set -euo pipefail
START=${1:?start}
END=${2:?end}
PROM=${PROM:-http://localhost:19090}
if ! curl -s "$PROM/-/ready" >/dev/null 2>&1; then
  kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 19090:9090 >/dev/null 2>&1 &
  until curl -s "$PROM/-/ready" >/dev/null 2>&1; do sleep 1; done
fi
RANGE="$((END - START))s"

# Instant query evaluated at END over the whole window. -g: PromQL braces aren't URL globs.
one() {
  curl -sg --get "$PROM/api/v1/query" --data-urlencode "query=$2" --data-urlencode "time=$END" |
    python -c "
import json, sys
r = json.load(sys.stdin)['data']['result']
for s in sorted(r, key=lambda s: str(s['metric'])):
    label = ' '.join(v for k, v in s['metric'].items())
    try: val = float(s['value'][1])
    except: val = float('nan')
    print(f'  {\"$1\":<34} {label:<30} {val:10.3f}')"
}
echo "== server side, ${RANGE} window"
one "p50 latency s by route" "histogram_quantile(0.50, sum by (le, route) (rate(ticket_http_request_duration_seconds_bucket{route=~\"/v1/(events|holds|queue).*\"}[$RANGE])))"
one "p99 latency s by route" "histogram_quantile(0.99, sum by (le, route) (rate(ticket_http_request_duration_seconds_bucket{route=~\"/v1/(events|holds|queue).*\"}[$RANGE])))"
one "requests/s by route" "sum by (route) (rate(ticket_http_requests_total{route=~\"/v1/(events|holds|queue).*\"}[$RANGE]))"
one "5xx/s" "sum(rate(ticket_http_requests_total{status=~\"5..\"}[$RANGE]))"
one "seat-map cache lookups/s" "sum by (result) (rate(ticket_cache_lookups_total{kind=\"seatmap\"}[$RANGE]))"
one "payment charge p99 s" "histogram_quantile(0.99, sum by (le) (rate(ticket_payment_call_duration_seconds_bucket{op=\"charge\"}[$RANGE])))"
one "db pool: wait s per s" "sum(rate(ticket_db_pool_acquire_wait_seconds_total{job=~\".*api.*\"}[$RANGE]))"
one "db pool: peak in use / max" "max_over_time((sum(ticket_db_pool_acquired_conns{job=~\".*api.*\"}) / sum(ticket_db_pool_max_conns{job=~\".*api.*\"}))[$RANGE:15s])"
one "redis pool: wait s per s" "sum(rate(ticket_redis_pool_wait_seconds_total[$RANGE]))"
one "cpu cores by container (avg)" "sum by (container) (rate(container_cpu_usage_seconds_total{namespace=~\"ticket|traefik\", container=~\"api|postgres|pgbouncer|redis|traefik|onsale|k6\"}[$RANGE]))"
one "api pods (max)" "max_over_time(count(up{job=~\".*api.*\", namespace=\"ticket\"})[$RANGE:15s])"
