# Go 微服务脚手架模板（API + Worker，Kubernetes 部署）

一个**可直接运行**的参考服务，同时就是模板：`tools/new-service.sh` 复制并改名即得到新服务。基础设施开箱即用，开发者只写业务代码。按 Go 生态的惯例组织（标准库优先、`net/http` 中间件、按限界上下文分包、接口定义在使用方、组合根显式装配），只在**平台互通契约**上与平台现有服务（含 C# 服务与 user-svc）保持一致：错误契约（`errorCode` / `traceId` / `urn:msvc:error:`）、消息线格式（AMQP 头、队列命名）、指标名、令牌格式。契约清单见 `docs/architecture.md`。

- **技术栈**：Go 1.26+ · chi v5（纯 `net/http`）· PostgreSQL + pgx v5（原生 SQL，无 ORM）· Redis + go-redis v9 · RabbitMQ 官方 amqp091-go · OpenTelemetry（traces OTLP 推送 / metrics Prometheus 抓取）· `log/slog` JSON 日志 · koanf 配置（内嵌 YAML 默认值 + `SECTION__KEY` 环境变量）· golang-jwt v5 HS256（user-svc 签发的令牌本地验签）· gobreaker 熔断 · 数据库任务队列（间隔 + cron + misfire 策略）· Testcontainers 集成测试
- **交付物**：一个二进制、两种形态（`serve` / `worker`，同一镜像）· `pkg/` 平台基础包（kit，随服务源码分发）· Orders 端到端示例 · docker-compose 全家桶 · Helm Chart · Azure Pipelines · 生成脚本

## 仓库结构

```
cmd/msvcservice/           main：serve | worker | version
internal/
  app/                     组合根：读配置、显式装配、注册生命周期组件（没有 DI 容器）
  config/                  类型化配置 + 内嵌 default.yaml / development.yaml
  order/                   示例限界上下文：聚合（不变量、状态机、错误码）+ 用例服务 + 端口接口 + 集成事件
  postgres/                order 端口的仓储实现（原生 SQL、ctx 事务、xmin 乐观并发、审计列）
  inventory/               下游库存服务的 HTTP 适配器（实现 order 的端口，走弹性客户端）
  httpapi/                 路由（中间件顺序）、handler、请求/响应类型、/docs
  consumers/               事件消费者（inbox 事务内运行）
  tasks/                   后台任务（recurring + cron 示例）
pkg/                       平台基础包（kit）——永不 import internal/，将来可整体抽成共享模块
  apperror  actor  paging  conf  logging  telemetry  health  lifecycle  httpserver  reqmeta（平台请求头）  i18n（错误文案目录）
  httpx（RFC 9457 / JSON / 校验）  middleware（日志、恢复、超时、CORS、限流、幂等、客户端 IP、平台请求头、集群内路由守卫）
  auth（HS256 / realm / scope / 开发请求头）  postgres（池、ctx 事务、池指标、慢 SQL 日志）  redisx（锁、幂等存储、业务限流）  cache（L1+L2）
  resilience（重试、熔断）  httpclient（弹性下游客户端）  messaging（事件、outbox、inbox、订阅）  money（金额线格式）
  rabbitmq（连接、拓扑、发布、消费）  taskqueue（存储、runner、cron）  idempotency  testutil
db/                        手写幂等 DDL（schema 唯一真相，应用永不改库）
api/openapi.yaml           手写 OpenAPI 3.1，开发环境在 /docs 以 Scalar 渲染
deploy/helm/msvcservice/   Helm Chart（api + worker 两个 Deployment、HPA/PDB/KEDA/ServiceMonitor/Ingress/NetworkPolicy）
deploy/grafana/            看板 JSON、告警规则（含路由标签与 runbook）、导入脚本；compose 与集群共用这一份
deploy/compose/            本地栈的 Prometheus / Grafana provisioning、WireMock 库存桩
deploy/kind/               kind 冒烟
tests/arch/                架构守卫：依赖方向、DDL 规范（go test 即跑）
tests/integration/         Testcontainers：PG / Redis / RabbitMQ 真实依赖（-tags=integration）
tools/                     new-service.sh · mint-dev-token.sh · dlq-replay.sh · e2e.sh · go.mod（CI 工具版本，`go tool -modfile=tools/go.mod`）
docs/                      architecture.md（代码强制的约束）· known-open.md（有意留着不做的事）
```

## 使用模板

```bash
# 从模板生成一个新服务（复制、改模块路径、改名、tidy、build、test）
tools/new-service.sh orders github.com/acme/orders ../orders
# 或 make new-service NAME=orders MODULE=github.com/acme/orders
```

生成后的第一步见生成服务里的 README：`make up`（一次性 `schema` 服务代跑 `db/*.sql`）→ `make run` → `requests.http` 走一遍。

**能力裁剪走配置而不是生成参数**：Go 二进制把所有能力都链接进去，不用的部分由配置关掉——`rabbitmq.enabled=false`（outbox 行堆积可见、不丢事件）、`tasks.max_workers=0`（本宿主不碰任务队列）、`rabbitmq.consumers_enabled=false`（有独立 worker 时 API pod 不消费）。同一镜像靠配置区分形态，撤掉能力是改配置不是发版。

## 本地跑通（验收路径）

```bash
make up              # PG 18 / Redis 8 / RabbitMQ 4 / WireMock / Jaeger / Prometheus / Grafana + schema
make run                                 # APP_ENV=development：API :8080（/docs 可用），探针与 /metrics 在 :8081
# 本机 5432/6379 被占用时：POSTGRES_HOST_PORT=15432 REDIS_HOST_PORT=16379 make up
#   然后 POSTGRES__DSN='postgres://msvc:msvc@localhost:15432/msvcservice?sslmode=disable' REDIS__ADDR=localhost:16379 make run
tools/e2e.sh --no-app --keep             # 对宿主机上的服务做端到端验收（见下）
tools/e2e.sh                             # 或连服务一起以容器形式起（docker build → --profile app）
```

`tools/e2e.sh` 覆盖：admin 端口的三探针与 /metrics（API 端口上 404）→ 铸一个 user-svc 形状的 HS256 令牌 → 未认证 401、跨平面 403、伪造密钥 401、后台平面 200 → 创建订单（API → WireMock 库存报价 → 落库+outbox 同事务 → RabbitMQ → 同进程消费者 inbox 确认）→ 同 key 重放 `Idempotency-Replayed: true` + `X-Trace-ID` → 400/422/404/409 错误契约 → 限流 429 → 熔断（WireMock `FAULT` SKU 连接重置，5 次后 502 "circuit breaker is open"）→ **重启 RabbitMQ 自动重连**再走一单。

控制台：RabbitMQ `:15672`（guest/guest）· Prometheus `:9090` · Grafana `:3000`（admin/admin，预置 RED/消息/任务看板）· Jaeger `:16686` · WireMock `:8089`。

没有本地 IdP：`curl -H 'X-Dev-Subject: 1' -H 'X-Dev-Realm: CONSUMER'` 即可打受保护端点（仅 `APP_ENV=development`）；真令牌用 `tools/mint-dev-token.sh --sub 1 --realm CONSUMER` 铸（与 user-svc 开发密钥相同）。

## 测试与验证

```bash
make test               # 单元测试 + 架构守卫（依赖方向）+ DDL 规范守卫
make test-integration   # Testcontainers（无 Docker 自动跳过；CI=true 时跳过视为失败）
make lint               # golangci-lint v2（120 列、函数 80 行、嵌套 4 层、switch 必有 default、depguard 分层）
make helm-lint          # helm lint + 两个 overlay template
deploy/kind/kind-smoke.sh   # K8s 冒烟：kind + 依赖 + db/*.sql + helm install + 探针 + 优雅停机
```

## 从零新增一个服务（完整步骤）

1. `tools/new-service.sh payment github.com/acme/payment` 并推到新仓库。
2. 建库：为服务申请独立数据库；`POSTGRES__DSN` 走 K8s Secret。
3. 写领域模型（参考 `internal/order` 的注释），手写 `db/0001_init.sql`（团队规范：`text`、`TIMESTAMPTZ`、`SERIAL`、复数表名、软状态，守卫会检查）并交 DBA 执行；仓储放 `internal/postgres`，用 `db.Q(ctx)` 取执行器以加入事务。
4. 定义集成事件（`EventName()` 显式带版本，只带业务键）与消费者（`messaging.Subscribe` 自动套 inbox）；定时/后台任务实现 `taskqueue.Handler` 挂到 runner。
5. `docker compose up -d` + `make run`；`requests.http` 走一遍；写 handler 测试（参考 `internal/httpapi/router_test.go`，无需容器）。
6. 配 Azure DevOps：导入 `azure-pipelines.yml`，配置 registry / K8s service connection，设置 `CiDatabaseUrl`（DDL 双跑门禁），有独立 worker 时 `buildWorker: true`。
7. 首次部署前在目标环境创建 Secret（`POSTGRES__DSN`、`RABBITMQ__URL`、`AUTH__SIGNING_KEY`），并由 DBA 手动执行 `db/` 全部脚本；`helm upgrade --install` 由流水线执行。

## 配置

`internal/config/default.yaml` 是每个部署的起点（每个键都有注释），`APP_ENV` 选 overlay（`development.yaml`），环境变量 `SECTION__KEY` 覆盖任意键（列表用逗号：`RABBITMQ__RETRY_DELAYS=30s,2m,10m`；map 键是路径的一段：`TASKS__RECURRING__HEARTBEAT=5m`）。没有 `__` 的环境变量永远不会进入配置。所有段在启动前校验（struct tag + `Validate()`，含跨字段规则如 `task_timeout < stale_after`），配错 fail-fast。

| 段 | 要点 |
|---|---|
| `server` | `addr`（API）与 `admin_addr`（探针 + /metrics）两个监听、各超时、`shutdown_timeout`（总停机预算）与 `drain_timeout`（API 排空份额，须小于前者）、`request_timeout`（→ 504）、`max_body_bytes` |
| `http` | `forwarded_headers`（chart 打开；`trusted_proxies` CIDR）、`cors`、`rate_limiting`（per-pod！）、`idempotency`、`required_headers.consumer / backoffice`（缺则 400 MISSING_HEADERS）、`problem_type_base_uri`、`docs_enabled` |
| `logging` / `telemetry` | 级别与格式、脱敏字段；OTLP 端点（traces）、头采样率（生产 chart 设 0.1）、`attribute_value_length_limit`（span 属性截断） |
| `postgres` / `redis` / `cache` | 池参数、`statement_timeout`、`idle_in_transaction_timeout`、`slow_query_threshold` / `slow_query_log_rate`（慢 SQL 日志，只记语句不记实参）；Redis 短超时 + `key_prefix` 必填；L2/L1 TTL |
| `rabbitmq` / `outbox` | 交换机、quorum 队列、prefetch/并发、重试梯队、confirms、`publish_timeout`（一次发布的总时限）、`drain_timeout`（消费者排空）；投递器轮询/批量/分批清理 |
| `tasks` | `max_workers`（0 = kill switch）、attempts 预算与失败退避（`retry_base_delay` → `retry_max_delay`）、`task_timeout < stale_after`、`recurring.*` / `cron.*` 覆盖 |
| `auth` | `issuer`、`signing_key`（Secret）、`additional_signing_keys`（轮换期间并存的密钥）、`audience`（默认空）、`dev_headers` |
| `service_clients.*` | `base_url`（必须以 `/` 结尾）、总超时、重试、熔断、`credentials: none\|forward_caller`、`propagate_headers`（默认透传 X-Request-ID、X-Language，总是带 X-Service-Name） |

## 关键决策记录

| 决策 | 选择 | 理由与取舍 |
|---|---|---|
| 模板形态 | **仓库本身就是一个能跑的服务**，`tools/new-service.sh` 复制改名 | Go 官方的 `gonew` 只改模块路径，没有参数化裁剪；一个随时 `go test` 全绿的参考服务比一套条件生成规则更不容易腐烂。代价：生成后要手动删掉 orders 示例 |
| 分包方式 | 按限界上下文分包（`internal/order`：聚合 + 用例服务 + 端口 + 事件），适配器按依赖命名（`postgres`、`inventory`、`httpapi`），接口定义在使用方 | Go 社区的主流做法（Go 团队的模块组织指南、Ben Johnson 的 Standard Package Layout）：包名说明它提供什么，不设 `domain/application/infrastructure` 这类分层目录。依赖反转照样成立：端口在 `order` 包，实现在旁边，只有 `internal/app` 同时看见两者，`tests/arch` + depguard 强制 |
| 装配与对象生命周期 | 组合根 `internal/app` 显式构造进程级依赖，经构造函数传入；请求范围的数据（事务、身份、截止时间）只走 `ctx`；没有 DI 容器 | Go 社区的主流写法（Mat Ryer 的 `NewServer(deps...)`、Ben Johnson 的 WTF Dial），`context` 包的约定也是 ctx 只放请求范围数据：依赖在编译期可见，缺了编译不过。代价是组合根随限界上下文数线性变长，变长时按上下文拆 `buildXxx`。规则见 `docs/architecture.md` |
| 能力裁剪 | 配置开关，不在生成期裁代码 | Go 静态链接，未使用的包只多几 MB；`rabbitmq.enabled` / `tasks.max_workers=0` / `consumers_enabled` 让同一镜像在 API pod 与 worker pod 之间只差一个配置值；不用消息的服务设 `RABBITMQ__ENABLED=false` 即可 |
| kit 分发 | `pkg/` 随服务源码分发，`tests/arch` + depguard 保证它不反向依赖 `internal/` | Go 的正解是**版本化的共享模块**；在私有模块源（`GOPRIVATE` + Git 直连或代理）就绪前，kit 随源码分发保证生成即编译。抽取时 `pkg/` 整体搬到 `<org>/go-msvc-kit`，改 import 路径即可，守卫已保证它没有反向依赖 |
| 宿主形态 | 一个二进制 `serve` / `worker` 子命令，一个镜像 | 同一静态二进制靠子命令选形态，不需要两个工程；Helm 的 `worker.enabled=true` 自动给 API pod 注入 `TASKS__MAX_WORKERS=0` 与 `RABBITMQ__CONSUMERS_ENABLED=false` |
| 路由 | chi v5 | 纯 `http.Handler`，中间件可组合、路由分组（按平面套 `RequireRealm`），`otelhttp` 直接兼容；不引 gin/echo 这类自有上下文的框架 |
| 配置 | koanf + 内嵌 YAML + `SECTION__KEY` 环境变量 | 镜像自带默认值（无需挂文件）；`__` 作为层级分隔符让键名自己可以含 `_`（`TASKS__MAX_WORKERS`），也是平台服务 Helm values 的统一写法；禁止无 `__` 的变量混入 |
| 数据访问 | pgx v5 原生 SQL + `RowToStructByName`，无 ORM、无 codegen | 团队规范是"DDL 手写是唯一真相"，ORM 的模型/迁移与之天然冲突；sqlc 是好选项但需要 cgo 工具链生成代码，暂不默认（见 known-open）。集成测试用 `db/*.sql` 建库，漂移在测试里暴露 |
| 事务 | `db.WithTx(ctx, fn)`，事务挂在 ctx 上，仓储用 `db.Q(ctx)`；库外副作用用 `db.AfterCommit` | 业务行 + outbox 行 + 任务行一次提交，各包互不知晓；嵌套加入外层。`WithTxRetry` 只重试 40001/40P01，整段重跑。删缓存必须在提交之后，否则并发读会把旧行写回缓存 |
| 乐观并发 | `xmin` | PostgreSQL 系统列，零 schema 改动、不会忘记自增；`UPDATE ... AND xmin = $n` 零行 → 409 `CONCURRENCY_CONFLICT`。要跨库可移植时换成显式 `version` 列 |
| 日志 | `log/slog`，JSON 到 stdout；handler 包装器加 trace_id/span_id、脱敏 | 标准库，无第三方；与 OTel 的 trace id 同值 |
| 指标通道 | /metrics Prometheus 抓取（OTel SDK + Prometheus exporter），traces 走 OTLP | 指标拉、trace 推，是 K8s 上最省事的分工；`otelhttp` 直接产出 OTel 语义约定的 `http_server_request_duration_seconds`；`msvc_messaging_*`、`msvc_tasks_*` 是平台指标名，一套看板/告警覆盖所有服务 |
| 认证 | HS256 本地验签（`golang-jwt/v5`），`realm` 平面策略，开发请求头只认 `APP_ENV=development`；验证端可同时认多把密钥 | 与 user-svc 的现状一致；多把密钥让轮换不需要全平台同时切换（令牌格式不变）；OIDC 留给 B2B 签发方（known-open #4） |
| 错误契约 | handler 返回 `error`，`httpx.ErrorWriter` 统一写 RFC 9457 | 一处决定状态码、`errorCode`、`traceId`、`Retry-After`、`WWW-Authenticate`；路由器 404/405 同形；服务自己的异常类型用 `Mappers` 接入，本地化用 `Customizers` |
| HTTP 幂等 | `Idempotency-Key` + Redis `SET NX GET`，fail-closed 503 | IETF `Idempotency-Key` 草案的语义：成功重放、并发 409、错误不缓存；存储不可达时放行等于删除保证 |
| 限流 | `httprate` 本地滑动窗口，`caller\|ip\|none` 分桶，拿不到地址不计数 | per-pod 语义写进文档；跨副本的业务限流用 `redisx.FixedWindowAllow`（一段 Lua、fail-open） |
| 弹性 | HTTP 客户端：breaker → retry（只幂等方法 / 带 Idempotency-Key）→ 透传 token → otelhttp；非 HTTP：`resilience.Retry` + 调用方给的分类器 | 价值在分类而不是次数：`postgres.IsTransient` 不把 23505 当瞬态；熔断默认只在单个下游前（共享资源前的熔断会把局部故障放大成全局） |
| 消息 | 官方 amqp091-go + 自写重连；拓扑/头/指标按平台线格式；outbox（SKIP LOCKED）+ inbox（事务内） | 官方客户端没有自动恢复，`rabbitmq.Connection` 补上；不引 watermill 等框架以保持线格式可控。队列名 `{service}.{event}` 与宿主形态无关，API 与 worker 是同一队列的竞争消费者 |
| 调度 | 数据库任务队列（`task_queues` 表设计与平台其他服务一致，运维 SQL 通用）：间隔 + cron（robfig/cron 解析，时区、misfire 策略）；handler 失败/panic 按指数退避重排并计 attempts | 多副本单实例执行、attempts、fixer、指标全在一处；失败走退避而不是等 fixer，预算对 error 与 Retry 一视同仁；River 是不想自己维护时的替代（known-open #6） |
| 优雅停机 | `lifecycle.Group` 反序停止：API 先停（readiness 变 503，在途请求最多 `drain_timeout`）→ 任务 runner 排空 → 消费者停收、排空后再取消 → outbox → 连接 → admin 最后停 | grace period 从 preStop 之前开始计时，chart 设 45s = preStop 5 + 预算 25 + flush 5 + 余量；API 只拿预算的一部分，后面的组件不会被饿死；关连接池有上限 |
| 架构守卫 | `tests/arch`（`go list` 直接 import）+ DDL 规范测试 + depguard | 靠人记住的约束平均被违反两次以上才有人写下来；守卫让违规在 `go test` 时红 |
| 镜像 | 多阶段：`golang:1.27-alpine` 编译静态二进制 → `distroless/static:nonroot`，都按 digest 固定 | 无 shell、非 root、只读根文件系统（chart 打开）；镜像约 20 MB；tag 是会移动的指针，digest 让同一提交重建出同一镜像，Renovate 以 PR 形式升级 |
| 测试基础设施 | testify 不是必需：标准 `testing` + `httptest` + miniredis（单测）+ Testcontainers（集成） | 单测不需要 Docker 也能覆盖中间件、幂等、缓存、认证、handler；真依赖的行为（SKIP LOCKED、xmin、SET NX GET、TTL 梯队）只在容器里验 |
| 平台请求头 | 入站由 `PlatformHeaders` 捕获，下游调用自动带 `X-Service-Name` 并透传 `X-Request-ID`、`X-Language`；`http.required_headers` 按平面要求必带头 | 平台现有服务的内部路由要求这三个头，缺了直接 400；放进 kit 后每个 handler 都不用记。入站没有 X-Request-ID 时用 trace id 顶上，两边日志对得上 |
| 错误文案本地化 | `pkg/i18n`：按 errorCode 的多语言目录，`X-Language` / `Accept-Language` 协商，写进 RFC 9457 的 `title`；守卫测试保证每个码每种语言都有文案 | title 是"按类型"的摘要，正好一码一句，detail 留给开发者。kit 带平台码的 en / zh-CN / zh-TW / ja，服务只补自己的码 |
| 数据库可观测 | pgxpool 指标（`pgxpool_*`）+ 慢 SQL 日志（只记语句不记实参、截断 2000 字节、每秒限速、被限速的也计数） | 池满和慢查询是两种故障，各要有自己的信号。慢 SQL 的 elapsed 不含等连接的时间，排队看 `pgxpool_empty_acquire_total` |
| 积压信号 | 队列与 outbox 都报"最老积压时长"；死信按 reason 拆成 exhausted / non_retryable（`messaging.NonRetryable`）、discarded | 深度说明有多少在等，时长说明已经晚了多久，告警盯后者；主动放弃的消息是日常噪声，不能淹没真故障 |
| 任务的 trace | 任务 payload 是 JSON 对象时入队带上 `traceparent`，执行时的 span 挂在发起请求的 trace 下 | 与 outbox 一致：异步工作能从发起它的请求找到；共享的 `task_queues` 表不加列 |
| 无凭证内部路由 | `middleware.ClusterInternalOnly`：拒绝带 forwarded 头的请求并要求私网对端 | 服务间凭证签发（known-open #4）落地前的纵深防御；它不是授权 |
| Go 运行时 | chart 按 `limits.memory` 的 80% 推导 `GOMEMLIMIT`；`GOMAXPROCS` 交给 Go 1.25+ 按 cgroup 配额自动设置 | GC 只认 GOGC，不认 cgroup 上限，突发流量会在 GC 来得及跑之前被 OOMKill |
| 健康检查 | readiness 只 gating PostgreSQL；Redis、RabbitMQ 是 informational（报告里 `Degraded`，仍 200）；每项结果导出 `msvc_health_check_up` | readiness 回答"这个 pod 该不该接流量"。共享依赖一抖就摘掉所有 pod，降级变成全量 503，修复的发布也起不来；共享依赖的故障靠 `DependencyUnavailable` 告警发现 |
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
- **DLQ 里的消息怎么处理？** `{queue}.dlq` 手工检视（管理台），`x-exception` 头带失败原因；修复后用 `tools/dlq-replay.sh <queue>` 批量回放（重置重试计数）。
- **API pod 和 worker pod 都在消费？** 它们是同一队列（`{service}.{event}`）的竞争消费者，每条消息只被处理一次；想让 API pod 完全不消费，设 `RABBITMQ__CONSUMERS_ENABLED=false`（chart 在 `worker.enabled=true` 时自动设置）。
- **Redis / RabbitMQ 挂了，pod 为什么还是 Ready？** 有意的：它们是所有副本共享的依赖，摘流量会让降级变成全量 503。看 `DependencyUnavailable` 告警和 admin 端口 `/health/ready` 的报告（`Degraded`，对应项 `"gating": false`）。只有 PostgreSQL 会让 pod NotReady。
- **任务一直失败会怎样？** handler 返回 error 或 panic：按 `tasks.retry_base_delay` 起指数退避重排，一次性任务失败满 `max_attempts` 后删除并计 `msvc_tasks_dead_total{reason="exhausted"}`；周期任务不受预算限制，持续失败看 `TasksFailing` 告警。
- **限流为什么挡不住匿名洪水？** 认证失败在中间件链里先 401，到不了限流——匿名/凭证爆破类流量的防护是网关的职责。
- **outbox 积压告警**：`msvc_messaging_outbox_pending`（Grafana 面板 + Prometheus 告警样例已内置）；任务积压看 `msvc_tasks_queue_depth`。
- **审计"操作人"取值**：HTTP = JWT `sub`（或 `client_id`）；消息消费 / 后台任务 = `system`。
- **慢 SQL 在哪看？** 日志 `msg="slow query"`，带语句文本不带实参；阈值 `POSTGRES__SLOW_QUERY_THRESHOLD`，每秒限速 `POSTGRES__SLOW_QUERY_LOG_RATE`。elapsed 不含等连接的时间：主键查询也慢说明池满了，看 `pgxpool_empty_acquire_total`。
- **DLQ 告警为什么只盯 exhausted？** `msvc_messaging_dead_lettered_total{reason}` 把"重试用尽"和 handler 用 `messaging.NonRetryable` 主动放弃分开，后者是日常噪声。任务同理：`msvc_tasks_dead_total{reason}`。
- **调别的平台服务被 400 MISSING_HEADERS？** 用 `pkg/httpclient` 就不会：它自动带 X-Service-Name，透传 X-Request-ID 与 X-Language。自己 new 的 `http.Client` 不会。
- **看板和告警怎么进集群？** `deploy/grafana/import-dashboard.sh` 与 `import-alerts.sh`（见同目录 README）；导入告警后把每条规则的 No data 设为 Normal。
