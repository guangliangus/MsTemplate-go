# 架构约束

本文只列**代码里被强制执行**的部分。改动这些约束前，先改设计，再改守卫，最后改代码。

**有意留着不做的事**不在这里，在 [`known-open.md`](known-open.md)——每条写清开着的是什么、代价是什么、关掉它需要什么。

## 依赖规则

```
cmd/ → internal/app ──► internal/httpapi · internal/consumers · internal/tasks ──► internal/order
                   ──► internal/postgres · internal/inventory ──────────────────► internal/order（实现它的端口）
                   ──► pkg/*（kit：平台基础设施，永不 import internal/）
```

- 按限界上下文分包：`internal/order` 是业务（聚合 + 用例服务 + 端口接口 + 集成事件），只 import 标准库、`golang.org/x/sync` 和 kit 的抽象（`pkg/apperror`、`pkg/paging`、`pkg/cache`、`pkg/messaging`、`pkg/money`），**不** import 任何驱动（pgx、amqp091、chi）或适配器。
- 适配器按依赖命名：`internal/postgres`、`internal/inventory` 实现 `order` 的端口；`internal/httpapi` 持有请求/响应类型，只调用 `order.Service`。只有组合根 `internal/app` 同时看见端口与实现。
- `pkg/` 是将来要抽成共享模块的 kit：它**不能** import `internal/`。
- `tests/arch/deps_test.go` 用 `go list` 检查**直接 import**，违规让 `go test ./...` 失败；`.golangci.yml` 的 `depguard` 在 lint 时再查一遍。

**什么该进 ports**：只有**跨越进程边界**的接口（DB / HTTP / Redis / MQ），或单测里需要替换、实现可能变的东西。纯计算、校验器、映射函数不进去。

## 对象的生命周期：依赖走字段，请求数据走 ctx

没有 DI 容器。对象按"活多久"分四档，各用 Go 自己的机制：

| 活多久 | 怎么管 | 例子 | 怎么结束 |
|---|---|---|---|
| 进程 | `internal/app.build` 构造一次，经构造函数传入、存成字段 | `postgres.DB`、`redisx.Client`、`order.Service`、各 handler、`auth.Validator`、`cache.Cache` | 持有外部资源的用 `onClose` 登记，组件全停之后倒序关闭（见停机预算）；其余交给 GC |
| 后台组件 | 实现 `lifecycle.Runner`；`group.Add` 的顺序是启动顺序，反过来是停止顺序 | admin / API 监听、rabbitmq 连接、outbox、消费者、任务 runner | `group.Run` 取消它的 ctx，在预算内等它返回 |
| 一次请求 / 消息 / 任务 | 放在 `context.Context` 上，作为每个方法的第一个参数往下传 | 事务（`db.WithTx`）、`auth.Principal`、`httpx.ErrorWriter`、`reqmeta`、`actor`、span、截止时间 | 创建它的函数返回即结束：`WithTx` 在 defer 里提交或回滚；请求结束后 ctx 不再被引用 |
| 一次调用 | 局部变量 | 聚合、请求 / 响应类型 | GC |

- 依赖**不进** ctx：`context` 包的约定是 ctx 只放跨 API 边界的请求范围数据。handler、用例服务、仓储需要什么就在构造函数里声明什么，组合根之外没有地方能"取出"一个依赖。
- 进程级对象被所有请求并发使用，**字段里不放任何请求状态**；需要可变状态就自己加锁（`cache` 的 L1、`DevHeaders` 的 `sync.Once`）。
- ctx 上的事务不跨 goroutine：`pgx.Tx` 不是并发安全的，`WithTx` 回调拿到的 ctx 不要交给另起的 goroutine。
- 要保留 ctx 上的值、去掉它的取消，用 `context.WithoutCancel`（任务 handler、`AfterCommit` 钩子）。
- 没有"请求结束"的统一回调：要清理的东西由创建它的函数 `defer`；提交之后才能做的库外副作用挂 `db.AfterCommit`。

## 平台契约：哪些东西不能自己发明

C# 模版只是参考，本模板按 Go 生态的惯例组织（标准库优先、`net/http` 中间件、按限界上下文分包、接口定义在使用方、组合根显式装配）。下面这些是**平台级契约**：服务之间要互通，所以它们不是语言偏好。改它们要和平台上的现有服务（含 C# 服务、user-svc）一起改。

| 契约 | 内容 | 谁依赖它 |
|---|---|---|
| 错误响应 | RFC 9457 + `errorCode`（UPPER_SNAKE）+ `traceId`（裸 hex，与 `X-Trace-ID` 同值）+ `urn:msvc:error:<kebab>` | 客户端、网关、排障 |
| 令牌 | user-svc 签发的 HS256，`realm` 声明区分平面，scope 字符串 | 所有受保护端点 |
| 消息线格式 | AMQP 头 `x-event-name` / `x-attempt` / `traceparent` / `x-exception` / `x-occurred-at`；body 带 `messageId` / `occurredAt`；队列 `{queue}.retry.N` / `{queue}.dlq` | 跨语言的生产者与消费者、`tools/dlq-replay.sh` |
| 指标名 | `http_server_request_duration_seconds`（OTel 语义约定）、`msvc_messaging_*`、`msvc_tasks_*` | 共享的 Grafana 看板与告警规则 |
| 运维表设计 | `task_queues` / `outbox_messages` / `inbox_messages` 的列与状态值 | 跨服务的运维 SQL 与 runbook（库本身每个服务各自一套） |

契约之外的一切（分包、路由器、配置库、日志、数据访问、测试方式）以 Go 的惯例为准，理由见 README 的决策记录。

## 平台请求头

- 入站：`middleware.PlatformHeaders` 把 `X-Request-ID`、`X-Language`、`X-Service-Name`、`X-Platform`、`X-Device-ID`、`X-App-Version`、`X-Currency`、`X-Region` 捕获到 ctx（`pkg/reqmeta`）；超过 128 字节或含控制字符的值直接丢弃。请求日志记 `request_id`。
- 出站：`pkg/httpclient` 自动带 `X-Service-Name`（本服务名），透传 `X-Request-ID` 与 `X-Language`（`service_clients.*.propagate_headers` 可改）；入站没有 `X-Request-ID` 时用 trace id 顶上。设备类头默认不透传，需要的下游显式要。
- 强制：`http.required_headers.consumer / backoffice` 列出的头缺失即 400 `MISSING_HEADERS`，缺的名字在 `errors` 里。平台 C 端 App 固定发 `X-Platform`、`X-Device-ID`、`X-Request-ID`、`X-Language`，服务真的用到时才列进去。
- 无凭证的 `/internal` 路由组套 `middleware.ClusterInternalOnly`：带 forwarded 头的请求一律拒绝，对端必须是私网地址。它不是授权，是 ingress 规则配错时的兜底。

## 错误契约：RFC 9457，没有信封

- 成功响应就是 DTO 本身，没有 `{ success, data }`。列表两种分页：有界的列表（一个客户自己的订单）用页码 `paging.Result[T]`（`page` 上限 10000，超过 400）；无界的列表（后台看全部订单）用 keyset 游标 `paging.CursorResult[T]`，不数总数、不用 OFFSET，每页代价相同。
- 金额在线上是 JSON 数字（`39.98`），由类型 `money.Amount` 决定，不靠包级全局开关：响应体与事件 payload 里的金额字段一律用它，测试与生产看到的是同一种格式。
- 失败是 ProblemDetails + **真实 HTTP 状态码**，`errorCode` / `traceId` 走扩展成员；`traceId` 是裸 32 位 hex，与日志里的 `trace_id` 和响应头 `X-Trace-ID` 是同一个值。
- Handler 返回 `error`，不写错误响应；`*apperror.Error` 按它的 status/code 写出，其余一律 500 `INTERNAL_ERROR` 并把原因记在日志里（永不泄露给客户端）。状态码按**客户端该做什么反应**分组：400 改一下重提交 · 401 去重新认证 · 403 别再试了 · 404 不存在 · 409 状态冲突 · 422 违反业务规则 · 429 限流 · 502 上游挂了 · 504 超时。
- `errorCode` 是客户端契约，UPPER_SNAKE_CASE，**只增不改**。平台通用码在 `pkg/apperror/codes.go`（平台统一词汇表，user-svc 与其他服务共用），本服务自己的码放在它的上下文包（`internal/order` 的 `Code*`）。
- 路由器自己产生的 404/405 也走同一形状（`router.go` 的 `NotFound` / `MethodNotAllowed`）。
- `title` 由 `pkg/i18n` 的目录按调用方语言填（`X-Language` 优先，其次 `Accept-Language`，回退英文），`detail` 保持开发者可读的英文句子。kit 带平台码的 en / zh-CN / zh-TW / ja，服务在 `internal/httpapi/locales` 补自己的码，`locales_test.go` 保证每个码每种语言都有。
- 每个写出的问题都计入 `msvc_http_problems_total{http_route,http_response_status_code,error_code}`，请求日志同时带 `error_code`，告警可以只盯一个码。

## 一个缺失的能力只能弄坏它自己

- 每个部署都必须有的配置（连接串、Redis、签名密钥）带 `validate:"required"`，缺了拒绝启动（`pkg/conf` 在启动前校验全部 struct tag 和 `Validate()` 方法，含嵌套段）。
- 只有某个能力才需要的配置（某个下游的 `base_url`）不阻止启动：`httpclient.New` 构造出一个"未配置"的客户端，第一次调用答 500 `NOT_CONFIGURED` 并在 detail 里点名缺的段。
- 健康检查只报依赖状态：PostgreSQL 不可达时服务照样启动、readiness 报不健康（不要 CrashLoop，等它回来）。
- readiness 回答"**这个** pod 该不该接流量"，不是"一切都好吗"。只有离开它本 pod 就答不对的依赖才 gating（`AddReadiness`，现在只有 PostgreSQL）；所有副本共享、调用方会降级绕开的依赖（Redis、RabbitMQ）登记为 informational（`AddInformational`）：报告里照样显示（整体 `Degraded`，仍是 200），但不摘流量——否则它一抖，所有 pod 同时被摘出 Service，入口只剩裸 503，修复它的那次发布也起不来。
- 每项检查的最新结果导出为 `msvc_health_check_up{check,gating}`，`Registry.Watch` 每 15s 刷一次（不依赖探针），`DependencyUnavailable` 告警盯它：共享依赖的故障靠告警发现，不靠摘流量发现。

## 两个监听端口

- `server.addr`（8080）只有 API；`server.admin_addr`（8081）只有三个探针和 `/metrics`。Ingress 只指向 API 端口，所以探针与指标永远不会经它暴露；API 被打满时探针也不受影响。worker 没有 API 监听，只起 admin。
- 生命周期里 admin 最先注册、最后停止：排空期间 readiness 一直答 503，指标一直可抓。chart 的探针与 ServiceMonitor 都指向 `admin` 端口。

## 停机预算

K8s 的 `terminationGracePeriodSeconds` 从 preStop **之前**开始计时，所以它必须盖住 preStop + `server.shutdown_timeout` + telemetry flush（5s）+ 余量：chart 用 5 + 25 + 5 + 10 = 45s。

- `shutdown_timeout`（25s）是所有组件按停止顺序共享的总预算；API 监听只拿其中的 `server.drain_timeout`（15s，启动校验必须小于总预算），超时就切断仍在跑的请求——否则一个慢请求能吃光全部预算，后面的消费者、任务、outbox 拿到 0 秒被直接放弃。
- 消费者先停止接收新消息，在途 handler 有 `rabbitmq.drain_timeout`（5s）跑完，之后才取消（消息 requeue）；任务 runner 同理（`tasks.drain_timeout`）。
- 组件停完后关闭连接池，每个最多等 3s：被预算放弃的组件可能还占着连接，`pgxpool.Close` 会一直等它。

## 数据库

`db/*.sql` 是 schema 的唯一真相，手动执行、幂等、先 DDL 后发代码；应用启动永不改库，没有 migration 框架。`tests/arch/ddl_test.go` 在每次 `go test` 时检查团队 DDL 规范，有意的偏离用 `-- ddl-allow: <rule>  <理由>` 写在那一行。集成测试（Testcontainers）用这些脚本建库，所以仓储层 SQL 与 DDL 的漂移在测试里暴露。

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
- 签名密钥只从环境进来（`AUTH__SIGNING_KEY`，Secret）。拿到它的人能铸出任何服务都接受的令牌。
- 轮换密钥不需要全平台同一时刻切换：验证端可以同时认多把（`AUTH__ADDITIONAL_SIGNING_KEYS`，逗号分隔）。顺序是：各服务先把新密钥加进 additional → user-svc 开始用新密钥签 → 各服务把新密钥提为 `signing_key`、旧的留在 additional → 过了最长令牌有效期后删掉旧的。令牌格式不变，不涉及平台契约。
- 开发请求头认证（`X-Dev-Subject` 等）**在代码里**只认 `APP_ENV=development`，配置无法在别的环境打开它。

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

`pkg/resilience` 和 `pkg/httpclient` 能替你分类哪些失败是瞬态的，**分不清你的操作能不能重复执行**。超时之后没有任何谓词知道第一次尝试是否已经提交。所以：

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
| 下游服务 5xx / 超时 / 熔断开 | 502 `UPSTREAM_SERVICE_UNAVAILABLE` | 上游的错，不是调用方的错 |
| 下游服务 4xx | 500 类（`*httpclient.StatusError`），响应体只记日志 | 是**我们**发错了；变成 502 等于甩锅给上游 |

## 任务队列的两个时钟和一个窗口

- 所有"到期"比较和相对重排都用**数据库的 `now()`**，pod 时钟漂移不改变任务何时运行。
- `tasks.task_timeout` 必须**严格小于** `tasks.stale_after`（启动校验）：两者相等时 fixer 会放开 handler 还在跑的行，同一任务并发多份。
- handler 的 ctx **只**由超时取消，停机绝不取消它：排空有上界（`drain_timeout`），超时未完成的行保持 claimed 交给 fixer——一个慢 handler 不会拖过 `terminationGracePeriodSeconds` 变成 SIGKILL。
- handler 返回 error 或 panic 都按失败处理：花一次 attempts，按指数退避（`tasks.retry_base_delay` 起、翻倍、封顶 `retry_max_delay`）重排；一次性任务用满 `max_attempts` 即 dead（`msvc_tasks_dead_total{reason="exhausted"}`），周期任务不受预算限制。失败计入 `msvc_tasks_failed_total`；`reclaimed` 只表示持有任务的 worker 没了（崩溃、排空超时）。panic 在 runner 里被 recover，绝不打崩进程。
- 任务 payload 是 JSON 对象时，入队会带上 `traceparent`，执行时的 span 挂在发起请求的 trace 下；非对象 payload 原样不动。fixer 每轮同时采样每个队列的深度与最老积压时长（`msvc_tasks_oldest_pending_age_seconds`）。

## 消息消费者不能被一条消息打崩

- handler panic 在消费者宿主里被 recover，按普通失败走重试梯队，最后进 DLQ（`exhausted`，会告警：panic 是 bug）；毒消息不会在所有副本之间轮流打崩进程。
- payload 解码失败由 `messaging.Subscribe` 直接标成 `NonRetryable`：现在解不开的消息十分钟后也解不开，直接进 DLQ 记 `non_retryable`，不走梯队、不触发 `exhausted` 告警。
- 发布（outbox 投递与消费端的重试/死信路由共用一个 publisher）整体受 `rabbitmq.publish_timeout` 约束：等连接、等通道锁、等确认都在这一个时限里，broker 不在时调用方很快拿到错误，而不是攥着事务或通道锁一直等。
