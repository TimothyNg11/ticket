#!/usr/bin/env bash
# Seeds buyers, starts an on-sale, and captures a 30-second CPU profile from one
# API pod while it runs. Writes the profile to $OUT (default /tmp/cpu.pprof) and
# prints the hottest functions.
#
# Usage: BASE=http://localhost:18080 loadtest/profile-under-load.sh [users]
set -euo pipefail
cd "$(dirname "$0")/.."
BASE=${BASE:-http://localhost:18080}
USERS=${1:-15000}
OUT=${OUT:-/tmp/cpu.pprof}
NS=ticket

secret() { kubectl -n "$NS" get secret ticket-secrets -o jsonpath="{.data.$1}" | base64 -d; }
kubectl -n "$NS" port-forward svc/postgres 15432:5432 >/dev/null 2>&1 &
PF_DB=$!
POD=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=api -o name | head -1)
kubectl -n "$NS" port-forward "$POD" 16060:6060 >/dev/null 2>&1 &
PF_PPROF=$!
trap 'kill $PF_DB $PF_PPROF 2>/dev/null || true' EXIT
until (echo >/dev/tcp/127.0.0.1/15432) 2>/dev/null && curl -s localhost:16060/debug/pprof/ >/dev/null; do sleep 1; done

DATABASE_URL="postgres://ticket:$(secret POSTGRES_PASSWORD)@localhost:15432/ticket?sslmode=disable" \
  JWT_SECRET="$(secret JWT_SECRET)" go run ./loadtest/seed -users "$USERS" -ttl 12h \
  -out loadtest-tokens.txt -admin-out loadtest-admin-token.txt

go run ./loadtest/onsale -base "$BASE" -users "$USERS" -seats 5000 -label profile > /tmp/profile-run.log 2>&1 &
SALE=$!
sleep 60 # past the initial rush, into steady buying
echo "profiling $POD for 30s"
curl -s "localhost:16060/debug/pprof/profile?seconds=30" -o "$OUT"
go tool pprof -top -nodecount=30 "$OUT" 2>/dev/null | head -38
wait "$SALE" || true
sed -n '/buyers in/,$p' /tmp/profile-run.log
