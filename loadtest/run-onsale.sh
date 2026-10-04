#!/usr/bin/env bash
# Seeds buyers into the kind cluster and runs the on-sale simulation against it.
# Results are printed and saved to loadtest/results/<label>.txt for BENCHMARKS.md.
#
# Usage: BASE=http://localhost:18080 loadtest/run-onsale.sh <label> [users] [extra onsale flags...]
set -euo pipefail
cd "$(dirname "$0")/.."
LABEL=${1:?label}
USERS=${2:-50000}
shift $(($# < 2 ? $# : 2))
BASE=${BASE:-http://localhost:18080}
NS=ticket
mkdir -p loadtest/results

secret() { kubectl -n "$NS" get secret ticket-secrets -o jsonpath="{.data.$1}" | base64 -d; }
kubectl -n "$NS" port-forward svc/postgres 15432:5432 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
until (echo >/dev/tcp/127.0.0.1/15432) 2>/dev/null; do sleep 1; done
DATABASE_URL="postgres://ticket:$(secret POSTGRES_PASSWORD)@localhost:15432/ticket?sslmode=disable" \
  JWT_SECRET="$(secret JWT_SECRET)" go run ./loadtest/seed -users "$USERS" -ttl 12h \
  -out loadtest-tokens.txt -admin-out loadtest-admin-token.txt

go run ./loadtest/onsale -base "$BASE" -users "$USERS" -seats 5000 -label "$LABEL" "$@" \
  | tee "loadtest/results/$LABEL.txt"
