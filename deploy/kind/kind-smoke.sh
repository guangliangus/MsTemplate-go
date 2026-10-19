#!/usr/bin/env bash
# K8s smoke test on kind:
#   helm lint/template → kind cluster → deps → db/*.sql applied → probes ready → graceful delete.
# Requires: docker, kind, kubectl, helm — and a credential for the private platform kit, which the
# image build reads as a BuildKit secret (one netrc line): KIT_NETRC from the environment, or
# built from `gh auth token` (README, "平台 kit"), the same as `make docker`.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLUSTER=msvc-smoke
CHART="$ROOT/deploy/helm/msvcservice"
# renovate: datasource=docker depName=curlimages/curl
CURL_VERSION=8.10.1

for tool in docker kind kubectl helm; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool"; exit 1; }
done
if [[ -z "${KIT_NETRC:-}" ]]; then
  command -v gh >/dev/null || { echo "set KIT_NETRC (one netrc line for github.com) or install the GitHub CLI"; exit 1; }
  KIT_NETRC="machine github.com login x-access-token password $(gh auth token)"
fi
export KIT_NETRC

log() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

log "helm lint + template"
helm lint "$CHART"
helm template smoke "$CHART" -f "$ROOT/deploy/kind/values-kind.yaml" > /dev/null

log "building the image"
docker build --secret id=netrc,env=KIT_NETRC -t msvcservice:smoke "$ROOT"

log "creating the kind cluster"
kind delete cluster --name $CLUSTER >/dev/null 2>&1 || true
kind create cluster --name $CLUSTER --wait 120s
cleanup() { kind delete cluster --name $CLUSTER >/dev/null 2>&1 || true; }
trap cleanup EXIT

log "loading the image into kind"
kind load docker-image msvcservice:smoke --name $CLUSTER

log "deploying dependencies (postgres/redis/rabbitmq/wiremock)"
kubectl apply -f "$ROOT/deploy/kind/dependencies.yaml"
kubectl rollout status deploy/postgres deploy/redis deploy/rabbitmq deploy/wiremock --timeout=180s

log "applying db/*.sql (the manual DDL step)"
PGPOD=$(kubectl get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}')
for f in "$ROOT"/db/*.sql; do
  echo "applying $(basename "$f")"
  kubectl exec -i "$PGPOD" -- psql -U msvc -d msvcservice -v ON_ERROR_STOP=1 -q < "$f"
done

log "helm install"
helm install smoke "$CHART" -f "$ROOT/deploy/kind/values-kind.yaml" --wait --timeout 5m

log "verifying probes on the admin port, and that the API port does not serve them"
kubectl run curl --image="curlimages/curl:$CURL_VERSION" --restart=Never --rm -i --quiet -- \
  sh -c "curl -fsS http://smoke-api:8081/health/ready && echo && curl -fsS http://smoke-worker:8081/health/ready && echo \
    && test \"\$(curl -s -o /dev/null -w '%{http_code}' http://smoke-api:8080/metrics)\" = 404 && echo READY_OK"

log "graceful shutdown: delete the api pod and watch it drain within the grace period"
POD=$(kubectl get pod -l app.kubernetes.io/component=api -o jsonpath='{.items[0].metadata.name}')
kubectl delete pod "$POD" --wait --timeout=60s
kubectl rollout status deploy/smoke-api --timeout=120s

log "KIND SMOKE PASSED"
