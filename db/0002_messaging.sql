-- 消息可靠性表：Transactional Outbox + Inbox 幂等台账（pkg/messaging 使用）。
-- 表设计与平台其他服务一致：运维 SQL 与 runbook 通用（库本身每个服务各自一套）。
-- 幂等，可重复执行。

-- Transactional Outbox：待投递事件，与业务变更同事务提交。
-- id 用 BIGSERIAL（偏离团队 int 默认）：流水表清理只删行、不重置序列，int32 会耗尽。
CREATE TABLE IF NOT EXISTS outbox_messages
(
    -- ddl-allow: serial-primary-key  flow table: cleanup deletes rows without resetting the sequence, int32 would run out
    id             BIGSERIAL   NOT NULL,
    message_id     uuid        NOT NULL,
    event_name     text        NOT NULL,
    payload        jsonb       NOT NULL,
    status         text        NOT NULL,
    attempts       integer     NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    published_at   TIMESTAMPTZ NULL,
    trace_parent   text        NULL,
    correlation_id text        NULL,
    last_error     text        NULL,
    CONSTRAINT pk_outbox_messages PRIMARY KEY (id)
);

COMMENT ON COLUMN outbox_messages.status IS 'PENDING | PUBLISHED | FAILED';
COMMENT ON COLUMN outbox_messages.last_error IS '最近一次投递失败的原因（成功后清空）';

CREATE UNIQUE INDEX IF NOT EXISTS ix_outbox_messages_message_id
    ON outbox_messages (message_id);

-- 投递器轮询用：WHERE status = 'PENDING' ORDER BY id
CREATE INDEX IF NOT EXISTS ix_outbox_messages_pending
    ON outbox_messages (id) WHERE status = 'PENDING';

-- 清理用：WHERE status = 'PUBLISHED' AND published_at < cutoff
CREATE INDEX IF NOT EXISTS ix_outbox_messages_published_at
    ON outbox_messages (published_at) WHERE status = 'PUBLISHED';

-- Inbox：已消费消息台账，UNIQUE(message_id, consumer) 使重复投递成为空操作。
CREATE TABLE IF NOT EXISTS inbox_messages
(
    -- ddl-allow: serial-primary-key  flow table: cleanup deletes rows without resetting the sequence, int32 would run out
    id           BIGSERIAL   NOT NULL,
    message_id   uuid        NOT NULL,
    consumer     text        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT pk_inbox_messages PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS ix_inbox_messages_message_id_consumer
    ON inbox_messages (message_id, consumer);

CREATE INDEX IF NOT EXISTS ix_inbox_messages_processed_at
    ON inbox_messages (processed_at);
