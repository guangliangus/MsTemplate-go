-- 下单的业务幂等：客户端在 Idempotency-Key 头里给的请求 id 存进订单行，同一个消费者的同一个请求 id
-- 只会有一张订单。唯一索引和订单行、outbox 行在同一个事务里生效，所以保证跨副本、跨重启都成立：
-- 重复的请求要么先查到已有订单，要么插入时撞上这个索引（并发的重复请求会等前一个事务结束），
-- 不会出现第二张订单或第二个事件（internal/order/service.go 的 Create）。
-- 空串表示请求没带 key，不参与去重。幂等，可重复执行；大表上由 DBA 改用 CREATE UNIQUE INDEX CONCURRENTLY。
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_id text NOT NULL DEFAULT '';

COMMENT ON COLUMN orders.request_id IS '下单请求的 Idempotency-Key（空串 = 未提供）';

CREATE UNIQUE INDEX IF NOT EXISTS ix_orders_customer_id_request_id
    ON orders (customer_id, request_id)
    WHERE request_id <> '';
