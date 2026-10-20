#!/usr/bin/env bash
# End-to-end acceptance against the compose stack:
#   probes & /metrics on the admin port (and not on the API port) → token minted like user-svc's → 401 / 403 / forged-key 401 → create order
#   → outbox → RabbitMQ → consumer confirms → 400 / 422 error contract → idempotent create (same key: 200, same order;
#   reused for another basket: 422)
#   → rate limit (429) → circuit breaker (502 via the WireMock FAULT SKU) → RabbitMQ restart,
#   automatic reconnect, one more order.
# Usage: tools/e2e.sh [--keep] [--no-app [--up]]
#   --keep    leaves the stack running afterwards (only matters for a stack this run started)
#   --no-app  does not build/start the containerised service: the API is expected to be running
#             already (e.g. `make run` on the host: API=http://localhost:8080 and
#             ADMIN=http://localhost:8081) on top of the compose infrastructure `make up` started.
#             The running stack is NOT touched (no `up`, so PostgreSQL/Redis are never recreated
#             under the host process) and is not torn down afterwards.
#   --up      with --no-app: start whatever part of the infrastructure is missing
#             (`up -d --no-recreate`, running containers stay as they are); torn down at the end
#             unless --keep.
# Host ports: the compose defaults (5432/6379/...) unless POSTGRES_HOST_PORT etc. are set in the
# environment — the same variables `make up` honours, so this script and the host app agree.
set -euo pipefail
KEEP=false; NO_APP=false; UP=false
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=true ;;
    --no-app) NO_APP=true ;;
    --up) UP=true ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done
if $UP && ! $NO_APP; then echo "--up only makes sense with --no-app" >&2; exit 2; fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
API=${API:-http://localhost:${API_HOST_PORT:-8080}}
ADMIN=${ADMIN:-http://localhost:${ADMIN_HOST_PORT:-8081}}
MINT="$ROOT/tools/mint-dev-token.sh"
COMPOSE="docker compose --profile app"
if $NO_APP; then COMPOSE="docker compose"; fi
# What this run brought up is what it tears down: never the stack the host app is sitting on.
STARTED=false

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
if $NO_APP && $UP; then
  log "compose up --no-recreate (missing infrastructure only; the API is expected at $API)"
  $COMPOSE up -d --no-recreate
  STARTED=true
elif $NO_APP; then
  log "using the running compose infrastructure as it is (the API is expected at $API)"
else
  log "compose up (--build so the run always tests the CURRENT code)"
  # The image build fetches the private platform kit; its credential is one netrc line (compose
  # passes it as a build secret). From the environment in CI, from the GitHub CLI locally.
  KIT_NETRC=${KIT_NETRC:-"machine github.com login x-access-token password $(gh auth token)"}
  export KIT_NETRC
  $COMPOSE up -d --build --quiet-pull
  STARTED=true
fi
# The one-shot schema service must have exited 0. `docker compose wait` only sees a container that
# is still running, so it is used while one is; a schema run that finished before this script
# started (make up earlier, the --no-app case) is read back from its recorded exit code instead.
schema_applied() {
  local state
  state=$(docker compose ps -a --format '{{.State}} {{.ExitCode}}' schema 2>/dev/null)
  case "$state" in
    running*) docker compose wait schema >/dev/null ;;
    "exited 0") return 0 ;;
    *) return 1 ;;
  esac
}
schema_applied \
  || fail "db/*.sql not applied: no running stack (make up, or --no-app --up) or the schema service failed (docker compose logs schema)"

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

log "idempotent create: the same key answers 200 with the same order; reused for another basket, 422"
REPLAY_HEADERS=$(mktemp)
REPLAY=$(curl -fsS -D "$REPLAY_HEADERS" -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" -d '{"items":[{"sku":"SKU-E2E","quantity":2}]}')
head -1 "$REPLAY_HEADERS" | grep -q " 200" || fail "a repeated create is a 200: $(head -1 "$REPLAY_HEADERS")"
grep -qi "X-Trace-ID:" "$REPLAY_HEADERS" || fail "X-Trace-ID header missing"
rm -f "$REPLAY_HEADERS"
[ "$(echo "$REPLAY" | json '["id"]')" = "$ORDER_ID" ] || fail "the repeat answered with another order: $REPLAY"
[ "$(status -X POST "$API/api/v1/orders" "${AUTH[@]}" -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" \
  -d '{"items":[{"sku":"SKU-E2E","quantity":3}]}')" = 422 ] || fail "a key reused for another basket is a 422"

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
if $STARTED && ! $KEEP; then
  $COMPOSE down -v >/dev/null
fi
