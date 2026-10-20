-- 订单的乐观并发版本号（取代系统列 xmin）：GORM 的 optimisticlock 插件在经模型 API 的更新里
-- 自动加 WHERE version = ? 与 SET version = version + 1；Raw SQL 绕过插件，必须自己自增
-- （internal/postgres/order_repository.go 的 reaper）。漏了自增的写入不会被并发检测发现。
-- 幂等，可重复执行。
ALTER TABLE orders ADD COLUMN IF NOT EXISTS version integer NOT NULL DEFAULT 1;

COMMENT ON COLUMN orders.version IS '乐观并发版本号（每次更新 +1）';
