# Repository Guidelines（给 AI 助手和新成员）

这是用 Go 微服务模板生成的服务 `msvcservice`。仓库本身就是一个能跑、有测试的服务；
`pkg/` 是平台基础包（kit），`internal/` 是这个服务自己的代码。约束写在 `docs/architecture.md`，
有意不做的事写在 `docs/known-open.md`，每条决策的理由在 `README.md` 的决策记录表。改代码前先读这三处。

## 常用命令

```bash
make up                 # 本地基础设施：PostgreSQL / Redis / RabbitMQ / WireMock / Jaeger / Prometheus / Grafana + schema
make run                # APP_ENV=development 起 API（:8080，开发请求头认证，/docs 可用；探针与 /metrics 在 :8081）
make test               # 单元测试 + 架构守卫 + DDL 规范守卫（不需要 Docker）
make test-integration   # Testcontainers 集成测试（需要 Docker）
make lint               # golangci-lint v2（120 列、函数 80 行、嵌套 4 层、switch 必有 default、depguard 分层）
make helm-lint          # chart lint + 两个 overlay 渲染
tools/e2e.sh --no-app   # 对宿主机上跑着的服务做端到端验收
make vuln               # govulncheck（版本固定在 tools/go.mod，`go tool -modfile=tools/go.mod <tool>`）
make new-service NAME=x MODULE=github.com/acme/x   # 从模板生成新服务
```

## 目录与依赖方向

- `internal/order`：限界上下文（聚合、用例服务、端口接口、集成事件）。它不 import 任何驱动或适配器。
- `internal/postgres`、`internal/inventory`：实现 `order` 的端口。`internal/httpapi`：路由、handler、请求/响应类型、错误文案。
- `internal/consumers`、`internal/tasks`：消费者与后台任务，只调用 `order.Service`。`internal/app`：组合根，唯一同时看见端口与实现的地方。
- `pkg/`：永不 import `internal/`。`tests/arch` 与 depguard 在 `go test` / `make lint` 时强制这些规则。

## 改代码的规矩

- handler 返回 `error`，不手写错误响应；错误用 `apperror` 的构造器，错误码 UPPER_SNAKE_CASE，只增不改。
  新错误码要同时加到 `internal/httpapi/locales/*.json` 的每种语言，`locales_test.go` 会检查。
- 依赖经构造函数传入、存成字段；请求范围的数据（事务、身份、截止时间）只走 `ctx`；进程级对象的字段里不放请求状态。
  新的限界上下文挂进已有的两个平面组，它的 scope 写在自己的子路由上，不加到平面组。
- 仓储一律从 `db.Q(ctx)` 取执行器，这样 `db.WithTx` 里的业务行、outbox 行、任务行同一事务提交。
  库外副作用（删缓存）挂 `AfterCommit`，事务里不等任何慢东西（broker、下游 HTTP）。
- DDL 只写在 `db/*.sql`，幂等、手动执行、应用永不改库；规范见 `db/README.md`，`tests/arch/ddl_test.go` 会检查。
- 配置：默认值写进 `internal/config/default.yaml` 并加注释，环境变量 `SECTION__KEY` 覆盖；秘密只从环境变量进来。
- 调其他服务用 `pkg/httpclient`（熔断、重试、透传平台请求头），不要自己 new `http.Client`。
- 消息 handler 对不可能成功的消息返回 `messaging.NonRetryable(err)`，让它直接进 DLQ 并记成 non_retryable。
- 新增跨进程依赖时，在 `docs/architecture.md` 的失败语义表加一行，再写代码；健康检查要决定它是 gating
  （`AddReadiness`：离开它本 pod 答不对）还是 informational（`AddInformational`：所有副本共享、能降级绕开）。
- 响应体和事件里的金额用 `money.Amount`；会无限增长的列表用 keyset 游标分页（`paging.CursorResult`）。
- 不引入 gin / GORM / logrus 这类替代品；路由器是 chi，数据访问是 pgx 原生 SQL，日志是 slog。

## 提交前

`make lint && make test`；改了 chart 跑 `make helm-lint`；改了决策就更新 README 的决策记录表；
改了共享契约（错误契约、AMQP 头、指标名、令牌格式）要和平台一起改，见 `docs/architecture.md` 的"平台契约"。
