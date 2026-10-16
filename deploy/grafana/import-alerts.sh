#!/usr/bin/env bash
# Import deploy/grafana/alerts.yml into Grafana as Grafana-managed, page-editable alert rules.
#
# Usage: deploy/grafana/import-alerts.sh [-u GRAFANA_URL] [-f FOLDER] [-d DATASOURCE_UID] [--yes] [rules.yml]
#   GRAFANA_URL         default http://localhost:3000; or set GRAFANA_PORT_FORWARD="-n monitoring svc/grafana 3000:80"
#                       and the script port-forwards with kubectl for the duration of the import.
#   credentials         GRAFANA_TOKEN (a service account token, Editor or Admin) or GRAFANA_USER/GRAFANA_PASSWORD.
#                       Deliberately NOT read from the cluster: the admin password seeded by the chart goes
#                       stale once anyone changes it in the UI, and a stale credential "works" until it 401s.
#   FOLDER              default msvcservice (created when missing)
#   DATASOURCE_UID      default prometheus; the Prometheus rule format carries no datasource
#
# The rules go through POST /api/convert/prometheus/config/v1/rules/<folder>: Grafana translates the
# Prometheus format into its own model (threshold expressions, pending period) and, with
# X-Disable-Provenance, leaves the result editable in the page. The page's own "Import" button is
# behind a feature toggle; the API behind it is not.
#
# After the import, set every rule's "No data" handling to Normal: a counter has no series before
# its first event, Grafana reads that as NoData, and the default NoData behaviour is Alerting.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
URL=${GRAFANA_URL:-http://localhost:3000}
FOLDER=${GRAFANA_FOLDER:-msvcservice}
DATASOURCE=${GRAFANA_DATASOURCE_UID:-prometheus}
YES=false
RULES="$ROOT/alerts.yml"
while [[ $# -gt 0 ]]; do
  case "$1" in
    -u|--url) URL=$2; shift 2 ;;
    -f|--folder) FOLDER=$2; shift 2 ;;
    -d|--datasource) DATASOURCE=$2; shift 2 ;;
    --yes) YES=true; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    -*) echo "unknown option: $1" >&2; exit 2 ;;
    *) RULES=$1; shift ;;
  esac
done
[[ -f "$RULES" ]] || { echo "rules file not found: $RULES" >&2; exit 2; }
grep -qE '^(groups|name):' "$RULES" || { echo "$RULES does not look like a Prometheus rules file" >&2; exit 2; }

auth=()
if [[ -n "${GRAFANA_TOKEN:-}" ]]; then
  auth=(-H "Authorization: Bearer $GRAFANA_TOKEN")
elif [[ -n "${GRAFANA_USER:-}" && -n "${GRAFANA_PASSWORD:-}" ]]; then
  auth=(-u "$GRAFANA_USER:$GRAFANA_PASSWORD")
elif [[ -t 0 ]]; then
  read -r -s -p "Grafana service account token (enter for user/password): " GRAFANA_TOKEN; echo
  if [[ -n "$GRAFANA_TOKEN" ]]; then
    auth=(-H "Authorization: Bearer $GRAFANA_TOKEN")
  else
    read -r -p "user: " GRAFANA_USER; read -r -s -p "password: " GRAFANA_PASSWORD; echo
    auth=(-u "$GRAFANA_USER:$GRAFANA_PASSWORD")
  fi
else
  echo "no credentials: set GRAFANA_TOKEN or GRAFANA_USER/GRAFANA_PASSWORD" >&2; exit 2
fi

pf_pid=""
if [[ -n "${GRAFANA_PORT_FORWARD:-}" ]]; then
  echo "kube context: $(kubectl config current-context)"
  # shellcheck disable=SC2086
  kubectl port-forward $GRAFANA_PORT_FORWARD >/dev/null 2>&1 &
  pf_pid=$!
  trap '[[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null || true' EXIT
  sleep 2
fi

echo "importing $RULES into $URL, folder '$FOLDER', datasource '$DATASOURCE'"
if ! $YES; then
  read -r -p "continue? [y/N] " answer
  [[ "$answer" == "y" || "$answer" == "Y" ]] || exit 1
fi

curl -sS --fail-with-body -X POST "$URL/api/convert/prometheus/config/v1/rules/$FOLDER" \
  "${auth[@]}" \
  -H "Content-Type: application/yaml" \
  -H "X-Disable-Provenance: true" \
  -H "X-Grafana-Alerting-Datasource-UID: $DATASOURCE" \
  --data-binary "@$RULES"
echo

echo "rules now in folder '$FOLDER':"
curl -sS --fail-with-body "$URL/api/convert/prometheus/config/v1/rules/$FOLDER" "${auth[@]}" \
  | grep -E '^\s*- name:|^\s*- alert:' || true

echo
echo "next: Alerting → Alert rules → folder '$FOLDER' → each rule → Edit → 'Configure no data and error"
echo "handling' → No data = Normal. Counters have no series before their first event."
