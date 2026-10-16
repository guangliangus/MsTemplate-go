# 已知未决项

这里只放**有意留着不做**的事。每一条写三样东西：**开着的是什么**、**代价是什么**、**关掉它需要什么**。目的是让下一个人**接手**，而不是重新发现一遍。

不属于这里的：bug（去修）、TODO（去做）、想法（去设计文档）。
**一条进来的前提是有人已经判断过"现在不做"，并且知道为什么。** 每一条都要标核实日期。

约束本身在 [`architecture.md`](architecture.md)。本文只讲缺口。

---

## 1 · HTTP 幂等不指纹校验请求体（核实 2026-10-12）

**开着的是什么。** `pkg/middleware/idempotency.go` 以「调用方 + 方法 + 路径 + key」为键重放，不比对请求体。同一个 key 换一个 payload 会重放第一次的响应。

**代价是什么。** 客户端 bug（key 复用）会拿到一个"看起来成功"但内容不对的响应。平台上的现有服务同样不校验。

**关掉它需要什么。** 在 `executeAndCapture` 存储时附带请求体的 SHA-256，`TryBegin` 命中时比对，不同则 422 `IDEMPOTENCY_KEY_REUSED`。约半天；要先给 user-svc 的客户端约定这个新错误码。

## 2 · 缓存 L1 不跨副本失效（核实 2026-10-12）

**开着的是什么。** `pkg/cache` 的 `Remove` 只清本副本的 L1 和共享的 L2；别的副本的 L1 最多再服务 `cache.local_ttl`（默认 30s）的旧值。

**代价是什么。** 多副本下一次状态变更后，最长 30s 内另一个 pod 可能读到旧订单状态。对订单状态这类"最终会对"的读可接受。

**关掉它需要什么。** 用 Redis pub/sub 广播失效键（每副本订阅一个频道，收到后删 L1），或把必须实时的读直接走数据库。约一天。

## 3 · 进程内限流是 per-pod 额度（核实 2026-10-12）

**开着的是什么。** `http.rate_limiting` 用 `httprate` 的本地滑动窗口；全局额度 = 配置值 × 副本数，HPA 扩容会放大。

**代价是什么。** 它只能保护单个 pod 不被打爆，不是全局配额。

**关掉它需要什么。** 真全局限流属网关职责（平台决策）。进程内也可以换成 `redisx.FixedWindowAllow`（跨副本共享、fail-open），一个 `middleware.RateLimiter` 的 Redis 实现，约半天。

## 4 · 没有 OIDC / 服务间凭证签发（核实 2026-10-12）

**开着的是什么。** 认证只认 user-svc 的 HS256 令牌；服务间调用要么透传调用方令牌（`credentials: forward_caller`），要么匿名走受信网络（`none`）。没有 client credentials。

**代价是什么。** 判断"调用方是哪个服务"的下游今天只能靠网络边界（NetworkPolicy / mTLS 由平台提供）。过渡期无凭证的 `/internal` 路由用 `middleware.ClusterInternalOnly` 挡住一切经代理进来的请求：它不是授权，只是 ingress 配错时的纵深防御。

**关掉它需要什么。** 平台出现 B2B 签发方（OIDC 发现 + JWKS）后：`pkg/auth` 加 RS256/JWKS 验证器（`golang-jwt` 的 `keyfunc`），`pkg/httpclient` 加 client-credentials 的 RoundTripper（token 缓存 + 过期前续期 + 401 单次重试）。约两天。

## 5 · OpenAPI 文档手写、不从代码生成也不校验请求（核实 2026-10-12）

**开着的是什么。** `api/openapi.yaml` 由人维护，handler 不从它生成，运行时也不按它校验请求。

**代价是什么。** 文档与实现可能漂移，只有评审能发现。

**关掉它需要什么。** 两条路选一：spec-first 用 `oapi-codegen` 生成 chi 的 server interface 与类型（handler 实现接口，漂移即编译失败）；或 code-first 用 `swaggo/swag` 注释生成。团队定了再做，约一天。

## 6 · 任务队列没有 UI、DLQ 回放靠脚本（核实 2026-10-12）

**开着的是什么。** `task_queues` 的积压只有指标（`msvc_tasks_queue_depth`）和 SQL；RabbitMQ 的 DLQ 用 `tools/dlq-replay.sh` 手工回放（管理 API `get` 先删后发，脚本中途死掉会丢那一条）。

**代价是什么。** 排查靠 psql 和管理台；要求零丢失的回放需要 Shovel 插件。

**关掉它需要什么。** 若团队更想要带 UI 的队列，评估 River（`riverqueue/river`，pgx 原生、事务入队、周期任务、UI）替换 `pkg/taskqueue`——代价是它自带的表与迁移不符合本团队"DDL 手写、SERIAL 主键"的规范，且与平台其他 Go 服务共用的 `task_queues` 表不再兼容。

## 7 · outbox 逐条发布、逐条等确认（核实 2026-10-16）

**开着的是什么。** `OutboxDispatcher` 在认领事务里逐条 `Publish` 并等 broker 确认，单个 publisher 通道串行。一批 50 条的事务时长约是 50 次往返。

**代价是什么。** 每个副本的投递吞吐上限大约是每秒几百条；事务时长随批量线性增长（但每条都受 `rabbitmq.publish_timeout` 封顶，broker 不在时根本不认领，所以不会无限攥着行锁）。

**关掉它需要什么。** 整批 `PublishWithDeferredConfirm` 再统一等确认，事务时长降到约一次往返。难点是隔离毒消息：一条让 broker 关闭通道的消息（超过 `max_message_size` 等）会让同批所有未确认的消息都按 nack 返回，直接改会把同批的好事件一起计 attempts、一起停成 FAILED。需要"整批失败且连接仍在时，对未确认的逐条重发一次"的回退，并用一条超大消息的集成测试证明隔离成立。约一天；吞吐真成为瓶颈再做。

---

## 怎么维护这份清单

- 关掉一条就**删掉它**，别留一条"已完成"。
- 新增一条要能回答那三个问题。答不上"关掉它需要什么"的，是 TODO，不是未决项。
- 关掉一条留下的代价，要作为新的一条写下来，并且不要复用被删掉的编号。
