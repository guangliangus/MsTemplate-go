# 数据库 DDL 脚本

schema **不走任何 migration 框架**：本目录的 SQL 按文件名顺序**手动执行**（DBA/开发），应用启动和部署流水线从不改库。

## 执行

```bash
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/0001_init.sql
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/0002_messaging.sql
psql "postgresql://msvc:msvc@localhost:5432/msvcservice" -v ON_ERROR_STOP=1 -f db/0003_tasks.sql
```

本地 `docker compose up` 的一次性 `schema` 服务会代跑这一步。生产环境换成对应连接串，用有 DDL 权限的账号执行；应用运行账号只需要 DML 权限。

| 脚本 | 属于 | 不用该能力时 |
|---|---|---|
| `0001_init.sql` | 示例业务（orders / order_items） | 换成你的业务表 |
| `0002_messaging.sql` | `rabbitmq.enabled` 的 outbox / inbox | 可不执行 |
| `0003_tasks.sql` | `tasks.max_workers > 0` 的任务队列 | 可不执行 |

## 规则

1. **已执行过的脚本永不修改**——任何变更都追加新编号脚本（`0004_add_xxx.sql`、`0005_...`）。
2. 脚本必须**幂等**（`IF NOT EXISTS` 等），可安全重复执行；流水线门禁会把每个脚本**执行两遍**。
3. **先执行 DDL，再发布代码**。滚动发布期间新旧代码并存，DDL 必须向后兼容：只加列/加表/加索引；改名和删除拆成两个版本（先让所有代码停用，再删）。
4. 表结构与仓储层的 SQL（`internal/postgres`）必须一致，不一致会在集成测试（Testcontainers 用本目录建库）和运行时报错。
5. 每个环境执行到哪个脚本需有记录（工单/文档均可；也可在库里建一张执行记录表）。

## 守卫

`tests/arch/ddl_test.go` 在每次 `go test ./...` 时扫描本目录：`text` 而非 `varchar`、`TIMESTAMPTZ`、`SERIAL` 而非 `BIGSERIAL`、不用 PG enum、不物理删除、`IF NOT EXISTS`、复数表名、不用保留字、外键 `NOT NULL`。
有意的偏离写在脚本里被豁免的那一行（或上一行）：`-- ddl-allow: <rule-id>  <理由>`；
第三方原样 vendored、不能改写的 DDL 在文件前几行整文件豁免：`-- ddl-allow-file: <理由>`。
