#!/usr/bin/env bash
# Hands the kind cluster over to GitOps: installs Argo CD and the root
# Application, which applies apps/ticket.yaml from the gitops branch; that in
# turn deploys the chart from main with the image tag CI recorded. After this,
# merging to main is the only deploy step.
#
# Run deploy/kind/up.sh first (cluster, add-ons, secrets).
# Usage: deploy/kind/gitops-up.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
NS=ticket

helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
helm upgrade --install argocd argo/argo-cd -n argocd --create-namespace \
  -f deploy/argocd/argocd-values.yaml --wait --timeout 8m >/dev/null

# The chart was installed directly by up.sh; Argo CD takes ownership of the same
# resources (same release name, so Helm-rendered names match). The data volumes
# are kept.
if helm -n "$NS" status ticket >/dev/null 2>&1; then
  helm -n "$NS" uninstall ticket --wait >/dev/null
fi

kubectl apply -f deploy/argocd/root.yaml >/dev/null
echo "waiting for Argo CD to sync"
until [ "$(kubectl -n argocd get application ticket -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null)" = "Synced/Healthy" ]; do
  sleep 10
done
kubectl -n argocd get application ticket -o jsonpath='{.status.sync.revisions}{"\n"}'
echo "deployed image: $(kubectl -n $NS get deploy api -o jsonpath='{.spec.template.spec.containers[0].image}')"
echo "Argo CD UI: kubectl -n argocd port-forward svc/argocd-server 8443:80"
echo "  password: kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
