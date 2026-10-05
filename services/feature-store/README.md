# feature-store

推荐特征的**在线读取**与**离线回填**服务：谁在什么时刻读到哪个版本的哪个值、值读不到时怎么显式降级、
版本是谁在什么时候按什么理由切的——这三件事是它的职责。

它**不是**特征计算引擎，也**不是**广告/商业化分析存储。

## 1. 职责与边界

负责：

1. 特征定义与版本的注册、状态机（DRAFT→ACTIVE→RETIRED）、隐私级别调整，全部留痕可审计。
2. 在线读：单条与批量（feature × entity 笛卡尔积，硬上限 50 × 20）；**读不到必须给出降级原因**，
   禁止把「读不到」伪装成 0 值或空 Reply。
3. 批量写入与回填：`request_id` 整批幂等 + 逐行结果，回填作业带进度、租约与断点续跑。
4. 版本切换与回滚：乐观校验（`expected_from_version`）+ 追加式审计，回滚是「一次新的切换」而不是删历史。
5. 生命周期与隐私：TTL 清理（`PurgeExpired`）、按主体擦除（`EraseEntityFeatures`）与自助导出（`ListEntityFeatures`）。

不负责（越界即为设计错误）：

- **不做特征计算**。完播率、留存、内容热度、用户兴趣这类指标的**计算与口径归属** `spm`；本服务只在
  `feature_value.source_metric_key` 上记录「这个值是 spm 的哪个指标版本算出来的」（形如
  `play_finish_rate@v3`），值由上游 `WriteFeatures` 写入或回填 worker 搬运（AGENTS.md §7）。
  特征定义侧则只有来源枚举 + 窗口长度：`FeatureSource` 里结构上不存在 AD/PAYMENT/MEMBERSHIP，
  隐私级别 4（用户画像）以外的敏感维度无处登记，`Config.Validate` 还会把导出可见级别再收一道。
- 不替 `recommend-recall`/`recommend-rank` 决定用哪个版本：只提供 ACTIVE 指针与降级信息，排序侧自行选择。
- 不保存明文 PII。`DEVICE`/`IP_HASH` 维度的 `entity_id` 必须是十六进制摘要（`model.ValidEntityID` 拒绝
  任何非 hex 形态，即拒绝明文设备号），`MID`/`AID` 等只存内容/用户主键。
- 不跨库建外键。跨服务只传 `content_id`/`mid`/`spm` 批次引用，数据所有权见 §7。

## 2. 在线读取路径与降级语义

**真实特征值的主存是 Redis（`CacheRedis`），MySQL 里的 `feature_value` 是可重算投影 + 回源兜底。**

```text
GetFeature / BatchGetFeatures
  └─ 解析版本：req.version == 0 → 读 ACTIVE 指针（fs:active:<key>，miss 回源 feature_active_version）
  └─ 主读：fs:val:<feature_key>:<version>:<entity_scope>:<entity_id>（一次 MGET 批量取）
  └─ miss 且 Read.DBFallbackEnabled=true：FeatureValueModel.FindEntries 行构造器 IN 批查回源
  └─ 仍无值 → 按降级矩阵回填 default_value / 上一 ACTIVE 版本，并在响应里标出原因
```

为什么这样分：在线读的单请求 P99 预算撑不住「每请求一次 MySQL 点查 × 笛卡尔积」，所以值主存放缓存；
但缓存不是事实源——缓存被驱逐、被误清、实例重建后都是空的，所以 MySQL 保留同键的同构副本作为回源与
重放依据，两侧都可用 `WriteFeatures`/回填 worker 重新算出来。代价是**一致性靠"删除"而不是"更新"维护**：
写路径在事务提交后 **DEL** 命中的键（不做增量写缓存，半新半旧的缓存比空缓存更难解释），
版本切换只清 `fs:active:<key>`（值键带 version，天然不冲突）。

降级码（`model.Degradation*`，与 `rpc.FeatureDegradation` 对齐，每条返回值都必须带一个）：

| 码 | 含义 | 触发条件 |
| --- | --- | --- |
| 1 `NONE` | 正常取到 | 未过期的当前版本值 |
| 2 `DEFAULT_VALUE` | 用定义里的 `default_value` | 从未写过值，或定义声明的默认口径 |
| 3 `PREVIOUS_VERSION` | 读上一 ACTIVE 版本 | 新版本刚切、值还没铺满 |
| 4 `EXPIRED` | 值已过 `expire_at` | 仅当 `allow_stale=true` 时返回旧值并这样标出 |
| 5 `SOURCE_UNAVAILABLE` | 缓存与 DB 都不可用 | 故障演练/降级窗口，用最后快照或明确不可用 |
| 6 `FEATURE_RETIRED` | 特征已下线 | 定义 `state=RETIRED`，不静默返回残值 |

批量读的三道上限：`model.MaxBatchFeatures`(50) × `model.MaxBatchEntities`(20) = `MaxBatchReadEntries`(1000)
条条目，再按 `model.MaxBatchResponseBytes`(1 MiB) 二次收口——条数上限挡不住「50 个 512 维向量」这种组合。
超限一律报错，不截断（截断会让上游把「缺特征」误读成「值为 0」）。

## 3. RPC 方法（16）

契约见 `rpc/featurestore.proto`，生成物 `rpc/pb/featurestore`、`internal/server`。

| 分组 | 方法 | 一句话 |
| --- | --- | --- |
| 定义 | `RegisterFeature` | 注册一个特征版本，不可变字段冲突即拒 |
| 定义 | `UpdateFeatureState` | DRAFT/ACTIVE/RETIRED 迁移 |
| 定义 | `UpdateFeaturePrivacy` | 单独入口调隐私级别，单独留痕 |
| 定义 | `GetFeatureDefinition` | 单查定义（`version=0` 走 ACTIVE 指针） |
| 定义 | `ListFeatureDefinitions` | 分页 + scope/source/state/`max_privacy_level` 过滤 |
| 读写 | `WriteFeatures` | 批量写值，整批幂等 + 逐行结果 |
| 读写 | `GetFeature` | 单读，缺失必降级 |
| 读写 | `BatchGetFeatures` | 批量读，笛卡尔积硬上限 |
| 版本 | `SwitchFeatureVersion` | 切换 ACTIVE 版本，乐观校验 + 审计 |
| 版本 | `ListVersionSwitches` | 切换/变更审计列表 |
| 回填 | `SubmitBackfillJob` | 提交回填作业，`request_id` 幂等 |
| 回填 | `GetBackfillJob` | 按 `job_id` 或 `request_id` 查进度 |
| 回填 | `ListBackfillJobs` | 分页巡检 |
| 运维/隐私 | `PurgeExpired` | 清理 TTL 过期值（cron 调用） |
| 运维/隐私 | `EraseEntityFeatures` | 按主体擦除（隐私工单执行） |
| 运维/隐私 | `ListEntityFeatures` | 按主体自助导出核对 |

## 4. 数据表（`go_video_feature_store`）

迁移在 `deploy/migrations/feature-store/`，共 6 表 113 列，与 `model/` 逐列一致
（`model/migration_sync_test.go` 守卫）。全部 `CREATE TABLE IF NOT EXISTS` + InnoDB + utf8mb4 + 每列 COMMENT，
头部写明用途/数据所有者/回滚/锁风险。

| 表 | 列 | 主键 / 唯一键 | 定位 | 事实源判定 |
| --- | --- | --- | --- | --- |
| `feature_definition` | 20 | `PK (feature_key, version)` | 特征版本口径：类型、主体维度、来源、窗口、维度、`default_value`、隐私级别 | **事实源**，不可重算 |
| `feature_active_version` | 7 | `PK (pointer_id)` + `UNIQUE uniq_feature_key (feature_key)` | 单行指针表：「同一时刻只有一个生效版本」这条不变量的 DB 层保证 | **事实源**，CAS 更新 |
| `feature_value` | 19 | `PK (value_id)` + `UNIQUE uniq_feature_entity (feature_key, version, entity_scope, entity_id)` | 在线值的 MySQL 投影（回源、重放、导出用） | **可重算投影**，不是唯一事实源（主读在 `CacheRedis`） |
| `feature_version_switch` | 15 | `PK (switch_id)` + `UNIQUE uniq_request_switch (request_id, switch_type)` | 追加式审计：激活/切换/回滚/状态/隐私/回填自动切换 6 类 | **事实源**，只增不改不可重算 |
| `feature_backfill_job` | 29 | `PK (job_id)` + `UNIQUE uniq_request_id (request_id)` | 回填台账：范围、进度、租约、错误、`auto_switch` 基线 | **事实源**（作业幂等与断点续跑依据） |
| `feature_write_receipt` | 23 | `PK (receipt_id)` + `UNIQUE uniq_request_op (request_id, op_type)` | 写类操作的执行权回执与首次结果快照 | 幂等回执，非业务事实；按 `Write.ReceiptRetentionSeconds` 回收 |

普通索引只服务真实查询谓词：`feature_value` 的 `idx_expire (expire_at, value_id)` 支撑
「先选主键再按主键批删」，`idx_entity` 支撑按主体导出/擦除，`idx_feature_version_entity` 支撑回填进度统计；
`feature_backfill_job` 的 `idx_state_lease (state, lease_expire_at)` 支撑 worker 认领与租约接管扫描。

隐私与留存：个体维度键只存 `entity_scope + entity_id`（设备/IP 为加盐哈希摘要），
值列 `TEXT`/`BLOB` 形态但按 `MaxDimension=512`、`MaxListValueBytes=12 KiB` 收敛；
明文 PII 列名被 `TestMigrationHasNoForeignKeyOrPIILabel` 禁止，跨库外键同理。

## 5. 方法 ↔ 表 / 缓存 / 幂等键

| 方法 | MySQL | Redis | 幂等键与并发控制 |
| --- | --- | --- | --- |
| `RegisterFeature` | `feature_definition` | 无 | `(feature_key, version)` 复合主键：命中且不可变字段全等 → `reused=true`；不同 → `ErrFeatureDefinitionImmutable`。回执 `op_type=register` |
| `UpdateFeatureState` | `feature_definition` + `feature_active_version` + `feature_version_switch` | `DEL fs:active:<key>` | 回执 `op_type=state_change`；升 ACTIVE 前 `CountUnfinished==0` 且该版本有值或有上一版本；行锁 + 事务 |
| `UpdateFeaturePrivacy` | `feature_definition` + `feature_version_switch(privacy_change)` | `DEL fs:active:<key>`（收紧时） | 回执 `op_type=privacy_change`；级别必须与 `entity_scope` 自洽 |
| `GetFeatureDefinition` | `feature_definition` (+ `feature_active_version`) | `fs:active:<key>` | 只读 |
| `ListFeatureDefinitions` | `feature_definition`（`idx_scope_source`/`idx_state_privacy`） | 无 | 只读，`ps<=100` |
| `WriteFeatures` | `feature_value`（`uniq_feature_entity`）+ `feature_definition` + `feature_write_receipt` | `DEL fs:val:*`（提交后） | 回执 `op_type=write`：`Begin` 取执行权（租约 `Write.ReceiptLeaseSeconds`，过期可接管）→ `MarkDone` 存首次结果，重放 `reused=true` 且**绝不重复计数**；≤500 行 |
| `GetFeature` | 兜底 `feature_value` | 主读 `fs:val:<key>:<ver>:<scope>:<entity>` | 只读 |
| `BatchGetFeatures` | 兜底 `feature_value`（`FindEntries` IN 批查） | 同上（一次 MGET） | 只读，硬上限 50 × 20 且响应 ≤1 MiB |
| `SwitchFeatureVersion` | `feature_active_version` + `feature_version_switch` + `feature_write_receipt` | `DEL fs:active:<key>` | 回执 `op_type=switch`；`LockForUpdate` 行锁 + `expected_from_version` CAS，冲突 → `ErrVersionConflict`；回滚须带 `rollback_switch_id` |
| `ListVersionSwitches` | `feature_version_switch`（`idx_key_switch`/`idx_ctime`） | 无 | 只读 |
| `SubmitBackfillJob` | `feature_backfill_job`（`uniq_request_id`）+ `feature_definition` | 无 | 同 `request_id` 重放返回原作业；`entity_ids <= 1000` 且总字节 `<= 65000` |
| `GetBackfillJob` | `feature_backfill_job`（`job_id` 或 `uniq_request_id`） | 无 | 只读 |
| `ListBackfillJobs` | `feature_backfill_job`（`idx_key_version_state`） | 无 | 只读，`ps<=100` |
| `PurgeExpired` | `feature_value`（`idx_expire`）+ `feature_write_receipt(op_type=purge)` | `DEL` 命中键 | 回执 `op_type=purge`；`limit<=5000`，先选 `value_id` 再按主键批删（不做范围 DELETE，避免间隙锁） |
| `EraseEntityFeatures` | `feature_value`（`idx_entity`）+ `feature_write_receipt(op_type=erase)` | `DEL` 命中键 | 回执 `op_type=erase`；`operator` 必须命中 `Privacy.OperatorPrefixes`（空白名单一律拒绝）；覆盖历史版本残值，保留定义与审计 |
| `ListEntityFeatures` | `feature_value` + `feature_active_version` + `feature_definition` | 无 | 只读，`ps<=100`，可见级别受 `Privacy.ExportMaxPrivacyLevel` 约束 |

`BatchGetFeatures`/`GetFeature` 不产生回执（读操作无需幂等键）；`feature_write_receipt.op_type` 的 8 个取值
（`register/state_change/privacy_change/write/switch/backfill/purge/erase`）正好覆盖全部写类方法——
这就是「新表」被否掉的依据：审计维度靠 `switch_type` + `op_type` 两列展开，不需要额外的隐私审计表。

## 6. 特征版本与回填语义

**版本**：一个 `feature_key` 有多行定义（`PK (feature_key, version)`），但只有一个 ACTIVE 指针。
不可变字段（`value_type`、`entity_scope`、`source`、`window_seconds`、`dimension`）一旦注册不得原地改写，
跨版本必须摘要一致（`FeatureDefinition.SameImmutableAs`），否则切换被拒——口径变了就必须换新 key，
不能让同一个 key 的含义在排序侧脚下漂移。`version=0` 一律表示「按 ACTIVE 指针解析」。

**回填**：`SubmitBackfillJob` 只落台账（`PENDING`），不自己算值。作业状态机
`PENDING → RUNNING → SUCCEEDED|FAILED|CANCELLED`，其中两条迁移刻意不存在：`PENDING→FAILED`（worker 必须先
`Claim` 才能观测失败，否则「谁在何时开始跑」在审计里消失）、`RUNNING→PENDING`（接管由「租约过期的 RUNNING
可被再次 `Claim`」表达，退回 PENDING 会丢 `entities_done` 归属导致重复计数）。
`Claim`/`AddProgress`/`Finish` 都带 `lease_owner` + `lease_expire_at` CAS，进程崩溃后租约到期即被接管，
`cursor_entity_id` 保证断点续跑不重复。`auto_switch=true` 时以 `from_version` 作为乐观基线，
成功后追加 `switch_type=backfill_auto_switch` 审计。`entities_truncated=1` 的作业必须转 `FAILED`：
截断的补数看起来是成功、实际缺一批主体，比明确失败更危险。

**写入的状态闸门只有一处，但分内外两侧**：对外入口 `WriteFeatures` 只接受 ACTIVE 目标版本（DRAFT 行级
拒绝为 `FEATURE_NOT_ACTIVE`、RETIRED 为 `FEATURE_RETIRED`，见 `internal/logic/writefeatureslogic.go`
的 `checkRow`）；内部行构造入口 `model.NewFeatureValue` 只挡契约写明禁止写入的 RETIRED，**DRAFT 必须放行**——
回填目标恒为 DRAFT（`SubmitBackfillJobReq.version` 注释），而「升 ACTIVE 前该版本已有值」是
`UpdateFeatureState` 的上线前置；若行构造也挡 DRAFT，一个 key 的**首个**版本（没有上一版本可降级）
在任何路径上都凑不出可上线的状态。`FeatureDefinition.IsWritable()` 表达的是前者而不是后者，
两侧各有测试守着（`internal/logic/idempotent_writes_test.go`、`model/featurevalue_writegate_test.go`）。

**清理**：`feature_value` 靠 `expire_at` + `PurgeExpired`（cron 周期调用）收敛；
`feature_write_receipt` 靠 `Write.ReceiptRetentionSeconds` 回收，但**必须大于上游最大重试跨度**——
删早了等于作废幂等键，重试会拿到「新执行一遍」而不是回放。

## 7. 依赖与被依赖

- `deploy/migrations/README.md` 与 `scripts/migrate.ps1`：建库与执行迁移（本服务 DSN 见 `etc/featurestore.v1.yaml`）。
- MySQL `go_video_feature_store`（**本服务独占**，其他服务只能通过 RPC 访问）。
- Redis（`CacheRedis`）：在线值主读 + ACTIVE 指针缓存，键前缀 `fs:`。
- 上游写入方：`spm`（行为特征/来源指标）、离线回填 worker、`risk-control`（风控特征）。
- 下游读取方：`recommend-recall`、`recommend-rank`、`risk-control`；`services/cron` 调 `PurgeExpired` 与巡检回填。
- 本服务**不依赖**任何其他 go-video 服务的 RPC（无 `Etcd` 客户端配置项），因此不存在启动顺序耦合。
- 跨服务引用只存 `content_id`/`mid`/`spm` 批次标识，不做跨库 JOIN。

## 8. 配置（`etc/featurestore.v1.yaml`）

| key | 说明 |
| --- | --- |
| `ListenOn` | `0.0.0.0:8130`（本批次 8118~8121 归 live-*，8116/8123 归 recommend-*，8150/8151 归 private-message/open-platform；`0.0.0.0:8080` 是 gateway 的 HTTP 端口，同机必撞） |
| `Etcd.Key` | `featurestore.v1.rpc` |
| `CacheRedis` | 业务缓存。**绝不能命名成 `Redis`**：`zrpc.RpcServerConf` 内嵌了 `Redis redis.RedisKeyConf`（限流用），同名字段会让 `conf.Load` 报 `conflict key redis`，能编译、启动即挂。`config_load_test.go` 用反射守住这条 |
| `DataSource` | 必须含 `go_video_feature_store`（`Validate` 拒绝指向别库的 DSN，防串库写） |
| `Read.MaxBatchResponseBytes` | 只能 ≤ `model.MaxBatchResponseBytes`（1 MiB） |
| `Read.CacheJitterRatio` | 缓存 TTL 向下抖动（0..0.5），防上游按分钟批量写入造成同秒惊群 |
| `Read.ActivePointerCacheSeconds` | ACTIVE 指针缓存秒数，过长会表现成「切了版本在线还读旧版」 |
| `Read.DBFallbackEnabled` | `false` = 缓存 miss 不回源直接降级（故障演练用） |
| `Write.ReceiptLeaseSeconds` | 回执执行权租约，≤ `model.MaxReceiptLeaseSeconds`(300) |
| `Write.ReceiptRetentionSeconds` | 回执保留期，必须 > 租约且 > 上游最大重试跨度 |
| `Write.MaxPurgeRowsPerCall` | ≤ `model.MaxPurgeRows`(5000) |
| `Backfill.LeaseSeconds` / `BatchRows` | 认领租约（心跳续期用同值）/ 单批主体行数（worker 侧，不是 RPC 的 500 行上限） |
| `Backfill.WorkerEnabled` | 默认 `false`：取数来源适配器尚未创建，打开只会让作业稳定 `FAILED` |
| `Privacy.OperatorPrefixes` | 允许触发擦除的 operator 前缀白名单，**空白名单 = 谁都拒绝**（`Validate` 视为启动错误：擦除不可用是合规缺陷） |
| `Privacy.ExportMaxPrivacyLevel` | 自助导出可见的最高级别（默认 4，收紧即削弱主体核对路径） |

`Config.Validate()` 是真实启动自检（不是空函数）：库名归属、`CacheRedis.Host`、四个上限与 model 常量的
上界关系、租约与保留期的大小关系、隐私白名单非空且无空白项、导出级别必须是已声明的枚举值——
这些都能在「配置能加载」的前提下把服务跑挂或让隐私承诺失效。

## 9. 当前阶段（重要）

**逻辑轮已落地：16 个 logic 全部实现，无一处 `ErrNotImplemented` 占位。**

- `internal/svc/servicecontext.go` 已装配 `DB`（`sqlx.SqlConn`）、`Cache`（`*redis.Redis`，`CacheRedis.Host`
  为空时为 `nil`）与 6 个 model（`Definitions`/`ActiveVersions`/`Values`/`Switches`/`Backfills`/`Receipts`）；
  启动期由 `Notes()` 打印「这个环境哪些能力必然降级」。
- 写类 8 个方法（register / state_change / privacy_change / write / switch / backfill / purge / erase）
  统一走 `WriteReceiptModel.Begin → 执行 → MarkDone/MarkFailed` 三步，未取得执行权一律回放首次快照。
- 读类 4 个方法（`GetFeature`/`BatchGetFeatures`/`ListEntityFeatures`/`GetFeatureDefinition`）
  共用 `internal/logic/helpers.go` 里的一张降级矩阵（`model.ClassifyDegradation`），
  每条 `FeatureEntry` 都带 `resolved_version` + `degradation` + `event_time`/`expire_at`/`ttl_seconds`。
- 仍未接线的是**回填 worker**：`internal/featuresource`（取数来源接口）不存在，`Backfill.WorkerEnabled`
  保持 `false`，`SubmitBackfillJob` 只落台账（PENDING），`Claim`/`AddProgress`/`Finish` 无调用方。
- 迁移脚本只在**隔离实例**（`127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）上跑通：2026-09-21 全量 `up` +
  `status` 无 pending，库 `go_video_feature_store` 现有 6 张业务表 + `schema_migrations` 台账表。
  真实/共享实例上的执行与索引/EXPLAIN 比对仍未做；静态侧另有 `model/migration_sync_test.go` 逐列比对 model 与 DDL。
- **消费者只有一处，且尚未真正打通**：`gateway/admin` 已于 2026-09-22 接入本服务客户端
  （`FeatureStoreRPC` → etcd key `featurestore.v1.rpc`，供 `/admin/feature-store` 的 13 条运营路由使用，
  见 `gateway/admin/README.md`）；`spm`、`recommend-recall`、`recommend-rank` 仍没接入本服务客户端。
  `risk-control` 声明了 `FeatureStoreRPC`（`json:",optional"`）但 `etc/riskcontrol.v1.yaml` 里整段注释未启用；
  该服务 README/配置注释已于 2026-09-21 更正为对端实际注册的 `featurestore.v1.rpc`（此前写作
  `feature-store.v1.rpc`，且把本服务描述成「只有 README、无 .proto」）。接线时以本文件 §8 为准。
  因此本服务当前对线上行为仍零影响：唯一的调用方在网关侧，而本服务自己还没在任何环境跑起来。
- 服务未在任何环境注册到 Etcd，`ListenOn: 0.0.0.0:8130` 只是为本地同机共存分配的端口。

## 10. 已知缺口

### 分组 A：设计取舍与跨服务契约（既有 1-11）

1. **没有快照表，「快照」由三件东西合成**：回执（`feature_write_receipt`）+ 追加式审计
   （`feature_version_switch`）+ ACTIVE 指针的 `previous_version`。因此 `model.ReadOutcome` 里没有
   snapshot 字段，读响应能声明「服务的是哪个版本、是否降级」，但**无法**声明「整批条目来自同一次快照」：
   一次批量读跨过了切换时，两条条目可能来自不同版本。契约若要真正的快照语义，需要下一轮加
   `snapshot_id` 并在指针切换时推进它。
2. **`SOURCE_UNAVAILABLE` 只能回落默认值**：`ReadOutcome` 无「最近一次可用快照」的位置，
   缓存与 DB 同时不可用时按 `DEFAULT_VALUE` 的载荷 + `SOURCE_UNAVAILABLE` 标记返回（`found=false`），
   而不是返回一份带旧时间戳的值。
3. **回填取数来源未落地**：`internal/featuresource` 尚不存在，`Backfill.WorkerEnabled` 保持 `false`；
   全量扫描的 `entities_total` 提交时固定为 0（本服务没有跨域「主体全集」可读，猜分母等于说谎），
   由 worker 开跑后填。`entities_truncated` 提交侧恒为 0（超限一律报错，不截断）。
4. **`SwitchFeatureVersionReq` 表达不出「回滚」**：契约只有 `to_version` + `expected_from_version`，
   实现只能在 `to_version == previous_version` 时把审计的 `switch_type` 记成 `rollback` 并回填
   `rollback_switch_id`。调用方无法显式声明「这是一次回滚」，也无法要求「只允许回滚」。
5. **`WriteFeatures` 的整批明细不总是可回放**：回执 `result_json` 列宽 2048 字节，500 行的逐行结果装不下。
   超宽时落 `DetailKept=false`（只存 written/rejected 计数），回放返回**计数 + 空 results**，
   完整首次结果由 `result_digest` 证明存在但不重放。要可重放明细需要另立一张明细表。
6. **擦除存证缺外部承接**：`EraseEntityFeatures` 规划里要向 `audit` 服务补一条存证，但 `audit` 尚无对应
   RPC 方法。本服务侧也没有留任何擦除审计行——`feature_version_switch` 的 `switch_type` 枚举里没有
   「个体删除」这一类，且它的 from/to 列表达的是版本而不是个体（写进去会污染版本审计的可读性）。
   当前可证明擦除发生的只有回执行的 `affected_rows` + `request_id`，日志按隐私纪律不含 `entity_id`。
7. **`features_touched` 的口径是实现事实而不是契约原文**：按「本次真正擦除的行里出现的 distinct
   `feature_key`」计数，而不是蓝图里的 `CountByEntity`（那个数在擦除后必然为 0，擦除前又包含没被本次
   条件命中的行）。若下游按「该主体共有几路特征」理解这个字段，需要契约评审时改名。
8. **无人真正消费**，故缺少跨服务端到端契约测试；`docs/` 里的服务清单也未包含本服务的接入说明
   （本轮禁止改 `docs/`，需要维护者在接入时同步）。
9. **`feature_value` 与 Redis 的一致性只有"删除"这一种手段**，没有双写校验任务；
   若要长期保证两者一致，需要一个对账 job（尚未设计）。过期清理与隐私擦除已改为「先选完整行、
   按主键删、删后 DEL 精确的键」，因此删除集合与缓存失效集合不会错位。
10. **候选侧批量读的粒度对不上调用方预期**：`recommend-rank` 的 `Options.FeatureBatchSize` 默认 128（一次取 128 个候选），
    而本契约 `BatchGetFeaturesReq.entities` 上限 20 —— 接线后它必须把一次排序拆成 ~7 次 RPC。
    要么由调用方分批（正确但需要它知情），要么在下一轮契约评审里把 entities 上限按「实体侧特征」抬高并同步收紧
    笛卡尔积总条目与响应字节上限。**本轮不擅自改上限**：现在的 50 × 20 + 1 MiB 三道闸是自洽的。
11. **`WriteFeatures` 的跨来源写入例外**：契约只说 writer 要「与定义 source 一致或为受控系统」，
    实现把「受控系统」解释为 `operator` 前缀 `system:` / `offline-job:`。这是实现选择而不是契约条款，
    若接入方（尤其 `risk-control`）用了别的身份命名，需要在契约评审时把受控身份列成显式配置。

### 分组 B：隐私导出与分页读侧（2026-10-03 逐条读源码复核，每条都有判别用例）

12. **导出隐私上限配置越界时「失效开放」到最高敏感级**：`listentityfeatureslogic.go:55-59` 在
    `!model.ValidPrivacyLevel(maxPrivacy)` 时把上限设成 `model.PrivacyUserProfile`（`model/types.go:150`，值 = 4，
    即 `ValidPrivacyLevel` 的上界）。注释写的是「不能因为一个坏配置变成全部可见」，代码做的正是全部可见：
    把 `Privacy.ExportMaxPrivacyLevel` 留空 / 写成 0（想「关闭导出」的常见写法）会被静默改成 4，
    主体的画像级特征（级别 4）反而进入导出集。只有越界的**上**半区（>4）才是收紧。
    修法应当是收敛到 `PrivacyPublicAggregate` 或直接拒绝启动，而不是取上界。
    钉：`TestListEntityFeaturesBadExportCeilingFallsOpenToTheMostSensitiveLevel`。
13. **下限高于上限时返回空 Reply，与「这个主体没存任何东西」不可区分**：
    `listentityfeatureslogic.go:60-64` 直接 `return &rpc.ListEntityFeaturesReply{}, nil`——
    没有错误、没有日志、`Entries` 为 `nil`、`Total` 为 0；而「库里确实没有行」走的是
    `:77` 的 `make([]*rpc.FeatureEntry, 0, 0)` + `Total=0`。两条路径在 `len()` 口径下同值，
    调用方（当前是 `gateway/admin` 的合规页）无法告诉主体「这一档不对外导出」还是「没有被存过」。
    契约里也**没有**承载这个区别的字段（`ListEntityFeaturesReply` 只有 `entries`/`total`）。
    钉：`TestListEntityFeaturesFloorAboveCeilingIsAnEmptyReplyWithNoRead`（同时钉住「零次读库」）。
14. **分页 `pn` 无上界，`OFFSET` 用 int32 相乘会溢出成负数**：
    `model.ValidatePageSize`（`model/types.go:410-418`）只挡 `pn < 1` 与 `ps` 越界，
    两条列表路径的偏移都由 int32 直接相乘——`model/featureversionswitch.go:257` 的
    `(f.Pn-1)*f.Ps` 与 `model/featurevalue.go:880` 的 `(pn-1)*ps`。`ps<=100`（`MaxListPageSize`）时
    `pn` 超过约 2.1e7 即翻负，MySQL 对负 `OFFSET` 直接报语法错，
    错误经 `List`/`ListByEntity` 原样外传成 5 类内部错误文本，而不是「页码太大」的业务错误。
    钉：`TestListVersionSwitchesAcceptsUnboundedPageNumbers`。
15. **版本切换审计「查得到却传不出」**：`feature_version_switch` 有 15 列、6 类 `switch_type`
    （§4），但 `rpc/featurestore.proto:325-334` 的 `SwitchRecord` 只有 8 个字段，
    `switch_type`、`from_value`/`to_value`、前后摘要、`trace_id`、`rollback_switch_id` 都不在契约里，
    `listversionswitcheslogic.go:57-66` 因此只投影 8 列。后果是审计读侧**无法区分**
    `state_change` 与 `privacy_change`（同 key、同操作人、同 request_id 的两行渲染完全一致），
    也看不出一次切换是不是回滚——缺口 4 想表达的「回滚」意图在列表侧同样丢失。
    **修法只能先改 proto 再 `./scripts/gen.sh feature-store` 重新生成**，不能靠改 logic 绕过。
    钉：`TestListVersionSwitchesProjectsEveryAuditTypeWithoutTheTypeColumn`。
16. **`ListVersionSwitches` 没有响应字节闸门**：导出路径有 `:109-112` 的 `proto.Size(reply)` 对
    `MaxResponseBytes()` 的复核，审计列表路径（`listversionswitcheslogic.go` 全文）没有对应检查，
    `ps=100` × 长 `reason` 可以稳定开出比导出侧更大的应答。两侧的分页上限同为 100，
    所以这不是「审计行更小」的合理结果，而是漏了一条闸。
    钉：`TestListVersionSwitchesPageSizeComesFromTheRequestNotTheModelClamp`（同时钉住 `MaxResponseBytes=64` 被忽略）。
17. **导出路径的降级错误分支不可达**：`listentityfeatureslogic.go:88-101` 以
    `DefinitionFound: true` 硬编码调用 `model.ClassifyDegradation`，而该函数唯一的 `err` 出口就是
    `!o.DefinitionFound`（`model/types.go:322-324`），因此 `:99-101` 的包装分支永不执行。
    它不是错（定义缺失已由 `:80-85` 的 `def == nil` 拦住），但**日志与错误文案里这条路径无法被观测**，
    属于覆盖边界而非生产缺陷；若将来给 `ClassifyDegradation` 加新的错误出口，这里必须同步补用例。

## 11. 验证

```bash
go build ./services/feature-store/...
go vet ./services/feature-store/...
go test -p 1 -count=1 ./services/feature-store/...   # config 加载/自检 + model↔DDL 一致性（-p 1：Windows 页面文件限制）
gofmt -l services/feature-store                   # 必须为空
```

契约变更后重新生成（禁止手改 `internal/server`、`rpc/pb`，见 docs/commands.md §5）：

```bash
./scripts/gen.sh feature-store   # 内部即 goctl rpc protoc --zrpc_out=.. --module $(go list -m)
git diff -- services/feature-store/rpc services/feature-store/internal/server
```

建库建表（**已在隔离实例复验**：`127.0.0.1:3399`，库 `go_video_feature_store`，
6 个迁移文件 ↔ 6 张业务表逐一对上，2026-09-21；真实/共享实例（本机 3306 是维护者的库）从未写入，
上线仍须由维护者在目标实例按下述命令执行）：

```powershell
./scripts/migrate.ps1 up -Service feature-store      # DSN 取自 etc/featurestore.v1.yaml
./scripts/migrate.ps1 status                          # 核对 schema_migrations 已应用的版本
```

## 12. 测试覆盖

离线单测（纯 Go 替身，无 DB/Redis/gRPC 依赖）。数字由 `grep -cE '^func Test'`（已排除 `TestMain`）
与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. `internal/logic`（8 个用例文件 + `fakes_test.go` 替身层）— `116/46`

| 文件 | 用例 | 守住的判定链 |
| --- | --- | --- |
| `definition_registry_test.go` | 23/9 | 注册不可变字段、`SameImmutableAs`、scope↔隐私自洽、分页与过滤 |
| `cas_transitions_test.go` | 21/4 | 状态机单向迁移、`expected_from_version` CAS、行锁与 `CountUnfinished` 上线前置 |
| `entity_feature_export_test.go` | 10/15 | 隐私上下限分别成列（缺口 B12/B13）、只导出生效版本、定义分块读、字节闸、过期标记 |
| `freshness_versioning_test.go` | 16/7 | `resolved_version`/`event_time`/`expire_at`/`ttl` 与降级矩阵逐条对齐 |
| `lifecycle_purge_erase_test.go` | 14/1 | 先选主键再批删、擦除前缀白名单、缓存失效集合与删除集合同构 |
| `idempotent_writes_test.go` | 13/0 | 回执 `Begin/MarkDone/MarkFailed` 三步、租约接管、重放不重复计数 |
| `backfill_ledger_test.go` | 12/5 | 台账状态机两条不存在的迁移、`truncated` 必须 `FAILED`、断点游标 |
| `version_switch_audit_test.go` | 7/5 | `switch_id DESC` 跨页、审计列投影缺失（缺口 B15）、无字节闸（缺口 B16）、`pn` 无上界（缺口 B14） |

### 2. `model` — `12/1`

- `featurevalue_writegate_test.go`(4/1)：内外两侧写闸——对外只允许 ACTIVE，对内 `NewFeatureValue`
  必须放行 DRAFT（否则首个版本永远凑不出可上线状态，见 §6）。
- `migration_sync_test.go`(8/0)：6 表逐列 ↔ DDL 一致性、无外键、无明文 PII 列名。

### 3. `internal/config` — `5/2`

`config_load_test.go`：示例配置可加载；反射守住 `CacheRedis` 不得命名成 `Redis`（`conflict key redis`
会让服务启动即挂）；`Validate()` 的上界/大小关系与隐私白名单非空各有一组对照用例。

### 4. 门禁口径（实测）

- 构造器覆盖 **16/16**：探针取 `internal/logic` 全部 `New*Logic(`，逐个在 `*_test.go` 查引用，无 `GAP`。
- 规模 **133 顶层 + 49 子用例**（logic 116/46、model 12/1、config 5/2）。
- `go vet ./services/feature-store/...` 无输出；`go test -p 1 -count=1 ./services/feature-store/...`
  全部 `ok`；**0 skip、0 fail**；`gofmt -l` 为空。
- 断言强度：拒绝类用例一律断「零次 recorded op」而不是「返回了错误」；列表类断逐字段等值 +
  手写期望序（`feature_key ASC, version ASC` / `switch_id DESC`），值经 `Payload()` 两侧同口径比较，
  因此标量替身冒充列表行会红；字节闸用 size / size-1 对照。缺口 B12-B16 的每条都由
  判别性布景钉住（例：同 key 同 request_id 的两类审计行渲染一致才证明 `switch_type` 未被投影）。
