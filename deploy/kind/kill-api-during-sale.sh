#!/usr/bin/env bash
# Phase 7 done-when: run a flash sale through the ingress and delete an API pod
# every 15 seconds while it runs. Passes only if no order fails and every
# invariant holds (the flashsale tool exits non-zero otherwise).
#
# Usage: deploy/kind/kill-api-during-sale.sh [duration-seconds]
set -euo pipefail
cd "$(dirname "$0")/../.."
DURATION=${1:-60}
NS=ticket

go run ./tools/flashsale -base https://localhost -insecure \
  -duration "${DURATION}s" -users 300 -seats 150 -concurrency 60 &
SALE=$!

killed=0
for ((t = 15; t < DURATION; t += 15)); do
  sleep 15
  pod=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=api -o name | shuf -n1)
  echo "[chaos t=${t}s] deleting $pod"
  kubectl -n "$NS" delete "$pod" --wait=false >/dev/null
  killed=$((killed + 1))
done

wait "$SALE"
echo "[chaos] deleted $killed API pods during the sale; flash sale passed"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=api
