#!/usr/bin/env bash
# Replay messages from <queue>.dlq back into <queue> via the RabbitMQ management HTTP API,
# resetting the retry counter (x-attempt=0) and dropping the failure marker (x-exception).
# Works for every queue on the platform's topology: same headers whatever language declared it.
#
# NOTE: the management API "get" removes the message before the republish happens — if the
# script dies between the two calls that one message is lost. Fine for day-to-day ops; for
# irreplaceable messages prefer the Shovel plugin.
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
    call(f"/exchanges/{vhost_enc}/amq.default/publish", {
        "properties": properties,
        "routing_key": queue,
        "payload": message["payload"],
        "payload_encoding": message.get("payload_encoding", "string"),
    })
    replayed += 1

print(f"replayed {replayed} message(s) from {dlq} to {queue}")
PY
