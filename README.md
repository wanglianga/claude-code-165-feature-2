# 临期食品捐赠预约冷链凭证服务

连锁烘焙门店临期食品捐赠的**预约 + 冷链 + 凭证**平台。门店闭店前上报可捐赠批次，公益机构按服务半径/接收能力/冷链能力预约，门店确认后扫码交接，机构签收后上传配送计划并分站点发放。每个批次沉淀 **来源 / 预约 / 确认 / 交接（温控+封签+身份）/ 配送 / 发放 / 异常 / 闭环** 的哈希链凭证，支撑监管抽查与品牌风险控制，避免临期食品在公益流向中失去食安责任。

内置**过敏原缺失拦截**：门店上报疑似含坚果/乳制品但未填写过敏原的食品时，服务阻止该批次发布给公益机构并生成门店补录任务；交接时标签仍缺失则接收方无法签收，门店需选择补齐标签、改为内部报损或等待食安专员复核；补录后重新计算可接收机构（儿童、老人、过敏高风险受助点可能被排除）；接收方签收时须再次确认标签完整。

## 技术栈

Go 1.19+ (Gin) · PostgreSQL 16 · Redis 7 · Kafka 3.7 (KRaft) · Docker Compose

## 一键启动（Docker Compose）

```bash
docker compose up --build
```

| 入口 | 地址 |
|---|---|
| API | http://localhost:8080 （健康检查 `/healthz`） |
| Swagger UI | http://localhost:8081 （OpenAPI 3.0 文档与在线调试） |
| OpenAPI 源文件 | http://localhost:8080/openapi.yaml |
| 捐赠公示（公开） | http://localhost:8080/api/v1/publicity/donations |

启动后自动完成：建表迁移 → 演示数据（2 门店 / 3 机构 / 9 账号）→ 后台过期巡检任务 → Kafka outbox relay 与 consumer。

## 测试账号

| 账号 | 密码 | 角色 |
|---|---|---|
| `store1.manager` | `Store@123` | 中山公园店店长（确认预约） |
| `store1.staff` | `Store@123` | 中山公园店店员（上报/扫码交接） |
| `store2.manager` | `Store@123` | 张江店店长 |
| `charity1.staff` | `Charity@123` | 绿洲食物银行（有 2 辆冷藏车，半径 15km，日能力 200 箱） |
| `charity2.staff` | `Charity@123` | 晨光公益（无冷藏车，半径 8km，日能力 60 箱） |
| `charity3.staff` | `Charity@123` | 童心儿童关怀中心（仅服务儿童/过敏高风险人群） |
| `regulator` | `Regulator@123` | 监管（全链路审计、抽查留痕、关闭异常） |
| `foodsafety` | `FoodSafety@123` | 食安专员（复核过敏原拦截批次） |
| `admin` | `Admin@123` | 平台管理员 |

登录：`POST /api/v1/auth/login` 获取 token，之后请求带 `Authorization: Bearer <token>`。

## 验证方式（本题已实际执行）

```bash
# 1) 单元测试：预约规则/过敏原风险规则/状态机/凭证哈希链/温度校验
go test ./...

# 2) 端到端验证（需服务已启动）：144 项断言，覆盖主流程 + 全部异常场景 + 过敏原拦截专项
./scripts/verify.sh
```

本地验证环境（PostgreSQL 15 + Redis 7，Kafka 未配置时事件落库 outbox 表）实测结论：

- `go build ./...`、`go vet ./...`、`gofmt` 全部通过；
- `go test ./...` 通过（domain 规则与凭证链 13 个用例，含坚果/乳制品关键词检测、敏感受助点拆分、发布拦截与高风险排除预约规则）；
- `./scripts/verify.sh` **PASS=144 FAIL=0**，覆盖：
  - 主流程：上报(4 箱冷链批次) → 预约 → 店长确认 → 待交接清单 → 逐箱扫码 → 完成交接 → 配送计划(2 站点) → 发放闭环 → 公示；
  - 幂等与防重：Idempotency-Key 重放、同批次重复预约 409、重复扫码返回首扫记录、重复开始交接/重复发放幂等；
  - 规则：无冷藏车机构预约冷链批次 422、缺 Idempotency-Key 400、配送箱数不一致 422、接收人证件不符 422、未扫完箱不可完成交接 422；
  - 异常场景：箱号错扫(wrong_scan)、温度超标(temp_breach)、过敏原缺失阻断预约(allergen_missing)、门店临时售罄(sold_out→批次与预约取消)、超过可捐赠时限自动过期(expired)、冷链车迟到(truck_late)、机构爽约(no_show→批次过期)、受助点反馈变质(spoilage_report→3 箱标记 spoiled)、监管抽查(regulatory_check) 与异常关闭；
  - **过敏原缺失拦截专项**：含坚果乳制品未填过敏原 → 拦截发布(publish_blocked)+生成补录任务+机构不可见不可约(PUBLISH_BLOCKED)；补录后重算机构（儿童机构整体排除、老人站点排除、普通机构可约）；敏感机构预约 422(ALLERGEN_RISK_EXCLUDED) 并落 allergen_risk_excluded 异常；配送老人站点 422；交接时标签缺失签收 422(ALLERGEN_LABEL_MISSING)；门店三选一（内部报损→批次取消+internal_write_off 异常 / 申请复核 / 补齐标签）；食安专员复核通过放行、驳回退回门店后补齐标签；签收缺 label_confirmed 400、显式 false 422、确认后放行；
  - 凭证与审计：凭证链 `chain_valid=true`、监管审计接口返回全链路轨迹（含补录任务）、公示发放箱数与交接一致；
  - 事件链路：全部领域事件（含 allergen.task_created/supplemented/review_requested/reviewed/write_off）经事务型 outbox 落库；配置 Kafka 后由 relay 投递、consumer 二次幂等投影公示。

## 本地开发（无 Docker）

依赖：PostgreSQL（建库 `donation`）、Redis、Kafka（可选，不配置则事件仅落库 outbox 表）。

```bash
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/donation?sslmode=disable'
export REDIS_ADDR='localhost:6379'
export KAFKA_BROKERS='localhost:9092'   # 可选
go run ./cmd/server
```

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `HTTP_ADDR` | `:8080` | 监听地址 |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/donation?sslmode=disable` | PG 连接 |
| `MIGRATIONS_DIR` | `migrations` | 迁移目录 |
| `REDIS_ADDR` | `localhost:6379` | Redis 地址 |
| `KAFKA_BROKERS` | 空（禁用） | 逗号分隔 |
| `KAFKA_TOPIC` | `donation-events` | 事件主题 |
| `SEED_DEMO` | `true` | 是否写入演示数据 |
| `TOKEN_TTL` | `24h` | 登录令牌有效期 |
| `EXPIRY_JOB_INTERVAL` | `30s` | 过期巡检周期 |
| `RESERVATION_TTL` | `30m` | 预约未确认自动释放时长 |

## 核心业务流与状态机

```
reported → reserved → confirmed → handover → handed_over → in_delivery → distributed
    │          │          │                     (售罄/爽约/超时) ↓
    └──────────┴──────────┴──→ cancelled / expired
```

1. **上报** `POST /api/v1/stores/{id}/batches`：记录品类、数量、生产时间、保质期、过敏原、是否冷藏、包装完整度、位置、最晚取走时间；自动生成箱号与来源凭证。包装破损直接拒绝；最晚取走时间不得超过保质期末。**疑似含坚果/乳制品但未填写过敏原时，批次被拦截发布并生成补录任务**（见下文「过敏原缺失拦截」）。
2. **预约** `POST /api/v1/batches/{id}/reserve`（需 `Idempotency-Key` 头）：校验服务半径(haversine)、当日接收能力、冷链车辆与冷链容量、取货时限、过敏原已确认且未被拦截、高风险过敏原不流向纯敏感受助点机构；**大批量释放规则**——单次预约 ≥ 机构日能力 50% 时要求 3 小时内取货，保证机构“接得下、发得出”。
3. **确认** `POST /api/v1/reservations/{id}/confirm`（店长）：生成待交接清单 `GET /reservations/{id}/handover-list`。
4. **交接**：`handover/start` → 逐箱 `handover/scan`（核对箱号/温度/封签，错扫记 `wrong_scan`、温度超标记 `temp_breach`）→ `handover/complete`（全部箱扫完 + 接收人证件核验 + 整体温度 + **接收方再次确认过敏原标签完整 `label_confirmed`**；标签缺失时无法签收）。
5. **配送发放**：`delivery-plan`（站点箱数合计须等于交接箱数，受助对象类型须在机构能力范围内，**高风险过敏原批次排除儿童/老人/过敏高风险站点**）→ `delivery-stops/{id}/complete` 逐站发放 → 全部完成自动闭环并生成**捐赠公示**。
6. **反馈**：`distributions/{id}/feedback` 受助点反馈变质 → `spoilage_report` 异常 + 对应箱标记 `spoiled`。

## 过敏原缺失拦截

```
上报疑似含坚果/乳制品且未填过敏原
  → publish_blocked=true（机构不可见、不可预约）+ 门店补录任务(open) + allergen_missing 异常
交接前/交接中发现标签缺失（门店或监管 POST /batches/{id}/exceptions allergen_missing）
  → 同上拦截；签收被拒（422 ALLERGEN_LABEL_MISSING）
门店三选一：POST /batches/{id}/allergen/resolve
  ├─ supplement     补齐标签 → 解除拦截，任务 supplemented，重算可接收机构
  ├─ write_off      改为内部报损 → 批次与生效预约取消，任务 write_off，internal_write_off 异常
  └─ request_review 等待食安专员复核 → 任务 review_pending
食安专员复核：POST /batches/{id}/allergen/review（foodsafety / regulator）
  ├─ approve → 解除拦截，任务 review_approved
  └─ reject  → 任务退回 open，批次保持拦截
补录/复核通过后：PATCH /batches/{id}/allergens 或 GET /batches/{id}/eligible-charities
  → 重算可接收机构：含坚果/乳制品批次排除儿童(children)、老人(elderly_care)、
    过敏高风险(allergy_high_risk)受助点；仅服务敏感人群的机构整体不可接收
签收：handover/complete 必须携带 label_confirmed=true（接收方再次确认标签完整）
```

- 任务查询：`GET /api/v1/allergen-tasks?status=&store_id=`（门店仅见本店；食安专员/监管/管理员见全部）。
- 高风险过敏原分组：坚果 `nut`（核桃/杏仁/腰果/花生/芝麻等）、乳制品 `dairy`（奶/芝士/黄油/奶油等），中英文关键词与标准过敏原代码均可识别。
- 同一批次同一时刻最多一条待处理任务（数据库部分唯一索引保证），重复标记幂等复用。

## 异常场景处理

| 场景 | 入口 | 系统行为 |
|---|---|---|
| 门店临时售罄 | `POST /batches/{id}/exceptions` `sold_out` | 批次取消、生效预约取消、异常凭证+事件 |
| 超过可捐赠时限 | 后台任务周期巡检（或 `POST /admin/jobs/expire`） | 批次过期、预约取消、`expired` 异常 |
| 冷链车迟到 | `truck_late` | 留痕不推进状态，门店可决定等待或按爽约处理 |
| 公益机构爽约 | `no_show` | 预约取消、批次过期 |
| 箱号错扫 | 扫码接口自动 | 记录错扫结果 + `wrong_scan` 异常，不中断交接 |
| 过敏原信息缺失 | 上报/预约/交接自动 + 手动标记 | 拦截发布 + 补录任务 + `allergen_missing` 异常；交接签收被拒，门店三选一（补齐标签/内部报损/食安复核） |
| 高风险过敏原排除 | 预约/配送计划自动 | 儿童/老人/过敏高风险受助点被排除 + `allergen_risk_excluded` 异常 |
| 过敏原缺失内部报损 | `POST /batches/{id}/allergen/resolve` `write_off` | 批次与生效预约取消 + `internal_write_off` 异常 |
| 受助点反馈变质 | 反馈接口 `spoiled` | `spoilage_report` 异常 + 箱标记 spoiled |
| 监管抽查 | `regulatory_check` + `GET /batches/{id}/audit` | 抽查留痕；审计接口返回全链路轨迹并实时重算哈希链 |

## 一致性与可靠性设计

- **防重复预约三重保障**：Redis `SET NX` 批次锁 → 数据库部分唯一索引 `ux_reservations_active_batch`（同批次仅一条生效预约）→ `Idempotency-Key`（Redis 缓存 24h + 数据库唯一列兜底）。
- **状态机守卫迁移**：`UPDATE ... WHERE status=<旧状态>` 乐观并发，非法迁移直接拒绝；批次行 `SELECT ... FOR UPDATE` 串行化写操作，同时保护凭证哈希链顺序。
- **幂等**：扫码按 `(交接单, 箱号)` 唯一、交接单按预约唯一、发放按站点唯一、配送计划按预约唯一，重试均返回首次结果。
- **凭证链**：`hash = SHA256(prev_hash | type | payload | created_at)`，审计接口实时重算校验（`chain_valid`）。
- **公示一致性**：发放完成与公示投影在**同一事务**落库；事件经事务型 outbox → Kafka → consumer 二次幂等投影，兼顾强一致与流式扩展。
- **规则阻断留痕**：预约被规则拒绝时，阻断原因在独立事务落库异常（不随业务回滚），形成完整可审计轨迹。

## 目录结构

```
cmd/server/          入口 + 内嵌 openapi.yaml
internal/config/     环境变量配置
internal/domain/     实体、状态机、预约规则、过敏原风险规则、凭证哈希（含单测）
internal/store/      PG 连接池、迁移执行器、事务助手
internal/service/    业务用例（批次/预约/交接/配送/异常/过敏原拦截/审计/公示/过期巡检）
internal/httpapi/    路由、中间件（认证/角色/CORS）、处理器
internal/events/     outbox relay → Kafka、consumer 公示投影
internal/jobs/       过期巡检定时任务
internal/seed/       演示数据
migrations/          SQL 迁移（0001 初始 schema；0002 过敏原拦截）
scripts/verify.sh    端到端验证（144 项断言）
```
