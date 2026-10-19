# 架构约束

本文只列**代码里被强制执行**的部分。改动这些约束前，先改设计，再改守卫，最后改代码。

**有意留着不做的事**不在这里，在 [`known-open.md`](known-open.md)——每条写清开着的是什么、代价是什么、关掉它需要什么。

## 依赖规则

```
cmd/ → internal/app ──► internal/httpapi · internal/consumers · internal/tasks ──► internal/order
                   ──► internal/postgres · internal/inventory ──────────────────► internal/order（实现它的端口）
                   ──► github.com/guangliangus/go-msvc-kit（kit：平台基础设施，独立模块，按版本依赖）
```

- 按限界上下文分包：`internal/order` 是业务（聚合 + 用例服务 + 端口接口 + 集成事件），只 import 标准库、`golang.org/x/sync`、`github.com/shopspring/decimal`（金额）和 kit 的抽象（`apperror`、`paging`、`cache`、`messaging`、`money`），**不** import 任何驱动（pgx、amqp091、chi）或适配器。它的测试假实现在 `internal/order/ordertest`，同样守这条规则。
- 适配器按依赖命名：`internal/postgres`、`internal/inventory` 实现 `order` 的端口；`internal/httpapi` 持有请求/响应类型，只调用 `order.Service`。只有组合根 `internal/app` 同时看见端口与实现。
- kit 是独立的私有模块（README 的"平台 kit"）。它是另一个模块，Go 不允许它 import 本服务的 `internal/`，这个方向不需要守卫。kit 自己的表（outbox / inbox / task_queues）的 DDL 随 kit 的版本发布：`db/kit_*.sql` 是逐字节副本，`tests/arch/kitddl_test.go` 拿它们和 kit 内嵌的版本比对（`db/README.md`）。
- `tests/arch/deps_test.go` 用 `go list` 检查**直接 import**，违规让 `go test ./...` 失败。它是表驱动的：文件顶部四张名单（`businessContexts` / `adapters` / `entryPoints` / `compositionRoot`），规则对名单循环；`TestGuardedPackagesExist` 在名单里的目录不存在时失败，`listPackages` 在 `go list` 匹配到零个包时失败——`go list` 对空模式只打警告，一条在零个包上检查的规则会永远绿。新增或改名一个上下文 / 适配器必须改这张表。
- `.golangci.yml` 的 `depguard` 在 lint 时再查一遍，方向相反：`order-stays-pure` 覆盖 `internal/` 下**除**组合根与适配器（`app`、`httpapi`、`postgres`、`inventory`，用 `!` 否定）之外的一切，所以新上下文自动受管，新适配器要写进否定列表；另一条规则禁掉 gin / echo / GORM / logrus / zap，与 kit 的 lint 配置相同。
- 还有两个守卫不在 `tests/arch`：`internal/httpapi/openapi_test.go` 遍历路由器，与 `api/openapi.yaml` 的 `paths` 双向比对（有路由没文档、有文档没路由都红）；`tests/arch/kitddl_test.go` 把 `db/kit_*.sql` 与 kit 内嵌的脚本逐字节比对。

**什么该进 ports**：只有**跨越进程边界**的接口（DB / HTTP / Redis / MQ），或单测里需要替换、实现可能变的东西。纯计算、校验器、映射函数不进去。

## 对象的生命周期：依赖走字段，请求数据走 ctx

没有 DI 容器。对象按"活多久"分四档，各用 Go 自己的机制：

| 活多久 | 怎么管 | 例子 | 怎么结束 |
|---|---|---|---|
| 进程 | `internal/app.build` 构造一次，经构造函数传入、存成字段 | `postgres.DB`、`redisx.Client`、`order.Service`、各 handler、`auth.Validator`、`cache.Cache` | 持有外部资源的用 `lifecycle.Closers` 登记（`closers.Add`），组件全停之后倒序关闭、每个最多等一个超时（见停机预算）；其余交给 GC |
| 后台组件 | 实现 `lifecycle.Runner`；`group.Add` 的顺序是启动顺序，反过来是停止顺序 | admin / API 监听、rabbitmq 连接与 kafka 客户端、每个 bus 的 outbox 投递器与消费者宿主、任务 runner | `group.Run` 取消它的 ctx，在预算内等它返回 |
| 一次请求 / 消息 / 任务 | 放在 `context.Context` 上，作为每个方法的第一个参数往下传 | 事务（`db.WithTx`）、`auth.Principal`、`httpx.ErrorWriter`、`reqmeta`、`actor`、span、截止时间 | 创建它的函数返回即结束：`WithTx` 在 defer 里提交或回滚；请求结束后 ctx 不再被引用 |
| 一次调用 | 局部变量 | 聚合、请求 / 响应类型 | GC |

- 依赖**不进** ctx：`context` 包的约定是 ctx 只放跨 API 边界的请求范围数据。handler、用例服务、仓储需要什么就在构造函数里声明什么，组合根之外没有地方能"取出"一个依赖。
- 进程级对象被所有请求并发使用，**字段里不放任何请求状态**；需要可变状态就自己加锁（`cache` 的 L1、`DevHeaders` 的 `sync.Once`）。
- ctx 上的事务不跨 goroutine：`pgx.Tx` 不是并发安全的，`WithTx` 回调拿到的 ctx 不要交给另起的 goroutine。
- 要保留 ctx 上的值、去掉它的取消，用 `context.WithoutCancel`（任务 handler、`AfterCommit` 钩子）。
- 没有"请求结束"的统一回调：要清理的东西由创建它的函数 `defer`；提交之后才能做的库外副作用挂 `db.AfterCommit`。

## 平台契约：哪些东西不能自己发明

C# 模版只是参考，本模板按 Go 生态的惯例组织（标准库优先、`net/http` 中间件、按限界上下文分包、接口定义在使用方、组合根显式装配）。下面这些是**平台级契约**：服务之间要互通，所以它们不是语言偏好。它们实现在 kit 里，所以改一项契约 = kit 的一次发版 **加上** 和平台上的现有服务（含 C# 服务、user-svc）的一次协调。

| 契约 | 内容 | 谁依赖它 |
|---|---|---|
| 错误响应 | RFC 9457 + `errorCode`（UPPER_SNAKE）+ `traceId`（裸 hex，与 `X-Trace-ID` 同值）+ `urn:msvc:error:<kebab>` | 客户端、网关、排障 |
| 令牌 | user-svc 签发的 HS256，`realm` 声明区分平面，scope 字符串 | 所有受保护端点 |
| 消息线格式 | AMQP 头 `x-event-name` / `x-attempt` / `traceparent` / `x-occurred-at`，属性 `app-id` 是发布方服务名；body 带 `messageId` / `occurredAt`；队列 `messaging.QueueName` = `{service}.{event}`，`{queue}.retry.N` / `{queue}.dlq`；进 DLQ 的副本另带 `x-exception`、`x-dead-letter-reason`（exhausted / non_retryable）、`x-failed-at`（RFC 3339 UTC）、`x-origin-queue` | 跨语言的生产者与消费者、`tools/dlq-replay.sh`、chart 的 KEDA `queueName` |
| Kafka 线格式 | **平台的 Kafka 契约，由本模板（kit 的 `kafka` 包）首次定义**——平台此前没有 Kafka，之后接入的服务（含 C#）照它来。topic = 事件名（`kafka.topics` 可逐事件覆盖）；record key = 分区键（实现 `messaging.Keyed` 的事件给聚合 id，否则无 key）；value 与 AMQP body 相同；头全是字符串：`x-event-name`、`x-attempt`、`x-occurred-at`、`traceparent`，以及代替 AMQP 属性的 `x-message-id`、`x-correlation-id`、`content-type`、`x-app-id`（发布方服务名）；consumer group = `messaging.QueueName` = `{service}.{event}`，重试 topic `{group}.retry.N`、死信 topic `{group}.dlq` 归 group 所有，死信副本另带 `x-exception`、`x-dead-letter-reason`、`x-failed-at`、`x-origin-queue`（group）、`x-origin-topic`。完整规则（分区器、重试副本的时间戳、缺 `x-message-id` 时的 id）以 kit README 的平台契约与 `kafka` 包文档为准 | 跨语言的生产者与消费者、chart 的 KEDA `kafka.consumerGroup` / `kafka.topic` |
| 指标名 | 族：`http_server_request_duration_seconds`（OTel 语义约定）、`msvc_messaging_*`、`msvc_tasks_*`、`msvc_http_*`、`msvc_db_*`、`msvc_resilience_*`、`msvc_cache_*`、`msvc_health_check_up`、`pgxpool_*`。完整清单（含标签）以 kit README 的平台契约一节为准 | 共享的 Grafana 看板与告警规则 |
| 运维表设计 | `task_queues` / `outbox_messages` / `inbox_messages` 的列与状态值 | 跨服务的运维 SQL 与 runbook（库本身每个服务各自一套） |

契约之外的一切（分包、路由器、配置库、日志、数据访问、测试方式）以 Go 的惯例为准，理由见 README 的决策记录。

## 平台请求头

- 入站：`middleware.PlatformHeaders` 把 `X-Request-ID`、`X-Language`、`X-Service-Name`、`X-Platform`、`X-Device-ID`、`X-App-Version`、`X-Currency`、`X-Region` 捕获到 ctx（kit 的 `reqmeta`）；超过 128 字节或含控制字符的值直接丢弃。请求日志记 `request_id`。
- 出站：kit 的 `httpclient` 自动带 `X-Service-Name`（本服务名），透传 `X-Request-ID` 与 `X-Language`（`service_clients.*.propagate_headers` 可改）；入站没有 `X-Request-ID` 时用 trace id 顶上。设备类头默认不透传，需要的下游显式要。
- 强制：`http.required_headers.consumer / backoffice` 列出的头缺失即 400 `MISSING_HEADERS`，缺的名字在 `errors` 里。平台 C 端 App 固定发 `X-Platform`、`X-Device-ID`、`X-Request-ID`、`X-Language`，服务真的用到时才列进去。
- 若需要无凭证的 `/internal` 路由时，给那个路由组套 kit 的 `middleware.ClusterInternalOnly`：带 forwarded 头的请求一律拒绝，对端必须是私网地址。它不是授权，是 ingress 规则配错时的兜底。模板本身没有挂这样的路由。

## 错误契约：RFC 9457，没有信封

- 成功响应就是 DTO 本身，没有 `{ success, data }`。列表两种分页：有界的列表（一个客户自己的订单）用页码 `paging.Result[T]`；无界的列表（后台看全部订单）用 keyset 游标 `paging.CursorResult[T]`，不数总数、不用 OFFSET，每页代价相同。分页参数：`page` / `pageSize` / `limit` 不是整数 → 400 `VALIDATION_FAILED` 并在 `errors` 里点名参数（把 `?page=abc` 当成"没给"会静默返回第一页）；越界的数字夹到边界（`pageSize` 上限 200），只有 `page` 超过 10000 拒绝（那么深的列表该用游标）；解不开的 `cursor` 400。
- 校验分两层，按"客户端错在哪"分状态码。请求的**形状**归 `httpapi`：请求类型的 struct tag 检查字段有没有、类型、长度与范围，答 400 `VALIDATION_FAILED`，`errors` 里一次列出所有出错字段，业务代码一行都不跑。业务**规则**归上下文包：条数上限（`order.MaxItems`）、能不能卖、金额上限，答 422 带上下文自己的码（`ORDER_ITEMS_REQUIRED`、`ORDER_ITEM_INVALID`、`ORDER_ITEM_UNAVAILABLE`、`ORDER_TOTAL_TOO_LARGE`）。规则只在上下文包里存一份，tag 故意不复制它：消费者和任务不经过 tag，聚合不信任调用方。
- 金额在线上是 JSON 数字（`39.98`），由类型 `money.Amount` 决定，不靠包级全局开关：响应体与事件 payload 里的金额字段一律用它，测试与生产看到的是同一种格式。
- 失败是 ProblemDetails + **真实 HTTP 状态码**，`errorCode` / `traceId` 走扩展成员；`traceId` 是裸 32 位 hex，与日志里的 `trace_id` 和响应头 `X-Trace-ID` 是同一个值。
- Handler 返回 `error`，不写错误响应；`*apperror.Error` 按它的 status/code 写出，其余一律 500 `INTERNAL_ERROR` 并把原因记在日志里（永不泄露给客户端）。状态码按**客户端该做什么反应**分组：400 改一下重提交 · 401 去重新认证 · 403 别再试了 · 404 不存在 · 409 状态冲突 · 422 违反业务规则 · 429 限流 · 502 上游挂了 · 504 超时。
- `errorCode` 是客户端契约，UPPER_SNAKE_CASE，**只增不改**。平台通用码在 kit 的 `apperror/codes.go`（平台统一词汇表，user-svc 与其他服务共用），本服务自己的码放在它的上下文包（`internal/order` 的 `Code*`）。
- 路由器自己产生的 404/405 也走同一形状（`router.go` 的 `NotFound` / `MethodNotAllowed`）。
- `title` 由 kit 的 `i18n` 目录按调用方语言填（`X-Language` 优先，其次 `Accept-Language`，回退英文），`detail` 保持开发者可读的英文句子。kit 带平台码的 en / zh-CN / zh-TW / ja，服务在 `internal/httpapi/locales` 补自己的码，`locales_test.go` 保证每个码每种语言都有。
- 每个写出的问题都计入 `msvc_http_problems_total{http_route,http_response_status_code,error_code}`，请求日志同时带 `error_code`，告警可以只盯一个码。

## 一个缺失的能力只能弄坏它自己

- 每个部署都必须有的配置缺了就拒绝启动：连接串、Redis、broker 由 kit 各段的 struct tag 与 `Validate()` 管（kit 的 `conf` 在启动前校验全部 struct tag 和 `Validate()` 方法，含嵌套段）；`auth.signing_key` 由本服务的 `internal/config.Config.Validate` 要求——kit 不再对每个服务都要密钥（只有集群内路由的服务没有令牌可验），本服务验两个平面的 user-svc 令牌，所以在读配置时就拒绝，而不是等到构造 validator。`default.yaml` 是生产基线，不带任何 localhost：compose 的地址只在 `development.yaml`，production 缺 `POSTGRES__DSN` / `REDIS__ADDR` / `RABBITMQ__URL` / `AUTH__SIGNING_KEY` 之一就起不来（`config_test.go`）。
- Kafka 默认关：打开后 `kafka.brokers` 必填（kit 的校验）；`kafka.events` 由本服务的 `KafkaConfig.Validate` 检查——Kafka 关着时必须为空（否则这些事件永远发不出去）、不得重复、事件名（或它在 `kafka.topics` 的覆盖）必须是合法 topic 名。
- 只有某个能力才需要的配置（某个下游的 `base_url`）不阻止启动：`httpclient.New` 构造出一个"未配置"的客户端，第一次调用答 500 `NOT_CONFIGURED` 并在 detail 里点名缺的段。
- 健康检查只报依赖状态：PostgreSQL 不可达时服务照样启动、readiness 报不健康（不要 CrashLoop，等它回来）。
- readiness 回答"**这个** pod 该不该接流量"，不是"一切都好吗"。只有离开它本 pod 就答不对的依赖才 gating（`AddReadiness`，现在只有 PostgreSQL）；所有副本共享、调用方会降级绕开的依赖（Redis、RabbitMQ、Kafka）登记为 informational（`AddInformational`）：报告里照样显示（整体 `Degraded`，仍是 200），但不摘流量——否则它一抖，所有 pod 同时被摘出 Service，入口只剩裸 503，修复它的那次发布也起不来。
- 每项检查的最新结果导出为 `msvc_health_check_up{check,gating}`，`Registry.Watch` 每 15s 刷一次（不依赖探针），`DependencyUnavailable` 告警盯它：共享依赖的故障靠告警发现，不靠摘流量发现。

## 两个监听端口

- `server.addr`（8080）只有 API；`server.admin_addr`（8081）只有三个探针和 `/metrics`。Ingress 只指向 API 端口，所以探针与指标永远不会经它暴露；API 被打满时探针也不受影响。worker 没有 API 监听，只起 admin。
- admin 的 handler 是 kit 的 `httpserver.AdminHandler(registry, metrics)`：一个裸 `http.ServeMux`，不套任何中间件（kubelet 每几秒探一次，日志、span、认证都是噪声），GET 以外一律 405。API 监听上的 `/docs/`（Scalar 页面）与 `/docs/openapi.yaml`（文档本身）是 kit 的 `httpx.Docs`，挂在中间件链外面，只在 `http.docs_enabled` **且** `APP_ENV=development` 时存在。
- 生命周期里 admin 最先注册、最后停止：排空期间 readiness 一直答 503，指标一直可抓。chart 的探针与 ServiceMonitor 都指向 `admin` 端口。

## 停机预算

K8s 的 `terminationGracePeriodSeconds` 从 preStop **之前**开始计时，所以它必须盖住 preStop + `server.shutdown_timeout` + telemetry flush（5s）+ 余量：chart 用 5 + 30 + 5 + 10 = 50s。

- `shutdown_timeout`（30s）是所有组件按停止顺序共享的总预算；API 监听只拿其中的 `server.drain_timeout`（12s，启动校验必须小于总预算），超时就切断仍在跑的请求——否则一个慢请求能吃光全部预算，后面的消费者、任务、outbox 拿到 0 秒被直接放弃。
- 消费者先停止接收新消息，在途 handler 有 `rabbitmq.drain_timeout` / `kafka.drain_timeout`（各 5s）跑完，之后才取消（RabbitMQ 的消息 requeue；Kafka 的记录不提交，由下一个拿到分区的副本重新消费）；任务 runner 同理（`tasks.drain_timeout`）。默认声明的排空之和 12 + 5 + 5 + 5 = 27s，小于 30s，给 outbox、连接与 admin 留出余量；`internal/config` 的测试锁住这条不等式。
- 每个有排空时间的组件在注册时**声明**它：`group.Add("api", …, lifecycle.WithDrain(cfg.Server.DrainTimeout))`，consumers、kafka-consumers 与 tasks 同样。group 不强制执行它（各组件自己兜住自己的停止），但在 `Run` 启动时把声明按停止顺序加总：排在前面的声明之和已经达到 `shutdown_timeout` 时，后面的组件轮到它就会被立刻放弃——这种配置在**启动时**就 Warn 并点名，而不是等第一次 SIGTERM。
- 组件停完后由 `lifecycle.Closers` 倒序关闭连接池，每个最多等 3s：被预算放弃的组件可能还占着连接，`pgxpool.Close` 会一直等它；超时的 close 留在后台，进程照常退出。

## 数据库

`db/*.sql` 是 schema 的唯一真相，手动执行、幂等、先 DDL 后发代码；应用启动永不改库，没有 migration 框架。团队 DDL 规范是 kit 的 `ddlguard` 包，`tests/arch/ddl_test.go` 只有一行 `ddlguard.Check(t, "../../db")`，每次 `go test` 跑；有意的偏离用 `-- ddl-allow: <rule>  <理由>` 写在那一行（规则 id 与豁免写法见 `db/README.md`）。集成测试（Testcontainers）用这些脚本建库，所以仓储层 SQL 与 DDL 的漂移在测试里暴露。

- kit 自己的表随 kit 发版：`db/kit_0001_messaging.sql` 的 `outbox_messages` 有 `next_attempt_at`（被 broker 拒收后按指数退避推后的最早重投时间，`outbox.retry_base_delay` → `retry_max_delay`）、`partition_key`（事件的分区键，Kafka 的 record key，空串 = 无键）和部分索引 `(next_attempt_at, id) WHERE status = 'PENDING'`。早于这一版建的本地库光重跑脚本补不上——`CREATE TABLE IF NOT EXISTS` 不加列，新列的 `COMMENT ON COLUMN` 因此失败，`CREATE INDEX IF NOT EXISTS` 看到同名旧索引就跳过——要么 `make down && make up` 重建，要么执行 `db/README.md` 里的两句 `ALTER TABLE … ADD COLUMN IF NOT EXISTS`（`next_attempt_at`、`partition_key`）加 `DROP INDEX` 再重跑脚本。

- 事务随 `context` 传递：仓储方法一律从 `db.Q(ctx)` 取执行器，于是 `db.WithTx(ctx, fn)` 里面的所有写（业务行、outbox 行、任务行）同一个事务提交。嵌套 `WithTx` 加入外层事务。
- 库外的副作用（删缓存）用 `db.AfterCommit(ctx, fn)` 挂到**最外层事务提交之后**，回滚则不执行，不在事务里则立即执行。提交前删缓存会让并发读把旧行重新写回缓存，一个 TTL 内都是旧值。用例服务通过 `Transactor` 端口拿到它。
- 每个会话设 `idle_in_transaction_session_timeout`（`postgres.idle_in_transaction_timeout`，默认 60s）兜底：事务里不许等任何慢东西（broker、下游），真等了由数据库结束它，而不是一直占着行锁和 vacuum 视界。
- `WithTxRetry` 只在序列化失败/死锁（40001/40P01）时**整段重跑**，里面的工作必须可重复。
- 乐观并发用 `xmin`：仓储 `UPDATE ... WHERE id = $1 AND xmin = $4`，零行即 409 `CONCURRENCY_CONFLICT`。零 schema 成本。
- 审计列由仓储显式写，操作人来自 `actor.FromContext(ctx)`：HTTP = token 的 `sub`/`client_id`，消费者与后台任务 = `system`。
- 慢 SQL 日志只记语句不记实参（`postgres.slow_query_threshold`，每秒限速 `slow_query_log_rate`，被限速的仍计入 `msvc_db_slow_queries_total{logged="false"}`）。elapsed 不含等连接的时间，排队看 `pgxpool_empty_acquire_total`；池的 `pgxpool_*` 指标在 `postgres.Open` 里注册。

## 认证：两个平面，一把密钥

平台用户服务（user-svc）给 C 端用户和后台运营两个平面签发令牌，同一个签发方、同一把 HS256 密钥，两个平面各自独立编号——消费者 5 号和运营 5 号是两个人、同一个整数。令牌上的 `realm` 声明是区分两者的唯一信号，所以：

- **每组路由声明自己服务的平面**：`auth.RequireRealm(auth.RealmConsumer)` 或 `RealmBackOffice`。裸 `auth.Require()` 不算——运营令牌照样满足它，接着就被当成消费者 id 去查库。
- `/api/v1` 下只有两个平面组，组上只挂整个平面共有的东西（realm、必带头）。scope 属于某一个上下文，挂在该上下文自己的子路由上（`OrdersHandler.BackOfficeRoutes` 里的 `RequireScope(ScopeOrdersRead)`）；放在平面组上，下一个挂进来的上下文就会被要求别人的 scope。
- 缺 `realm` 的令牌不满足任何平面策略：缺失也是畸形令牌、外来令牌的样子，基于缺失的判断是 fail-open。
- 认证中间件是一条 **scheme 链**：`auth.Authenticate(log, validator, devHeaders)` 按参数顺序问每个 scheme 认不认得这个请求，第一个认得的建立 principal；凭证被拒就地 401（坏令牌绝不会以"匿名"身份到达 handler，没人认得的 `Authorization` 头同样拒绝），完全没有凭证的请求以匿名继续、由 `Require*` 决定。要接别的凭证（将来的 B2B 签发方、服务间凭证）就再实现一个 `auth.Scheme` 加进链里，路由与 handler 不动。
- 签名密钥只从环境进来（`AUTH__SIGNING_KEY`，Secret），缺了拒绝启动——这是本服务 `internal/config.Config.Validate` 的规则（kit 不再对每个服务都要密钥）。拿到它的人能铸出任何服务都接受的令牌。
- 轮换密钥不需要全平台同一时刻切换：验证端可以同时认多把（`AUTH__ADDITIONAL_SIGNING_KEYS`，逗号分隔）。顺序是：各服务先把新密钥加进 additional → user-svc 开始用新密钥签 → 各服务把新密钥提为 `signing_key`、旧的留在 additional → 过了最长令牌有效期后删掉旧的。令牌格式不变，不涉及平台契约。
- 开发请求头认证（`X-Dev-Subject` 等）**在代码里**只认 `APP_ENV=development`（`auth.DevelopmentEnvironment`），配置无法在别的环境打开它；`/docs` 同样：`http.docs_enabled` 泄漏进集群的 config map 也不会挂出任何东西。

## 中间件顺序里不能动的地方

见 `internal/httpapi/router.go` 的注释：

1. `ClientIPResolver` 第一个——它决定后面所有人看到的客户端地址；`PlatformHeaders` 紧随其后，之后任何调下游的代码都能透传平台头。
2. `otelhttp` 在请求日志**外面**——日志行要带 trace_id。
3. `RequestLogger` 在 `Recoverer` **外面**——它记录客户端真正收到的状态，被处理成 400 的校验失败绝不能记成 500，否则基于请求日志的 SLO 看板会把自己的 4xx 读成自己的宕机。
4. `Authenticate` 在限流之前（按调用方分桶）、两者都在 `Idempotency` 之前（key 按调用方隔离，重放也要占预算）。

## 请求超时为什么可以放在应用里

平台上有服务曾经因为一个请求级超时中间件在压测中毒化 keep-alive 连接，而把所有 server 超时整体撤掉、全交给网关。那个中间件在超时后替 handler 写了响应，handler 稍后又写了一次，同一条连接上两份响应，后面的请求全部错位。

模板的 `RequestTimeout` 只做一件事：给 ctx 设截止时间。到期后 handler 自己返回 `context.DeadlineExceeded`，错误写入器答 504，始终只有一个写响应的人。`server.write_timeout` 必须大于 `request_timeout`（启动校验），否则连接会在 504 写出前被切断。所以 server 级超时保留，不交给网关。

## 重试只对幂等操作做

kit 的 `resilience` 和 `httpclient` 能替你分类哪些失败是瞬态的，**分不清你的操作能不能重复执行**。超时之后没有任何谓词知道第一次尝试是否已经提交。所以：

- 读操作、`ON CONFLICT DO NOTHING`、带 `Idempotency-Key` 的写——可以重试。HTTP 客户端只自动重试幂等方法（GET/HEAD/OPTIONS/PUT/DELETE）或带 `Idempotency-Key` 的请求。
- 裸 INSERT、扣款、发消息——不要放进重试，或者先给它一个幂等键。
- 熔断器**默认只架在单个下游前面**（`service_clients.*.breaker`）；共享资源（数据库、Redis）前不放熔断器——一个病态调用方打开它就拒掉所有不相干调用。

## 失败语义：判断标准是「降级后回落到什么」

下面有东西垫着 → 可以 fail-open；自己就是最后一道 → fail-closed。新增一个跨进程依赖时，**在这张表里加一行**再写代码。

| 组件 | 失败时 | 为什么 |
|---|---|---|
| PostgreSQL | fail-closed，`/health/ready` 报不健康（gating：pod 被摘出 Service） | 没有库就给不出正确答案 |
| Redis（整体） | readiness 只报告不摘流量（informational），`DependencyUnavailable` 告警 | 所有副本同时受影响，摘流量只会把降级变成全量 503；下面两行各自决定降级方式 |
| Redis · HTTP 幂等键 | fail-closed，503 `IDEMPOTENCY_STORE_UNAVAILABLE` + Retry-After | 幂等键底下没有兜底：存储不可达时放行等于把"绝不能重复"的写静默变成可重复 |
| Redis · 缓存 | fail-open，回源并记 warn | 数据库仍然是真相 |
| Redis · 业务限流（`FixedWindowAllow`） | 返回 allow + error，调用方 fail-open | 它保护的端点仍会被认证、校验、审计；抖动期间全拒会让限流器亲手造成它要防的那次故障 |
| RabbitMQ 不可达 | readiness 只报告不摘流量；投递器不认领行（不开事务、不锁行、不花 attempts），outbox 行堆积为 PENDING，积压指标照常采样（`msvc_messaging_outbox_oldest_pending_age_seconds` 会涨、会告警）；消费者自动重连 | 事件永不丢，只是晚到。只有 broker 拒收这条消息本身才计 attempts，满了停成 FAILED |
| Kafka 不可达 | readiness 只报告不摘流量（informational，客户端每 `kafka.ping_interval` 探一次）；Kafka 的投递器不认领 `kafka.events` 的行，它们堆积为 PENDING（积压指标照常、会告警），超时与连不上不花 attempts；RabbitMQ 的投递器只认领自己的事件，照常投递；消费者由客户端自己重连，回来后从已提交的 offset 接着消费（只提交已处理或已转到重试 / 死信 topic 的 record） | 与 RabbitMQ 同理，事件永不丢、只是晚到；每个 bus 一个投递器，一个 broker 宕掉不拖住另一个的事件 |
| 下游服务 5xx / 超时 / 熔断开 | 502 `UPSTREAM_SERVICE_UNAVAILABLE` | 上游的错，不是调用方的错 |
| 下游服务 4xx | 500 类（`*httpclient.StatusError`），响应体只记日志 | 是**我们**发错了；变成 502 等于甩锅给上游 |

## 任务队列的两个时钟和一个窗口

- 所有"到期"比较和相对重排都用**数据库的 `now()`**，pod 时钟漂移不改变任务何时运行。
- `tasks.task_timeout` 必须**严格小于** `tasks.stale_after`（启动校验）：两者相等时 fixer 会放开 handler 还在跑的行，同一任务并发多份。
- handler 的 ctx **只**由超时取消，停机绝不取消它：排空有上界（`drain_timeout`），超时未完成的行保持 claimed 交给 fixer——一个慢 handler 不会拖过 `terminationGracePeriodSeconds` 变成 SIGKILL。
- handler 返回 error 或 panic 都按失败处理：花一次 attempts，按指数退避（`tasks.retry_base_delay` 起、翻倍、封顶 `retry_max_delay`）重排；一次性任务用满 `max_attempts` 即 dead（`msvc_tasks_dead_total{reason="exhausted"}`），周期任务不受预算限制。失败计入 `msvc_tasks_failed_total`；`reclaimed` 只表示持有任务的 worker 没了（崩溃、排空超时）。panic 在 runner 里被 recover，绝不打崩进程。
- 任务 payload 是 JSON 对象时，入队会带上 `traceparent`，执行时的 span 挂在发起请求的 trace 下；非对象 payload 原样不动。fixer 每轮同时采样每个队列的深度与最老积压时长（`msvc_tasks_oldest_pending_age_seconds`）。

## 两条消息总线，每个事件只在一条上

- `kafka.events` 列出的事件发布到 Kafka、也从 Kafka 消费，其余事件在 RabbitMQ（`KafkaConfig.OnKafka`；Kafka 关着时一律 RabbitMQ）。生产者和所有消费者必须对此一致，所以这张列表随平台决策改，不是一个服务的开关。
- 每个 bus 一个 outbox 投递器，读同一张 `outbox_messages`、各认领自己的行：RabbitMQ 的带 `messaging.ExceptEvents(kafka.events...)`，Kafka 的带 `messaging.ForEvents(kafka.events...)`。积压指标与清理覆盖全部 PENDING 行，每个进程只由一个投递器做：RabbitMQ 启用时是它，Kafka 的加 `messaging.WithoutHousekeeping()`；RabbitMQ 关着时由 Kafka 的做。一个 broker 宕掉只让它自己的事件堆积。
- 订阅在 `internal/app/messaging.go` 的 `subscriptions` 里声明一次，`splitByBus` 按事件交给 RabbitMQ 或 Kafka 的消费者宿主（`messaging_test.go` 锁住"每个订阅恰好去一边"）；队列名与 consumer group 都是 `messaging.QueueName(service, event)`。走 Kafka 的事件不再声明 RabbitMQ 队列。
- 生命周期组件：`rabbitmq`、`outbox`、`consumers`；`kafka`（客户端，先注册所以后停）、`kafka-outbox`、`kafka-consumers`（`WithDrain(kafka.drain_timeout)`）。readiness 的 `rabbitmq` 与 `kafka` 都是 informational。
- Kafka 上每个分到的分区一个 worker 按 offset 顺序处理，并行度 = 分到的分区数；`Subscription.Concurrency` / `Prefetch` 只对 RabbitMQ 生效。实现 `messaging.Keyed` 的事件（`order.CreatedEvent` 以订单 id 为 key）同一 key 落在同一分区、按发布顺序处理；失败的 record 转到 `{group}.retry.N` 就离开了分区，不堵住后面的 record，代价是它不再与同 key 的后续 record 保序。

## 消息消费者不能被一条消息打崩

- handler panic 在消费者宿主里被 recover，按普通失败走重试梯队，最后进 DLQ（`exhausted`，会告警：panic 是 bug）；毒消息不会在所有副本之间轮流打崩进程。
- payload 解码失败由 `messaging.Subscribe` 直接标成 `NonRetryable`：现在解不开的消息十分钟后也解不开，直接进 DLQ 记 `non_retryable`，不走梯队、不触发 `exhausted` 告警。
- RabbitMQ 的发布（outbox 投递与消费端的重试/死信路由共用一个 publisher）整体受 `rabbitmq.publish_timeout` 约束：等连接、等通道锁、等确认都在这一个时限里，broker 不在时调用方很快拿到错误，而不是攥着事务或通道锁一直等。Kafka 的一次发布受 `kafka.publish_timeout` 约束（等到所有同步副本写入：acks=all、幂等生产者）；消费端转到重试 / 死信 topic 失败时带退避重试，转不走的 record 不提交 offset，不会丢。
