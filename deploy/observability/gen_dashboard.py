"""Generates the Grafana "On-sale" dashboard JSON.

Run from the repo root: python deploy/observability/gen_dashboard.py
Writes deploy/helm/ticket/files/onsale-dashboard.json, which both the Helm chart
(Grafana sidecar ConfigMap) and Docker Compose (Grafana provisioning) load.

Panels are laid out top to bottom in the order you'd investigate a latency
spike: is traffic up -> which route is slow -> is it the cache, the database
pool, the payment provider, or the queue -> did correctness hold.
"""
import json
import os

DS = {"type": "prometheus", "uid": "prometheus"}
panels = []
y = 0


def row(title):
    global y
    panels.append({"type": "row", "title": title, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []})
    y += 1


def ts(title, exprs, unit="short", w=12, x=0, h=8, desc=""):
    panels.append({
        "type": "timeseries", "title": title, "description": desc, "datasource": DS,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 1, "fillOpacity": 8}}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull", "max"]},
                    "tooltip": {"mode": "multi"}},
        "targets": [{"datasource": DS, "expr": e, "legendFormat": l, "refId": chr(65 + i)}
                    for i, (e, l) in enumerate(exprs)],
    })


def stat(title, expr, unit="short", w=4, x=0, thresholds=None, desc=""):
    panels.append({
        "type": "stat", "title": title, "description": desc, "datasource": DS,
        "gridPos": {"h": 4, "w": w, "x": x, "y": y},
        "fieldConfig": {"defaults": {"unit": unit, "thresholds": {"mode": "absolute", "steps": thresholds or [
            {"color": "green", "value": None}]}}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "background", "graphMode": "area"},
        "targets": [{"datasource": DS, "expr": expr, "refId": "A"}],
    })


RED = [{"color": "green", "value": None}, {"color": "red", "value": 1}]

row("Is the sale healthy?")
stat("Requests / s", 'sum(rate(ticket_http_requests_total{route=~"/v1.*"}[1m]))', "reqps", 4, 0)
stat("Error rate (5xx)", 'sum(rate(ticket_http_requests_total{status=~"5.."}[5m])) / clamp_min(sum(rate(ticket_http_requests_total[5m])), 1e-9)',
     "percentunit", 4, 4, [{"color": "green", "value": None}, {"color": "red", "value": 0.01}])
stat("p99 hold latency", 'histogram_quantile(0.99, sum by (le) (rate(ticket_http_request_duration_seconds_bucket{route="/v1/events/{id}/holds"}[5m])))',
     "s", 4, 8, [{"color": "green", "value": None}, {"color": "orange", "value": 0.3}, {"color": "red", "value": 1}])
stat("Seats sold / min", "sum(rate(ticket_seats_sold_total[1m])) * 60", "short", 4, 12)
stat("Invariant violations", "sum(ticket_invariant_violations)", "short", 4, 16, RED,
     "Double-sold seats or money/seat mismatches. Must be 0; any other value pages.")
stat("Payment circuit", "max(ticket_payment_circuit_state)", "short", 4, 20,
     [{"color": "green", "value": None}, {"color": "red", "value": 1}, {"color": "orange", "value": 2}],
     "0 closed (healthy), 1 open (failing fast), 2 half-open (probing)")
y += 4

row("Traffic and latency by route")
ts("Request rate by route", [('sum by (route) (rate(ticket_http_requests_total{route=~"/v1.*"}[1m]))', "{{route}}")], "reqps")
ts("Responses by status", [('sum by (status) (rate(ticket_http_requests_total{route=~"/v1.*"}[1m]))', "{{status}}")], "reqps", x=12)
y += 8
ts("p99 latency by route", [('histogram_quantile(0.99, sum by (le, route) (rate(ticket_http_request_duration_seconds_bucket{route=~"/v1.*"}[1m])))', "{{route}}")], "s")
ts("p50 latency by route", [('histogram_quantile(0.5, sum by (le, route) (rate(ticket_http_request_duration_seconds_bucket{route=~"/v1.*"}[1m])))', "{{route}}")], "s", x=12)
y += 8

row("Why is it slow? Cache, database, payments")
ts("Seat-map cache", [
    ('sum by (result) (rate(ticket_cache_lookups_total{kind="seatmap"}[1m]))', "{{result}}"),
], "ops", w=8, desc="A drop in hits or a rise in errors (Redis down) pushes reads onto Postgres.")
ts("DB pool: connections in use vs. max", [
    ("sum(ticket_db_pool_acquired_conns)", "in use"),
    ("sum(ticket_db_pool_max_conns)", "max"),
], "short", w=8, x=8, desc="In use pinned at max = requests queueing for a connection.")
ts("DB pool: waits for a connection", [
    ("sum(rate(ticket_db_pool_empty_acquires_total[1m]))", "waits/s"),
    ("sum(rate(ticket_db_pool_acquire_wait_seconds_total[1m]))", "seconds waited/s"),
], "short", w=8, x=16)
y += 8
ts("Payment calls by outcome", [
    ('sum by (op, outcome) (rate(ticket_payment_calls_total[1m]))', "{{op}} {{outcome}}"),
    ("sum(rate(ticket_payment_retries_total[1m]))", "retries"),
], "ops", w=12)
ts("Payment latency (p50 / p99 per attempt)", [
    ('histogram_quantile(0.5, sum by (le) (rate(ticket_payment_call_duration_seconds_bucket{op="charge"}[1m])))', "p50"),
    ('histogram_quantile(0.99, sum by (le) (rate(ticket_payment_call_duration_seconds_bucket{op="charge"}[1m])))', "p99"),
], "s", w=12, x=12, desc="Checkout latency follows this line when the provider is slow.")
y += 8

row("Inventory and the waiting room")
ts("Holds by outcome", [('sum by (outcome) (rate(ticket_holds_total[1m])) * 60', "{{outcome}}")], "short", w=8,
   desc="per minute: created, seat_taken (lost the race), released, expired")
ts("Active holds", [("max(ticket_active_holds)", "active")], "short", w=8, x=8)
ts("Orders by status", [('sum by (status) (rate(ticket_orders_total[1m])) * 60', "{{status}}")], "short", w=8, x=16,
   desc="per minute")
y += 8
ts("Waiting room: queue length", [("sum by (event) (ticket_queue_length)", "{{event}}")], "short", w=8)
ts("Admissions / min", [("sum(rate(ticket_queue_admitted_total[1m])) * 60", "admitted")], "short", w=8, x=8)
ts("Rate limited (429) by bucket", [('sum by (bucket) (rate(ticket_rate_limited_total[1m]))', "{{bucket}}")], "reqps", w=8, x=16)
y += 8

row("Async: outbox, notifications, reconciliation")
ts("Outbox lag", [("max(ticket_outbox_lag_seconds)", "oldest unpublished")], "s", w=8,
   desc="Growing lag = the relay is down or behind; notifications are delayed, nothing is lost.")
ts("Events published / notifications", [
    ("sum(rate(ticket_outbox_published_total[1m]))", "published"),
    ('sum by (type) (rate(ticket_notifications_total[1m]))', "notified {{type}}"),
], "ops", w=8, x=8)
ts("Reconciled orders", [('sum by (result) (increase(ticket_reconciled_total[5m]))', "{{result}}")], "short", w=8, x=16)
y += 8

row("Pods (Kubernetes only)")
ts("API pods: CPU", [('sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="ticket", container="api"}[1m]))', "{{pod}}")], "short", w=8)
ts("API pods: memory", [('sum by (pod) (container_memory_working_set_bytes{namespace="ticket", container="api"})', "{{pod}}")], "bytes", w=8, x=8)
ts("Restarts (15m)", [('sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace="ticket"}[15m]))', "{{pod}}")], "short", w=8, x=16)
y += 8

dashboard = {
    "uid": "ticket-onsale", "title": "Ticket: On-sale", "tags": ["ticket"],
    "timezone": "browser", "refresh": "10s", "schemaVersion": 39,
    "time": {"from": "now-30m", "to": "now"},
    "panels": panels,
}
out = os.path.join("deploy", "helm", "ticket", "files", "onsale-dashboard.json")
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, "w", encoding="utf-8", newline="\n") as f:
    json.dump(dashboard, f, indent=2)
    f.write("\n")
print("wrote", out, len(panels), "panels")
