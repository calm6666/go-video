# ops-config

内容运营配置服务：发布式配置（版本/回滚/灰度）、专题、推荐位与坑位、客户端开关、
运行时缓存刷新。本服务是 `ops_*` 8 张表的数据所有者（见 [AGENTS.md §5](../../AGENTS.md)），
纯 gRPC 服务，不提供 HTTP（AGENTS.md §3/§4）。

## 职责

- **持有数据**：`ops_config_item`、`ops_config_version`、`ops_rollout_rule`、`ops_topic`、
  `ops_topic_item`、`ops_recommend_slot`、`ops_recommend_slot_item`、`ops_client_switch`
  （库名 `go_video_ops_config`）。
- **不持有**：分区/标签（`catalog_zone`/`catalog_tag` 归 `catalog`，本服务只在
  `ops_topic.zone_ids`/`tag_ids` 存 ID 引用，见 `rpc/opsconfig.proto:16-21`）；
  内容条目（稿件/作品/季/集）只以 `item_type + item_id` 引用（`rpc/opsconfig.proto:22-23`）；
  管理员身份与 RBAC 归 `operation`，本服务只保存 `operator_id` 引用。
- **发布语义**：配置值写进不可变版本行（`ops_config_version`），发布/回滚只换指针并
  `BumpEpoch`，读侧按 epoch 判缓存是否换代。
- **灰度语义**：`ops_rollout_rule` 支持百分比桶（`model/rollout.go:46` `PercentageBucket`）、
  mid 尾号（`:174` `MidSuffixHit`）、白名单（`:189`）、app 版本区间（`:111` `AppVersionInRange`）、
  时间窗（`:316`/`:327`）；`PickRollout`（`:387`）在一次读取里选出命中规则。
- **审计分工**：本服务是自身配置动作的发起者，因此由本服务调用 `audit.AppendAudit`
  写入 `action_domain = ops_config` 的条目（`rpc/opsconfig.proto:35-48`）。

## gRPC API

package `opsconfig.v1`，端口 8111，etcd key `opsconfig.v1.rpc`，20 个方法。
分组轴与 `rpc/opsconfig.proto` 的 7 个契约分段一一对应，不得平铺。

### 1. 运行时读取（带灰度命中与缓存 TTL）

| 方法 | 说明 |
|---|---|
| `ResolveConfig` | 按 `cfg_key + scope + target(mid/platform/app_version)` 解析当前生效值，返回命中规则与 TTL |
| `BatchResolveConfig` | 一次解析多个 key（条数受 `model/limits.go` 约束） |

### 2. 配置项与发布/回滚

| 方法 | 说明 |
|---|---|
| `ListConfigs` | 配置项分页 |
| `PublishConfig` | 发布新版本并切当前指针（写审计 + 版本行） |
| `RollbackConfig` | 回滚到指定历史版本（产生新版本行，不改写历史） |
| `ListConfigVersions` | 版本历史分页 |

### 3. 灰度规则

| 方法 | 说明 |
|---|---|
| `SaveRolloutRule` | 新建/更新规则（形态校验见 `model/rollout.go:225` `ValidateRuleShape`） |
| `ListRolloutRules` | 规则分页 |
| `SetRolloutRuleState` | 状态机迁移（非法目标态拒绝） |

### 4. 专题 / 合集

| 方法 | 说明 |
|---|---|
| `SaveTopic` | 新建/更新专题（`zone_ids`/`tag_ids` 全量覆盖引用 + `expect_version` 乐观锁） |
| `GetTopic` | 专题详情（含条目） |
| `ListTopics` | 专题分页 |
| `SaveTopicItems` | 条目全量替换（`uniq_topic_position` 保证位置唯一） |

### 5. 推荐位与坑位

| 方法 | 说明 |
|---|---|
| `SaveSlot` | 坑位定义（`code`/`capacity`/`platforms`/`state`） |
| `ListSlots` | 坑位分页 |
| `SaveSlotItems` | 坑位条目全量替换（带排期窗口） |
| `ResolveSlot` | 运行时解析：某端某时刻该位置的内容引用 + TTL |

### 6. 客户端开关与版本门槛

| 方法 | 说明 |
|---|---|
| `SaveClientSwitch` | 按 `(switch_key, platform)` 保存开关，可绑定 `cfg_key` |
| `ListClientSwitches` | 开关分页（管理面口径：`pn/ps/total`，不是端上解析接口） |

### 7. 缓存刷新

| 方法 | 说明 |
|---|---|
| `RefreshCache` | 按 `target = config/topic/slot/all` 递增 epoch 并删 Redis 读投影；`all` 必写审计 |

## 数据模型与迁移

| 表 | 唯一约束（迁移脚本内可查） | 用途 |
|---|---|---|
| `ops_config_item` | `uniq_key_scope(cfg_key, scope)` | 配置项当前态与 epoch |
| `ops_config_version` | `uniq_config_version(config_id, version)`、`uniq_request_id(request_id)` | 不可变版本快照 + 写幂等键 + `audit_entry_id` |
| `ops_rollout_rule` | `uniq_config_version_name(config_id, version, name)` | 灰度规则 |
| `ops_topic` | `uniq_slug(slug)` | 专题 |
| `ops_topic_item` | `uniq_topic_position(topic_id, position)` | 专题条目 |
| `ops_recommend_slot` | `uniq_code(code)` | 坑位定义 |
| `ops_recommend_slot_item` | `uniq_slot_position(slot_id, position)` | 坑位条目与排期窗口 |
| `ops_client_switch` | `uniq_key_platform(switch_key, platform)` | 端开关 |

迁移脚本位于 `deploy/migrations/ops-config/`（`000001`~`000004`，每个字段与索引均带中文注释）。
迁移只在隔离实例（`127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）应用与对账；
用户本机 3306 实例从未被本项目的迁移写入。

## 读写链路与降级语义

- 读路径：Redis 只承载**可整域重建的只读投影**（解析结果、专题视图、坑位视图），
  事实源恒在 `ops_*` 表；Redis 不可用即退化直连 MySQL，判定结果不变
  （`etc/opsconfig.v1.yaml:19-24`、`internal/config/config.go:25-33`）。
  钉住用例：`internal/logic/failclosed_test.go` 的
  `TestEveryWriteSucceedsWhenTheCacheProjectionIsGone`。
- 写路径：`request_id` + `operator_id` 双必填（缺任一拒绝，
  `TestEveryWriteRequiresRequestIdAndOperator`），版本行按 `uniq_request_id` 幂等。
- 审计写失败**不回滚**业务写入，但必须留 `audit_entry_id=0` 并打 Error 日志
  （`internal/logic/helpers.go:516`、`:521`），钉住用例
  `TestEveryWriteDegradesVisiblyWhenAuditCannotBeStored`。
- `AuditRPC` 未配置时不构造客户端，写接口照常推进状态机（`internal/svc/servicecontext.go:144-147`）。

## 所有权结论与待评审（`rpc/opsconfig.proto:24-30` 引用本节）

- `operation.op_config` 与 `ops_config_item` **本期并存**，不做数据搬迁：
  前者是「后台自用的一行一键值对 + 乐观锁版本」，没有发布/回滚/灰度/版本历史；
  后者是「带不可变版本快照、灰度规则与缓存失效语义的发布式配置」。
- 约定：**新增的面向端展示与灰度配置写本服务**；`operation` 既有键保持不动，
  等维护者确认后再按「迁移 + RPC」接管。该裁决仍未做出，属本节待办而非缺陷。

## 已知缺口

按影响分组编号；每条给出口位置，改动前先复核行号。

### A. 读侧接线（发布阻塞）

1. **没有任何终端读取路径**。`gateway/app` 全仓零 `OpsConfig`/`ClientSwitch`/
   `ResolveConfig`/`RecommendSlot` 引用（`grep` 全域复核），
   因此运营配置、专题、坑位、端开关**从未到达 Android/iOS/HarmonyOS/桌面端**。
   唯一的 `ops-config` 调用方是 `gateway/admin`（18 个 logic 文件）。
2. **`ListClientSwitches` 是管理面分页，不是端上解析接口**。契约里只有
   `pn/ps/total`（`rpc/opsconfig.proto:565-577`），没有「按平台+版本+mid 解析开关」的方法，
   于是 `ops_client_switch` 与版本门槛在终端侧不可消费；
   `SaveClientSwitch` 自己也承认这一点（`internal/logic/saveclientswitchlogic.go:170-174`：
   「本服务没有面向端上的开关读取接口……也就无键可删」）。
3. **发布出来的配置不影响排序**。`recommend-rank` 的 `OpsConfigReader` 仍是
   `StubOpsConfigReader`，`Resolve` 返回 `model.ErrNotImplemented`
   （`services/recommend-rank/internal/repository/downstream.go:92-93`、`:116`、`:162-166`），
   运营干预在排序里是空转（rank 侧同条缺口已登记在
   `services/recommend-rank/README.md` 的接线表）。

### B. 引用完整性

4. **专题/坑位的内容与分区引用从不校验存在性**。本服务除 `audit` 外没有任何下游客户端
   （`internal/config/config.go:13` 明示「本服务只有**一个下游** —— audit」），
   `SaveTopic`/`SaveTopicItems`/`SaveSlotItems` 只校形态；`gateway/admin` 侧同样原样透传
   `req.ZoneIds`（`gateway/admin/internal/logic/opssavetopiclogic.go:65`），
   于是「引用了不存在的分区/标签/已下架稿件」可以一路写进库并被 `ResolveSlot`/`GetTopic`
   原样回给调用方。契约把这个判定交给了 `video`/`catalog`/`rights`
   （`rpc/opsconfig.proto:22-23`），但**当前没有任何调用方真的去判定**。
5. **坑位条目没有 app 版本区间列**，`target.app_version` 无法参与条目过滤。
   实现刻意不假装过滤，只打 Debug 日志（`internal/logic/resolveslotlogic.go:93-98`）：
   灰度规则有版本区间能力（`model/rollout.go:111`），坑位内容没有，两侧口径不一致。

### C. 审计与补偿

6. **审计补偿任务不存在**。写侧在审计失败时只打 Error 并留 `audit_entry_id=0`，
   注释与契约都写明「由后续补偿任务重投」（`internal/logic/helpers.go:483-485`、
   `internal/svc/servicecontext.go:146`、`rpc/opsconfig.proto:47-48`），
   但 `deploy/migrations/cron/000001_create_cron_task_tables.sql` 没有为本服务登记任何
   `cron_task_definition` 种子，服务内也没有扫描 `audit_entry_id=0` 的重投逻辑。
   结果是**审计缺口永久可见但永不修复**——比静默吞掉好，但不构成闭环。

### D. 运营面口径

7. `RefreshCache(target=all)` 是整域失效的故障兜底，除审计外没有速率限制；
   连续调用会把读投影整体清空并放大回源压力。
8. `SetRolloutRuleState` 拒绝非法目标态（`TestRolloutStateTransitionRejectsIllegalTargets`），
   但规则状态与版本行状态各自推进，没有一致性核对入口——排查时两侧需要分别查。

### E. 覆盖边界

9. 测试全部走进程内替身（`internal/logic/fakes_test.go` + `testsupport_test.go`），
   SQL 文本只由 `model/migration_parity_test.go`（13 个用例）与迁移脚本做静态对账，
   没有真库联跑；因此「索引真的存在」「唯一键真的挡住并发重复」这类事实的证明强度是
   静态一致级别。
10. 缓存路径覆盖以「投影丢失仍能判定」为主，多实例间 epoch 收敛（一台 bump、另一台读旧投影）
    没有跨进程用例。

## 运行

```powershell
# 从仓库根目录生成（框架代码由 goctl 产出，业务 logic/model/迁移是源文件）
./scripts/gen.ps1 -Service ops-config

# 运行（纯 RPC）
go run ./services/ops-config -f services/ops-config/etc/opsconfig.v1.yaml

# 健康检查：gRPC health 探针（grpc_health_probe -addr=127.0.0.1:8111）
```

## 关键约束

- 只暴露 gRPC；对外 HTTP 由 `gateway/admin`（运营面）聚合，终端面见缺口 A。
- 写方法必须带 `request_id`（幂等）与 `operator_id`；权限判定不在本服务，归 `operation` RBAC。
- 不复制可变主资料（分区名/标签名/稿件标题），不建内容条目副本。
- 契约中不存在广告主、出价、排期购买、投放计费、分成等字段（AGENTS.md §1）；
  平台枚举只覆盖四类终端，不支持小程序（AGENTS.md §6）。
- 生产密码/Token/OSS 密钥进 Secret/Vault，不提交；配置示例只写占位。

## 测试覆盖

离线单测（纯 Go，不连 MySQL/Redis/gRPC，AGENTS.md §9）。数字来自导出明细，格式 `顶层/子用例`。
规模合计 **159 个顶层用例 + 50 个子用例**，0 条 skip。

### 1. `internal/logic`（9 个用例文件 + `fakes_test.go`/`testsupport_test.go` 两层脚手架）— `108/43`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `publish_rollback_test.go` | 24/5 | 发布与回滚的判定链：身份与乐观锁入参守卫、值与声明类型不符拒、首次发布追加版本并推进指针、`expect_version` 矩阵、省略类型时继承、带灰度时**按住指针不回写**、规则形态错误指到越界下标、未知平台枚举是拒绝不是忽略、白名单溢出用自己的错误码、**CAS 输了不留幽灵版本行**、版本竞态报成冲突、事务失败什么都不留、`request_id` 幂等、重放拒绝半成品发布；回滚**追加新版本而不改写历史**、同事务里关掉未关闭的规则、回滚到当前版本/无目标/no-op 一律拒、不肯猜测无法解释的历史值类型、审计缺口保持可见而不是伪造、审计带上归因 |
| `resolve_test.go` | 20/3 | 运行时解析：投影出生效版本视图、**读不到是结果而不是错误**、每种值类型都以文本承载、五种灰度形态各自命中、百分比桶严格按 proto 公式、命中序为「优先级→规则 ID」、时间窗外规则忽略、`ignore_rollout` 预览生效版本、灰度指向未发布版本时回落、指针损坏报成数据错误、非法入参先于任何存储读、投影命中且发布后重算、**Redis 投影不可用时结论仍属于 MySQL**、批量与单读一致、批量取最小 TTL 且零写、上限按去重后的 key 判定；`RefreshCache` 先 bump epoch 且只删自己那组键、无法解释的 target 拒；`ListConfigs` 夹取 `ps` 并报真实 total |
| `topicread_test.go` | 14/5 | 专题读侧：定位参数先于存储读、未命中不缓存但已下架专题以 `ttl=0` 命中、只缓存定义且用两个独立键、**失效以真实删除可观测**、条目上限矩阵且不得超过配置上限、只回生效条目、缓存损坏不改变结论、存储错误原样外传；`ListTopics` 排序与 state/窗口/ref/关键词过滤各自可判别、列表与详情对同一配置结论一致 |
| `resolveslot_test.go` | 11/4 | 坑位解析：非法入参先拒、未知 `code` 是未命中且不缓存、配了但为空＝`found` + 坑位 TTL、停用/不可见在**不读条目**的情况下判 miss、容量是读时的硬边界、排期窗口半开且以服务器时钟为准、投影只放定义行、缓存损坏不改变结论、错误原样外传、`app_version` **不能**过滤坑位条目（缺口 B5 的行为哨兵）、建议 TTL 被夹取而投影 TTL 不被夹取 |
| `failclosed_test.go` | 9/17 | 跨全部写方法的三条不变量（以 `t.Run` 逐方法展开）：审计存不下时**可见降级**（`audit_entry_id=0` + Error 日志，不伪造条目）、每个写用自己的 `event_id` 存审计、`request_id` 与 `operator_id` 缺一即拒、`request_id` 列宽 64 字符、缓存投影丢失时每个写仍成功、开关失效跟随其 `cfg_key` 绑定、规则状态迁移拒绝非法目标、`SaveRolloutRule` 拒绝无法锚定的规则、目录类写入以非法形态拒而不触行 |
| `versionread_test.go` | 8/2 | 版本历史读侧：非法入参先拒、key 长度上限来自正则而不是配置旋钮、历史按版本**倒序**、按 `config_id + scope` 隔离、未知 key 是 fail-soft 空页、每条历史字段原样回传、存储错误原样外传、无外部依赖闸门 |
| `rolloutread_test.go` | 8/2 | 规则列表：非法入参先拒、排序为「配置→优先级→规则 ID」、每个过滤位各自可判别、未知 `cfg_key` fail-stop、`count_total` 开关被忽略（行为哨兵：用例名即结论）、存储形态还原成数组、错误原样外传、列表序与 `Resolve` 的候选序一致 |
| `switchread_test.go` | 7/2 | 开关列表（管理面口径）：非法入参先拒、**没有关键词长度守卫**（行为哨兵：用例名即结论）、按 `switch_key`→`platform` 排序、分页/总数/空页、每个 WHERE 段单独可判别、**永不走运行时路径**（缺口 A2）、错误原样外传 |
| `listslots_test.go` | 7/3 | 坑位定义分页：非法入参先拒、排序稳定、过滤可判别、总数与空页、只回定义不展开条目 |
| `fakes_test.go` / `testsupport_test.go` | 0/0 | 替身层与共用脚手架（见 §4），不贡献用例数 |

### 2. 其他层

| 层 | 文件 | 顶层/子 | 内容 |
|---|---|---|---|
| `model` | 3 | 46/6 | `model_rules_test.go`(28/5) 枚举谓词拒 0 与未知值、scope 不含小程序、`item_type=topic` 只允许在坑位、四端平台映射与「未知平台是拒绝不是丢弃」、平台包含与可见性语义、ID 列表往返去重、mid 尾号按十进制末位、app 版本比较按数值不按字典序、百分比桶严格等于文档公式且确定性、`PickRollout` 命中矩阵与桶区间、时间窗 nil 安全、规则形态校验矩阵、**校验先于任何 SQL**、`InsertVersion` 拒绝无法解释的行、slug/code 格式、坑位容量与条目窗口、分页默认值、硬上限自洽；`migration_parity_test.go`(13/0) 迁移文件覆盖每张 model 表、struct 列与列常量 ↔ SQL 逐列一致、唯一键在 SQL 里真的唯一、发布 CAS 所需约束存在、每个文本列声明长度、格式与列宽互相一致、列表列宽容得下最坏情况、枚举列注释写全合法值；`enum_contract_test.go`(5/1) RPC 枚举镜像 model 合法性、编号与名字双向一致、契约里无处出现小程序、`item_type` 是闭集、字符串枚举常量精确且互异 |
| `internal/config` | 1 | 5/1 | `etc` 示例配置可被 go-zero 加载；约束项必须为正；配置值不得越过 `model` 硬上限；配置与契约里都**不存在商业化字段**（两道同一底线的对照断言） |
| `internal/svc` | 0 | — | **无离线单测**：`ServiceContext` 里「`AuditRPC` 未配置就不构造客户端」由 logic 用例间接覆盖（`failclosed_test.go`） |
| `internal/server` | 0 | — | **无离线单测**（goctl 生成壳，见 §5） |
| `internal/repository` / `internal/consumer` / `internal/policy` | — | — | 本服务没有这三层：数据访问直接是 `model` 接口，无 MQ 消费者，判定规则在 `model/rollout.go` 与 `internal/logic/helpers.go` |

### 3. 构造器级覆盖：`20/20`

探针取 `internal/logic` 全部 `NewXxxLogic` 构造器（20 个，与 20 个 RPC 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空——没有只能靠间接断言的方法。

### 4. 替身层与断言口径

可测性设计前提：`model` 全是接口、事务入口只有 `sqlx.SqlConn.TransactCtx`、缓存只有 `CacheKV` 三方法，
所以「乐观锁 CAS / 幂等回放 / 事务回滚不留幽灵行 / 缓存键空间边界」能在不连 MySQL、不起 gRPC、
不依赖 Redis、不 sleep 的情况下被证明。

- `fakes_test.go`：`fakeDB` 一个实现覆盖全部 8 个 model 接口，写路径**复刻真 model 的唯一键与
  RowsAffected 语义**（重复 `(config_id, version)` → `ErrVersionExists`；CAS 条件不满足 → `false`；
  唯一键冲突 → `ErrConfigExists`/`ErrTopicSlugConflict`）；读写字段值一律深拷贝，
  因此 logic 改写返回出去的指针不会跟着改库——「回滚不改写历史」才可证；
  `fakeConn.TransactCtx` 先快照、失败即整库还原，等价于 MySQL 的事务回滚；
  **没实现的组合一律 panic**，避免用例悄悄走到假成功。
- `testsupport_test.go`：种子数据一律走 model 接口而不是直接写 map——只有经过真写路径，
  「库里有这一行」才等价于「生产库里可能出现这一行」；唯一例外 `putRawVersion`
  专门造「库被手工改坏」的行，那正是 fail-closed 分支的入口条件。
- 断言口径：读侧用例统一带「非法入参先于任何存储读」与「存储错误原样外传」两条，
  拒绝类断「零次 recorded op」；缓存类断言区分「投影内容」与「结论来源」，
  因此「Redis 坏了就照抄缓存」这种退化会被判红。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、etcd，也不起 gRPC server/client；`audit` 是替身。
- SQL 文本、列名与索引命中只由 `model/migration_parity_test.go` 与迁移脚本做**静态对账**，
  按本 README「数据模型与迁移」节的声明，迁移只在隔离实例（`127.0.0.1:3399`，
  数据目录 `.gotmp/mysql-data`）应用与对账，**未在目标/共享实例复验**；
  「索引真的存在」「唯一键真的挡住并发重复」的证明强度是静态一致级（「已知缺口」E9）。
- 多实例间的 epoch 收敛（一台 bump、另一台读旧投影）没有跨进程用例（「已知缺口」E10）。
- 替身不等价于真实数据库：CAS 是「下一步会输」的开关复刻，不是行锁/隔离级别行为；
  缓存只复刻键空间，不模拟 TTL 到期。
- `internal/server`、`*.pb.go`、handler/svc 生成壳不在纯单测范围。
- 本节不声明任何一次门禁运行的结论；`gofmt`/`go vet`/`go test` 的结果以下面命令的实际输出为准。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/ops-config/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l services/ops-config                      # 必须为空
go vet ./services/ops-config/...                  # 应无输出
```

契约变更后按 `./scripts/gen.ps1 -Service ops-config` 重新生成，禁止手改 `internal/server` 与 `rpc/pb`。
