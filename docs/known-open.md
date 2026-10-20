# 已知未决项

这里只放**有意留着不做**的事。每一条写三样东西：**开着的是什么**、**代价是什么**、**关掉它需要什么**。目的是让下一个人**接手**，而不是重新发现一遍。

不属于这里的：bug（去修）、TODO（去做）、想法（去设计文档）。
**一条进来的前提是有人已经判断过"现在不做"，并且知道为什么。** 每一条都要标核实日期。

约束本身在 [`architecture.md`](architecture.md)。本文只讲缺口。点名 kit 包的条目，改动发生在 kit 仓库（`github.com/guangliangus/go-msvc-kit`），随 kit 的版本到达各服务。

---

## 1 · （已关闭 2026-10-19）HTTP 幂等不比对请求体

通用的 HTTP 幂等中间件已从 kit 删除；幂等改由业务在数据库里做，下单比对商品，同一个 key 换了商品答 422 `ORDER_REQUEST_ID_REUSED`（见 `architecture.md`「幂等由业务在数据库里做」）。编号保留，别处引用的条目号不变。

## 2 · （已关闭 2026-10-20）缓存 L1 不跨副本失效

kit 的两级缓存拆成了两个独立的缓存：订单走只有 Redis 的 `cache.NewRedis`，`Remove` 对每个副本立即生效，没有本地副本能比它活得久；允许旧 N 秒的数据才用 `cache.NewLocal[T]`，它没有 `Remove`，旧度就是它自己的 TTL。拆分留下的代价见第 13 条。编号保留，别处引用的条目号不变。

## 3 · 进程内限流是 per-pod 额度（核实 2026-10-12）

**开着的是什么。** `http.rate_limiting` 用 `httprate` 的本地滑动窗口；全局额度 = 配置值 × 副本数，HPA 扩容会放大。

**代价是什么。** 它只能保护单个 pod 不被打爆，不是全局配额。

**关掉它需要什么。** 真全局限流属网关职责（平台决策）。进程内也可以换成 `redisx.FixedWindowAllow`（跨副本共享、fail-open），一个 `middleware.RateLimiter` 的 Redis 实现，约半天。

## 4 · 没有 OIDC / 服务间凭证签发（核实 2026-10-12）

**开着的是什么。** 认证只认 user-svc 的 HS256 令牌；服务间调用要么透传调用方令牌（`credentials: forward_caller`），要么匿名走受信网络（`none`）。没有 client credentials。

**代价是什么。** 判断"调用方是哪个服务"的下游今天只能靠网络边界（NetworkPolicy / mTLS 由平台提供）。过渡期无凭证的 `/internal` 路由用 `middleware.ClusterInternalOnly` 挡住一切经代理进来的请求：它不是授权，只是 ingress 配错时的纵深防御。

**关掉它需要什么。** 平台出现 B2B 签发方（OIDC 发现 + JWKS）后：kit 的 `auth` 加 RS256/JWKS 验证器（`golang-jwt` 的 `keyfunc`），`httpclient` 加 client-credentials 的 RoundTripper（token 缓存 + 过期前续期 + 401 单次重试）。约两天。

## 5 · OpenAPI 文档手写、不从代码生成也不校验请求（核实 2026-10-17）

**开着的是什么。** `api/openapi.yaml` 由人维护，handler 不从它生成，运行时也不按它校验请求。路径层面有守卫：`internal/httpapi/openapi_test.go` 遍历路由器，与文档 `paths` 下 `/api/v1` 的 (method, path) 双向比对，多一条路由或多一条文档都红。

**代价是什么。** 路径的漂移会被测试抓住；schema、参数、响应码仍靠人维护，与实现漂移只有评审能发现。

**关掉它需要什么。** 两条路选一：spec-first 用 `oapi-codegen` 生成 chi 的 server interface 与类型（handler 实现接口，漂移即编译失败）；或 code-first 用 `swaggo/swag` 注释生成。团队定了再做，约一天。

## 6 · 任务队列没有 UI、DLQ 回放靠脚本（核实 2026-10-17）

**开着的是什么。** `task_queues` 的积压只有指标（`msvc_tasks_queue_depth`）和 SQL；RabbitMQ 的 DLQ 用 `tools/dlq-replay.sh` 手工回放（Kafka 的死信 topic 连脚本都没有，见 #9）。管理 API 的 `get` 先把消息从 DLQ 拿走、再 `publish` 回工作队列，两步之间没有事务：发布失败或没有队列接收（`routed=false`）时那条消息已经不在 DLQ 里。脚本不再让它静默消失——把完整消息（属性 + payload）写到 stderr 和 `./dlq-replay-failed-<queue>-<ts>.json` 再停下，照着文件手动重发；但脚本本身在两步之间被杀掉仍然会丢。

**代价是什么。** 排查靠 psql 和管理台；要求零丢失的回放需要 Shovel 插件。

**关掉它需要什么。** 若团队更想要带 UI 的队列，评估 River（`riverqueue/river`，pgx 原生、事务入队、周期任务、UI）替换 kit 的 `taskqueue`——代价是它自带的表与迁移不符合本团队"DDL 手写、SERIAL 主键"的规范，且与平台其他 Go 服务共用的 `task_queues` 表不再兼容。

## 7 · outbox 逐条发布、逐条等确认（核实 2026-10-19）

**开着的是什么。** `OutboxDispatcher` 在认领事务里逐条 `Publish` 并等 broker 确认，单个 publisher 通道串行。一批 50 条的事务时长约是 50 次往返。Kafka 的 dispatcher 同样逐条：每条等到所有同步副本确认（acks=all）才发下一条，受 `kafka.publish_timeout` 封顶。

**代价是什么。** 每个副本的投递吞吐上限大约是每秒几百条；事务时长随批量线性增长（但每条都受 `rabbitmq.publish_timeout` 封顶，broker 不在时根本不认领，所以不会无限攥着行锁）。

**关掉它需要什么。** RabbitMQ：整批 `PublishWithDeferredConfirm` 再统一等确认，事务时长降到约一次往返。难点是隔离毒消息：一条让 broker 关闭通道的消息（超过 `max_message_size` 等）会让同批所有未确认的消息都按 nack 返回，直接改会把同批的好事件一起计 attempts、一起停成 FAILED。需要"整批失败且连接仍在时，对未确认的逐条重发一次"的回退，并用一条超大消息的集成测试证明隔离成立。约一天；吞吐真成为瓶颈再做。

## 8 · GORM 的映射只有部分检查（核实 2026-10-19）

**开着的是什么。** 数据访问用 GORM（README 决策记录"数据访问"），模型结构体与 `db/*.sql` 的 DDL 靠人工同步，没有编译期检查。已有的守卫是 `tests/integration` 的 `TestEveryGormModelMatchesTheSchema`：对 `postgres.Models()` 里每个模型在 Testcontainers 库上执行一次查询（`QueryFields` 让它逐列点名），模型写了表里没有的列就红。仍然只在执行时才暴露的：类型不配（表是空的就扫不到）、表里有而模型漏掉的列、`Raw` 语句里的一切（而且 Raw 绕过钩子与乐观锁插件）。

**代价是什么。** 这几类错误要到覆盖了那条语句的集成测试、或者线上才红；没加进 `Models()` 的新模型连列名也不检查。

**关掉它需要什么。** 改用 sqlc（按 DDL 生成代码、编译期检查，kit 的 `database/sql` 事务层同样能用，`sql_package: database/sql`），代价是丢掉 GORM 的关联加载与动态条件写法，约一天；或者守卫里给每个模型插一行再读回来，把类型也覆盖到，约半天。

## 9 · Kafka 的死信 topic 没有回放工具（核实 2026-10-19）

**开着的是什么。** `tools/dlq-replay.sh` 只走 RabbitMQ 管理 API。Kafka 的死信在 `{group}.dlq`，没有任何东西消费它（kit `kafka` 包文档：检视后手工回放），模板也没有对应的脚本。

**代价是什么。** 回放靠手工：用 broker 自带的 `kafka-console-consumer.sh --formatter-property print.key=true --formatter-property print.headers=true`（或 `kcat -C -t <group>.dlq -f '%k %h %s\n'`）读出死信，再用 `kafka-console-producer.sh --reader-property parse.key=true --reader-property parse.headers=true`（或 `kcat -P -k <key> -H name=value`）写进 `{group}.retry.1`：key、value、`x-message-id`、`x-event-name`、`traceparent`、`x-occurred-at` 原样，`x-attempt` 改成 `1`，去掉 `x-exception`、`x-dead-letter-reason`、`x-failed-at`、`x-origin-queue`、`x-origin-topic`。重试 topic 只有本 group 消费，别的服务不会再收一份；写回事件 topic 则每个 group 都会再收到，inbox 行已过 `outbox.retention`（模板默认 7 天）被清理的服务会再处理一次。手工改头容易错，没有批量与记录。和 RabbitMQ 不同的是读死信不会把它拿走，中途失败最多重复（inbox 吸收），不会丢。

**关掉它需要什么。** 一个 `tools/dlq-replay-kafka.sh`（kcat）或 kit 里的小命令（franz-go）：按上面的规则从 `{group}.dlq` 读、改头、写进 `{group}.retry.1`，用自己的 consumer group 记录回放到哪个 offset，写入被确认后才提交；支持按 `x-dead-letter-reason` / 时间段筛选。约半天到一天；第一次真要回放 Kafka 死信时做。

## 10 · 服务不导出 Kafka 的 consumer lag（核实 2026-10-19）

**开着的是什么。** kit 的 Kafka 不新增指标族：发布、消费、死信记在 `msvc_messaging_*` 里（`queue` = consumer group）。消费侧落后多少（分区末端 offset − 已提交 offset）服务不导出；chart 的 KEDA `trigger: kafka` 自己从 broker 读 lag 来扩缩 worker，但那不进 Prometheus。

**代价是什么。** 看板和告警只看得到发布侧的 outbox 积压；消费者卡住或全挂时 `msvc_messaging_consumed_total` 不再增长，但没有一条告警盯着"积压在变大"。

**关掉它需要什么。** 平台在 broker 侧部署 lag exporter（如 kafka-exporter 的 `kafka_consumergroup_lag`，或 Burrow），在 `deploy/grafana/alerts.yml` 加一条按 consumer group（`<service>.<event>`）的 lag 告警，跨服务通用。也可以在 kit 里用 `kadm.Client.Lag` 周期采样导出一个 gauge，但那是新的平台指标名，要和看板一起定。约半天（exporter 已有时）。

## 11 · Kafka 上没有 schema registry，payload 是 JSON（核实 2026-10-19）

**开着的是什么。** Kafka 的 value 与 AMQP body 相同：camelCase JSON 加 `messageId`、`occurredAt`，`content-type: application/json`。没有 Schema Registry、没有 Avro / Protobuf；事件的形状靠 Go 类型和事件名里的版本（`.v1`）约定，同一事件在两条 bus 上格式一致，换 bus 不改 payload。

**代价是什么。** 生产者改坏了形状 broker 不拦：消费者解码失败，`messaging.Subscribe` 直接标 `NonRetryable` 进死信。没有发布前的兼容性检查；Kafka Connect、流处理这类按 schema 工作的消费者没有 schema 可取。

**关掉它需要什么。** 平台引入 Schema Registry、出现按 schema 消费的下游（Connect / 流处理 / 数据湖），或真出过一次事件形状的兼容事故时再评估。做法：kit 的 Kafka 发布与消费加序列化出口（registry 的线格式：magic byte + schema id），schema 入库并在 CI 里对 registry 做兼容性检查。这是 Kafka 线格式的变更，按平台契约和平台一起改。约数天。

## 12 · Kafka 的 TLS / SASL 与多成员 rebalance 没有测试覆盖（核实 2026-10-19）

**开着的是什么。** kit 的 `tests/integration/kafka_test.go` 对单节点、明文、一个消费者成员的 broker 覆盖 outbox → handler 与契约头、重试梯队、死信 topic、同 key 顺序、inbox 去重、重启后从提交点继续、broker 不在时不花 attempts。没覆盖的：对真集群的 TLS 与 SASL（PLAIN / SCRAM）；一个 group 有多个成员、handler 正在跑时发生 rebalance（revoke 时先停 worker 再提交的那条路径）。

**代价是什么。** `kafka.tls` / `kafka.sasl` 只有配置校验的单测，握手与鉴权的问题要到第一次连真集群才暴露。rebalance 路径只在扩缩容和滚动发布时走到；设计上是至少一次 + inbox，最坏是重复，但没有测试证明它不会提交一条没处理完的 record。

**关掉它需要什么。** `testutil` 的 Kafka 容器加一个 SASL_SSL listener（自签证书 + SCRAM 用户），集成测试连它；加一个两成员场景：handler 阻塞时起第二个成员触发 rebalance，断言每条 record 都被处理、没有一条在处理完之前被提交。约一天。在那之前，接真集群的第一个服务先在目标环境用 `KAFKA__TLS` / `KAFKA__SASL__*` 冒烟一次。

## 13 · 订单缓存没有进程内一层（核实 2026-10-20）

**开着的是什么。** 订单的缓存只有 Redis（`cache.NewRedis`）：每次命中都是一次 Redis 往返，同一 pod 上并发查同一个 key 才合并成一次。Redis 不可达时缓存 fail-open，订单读全部回源到数据库，没有本地副本垫着。

**代价是什么。** 每次命中多一次集群内往返（亚毫秒级，仍远比回源两条 SQL 便宜）；Redis 的 QPS 等于订单读的 QPS（同 key 并发合并后）；Redis 故障期间数据库承担全部订单读。订单按客户读、没有热点 key，所以现在都可以接受。

**关掉它需要什么。** 某个 key 真成了热点（或 Redis 成了瓶颈），在调用处给它套一层 `cache.NewLocal[order.Order]`：`local.GetOrCreate(ctx, k, func(ctx) { return cache.GetOrCreate(ctx, shared, k, 0, load) })`，TTL 就是这类读接受的跨副本旧度，并在 `architecture.md` 写明哪些读可能旧这么久。约半天。不要把本地层放回共享缓存内部：那正是第 2 条关掉的问题。

---

## 怎么维护这份清单

- 关掉一条就**删掉它**，别留一条"已完成"。
- 新增一条要能回答那三个问题。答不上"关掉它需要什么"的，是 TODO，不是未决项。
- 关掉一条留下的代价，要作为新的一条写下来，并且不要复用被删掉的编号。
