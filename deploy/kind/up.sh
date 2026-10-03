#!/usr/bin/env bash
# Brings up the full system on a local kind cluster:
#   cluster -> Traefik ingress, cert-manager, metrics-server -> images -> Helm release.
# Re-running is safe: every step is idempotent.
#
# Usage: deploy/kind/up.sh            (from the repo root)
# Needs: docker, kind, kubectl, helm, openssl.
set -euo pipefail

CLUSTER=ticket
NS=ticket
TAG=${TAG:-dev}
cd "$(dirname "$0")/../.."

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --config deploy/kind/cluster.yaml --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

echo "== cluster add-ons"
helm repo add traefik https://traefik.github.io/charts --force-update >/dev/null
helm repo add jetstack https://charts.jetstack.io --force-update >/dev/null
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update >/dev/null
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update >/dev/null
helm upgrade --install cert-manager jetstack/cert-manager -n cert-manager --create-namespace \
  --set crds.enabled=true --wait >/dev/null
# kind kubelets use self-signed serving certs; the HPA needs metrics-server to talk to them.
helm upgrade --install metrics-server metrics-server/metrics-server -n kube-system \
  --set 'args={--kubelet-insecure-tls}' --wait >/dev/null
# Traefik runs on the control-plane node, bound to host ports 80/443 that kind
# maps to the machine (see cluster.yaml). ADR 0008 explains Traefik over NGINX.
helm upgrade --install traefik traefik/traefik -n traefik --create-namespace   -f deploy/kind/traefik-values.yaml --wait >/dev/null

# Prometheus, Alertmanager, Grafana, kube-state-metrics, node-exporter. The
# selectors are opened up so it picks up our ServiceMonitors, rules and dashboards.
helm upgrade --install monitoring prometheus-community/kube-prometheus-stack -n monitoring --create-namespace \
  -f deploy/kind/monitoring-values.yaml --wait --timeout 8m >/dev/null

echo "== images"
for svc in api workers payments_mock; do
  name=ticket-${svc//_/-}
  docker build -q --build-arg SERVICE="$svc" -t "$name:$TAG" . >/dev/null
  kind load docker-image "$name:$TAG" --name "$CLUSTER" >/dev/null
done

echo "== secrets"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# Generated once per cluster and never written to the repo.
if ! kubectl -n "$NS" get secret ticket-secrets >/dev/null 2>&1; then
  kubectl -n "$NS" create secret generic ticket-secrets \
    --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 16)" \
    --from-literal=JWT_SECRET="$(openssl rand -hex 32)" \
    --from-literal=TICKET_SIGNING_KEY="$(openssl rand -hex 32)" >/dev/null
fi

echo "== helm release"
helm upgrade --install ticket deploy/helm/ticket -n "$NS" \
  -f deploy/helm/ticket/values-local.yaml --set image.tag="$TAG" --wait --timeout 6m

echo "== ready: https://localhost (self-signed; use -k / --insecure)"
echo "   Grafana:    kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80   (dashboard: Ticket: On-sale)"
echo "   Prometheus: kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 9090"
echo "   Jaeger:     kubectl -n $NS port-forward svc/jaeger 16686"
kubectl -n "$NS" get pods
