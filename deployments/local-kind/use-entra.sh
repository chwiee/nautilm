#!/usr/bin/env bash
# Switch the local lab from the Entra MOCK to a REAL Entra ID tenant.
#
#   bash deployments/local-kind/use-entra.sh <tenant-id> <client-id> [admin-group-id] [viewer-group-id]
#
# Before: App Registration with redirect URI http://localhost:18095/auth/callback
# and a groups claim; its client secret in the cluster Secret (never in git):
#   read -rs S && kubectl --context kind-nautilm-lab -n nautilm patch secret nautilm-secrets \
#     --type merge -p "{\"stringData\":{\"NAUTILM_ENTRA_CLIENT_SECRET\":\"$S\"}}"; unset S
#
# Without the group IDs yet: sign in anyway; the "Access denied" page lists
# the group IDs of your sign-in, then run this again with them.
# Back to the mock: bash deployments/local-kind/up.sh
set -euo pipefail
K="kubectl --context kind-nautilm-lab -n nautilm"
uuid='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
TENANT="${1:?tenant id}" CLIENT="${2:?client id}" ADMIN="${3:-not-mapped-yet}" VIEWER="${4:-not-mapped-yet}"
for v in "$TENANT" "$CLIENT"; do [[ $v =~ $uuid ]] || { echo "not a GUID: $v" >&2; exit 1; }; done

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
$K get configmap nautilm -o jsonpath='{.data.config\.yaml}' > "$tmp"
sed -i -E \
  -e "s|^( *tenant_id:).*|\1 $TENANT|" \
  -e "s|^( *client_id:).*|\1 $CLIENT|" \
  -e "s|^( *admin_groups:).*|\1 [$ADMIN]|" \
  -e "s|^( *viewer_groups:).*|\1 [$VIEWER]|" \
  -e "/^ *issuer_url:/d" "$tmp"
$K create configmap nautilm --from-file=config.yaml="$tmp" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K rollout restart deployment/nautilm >/dev/null
$K rollout status deployment/nautilm --timeout=180s
echo
echo "Real Entra ID: tenant $TENANT, app $CLIENT, admin group $ADMIN, viewer group $VIEWER"
echo "Sign in at http://localhost:18095/dashboard (Microsoft's own page; your credentials never pass through nautilm)."
