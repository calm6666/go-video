# risk-control

登录、投稿、评论、弹幕、关注、改名、直播开播等动作的**行为与账号风控**服务：
频控/规则决策、黑白名单、处罚（封禁/挑战/复核）、设备画像与可解释裁决日志。

- 数据所有者：risk-control 服务（本库 6 张表只有本服务可读写，AGENTS.md §5）
- Owner：内容安全与风控域（规则/名单/处罚的运营入口在 `operation`，尚未接入）
- 数据库：`go_video_risk_control`（`deploy/migrations/risk-control/`）
- 注册中心 etcd Key：`risk-control.v1.rpc`，默认监听 `0.0.0.0:8105`
- 契约源：`rpc/riskcontrol.proto`（`rpc/riskcontrol_grpc.pb.go`、`rpc/riskcontrol.pb.go`、
  `internal/server/`、入口 `riskcontrol.v1.go` 均为 `goctl`/`protoc` 产物，禁止手改，
  改契约后执行 `powershell -File scripts/gen.ps1 -Service risk-control`）

## 1. 职责与边界

| 归本服务 | 不归本服务 |
|---|---|
| 行为频率规则（滑窗计数 + 阈值） | 内容安全（涉政/色情/暴恐/审核队列）→ `moderation-orchestrator` / `moderation-worker` |
| 黑名单/白名单、设备画像与设备风险分 | 账号封禁的产品化展示与申诉工单 → `operation` |
| 处罚状态机（ACTIVE→LIFTED/EXPIRED）与裁决回放 | 登录票据与会话 → `account`（本服务只给 `ACTION_LOGIN` 出裁决） |
| 投稿/开播等动作的放行、挑战、复核建议 | 稿件状态机推进 → `video`（本服务不写稿件状态） |
| 裁决审计日志 `risk_check_log` | 埋点/推荐特征/广告分析 → `event-collector` + `spm`（AGENTS.md §7 红线） |

边界要点：

- **内容安全归 moderation，行为/账号风控归本服务。** moderation 关心「这条内容是什么」，
  本服务关心「这个账号/设备/IP 在这个动作上有多可疑」。两者互不写对方的表。
- 审核不通过不等于风控处罚；风控 BLOCK 也不等于内容违规。`video`/`comment`/`danmaku` 在
  发布前分别咨询两者，结论来源不同，不得互相冒充。
- 本服务不接收、不落库、不打日志记录明文 IP、手机号、身份证或设备号原文：
  设备只存 `sha256` 摘要（`model.DeviceHash`），IP 只接受调用方预哈希的 `ip_hash`，
  传裸 IP 直接返回 `ErrRawIPForbidden`（`internal/logic/validate.go`）。

## 2. RPC 方法

`service RiskControl`（gRPC，客户端 `rpc.NewRiskControlClient(zrpc.MustNewClient(...))`）：

| 方法 | 用途 | 幂等 |
|---|---|---|
| `CheckAction` | 对一个受保护动作给出可解释裁决（放行/挑战/拒绝/复核） | `request_id` 唯一，Redis `rc:ck:<request_id>` 回放 + `risk_check_log.request_id` 唯一索引 |
| `ReportAction` | 上报一次已发生的动作，写入滑窗计数 | `event_id` 非空时 `SETNX` 去重，返回 `deduplicated=true` |
| `GetDeviceProfile` | 读设备画像（只回摘要，不回设备号原文） | 只读 |
| `UpsertDeviceProfile` | 写画像/标签/风险分并登记设备-账号关联 | `device_hash` 唯一键 upsert，`first_seen` 取 LEAST、`last_seen` 取 GREATEST |
| `ApplyPunishment` | 下发处罚（全域或单动作） | `idempotency_key` 唯一键；同 `(mid, scope)` 已有 ACTIVE 时返回 `ErrPunishmentAlreadyActive` |
| `LiftPunishment` | 解除处罚（按 `punishment_id` 或精确 `(mid, scope)`） | 已终态返回原记录且 `changed=false`；命中多条时报 `ErrAmbiguousPunishment` |
| `ListPunishments` | 分页查询处罚（运营侧） | 只读 |
| `UpsertRule` | 新建/修改规则，评估字段变化即 `version+1` | 按 `rule_id`/`name`；规则名不可变更 |
| `ListRules` | 分页查询规则 | 只读 |
| `UpsertListEntry` | 写入黑/白名单条目 | `(list_type, target_type, target_value)` 唯一键 upsert |
| `GetListEntries` | 分页查询名单 | 只读 |

写接口一律要求 `operator > 0`（`ErrOperatorRequired`），缺幂等键直接拒绝（`ErrIdempotencyKeyRequired`）。

## 3. 决策语义

`policy.Evaluate` 是纯函数，事实由 `repository.LoadFacts` 一次性装载，优先级固定：

```text
黑名单 → 生效处罚 → 白名单 → （Redis 全盲时的进程内兜底限流）→ 规则评估 → 无命中 ALLOW
```

- **黑名单最优先**：命中即 `BLOCK`，不再评估规则，`basis=blacklist`。
- **处罚优先于白名单**：白名单只豁免规则评估，不能替运营解除处罚，否则「已封禁账号因为在
  白名单里又能动作」无法审计（`basis=punishment`，回带 `PunishmentSnapshot` 与剩余秒数）。
  同时存在全域与动作级处罚时，取更具体的动作级处罚作为依据。
- **白名单命中**：`ALLOW`、`basis=whitelist`、`evaluated=false`（显式表示规则未参与）。
- **规则评估**：命中顺序恒为 `(priority DESC, rule_id ASC)`，与观测顺序无关，保证
  `hit_rule_ids` 可复现；裁决取最严重（ALLOW < CHALLENGE < REVIEW < BLOCK）。
  分数 = 裁决基线（CHALLENGE 25 / REVIEW 50 / BLOCK 100）+ 额外命中每条 5 分，上限 100，
  可复算而非黑盒。
- **不可观测 ≠ 无风险**：窗口超出 `CounterTiers` 最大档位、缺少 `ip_hash`/`device_hash`、
  指标未实现取数时，该规则进入 `skipped_rule_ids`，绝不退化成「值为 0」冒充命中或放行理由。
- `action_code` 只给稳定 key（`risk.action.*`），文案由各客户端按平台渲染（AGENTS.md §6）。

### 降级策略（依赖故障）

| 故障 | 行为 |
|---|---|
| MySQL 不可用（`LoadFacts` 报错） | 高危动作（默认 1 投稿、5 登录、6 改名、7 直播开播）**BLOCK-on-error**，`basis=fallback_db_unavailable`；其余动作 **ALLOW-on-error**（`RiskControl.OnDbFailureDefault: allow`，可切 `block` 全量保守拒绝）。此路径跳过 `risk_check_log` 写入，不再压同一份连接池，但仍写 Redis 回放缓存。 |
| Redis 计数器不可读 | 计数类规则逐条标为不可观测（进 `skipped_rule_ids`），裁决 `degraded=true`；若进程内影子滑窗 `LocalFallbackPerWindow` 被击穿，则 `BLOCK`、`basis=fallback_local_rate_limited` |
| 回放缓存读失败 | 视为未命中，正常重新裁决（幂等回放是优化，不是前提） |
| 审计写入失败 | 不翻转已产出的裁决，仅记 error 日志 |

只有入参非法（动作越界、传明文 IP、`request_context` 超限）才返回 gRPC 错误；
依赖故障一律返回一个带 `basis`/`degraded` 的可解释裁决。

## 4. 数据表

迁移目录 `deploy/migrations/risk-control/`（只建表，可重复执行；回滚见文件头 DROP 清单）：

| 文件 | 表 | 说明 |
|---|---|---|
| `000001_create_risk_control_decision_tables.sql` | `risk_rule` | 规则与版本；`uniq_name`；评估字段变更 `version+1` |
| | `risk_list` | 黑/白名单；`uniq_target(list_type,target_type,target_value)`；`target_value` 只存受控值 |
| | `risk_punishment` | 处罚状态机；`uniq_idempotency_key`；`(mid,scope)` **不建**唯一索引（终态历史行需保留） |
| `000002_create_risk_device_and_check_log_tables.sql` | `risk_device_profile` | 设备画像；`uniq_device_hash`（SHA-256 摘要） |
| | `risk_device_mid` | 设备-账号关联事实，`uniq_device_mid`；`related_mid_count` 是其可重算投影 |
| | `risk_check_log` | 裁决日志；`uniq_request_id`；`hit_rule_ids` 存 `rule_id@version,...`（上限 40 条） |

计数量不在 MySQL：滑窗计数只在 Redis，属可丢弃投影，Redis 清空后由后续上报重建，
期间规则按不可观测跳过（偏保守放行，不会误封）。

## 5. 依赖

| 依赖 | 用途 / 关键标识 |
|---|---|
| Redis（`CacheRedis`） | 滑窗计数 `rc:c:<metric>:<subject>:<action>:<tier>:<bucket>`；上报去重 `rc:evt:<event_id>`；裁决回放 `rc:ck:<request_id>`；规则缓存 `rc:rl:<action>`。只读写 `rc:` 前缀（AGENTS.md §5 禁止跨服务共用业务 key） |
| MySQL（`DataSource`） | 上述 6 张表，库名 `go_video_risk_control` |
| etcd | 本服务注册 `risk-control.v1.rpc` |
| 下游 RPC（可选，**当前禁用**） | `FeatureStoreRPC` → `featurestore.v1.rpc`。feature-store 的 `.proto` 与实现已落地，但本服务侧尚未装配 client，示例配置里整段注释；未配置时设备风险特征降级为 `risk_device_profile.risk_score` / `related_mid_count` + Redis 滑窗计数 |
| 调用方 | 已接入：`gateway/admin`（风控裁决/上报/处罚与名单运营面）、`live-room`（开播与房间处置前置判定）、`private-message`（私信发送前置判定）。预期但尚未接入：`account`（登录/改名）、`video`（投稿）、`comment`/`danmaku`/`social-graph`（互动）、`operation`（规则/名单/处罚管理），这些服务各自的 README 已把 `risk-control` 列为依赖；`moderation-orchestrator` 只咨询行为结论，不下发审核规则 |

## 6. 启动方式

```bash
# 1) 契约变更后重新生成（禁止手改生成产物）
powershell -File scripts/gen.ps1 -Service risk-control

# 2) 建库建表（需本地 MySQL；DSN 取自 etc/*.yaml）
powershell -File scripts/migrate.ps1 -Action up -Service risk-control

# 3) 运行（依赖 Redis + MySQL + etcd）
go run ./services/risk-control -f services/risk-control/etc/riskcontrol.v1.yaml

# 4) 自检
go build ./services/risk-control/... && go vet ./services/risk-control/...
go test ./services/risk-control/...
```

启动日志会打印窗口档位、桶数、默认窗口、兜底限流是否启用与降级默认方向，
用于确认配置按预期生效（AGENTS.md §4 可验证性）。

## 7. 配置 key

`etc/riskcontrol.v1.yaml`：

| Key | 说明 | 默认 |
|---|---|---|
| `Name` / `ListenOn` | 服务名与监听地址 | `risk-control.v1.rpc` / `0.0.0.0:8105` |
| `Etcd.Hosts` / `Etcd.Key` | 注册中心 | `127.0.0.1:2379` / `risk-control.v1.rpc` |
| `CacheRedis` | 业务缓存（滑窗计数 + 裁决回放 + 规则缓存）。**键名不能写 `Redis`**：`zrpc.RpcServerConf` 内嵌同名 `RedisKeyConf`，`conf.Load` 会报 `conflict key redis` 导致服务起不来 | `127.0.0.1:6379`, node |
| `DataSource` | MySQL DSN，库名必须是 `go_video_risk_control` | 本地示例；生产从配置中心/Secret 注入 |
| `RiskControl.DefaultWindowSeconds` | `ReportAction` 未指定窗口时的窗口，也决定去重 TTL 与兜底窗口 | 60 |
| `RiskControl.CounterTiers` | 计数档位（秒）。规则 `window_seconds` 向上取齐到最近档位；超过最大档位即不可观测 | `60,600,3600` |
| `RiskControl.WindowBuckets` | 每档位桶数（误差/读放大权衡） | 6 |
| `RiskControl.ChallengeTTLSeconds` | CHALLENGE 建议有效期；处罚期取「配置值与处罚剩余」较小者 | 300 |
| `RiskControl.DecisionCacheSeconds` | 同 `request_id` 裁决回放时间（幂等窗口） | 60 |
| `RiskControl.RuleCacheSeconds` | 启用规则集合缓存时间（规则变更时主动失效） | 30 |
| `RiskControl.HighRiskActions` | DB 故障时 BLOCK-on-error 的动作枚举值；为空时回落到代码默认（1/5/6/7），避免配置缺失改变语义 | `1,5,6,7` |
| `RiskControl.OnDbFailureDefault` | 其余动作的降级方向：`allow`（可用性优先）或 `block`（保守拒绝） | `allow` |
| `RiskControl.LocalFallbackPerWindow` | Redis 全盲时单实例每动作每窗兜底动作数上限，`<=0` 关闭 | 200 |
| `RiskControl.MaxHitRulesPerDecision` | 单次裁决返回的命中明细上限（裁决与分数仍按全部命中计算） | 20 |
| `RiskControl.CheckLogRetentionDays` | `risk_check_log` 期望保留天数（仅声明，归档由 `services/cron` 执行） | 30 |
| `FeatureStoreRPC` | 可选下游 `featurestore.v1.rpc`；本服务侧尚未装配 client，示例配置保持注释 | 注释 |
| 内嵌 `ServiceConf` | `Mode`、`Log.*`、`Telemetry.*`、`Prometheus.*` 按 go-zero 约定 | — |

## 8. 回滚开关

按影响面从小到大，全部为配置/数据级，不需要回滚代码：

1. **停单条规则**：`risk_rule.state=0`（或 `UpsertRule` 停用），最长 `RuleCacheSeconds` 后全实例生效；
   全域（`action_type=0`）规则变更会失效所有动作的规则缓存。
2. **加白名单豁免**：`UpsertListEntry(list_type=2, ...)` 让目标跳过规则评估；注意白名单
   **不解除**已生效处罚。
3. **解除处罚**：`LiftPunishment`（`lift_operator`/`lift_reason` 留审计痕迹）。
4. **收紧/放宽故障降级**：`RiskControl.OnDbFailureDefault: block` 全量保守拒绝；
   或调整 `HighRiskActions` 精确控制动作集合。
5. **关闭进程内兜底限流**：`LocalFallbackPerWindow: 0`（误伤客户端重试风暴时使用）。
6. **档位收缩**：从 `CounterTiers` 移除长档位会让对应规则变为不可观测（跳过而非误命中），
   属可控降级；恢复档位后计数需重新累积。
7. **表结构回滚**：执行迁移文件头的 `DROP TABLE`（先备份：处罚史与裁决日志是审计证据）。
8. **代码回滚**：本服务不写他人数据，回滚镜像即可；`rc:ck:*` 最多 60 秒内回放旧语义裁决，
   语义不兼容时手动 `DEL rc:ck:* rc:rl:*`（仅本服务前缀）。

## 9. 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储）。
数字为主代理实测导出（`.gotmp/readme-metrics/risk-control.txt`、
`.gotmp/readme-test-aggregate.txt`），`grep -cE '^func Test'`（已排除 `TestMain`）/
`grep -c 't.Run('`，格式 `顶层/子用例`。

### 9.1 `internal/logic` — `74/0`（6 个用例文件 + `fakes_test.go` 替身层）

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `checkaction_logic_test.go` | 20/0 | 裁决主链路「黑名单 → 生效处罚 → 白名单 → 兜底限流 → 规则 → 降级」：依赖故障必须回带 `basis`/`degraded` 的可解释裁决而不是裸错误（`TestCheckActionDegradesOnListFailureWithExplainedDecision`、`TestCheckActionHighRiskActionBlocksOnRuleFailure`）、`hit_rule_ids` 顺序与 `skipped_rule_ids` 的真实性（「读不到」绝不冒充「0 命中」，`TestCheckActionCounterFailureIsUnobservableNotZero`）、规则缓存命中即不回源、降级即跳过 `risk_check_log`、审计写失败不改判、明文 IP/设备号原文不得进日志列/缓存值/响应（`TestCheckActionNeverLeaksRawPII`） |
| `punishment_logic_test.go` | 21/0 | 处罚状态机与幂等两层：`idempotency_key` 命中即在 `FindByKey` 短路且回包是库存行原值、同一 `(mid, scope)` 重叠处罚必须被拒而全域/动作处罚按 `MatchesScope` 精确区分、`ExpireStale` 发生在重叠检查**之前**、故障原样上抛且要说清「哪一步已落库」；`TestApplyPunishmentLandsActiveAndTakesEffectImmediately` 与 `TestLiftPunishmentExpiredRowDisagreesBetweenPaths` 分别钉住缺口 #12（已修）与缺口 #13（现状哨兵） |
| `rule_logic_test.go` | 10/0 | `UpsertRule` 建后自增主键回填 + 缓存失效范围（全域要删 7 个动作键）、「只有影响评估结果的字段才 `+version`」使重试幂等、规则名不可变更、未知 ID 与重名不留残留、守卫先于任何依赖、故障原样传播；`ListRules` 的过滤条件确实进 SQL 参数（`TestListRulesFiltersLandInSQLParameters`）、读侧看到的就是写侧落下的 |
| `list_logic_test.go` | 10/0 | `risk_list` 是「不透明命中即拒」的安全控制：`uniq_target` 覆盖式写入不换 ID 不动 `ctime`、规范化既把同一实体不同写法折回一行又不把两个实体折成一行（超列宽即拒绝而非截断）、operator 必填、已过期条目在运营审计视图里必须继续返回（`TestGetListEntriesKeepsExpiredRowsForAudit`，与裁决读侧 `FindActive` 的过滤刻意不同）、`total=0` 不发第二条查询 |
| `device_logic_test.go` | 6/0 | `uniq_device_hash` 反复写入命中同一行、`first_seen` 取 `LEAST`/`last_seen` 取 `GREATEST`、UPSERT 语句**不含** `related_mid_count`（否则补一次标签就把关联数清零）、风险分 `>=0` 覆盖 / `>100` 收敛 / `<0` 不修改、标签溢出**拒绝而非裁断**（`TestUpsertDeviceProfileLabelsOverflowIsRejectedNotTruncated`）、`GetDeviceProfile` 只回库内真值或 NotFound |
| `decision_explain_test.go` | 7/0 | 入参规范化与决策解释出口：裸 IP 在进 model/policy 之前被挡下、model↔proto 枚举编号一致（`TestEnumNumberingStaysInSyncWithProto`）、指标往返、命中顺序原样出现在响应里、`sanitizeShortString` 的边界 |

### 9.2 其他层

- `internal/policy/engine_test.go` — `12/1`：裁决纯函数 `Evaluate`——名单/处罚优先级、
  `hit_rule_ids` 稳定顺序、分数与明细裁剪、不可观测规则跳过、兜底限流，全部不依赖 Redis/MySQL。
- `internal/policy/decide_test.go` — `8/1`：`fakeStore` 注入「DB 故障 / Redis 故障 / 缓存命中」，
  断 DB 故障时按动作分级的 ALLOW/BLOCK-on-error、缓存回放、审计失败不污染裁决。
- `internal/repository/window_test.go` — `12/2`：滑窗档位与桶数学、key 命名空间隔离、
  脏值容忍、进程内兜底限流器——这些数字决定「规则可观测/不可观测」的边界。
- `model/model_test.go` — `11/4`：阈值评估与裁决强度、处罚/名单时间边界、标签合并、命中序列化，
  固定时间戳不读系统时钟。
- `internal/svc/servicecontext_test.go` — `2/0`：`policyConfig` 是「配置 → 引擎语义」的唯一映射点，
  钉住降级开关映射与高风险动作集合（写错一次就会让线上降级方向相反）。
- `internal/config/config_load_test.go` — `1/1`：示例配置必须能被 `conf.Load` 真实加载
  （含 `CacheRedis` 键名冲突回归）。
- `internal/server/` 与 `rpc/*.pb.go` 是 goctl/protoc 生成壳，不在单测范围内。
- 本服务没有 `internal/consumer` 目录（§10 缺口 3：行为计数完全依赖调用方同步 `ReportAction`，
  事件订阅未实现）。
- `fakes_test.go` 是替身层，`top=0 sub=0` 属正常。

规模合计 **120 顶层 + 9 子用例**（logic 74/0、policy 20/2、repository 12/2、model 11/4、svc 2/0、config 1/1）。

### 9.3 构造器级覆盖

**11/11**：探针取 `internal/logic` 全部 `New*Logic(` 共 11 个，逐个回查 `*_test.go` 引用，`gaps:` 为空，
与 §2 的 11 个 RPC 方法一一对应。`t.Skip` 全仓实测口径中本服务为 0 条。

### 9.4 替身层与断言口径

注入缝的取舍写在 `internal/logic/fakes_test.go` 头注：`ServiceContext.Repository` 是具体类型
`*repository.Repository`，生产构造只吃 `*redis.Redis`，离线必然 panic。因此用例统一走
`repository.NewWithDeps(内存 Redis 原语, 内存 6 个 model)` 组装**真实 Repository**
（真实 `Cache` + 真实 `Counter` + 真实 `localRateGuard` + 真实桶数学），再按 `svc.NewServiceContext`
的口径装上**真实 `policy.Engine`**——Repository 与 Engine 都不 mock，否则
「黑名单→处罚→白名单→兜底限流→规则→降级」整条链就在被测路径之外。

四条替身纪律：每次读返回值拷贝；写入按真实 SQL 口径处理主键与 affected 行数
（`ON DUPLICATE KEY` 的 1/2/0 三态、`INSERT IGNORE` 的静默重复不做美化，
生产 SQL 不写的列替身同样不写）；副作用按**顺序**记录（`callLog`，`<pkg>.<method>:<key>`），
断序列而不是只断次数，非确定性值（时间戳/自增主键/桶号）改用 `wantCount` + 读回断言；
布数据走不记轨迹的静默入口（`seedRule`/`seedPunishment`/`warmJSON`），轨迹从 0 起数。

证明不了什么（替身头注自陈）：替身只复刻 model 层 SQL 的**语义**
（`uniq_name`、`uniq_idempotency_key`、`uniq(list_type,target_type,target_value)`、
`uniq_device_hash`、`uniq_device_mid`、`uniq_request_id`、`LEAST/GREATEST`、`INSERT IGNORE`、
`ORDER BY`、`LIMIT/OFFSET`），**不证明 SQL 文本与列名本身**，那部分归
`deploy/migrations/risk-control/*.sql` 与集成环境；`fakeRedis` 按同语义复刻
（MGET 缺失位返回空串、GET miss 报 `redis.Nil`、SETNXEX 已存在返回 false、INCRBY+EXPIRE 同批），
**不验证真实网络故障形态**；真库唯一键的并发行为（缺口 #15 登记的「同一 `(mid, scope)` 可被并发
插入两条 ACTIVE」在单线程替身里根本无法复现）。

断言口径：拒绝类一律断「守卫先于任何依赖」，即 `callLog` 为空而不是「返回了错误」；
写侧断「库里真实残留的行与列」；`list_logic_test.go` 头注额外如实登记了一处替身与真实 SQL 的差异
（真实 `model/list.go` 在 Upsert 后还会发一条 `FindOne`，替身直接返回库存行副本，
所以调用轨迹只有一条 `list.Upsert:*`）。

### 9.5 覆盖边界

1. 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC server；
   `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内。
2. **只验证本服务自己的裁决链与落库序列**：本服务没有 MQ 消费者（§10 缺口 3），
   所以「`event-collector` 的事件被本服务消费并按 `event_id` 去重」这类闭环**不在覆盖内，也无代码可测**；
   同理不验证任何下游消费方的幂等。
3. 设备风险分不是模型分：`FeatureStoreRPC` 只是占位、client 未装配（§10 缺口 1），
   「特征缺失时的降级」目前只有 `TestCheckActionMissingDeviceProfileIsSkipped` 这一条
   以库内替身形态存在，真实特征源不可达的行为无法断言。
4. **迁移未在目标实例复验**：本 README 没有隔离实例（`127.0.0.1:3399`）复验的登记，
   本节不声称已复验；列宽与唯一键的存在性只由用例引用迁移文件行号做静态对照
   （如 `TestUpsertListEntryStateAndExpireAgainstDDL`、`device_logic_test.go` 头注里的列宽出处），
   不等于真库验证。
5. 全仓 `t.Skip` 实测口径中本服务为 0 条。

### 9.6 验证命令

```bash
go test -p 1 -count=1 ./services/risk-control/...
gofmt -l services/risk-control   # 必须为空
go vet ./services/risk-control/...
```

`-p 1` 必须保留：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只描述用例断言范围，不构成任何门禁结论。

## 10. 已知缺口

1. **feature-store 未落地**：无 `.proto`、无实现，`FeatureStoreRPC` 仅为占位；设备风险分目前是
   运营/登录链路写入的静态分，不是实时模型分。接入需在 `internal/svc` 装配 client 并扩展
   `repository.observe`，同时补不可观测降级用例。
2. **无 HTTP 面**：`api/` 为空目录，运营后台的规则/名单/处罚管理尚未接入 `gateway/admin`
   （总任务「gateway/admin 接入阶段1-2 新服务」未完成）。目前只能经 gRPC 调用。
3. **无 MQ 消费者**：行为计数完全依赖调用方同步 `ReportAction`；`event-collector` → risk-control
   的事件订阅（含 `event_id` 去重、退避重试、死信）未实现，缺少事件侧的自动化频控输入。
4. **审计日志无归档 job**：`CheckLogRetentionDays` 只是声明，`services/cron` 尚无按 `ctime`
   归档 `risk_check_log` 的任务，长期增长会拖慢本库。
5. **`risk_check_log.trace_id` 未回填**：`repository.StoreResult` 构造日志行时没有写入 ctx 的
   trace_id（列已存在），单请求排障目前只能靠 `request_id`。
6. **规则更新无乐观锁**：`UpsertRule` 用 `version` 记录语义变化，但不校验调用方期望版本，
   并发编辑同名规则会互相覆盖（依赖 `uniq_name` 与运营串行操作）。
7. **滑窗是近似值**：窗口按桶边界对齐，计数最多比 `window_seconds` 大 `bucket-1` 秒；
   对秒级敏感的规则需增大 `WindowBuckets`。
8. **无自建监控指标**：`degraded` 比例、各 `basis` 分布、`skipped_rule_ids` 计数目前只进日志，
   未暴露 Prometheus 指标，运营无法在看板上发现「规则长期不可观测」。
9. **处罚状态推进是读时惰性**：`ExpireStale` 只在读取路径触发，无 cron 时终态行仍留在 ACTIVE
   之外的历史集合中，运营报表需按 `state`+`end_at` 双条件过滤。
10. **上游契约的明文 IP 与本服务约束冲突**：`services/user-profile/rpc/userprofile.proto` 的
    `MidReq.real_ip`、`MemberMidReq.remote_ip` 仍是明文字段。接入本服务时必须由网关侧预哈希后
    传 `ip_hash`，本服务不接受裸 IP（`ErrRawIPForbidden`）；该改造属上游契约变更，未在本期范围内。
11. **封禁（block）子域归属尚未收口**：`user-profile` 已按 AGENTS.md §5 把 BlockInfo/BlockBatchInfo
    三个方法留在本服务侧（未移植），但本服务只提供处罚记录与裁决，没有面向端的
    「查询我是否被封禁/封禁到何时」RPC，接入前需补契约（不新增 HTTP 面，走 gRPC）。
12. **处罚下发未设置 `state`（本期已修复）**：`model/punishment.go` 的 INSERT 显式绑定 `state` 列，
    DDL 的 `DEFAULT 1` 不生效，构造行时漏设会让新处罚落成 `state=0`，而 `ListActiveByMid`、
    `ExpireStale` 与 `Effective()` 全部按 `state=1` 判定 ⇒ 运营看到「下发成功」、端上裁决完全
    不生效。现由 `repository.ApplyPunishment` 归一为 ACTIVE，
    用例 `TestApplyPunishmentLandsActiveAndTakesEffectImmediately` 钉住该链路。
13. **同一条到期处罚的两条解除路径终态不一致**：`EndAt` 已过但 `ExpireStale` 尚未推进的行，
    按 `punishment_id` 解除会写入 `LIFTED`（前置判断只在「非 ACTIVE 且已失效」时才幂等返回），
    按 `(mid, scope)` 解除则先把该行推进成 `EXPIRED`、随后读不到 ⇒ `ErrPunishmentNotFound`。
    需统一「解除前先推进」或让按 ID 路径区分到期行；行为由
    `TestLiftPunishmentExpiredRowDisagreesBetweenPaths` 哨兵钉住。
14. **幂等键长度与列宽不一致（本期已修复）**：`logic` 原先把 `idempotency_key` 裁到 128，
    而 `risk_punishment.idempotency_key` 是 `VARCHAR(64)` 且带唯一索引 —— 静默截断会让两次不同的
    处罚共用同一键，后一次被误判成「重试」而丢弃。现在超过列宽直接拒绝。
15. **`risk_punishment` 没有 `(mid, scope)` 维度约束**：表上只有 `uniq_idempotency_key`，
    「同一账号同一范围只有一条生效处罚」是应用层「读后写」检查，两个并发的不同幂等键请求
    可以各插一条 ACTIVE 处罚，裁决解释随后会出现两条冲突处罚。需要部分唯一索引或行锁。
16. **解除说明没有契约出口**：`lift_reason` 落库但 `rpc.Punishment` 只有 `lift_operator`，
    申诉复核要读数据库才能看到解除理由；补字段属于契约变更（需重新生成并同步网关投影）。
17. **设备画像缺失时的注释与实现矛盾**：`repository.observe` 注释写「设备从未出现过时按 0 分处理
    （确实是低风险）」，实测行为是 `profile == nil` 与「读失败」共用同一分支 ⇒ 依赖设备维度的规则
    被记入 `skipped_rule_ids` 且不降级（`degraded=false`），既不是 0 分也不是可观测的低风险。
    新设备因此会静默跳过设备维度规则，需要区分「不可观测」与「读故障」两种 skipped 原因。
18. **`basis=blacklist` 不指明命中条目**：命中说明只存在于引擎的 `Facts`，
    `Result` 与响应里都没有条目 ID/类型，运营排障需要遍历名单。
19. **「超过列宽就裁断」的一整类缺陷已改为拒绝（本期已修复，但对外行为有变）**：
    `sanitizeShortString` 的 `maxLen` 语义是按**字节** `len` 截断，而 MySQL 的 `VARCHAR(n)` 按
    **字符**计，两者混用会带来三类真实后果——多字节中文被切成非法 UTF-8、唯一键上两条只在
    尾部才不同的值被折叠成同一行、审计来源被改写成另一个合法值。因此下列四处统一改成
    「先规范化、超宽即返回哨兵错误」，调用方会看到以前看不到的拒绝错误：
    `internal/logic/upsertrulelogic.go:35,52-55`（`risk_rule.name` VARCHAR(128) + `uniq_name`，
    见 `deploy/migrations/risk-control/000001_create_risk_control_decision_tables.sql:61`）、
    `internal/logic/upsertlistentrylogic.go:34,58-60`（`risk_list.target_value` VARCHAR(64) +
    `uniq_target`，同文件 `:71,79`）、`internal/logic/upsertdeviceprofilelogic.go:43,56-59`
    （`risk_device_profile.source` VARCHAR(32)，`000002_create_risk_device_and_check_log_tables.sql:52`；
    裁断会把 `login-<尾巴>` 变成合法的 `login`，即审计来源可被伪造）、
    以及第 14 条的 `idempotency_key`。钉住用例：`TestUpsertDeviceProfileLabelsOverflowIsRejectedNotTruncated`
    与 `rule_logic_test.go` / `list_logic_test.go` 的守卫用例（均断言「拒绝且零写入」）。
    **未做**：`reason`/`remark` 等非唯一键长文本仍按列宽裁断（折叠无危害），口径差异保留在此。
