#!/usr/bin/env bash
# Runs the on-sale simulation *inside* the kind cluster as a Job, entering through
# the ingress (Traefik) like real traffic. Running the generator on the Windows
# host measured Docker Desktop's userspace port proxy more than the system:
# server-side seat-map p50 was 5 ms while the host client saw 2.2 s.
#
# Usage: loadtest/job.sh <label> [users] [extra onsale flags...]
# Output is also saved to loadtest/results/<label>.txt.
set -euo pipefail
cd "$(dirname "$0")/.."
LABEL=${1:?label}
USERS=${2:-50000}
shift $(($# < 2 ? $# : 2))
NS=ticket
EXTRA=""
for a in "$@"; do EXTRA="$EXTRA, \"$a\""; done

for t in seed onsale; do
  docker build -q --build-arg PKG=./loadtest/$t -t ticket-loadtest-$t:dev . >/dev/null
  kind load docker-image ticket-loadtest-$t:dev --name ticket >/dev/null 2>&1
done

kubectl -n "$NS" delete job loadtest --ignore-not-found --wait=true >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: loadtest
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels: { app.kubernetes.io/part-of: ticket, app.kubernetes.io/component: loadtest }
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: { runAsNonRoot: true, runAsUser: 65532, seccompProfile: { type: RuntimeDefault } }
      volumes: [{ name: work, emptyDir: {} }]
      initContainers:
        - name: seed
          image: ticket-loadtest-seed:dev
          imagePullPolicy: Never
          args: ["-users", "$USERS", "-out", "/work/tokens.txt", "-admin-out", "/work/admin.txt", "-ttl", "6h"]
          env:
            - name: PGPW
              valueFrom: { secretKeyRef: { name: ticket-secrets, key: POSTGRES_PASSWORD } }
            - name: DATABASE_URL
              value: postgres://ticket:\$(PGPW)@postgres:5432/ticket?sslmode=disable
            - name: JWT_SECRET
              valueFrom: { secretKeyRef: { name: ticket-secrets, key: JWT_SECRET } }
          volumeMounts: [{ name: work, mountPath: /work }]
          securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
      containers:
        - name: onsale
          image: ticket-loadtest-onsale:dev
          imagePullPolicy: Never
          args: ["-base", "http://traefik.traefik.svc.cluster.local", "-users", "$USERS", "-seats", "5000",
                 "-tokens", "/work/tokens.txt", "-admin-token-file", "/work/admin.txt", "-label", "$LABEL"$EXTRA]
          env:
            - { name: GOMEMLIMIT, value: 2560MiB } # GC harder before the 3Gi limit
          volumeMounts: [{ name: work, mountPath: /work }]
          securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
          resources:
            requests: { cpu: "2", memory: 1Gi }
            limits: { cpu: "4", memory: 3Gi }
EOF
echo "loadtest job started ($USERS buyers)"
# Wait for the Job to finish rather than following its logs: a long-lived log
# stream can drop and end the script while the Job is still running.
until kubectl -n "$NS" get job loadtest -o jsonpath='{.status.conditions[*].type}' 2>/dev/null | grep -qE "Complete|Failed"; do
  sleep 15
done
kubectl -n "$NS" logs job/loadtest -c onsale | tee "loadtest/results/$LABEL.txt"
# Server-side numbers for exactly the Job's lifetime, taken from Kubernetes'
# own timestamps (so it works however late this script gets to it).
ts() { python -c "import sys,datetime;print(int(datetime.datetime.fromisoformat(sys.argv[1].replace('Z','+00:00')).timestamp()))" "$1"; }
JSTART=$(ts "$(kubectl -n "$NS" get pod -l job-name=loadtest -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.startedAt}')")
JEND=$(ts "$(kubectl -n "$NS" get pod -l job-name=loadtest -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.finishedAt}')")
bash loadtest/server-report.sh "$JSTART" "$JEND" | tee -a "loadtest/results/$LABEL.txt"
kubectl -n "$NS" get job loadtest -o jsonpath='{.status.conditions[*].type}' | grep -q Complete
