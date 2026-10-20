# 数据库 DDL 脚本

schema **不走任何 migration 框架**：本目录的 SQL 按文件名顺序**手动执行**（DBA/开发），应用启动和部署流水线从不改库。

## 执行

```bash
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/0001_init.sql
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/kit_0001_messaging.sql
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/kit_0002_tasks.sql
```

本地 `docker compose up` 的一次性 `schema` 服务会代跑这一步。生产环境换成对应连接串，用有 DDL 权限的账号执行；应用运行账号只需要 DML 权限。

| 脚本 | 属于 | 不用该能力时 |
|---|---|---|
| `0001_init.sql` | 示例业务（orders / order_items） | 换成你的业务表 |
| `0002_orders_version.sql` | 示例业务：`orders.version` 乐观并发列（GORM 的 `optimisticlock` 插件用它，取代 `xmin`） | 随示例一起删 |
| `0003_orders_request_id.sql` | 示例业务：下单的业务幂等，`orders.request_id` 与 `(customer_id, request_id)` 部分唯一索引 | 随示例一起删；你的写要幂等时照它的形状加 |
| `kit_0001_messaging.sql` | kit：`rabbitmq.enabled` / `kafka.enabled` 的 outbox / inbox（两个 broker 共用） | 可不执行 |
| `kit_0002_tasks.sql` | kit：`tasks.max_workers > 0` 的任务队列 | 可不执行 |

## kit 的表

`kit_NNNN_*.sql` 是平台 kit（`github.com/guangliangus/go-msvc-kit`）的表：读写它们的代码在 kit 里，表结构跟着 kit 的版本走。本目录放的是它们**逐字节相同的副本**，因为 DBA 执行的是本目录。

- 不要改这些文件；表结构变化由 kit 追加下一个 `kit_NNNN` 脚本，本服务自己的脚本用不带前缀的编号，两个系列互不占号。
- 升级 kit 时看它 `CHANGELOG.md` 的 **DDL** 一节：把新脚本复制进来（`go test` 失败信息里有复制命令），由 DBA 先执行，再发布代码。
- `tests/arch/kitddl_test.go` 在每次 `go test` 时拿这些副本和 kit 内嵌的版本比对，缺了或不一样就失败。
- `outbox_messages` 与 `inbox_messages` 的主键是 `BIGSERIAL`，偏离团队的 `SERIAL` 默认：它们是流水表，清理只删行、不重置序列，int32 会耗尽。kit 的脚本在那一行用 `-- ddl-allow: serial-primary-key  flow table: …` 声明了这个豁免，所以副本过守卫、也不是可以照抄的先例——业务表仍用 `SERIAL`。
- `outbox_messages.partition_key`（`text NOT NULL DEFAULT ''`）是事件的分区键：实现 `messaging.Keyed` 的事件存聚合 id，投递到 Kafka 时作 record key（同一聚合的事件落在同一分区、按序消费），空串表示无键；RabbitMQ 忽略它。
- **用更早的脚本建的本地库**（旧文件 `0002_messaging.sql`，或加 `partition_key` 之前的 `kit_0001_messaging.sql`）缺 `outbox_messages.next_attempt_at` 和 / 或 `partition_key`，同名索引 `ix_outbox_messages_pending` 可能还建在 `(id)` 上。光重跑脚本补不上：`CREATE TABLE IF NOT EXISTS` 不加列，脚本里新列的 `COMMENT ON COLUMN` 因列不存在而失败（`schema` 服务非零退出），`CREATE INDEX IF NOT EXISTS` 看到同名索引就跳过；而缺 `partition_key` 的库，新版 `OutboxPublisher` 写不进 outbox 行。要么 `make down && make up` 重建，要么按 kit `CHANGELOG.md` 的 DDL 一节手动补：

  ```sql
  ALTER TABLE outbox_messages ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();
  ALTER TABLE outbox_messages ADD COLUMN IF NOT EXISTS partition_key text NOT NULL DEFAULT '';
  DROP INDEX IF EXISTS ix_outbox_messages_pending;
  ```

  然后重跑 `db/kit_0001_messaging.sql`（本地 `docker compose up schema`）：它补上列注释，并建回 `(next_attempt_at, id) WHERE status = 'PENDING'` 的索引。生产库不会遇到这一步：kit 还没有发版，`kit_0001` 是发版前原地改的；发版之后的表结构变化一律追加新的 `kit_NNNN`。

## 规则

1. **已执行过的脚本永不修改**——任何变更都追加新编号脚本（`0002_add_xxx.sql`、`0003_...`；`kit_` 系列归 kit 管）。
2. 脚本必须**幂等**（`IF NOT EXISTS` 等），可安全重复执行；流水线门禁会把每个脚本**执行两遍**。
3. **先执行 DDL，再发布代码**。滚动发布期间新旧代码并存，DDL 必须向后兼容：只加列/加表/加索引；改名和删除拆成两个版本（先让所有代码停用，再删）。
4. 表结构与仓储的 GORM 模型（`internal/postgres`）必须一致：模型是这里 DDL 的映射，应用永不 AutoMigrate；不一致会在集成测试（Testcontainers 用本目录建库）和运行时报错，没有编译期检查（known-open #8）。改表先改这里，再改模型。
5. 每个环境执行到哪个脚本需有记录（工单/文档均可；也可在库里建一张执行记录表）。

## 守卫

规则是 kit 的 `ddlguard` 包，每个服务跑同一套而不是各抄一份；本服务的 `tests/arch/ddl_test.go` 只有一行 `ddlguard.Check(t, "../../db")`，每次 `go test ./...` 扫描本目录（目录里没有脚本也算失败：悄悄检查零个文件的守卫比没有更糟）。规则与它们的 id：

| id | 规则 |
|---|---|
| `varchar` | 字符串一律 `text`，不用 `varchar` / `character varying` / `char(n)` |
| `timestamp-without-tz` | 日期时间用 `TIMESTAMPTZ`（或 `timestamp with time zone`），不用裸 `timestamp` |
| `serial-primary-key` | 自增主键用 `SERIAL`，不用 `BIGSERIAL`（流水表的豁免见上一节） |
| `pg-enum` | 不用 `CREATE TYPE … AS ENUM`，枚举是 `text` + 代码控制 |
| `physical-delete` | 脚本里不出现 `DELETE FROM` / `DROP TABLE` / `TRUNCATE`，用软状态 |
| `if-not-exists` | `CREATE TABLE` / `CREATE INDEX` 都带 `IF NOT EXISTS` |
| `reserved-word` | 表名不用保留字 |
| `money-type` | 货币不用 PG 的 `money` 类型，用 `numeric(10,2)` |
| `plural-table`、`fk-not-null` | 表名最后一个单词复数；外键列 `NOT NULL`（跨行的结构检查，不是逐行规则，豁免方式相同） |

有意的偏离写在脚本里被豁免的那一行（或上一行）：`-- ddl-allow: <rule-id>  <理由>`；
第三方原样 vendored、不能改写的 DDL 在文件前五行整文件豁免：`-- ddl-allow-file: <理由>`。
报告格式是 `<file>:<line>: [<rule-id>] <message>`，每个文件一个子测试。
