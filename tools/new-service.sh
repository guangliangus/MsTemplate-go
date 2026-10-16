#!/usr/bin/env bash
# Generates a new service from this template.
#
# Usage: tools/new-service.sh <name> <module-path> [destination]
#   name         the service's short name, lower-case, letters/digits/dashes (orders, payment-svc)
#   module-path  the Go module path (github.com/acme/orders, dev.azure.com/acme/platform/_git/orders.git)
#   destination  directory to create (default: ../<name>)
#
# What it does: copies this repository (minus .git and build output), rewrites the module path,
# renames the placeholder tokens (msvcservice / MsvcService) to the new name, re-tidies the
# module and runs the tests. Capabilities are NOT pruned at generation time: messaging and the
# task queue are switched off per deployment by configuration (rabbitmq.enabled,
# tasks.max_workers), which is a Go binary's natural shape — everything links, unused parts idle.
set -euo pipefail

NAME=${1:?usage: new-service.sh <name> <module-path> [destination]}
MODULE=${2:?usage: new-service.sh <name> <module-path> [destination]}
TEMPLATE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DEST=${3:-"$TEMPLATE_DIR/../$NAME"}

if [[ ! "$NAME" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "name must be lower-case letters, digits and dashes, starting with a letter: $NAME" >&2
  exit 2
fi
if [[ -e "$DEST" ]]; then
  echo "destination already exists: $DEST" >&2
  exit 2
fi

# PascalCase for display names: payment-svc -> PaymentSvc
PASCAL=$(echo "$NAME" | awk -F- '{for (i = 1; i <= NF; i++) printf "%s%s", toupper(substr($i, 1, 1)), substr($i, 2)}')
OLD_MODULE=$(cd "$TEMPLATE_DIR" && go list -m)

echo "generating $NAME ($MODULE) into $DEST"
mkdir -p "$DEST"
tar -C "$TEMPLATE_DIR" --exclude .git --exclude bin --exclude dist --exclude '*.test' --exclude coverage.txt \
    --exclude coverage.html --exclude .idea --exclude .vscode -cf - . | tar -C "$DEST" -xf -

cd "$DEST"

# 1. the module path in go.mod and every import. Only the whole path: one that runs on from a host
#    name (registry.example.com/msvcservice, the chart's image) is not the module and is left alone.
go mod edit -module "$MODULE"
grep -rl --exclude-dir=.git "$OLD_MODULE" . | while read -r f; do
  perl -pi -e "s#(?<![A-Za-z0-9.-])\Q$OLD_MODULE\E#$MODULE#g" "$f"
done

# 2. the service name tokens: directories first, then file contents
if [[ -d cmd/msvcservice ]]; then mv cmd/msvcservice "cmd/$NAME"; fi
if [[ -d deploy/helm/msvcservice ]]; then mv deploy/helm/msvcservice "deploy/helm/$NAME"; fi
grep -rl --exclude-dir=.git -e msvcservice -e MsvcService . | while read -r f; do
  perl -pi -e "s/msvcservice/$NAME/g; s/MsvcService/$PASCAL/g" "$f"
done

# 3. a fresh README stub replaces the template's own
cat > README.md <<README
# $PASCAL

Generated from the Go microservice template. First steps:

1. \`make up\` — PostgreSQL, Redis, RabbitMQ, WireMock, Jaeger, Prometheus, Grafana; the one-shot \`schema\` service applies \`db/*.sql\`.
2. \`make run\` — the API on :8080 (probes and /metrics on :8081) with the development overlay (header identities, text logs, /docs).
3. \`requests.http\` — walk the reference order flow; \`make test\` / \`make test-integration\` — the suites.
4. \`AGENTS.md\` / \`CLAUDE.md\` brief AI assistants on the commands, the layout and the rules; keep them current.
5. Replace the \`orders\` reference context (\`internal/order\`, \`internal/postgres\`, \`internal/inventory\`, \`internal/httpapi\`, \`db/0001_init.sql\`) with your own; keep the shape.

See \`docs/architecture.md\` for the constraints the code enforces and \`docs/known-open.md\` for what is deliberately left open.
README

go mod tidy
gofmt -w .
echo "building and testing the generated service"
go build ./... && go test ./... >/dev/null
echo "done: $DEST"
