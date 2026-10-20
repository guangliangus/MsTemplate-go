# Repository Guidelines（给 AI 助手和新成员）

这是用 Go 微服务模板生成的服务 `msvcservice`。仓库本身就是一个能跑、有测试的服务；
平台基础包（kit）是独立的私有模块 `github.com/guangliangus/go-msvc-kit`（按版本依赖，见 README 的"平台 kit"），`internal/` 是这个服务自己的代码。约束写在 `docs/architecture.md`，
有意不做的事写在 `docs/known-open.md`，每条决策的理由在 `README.md` 的决策记录表。改代码前先读这三处。

## 常用命令

```bash
make up                 # 本地基础设施：PostgreSQL / Redis / RabbitMQ / WireMock / Jaeger / Prometheus / Grafana + schema
docker compose --profile kafka up -d   # 可选：再起单节点 Kafka（localhost:9092）；KAFKA__ENABLED=true KAFKA__EVENTS=<事件> make run 让该事件走 Kafka
make run                # APP_ENV=development 起 API（:8080，开发请求头认证，/docs/ 只在这个环境；探针与 /metrics 在 :8081）
make run-worker         # worker 形态：没有 API 监听，探针与 /metrics 在 :8082
make test               # 单元测试 + 架构守卫 + 路由↔OpenAPI 守卫 + DDL 规范守卫（不需要 Docker）
make test-integration   # Testcontainers 集成测试（需要 Docker）
make lint               # golangci-lint v2（120 列、函数 80 行、嵌套 4 层、有 default 的 switch 把它放最后、枚举 switch 必须穷尽、depguard 分层）
make helm-lint          # chart lint + 两个 overlay 渲染
tools/e2e.sh --no-app   # 对宿主机上跑着的服务做端到端验收
make vuln               # govulncheck（版本固定在 tools/go.mod，`go tool -modfile=tools/go.mod <tool>`）
make new-service NAME=x MODULE=github.com/acme/x   # 从模板生成新服务
```

## 目录与依赖方向

- `internal/order`：限界上下文（聚合、用例服务、端口接口、集成事件）。它不 import 任何驱动或适配器。
- `internal/postgres`、`internal/inventory`：实现 `order` 的端口。`internal/httpapi`：路由、handler、请求/响应类型、错误文案。
- `internal/consumers`、`internal/tasks`：消费者与后台任务，只调用 `order.Service`。`internal/app`：组合根，唯一同时看见端口与实现的地方（消息总线的装配与订阅在 `messaging.go`）。
- kit 是另一个模块，看不见本服务的 `internal/`。`tests/arch` 与 depguard 在 `go test` / `make lint` 时强制上面这些规则。
- 新增或改名一个上下文 / 适配器时，改两处：`tests/arch/deps_test.go` 顶部的表（`businessContexts` / `adapters` / `entryPoints`；
  目录不存在测试会红，漏列则守卫悄悄不覆盖它）和 `.golangci.yml` 里 `order-stays-pure` 的否定列表（只列组合根与适配器；
  新适配器不列进去，它一 import pgx 这类驱动就会被当成业务包报错）。
- 删掉 orders 示例按 `docs/replacing-the-example.md`，最后跑 `tools/check-example-removed.sh`（退出码 0 才算干净）。

## 改代码的规矩

- handler 返回 `error`，不手写错误响应；错误用 `apperror` 的构造器，错误码 UPPER_SNAKE_CASE，只增不改。
  新错误码要同时加到 `internal/httpapi/locales/*.json` 的每种语言，`locales_test.go` 会检查。
- 校验分两层：请求的**形状**（字段有没有、类型对不对、长度范围）写在 `httpapi` 请求类型的 struct tag 上，答 400 `VALIDATION_FAILED`；
  业务**规则**（条数上限、能不能卖、金额上限）写在上下文包里，答 422 带它自己的码。规则只在上下文包里存一份，tag 不复制它。
- `/docs/`（Swagger UI 页面）与 `/docs/openapi.yaml` 只在 `APP_ENV=development` 挂载，和开发请求头一样是代码写死的；路由与
  `api/openapi.yaml` 的路径由 `internal/httpapi/openapi_test.go` 双向比对，加路由要同时改文档。
- 依赖经构造函数传入、存成字段；请求范围的数据（事务、身份、截止时间）只走 `ctx`；进程级对象的字段里不放请求状态。
  新的限界上下文挂进已有的两个平面组，它的 scope 写在自己的子路由上，不加到平面组。
- 仓储用 GORM，一律经 `r.gorm.On(ctx)`（`internal/postgres/gorm.go`）取会话，这样 `db.WithTx` 里的业务行、outbox 行、
  任务行同一事务提交。永远不调 GORM 的 `Transaction` / `Begin`（事务归 kit），永远不调 `AutoMigrate`（DDL 手写在 `db/`）。
  模型是 DDL 的映射：加列先写 `db/*.sql`，再改模型，集成测试会验证映射。GORM 只出现在 `internal/postgres`（depguard + 架构测试）。
- 有版本号的表用 `optimisticlock.Version` 字段；更新前确认版本号已加载（为 0 时插件不加条件）。`Raw` 绕过钩子与插件：
  Raw 的 UPDATE 自己写 `version = version + 1` 和审计列。
  库外副作用（删缓存）挂 `AfterCommit`，事务里不等任何慢东西（broker、下游 HTTP）。
- 没有通用的 HTTP 幂等中间件。需要幂等的写由上下文自己在数据库里做：请求 id（`Idempotency-Key` 头）存进业务行，
  加唯一约束，和业务行、outbox 行同一事务；重复时按业务规则答复（下单：同样的商品答 200 和已有订单，换了商品 422）。
  参考 `order.Service.Create` 与 `db/0003_orders_request_id.sql`，不要在 Redis 里存响应快照。
- DDL 只写在 `db/*.sql`，幂等、手动执行、应用永不改库；规范见 `db/README.md`，`tests/arch/ddl_test.go` 会检查。
- 配置：默认值写进 `internal/config/default.jsonc`（JSONC：JSON 加 `//` 注释，时长写字符串 `"30s"`）并加注释，环境变量 `SECTION__KEY` 覆盖；秘密只从环境变量进来。
- 调其他服务用 kit 的 `httpclient`（熔断、重试、透传平台请求头），不要自己 new `http.Client`。
- kit 的代码不在这里改：缺能力或有 bug，改 kit 仓库、发版，这里 `go get` 新版本（联调用 `go work`，不提交 `replace`）。
  kit 是三个 module：根模块、`testutil`（集成测试用，单独 `go get github.com/guangliangus/go-msvc-kit/testutil@testutil/vX.Y.Z`）、
  只在 kit 仓库自测的 `tests/integration`（不可 import）。`db/kit_*.sql` 是 kit 表的副本，不要手改；升级 kit 时按它的
  CHANGELOG 复制，`tests/arch/kitddl_test.go` 会比对。
- 消息 handler 对不可能成功的消息返回 `messaging.NonRetryable(err)`，让它直接进 DLQ 并记成 non_retryable。
- 事件在哪个 bus 上只由配置 `kafka.events` 决定（列出的发布到 Kafka、也从 Kafka 消费，其余走 RabbitMQ）。这是平台决策：
  生产者和所有消费者一起改，不是一个服务自己的开关。把事件挪到 Kafka 代码里不用改别的：订阅照常在
  `internal/app/messaging.go` 用 `messaging.Subscribe` 声明一次，`splitByBus` 交给对应的宿主。要按聚合保序的事件实现
  `messaging.Keyed`（`PartitionKey()` 返回聚合 id，即 Kafka 的 record key）。`Subscription` 的 `Concurrency` / `Prefetch`
  只对 RabbitMQ 生效，Kafka 的并行度是分到的分区数。
- 新增跨进程依赖时，在 `docs/architecture.md` 的失败语义表加一行，再写代码；健康检查要决定它是 gating
  （`AddReadiness`：离开它本 pod 答不对）还是 informational（`AddInformational`：所有副本共享、能降级绕开）。
- 响应体和事件里的金额用 `money.Amount`；会无限增长的列表用 keyset 游标分页（`paging.CursorResult`）。
- 缓存：会被写改动的数据用 `cache.NewRedis`（失效用 `Remove` 挂 `AfterCommit`，不要用 `Set` 写新值）；
  一批 key 用 `GetOrCreateMany`，不要循环；本来就允许旧 N 秒的数据才用 `cache.NewLocal[T]`，TTL 跟着数据写；
  不要把两者叠成一个两级缓存。单测用 `ordertest.NewCache(t)` / `cachetest.NewRedis(t)`（进程内 miniredis）。
- 不引入 gin / logrus / zap 这类替代品；路由器是 chi，数据访问是 GORM（跑在 kit 的 `database/sql` 事务上），日志是 slog。

## 提交前

`make lint && make test`；改了 chart 跑 `make helm-lint`；改了决策就更新 README 的决策记录表；
改了共享契约（错误契约、AMQP 头、Kafka 的 topic / 头 / group 命名、指标名、令牌格式）要和平台一起改，见 `docs/architecture.md` 的"平台契约"。
