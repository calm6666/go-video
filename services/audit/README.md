# audit

管理员操作、登录、审核动作与敏感数据访问的**追加式**审计存证服务（含哈希链自证与保留期归档）。

- **拥有数据**：`audit_entry`（存证条目）、`audit_chain_head`（哈希链链头）、
  `audit_export_task`（导出任务台账）、`audit_retention_policy`（保留期策略）、
  `audit_archive_batch`（归档批次凭证），库名 `go_video_audit`。
- **提供能力**：单条/批量追加（幂等）、按主键与条件查询、链完整性校验、异步导出、
  保留期策略读写、归档与热表标记。
- **依赖**：MySQL、Redis（读多热路径缓存）、对象存储（导出文件与归档清单，只存引用）、
  `services/cron`（推进导出任务与归档作业）。**不依赖任何业务 RPC**：鉴权在网关与
  `operation`，本服务只记录调用方声称的身份。
- **约束**：
  - 契约里没有任何 Update/Delete 方法；「修正」只能再写一条补偿条目（`action` 形如 `<对象>.revoke`）。
    `model` 接口同样只有 `MarkArchived` 一处 UPDATE，且只写不参与哈希的 `archived_at` 列。
  - 明文 PII 不可入库：来源只有 `ip_hash`/`device_hash`（加盐短哈希），库里不存在明文列；
    前后摘要走格式白名单。
  - 查询/导出必须带时间范围 + 至少一个收窄维度（服务端强制，见下）。
  - 不记录广告投放、订单、支付、投币、分成相关动作，也不为其提供查询维度（AGENTS.md §1）。

## 本期落地范围

契约（`rpc/audit.proto`）+ 数据模型（`model/`）+ 迁移 SQL + 配置装配已完成；
**13 个 logic 方法已全部实现**（2026-09-21 逻辑轮），本服务的 logic 已无 `model.ErrNotImplemented` 桩。
`internal/logic` 的用例清单、构造器比例与断言口径见下方「测试覆盖」小节。
下面「方法与契约」小节里的「将来行为」注释仍是评审依据。

## 方法与契约

`CallContext{caller_service, operator_id, request_id, trace_id, source_ip, device_id}` 所有方法必带：
`request_id` 是写接口幂等键的一部分，`source_ip`/`device_id` 只用于服务端算哈希、绝不落原文。

| 方法 | 类型 | 幂等键 | 说明 |
|---|---|---|---|
| `AppendAudit` | 写 | `event_id` | 追加一条存证。命中已有 `event_id` 返回原条目 + `reused=true`（不新增行） |
| `BatchAppendAudit` | 写 | 每条各自的 `event_id` | 批量追加，单次上限 `Write.MaxBatchSize`；一条失败不影响其余，逐条回结果 |
| `GetAuditEntry` | 读 | — | 按 `entry_id` 或 `event_id` 取一条；未命中用 `found=false` 表达，不返回 gRPC NotFound |
| `ListAuditEntries` | 读 | — | 强约束分页查询（见下）；同时写一行 `data_access` 自审计 |
| `VerifyAuditChain` | 读 | — | 按 `chain_key` 重放区间，回 `intact` / 首个断点序号 / `broken_reason` |
| `CreateAuditExport` | 写 | `request_id` | 提交导出任务（不是同步查询）；重复提交 `reused=true` |
| `GetAuditExport` | 读 | — | 查任务；仅 `succeeded` 且未过期时返回短期签名地址 |
| `ListAuditExports` | 读 | — | 任务列表（按申请人/状态/时间） |
| `RunAuditExportTask` | 写 | `task_id` + 状态条件更新 | 由 `services/cron` 推进一批；本服务不内置 worker（AGENTS.md §3） |
| `ListRetentionPolicies` | 读 | — | 保留期策略列表 |
| `SaveRetentionPolicy` | 写 | `action_domain` + `expect_version` | 乐观锁 upsert；变更本身要由调用方另写一条审计 |
| `ArchiveAuditEntries` | 写 | `request_id` | 归档一条链的一个区间；`purge_hot=true` 时只标记 `archived_at`，不做物理删除 |
| `ListArchiveBatches` | 读 | — | 归档批次凭证列表 |

### 查询硬约束（`repository.CheckQueryWindow`，违反即 InvalidArgument）

1. `start_at`/`end_at` 必填且 `start_at < end_at`，跨度 ≤ `Query.MaxRangeDays`（默认 92 天），
   服务端把实际生效上限回写在 `max_range_seconds`，便于调用方自我修正；
2. 至少给一个收窄维度：`actor_id` / `action` / `action_domain` / `target_type`+`target_id` / `trace_id`；
3. `ps` ≤ `Query.MaxPageSize`（默认 100），超出直接夹到上限；
4. 排序固定 `occurred_at DESC, entry_id DESC`，不接受客户端指定排序键。

这三条是约束不是建议：`audit_entry` 是按年亿级只增表，无界扫描会拖垮数据库；
而「谁在什么时候查了什么」本身也是合规风险点，所以每次读还会回写一行
`action_domain=data_access`、`action=audit.entry.list` 的自审计条目。

## 完整性自证（哈希链）

实现固定在 `model/hashchain.go`，声明同步在 `rpc/audit.proto` 文件头。**改动算法会让已入库
条目无法复算**，只允许在 `schema_version` 递增并新开链时变更。

```
chain_key  = "<action_domain>/<yyyy-MM-dd UTC>"            # DateKey 用 UTC，与部署机时区无关
prev_hash  = 同链上一条的 entry_hash；链头 (seq=1) = sha256hex("go-video/audit/v1/genesis/" + chain_key)
entry_hash = sha256hex(V1_SERIALIZATION)
V1_SERIALIZATION = 19 个字段按序用单字节 0x1F 连接，以 prev_hash 结尾
  1 schema_version  2 chain_key      3 seq        4 event_id
  5 actor_type      6 actor_id       7 action     8 target_type
  9 target_id      10 result        11 before_digest 12 after_digest
 13 reason         14 occurred_at  15 trace_id   16 source_app
 17 ip_hash        18 device_hash  19 prev_hash
不参与哈希：entry_id（由 chain_key+seq 决定）、ctime（入库时间）、archived_at（归档改写，
            其证据由 audit_archive_batch.manifest_hash 覆盖）
```

- **按「域 + UTC 日」分链**而不是一条全局链：单链会把所有写入串行化在一行 `FOR UPDATE` 上；
  分链后校验与归档可按链并行，历史链封口后不再变化，某域异常也不阻塞其它域。
- 创世摘要混入 `chain_key`，因此把 A 链的整段条目搬到 B 链仍然会断链。
- 追加流程（`AppendAudit` 将来实现）：开事务 → `Chains.Ensure` → `LockForUpdate(chain_key)`
  → 算 `seq = head.seq+1`、`prev_hash = head.last_hash` → `ComputeEntryHash` → `Entries.Insert`
  → `Chains.Advance`（`WHERE chain_key=? AND seq=?` 乐观推进）→ 提交。
  任何一步冲突转 `ErrChainConflict` 由调用方重试。
- `LockForUpdate` 在 `session == nil` 时直接返回 `ErrChainConflict`：
  脱离事务的 `FOR UPDATE` 会立刻释放锁，那是**假的串行化保证**，宁可失败也不能假装安全。

## 脱敏规则

| 面 | 规则 | 实现 |
|---|---|---|
| 前后摘要 | 白名单格式：空串 / 64 位十六进制 / `字段名=16 位十六进制` 的分号列表 | `model.ValidDigest`（`Insert` 内拦截，`ErrDigestInvalid`） |
| 自由文本 | `reason` / `actor_name` 命中手机号、邮箱、18 位证件号、`password=…`/`token: …` 即整条拒写 | `model.LooksLikePII`（`ErrDigestLooksPII`） |
| 来源标识 | 只存 `sha256hex(salt + value)` 的前 32 位；盐缺失时拒绝写入而非静默降级 | `model.ShortHash`、`Security.IpHashSaltRef` |
| 配置 | 盐与对象存储凭据在 yaml 里只写**环境变量名**，字面量绝不进仓库 | `etc/audit.v1.yaml` + `config_load_test.go` 断言 |

采用「白名单格式」而不是「关键词黑名单」，是因为黑名单永远列不全；
而审计摘要的语义本来就是「变更点不可逆摘要」，不需要自由文本。

## 大表容量与索引策略

`audit_entry` 的索引逐条对应真实查询路径（DDL 见
`deploy/migrations/audit/000001_create_audit_entry_and_chain_tables.sql`）：
`uniq_event_id`（幂等）、`uniq_chain_seq(chain_key, seq)`（防分叉）、
`idx_occurred(occurred_at, entry_id)`（与固定排序键一致，避免 filesort）、
`idx_actor`、`idx_action`、`idx_domain_time`、`idx_target`、`idx_trace`、`idx_archived`。

**为什么不用 MySQL RANGE 分区**：分区键必须进入每一个唯一键，而本表有两个唯一键
（`event_id` 与 `(chain_key, seq)`），把 `occurred_at` 塞进去就会破坏 `event_id` 的全局幂等语义。
因此容量治理走「策略 + 归档批次」路线：

1. `audit_retention_policy` 按 `action_domain` 给出 `archive_after_days` / `hot_days` / `delete_after_days`，
   校验 `0 < archive_after_days <= hot_days` 且 `delete_after_days == 0 || >= archive_after_days`
   （`model.CanPolicyDays`，单点实现，`repository` 与 `logic` 共用）；
2. `ArchiveAuditEntries` 按链区间导清单（每行 `entry_id/seq/entry_hash`）到对象存储，
   状态机 `pending → writing → verified → purged`（任一阶段失败 → `failed`，终态无出边）；
3. `repository.PurgeMark` 硬要求批次处于 `verified` 且 `manifest_hash` 非空，
   **且不受 `Archive.VerifyBeforePurge` 配置影响**——销毁证据的路径上不允许存在可配置旁路；
4. 物理删除不在本契约内，由 DBA 作业在 `delete_after_days` 窗口外执行（见缺口）。

## 数据所有权结论（本轮裁决，含待评审）

| 争议面 | 结论 | 依据 |
|---|---|---|
| 管理操作存证归谁 | `audit.audit_entry` 是唯一不可抵赖存证；`operation.op_audit_index` 保留为后台控制台轻量索引，两者**长期共存**，用 `request_id` 对齐 | `deploy/migrations/operation/000003_*.sql` 已上线且注释明确写了「长期存证由 services/audit 承接」；迁移历史数据不在本期边界内 |
| 谁写审计 | **发起业务动作的领域服务自己写**（`ops-config` 发布/回滚由 ops-config 写，登录由 operation 写，审核结论由 moderation-orchestrator 写）；同一 `event_id` 只允许一个所有者写 | 否则会出现双写分叉与「谁都没记」的空档；`audit` 只提供写入契约与归因字段 |
| 审计读权限从哪来 | RBAC 归 `operation`，本服务只要求 `CallContext.operator_id` 并由网关侧鉴权；audit 不 import 也不 JOIN 他人表 | AGENTS.md §5；`audit` 无 `operation` RPC 依赖，故障时不因下游不可用而无法存证 |
| 业务主数据 | 一律只存 `target_type` + `target_id` 引用，不复制正文 | 同上 |

**待评审**（成本与取舍已列在下方缺口，需维护者拍板）：`op_audit_index` 是否长期保留、
以及是否需要 operation 侧提供「按 `request_id` 反查 audit 存证」的控制台入口。

## 已知缺口 / 待评审

- **没有物理 DELETE**：契约与 `model` 都没有删除方法，热表只会被标记 `archived_at`。
  好处是删除动作不可能被服务代码误触发；代价是热表体积只随归档标记「逻辑收缩」，
  物理回收必须由库外 DBA 作业完成，本期没有该作业的脚本与验收标准。
- **写侧留痕失败一律被吞掉**：`selfAuditLogged`（`helpers.go:770`）把上链错误降成一条 Error 日志，
  四个写方法都在用它 —— `saveretentionpolicylogic.go:184`、`createauditexportlogic.go:157`、
  `archiveauditentrieslogic.go:312`、`runauditexporttasklogic.go:244`。于是「保留期策略已经改到库里、
  但链上一行痕迹都没有，调用方拿到成功应答」是当前真实形态，
  由 `TestSaveRetentionPolicySwallowsSelfAuditFailure` 钉住现状。
  反向对照：`GetAuditExport`（读接口）对下载留痕失败是 fail-closed 的
  （`getauditexportlogic.go:118`，回 `ErrDownloadTrailUnwritten`）。
  待评审的是「哪一类写必须无痕即不生效」，评审前不改代码。
- **自审计的递归**：`ListAuditEntries` 要写一行 `data_access` 条目，而这条写入本身又是一次写。
  当前实现约定是 `appendSelfAudit` 只经 `prepareEntry + appendIdempotent` 直接上链、
  不调用任何读接口，因此深度恒为 1（`helpers.go:700-706` 的文件内注释）。
  契约谈判点仍是给 `CallContext` 加 `self_audit` 标记还是靠 `caller_service = "audit"` 隐式判定。
- **导出读已归档数据**：`RunAuditExportTask` 需要同时读热表与对象存储里的归档清单，
  后者依赖对象存储客户端（本期未引入任何新依赖），因此跨度超热表窗口的导出会明确失败而不是静默少行。
- **签名地址**：`GetAuditExport` 的 `download_url` 需要一个真实的 S3/OSS 签名实现，
  本期只有 `Storage` 配置与 `ErrObjectStorageMissing` 兜底。
- **哈希盐轮换**：`ShortHash` 用单一盐值，轮换盐会让历史 `ip_hash` 与新写入无法聚合。
  若要支持轮换，需要加盐版本号列（属 DDL 变更，本期未做）。
- **端口与文档登记**：本服务监听 `8110`。`deploy/migrations/README.md` 已有
  `audit | go_video_audit | 3 | applied` 行，`docs/service-catalog.md` 也已列 `audit`；
  `services/README.md` 按设计不放逐服务清单（只有目录边界说明），不需要补行。
- **本期不做**：事件投递（审计写入是同步 RPC，不走事件）、gRPC 错误码到
  `google.rpc.ErrorInfo` 的映射。

## 表与迁移文件

| 表 | 迁移文件 | model |
|---|---|---|
| `audit_entry` / `audit_chain_head` | `000001_create_audit_entry_and_chain_tables.sql` | `model/auditentry.go`、`model/auditchainhead.go` |
| `audit_export_task` | `000002_create_audit_export_task_table.sql` | `model/auditexporttask.go` |
| `audit_retention_policy` / `audit_archive_batch`（含 `default` 策略种子行） | `000003_create_audit_retention_archive_tables.sql` | `model/auditretentionpolicy.go`、`model/auditarchivebatch.go` |

## 配置

```yaml
Name: audit.v1.rpc
ListenOn: 0.0.0.0:8110
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_audit?charset=utf8mb4&parseTime=true
CacheRedis: {Host: 127.0.0.1:6379, Type: node}   # 字段名必须是 CacheRedis：RpcServerConf 内嵌了同名
                                                 # RedisKeyConf，叫 Redis 会让 conf.Load 报 conflict key redis
Security: {IpHashSaltRef: AUDIT_IP_HASH_SALT, MaxUserAgentLen: 255}  # 值是环境变量名，不是密钥本身
Query:    {MaxRangeDays: 92, MaxPageSize: 100, CountTotal: true}
Write:    {MaxBatchSize: 200, ChainRetry: 3, MaxReasonLen: 500}
Verify:   {MaxEntriesPerCall: 50000}
Export:   {BatchRows: 2000, MaxRowsPerTask: 2000000, ObjectTTLSeconds: 604800, PresignTTLSeconds: 300}
Archive:  {MaxEntriesPerBatch: 100000, VerifyBeforePurge: true}
Storage:  {Enabled: false, Endpoint: 127.0.0.1:9000, Region: us-east-1, Bucket: go-video-audit,
           AccessKeyRef: AUDIT_OSS_ACCESS_KEY, SecretKeyRef: AUDIT_OSS_SECRET_KEY,
           UseSSL: false, PathStyle: true}       # 凭据只写环境变量名，密钥不进仓库
```

`Archive.VerifyBeforePurge` 只影响归档作业是否**额外**重算清单；`PurgeMark` 的
「必须 verified + manifest_hash 非空」判定始终生效。

## 运行与测试

```powershell
go run ./services/audit -f services/audit/etc/audit.v1.yaml
powershell -File scripts/gen.ps1 -Service audit   # 契约变更后重新生成，禁止手改 rpc/*.pb.go、internal/server
```

- `go test -p 1 -count=1 ./services/audit/...` 不连接 MySQL/Redis/etcd/对象存储；
  用例清单、构造器比例、替身口径与覆盖边界见下一节「测试覆盖」。
- 迁移 SQL 已在**隔离实例**（`127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）执行并做过
  表名级对账，见 `deploy/migrations/README.md` 的 `applied` 状态；
  本机 3306 是维护者真实库，任何迁移都不得指向它。

## 测试覆盖

离线单测（纯 Go 内存替身，不起 gRPC、不连 MySQL/Redis/etcd/对象存储、不用 `time.Sleep`）。
数字为主代理实测导出（`.gotmp/readme-metrics/audit.txt`、`.gotmp/readme-test-aggregate.txt`），
`grep -cE '^func Test'`（已排除 `TestMain`）/ `grep -c 't.Run('`，格式 `顶层/子用例`。

### 1. `internal/logic` — `86/58`（6 个用例文件 + `fakes_test.go` 替身层）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `append_only_test.go` | 14/4 | 「写下去就不能改」的四层闸：五个 model 接口方法集是封闭白名单（反射断言）、扫 model 源码证明全包无 DELETE/TRUNCATE 且 `audit_entry` 只有一条只写 `archived_at` 的 UPDATE、写入只走 `helpers.appendOnce` 且链冲突重试不留半成品、改列/删行/搬链/重编号四类篡改必须被检出而非回 `intact=true`（`TestTamperAndErasureAreDetected`、`TestVerifyChainReportsTruncatedInsteadOfIntact`） |
| `archive_test.go` | 18/13 | 归档区间必须「完整、连续、可重放、已过保留期」才允许推进 `verified`、`pending→writing→verified→purged` 逐级不可跳过且失败停在 `failed` 不留「产物已落地」引用、三档下游故障的三种处置（原错上抛 / 换不透明哨兵 / 只写日志）、归档区间的条数上限是 `Archive.MaxEntriesPerBatch` 而不是校验接口那道条数闸 |
| `export_test.go` | 16/7 | 提交阶段绝不碰对象存储只落一行 pending、推进阶段任何一步做不到都收敛到 `failed` 且绝不「库记 succeeded 而桶里没对象」、取件只对 `succeeded` 未到期产物签发地址且地址不进任何留痕列、三步都以 `request_id` 幂等（`TestRunAuditExportTaskNeverLeavesTaskRunningOnFailure`） |
| `query_test.go` | 13/13 | 读侧三条硬约束在触库之前拒绝、`ps` 服务端夹取且排序键不可由调用方指定、留痕只记 `字段=16hex` 且自审计深度恒为 1、「未命中」与「查不动」严格区分（`found=false` vs 报错）；`TestKnownGapListTrailStoresRawTargetID` 以哨兵钉住 `target_id` 仍被原文直填的现状 |
| `retention_test.go` | 13/13 | 判定与自洽校验全部发生在碰库之前、「放宽物理清理窗口」的动机检查先于写数据、应答 version 取自回读而非内存结构体、失败用例断的是假库里**真实的残留形态**（`TestSaveRetentionPolicyLoosenWithoutRemarkLeavesRowUntouched`、`TestSaveRetentionPolicySwallowsSelfAuditFailure`） |
| `validation_test.go` | 12/8 | 必填与长度上限逐字段取 model 的列宽常量（改列不改校验即红）、明文 PII 既进不了库也进不了投影回参、校验失败时数据面零调用、盐缺失拒绝一切写入（`TestHashSaltMissingRefusesEveryWrite`、`TestNoPlaintextSourceColumnsOrViewFieldsExist`） |

### 2. 其他层

- `model/hashchain_test.go` — `11/0`：哈希链契约本身。19 个参与哈希的字段逐字段扰动、
  非哈希列（`entry_id`/`ctime`/`archived_at`）不进摘要、创世摘要按 `chain_key` 隔离、
  `ShortHash` 加盐与截断、`ValidDigest` 白名单、`LooksLikePII`、导出与归档两个状态机、`CanPolicyDays`。
- `internal/config/config_load_test.go` — `3/1`：`etc/*.yaml` 逐个真实 `conf.Load`；
  反射守住业务缓存字段必须叫 `CacheRedis`（叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌字段冲突，
  能编译但启动即挂）；`TestSecurityRefsAreEnvNames` 断密钥字段只写环境变量名；
  `TestQueryConstraintsPositive` 断查询约束为正数。
- `internal/repository/`、`internal/svc/` **无离线单测**：`CheckQueryWindow`、链头 `Advance` 等
  仓储侧判定只被 logic 用例经真实 repository 代码路径间接覆盖，没有独立断言。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（审计写入是同步 RPC，不产事件）。
- `fakes_test.go` 是替身层，`top=0 sub=0` 属正常，不是漏计。

### 3. 构造器级覆盖

**13/13**：探针取 `internal/logic` 全部 `New*Logic(` 共 13 个，逐个回查 `*_test.go` 引用，`gaps:` 为空，
与 13 个 RPC 方法一一对应。

### 4. 替身层与断言口径

`fakes_test.go` 提供五个 model 接口替身 + 假对象存储 + deps 装配，被测的是真实 logic 与真实
`internal/repository`。复刻的语义：

- `uniq_event_id` 命中即返回冲突哨兵，用于覆盖 logic 的「回查兜底」分支（替身注释自陈真库这里是
  驱动抛 1062 再由 repository 归一）；
- 链头 `Advance` 可注入「前 N 次被并发抢先」，并同时把 `seq`/`last_hash` 推进一格，让重试那一轮
  读到新的 `prev_hash`——这是无并发条件下唯一能复现链冲突裁决的形状；
- `LockForUpdate` 只接受 session，读回按 `seq` 升序；`seedChain` 造的链逐条按 V1 序列化重算摘要。

证明不了的（如实声明）：真实 SQL 文本与列名、真库唯一索引的并发行为（替身单线程，只模拟抢先的
**结果**而非锁）、`FOR UPDATE` 的真实串行化、对象存储签名与真实读失败形态。

断言口径：拒绝类用例断「数据面/依赖零调用」而不是「返回了错误」；留痕类断按 `seq` 升序的链上实参；
失败类断库里真实的残留形态与批次状态，而不是「应当回滚成什么」。

### 5. 覆盖边界

1. 用例不连接 MySQL/Redis/etcd/对象存储，也不起 gRPC server；`internal/server`、`rpc/*.pb.go`
   与 goctl 生成的壳不在单测范围内。
2. **只验证本服务自己的判定链与落库序列**：审计写入是同步 RPC，本服务既不生产也不消费事件
   （见「本期不做」），因此 MQ 投递、下游消费方的幂等、跨服务事件闭环**都不在覆盖内**，
   也没有任何用例去断言「消息被谁收到」。
3. 导出与归档只对着假对象存储：`GetAuditExport.download_url` 的真实签名实现未落地
   （缺口节已登记，现状只有 `ErrObjectStorageMissing` 兜底），故「文件真的可下载」不可断言。
4. 迁移 SQL 的复验口径是本 README 上一节登记的隔离实例 `127.0.0.1:3399` **表名级**对账；
   列级一致性没有独立对账用例，由 `model` 的列名常量与迁移脚本、以及 `validation_test.go`
   取列宽常量做校验这两处间接兜住。
5. 全仓 `t.Skip` 实测口径中本服务为 0 条。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/audit/...
gofmt -l services/audit      # 必须为空
go vet ./services/audit/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节的「覆盖」只描述用例断言范围，不构成任何门禁结论。
