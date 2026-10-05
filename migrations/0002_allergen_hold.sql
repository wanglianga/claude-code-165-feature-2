-- 0002_allergen_hold.sql 过敏原缺失拦截：高风险成分（坚果/乳制品）未填过敏原时
-- 阻断批次发布，生成门店补录任务；补录后重算可接收机构；交接签收二次确认。

-- ---------- 用户角色：食安专员 ----------
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('store_staff','store_manager','charity_staff','regulator','admin','safety_officer'));

-- ---------- 批次：拦截状态、触发成分、资格快照 ----------
ALTER TABLE batches ADD COLUMN IF NOT EXISTS allergen_held boolean NOT NULL DEFAULT false;
ALTER TABLE batches ADD COLUMN IF NOT EXISTS hold_reason text NOT NULL DEFAULT '';
-- 触发拦截的高风险成分组（如 nuts/dairy，补录前推断值，仅用于提示）
ALTER TABLE batches ADD COLUMN IF NOT EXISTS risk_ingredients text[] NOT NULL DEFAULT '{}';
-- 补录过敏原后重算的可接收机构 id 列表（null=尚未重算；空数组=没有任何机构可接收）
ALTER TABLE batches ADD COLUMN IF NOT EXISTS eligible_charity_ids bigint[];

-- ---------- 机构：高风险受助点与坚果/乳制品风险管理能力 ----------
ALTER TABLE charities ADD COLUMN IF NOT EXISTS high_risk_recipients boolean NOT NULL DEFAULT false;
ALTER TABLE charities ADD COLUMN IF NOT EXISTS allergen_managed boolean NOT NULL DEFAULT false;

-- ---------- 交接单：签收时标签完整再次确认 ----------
ALTER TABLE handovers ADD COLUMN IF NOT EXISTS label_confirmed boolean NOT NULL DEFAULT false;

-- ---------- 异常类型扩展 ----------
ALTER TABLE exceptions DROP CONSTRAINT IF EXISTS exceptions_type_check;
ALTER TABLE exceptions ADD CONSTRAINT exceptions_type_check CHECK (type IN
    ('sold_out','expired','truck_late','no_show','wrong_scan','allergen_missing',
     'spoilage_report','regulatory_check','capacity_rejected','temp_breach',
     'internal_loss','allergen_review','allergen_excluded'));

-- ---------- 凭证类型扩展 ----------
ALTER TABLE vouchers DROP CONSTRAINT IF EXISTS vouchers_type_check;
ALTER TABLE vouchers ADD CONSTRAINT vouchers_type_check CHECK (type IN
    ('source','reservation','confirmation','handover','delivery','distribution','exception','closure',
     'allergen_update'));

-- ---------- 门店任务 ----------
CREATE SEQUENCE IF NOT EXISTS task_no_seq START 1;
CREATE TABLE IF NOT EXISTS store_tasks (
    id            BIGSERIAL PRIMARY KEY,
    task_no       TEXT UNIQUE NOT NULL,
    batch_id      BIGINT NOT NULL REFERENCES batches(id),
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    type          TEXT NOT NULL DEFAULT 'allergen_supplement',
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','done','cancelled')),
    priority      TEXT NOT NULL DEFAULT 'high' CHECK (priority IN ('high','normal')),
    reason        TEXT NOT NULL DEFAULT '',
    -- done 时记录门店选择的处置方式：supplement 补齐标签 / internal_loss 内部报损 / safety_review 等待食安复核
    resolution    TEXT NOT NULL DEFAULT '',
    resolution_by BIGINT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    CHECK (
        status != 'done' OR
        resolution IN ('supplement','internal_loss','safety_review')
    )
);
CREATE INDEX IF NOT EXISTS ix_tasks_store_status ON store_tasks(store_id, status);
CREATE INDEX IF NOT EXISTS ix_tasks_batch ON store_tasks(batch_id);

-- ---------- 食安复核单 ----------
CREATE TABLE IF NOT EXISTS allergen_reviews (
    id                 BIGSERIAL PRIMARY KEY,
    batch_id           BIGINT NOT NULL REFERENCES batches(id),
    store_id           BIGINT NOT NULL REFERENCES stores(id),
    status             TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','approved','rejected')),
    decision_note      TEXT NOT NULL DEFAULT '',
    submitted_by       BIGINT REFERENCES users(id),
    reviewed_by        BIGINT REFERENCES users(id),
    proposed_allergens TEXT[] NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_reviews_batch ON allergen_reviews(batch_id);
CREATE INDEX IF NOT EXISTS ix_reviews_status ON allergen_reviews(status);

-- 内部报损不另建表：exceptions(type='internal_loss') + 批次 cancelled + 任务 done 即完整轨迹。
