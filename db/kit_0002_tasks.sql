-- 数据库任务队列（pkg/taskqueue 使用；表设计与平台其他服务一致，运维 SQL 通用）。
-- 多副本用 FOR UPDATE SKIP LOCKED 抢占，UNIQUE(queue_name, task_id) 保证幂等入队，
-- attempts 由每次抢占自增（重试预算/退避梯度依据）。幂等，可重复执行。

CREATE TABLE IF NOT EXISTS task_queues
(
    id         SERIAL      NOT NULL,
    queue_name text        NOT NULL,
    task_id    text        NOT NULL,
    priority   integer     NOT NULL DEFAULT 0,
    attempts   integer     NOT NULL DEFAULT 0,
    payload    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    deliver_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    popped     boolean     NOT NULL DEFAULT false,
    popped_at  TIMESTAMPTZ NULL,
    popped_by  text        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by text        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by text        NOT NULL,
    CONSTRAINT pk_task_queues PRIMARY KEY (id),
    CONSTRAINT uq_task_queues_queue_name_task_id UNIQUE (queue_name, task_id)
);

COMMENT ON COLUMN task_queues.popped IS '已被 worker 抢占（僵死超时后由 fixer 重置）';

-- priority >= 0（越大越先弹出）。ADD CONSTRAINT 没有 IF NOT EXISTS，用 pg_constraint 判断保持幂等。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'task_queues'::regclass
          AND conname = 'ck_task_queues_priority'
    ) THEN
        ALTER TABLE task_queues
            ADD CONSTRAINT ck_task_queues_priority CHECK (priority >= 0);
    END IF;
END
$$;

-- Pop 扫描，与 Store.Pop 的抢占 SQL 逐列一致：
--   WHERE queue_name = $1 AND popped = false AND deliver_on <= now()
--   ORDER BY priority DESC, deliver_on ASC, id ASC
CREATE INDEX IF NOT EXISTS ix_task_queues_lookup
    ON task_queues (queue_name, priority DESC, deliver_on, id)
    WHERE popped = false;

-- Fixer 扫描：WHERE popped = true AND popped_at < now() - stale_after
CREATE INDEX IF NOT EXISTS ix_task_queues_abnormal
    ON task_queues (popped_at)
    WHERE popped = true;
