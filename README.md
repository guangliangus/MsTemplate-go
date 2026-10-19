# Go 微服务脚手架模板（API + Worker，Kubernetes 部署）

一个**可直接运行**的参考服务，同时就是模板：`tools/new-service.sh` 复制并改名即得到新服务。基础设施开箱即用，开发者只写业务代码。按 Go 生态的惯例组织（标准库优先、`net/http` 中间件、按限界上下文分包、接口定义在使用方、组合根显式装配），只在**平台互通契约**上与平台现有服务（含 C# 服务与 user-svc）保持一致：错误契约（`errorCode` / `traceId` / `urn:msvc:error:`）、消息线格式（AMQP 头、队列命名；Kafka 的 topic、key、头、consumer group）、指标名、令牌格式。契约清单见 `docs/architecture.md`。

- **技术栈**：Go 1.26+ · chi v5（纯 `net/http`）· PostgreSQL + pgx v5（原生 SQL，无 ORM）· Redis + go-redis v9 · RabbitMQ 官方 amqp091-go · Kafka franz-go（可选，按事件与 RabbitMQ 并存）· OpenTelemetry（traces OTLP 推送 / metrics Prometheus 抓取）· `log/slog` JSON 日志 · koanf 配置（内嵌 YAML 默认值 + `SECTION__KEY` 环境变量）· golang-jwt v5 HS256（user-svc 签发的令牌本地验签）· gobreaker 熔断 · 数据库任务队列（间隔 + cron + misfire 策略）· Testcontainers 集成测试
- **交付物**：一个二进制、两种形态（`serve` / `worker`，同一镜像）· 平台基础包 kit（独立的私有 Go 模块 `github.com/guangliangus/go-msvc-kit`，按版本依赖）· Orders 端到端示例 · docker-compose 全家桶 · Helm Chart · Azure Pipelines · 生成脚本

## 仓库结构

```
cmd/msvcservice/           main：serve | worker | version
internal/
  app/                     组合根：读配置、显式装配、注册生命周期组件（没有 DI 容器）；messaging.go 按事件把 outbox 与订阅分给 RabbitMQ / Kafka
  config/                  类型化配置 + 内嵌 default.yaml / development.yaml
  order/                   示例限界上下文：聚合（不变量、状态机、错误码）+ 用例服务 + 端口接口 + 集成事件
    ordertest/             端口的内存假实现 + NewService 一行装配；handler / consumer / task 的测试共用
  postgres/                order 端口的仓储实现（原生 SQL、ctx 事务、xmin 乐观并发、审计列）
  inventory/               下游库存服务的 HTTP 适配器（实现 order 的端口，走弹性客户端）
  httpapi/                 路由（中间件顺序）、handler、请求/响应类型、错误文案目录、路由↔OpenAPI 守卫；/docs 挂 kit 的 httpx.Docs（只在开发环境）
  consumers/               事件消费者（inbox 事务内运行）
  tasks/                   后台任务（recurring + cron 示例）
db/                        手写幂等 DDL（schema 唯一真相，应用永不改库）；kit_*.sql 是 kit 的表的副本
api/openapi.yaml           手写 OpenAPI 3.1，开发环境在 /docs 以 Scalar 渲染
deploy/helm/msvcservice/   Helm Chart（api + worker 两个 Deployment、HPA/PDB/KEDA/ServiceMonitor/Ingress/NetworkPolicy）
deploy/grafana/            看板 JSON、告警规则（含路由标签与 runbook）、导入脚本；compose 与集群共用这一份
deploy/compose/            本地栈的 Prometheus / Grafana provisioning、WireMock 库存桩
deploy/kind/               kind 冒烟
tests/arch/                架构守卫：依赖方向（表驱动）、DDL 规范（kit 的 ddlguard）、kit 表副本与 kit 一致（go test 即跑）；路由↔OpenAPI 的守卫在 internal/httpapi
tests/integration/         Testcontainers：本服务自己的仓储与事务（-tags=integration；kit 的在 kit 仓库）
tools/                     new-service.sh · check-example-removed.sh · mint-dev-token.sh · dlq-replay.sh · e2e.sh · go.mod（CI 工具版本，`go tool -modfile=tools/go.mod`）
docs/                      architecture.md（代码强制的约束）· known-open.md（有意留着不做的事）· replacing-the-example.md（怎么删掉 orders 示例）
```

## 平台 kit

平台基础包是独立的私有模块 [`github.com/guangliangus/go-msvc-kit`](https://github.com/guangliangus/go-msvc-kit)：错误契约、认证、中间件、配置、日志与遥测、PostgreSQL / Redis / RabbitMQ / Kafka 封装、outbox / inbox、任务队列、优雅停机、DDL 规范守卫（`ddlguard`）。服务按版本依赖它（`go.mod` 的 `require`）；包列表、发版规则、平台契约见 kit 的 README。

kit 仓库里是**三个 module**：根模块（服务代码依赖的全部包）、`testutil`（`github.com/guangliangus/go-msvc-kit/testutil`，Testcontainers 起真依赖并执行 `db/*.sql`，只给集成测试用，单独 tag `testutil/vX.Y.Z`，服务要单独 `go get`）、以及只在 kit 仓库内自测的 `tests/integration`（不发布、不可 import）。本服务的集成测试 import `testutil`，所以 `go.mod` 要分别 require 根模块和 `testutil`。

每台机器配一次：

```bash
go env -w GOPRIVATE='github.com/guangliangus/*'   # 私有模块不经公共代理与校验库
gh auth setup-git                                  # git 用 gh 的凭证访问 github.com
```

- **升级**：`go get github.com/guangliangus/go-msvc-kit@vX.Y.Z github.com/guangliangus/go-msvc-kit/testutil@testutil/vX.Y.Z && go mod tidy`。kit 的 `CHANGELOG.md` 写明不兼容改动和 **DDL**；表结构有变化时 `go test` 会因 `db/kit_*.sql` 副本过期而失败，失败信息里带复制命令，复制后交 DBA 执行（`db/README.md`）。当前的 `kit_0001_messaging.sql` 给 `outbox_messages` 加了 `next_attempt_at`、`partition_key` 两列和新索引：早于它建的本地库要么 `make down && make up` 重建，要么按 `db/README.md` 手动 `ALTER` 后重跑脚本。
- **同时改 kit 和服务**：`go work init . ../go-msvc-kit ../go-msvc-kit/testutil`（`go.work` 不提交）；不要在 `go.mod` 里提交 `replace`。
- **流水线与镜像**：Azure Pipelines 用 secret 变量 `GitHubKitToken`（只读 fine-grained token：只授权 go-msvc-kit，Contents: Read）；镜像构建以 BuildKit secret 拿凭证，不进镜像层也不进历史，Build 阶段结束时删掉 agent 上的 `.netrc`。Build 阶段就构建镜像（不推送）并 lint/渲染 chart，合并前即可发现 Dockerfile 或 chart 坏了；Package 阶段先 Trivy 扫本地 tag，通过才推送。本机的 `make docker`、`make up-app`、`tools/e2e.sh`、`deploy/kind/kind-smoke.sh` 从 `gh auth token` 取，`KIT_NETRC` 可覆盖。

## 使用模板

```bash
# 从模板生成一个新服务（复制、改模块路径、改名、tidy、build、test）
tools/new-service.sh orders github.com/acme/orders ../orders
# 或 make new-service NAME=orders MODULE=github.com/acme/orders
```

生成前先按上一节配好 kit 的私有模块访问（生成脚本会 `go mod tidy`）。生成后的第一步见生成服务里的 README：`make up`（一次性 `schema` 服务代跑 `db/*.sql`）→ `make run` → `requests.http` 走一遍。

**能力裁剪走配置而不是生成参数**：Go 二进制把所有能力都链接进去，不用的部分由配置关掉——`rabbitmq.enabled=false`（outbox 行堆积可见、不丢事件）、`tasks.max_workers=0`（本宿主不碰任务队列）、`rabbitmq.consumers_enabled=false` / `kafka.consumers_enabled=false`（有独立 worker 时 API pod 不消费）；Kafka 默认关，`kafka.enabled` + `kafka.events` 打开后只有列出的事件走它。同一镜像靠配置区分形态，撤掉能力是改配置不是发版。

## 本地跑通（验收路径）

```bash
make up              # PG 18 / Redis 8 / RabbitMQ 4 / WireMock / Jaeger / Prometheus / Grafana + schema
make run                                 # APP_ENV=development：API :8080（/docs/ 页面与 /docs/openapi.yaml 文档只在这个环境），探针与 /metrics 在 :8081
make run-worker                          # worker 形态：没有 API 监听，探针与 /metrics 在 :8082（和 API 并排跑，两者是同一队列的竞争消费者）
# 本机 5432/6379 被占用时：POSTGRES_HOST_PORT=15432 REDIS_HOST_PORT=16379 make up
#   然后 POSTGRES__DSN='postgres://msvc:msvc@localhost:15432/msvcservice?sslmode=disable' REDIS__ADDR=localhost:16379 make run
tools/e2e.sh --no-app                    # 对宿主机上的服务做端到端验收（见下）：不动正在跑的 compose 栈，也不收掉它
tools/e2e.sh                             # 或连服务一起以容器形式起（docker build → --profile app），结束后收掉（--keep 保留）

# Kafka（可选，profile kafka）：上面的栈之外再起单节点 Kafka（apache/kafka，KRaft；宿主机 localhost:9092，容器内 kafka:19092）
docker compose --profile kafka up -d
KAFKA__ENABLED=true KAFKA__EVENTS=orders.order-created.v1 make run   # 这个事件改走 Kafka（outbox → topic → consumer group → CONFIRMED），它的 RabbitMQ 队列不再声明
```

`tools/e2e.sh` 覆盖（与脚本头部的清单同一份）：admin 端口的探针与 /metrics（API 端口上没有）→ 铸一个 user-svc 形状的令牌 → 401 / 403 / 伪造密钥 401 → 创建订单 → outbox → RabbitMQ → 消费者确认 → 400 / 422 错误契约 → 幂等重放 → 限流 429 → 熔断（WireMock `FAULT` SKU，502）→ **重启 RabbitMQ 自动重连**再走一单。它只走 RabbitMQ 路径；Kafka 上的发布、消费、重试与死信 topic 由 kit 的 `tests/integration/kafka_test.go` 对真 broker 覆盖。

控制台：RabbitMQ `:15672`（guest/guest）· Prometheus `:9090` · Grafana `:3000`（admin/admin，预置 RED/消息/任务看板）· Jaeger `:16686` · WireMock `:8089`。

没有本地 IdP：`curl -H 'X-Dev-Subject: 1' -H 'X-Dev-Realm: CONSUMER'` 即可打受保护端点（仅 `APP_ENV=development`）；真令牌用 `tools/mint-dev-token.sh --sub 1 --realm CONSUMER` 铸（与 user-svc 开发密钥相同）。

## 测试与验证

```bash
make test               # 单元测试 + 架构守卫（依赖方向、kit 表副本、路由↔OpenAPI）+ DDL 规范守卫（kit 的 ddlguard）
make test-integration   # Testcontainers（无 Docker 自动跳过；CI=true 时跳过视为失败）
make lint               # golangci-lint v2（120 列、函数 80 行、嵌套 4 层、有 default 的 switch 把它放最后、枚举 switch 必须穷尽、depguard 分层）；版本钉在 Makefile 的 GOLANGCI_LINT_VERSION，与流水线同一个，本机不同会警告
make helm-lint          # helm lint + 两个 overlay 与 worker + KEDA 形态的 template（RabbitMQ、Kafka 两种 trigger 各渲染一遍）
deploy/kind/kind-smoke.sh   # K8s 冒烟：kind + 依赖 + db/*.sql + helm install + 探针 + 优雅停机（镜像构建要 kit 凭证：gh 或 KIT_NETRC）
```

## 从零新增一个服务（完整步骤）

0. 配好 kit 的私有模块访问（上面的"平台 kit"：`GOPRIVATE` + `gh auth setup-git`，每台机器一次）——生成脚本要 `go mod tidy`，拉不到 kit 就停在那一步。
1. `tools/new-service.sh payment github.com/acme/payment` 并推到新仓库。
2. 建库：为服务申请独立数据库；`POSTGRES__DSN` 走 K8s Secret。
3. 写领域模型（参考 `internal/order` 的注释），手写 `db/0001_init.sql`（团队规范：`text`、`TIMESTAMPTZ`、`SERIAL`、复数表名、软状态，守卫会检查）并交 DBA 执行；仓储放 `internal/postgres`，用 `db.Q(ctx)` 取执行器以加入事务。
4. 定义集成事件（`EventName()` 显式带版本，只带业务键；要按聚合保序的实现 `messaging.Keyed`）与消费者（`messaging.Subscribe` 自动套 inbox）；事件在哪个 bus 上由 `kafka.events` 决定（平台决策，见决策记录）；定时/后台任务实现 `taskqueue.Handler` 挂到 runner。
5. 删掉 orders 示例：按 `docs/replacing-the-example.md` 逐项清（先加后删，`make test` 一路绿；留意 KEDA `queueName`（Kafka trigger 是 `kafka.consumerGroup` / `kafka.topic`）与 `tests/arch/deps_test.go` 顶部的表这两个编译器发现不了的地方），最后跑 `tools/check-example-removed.sh`，退出码 0 才算删干净。
6. `make up` + `make run`；`requests.http` 走一遍；写 handler 测试（参考 `internal/httpapi/router_test.go`，无需容器）。
7. 配 Azure DevOps：导入 `azure-pipelines.yml`，配置 registry service connection（`containerRegistry`，并把 `registryHost` 设成它的主机名：镜像的构建、扫描、推送、Helm 都用这一个引用）与 K8s service connection（`k8sConnectionDev` / `k8sConnectionProd`），设置 secret 变量 `GitHubKitToken`（拉私有 kit），有独立 worker 时 `buildWorker: true`。DDL 双跑门禁在 agent 上自起 PostgreSQL，不需要任何变量。
8. 首次部署前在目标 namespace 创建 Secret `<release>-secrets`（流水线的 release 名就是服务名，即 `msvcservice-secrets`），内容 `POSTGRES__DSN`、`RABBITMQ__URL`、`AUTH__SIGNING_KEY`（Kafka 用 SASL 时加 `KAFKA__SASL__PASSWORD`）；已有别名的 Secret 则设 `secret.existingName`。dev/prod overlay 的 chart 不自建 Secret（`secret.create=false`；设成 true 又没给 `secret.values` 时 `helm template` 直接报错，而不是起一个启动不了的 pod）。改掉 `values.yaml` 里标了 `CHANGE ME` 的键（Redis、OTLP、下游地址、ingress host），并由 DBA 手动执行 `db/` 全部脚本；`helm upgrade --install` 由流水线执行。

## 配置

`internal/config/default.yaml` 是每个部署的起点（每个键都有注释），也是**生产基线**：不带任何 localhost 连接串，compose 的地址只在 `development.yaml` 里；`APP_ENV` 不设即 production，没有 `POSTGRES__DSN` / `REDIS__ADDR` / `RABBITMQ__URL` / `AUTH__SIGNING_KEY` 就拒绝启动（`config_test.go` 证明这一点）。`APP_ENV` 选 overlay（`development.yaml`），环境变量 `SECTION__KEY` 覆盖任意键（列表用逗号：`RABBITMQ__RETRY_DELAYS=30s,2m,10m`；map 键是路径的一段：`TASKS__RECURRING__HEARTBEAT=5m`）。没有 `__` 的环境变量永远不会进入配置。所有段在启动前校验（struct tag + `Validate()`，含跨字段规则如 `task_timeout < stale_after`），配错 fail-fast。

| 段 | 要点 |
|---|---|
| `server` | `addr`（API）与 `admin_addr`（探针 + /metrics）两个监听、各超时、`shutdown_timeout`（总停机预算）与 `drain_timeout`（API 排空份额，须小于前者）、`request_timeout`（→ 504）、`max_body_bytes`、`max_header_bytes`（0 = net/http 默认） |
| `http` | `forwarded_headers`（chart 打开；`trusted_proxies` CIDR）、`cors`、`rate_limiting`（per-pod！）、`idempotency`、`required_headers.consumer / backoffice`（缺则 400 MISSING_HEADERS）、`problem_type_base_uri`、`docs_enabled`（只在 `APP_ENV=development` 生效，代码里写死） |
| `logging` / `telemetry` | 级别与格式、脱敏字段；OTLP 端点（traces）、头采样率（生产 chart 设 0.1）、`attribute_value_length_limit`（span 属性截断） |
| `postgres` / `redis` / `cache` | 池参数、`statement_timeout`、`idle_in_transaction_timeout`、`slow_query_threshold` / `slow_query_log_rate`（慢 SQL 日志，只记语句不记实参）；Redis 单节点 `addr` 或 Sentinel / Cluster 的 `addrs`（+ `master_name`）二选一、短超时、`key_prefix` 必填；L2/L1 TTL、`load_timeout`（冷 key 回源的独立预算） |
| `rabbitmq` / `outbox` | 交换机、quorum 队列、prefetch/并发、重试梯队、confirms、`publish_timeout`（一次发布的总时限）、`drain_timeout`（消费者排空）；投递器轮询/批量、被拒消息的退避（`retry_base_delay` → `retry_max_delay`）、分批清理（`outbox` 段两个 broker 的投递器共用） |
| `kafka` | 默认关。`events`（走 Kafka 的事件名：发布与消费都在 Kafka，其余事件留在 RabbitMQ；只在 `enabled` 时可非空、不得重复、须是合法 topic 名）、`consumers_enabled`、`brokers`、`tls`、`sasl.mechanism` / `username`（密码只从 Secret 的 `KAFKA__SASL__PASSWORD` 来）、`topics`（事件 → topic 覆盖）、`retry_delays`（`{group}.retry.N` 梯队）、`publish_timeout`、`drain_timeout`、`ping_interval`、`consume_from`（新 group 的起点，默认 `earliest`）、`create_topics`、`topic_partitions` / `topic_replication_factor`（-1 = broker 默认）、`max_poll_records` |
| `tasks` | `max_workers`（0 = kill switch）、attempts 预算与失败退避（`retry_base_delay` → `retry_max_delay`）、`task_timeout < stale_after`、`recurring.*` / `cron.*` 覆盖、`queues.<q>.*` 按队列覆盖（含 `task_timeout`，同样须小于 `stale_after`） |
| `auth` | `issuer`、`signing_key`（Secret）、`additional_signing_keys`（轮换期间并存的密钥）、`audience`（默认空）、`dev_headers` |
| `service_clients.*` | `base_url`（必须以 `/` 结尾）、总超时、重试、熔断、`credentials: none\|forward_caller`、`propagate_headers`（默认透传 X-Request-ID、X-Language，总是带 X-Service-Name） |

## 关键决策记录

| 决策 | 选择 | 理由与取舍 |
|---|---|---|
| 模板形态 | **仓库本身就是一个能跑的服务**，`tools/new-service.sh` 复制改名 | Go 官方的 `gonew` 只改模块路径，没有参数化裁剪；一个随时 `go test` 全绿的参考服务比一套条件生成规则更不容易腐烂。代价：生成后要手动删掉 orders 示例 |
| 分包方式 | 按限界上下文分包（`internal/order`：聚合 + 用例服务 + 端口 + 事件），适配器按依赖命名（`postgres`、`inventory`、`httpapi`），接口定义在使用方 | Go 社区的主流做法（Go 团队的模块组织指南、Ben Johnson 的 Standard Package Layout）：包名说明它提供什么，不设 `domain/application/infrastructure` 这类分层目录。依赖反转照样成立：端口在 `order` 包，实现在旁边，只有 `internal/app` 同时看见两者，`tests/arch` + depguard 强制 |
| 装配与对象生命周期 | 组合根 `internal/app` 显式构造进程级依赖，经构造函数传入；请求范围的数据（事务、身份、截止时间）只走 `ctx`；没有 DI 容器 | Go 社区的主流写法（Mat Ryer 的 `NewServer(deps...)`、Ben Johnson 的 WTF Dial），`context` 包的约定也是 ctx 只放请求范围数据：依赖在编译期可见，缺了编译不过。代价是组合根随限界上下文数线性变长，变长时按上下文拆 `buildXxx`。规则见 `docs/architecture.md` |
| 能力裁剪 | 配置开关，不在生成期裁代码 | Go 静态链接，未使用的包只多几 MB；`rabbitmq.enabled` / `tasks.max_workers=0` / `consumers_enabled` 让同一镜像在 API pod 与 worker pod 之间只差一个配置值；不用消息的服务设 `RABBITMQ__ENABLED=false` 即可 |
| kit 分发 | 独立的私有模块 `github.com/guangliangus/go-msvc-kit`，按语义化版本依赖；kit 自己的表（outbox / inbox / task_queues）的 DDL 随 kit 发布，服务 `db/` 保留逐字节副本、守卫比对 | Go 的正解是**版本化的共享模块**：平台契约在一处演进，服务按版本升级，不再每个服务各持一份会漂移的副本。代价是私有模块的访问要配三处（开发机 GOPRIVATE + gh、流水线 token、镜像构建 secret），kit 的改动要守发版纪律（tag 不可变、`gorelease` 查兼容性，见 kit README） |
| 宿主形态 | 一个二进制 `serve` / `worker` 子命令，一个镜像 | 同一静态二进制靠子命令选形态，不需要两个工程；Helm 的 `worker.enabled=true` 自动给 API pod 注入 `TASKS__MAX_WORKERS=0`、`RABBITMQ__CONSUMERS_ENABLED=false` 与 `KAFKA__CONSUMERS_ENABLED=false` |
| 路由 | chi v5 | 纯 `http.Handler`，中间件可组合、路由分组（按平面套 `RequireRealm`），`otelhttp` 直接兼容；不引 gin/echo 这类自有上下文的框架 |
| 配置 | koanf + 内嵌 YAML + `SECTION__KEY` 环境变量 | 镜像自带默认值（无需挂文件）；`__` 作为层级分隔符让键名自己可以含 `_`（`TASKS__MAX_WORKERS`），也是平台服务 Helm values 的统一写法；禁止无 `__` 的变量混入 |
| 数据访问 | pgx v5 原生 SQL + `RowToStructByName`，无 ORM、无 codegen | 团队规范是"DDL 手写是唯一真相"，ORM 的模型/迁移与之天然冲突；sqlc 是好选项但需要 cgo 工具链生成代码，暂不默认（known-open #8）。集成测试用 `db/*.sql` 建库，漂移在测试里暴露 |
| 事务 | `db.WithTx(ctx, fn)`，事务挂在 ctx 上，仓储用 `db.Q(ctx)`；库外副作用用 `db.AfterCommit` | 业务行 + outbox 行 + 任务行一次提交，各包互不知晓；嵌套加入外层。`WithTxRetry` 只重试 40001/40P01，整段重跑。删缓存必须在提交之后，否则并发读会把旧行写回缓存 |
| 乐观并发 | `xmin` | PostgreSQL 系统列，零 schema 改动、不会忘记自增；`UPDATE ... AND xmin = $n` 零行 → 409 `CONCURRENCY_CONFLICT`。要跨库可移植时换成显式 `version` 列 |
| 日志 | `log/slog`，JSON 到 stdout；handler 包装器加 trace_id/span_id、脱敏 | 标准库，无第三方；与 OTel 的 trace id 同值 |
| 指标通道 | /metrics Prometheus 抓取（OTel SDK + Prometheus exporter），traces 走 OTLP | 指标拉、trace 推，是 K8s 上最省事的分工；`otelhttp` 直接产出 OTel 语义约定的 `http_server_request_duration_seconds`；`msvc_messaging_*`、`msvc_tasks_*` 是平台指标名，一套看板/告警覆盖所有服务 |
| 认证 | HS256 本地验签（`golang-jwt/v5`），`realm` 平面策略，开发请求头只认 `APP_ENV=development`；验证端可同时认多把密钥 | 与 user-svc 的现状一致；多把密钥让轮换不需要全平台同时切换（令牌格式不变）；OIDC 留给 B2B 签发方（known-open #4） |
| 错误契约 | handler 返回 `error`，`httpx.ErrorWriter` 统一写 RFC 9457 | 一处决定状态码、`errorCode`、`traceId`、`Retry-After`、`WWW-Authenticate`；路由器 404/405 同形；服务自己的异常类型用 `Mappers` 接入，本地化用 `Customizers` |
| 校验边界 | 请求的**形状**归 `httpapi` 的 struct tag：400 `VALIDATION_FAILED`，逐字段列在 `errors` 里；业务**规则**归上下文包：422 带它自己的码（`order.CheckLines`、`order.New`） | 形状错了业务代码一行都不跑，所有字段错误一次报完；规则只在一处（`order.MaxItems` 是条数上限的唯一来源，tag 里故意不复制），消费者和任务绕过 tag 也受同一套规则约束。分页参数同理：不是整数 400 并点名参数，越界的数字夹到边界（`page` > 10000 拒绝） |
| 守卫 | `tests/arch/deps_test.go` 表驱动（上下文 / 适配器 / 入口 / 组合根四张名单，目录不存在或 `go list` 匹配到零个包都算失败）；`internal/httpapi/openapi_test.go` 把路由和 `api/openapi.yaml` 双向比对；`tests/arch/kitddl_test.go` 把 `db/kit_*.sql` 和 kit 内嵌的脚本逐字节比对 | 守卫最常见的失效方式是"检查了零个东西还报绿"，所以零匹配是错误；改名一个目录必须改表，否则测试红。文档漂移只能抓路径层面，schema 仍靠人（known-open #5） |
| HTTP 幂等 | `Idempotency-Key` + Redis `SET NX GET`，fail-closed 503 | IETF `Idempotency-Key` 草案的语义：成功重放、并发 409、错误不缓存；存储不可达时放行等于删除保证 |
| 限流 | `httprate` 本地滑动窗口，`caller\|ip\|none` 分桶，拿不到地址不计数 | per-pod 语义写进文档；跨副本的业务限流用 `redisx.FixedWindowAllow`（一段 Lua、fail-open） |
| 弹性 | HTTP 客户端：breaker → retry（只幂等方法 / 带 Idempotency-Key）→ 透传 token → otelhttp；非 HTTP：`resilience.Retry` + 调用方给的分类器 | 价值在分类而不是次数：`postgres.IsTransient` 不把 23505 当瞬态；熔断默认只在单个下游前（共享资源前的熔断会把局部故障放大成全局） |
| 消息 | 官方 amqp091-go + 自写重连；拓扑/头/指标按平台线格式；outbox（SKIP LOCKED）+ inbox（事务内） | 官方客户端没有自动恢复，`rabbitmq.Connection` 补上；不引 watermill 等框架以保持线格式可控。队列名 `{service}.{event}` 与宿主形态无关，API 与 worker 是同一队列的竞争消费者 |
| 消息总线：RabbitMQ 与 Kafka 并存，按事件路由 | 每个事件只在一个 bus 上：`kafka.events` 列出的事件发布到 Kafka、也从 Kafka 消费，其余留在 RabbitMQ。每个 bus 一个 outbox 投递器（`messaging.ForEvents` / `ExceptEvents`）、一个消费者宿主，订阅只声明一次、按事件分给两边（`internal/app/messaging.go`）。Kafka 客户端 franz-go；topic = 事件名，consumer group = `{service}.{event}`，失败走 `{group}.retry.N` 梯队再进 `{group}.dlq`；新 group 从 `earliest` 开始 | 生产者和所有消费者必须对"事件在哪个 bus 上"达成一致，所以按事件而不是按服务切，换 bus 是平台决策、不是一个服务的配置；一个 broker 宕掉不拖住另一个的事件。重试用 topic 梯队（同样的 30s / 2m / 10m）与 RabbitMQ 对齐，失败的 record 不堵住分区，代价是它离开分区后不再与同 key 的后续 record 保序（只有一次成功的才保序）。franz-go 是纯 Go，`CGO_ENABLED=0` 的静态二进制与 distroless 镜像不变；confluent-kafka-go 包着 librdkafka、要 cgo。`earliest`：有 inbox 去重，重放安全、跳过不安全。线格式见 `docs/architecture.md` 的平台契约 |
| 调度 | 数据库任务队列（`task_queues` 表设计与平台其他服务一致，运维 SQL 通用）：间隔 + cron（robfig/cron 解析，时区、misfire 策略）；handler 失败/panic 按指数退避重排并计 attempts | 多副本单实例执行、attempts、fixer、指标全在一处；失败走退避而不是等 fixer，预算对 error 与 Retry 一视同仁；River 是不想自己维护时的替代（known-open #6） |
| 优雅停机 | `lifecycle.Group` 反序停止：API 先停（readiness 变 503，在途请求最多 `drain_timeout`）→ 任务 runner 排空 → 消费者停收、排空后再取消 → outbox → 连接（每个 bus 一组，Kafka 的先停）→ admin 最后停 | grace period 从 preStop 之前开始计时，chart 设 50s = preStop 5 + 预算 30 + flush 5 + 余量；API 只拿预算的一部分，后面的组件不会被饿死；关连接池有上限 |
| 架构守卫 | `tests/arch`（`go list` 直接 import）+ kit 的 `ddlguard`（服务侧一行 `ddlguard.Check(t, "../../db")`）+ depguard（`order-stays-pure` 覆盖 `internal/` 下除组合根与适配器之外的一切） | 靠人记住的约束平均被违反两次以上才有人写下来；守卫让违规在 `go test` 时红。DDL 规则放进 kit，每个服务跑同一套而不是各抄一份 |
| 镜像 | 多阶段：`golang:1.27-alpine` 编译静态二进制 → `distroless/static:nonroot`，都按 digest 固定 | 无 shell、非 root、只读根文件系统（chart 打开）；镜像约 20 MB；tag 是会移动的指针，digest 让同一提交重建出同一镜像，Renovate 以 PR 形式升级 |
| 测试基础设施 | testify 不是必需：标准 `testing` + `httptest` + miniredis（单测）+ Testcontainers（集成） | 单测不需要 Docker 也能覆盖中间件、幂等、缓存、认证、handler；真依赖的行为（SKIP LOCKED、xmin、SET NX GET、TTL 梯队）只在容器里验 |
| 平台请求头 | 入站由 `PlatformHeaders` 捕获，下游调用自动带 `X-Service-Name` 并透传 `X-Request-ID`、`X-Language`；`http.required_headers` 按平面要求必带头 | 平台现有服务的内部路由要求这三个头，缺了直接 400；放进 kit 后每个 handler 都不用记。入站没有 X-Request-ID 时用 trace id 顶上，两边日志对得上 |
| 错误文案本地化 | kit 的 `i18n`：按 errorCode 的多语言目录，`X-Language` / `Accept-Language` 协商，写进 RFC 9457 的 `title`；守卫测试保证每个码每种语言都有文案 | title 是"按类型"的摘要，正好一码一句，detail 留给开发者。kit 带平台码的 en / zh-CN / zh-TW / ja，服务只补自己的码 |
| 数据库可观测 | pgxpool 指标（`pgxpool_*`）+ 慢 SQL 日志（只记语句不记实参、截断 2000 字节、每秒限速、被限速的也计数） | 池满和慢查询是两种故障，各要有自己的信号。慢 SQL 的 elapsed 不含等连接的时间，排队看 `pgxpool_empty_acquire_total` |
| 积压信号 | 队列与 outbox 都报"最老积压时长"；死信按 reason 拆成 exhausted / non_retryable（`messaging.NonRetryable`）、discarded | 深度说明有多少在等，时长说明已经晚了多久，告警盯后者；主动放弃的消息是日常噪声，不能淹没真故障 |
| 任务的 trace | 任务 payload 是 JSON 对象时入队带上 `traceparent`，执行时的 span 挂在发起请求的 trace 下 | 与 outbox 一致：异步工作能从发起它的请求找到；共享的 `task_queues` 表不加列 |
| 无凭证内部路由 | 若需要无凭证的 `/internal` 路由时套用 kit 的 `middleware.ClusterInternalOnly`：拒绝带 forwarded 头的请求并要求私网对端（模板本身没有挂这样的路由） | 服务间凭证签发（known-open #4）落地前的纵深防御；它不是授权 |
| Go 运行时 | chart 按 `limits.memory` 的 80% 推导 `GOMEMLIMIT`；`GOMAXPROCS` 交给 Go 1.25+ 按 cgroup 配额自动设置 | GC 只认 GOGC，不认 cgroup 上限，突发流量会在 GC 来得及跑之前被 OOMKill |
| 健康检查 | readiness 只 gating PostgreSQL；Redis、RabbitMQ、Kafka 是 informational（报告里 `Degraded`，仍 200）；每项结果导出 `msvc_health_check_up` | readiness 回答"这个 pod 该不该接流量"。共享依赖一抖就摘掉所有 pod，降级变成全量 503，修复的发布也起不来；共享依赖的故障靠 `DependencyUnavailable` 告警发现 |
| 管理端口 | 探针与 `/metrics` 在 `server.admin_addr`（8081），API 在 8080；worker 只有 admin | Ingress 只指 API 端口，探针与指标不会被暴露；API 打满不影响探针。Go 服务的常见做法 |
| 金额线格式 | `money.Amount`：JSON 数字，类型自带 `MarshalJSON` | 平台约定金额是数字；格式是类型的属性，测试与生产一致，不依赖 `decimal` 的包级开关 |
| 分页 | 有界列表用页码（`page` ≤ 10000）；无界列表用 keyset 游标（`paging.CursorResult`） | `count(*)` + OFFSET 越往后越贵；游标每页代价相同、不数总数。示例：客户自己的订单用页码，后台全部订单用游标 |
| outbox 投递 | broker 不就绪就不认领；发布有总时限；只有消息被拒才计 attempts；积压与清理在独立循环里 | broker 故障时既不攥着事务和行锁，也不把好事件停成 FAILED；积压指标在投递卡住时照常更新，告警才响得出来 |
| CI 工具 | Go 工具在 `tools/go.mod`（`tool` 指令，`go tool -modfile=tools/go.mod`）；安装脚本按版本 tag 取；版本由 Renovate 升级 | 本地与流水线同一版本、go.sum 校验，且不混进服务自己的依赖图；从 `HEAD`/`main` 取安装脚本等于把供应链交给一个会移动的分支 |
| 告警落地 | `deploy/grafana/`：Prometheus 格式规则 + 看板 JSON，compose 直接加载，集群用脚本导入成 Grafana-managed 规则（页面可编辑） | 阈值上线初期要反复调，走 provisioning 每次都要 PR 加重启 Grafana。规则带 service / alertgroup / severity 三个路由标签，annotation 里写 LogQL 与 SQL |

## 常见运维问答

- **DDL 在哪跑？** 手动执行 `db/*.sql`（先 DDL、后发代码，脚本幂等且向后兼容）；compose/kind 冒烟里由 `schema` 一次性服务代跑。应用启动永不改库。
- **客户端超时重试 POST 会重复下单吗？** 带 `Idempotency-Key` 就不会：成功响应被快照重放（`Idempotency-Replayed: true`），同 key 并发得 409，错误响应不缓存、可重试。
- **两个副本同时改同一行？** 后写的 `UPDATE` 匹配不到 `xmin` → 409 `CONCURRENCY_CONFLICT`，客户端重读重试。
- **DLQ 里的消息怎么处理？** `{queue}.dlq` 手工检视（管理台）：`x-exception` 带失败原因，`x-dead-letter-reason`（exhausted / non_retryable）、`x-failed-at`、`x-origin-queue` 说明为什么、何时、从哪个队列来，AMQP `app-id` 是发布它的服务；修复后用 `tools/dlq-replay.sh <queue>` 批量回放（重置重试计数）。回放中途失败的那条已经不在 DLQ 里，脚本会把它完整写到 stderr 和 `./dlq-replay-failed-<queue>-<ts>.json` 再停下，照着文件手动重发。Kafka 的死信在 `{group}.dlq` topic，头相同、另带 `x-origin-topic`；脚本只管 RabbitMQ，Kafka 的回放见 known-open #9。
- **API pod 和 worker pod 都在消费？** 它们是同一队列（`{service}.{event}`；Kafka 上是同名 consumer group）的竞争消费者，每条消息只被处理一次；想让 API pod 完全不消费，设 `RABBITMQ__CONSUMERS_ENABLED=false` / `KAFKA__CONSUMERS_ENABLED=false`（chart 在 `worker.enabled=true` 时自动设置）。
- **Redis / RabbitMQ / Kafka 挂了，pod 为什么还是 Ready？** 有意的：它们是所有副本共享的依赖，摘流量会让降级变成全量 503。看 `DependencyUnavailable` 告警和 admin 端口 `/health/ready` 的报告（`Degraded`，对应项 `"gating": false`）。只有 PostgreSQL 会让 pod NotReady。
- **任务一直失败会怎样？** handler 返回 error 或 panic：按 `tasks.retry_base_delay` 起指数退避重排，一次性任务失败满 `max_attempts` 后删除并计 `msvc_tasks_dead_total{reason="exhausted"}`；周期任务不受预算限制，持续失败看 `TasksFailing` 告警。
- **限流为什么挡不住匿名洪水？** 认证失败在中间件链里先 401，到不了限流——匿名/凭证爆破类流量的防护是网关的职责。
- **outbox 积压告警**：`msvc_messaging_outbox_pending`（Grafana 面板 + Prometheus 告警样例已内置）；任务积压看 `msvc_tasks_queue_depth`。
- **审计"操作人"取值**：HTTP = JWT `sub`（或 `client_id`）；消息消费 / 后台任务 = `system`。
- **慢 SQL 在哪看？** 日志 `msg="slow query"`，带语句文本不带实参；阈值 `POSTGRES__SLOW_QUERY_THRESHOLD`，每秒限速 `POSTGRES__SLOW_QUERY_LOG_RATE`。elapsed 不含等连接的时间：主键查询也慢说明池满了，看 `pgxpool_empty_acquire_total`。
- **DLQ 告警为什么只盯 exhausted？** `msvc_messaging_dead_lettered_total{reason}` 把"重试用尽"和 handler 用 `messaging.NonRetryable` 主动放弃分开，后者是日常噪声。任务同理：`msvc_tasks_dead_total{reason}`。
- **调别的平台服务被 400 MISSING_HEADERS？** 用 kit 的 `httpclient` 就不会：它自动带 X-Service-Name，透传 X-Request-ID 与 X-Language。自己 new 的 `http.Client` 不会。
- **看板和告警怎么进集群？** `deploy/grafana/import-dashboard.sh` 与 `import-alerts.sh`（见同目录 README）；导入告警后把每条规则的 No data 设为 Normal。
