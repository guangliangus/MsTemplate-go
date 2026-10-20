# 把 orders 示例换成你的业务

仓库本身是一个能跑的参考服务，`orders` 是贯穿每一层的示例：限界上下文、两个适配器、三种入口、组合根、配置、DDL、API 文档、守卫、本地栈、chart、脚本。生成新服务之后它要被整个换掉，而且散落的地方比 `internal/order` 一个目录多得多。本文是**完整、核对过的清单**（`tools/check-example-removed.sh` 用同一条正则找的），按动手的顺序排。

**先加后删。** 先把你自己的上下文写出来、挂进组合根，与 orders 并存，`make test` 一路绿；再按下面的顺序拆示例。每一节做完跑一次 `go build ./... && go test ./...`，编译器会替你指出漏网的引用；最后两条"静默陷阱"编译器指不出来，单独看。

命名约定（README 的决策记录）：上下文按业务命名（`internal/<ctx>`），适配器按**依赖**命名（`internal/postgres`、`internal/<下游服务名>`），事件名显式带版本（`<ctx>.<event>.v1`），错误码 `UPPER_SNAKE_CASE` 带上下文前缀。

## 1 · 业务上下文：`internal/order/`

整个目录换成你的 `internal/<ctx>/`。保留形状，换掉内容：

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `order.go` | 聚合 `Order`、状态机、`Code*` 错误码与 `Codes()`、`CheckLines` / `New` / `MaxItems` / `MaxAmount` | 换成你的聚合；**保留** `Codes()`（`locales_test.go` 靠它）和"规则答 422"的做法（`apperror.DomainRule` / `Conflict`） |
| `service.go` | 用例服务 `Service`：下游报价、落库 + outbox 同事务、缓存读、提交后失效 | 换成你的用例；保留 `tx.WithTx` 包住业务行 + `publisher.Publish`、`AfterCommit` 删缓存的写法 |
| `ports.go` | 端口 `Repository` / `Transactor` / `InventoryClient` / `Quote` | 只保留跨进程边界的接口；`Transactor` 原样可留 |
| `events.go` | `EventCreated = "orders.order-created.v1"`、`CreatedEvent`（实现 `messaging.Keyed`：`PartitionKey()` = 订单 id） | 换成你的事件：常量显式带版本，payload 只带业务键；同一聚合的事件要按序消费（Kafka 上落同一分区）就实现 `PartitionKey()` 返回聚合 id |
| `export_test.go`、`order_test.go`、`service_test.go` | 聚合与服务的单测 | 删，写你的 |
| `ordertest/ordertest.go` | 端口的**内存假实现**（`Repo` / `Transactor` / `Inventory` / `Publisher`）+ `NewService` 一行装配，handler、consumer、task 的测试共用 | 换成 `internal/<ctx>/<ctx>test/`：同样的形状（假实现而不是 mock，记录"写在事务里"与"提交后钩子"） |

## 2 · 适配器与 DDL

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `internal/postgres/order_repository.go` | `OrderRepository`：`orderModel` / `orderItemModel` 两个 GORM 模型（映射 DDL、钩子写审计列、`optimisticlock.Version`）、`r.gorm.On(ctx)` 取会话、`Preload` 加载明细、keyset 分页、reaper 的 Raw SQL | 删，写你的 `<ctx>_repository.go`；包名 `postgres` 不变，守卫表里就是这个名字。`gorm.go`（`OpenGorm` / `Gorm.On`）是通用的，**保留**，只把 `Models()` 里的两个示例模型换成你的（映射守卫按它逐个检查） |
| `internal/inventory/`（`client.go`、`client_test.go`） | 下游库存服务的适配器：`httpclient.Doer` 之上的类型化客户端，404 → 不可售、5xx → 502、无价格 → 502 | 没有同类下游就**整个目录删掉**；有就改名成你的下游（`internal/<下游名>`），并把新名字写进第 7 节的两张表 |
| `db/0001_init.sql`、`db/0002_orders_version.sql`、`db/0003_orders_request_id.sql` | `orders` / `order_items` 两张表、`orders.version` 列、下单幂等的 `orders.request_id` 与唯一索引 | 删 `0002_orders_version.sql` 与 `0003_orders_request_id.sql`（你的写要幂等时照 0003 的形状加），换成你的 `0001_init.sql`（规范见 `db/README.md`，`tests/arch/ddl_test.go` 会查）。`db/kit_*.sql` 是 kit 的表，**不动** |

## 3 · 入口：HTTP、消费者、任务

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `internal/httpapi/orders_handler.go` | `OrdersHandler`、请求/响应类型（`CreateOrderRequest` 的 struct tag 管"形状"→ 400）、`ScopeOrdersRead`、两个平面的路由 | 删，写你的 `<ctx>_handler.go`：wire 类型留在这里，scope 挂在上下文自己的子路由上 |
| `internal/httpapi/router.go` | `Deps.Orders`（第 31 行）、`ordersHandler`（第 87 行）、`/orders` 与 `/backoffice/orders` 两个 Mount（第 97、102 行）、import `internal/order`（第 16 行） | 改成你的 handler 与路径；中间件链与两个平面组**不动** |
| `internal/httpapi/locales/{en,zh-CN,zh-TW,ja}.json` | 六个 `ORDER_*` 码的四种文案 | 换成你的码；每个码每种语言都要有（`locales_test.go`） |
| `internal/httpapi/locales_test.go` | 用 `order.Codes()` 核对目录 | 改成你的 `Codes()` |
| `internal/httpapi/router_test.go` | handler 测试：平面、错误契约、分页、游标、本地化，全部围绕 orders | 删，照同样的套路写你的（`testServer` + `ordertest.NewService` 这两段改成你的假实现） |
| `internal/httpapi/openapi_test.go` | 路由↔`api/openapi.yaml` 双向比对；第 39–61 行的解析器自测用 `/api/v1/orders` 做夹具 | 比对逻辑不动；夹具路径随手换成你的（任何路径都行，它只测解析器） |
| `internal/consumers/order_created.go`、`order_created_test.go` | `OrderCreatedHandler`：inbox 事务内确认订单，404 答 `messaging.NonRetryable` | 删，写你的消费者；保留"不可能成功的消息直接 `NonRetryable`"的做法 |
| `internal/tasks/tasks.go`、`tasks_test.go` | `PendingOrdersReaper`（recurring 示例）+ `Heartbeat`（cron 示例） | 删 `PendingOrdersReaper` 及其测试；`Heartbeat` 与业务无关，可留作 cron 样例 |

## 4 · 组合根：`internal/app/app.go`、`messaging.go`

| 位置 | 里面是什么 | 做法 |
|---|---|---|
| `app.go` 第 23–24 行 | import `internal/inventory`、`internal/order` | 换成你的包 |
| `app.go` 第 154 行、第 132 行 | `dependencies.orders *order.Service`、`httpapi.Deps{Orders: deps.orders}` | 换成你的服务字段 |
| `app.go` 的 `build`，第 201–213 行 | `httpclient.New("inventory", cfg.ServiceClients["inventory"], …)` 与 `order.NewService(postgres.NewOrderRepository(db), db, inventory.New(inventoryHTTP), messaging.NewOutboxPublisher(db), cache.NewRedis(…), log)` | 换成你的仓储、下游客户端、服务；没有下游就删掉 `httpclient.New` 那一段 |
| `app.go` 的 `registerTasks`，第 222–225 行 | `runner.Recurring("pending-orders-reaper", time.Minute, tasks.NewPendingOrdersReaper(d.orders, cfg.Orders.PendingTimeout, log))` 与它上面的注释 | 删或换成你的任务；`Heartbeat` 那一行可留 |
| `messaging.go` 第 9 行、`subscriptions`（第 51–57 行） | import `internal/order`；`messaging.Subscribe(order.EventCreated, messaging.QueueName(cfg.Service.Name, order.EventCreated), inbox, consumers.NewOrderCreatedHandler(…))` | 换成你的订阅，只声明一次（`splitByBus` 按 `kafka.events` 交给 RabbitMQ 或 Kafka）；队列名 / consumer group 一律从 `messaging.QueueName` 取（第 9 节的 KEDA 陷阱）。两条总线的装配（`registerRabbitMQ` / `registerKafka`）不动 |
| `messaging_test.go` 第 13–14 行 | `splitByBus` 测试的夹具用 `orders.order-*` 事件名 | 随手换成你的（任何名字都行，它只测拆分） |

组合根随上下文数线性变长，变长时按上下文拆 `buildXxx`（`docs/architecture.md`）。

## 5 · 配置

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `internal/config/config.go` | `Config.Orders OrdersConfig`（第 57 行）、`OrdersConfig`（第 145–149 行） | 换成你的段，或删掉 |
| `internal/config/default.jsonc` | `kafka.events` 注释里的示例事件名（第 191 行）；`tasks.recurring.pending-orders-reaper`（第 262–264 行）、`service_clients.inventory`（第 291–312 行）、`"orders"`（第 314–318 行） | 任务覆盖与 `"orders"` 跟着第 4、5 节删（删掉最后一节后，上一节 `}` 后面的逗号可留可删，kit 接受尾逗号）；`service_clients.inventory` 改成你的下游名（没有下游就留空对象：`"service_clients": {}`）；注释里的事件名换成你的。 |
| `internal/config/development.jsonc` | `kafka` 注释里的 `KAFKA__EVENTS=orders.order-created.v1`（第 20–21 行）；`service_clients.inventory.base_url` 指向 compose 的 WireMock（第 27–32 行） | 事件名换成你的；下游跟着改或删 |
| `internal/config/config_test.go` | Kafka 路由与校验的测试（第 105–128 行）用 `orders.order-created.v1` 等作夹具 | 换成你的事件名（任何合法名字都行），断言不动 |

## 6 · API 文档与请求样例

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `api/openapi.yaml` | `paths` 里 `/api/v1/orders`、`/api/v1/orders/{id}`、`/api/v1/backoffice/orders`（第 19–152 行）；`components.schemas` 里 `CreateOrderRequest` / `CreateOrderItem` / `Order` / `OrderItem` / `OrderPage` / `OrderCursorPage`（第 265 行起）；`info.description` 与 `tags` | 换成你的路径与 schema。**保留**：Operational 四条路径、`securitySchemes`、`parameters`（分页语义）、`headers`、`responses.Problem`、`schemas.Problem`。路由↔文档的守卫（`openapi_test.go`）会在 `go test` 时逼你两边一致 |
| `requests.http` | 走一遍 orders 流程 | 换成你的流程；探针、401、幂等下单、400/422/404/409、两个平面这些段落按你的 API 改一遍 |

## 7 · 守卫与集成测试

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `tests/arch/deps_test.go` | 顶部的表（第 23–32 行）：`businessContexts = []string{"order"}`、`adapters = []string{"postgres", "inventory"}`、`entryPoints`、`compositionRoot` | **改表**：上下文名、适配器名。见下面的陷阱 |
| `.golangci.yml` | depguard 规则 `order-stays-pure`（第 65 行）：`files` 的否定列表（第 68–71 行，`!**/internal/inventory/**`）、`deny` 里 `internal/inventory` 的条目与 `desc`（第 73–76 行）；`httpapi-uses-the-service` 的 `desc`（第 92–94 行）；第 59 行的注释 | 规则名改成你的；**否定列表只列适配器与组合根**——改名或新增适配器要同步；删掉 `internal/inventory` 的 deny 条目 |
| `tests/integration/postgres_test.go` | 仓储往返、keyset 分页、"业务行 + outbox 行同事务提交/回滚"（Testcontainers，`-tags=integration`） | 换成你的仓储测试；`TestTransactionsCarryOnTheContext` 的形状值得保留 |

## 8 · 本地栈

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `docker-compose.yml` | `wiremock` 服务（第 98–106 行）、`api` 与 `worker` 的 `SERVICE_CLIENTS__INVENTORY__BASE_URL`（第 185、209 行）、头部注释 | 没有下游桩就删掉 `wiremock` 服务与两处环境变量；有就改名 |
| `deploy/compose/wiremock/mappings/inventory.json` | 库存桩：`OUT-OF-STOCK` / `FAULT` / `SLOW` / `UNKNOWN` 四种 SKU | 删，或换成你的下游桩 |
| `Makefile` | `run` 目标里的注释提到 inventory client 指向 WireMock（第 28–29 行） | 顺手改掉（脚本不扫 Makefile） |

## 9 · 部署

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `deploy/helm/msvcservice/values.yaml` | `config.SERVICE_CLIENTS__INVENTORY__BASE_URL`（第 34–36 行，`CHANGE ME`）；注释掉的 `KAFKA__EVENTS: orders.order-created.v1` 示例（第 41 行）；`secret.values` 注释里的示例 DSN（第 62–63 行）；`optionalRefs` 示例 `inventory-api-key`（第 71 行）；**`worker.keda.queueName: msvcservice.orders.order-created.v1`（第 139 行）**；**`worker.keda.kafka.consumerGroup` / `topic`（第 157、159 行）** | 下游 URL 换成你的 `service_clients.*`，注释示例随意；`queueName` 与 `kafka.consumerGroup` / `topic` 见陷阱 |
| `deploy/kind/values-kind.yaml` | `SERVICE_CLIENTS__INVENTORY__BASE_URL`（第 10 行） | 跟着下游改或删 |
| `deploy/kind/dependencies.yaml`、`deploy/kind/kind-smoke.sh` | `name: wiremock` 的 Deployment 与 Service；冒烟脚本第 41、43 行等 `deploy/wiremock` 就绪 | 没有下游桩就一起删 |

## 10 · 脚本

| 文件 | 里面是什么 | 做法 |
|---|---|---|
| `tools/e2e.sh` | 头部注释的覆盖清单；第 100 行起的路径、scope 与整条验收流程都是 orders（401/403 打的是 `/api/v1/orders`，然后建单、确认、400/422/404/409、限流、熔断的 `FAULT` SKU、重启 broker 再建单） | 保留框架（起栈、等就绪、探针、铸令牌、401/403、收栈），把业务段换成你的 API；没有下游就删掉熔断那一段 |
| `tools/mint-dev-token.sh` | 用法示例 `--scope "orders.read"`（第 9 行） | 改成你的 scope 名 |
| `tools/new-service.sh` | 模板自己的生成器，用法示例与 README 存根提到 orders | 生成出的服务里用不到，删掉；留着也行，`check-example-removed.sh` 跳过它 |

## 11 · 文档

脚本不扫 Markdown，这些靠人：

| 文件 | 提到 orders 的地方 |
|---|---|
| `README.md` | 仓库结构树里 `order/`、`postgres/`、`inventory/`、`tasks/` 四行；本地跑通里 Kafka 那行的 `KAFKA__EVENTS=orders.order-created.v1`；`tools/e2e.sh` 覆盖清单；决策记录表里的例子（分包方式、分页、宿主形态的 KEDA）；运维问答 |
| `AGENTS.md` | "目录与依赖方向"四条 |
| `docs/architecture.md` | 依赖规则图与第一条；认证一节的 `OrdersHandler.BackOfficeRoutes` / `ScopeOrdersRead`；错误契约一节的分页例子；两条消息总线一节的 `order.CreatedEvent` |
| `db/README.md` | 脚本表里 `0001_init.sql` 那一行 |

## 两个静默陷阱

编译器和 `go test` 都不会替你发现：

1. **`deploy/helm/msvcservice/values.yaml` 的 `worker.keda.queueName`（Kafka trigger 的 `consumerGroup` / `topic`）。** 队列名是 `messaging.QueueName(service, event)` 算出来的 `{service}.{event}`，chart 里却是手写的字符串 `msvcservice.orders.order-created.v1`。事件改了名而这里没改，`helm template` 照样通过（它不校验队列名），KEDA 轮询一个不存在的队列：积压再多 worker 也不扩容。改事件名时一起改它；`make helm-lint` 只能证明它渲染得出来。`trigger: kafka` 时同理：`worker.keda.kafka.consumerGroup`（`{service}.{event}`）与 `topic`（事件名，或它在 `kafka.topics` 的覆盖）也是手写的，写错了 KEDA 读的是一个不存在的 group 的 lag。部署里的 `KAFKA__EVENTS` 同样没人核对：启动校验只看名字是不是合法 topic，改了事件名而没改它，这个事件就悄悄回到 RabbitMQ。
2. **`tests/arch/deps_test.go` 顶部的表。** 守卫只检查表里列出的目录：改名了 `internal/order` 而没改 `businessContexts`，`TestGuardedPackagesExist` 会红（目录不存在）——这是响的那一半；**新增**一个上下文或适配器而没写进表，什么都不会红，新包就是不受守卫覆盖。`.golangci.yml` 的 `order-stays-pure` 方向相反：它覆盖 `internal/` 下除否定列表之外的一切，所以新上下文自动受管，**新适配器**必须写进否定列表，否则它自己 import pgx 就会被当成业务包报错。两处都改，再故意在你的上下文里 import 一次 `github.com/jackc/pgx/v5` 跑 `make lint && make test`：两边都红，才说明守卫真的在检查你的包。

## 验证

```bash
make lint && make test                 # 守卫在检查你的包；locales、OpenAPI↔路由、DDL 规范都过
tools/check-example-removed.sh         # 退出码 0：Go / YAML / SQL / HTTP / sh / JSON(C) 里没有 order / inventory / sku
                                       # 你自己的上下文恰好叫这些名字时：--context <name>
make up && make run                    # requests.http 走一遍
tools/e2e.sh --no-app                  # 改完 tools/e2e.sh 之后
```

`check-example-removed.sh` 只认 Go / YAML / SQL / HTTP / sh / JSON / JSONC，不扫 Markdown；它把 SQL 的 `ORDER BY` 和英文里当普通单词用的 `order`（start order、stop order）排除在外，示例一律以标识符的形态出现（`orders`、`order.`、`/order`、`Order…`、`ORDER_…`），漏不掉。
