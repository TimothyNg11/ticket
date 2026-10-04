#!/usr/bin/env bash
# Runs a k6 script inside the kind cluster (through the ingress) and saves the
# summary to loadtest/results/<label>.txt.
#
# Usage: loadtest/k6-job.sh <script> <label> [ENV=value ...]
#   e.g. loadtest/k6-job.sh loadtest/k6/seatmap.js seatmap-reads EVENT_ID=<uuid>
set -euo pipefail
cd "$(dirname "$0")/.."
SCRIPT=${1:?script}
LABEL=${2:?label}
shift 2
NS=ticket
ENVS=""
for kv in "$@"; do ENVS="$ENVS            - { name: ${kv%%=*}, value: \"${kv#*=}\" }
"; done

kubectl -n "$NS" create configmap k6-script --from-file=script.js="$SCRIPT" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete job k6 --ignore-not-found --wait=true >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: k6
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels: { app.kubernetes.io/part-of: ticket, app.kubernetes.io/component: loadtest }
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      volumes: [{ name: script, configMap: { name: k6-script } }]
      containers:
        - name: k6
          image: grafana/k6:1.3.0
          args: ["run", "--quiet", "/scripts/script.js"]
          env:
$ENVS          volumeMounts: [{ name: script, mountPath: /scripts }]
          resources:
            requests: { cpu: "2", memory: 512Mi }
            limits: { cpu: "4", memory: 2Gi }
EOF
echo "k6 job started"
until kubectl -n "$NS" get job k6 -o jsonpath='{.status.conditions[*].type}' 2>/dev/null | grep -qE "Complete|Failed"; do sleep 10; done
kubectl -n "$NS" logs job/k6 | tee "loadtest/results/$LABEL.txt"
