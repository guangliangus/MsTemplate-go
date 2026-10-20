#!/usr/bin/env bash
# Mints an access token shaped exactly like the ones the platform's user service (user-svc) signs:
# HS256 over the shared key, iss "user-svc", claims sub / realm / sid / scope / jti by their wire
# names. For local runs, because there is no local issuer anywhere and a service that validates
# tokens locally needs nothing more than a token.
#
# Usage:
#   tools/mint-dev-token.sh [--sub 1] [--realm CONSUMER|BACKOFFICE] [--sid <session id>]
#                           [--scope "orders.read"] [--ttl <seconds>] [--issuer user-svc] [--key <base64>]
#
# The default key is the throwaway one in internal/config/development.jsonc. Override it with
# --key or AUTH_SIGNING_KEY to mint against another deployment; a token for production needs
# production's key, which this script must never see.
set -euo pipefail

SUB=1
REALM=CONSUMER
SID=""
SCOPE=""
TTL=3600
ISSUER="${AUTH_ISSUER:-user-svc}"
KEY="${AUTH_SIGNING_KEY:-ZGV2LW9ubHktYWNjZXNzLXRva2VuLWtleS0zMmJ5dGU=}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sub) SUB=$2; shift 2 ;;
    --realm) REALM=$2; shift 2 ;;
    --sid) SID=$2; shift 2 ;;
    --scope) SCOPE=$2; shift 2 ;;
    --ttl) TTL=$2; shift 2 ;;
    --issuer) ISSUER=$2; shift 2 ;;
    --key) KEY=$2; shift 2 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

python3 -I - "$SUB" "$REALM" "$SID" "$SCOPE" "$TTL" "$ISSUER" "$KEY" <<'PY'
import base64, hashlib, hmac, json, sys, time, uuid

sub, realm, sid, scope, ttl, issuer, key_b64 = sys.argv[1:8]

def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

key = base64.b64decode(key_b64)
if len(key) < 32:
    sys.exit("the signing key must decode to at least 32 bytes")

now = int(time.time())
claims = {
    "sub": sub,
    "realm": realm,
    "iss": issuer,
    "jti": uuid.uuid4().hex,
    "iat": now,
    "nbf": now,
    "exp": now + int(ttl),
}
if sid:
    claims["sid"] = sid
if scope:
    claims["scope"] = scope

header = b64url(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
payload = b64url(json.dumps(claims, separators=(",", ":")).encode())
signature = hmac.new(key, f"{header}.{payload}".encode(), hashlib.sha256).digest()
print(f"{header}.{payload}.{b64url(signature)}")
PY
