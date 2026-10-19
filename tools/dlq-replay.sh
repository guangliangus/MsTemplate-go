#!/usr/bin/env bash
# Replay messages from <queue>.dlq back into <queue> via the RabbitMQ management HTTP API,
# resetting the retry counter (x-attempt=0) and dropping the failure marker (x-exception).
# Works for every queue on the platform's topology: same headers whatever language declared it.
#
# NOTE: the management API "get" removes the message before the republish happens — if the
# republish fails (or the script dies between the two calls) that message is no longer in the
# DLQ. The script never lets it vanish silently: a failed or unrouted publish writes the full
# message (properties + payload, as the API returned it) to stderr AND to
# ./dlq-replay-failed-<queue>-<timestamp>.json in the current directory, then stops; republish it
# by hand from that file (management UI → queue → Publish message, or the /publish endpoint).
# Fine for day-to-day ops; for irreplaceable messages prefer the Shovel plugin.
#
# Usage: tools/dlq-replay.sh <work-queue> [count=10] [mgmt-url=http://localhost:15672] [user=guest] [pass=guest] [vhost=/]
set -euo pipefail

QUEUE=${1:?usage: dlq-replay.sh <work-queue> [count] [mgmt-url] [user] [pass] [vhost]}
COUNT=${2:-10}
URL=${3:-http://localhost:15672}
USER=${4:-guest}
PASS=${5:-guest}
VHOST=${6:-/}

python3 -I - "$QUEUE" "$COUNT" "$URL" "$USER" "$PASS" "$VHOST" <<'PY'
import base64
import json
import sys
import time
import urllib.parse
import urllib.request

queue, count, url, user, password, vhost = (
    sys.argv[1], int(sys.argv[2]), sys.argv[3].rstrip("/"), sys.argv[4], sys.argv[5], sys.argv[6])
vhost_enc = urllib.parse.quote(vhost, safe="")
dlq = f"{queue}.dlq"
auth = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()


def call(path, payload):
    request = urllib.request.Request(
        f"{url}/api{path}", data=json.dumps(payload).encode(), method="POST",
        headers={"Content-Type": "application/json", "Authorization": auth})
    with urllib.request.urlopen(request) as response:
        return json.load(response)


def preserve(message, reason):
    """The message is already gone from the DLQ: keep a copy where a human will find it."""
    name = f"dlq-replay-failed-{queue}-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    body = json.dumps(message, indent=2, ensure_ascii=False)
    with open(name, "w", encoding="utf-8") as f:
        f.write(body + "\n")
    print(f"ERROR: republish to {queue} failed ({reason}); the message was already taken from {dlq}.",
          file=sys.stderr)
    print(f"saved to ./{name}; the message was:\n{body}", file=sys.stderr)


replayed = 0
for _ in range(count):
    messages = call(f"/queues/{vhost_enc}/{urllib.parse.quote(dlq, safe='')}/get",
                    {"count": 1, "ackmode": "ack_requeue_false", "encoding": "auto"})
    if not messages:
        break
    message = messages[0]
    properties = message.get("properties", {})
    headers = properties.get("headers", {}) or {}
    headers["x-attempt"] = 0
    headers.pop("x-exception", None)
    properties["headers"] = headers
    try:
        result = call(f"/exchanges/{vhost_enc}/amq.default/publish", {
            "properties": properties,
            "routing_key": queue,
            "payload": message["payload"],
            "payload_encoding": message.get("payload_encoding", "string"),
        })
    except Exception as exc:  # noqa: BLE001 - whatever failed, the copy must be written first
        preserve(message, repr(exc))
        raise
    # The API answers 200 with routed=false when no queue took the message (deleted, renamed):
    # that is a loss too, not a success.
    if not result.get("routed", False):
        preserve(message, "routed=false: no queue named like that is bound to the default exchange")
        sys.exit(1)
    replayed += 1

print(f"replayed {replayed} message(s) from {dlq} to {queue}")
PY
