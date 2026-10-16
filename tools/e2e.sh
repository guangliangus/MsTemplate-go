#!/usr/bin/env bash
# End-to-end acceptance against the compose stack:
#   probes & /metrics on the admin port (and not on the API port) → token minted like user-svc's → 401 / 403 / forged-key 401 → create order
#   → outbox → RabbitMQ → consumer confirms → 400 / 422 error contract → idempotent replay
#   → rate limit (429) → circuit breaker (502 via the WireMock FAULT SKU) → RabbitMQ restart,
#   automatic reconnect, one more order.
# Usage: tools/e2e.sh [--keep] [--no-app]
#   --keep    leaves the stack running afterwards
#   --no-app  does not build/start the containerised service: the API is expected to be running
#             already (e.g. `make run` on the host: API=http://localhost:8080 and
#             ADMIN=http://localhost:8081), only the
#             infrastructure comes from compose
set -euo pipefail
KEEP=false; NO_APP=false
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=true ;;
    --no-app) NO_APP=true ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
API=${API:-http://localhost:${API_HOST_PORT:-8080}}
ADMIN=${ADMIN:-http://localhost:${ADMIN_HOST_PORT:-8081}}
MINT="$ROOT/tools/mint-dev-token.sh"
export POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-15432}
export REDIS_HOST_PORT=${REDIS_HOST_PORT:-16379}
COMPOSE="docker compose --profile app"
if $NO_APP; then COMPOSE="docker compose"; fi

log() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; $COMPOSE logs --tail 40 api || true; exit 1; }
json() { python3 -c "import sys,json;print(json.load(sys.stdin)$1)"; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

wait_http() { # url, timeout_s
  local url=$1 timeout=${2:-120} start=$SECONDS
  while (( SECONDS - start < timeout )); do
    if curl -fsS --max-time 3 "$url" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  return 1
}

# No --wait: it reports the one-shot schema service's normal exit as a failure. Readiness is
# polled below instead, which is the check that matters.
if $NO_APP; then
  log "compose up (infrastructure only; the API is expected at $API)"
  $COMPOSE up -d
else
  log "compose up (--build so the run always tests the CURRENT code)"
  $COMPOSE up -d --build --quiet-pull
fi
docker compose wait schema >/dev/null || fail "db/*.sql could not be applied (docker compose logs schema)"

log "waiting for API readiness (db/*.sql applied by the schema service first)"
wait_http "$ADMIN/health/ready" 240 || fail "API never became ready"

log "probes + /metrics on the admin listener, never on the API listener"
curl -fsS "$ADMIN/health/startup" >/dev/null && curl -fsS "$ADMIN/health/live" >/dev/null || fail "probes"
METRICS=$(mktemp); curl -fsS "$ADMIN/metrics" -o "$METRICS" || fail "metrics endpoint"
grep -q "msvc_tasks_queue_depth\|go_goroutines" "$METRICS" || fail "metrics body lacks the expected series"
[ "$(status "$API/metrics")" = 404 ] && [ "$(status "$API/health/ready")" = 404 ] || fail "the API port serves operational endpoints"

log "minting a consumer token the way user-svc signs one"
TOKEN=$("$MINT" --sub 1 --realm CONSUMER --sid e2e)
AUTH=(-H "Authorization: Bearer $TOKEN")

log "unauthenticated → 401, back-office token on the consumer plane → 403, forged key → 401"
[ "$(status "$API/api/v1/orders")" = 401 ] || fail "expected 401"
BACKOFFICE=$("$MINT" --sub 1 --realm BACKOFFICE --scope orders.read)
[ "$(status "$API/api/v1/orders" -H "Authorization: Bearer $BACKOFFICE")" = 403 ] || fail "expected 403 for the wrong plane"
FORGED=$("$MINT" --sub 1 --realm CONSUMER --key "$(python3 -c 'import base64,os;print(base64.b64encode(os.urandom(32)).decode())')")
[ "$(status "$API/api/v1/orders" -H "Authorization: Bearer $FORGED")" = 401 ] || fail "expected 401 for a forged token"
[ "$(status "$API/api/v1/backoffice/orders" -H "Authorization: Bearer $BACKOFFICE")" = 200 ] || fail "back-office plane"
curl -fsS "$API/api/v1/backoffice/orders?limit=1" -H "Authorization: Bearer $BACKOFFICE" | json '["hasMore"]' >/dev/null \
  || fail "back-office list is a cursor page"

log "create order (API → inventory prices it via WireMock → outbox → RabbitMQ → consumer confirms)"
KEY="e2e-$(date +%s)"
ORDER=$(curl -fsS -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" \
  -d '{"items":[{"sku":"SKU-E2E","quantity":2}]}')
ORDER_ID=$(echo "$ORDER" | json '["id"]')
TOTAL=$(echo "$ORDER" | json '["totalAmount"]')
echo "order id=$ORDER_ID total=$TOTAL"
[ "$TOTAL" = "39.98" ] || fail "the inventory service prices the line (2 x 19.99), as a JSON number: $ORDER"

log "idempotent replay with the same key"
REPLAY_HEADERS=$(curl -s -D - -o /dev/null -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" -d '{"items":[{"sku":"SKU-E2E","quantity":2}]}')
echo "$REPLAY_HEADERS" | grep -qi "Idempotency-Replayed: true" || fail "replay not marked"
echo "$REPLAY_HEADERS" | grep -qi "X-Trace-ID:" || fail "X-Trace-ID header missing"

log "waiting for the consumer to confirm (event round trip)"
for _ in $(seq 1 30); do
  STATUS=$(curl -fsS "$API/api/v1/orders/$ORDER_ID" "${AUTH[@]}" | json '["status"]')
  [ "$STATUS" = "CONFIRMED" ] && break
  sleep 2
done
[ "${STATUS:-}" = "CONFIRMED" ] || fail "order stayed ${STATUS:-?} (event chain broken)"
echo "order CONFIRMED ✓"

log "error contract: 400 validation, 422 business rule, 404, 409"
BODY=$(curl -s -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{"items":[{"sku":"","quantity":0}]}')
[ "$(echo "$BODY" | json '["errorCode"]')" = "VALIDATION_FAILED" ] || fail "400 contract: $BODY"
BODY=$(curl -s -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{"items":[{"sku":"OUT-OF-STOCK","quantity":1}]}')
[ "$(echo "$BODY" | json '["errorCode"]')" = "ORDER_ITEM_UNAVAILABLE" ] || fail "422 contract: $BODY"
[ "$(status "$API/api/v1/orders/999999" "${AUTH[@]}")" = 404 ] || fail "404"
[ "$(status -X DELETE "$API/api/v1/orders/$ORDER_ID" "${AUTH[@]}")" = 409 ] || fail "409 on cancelling a confirmed order"

log "rate limit: the 'writes' policy (20 / 10s) trips with a burst"
CODES=$(for _ in $(seq 1 25); do status -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"items":[{"sku":"SKU-RL","quantity":1}]}'; echo; done | sort | uniq -c | tr '\n' ' ')
echo "$CODES"; echo "$CODES" | grep -q 429 || fail "no 429 in the burst"

log "circuit breaker: the FAULT SKU resets the connection; after 5 failures the breaker opens"
sleep 10 # let the write budget refill
for _ in $(seq 1 6); do
  LAST=$(curl -s -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{"items":[{"sku":"FAULT","quantity":1}]}')
done
[ "$(echo "$LAST" | json '["errorCode"]')" = "UPSTREAM_SERVICE_UNAVAILABLE" ] || fail "502 contract: $LAST"
echo "$LAST" | grep -q "circuit breaker is open" || fail "breaker did not open: $LAST"

log "RabbitMQ restart: the service reconnects and the next order still gets confirmed"
docker compose restart rabbitmq >/dev/null
# The breaker opened above stays open for service_clients.inventory.breaker.open_duration (30s)
# and the write budget refills over 10s, so the next order is retried until it is accepted.
ORDER_ID=""
for _ in $(seq 1 40); do
  RESP=$(curl -s -w '\n%{http_code}' -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' \
    -d '{"items":[{"sku":"SKU-AFTER","quantity":1}]}')
  if [ "$(echo "$RESP" | tail -1)" = 201 ]; then ORDER_ID=$(echo "$RESP" | sed '$d' | json '["id"]'); break; fi
  sleep 2
done
[ -n "$ORDER_ID" ] || fail "no order was accepted within 80s after the broker restart"
for _ in $(seq 1 45); do
  STATUS=$(curl -fsS "$API/api/v1/orders/$ORDER_ID" "${AUTH[@]}" | json '["status"]')
  [ "$STATUS" = "CONFIRMED" ] && break
  sleep 2
done
[ "${STATUS:-}" = "CONFIRMED" ] || fail "order after the broker restart stayed ${STATUS:-?}"
echo "reconnect ✓"

log "E2E PASSED"
if ! $KEEP; then
  $COMPOSE down -v >/dev/null
fi
