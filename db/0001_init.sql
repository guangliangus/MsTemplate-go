-- MsvcService 初始表结构（示例业务：订单）。
-- 幂等，可重复执行。应用启动永不改库：本目录脚本由 DBA/开发手动执行，先 DDL 后发代码。

CREATE TABLE IF NOT EXISTS orders
(
    id           SERIAL        NOT NULL,
    customer_id  text          NOT NULL,
    status       text          NOT NULL,
    total_amount numeric(10,2) NOT NULL,
    created_at   TIMESTAMPTZ   NOT NULL,
    created_by   text          NOT NULL,
    updated_at   TIMESTAMPTZ   NOT NULL,
    updated_by   text          NOT NULL,
    CONSTRAINT pk_orders PRIMARY KEY (id),
    CONSTRAINT ck_orders_total_amount CHECK (total_amount >= 0)
);

COMMENT ON COLUMN orders.customer_id IS '下单消费者的 sub（CONSUMER 平面编号）';
COMMENT ON COLUMN orders.status IS 'PENDING | CONFIRMED | CANCELLED';

CREATE INDEX IF NOT EXISTS ix_orders_customer_id
    ON orders (customer_id, id DESC);

-- 过期订单回收任务扫描：WHERE status = 'PENDING' AND created_at < now() - timeout
CREATE INDEX IF NOT EXISTS ix_orders_pending_created_at
    ON orders (created_at)
    WHERE status = 'PENDING';

CREATE TABLE IF NOT EXISTS order_items
(
    id         SERIAL        NOT NULL,
    order_id   integer       NOT NULL,
    sku        text          NOT NULL,
    quantity   integer       NOT NULL,
    unit_price numeric(10,2) NOT NULL,
    CONSTRAINT pk_order_items PRIMARY KEY (id),
    CONSTRAINT fk_order_items_order_id FOREIGN KEY (order_id) REFERENCES orders (id),
    CONSTRAINT ck_order_items_quantity CHECK (quantity > 0),
    CONSTRAINT ck_order_items_unit_price CHECK (unit_price >= 0)
);

CREATE INDEX IF NOT EXISTS ix_order_items_order_id
    ON order_items (order_id);
