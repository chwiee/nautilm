#!/usr/bin/env bash
# Local cluster for nautilm: kind + Postgres (PVC) + Valkey + Entra mock + nautilm.
# Run from the repository root:  bash deployments/local-kind/up.sh
# Tear down:                      kind delete cluster --name nautilm-lab
set -euo pipefail
cd "$(dirname "$0")/../.."
K="kubectl --context kind-nautilm-lab"

kind get clusters | grep -qx nautilm-lab || kind create cluster --config deployments/local-kind/kind.yaml

docker build -q -t nautilm:dev .
docker build -q -f examples/mock-entra/Dockerfile -t mock-entra:dev .
kind load docker-image nautilm:dev mock-entra:dev --name nautilm-lab

$K create namespace nautilm --dry-run=client -o yaml | $K apply -f -

# Secrets: generated here, only in the cluster (never in git). Kept across runs.
gen() { head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32; }
if ! $K -n nautilm get secret postgres-auth >/dev/null 2>&1; then
  PG="$(gen)"
  $K -n nautilm create secret generic postgres-auth --from-literal=password="$PG"
  $K -n nautilm create secret generic nautilm-secrets \
    --from-literal=NAUTILM_DATABASE_URL="postgres://nautilm:${PG}@postgres:5432/nautilm?sslmode=disable" \
    --from-literal=NAUTILM_ENTRA_CLIENT_SECRET="$(gen)" \
    --from-literal=NAUTILM_DASHBOARD_TOKEN="$(gen)"
fi

$K apply -f deployments/local-kind/postgres.yaml -f deployments/local-kind/services.yaml
$K -n nautilm rollout restart deployment/nautilm deployment/mock-entra >/dev/null
$K -n nautilm rollout status statefulset/postgres --timeout=180s
$K -n nautilm rollout status deployment/nautilm --timeout=240s
echo
echo "nautilm:    http://localhost:18095/dashboard   (mock sign-in: ana = admin, bruno = viewer)"
echo "mock Entra: http://localhost:18096"
