-- 0002_allergen_interception.sql 过敏原缺失拦截：发布阻断、补录任务、签收再确认

-- 批次：过敏原缺失拦截标记（阻止发布给公益机构）
ALTER TABLE batches ADD COLUMN IF NOT EXISTS publish_blocked BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS ix_batches_publish_blocked ON batches(publish_blocked) WHERE publish_blocked;

-- 交接：签收时接收方再次确认过敏原标签完整
ALTER TABLE handovers ADD COLUMN IF NOT EXISTS label_confirmed BOOLEAN NOT NULL DEFAULT FALSE;

-- 异常类型扩展：内部报损 / 高风险过敏原排除敏感受助点
ALTER TABLE exceptions DROP CONSTRAINT IF EXISTS exceptions_type_check;
ALTER TABLE exceptions ADD CONSTRAINT exceptions_type_check CHECK (type IN
    ('sold_out','expired','truck_late','no_show','wrong_scan','allergen_missing',
     'spoilage_report','regulatory_check','capacity_rejected','temp_breach',
     'internal_write_off','allergen_risk_excluded'));

-- 用户角色扩展：食安专员（复核过敏原拦截）
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN
    ('store_staff','store_manager','charity_staff','regulator','admin','food_safety_officer'));

-- 过敏原补录任务（门店待办）
CREATE SEQUENCE IF NOT EXISTS allergen_task_no_seq START 1;

CREATE TABLE IF NOT EXISTS allergen_tasks (
    id          BIGSERIAL PRIMARY KEY,
    task_no     TEXT UNIQUE NOT NULL,
    batch_id    BIGINT NOT NULL REFERENCES batches(id),
    store_id    BIGINT NOT NULL REFERENCES stores(id),
    reason      TEXT NOT NULL DEFAULT '',
    risk_groups TEXT[] NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN
        ('open','supplemented','write_off','review_pending','review_approved')),
    resolution  TEXT NOT NULL DEFAULT '',
    review_note TEXT NOT NULL DEFAULT '',
    created_by  BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resolved_by BIGINT REFERENCES users(id)
);
-- 同一批次同一时刻最多一条待处理（open/review_pending）任务
CREATE UNIQUE INDEX IF NOT EXISTS ux_allergen_tasks_open
    ON allergen_tasks(batch_id) WHERE status IN ('open','review_pending');
CREATE INDEX IF NOT EXISTS ix_allergen_tasks_store ON allergen_tasks(store_id, status);
CREATE INDEX IF NOT EXISTS ix_allergen_tasks_batch ON allergen_tasks(batch_id);
