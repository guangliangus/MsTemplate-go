#!/usr/bin/env bash
# Import (or overwrite) deploy/grafana/dashboards/*.json into Grafana via the HTTP API.
# Usage: deploy/grafana/import-dashboard.sh [-u GRAFANA_URL] [dashboard.json ...]
#   credentials as in import-alerts.sh (GRAFANA_TOKEN or GRAFANA_USER/GRAFANA_PASSWORD).
# Overwrite replaces whatever was edited in the page: export the page's JSON back into the repo first
# (dashboard → Export → Export as JSON, without "export for sharing externally") if those edits matter.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
URL=${GRAFANA_URL:-http://localhost:3000}
files=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -u|--url) URL=$2; shift 2 ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) files+=("$1"); shift ;;
  esac
done
[[ ${#files[@]} -gt 0 ]] || files=("$ROOT"/dashboards/*.json)

auth=()
if [[ -n "${GRAFANA_TOKEN:-}" ]]; then
  auth=(-H "Authorization: Bearer $GRAFANA_TOKEN")
elif [[ -n "${GRAFANA_USER:-}" && -n "${GRAFANA_PASSWORD:-}" ]]; then
  auth=(-u "$GRAFANA_USER:$GRAFANA_PASSWORD")
else
  echo "set GRAFANA_TOKEN or GRAFANA_USER/GRAFANA_PASSWORD" >&2; exit 2
fi

for f in "${files[@]}"; do
  payload=$(python3 - "$f" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["id"] = None
print(json.dumps({"dashboard": d, "overwrite": True, "message": "imported from the repository"}))
PY
)
  echo "importing $f"
  curl -sS --fail-with-body -X POST "$URL/api/dashboards/db" "${auth[@]}" \
    -H "Content-Type: application/json" --data "$payload"
  echo
done
