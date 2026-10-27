#!/usr/bin/env bash
# nautilm DEMO on an existing EKS (one namespace, no disks). Read README.md first.
#
#   IMAGE=<account>.dkr.ecr.<region>.amazonaws.com/nautilm:<tag> REGION=us-east-2 \
#   ENTRA_TENANT_ID=<guid> ENTRA_CLIENT_ID=<guid> ENTRA_ADMIN_GROUP=<guid> ENTRA_VIEWER_GROUP=<guid> \
#   bash deployments/demo-eks/up.sh
#
# Optional: KUBE_CONTEXT (default: current), FAST_MODEL, SMART_MODEL, EMBED_MODEL.
# The Entra client secret is read without echo the first time (never an argument).
# Remove everything: kubectl delete namespace nautilm-demo && kubectl delete clusterrolebinding nautilm-demo-auth-delegator
set -euo pipefail
cd "$(dirname "$0")"

: "${IMAGE:?set IMAGE (ECR image of nautilm)}" "${REGION:?set REGION (e.g. us-east-2)}"
: "${ENTRA_TENANT_ID:?}" "${ENTRA_CLIENT_ID:?}" "${ENTRA_ADMIN_GROUP:?}"
ENTRA_VIEWER_GROUP="${ENTRA_VIEWER_GROUP:-$ENTRA_ADMIN_GROUP}"
FAST_MODEL="${FAST_MODEL:-amazon.nova-lite-v1:0}"
SMART_MODEL="${SMART_MODEL:-amazon.nova-pro-v1:0}"
EMBED_MODEL="${EMBED_MODEL:-amazon.titan-embed-text-v2:0}"
K="kubectl ${KUBE_CONTEXT:+--context $KUBE_CONTEXT}"
NS=nautilm-demo

echo "cluster: $($K config current-context)  ·  region: $REGION  ·  image: $IMAGE"

# 1. Render the manifest (placeholders only; secrets never go into it).
rendered="$(mktemp)"
trap 'rm -f "$rendered"' EXIT
sed -e "s|__IMAGE__|$IMAGE|g" -e "s|__REGION__|$REGION|g" \
    -e "s|__FAST_MODEL__|$FAST_MODEL|g" -e "s|__SMART_MODEL__|$SMART_MODEL|g" -e "s|__EMBED_MODEL__|$EMBED_MODEL|g" \
    -e "s|__TENANT_ID__|$ENTRA_TENANT_ID|g" -e "s|__CLIENT_ID__|$ENTRA_CLIENT_ID|g" \
    -e "s|__ADMIN_GROUP__|$ENTRA_ADMIN_GROUP|g" -e "s|__VIEWER_GROUP__|$ENTRA_VIEWER_GROUP|g" \
    nautilm-demo.yaml > "$rendered"
if grep -q "__[A-Z_]*__" "$rendered"; then echo "unfilled placeholder: $(grep -o '__[A-Z_]*__' "$rendered" | sort -u)" >&2; exit 1; fi

$K create namespace $NS --dry-run=client -o yaml | $K apply -f - >/dev/null

# 2. Secrets: generated here, only in the cluster. Kept across runs (the
#    Postgres has no disk, but the password stays the same).
gen() { head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32; }
if ! $K -n $NS get secret postgres-auth >/dev/null 2>&1; then
  read -rsp "Entra client secret (Value, not Secret ID) — input hidden: " ENTRA_SECRET; echo
  [ -n "$ENTRA_SECRET" ] || { echo "empty client secret" >&2; exit 1; }
  PG="$(gen)"
  $K -n $NS create secret generic postgres-auth --from-literal=password="$PG" >/dev/null
  $K -n $NS create secret generic nautilm-secrets \
    --from-literal=NAUTILM_DATABASE_URL="postgres://nautilm:${PG}@postgres:5432/nautilm?sslmode=disable" \
    --from-literal=NAUTILM_ENTRA_CLIENT_SECRET="$ENTRA_SECRET" \
    --from-literal=NAUTILM_DASHBOARD_TOKEN="$(gen)" \
    --from-literal=NAUTILM_TOKEN_K8S="$(gen)" --from-literal=NAUTILM_TOKEN_MID="$(gen)" \
    --from-literal=NAUTILM_TOKEN_MARCIA="$(gen)" --from-literal=NAUTILM_TOKEN_CLAUDIA="$(gen)" >/dev/null
  unset ENTRA_SECRET PG
  echo "secrets created (only in the cluster)"
fi

# 3. Apply and wait.
$K apply -f "$rendered" >/dev/null
$K -n $NS rollout restart deployment/nautilm >/dev/null
$K -n $NS rollout status deployment/valkey --timeout=180s
$K -n $NS rollout status deployment/postgres --timeout=180s
$K -n $NS rollout status deployment/nautilm --timeout=300s

cat <<EOF

nautilm demo is up in namespace $NS.
  Dashboard:   kubectl -n $NS port-forward svc/nautilm 18095:80   →  http://localhost:18095/dashboard
  Logs:        kubectl -n $NS logs deploy/nautilm
  Pod Identity association needed for Bedrock (see README): namespace $NS, service account nautilm.
EOF
