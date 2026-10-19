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
# GOWORK=off: inside a workspace `go list -m` names every module in it, not just this one.
OLD_MODULE=$(cd "$TEMPLATE_DIR" && GOWORK=off go list -m)

# The service depends on the private platform kit: `go mod tidy` below fetches it unless it is
# already in the module cache, and fetching needs GOPRIVATE plus a git credential (README, "kit").
KIT=github.com/guangliangus/go-msvc-kit
if [[ -z "$(go env GOPRIVATE)" ]]; then
  echo "warning: GOPRIVATE is empty; unless $KIT is in the module cache, go mod tidy will fail." >&2
  echo "         once per machine: go env -w GOPRIVATE='github.com/guangliangus/*' && gh auth setup-git" >&2
fi

echo "generating $NAME ($MODULE) into $DEST"
mkdir -p "$DEST"
# Excluded: VCS, build output, editor state, the uncommitted workspace file — and the gitignored
# local secrets (.env, .env.*, *.local.yaml), which must never travel into a new repository.
tar -C "$TEMPLATE_DIR" --exclude .git --exclude bin --exclude dist --exclude '*.test' --exclude coverage.txt \
    --exclude coverage.html --exclude .idea --exclude .vscode --exclude go.work --exclude go.work.sum \
    --exclude .env --exclude '.env.*' --exclude '*.local.yaml' \
    -cf - . | tar -C "$DEST" -xf -

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

0. The platform kit (\`$KIT\`) is a private module. Once per machine:
   \`go env -w GOPRIVATE='github.com/guangliangus/*'\` and \`gh auth setup-git\`. Upgrade it with
   \`go get $KIT@vX.Y.Z\`; its CHANGELOG lists any DDL to copy into \`db/\` (\`go test\` checks the copies).
1. \`make up\` — PostgreSQL, Redis, RabbitMQ, WireMock, Jaeger, Prometheus, Grafana; the one-shot \`schema\` service applies \`db/*.sql\`.
2. \`make run\` — the API on :8080 (probes and /metrics on :8081) with the development overlay (header identities, text logs, /docs).
3. \`requests.http\` — walk the reference order flow; \`make test\` / \`make test-integration\` — the suites.
4. \`AGENTS.md\` / \`CLAUDE.md\` brief AI assistants on the commands, the layout and the rules; keep them current.
5. Replace the \`orders\` reference context with your own, keeping the shape. It lives in more places
   than \`internal/order\`: \`docs/replacing-the-example.md\` is the complete list in the order to do
   them (context, adapters and DDL, handlers/consumers/tasks, the composition root, config, the
   OpenAPI document, guards, the local stack, the chart, the scripts). Two of them nothing compiles
   against: the table at the top of \`tests/arch/deps_test.go\` (the guards only cover the
   directories listed there) and the chart's KEDA \`queueName\` (\`kafka.consumerGroup\` / \`topic\`
   with the Kafka trigger). Finish with
   \`tools/check-example-removed.sh\`: exit 0 means no \`order\` / \`inventory\` / \`sku\` is left.

## Deploy

- \`azure-pipelines.yml\` (import it in Azure DevOps) expects these pipeline variables: the secret
  \`GitHubKitToken\` (read-only fine-grained GitHub token for \`$KIT\`: the pipeline and the image build
  fetch the kit with it), \`containerRegistry\` (a Docker registry service connection) with
  \`registryHost\` set to that registry's host name, \`k8sConnectionDev\` / \`k8sConnectionProd\`
  (Kubernetes service connections) with \`helmNamespaceDev\` / \`helmNamespaceProd\`, and
  \`buildWorker: true\` when the service has its own worker deployment. The DDL gate needs nothing:
  it starts its own PostgreSQL on the agent.
- The chart (\`deploy/helm/$NAME\`) creates NO Secret with the dev/prod overlays. Before the first
  \`helm upgrade --install\`, create \`<release>-secrets\` in the namespace ($NAME-secrets for the
  pipeline's release name) holding \`POSTGRES__DSN\`, \`RABBITMQ__URL\` and \`AUTH__SIGNING_KEY\` — or
  set \`secret.existingName\` to the Secret you already manage. \`helm template\` refuses an empty
  chart-rendered Secret, so a misconfiguration fails at render time, not as a crash-looping pod.
- Kafka is off unless configured: \`KAFKA__ENABLED\`, \`KAFKA__BROKERS\` and \`KAFKA__EVENTS\` (the events that
  live on Kafka, a platform decision) go under the chart's \`config\`, \`KAFKA__SASL__PASSWORD\` into the
  Secret, and \`worker.keda.trigger: kafka\` (with \`worker.keda.kafka.*\`) scales the worker on consumer lag.
- Replace the \`CHANGE ME\` keys in \`deploy/helm/$NAME/values.yaml\` (Redis, OTLP collector,
  downstream base URLs, ingress host) and have the DBA run \`db/*.sql\` before the first deploy.

See \`docs/architecture.md\` for the constraints the code enforces and \`docs/known-open.md\` for what is deliberately left open.
README

go mod tidy
gofmt -w .
echo "building and testing the generated service"
go build ./...
# Quiet on success; on failure the test output is the only thing that says why, so show it then.
if ! test_output=$(go test ./... 2>&1); then
  printf '%s\n' "$test_output" >&2
  echo "go test failed in $DEST (output above)" >&2
  exit 1
fi
echo "done: $DEST"
