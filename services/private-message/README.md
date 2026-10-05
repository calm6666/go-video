# private-message

用户私信（单聊）领域服务：会话、消息、已读游标、撤回、举报与反骚扰偏好的数据所有者。
只提供 gRPC（无 `.api`）；面向终端的 HTTP/WebSocket 入口与响应信封在 `gateway/app`（AGENTS.md §3/§4/§6）。

- **拥有数据**（库 `go_video_private_message`）：
  `pm_conversation`（会话主体 + seq 锚点）、`pm_conversation_member`（成员游标与列表投影）、
  `pm_message`（密文正文 + 状态机）、`pm_user_setting`（反骚扰偏好）、
  `pm_report`（举报事实）、`pm_withdraw_log`（撤回审计流水，append-only）。
- **提供能力**：定位/创建单聊、发送、会话列表、消息分页、已读前移、未读汇总、撤回、
  隐藏会话、偏好读写、举报与运营处置、审核结论回写、留存到期清理。
- **依赖**：MySQL、Redis（未读与门禁短缓存，真值始终在 MySQL）、
  `social-graph`（黑名单/关注关系真值）、`risk-control`（名单与频控裁决）、
  `moderation-orchestrator`（送审与结论）、`asset`（`media_ref` 只存其主键）。
- **约束**：
  - 与 `inbox`（站内信/系统消息）**分库分表，绝不共表**：私信是用户之间的通信，
    系统消息是平台对用户的投递，留存、加密与审计口径不同。
  - 隐私级别 **P4**：正文以 AES-GCM 信封加密落库，明文永不入库、不入日志、不入事件；
    列表与预览只用脱敏 `preview` 列。
  - 黑名单/关注/风控真值**不复制进本库**，只缓存判定结果（AGENTS.md §5）。
  - 不做会员、订单、支付、投币、广告等商业化能力；也不做群聊/频道（本期只有单聊）。

## 本期落地范围

契约（`rpc/privatemessage.proto`）+ 数据模型（`model/` 6 张表）+ 迁移 SQL + 配置装配 +
**15 个 logic 方法的业务实现**均已完成：门禁判定、幂等、事务边界、加密与脱敏摘要、
审核结论回写与留存清理都按方法体里的实现注释落地，无 `ErrNotImplemented` 残留。

logic 包内的手写扩展（非 goctl 产物，与 `live-room` 同风格）：

| 文件 | 职责 |
|---|---|
| `helpers.go` | 入参校验、页大小与游标编解码、脱敏摘要与占位文案、缓存键与计数器、**参与者授权（`requireMembership`/`requireMessageAccess`）**、撤回的共享事务步骤 |
| `cipher.go` | AES-256-GCM 信封加解密与 HMAC-SHA256+pepper 内容指纹；密钥缺失即 `ErrCipherKeyMissing` |
| `gate.go` | social-graph 黑名单/关注、接收范围门槛、risk-control 裁决、moderation 送审适配（全部 fail-closed） |
| `conv.go` | model → rpc 投影（`ContentCipher` 永不出境，不可见行同时隐去 `media_ref`） |

本轮为发送与处置路径新增的 model 能力（手写、可编辑，未碰 `.proto`/生成码）：
`Conversation.FindByPairKey`、`Conversation.FindOrCreateInTx`、`Message.HasAnyFromSender`、
`Message.BindAuditEvent`、`Report.MarkHandledInTx`，以及哨兵错误 `ErrInvalidRetentionWindow`
（现共 **34 个**）。

迁移 SQL 已在**隔离实例**（`127.0.0.1:3399`，库 `go_video_private_message`）执行并核对：
该库现有 6 张业务表 + `schema_migrations`，与 `deploy/migrations/private-message/000001~000002`
的建表集合逐一对应。model 与 DDL 的一致性另由逐列比对测试兜住。
**真实/共享实例仍未执行**：本机 3306 是维护者真实库，禁止写入，上线前须由运维按
`docs/commands.md` 的迁移命令在目标环境执行。

## 方法与契约

| 方法 | 类型 | 幂等键 | 说明 |
|---|---|---|---|
| `GetOrCreateConversation` | 写 | `pair_key = min(mid):max(mid)` | 定位或建档，`uniq_pair_key` 保证同一对用户只有一行会话 |
| `SendMessage` | 写 | `(sender_mid, client_msg_id)` | 发送；重试回放首次结果（`replayed=true`），门禁顺序见下 |
| `ListConversations` | 读 | — | `(last_msg_time, id)` 双列游标倒序；查询侧统一过滤 |
| `ListMessages` | 读 | — | `conversation_id + seq < cursor` 倒序，走 `uniq_conv_seq`，禁止 offset 全扫 |
| `MarkRead` | 写 | 游标只前进（`read_seq < ?` 条件更新） | 前移已读游标并清零未读；回退请求回 `changed=false` |
| `GetUnreadSummary` | 读 | — | 未读汇总（投影，可重算），`force` 忽略缓存回源 |
| `WithdrawMessage` | 写 | `msg_id` + 状态 CAS | 只改状态位 + 写 `pm_withdraw_log`，不物理删行 |
| `HideConversation` | 写 | 单行状态位写入 | 本方隐藏/恢复，不影响对方、不删消息 |
| `UpdateUserSetting` | 写 | `mid` 主键 upsert | 反骚扰偏好；`optional` 字段区分「未传」与「传 false」 |
| `GetUserSetting` | 读 | — | 缺行按 `DefaultUserSetting` 回缺省（`mtime=0` 表示未设置过） |
| `ReportMessage` | 写 | `(msg_id, reporter_mid)` | 只写举报事实并向 moderation 送审 |
| `ListReports` | 读 | — | 运营分页（`report_id` 倒序游标），不返回正文 |
| `HandleReport` | 写 | `handle_idempotency_key` | 处置；连带撤回与处置**同事务** |
| `ApplyModerationVerdict` | 写 | `event_id`（落在 `audit_event_id`） | **唯一**写审核结论入口，重复投递回 `applied=false` |
| `PurgeExpiredMessages` | 写 | `content_purged=0` 条件更新 | 到期清空密文，保留行/状态/摘要/审计 |

### 发送门禁顺序（契约的一部分，不允许跳过或调换）

```
参数与幂等键校验 → client_msg_id 回放探测 → 本地写侧限流（WriteLimiter + MaxPerUserPerMinute
+ 新会话日配额）→ 会话定位（成员行证明，接收方恒由会话主体决定）
→ social-graph 黑名单 → 接收方 allow_from / reject_stranger 门槛 → risk-control CheckAction
→ 会话 state=FROZEN 判定（事务内复核）→ AES-GCM 加密 + content_hash + 脱敏 preview
→ 事务{ FindOrCreateInTx + Members.Ensure → AllocateSeq → Messages.Insert
        → Members.ApplyIncoming → Conversations.TouchLastMessage }
→ 提交后送 moderation 机审（PENDING_REVIEW，送审失败保持待审并回 audit_task_id=0）
```

- **参与者授权**：`ListMessages`/`MarkRead`/`HideConversation`/`WithdrawMessage`(SENDER、RECEIVER)/
  `ReportMessage` 都以 `pm_conversation_member(conversation_id, mid)` 命中为前置条件，
  「不是成员」与「会话不存在」同一口径（`ErrNotConversationMember`），不提供读-as-user 的运营后门；
  `SendMessage` 的两条会话定位路径同样要成员行证明：只带 `peer_mid` 时，若该 pair 已有会话，
  发送方必须是其成员（否则等于借 `peer_mid` 往别人的会话里灌消息）；`mid == peer_mid`
  也按同一口径回 `ErrNotConversationMember`——「能不能给自己建会话」是 `GetOrCreateConversation`
  的建档语义（`ErrSelfConversation`），不该用来回答「有没有权限发这条消息」；
- 三条链路（发送、会话列表、消息分页）共用同一套黑名单过滤，**不只拦发送侧**；
- **运营/系统侧主体**：`ListReports`/`HandleReport` 的处理人必须是真实运营账号（`checkOperator`）；
  `ApplyModerationVerdict`/`PurgeExpiredMessages` 额外接受 proto 规定的「无自然人主体」取值 `operator=0`
  （`privatemessage.proto:378`「0 表示机审」、`:398`「cron 传 0」），其余非正值一律 `ErrOperatorRequired`。
  主体/参数不成立的调用在**取数据之前**就被拒（`WithdrawMessage` 的 `source=MODERATION` 缺
  `audit_task_id` 同属此列）：被判权限的调用不允许先把消息行读进内存再决定要不要拒绝；
- 下游未配置或不可用一律返回 `model.ErrSocialGraphNotConfigured` /
  `ErrRiskControlNotConfigured` / `ErrModerationNotConfigured`，
  **绝不把「没接上」当成「已通过」**（fail-closed，AGENTS.md §8）；
- 关系数据取不到时用 `UserSetting.AcceptsUnknownSender()` 做保守判定；
  Redis 计数/限流取不到时按「未通过」拒（限额算不出来就等于没证明没超限）；
- 密钥缺失返回 `ErrCipherKeyMissing`，禁止退化成明文入库；解密失败与无权限对外同口径。

### 消息状态机

```
NORMAL ⇄ PENDING_REVIEW（送审/回结论）
   │            │
   └── WITHDRAWN（本人限时 / 接收方 / 审核 / 运营，写 pm_withdraw_log）
   └── REJECTED （VERDICT_REJECT）
   └── DELETED  （运营/司法处置，保留行与处置记录）
```

`model.MessageModel.MarkState` 用「当前态 ∈ from 集合」的 CAS 实现，
终态（WITHDRAWN/REJECTED/DELETED）不再有出边；重复投递与非法迁移都以 `applied=false` 表达，不抛错。

### 已读为什么是游标

逐条已读回执的行数量级是「会话成员数 × 消息数」（百万消息即亿级行），
游标方案固定 2 行/会话，未读数由 `unread_count` 投影列承担、
可用 `CountVisibleAfterSeq` + `RebuildProjection` 完全重算；
代价是无法回答「某人读过哪一条」，而产品只需要「读到哪」。

### 正文加密与留存

| 列 | 语义 |
|---|---|
| `content_cipher` | AES-256-GCM 密文（`nonce‖ciphertext`），明文永不入库 |
| `key_version` | 数据密钥版本；轮换只新增版本、不改历史行，旧密钥须留在密钥环里可解密 |
| `content_hash` | 明文 HMAC-SHA256 + `Cipher.HashPepper`，仅供风控查重，不可反推原文 |
| `preview` | 脱敏摘要，事件与日志只允许携带本列 |
| `content_purged` | 到期由 `PurgeExpiredMessages` 置 1 并清空密文/媒体引用，**保留行与审计** |

`Cipher.DataKeyBase64` / `HashPepper` 只从 Secret/Vault 注入，示例配置必须留空
（`internal/config/config_load_test.go` 断言不留密钥、`DataSource` 必须指向 `go_video_private_message`）。

## 表与迁移文件

| 表 | 迁移文件 | model |
|---|---|---|
| `pm_conversation` / `pm_conversation_member` / `pm_message` | `deploy/migrations/private-message/000001_create_private_message_conversation_tables.sql` | `model/pm_conversation.go`、`model/pm_conversation_member.go`、`model/pm_message.go` |
| `pm_user_setting` / `pm_report` / `pm_withdraw_log` | `deploy/migrations/private-message/000002_create_private_message_moderation_tables.sql` | `model/pm_user_setting.go`、`model/pm_report.go`、`model/pm_withdraw_log.go` |

关键索引（与真实查询路径一一对应）：
`uniq_pair_key`、`uniq_conv_mid`、`idx_mid_peer`、`idx_mid_list(mid,hide_state,last_msg_time,id)`、
`uniq_conv_seq(conversation_id,seq)`、`uniq_sender_client_msg(sender_mid,client_msg_id)`、
`idx_purge_scan(content_purged,ctime)`、`uniq_msg_reporter`、`uniq_handle_key`、`idx_state_report_id`、
`idx_msg`、`idx_operator_ctime`。

## 配置

```yaml
Name: privatemessage.v1.rpc
ListenOn: 0.0.0.0:8150          # 批次 D1 分配；8080 是 gateway/app 的 HTTP 端口
Etcd: {Hosts: [127.0.0.1:2379], Key: privatemessage.v1.rpc}
CacheRedis: {Host: 127.0.0.1:6379, Type: node}   # 不能叫 Redis：RpcServerConf 内嵌同名 RedisKeyConf
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_private_message?charset=utf8mb4&parseTime=true&loc=Local
SocialGraphRPC/RiskControlRPC/ModerationRPC: 走 etcd 发现（未配置时相关门禁 fail-closed 报错）
PrivateMessage: {PageSize: 20, MaxPageSize: 50, MaxTextLength: 2000, PreviewRunes: 30,
                 DefaultAllowFrom: 2, RejectStrangerByDefault: true, KeywordFilterEnabled: true,
                 MachineReviewEnabled: true, MaxPerUserPerMinute: 20,
                 MaxStrangerConversationsPerDay: 10, WithdrawWindowSeconds: 120,
                 MessageRetentionDays: 180, PurgeBatchSize: 500, PostQps: 400, PostBurst: 100}
Cipher: {KeyVersion: 1, DataKeyBase64: "", HashPepper: ""}   # 密钥留空，真实值走 Secret/Vault
```

`Config.Validate()` 在启动期把「能加载但语义危险」的组合直接 `Severe` 终止启动：
`MessageRetentionDays<=0`（正文永不清理）、`DefaultAllowFrom` 越界、
`WithdrawWindowSeconds<=0`（自助撤回永远被拒）、`MaxPageSize<PageSize`、`KeyVersion<=0`。

## 运行与测试

```powershell
go run ./services/private-message -f services/private-message/etc/privatemessage.v1.yaml
powershell -File scripts/gen.ps1 -Service private-message   # 契约变更后重新生成；禁止手改 rpc/*.pb.go、internal/server
```

离线单测（不连 MySQL/Redis/etcd）的覆盖面——逐文件用例清单、替身口径、构造器覆盖率与边界——
见下一节「测试覆盖」。

## 测试覆盖

本节只陈述**覆盖面**（哪个文件钉了哪条口径、哪些层根本没有用例），不复述任何门禁执行结果。
缺口与待评审口径的权威登记在下面「已知缺口 / 待评审」一节，含「举报台账（ListReports）本轮钉住的现状」子节。

### 1. `internal/logic`：15 个 RPC 方法

8 个 `*_test.go`，其中 5 个含 `Test*`，合计 **65 个顶层用例 / 5 个 `t.Run` 子用例 / 0 个 `t.Skip`**。
下游三个依赖（social-graph / risk-control / moderation-orchestrator）是 bufconn 上起的**真 gRPC server**、
logic 侧走**真 client**，所以「未配置 → `Err*NotConfigured`」「调用失败 → 保守拒收」两条分支也在被测路径上。

| 文件 | 顶层/子 | 主覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `authorization_test.go` | 18/5 | 全域授权与出口形态（`SendMessage`/`ListMessages`/`MarkRead`/`WithdrawMessage`/`HideConversation`/`ReportMessage`/`HandleReport`/`ApplyModerationVerdict`/`PurgeExpiredMessages` 的门禁侧） | 本服务最贵的那条不变量：**参与者授权只有 `requireMembership` 一个来源**，未过授权的调用零副作用（不写行、不开事务、不读消息行，因此也不可能解密任何正文）。`TestRejectedCallsDidAskTheMemberTable` 反过来钉「被拒前确实至少查了一次成员表」，否则「越权被拒」可能只是整条链路跑不通；`TestMemberCanReadOwnConversationInPlaintext` 作正向对照。「没解密」不靠读代码相信：把库里的密文换成随机字节，任何解密尝试都会以 `ErrDecryptFailed` 暴露，被拒时错误里只有 `ErrNotConversationMember`。另钉：`mid == peer_mid` 按非成员拒（建档语义属 `GetOrCreateConversation`）、只带 `peer_mid` 时若 pair 已有会话则必须是其成员、别的会话的成员身份不构成访问、有成员行但缺会话主体不算成员、`ErrNotConversationMember` 只带 ID 不带他人信息、`MessageNotFound` 不泄露会话归属、撤回快照不替代成员表、`PENDING_REVIEW` 对端不可见、`WITHDRAWN` 行不漏原文与 `media_ref`、`content_purged` 行保留元数据但丢正文、`HideConversation` 只动调用者自己那行、被拉黑读到空页且 social-graph 故障时也是空页（保守失败而不是变可见）、运营/系统侧主体门禁（`checkOperator` 与 `operator=0` 的两种取值）在取数据之前完成。**末条是源码结构级门禁**：`TestLogicEntryPointsAuthorizationEnumeration` 用 `go/ast` 解析整个 logic 包的调用可达性，断言「没有一条路径能绕过成员证明触达会话/消息数据、明文出口只有一个」，并按 `logicContracts` 台账逐入口登记授权域与允许的数据接口——新增第 16 个入口会先在这里失败 |
| `conversation_test.go` | 15/0 | `GetOrCreateConversation`/`ListConversations` | 建档侧三件事：幂等来自 `uniq_pair_key`（第二行=数据分裂）、会话行与两行成员投影**同事务**（成员行是越权判定的唯一依据，崩溃窗口要能被下一次调用自愈）、建档**不判关系也不扣配额**（`TestGetOrCreateConversationJudgesNoRelationship` 断言黑名单查询次数为 0，门禁在 `SendMessage`）；外加守卫先于任何写、错误上抛并回滚、自报 pair 会给别人建行（现状哨兵）。列表侧三件事：只扫登录者自己的成员行（`idx_mid_list`）、过滤在查询层（双向黑名单、隐藏会话、脏投影行都不外漏，且「下游未配置」=整页拒 vs「下游故障」=丢该行）、摘要只来自成员投影列——`TestListConversationsNeverReadsMessageTable` 把「整条读路径一条消息行都不取」钉成隐私最强证明；`(last_msg_time, id)` 双列游标、页大小与游标守卫、`TestListConversationsSelfReportedMidLeaksOthersPreviews`（现状哨兵）、未读总数是消息条数而不是会话数 |
| `reportlist_test.go` | 15/0 | `ListReports` | 运营面拿的是「谁举报了谁」的台账、不是「他们说了什么」：`ReportInfo` 字段集合与逐字段取值钉死，且整条链路一行消息都不会被物化进内存（用 `fixtures_test.go` 的 `loadedMessageRows` 记账来证，比「读代码说没读」强）；守卫次序 operator → state → ps → cursor 四层全先于查库，被拒时一次数据接口都不许碰；`operator_mid` 只是主体检查、**不是切分范围**（台账全站一份，这条越权口径登记在下节）；页大小上下界与配置联动（含 `MaxPageSize<=0` 等于不设上限、`ps+1` 溢出成 `MinInt32` 两处现状哨兵）；`report_id` 倒序位点「不漏、不重、不多给」，`ps+1` 探针是否真按 +1 下传只能从 fake 记的实参看（`listByCursorArg`）；`has_more` 跟随探针而非 total；游标编不出来时静默把「还有下一页」说成「到底」（现状哨兵）；state 与 target 过滤是 AND；举报描述只在读时脱敏（库里仍是原文）；`TestListReportsDoesNotUseReadSideBlacklistGate` 钉「本方法设计上不接 self 域读侧门禁」并配一条 self 域对照，避免被后人误加 |
| `setting_test.go` | 10/0 | `GetUserSetting`/`UpdateUserSetting` | 每条断言都落到**库里的行**而不只是返回值（「返回值对了但库里写错」是本域最坏的一类通过）：读路径不「顺手补一行缺省」（否则 `pm_user_setting` 会塞满从未设置过的用户，`mtime=0` 这条「未设置 vs 显式默认」的区分就永久丢失）；脏 `allow_from` 读时归一但**不持久化**；站点默认值本身非法时的兜底；作用域锁死请求 mid；未传字段保持基线（proto3 optional 布尔的 nil 与 false 分开，`onb()` 助手专为此存在）；同一份请求重放只一行（幂等 upsert）；越界 `allow_from` 直接拒、不写；站点级 `KeywordFilterEnabled=false` 时不允许被单个用户写回 1；复核失败要上抛；守卫与依赖错误传播 |
| `unread_test.go` | 7/0 | `GetUnreadSummary` | 这是最容易「测了等于没测」的读接口（只断言 `UnreadTotal >= 0` 会永远通过），故每条都落到具体数字并与「从成员投影行重算出来的数」对齐：只聚合请求 mid 自己的投影、`hide_state` 口径恒为「不含隐藏会话」（与 `ListConversations` 默认过滤一致，否则端上角标和列表对不上）、与 `MarkRead`/新消息落库严格一致（读到即 0、新到即 +1，一条不多不少）、缓存未配置与「配了但挂了」**都必须回源 MySQL 真值**（把「拿不到缓存」当成「没有未读」就是把角标清零的假成功）、入参守卫先于触库、依赖错误原样上抛；`TestUnreadCacheKeyIsNamespaced` 把键格式与前缀按纯函数钉住；`TestGetUnreadSummarySelfReportedMidReadsOthersBadge` 是现状哨兵（自报 mid 能读到别人的角标） |

**用例覆盖的形状（如实说明，不是缺口清单）**：`SendMessage`、`ListMessages`、`MarkRead`、
`ReportMessage`、`WithdrawMessage`、`HideConversation`、`ApplyModerationVerdict` 通过
`fixtures_test.go` 的快捷调用（`send`/`sendText`/`listMessages`/`markRead`/`report`/`withdraw`/
`hideConversation`/`verdict`，每个都直走 logic 入口，`send` 还强制要求自带 `client_msg_id`）被真实执行，
授权与出口形态在 `authorization_test.go` 里逐条钉，
但它们各自的**完整业务序列**（发送侧的限流/日配额/机审送审/五步同事务、`MarkRead` 的游标只前进、
`ApplyModerationVerdict` 的 `event_id` 重投回 `applied=false`、`HandleReport` 的处置与撤回同事务）
没有独立的「逐方法行为文件」；`PurgeExpiredMessages` 与 `HandleReport` 目前出现的是
`TestOperatorAndSystemSubjectGates` 里的主体门禁断言。这类形状差异请连同下节一起读，
本节不新判缺口。

### 2. 其它层

| 层 | 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- | --- |
| `model/` | `pure_test.go` | 11/11 | 纯函数与守卫契约：`PairKey` 的交换不变量与拼接歧义、`pair_key` 列宽余量、枚举状态编号钉死、三个 `Valid*` 拒绝 `UNSPECIFIED`、`DefaultUserSetting` 兜底、`AcceptsUnknownSender` fail-closed、成员投影的未读下限、`HasPlainContent`、34 个哨兵错误文案带 `private-message:` 前缀且两两不同（供 `gateway/app` 用 `errors.Is` 映射响应 `code`）、**非法入参在发起任何 SQL 之前**就返回哨兵错误（用 `nil` 连接构造 model，漏校验即 panic） |
| `internal/config/` | `config_load_test.go` | 3/1 | 用**真实** `conf.Load` 逐个加载 `etc/*.yaml` 并跑 `Validate()`（本仓库出现过 Config 自带 `Redis` 字段与 `zrpc.RpcServerConf` 内嵌同名 `RedisKeyConf`，代码可编译但启动即报 `conflict key redis`；「能加载但语义危险」的组合在启动期 `Severe` 终止）；`DefaultAllowFrom` 三个合法档位；`Cipher` 的 `DataKeyBase64`/`HashPepper` **不得入库**（真实值走 Secret/Vault） |
| `internal/svc` | — | **无离线单测** | 只做装配与 fail-closed 校验，`Validate()` 的语义由 config 用例覆盖 |
| `internal/server` | — | **无离线单测** | goctl 生成的 gRPC handler 外壳，逐方法转调 logic（见第 5 组） |

本服务**没有 `internal/repository` 层**（`svc.ServiceContext` 直接注入 6 个 model，见下节），
也**没有 `internal/consumer`**（审核结论走 `ApplyModerationVerdict` 同步 RPC）、没有 `internal/policy`。

### 3. 构造器级覆盖：**15/15**

`internal/logic` 下 15 个 logic 构造器与用例逐个对齐，探针的缺口清单为空——每个入口的构造器都被
实例化并至少跑过一条真实断言，不存在「建了对象但什么都没断言」的空壳用例。

### 4. 替身层与断言口径

三个 0 用例的文件是这套覆盖的地基，读用例前先读它们：

- `fakes_test.go`（内存库 + 事务探针 + 日志探针）：六个 fake 各自**复刻真 model 的 SQL 语义**
  ——唯一键回放、CAS 的 `WHERE`、`GREATEST` 下界、`RowsAffected` 的「实际改变行数」口径——
  而不是「一律返回成功」（替身比被测宽松 = 测试通过但生产出错）。表用**值类型 map** 存行、
  读写各拷一份，所以「回滚后行没被改」才是可证的；没有实现的路径一律 `panic`，避免用例悄悄走到假成功。
  `requireSameRows` 用来断言一次调用没留下任何行级痕迹。
- `fixtures_test.go`（种子 + 快捷调用 + 零副作用探针）：种子一律**走 model 写接口**而不是直接塞 map，
  于是「库里有这一行」等价于「生产库里可能出现这一行」，非法种子先被挡掉；
  例外是 `rawMessage`，专门造只有 DBA 手工改数据才会出现的现场（已清理正文、已终态、缺会话主体的成员行）。
  mid 全是假值（`alice=101`/`bob=202`/`mallory=909`），正文带可搜索哨兵 `secretBody`，
  隐私用例靠它证明「日志/错误/下游请求里没有原文」。
- `downstream_test.go`（下游真 gRPC）：bufconn 上起三个真 server。理由写在文件头——`gate.go` 拿到的
  依赖类型是 `zrpc.Client`（只暴露 `Conn()`），用假接口就根本到不了「未配置」与「失败保守拒收」两条分支。
  替身默认**不放行任何关系**（blocked/following 是显式白名单表），风控默认 `ALLOW`、机审返回真实
  `task_id`，需要失败时由 `failRisk`/`failSubmit`/`denyDecision` 显式注入。

断言口径：调用轨迹用 `callMark` **窗口差分**（只比对本次调用产生的轨迹切片），日志用
`logx.AddWriter` 差分捕获，因此**用例不许 `t.Parallel`**（共享的轨迹与日志写入会串窗）。
错误一律 `errors.Is` 到注入的哨兵。

**这套替身证明不了**：SQL 文本与列名本身、MySQL 的类型/越界/唯一键真实冲突集合、
Redis 的过期与内存语义，以及**缓存命中分支**——`svc.ServiceContext.Cache` 是具体类型
`*redis.Redis`，进程内无法替换（引入 miniredis 属新增第三方依赖，本轮禁止）。多数用例把
`Cache` 置 nil 走 helper 短路（这本身就是那条 fail-closed 分支），需要区分「没配缓存」与
「配了但挂了」时用 `deadCache()`（指向 `127.0.0.1:1`）配 `shortCtx()` 截止点。
所以这里能证的是「缓存不可用不改变结果」，**不是**「缓存命中时结果也正确」；
键格式与前缀改由 `TestUnreadCacheKeyIsNamespaced` 按纯函数钉住。

### 5. 覆盖边界（如实声明）

- **不连真实依赖**：无 MySQL、无 Redis、无 etcd、无 MQ、无对象存储；下游是进程内 bufconn server。
- **迁移 SQL 与真实库的列级对账**：本 README 的「数据模型与迁移」一节已声明在隔离实例
  `127.0.0.1:3399`（库 `go_video_private_message`）执行并核对过 6 张业务表 + `schema_migrations`，
  **真实/共享实例仍未执行**。需要一并核对口径的是：离线单测里**没有**读迁移 SQL 文本做逐列对账的
  门禁（`model/` 只有 `pure_test.go`，它钉的是 Go 侧常量与列宽余量，不打开 `deploy/migrations/`），
  所以「列名/宽度与 DDL 一致」这件事仍只在目标实例上才可核对。
- **`model/*.go` 的 SQL 无单测**：model 层只有纯函数与守卫用例；`TransactCtx` 内的 SQL 语义由
  `fakes_test.go` 的复刻承担（见第 4 组的「证明不了」）。
- **goctl 生成的外壳不在范围内**：`rpc/*.pb.go`、`internal/server`、`internal/svc` 的生成代码
  只做转发与序列化；`scripts/gen.ps1` 重新生成后由 `config_load_test.go` 与 logic 用例间接兜住配置面，
  `.proto` 才是契约真源。
- 本节**未复核**缺口清单：缺口与待评审口径的权威登记在下节「已知缺口 / 待评审」。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/private-message/...   # -p 1：Windows 下并行测试包会撞页面文件上限（errno=1455）
gofmt -l services/private-message                       # 期望无输出
go vet ./services/private-message/...
```

上述命令由评审者自行执行；本节不声称它们当前的结果。

## 已知缺口 / 待评审

- **风控/审核契约缺私信枚举**：`risk-control.GuardedAction` 与
  `moderation.ContentType` 本期都没有「私信」取值，实现里分别映射成
  `ACTION_COMMENT` 与 `CONTENT_TYPE_COMMENT` + `business=private_message`（集中在 `gate.go` 两处常量）。
  动作口径错位会让风控规则命中统计混入评论数据，需两个契约各加枚举后回改。
- **读路径不调 risk-control**：会话列表/消息分页只按 social-graph 黑名单 + 本域
  `conversation.state=FROZEN` 过滤（冻结语义按契约「禁止新发送、历史仍可读」）。
  `GuardedAction` 没有「读自己的信箱」这一动作，硬套 `ACTION_COMMENT` 反而会误伤读取。
- **`RECEIVER` 撤回是全局隐藏**：DDL 没有「逐条 × 逐成员」的可见性表，
  `MarkState` 只能把整条置 `WITHDRAWN`（双方都看到占位文案）。用户侧的单边隐藏目前由
  `hide_state`（整会话）承担；要做「只删我这一条」需要新表 + 契约评审。
- **关键词过滤没有词典表**：`keyword_filter` 偏好与 `KeywordFilterEnabled` 开关目前落到
  「摘要去引流形态（链接/长数字串）+ 机审送审」，命中词判定完全交给 moderation。
  真正的敏感词库属词典服务，接入前不能声称「已过滤关键词」。
- **密文密钥轮换策略未定**：`key_version` 只写入版本，解密需要「版本→密钥」的密钥环；
  当前 `CipherConf` 只有单个 `DataKeyBase64`，无法同时解密历史版本，需评审后加
  `KeyRing`（或按版本分列的 Secret 命名约定）。轮换前落库的消息在换密钥后会解不开（`ErrDecryptFailed`）。
- **`content_purged` 后 `preview` 仍保留**：清理只丢正文，`pm_message.preview` 与成员投影
  `last_preview` 都还在（读取路径按占位文案覆盖 `content`，但列表摘要仍是原文摘要）。
  若合规要求摘要也脱敏，需要额外一轮策略（属 DDL/流程变更）。
- **未读汇总是投影**：`MarkRead`/`ApplyIncoming`/`DecrementUnreadIfUnread` 只维护计数，
  没有定时对账任务把 `unread_count` 与 `CountVisibleAfterSeq` 比对；投影漂移需靠
  `RebuildProjection`（运营/cron 入口）修复，而该入口本期没有接线。
- **`PurgeExpiredMessages` 无调度方**：cron 侧尚未登记该任务，正文到期不会自动清理。
- **没有 MQ consumer**：审核结论走 `ApplyModerationVerdict` 同步 RPC。
  若 `moderation.result.v1` 需要本服务自建消费者（`internal/consumer`），
  要先定：谁来保证 `event_id` 去重（本表 `audit_event_id` 还是 `inbox` 那套消费台账），
  以及重投的退避与死信策略。
- **没有 `internal/repository` 层**：`svc.ServiceContext` 直接注入 6 个 model，
  与 `live-room` 同风格；跨表事务（发送/撤回/处置）的编排目前在 logic 里，
  若后续编排重复再抽 repository，不提前造层。
- **运营面 HTTP 入口已接，但权限点只覆盖写侧**：`gateway/admin` 已挂
  `/admin/private-message/report/list`（`gateway/admin/internal/handler/routes.go:1731-1741`）、
  `/report/handle`（同文件 :1743-1752）、`/retention/purge`（:1753-1758）；
  `ApplyModerationVerdict` 按设计不开 HTTP（moderation 直连 RPC，见
  `gateway/admin/api/admin.api:7905`）。三条里只有后两条挂了 `AdminPermission`，
  台账读取的越权口径见下节「举报台账（ListReports）本轮钉住的现状」。
- **`idx_operator_ctime` 与 `ListByOperator(ORDER BY log_id DESC)` 不完全对齐**：
  当前会走 filesort，数据量上来后需要 `(operator_mid, log_id)` 索引（新号迁移补，不改历史文件）。

### 举报台账（ListReports）本轮钉住的现状

本轮为 `internal/logic/listreportslogic.go` 补齐单测（`reportlist_test.go`，15 个用例），
把以下几条按**现状**钉住。本轮不改生产代码：页大小上限失效、`ps+1` 溢出、脏主键游标降级、
负 `target_mid` 四处写了「收严改哪里」，其中页大小配置、脏主键游标、负 `target_mid`
三处在对应用例里落了 ⚠ 现状哨兵（收严后必须变红）；`operator_mid` 那条是需要评审的越权口径
（用例只钉现状，不预判结论）；「不走 self 域读侧门禁」与「投影不含正文」两条是现状正确、
但容易被后人误改的口径，留痕防误加。全部行号为本轮 Read/Grep 亲自确认。

- **`MaxPageSize<=0` 等于不设上限，启动自检也放得过去**（分页/配置口径，收严候选）
  现状：`clampPageSize` 的上限分支写作 `if maxPS > 0 && ps > maxPS`（`internal/logic/helpers.go:251`），
  `MaxPageSize` 配成 0 或负数时整条上限检查被跳过；而 `Config.Validate()` 只比
  `MaxPageSize < PageSize`（`internal/config/config.go:117-119`），`PageSize=0`+`MaxPageSize=0`
  这种「两个都缺省」的组合能通过自检并带着不设上限的页大小启动。
  影响：运营台一次请求可以把整张 `pm_report` 拉进内存并回成单页响应，护栏在配置写错时静默消失。
  收严改哪里：`helpers.go:251` 改成 `maxPS <= 0` 即报错（或回落到硬编码上限），
  并在 `config.go:117` 旁边补 `PageSize <= 0 || MaxPageSize <= 0` 的启动期拒绝。
  用例：`TestListReportsPageSizeFollowsConfigLimits` 段②（含 `degenerate.Validate()==nil` 前提）、
  `TestListReportsPageSizeBounds`（正配置下的上下限）。
- **`ps+1` 探针不防 int32 溢出，本方法自己不发现**（分页口径，收严候选）
  现状：`ListReports` 为了判 `has_more` 多取一条，下传 `ps+1`（`internal/logic/listreportslogic.go:58`）。
  `MaxPageSize` 配到 `math.MaxInt32` 时 `ps=MaxInt32` 是合法入参，`ps+1` 溢出成 `MinInt32`；
  logic 不做二次检查，只由 model 的 `if ps <= 0 { return ErrInvalidPage }`
  （`model/pm_report.go:187-189`）原样挡回。
  影响：不产生错误数据（守卫在 SQL 之前），但「页大小非法」要以负数的形式在下一层被发现，
  且 `ErrInvalidPage` 的消息（`ps=-2147483648`）对排障是误导——真实入参是 21 亿。
  收严改哪里：`listreportslogic.go:58` 的 `ps+1` 前加 `ps == math.MaxInt32` 判定，
  或在 `clampPageSize` 里把上限压到 `math.MaxInt32-1`。
  用例：`TestListReportsPageSizeFollowsConfigLimits`（第③段，直接钉下传实参 `ps == math.MinInt32`）。
- **游标编不出来时把「还有下一页」说成「到底」**（游标口径，收严候选）
  现状：`encodeIDCursor` 对 `id <= 0` 静默回空串（`helpers.go:288-293`），
  logic 拿到空游标后连带把 `hasMore` 抹成 `false`（`listreportslogic.go:66-72`），
  既不报错也不写日志。与会话域的 `encodeTimeIDCursor` 同族。
  影响：`report_id` 是自增主键，正常写入永远到不了这条分支，只有 DBA 改库/数据迁移出错才会命中；
  一旦命中，台账**静默截断**——运营看到的是「已到底」，而后面还有行。
  收严改哪里：`listreportslogic.go:69-71` 这一支应记 `Errorw` 并回明确错误（至少保留 `has_more=true`
  让调用方知道没到底），而不是伪装成最后一页。
  用例：`TestListReportsCursorFailStopsOnUnencodablePrimaryKey`（手工塞 `report_id=0/-1` 两行脏主键，
  并先证明探针值确实是 `ps+1`，以免「has_more=false」只是因为表里本来就只有这些行）。
- **`operator_mid` 只做「有没有主体」检查，且不参与台账切分；读取权限点在服务与网关之间两头落空**（越权口径，收严候选）
  现状：服务侧 `checkOperator` 只判 `mid <= 0`（`helpers.go:131-136`，注释即写明
  「真正的管理员鉴权在 gateway/admin」），`ListReports` 过守卫后就不再使用该值
  （`listreportslogic.go:42` 与 `:58` 的实参里没有 operator），台账是全站一份；
  网关侧 `/admin/private-message/report/list` 只挂 `rest.WithPrefix`、**没有** `AdminPermission`
  （`gateway/admin/internal/handler/routes.go:1731-1741`，同一 prefix 下的 `/report/handle`、
  `/retention/purge` 都挂了，见 :1743-1761），`routePermissions` 里也只有
  `pm:report/handle` 与 `pm:retention/purge` 两个点（`gateway/admin/internal/middleware/adminpermissionmiddleware.go:244-245`）；
  而 `operator_mid` 取自请求体自报值（`gateway/admin/internal/logic/privatemessagereportlistlogic.go:47,59-66`，
  `requireOperator` 同样只判存在，`gateway/admin/internal/logic/validate.go:18-23`）。
  影响：能访问 admin HTTP 端口的调用方可翻页读取**全平台**举报台账（含举报人写的说明文本），
  且审计归因可伪造；当前唯一防线是网络隔离。网关侧注释
  （`privatemessagereportlistlogic.go:31-39`）说这条读路由排除在权限表外是刻意的（避免读放大），
  所以这是一个需要评审而非直接修的口径问题。
  收严改哪里：要么给该路由挂 `AdminPermission` 并登记 `pm:report:list` 权限点
  （同步 `gateway/admin` 的漂移门禁与 `op_permission` 种子），要么让本服务按 operator 的可见范围切分
  （需要新增「运营-数据范围」关系数据，属契约评审）。
  用例：`TestListReportsOperatorMidIsNotAScope`（三个合法 operator 回同一页，钉「不切分」）、
  `TestListReportsOperatorGateBlocksBeforeAnyQuery`（钉服务侧只拒无主，被拒时零数据句柄调用）。
- **举报台账不走 self 域读侧黑名单门禁**（门禁口径，现状正确、留痕防误加）
  现状：`peerBlockedForRead`（`internal/logic/gate.go:219`）只有两个调用方——
  `listconversationslogic.go:91` 与 `listmessageslogic.go:58`，都是 self 域；`ListReports` 一次都不调。
  口径依据：黑名单保护的是「用户 A 不想再看见用户 B」，而这里的读取主体是运营、读的是举报单；
  照搬 self 域口径等于让 social-graph 的可用性决定运营能不能看台账。
  用例：`TestListReportsDoesNotUseReadSideBlacklistGate` —— 三个下游全 nil 时台账**必须成功**，
  并在同环境对照 `ListConversations` 仍回 `ErrSocialGraphNotConfigured`（fail-closed），
  以免「门禁整体坏掉了」被误读成「本方法设计上不接门禁」。
- **正文与链路句柄都不出台账；`description` 只在读侧脱敏**（隐私口径，现状符合设计）
  现状：运营侧投影 `reportInfo`（`internal/logic/conv.go:70-89`）搬 13 个字段，
  与契约字段集合一一对应（`rpc/privatemessage.proto:322-336`），**不含** `content_cipher`/`preview`/
  `content_hash`，也不投影 `trace_id` 与 `handle_idempotency_key`；`description` 走 `displayUserText`
  （`conv.go:81` → `helpers.go:363-365`）在读侧去掉链接与 ≥7 位数字串，
  而写侧只 trim + 按列宽截断（`internal/logic/reportmessagelogic.go:61`、`helpers.go:41`），
  库里存的仍是举报人原话（处置证据）。入参集合同样钉死（`rpc/privatemessage.proto:338-346`：
  没有按 `conversation_id`/`msg_id`/`report_id` 收敛的过滤位，「只看未处理」只能用 `state=1` 表达）。
  影响：这条是隐私边界的正向证据，同时是「加字段必须先过评审」的 fence——
  新增字段/新增过滤器会让用例立刻变红。与上节「正文加密与留存」同口径（正文只经 moderation 脱敏通道取阅）。
  用例：`TestListReportsProjectionIsFieldForField`（逐 13 字段跟**回查到的落库行**比对 +
  导出字段集合 fence + `loadedMessageRows` 差分为 0，证明整条链路一行消息都没物化进内存）、
  `TestListReportsDescriptionIsMaskedOnlyAtReadTime`（末尾全表扫描断库里没有任何一行被回写成脱敏文案）。
- **`state`/`target_mid` 的校验强度不对称，且 model 故障是原样上抛、零日志**（入参与故障口径）
  现状：`state` 有范围校验（`listreportslogic.go:45-48`，非 0 且不在 `1..3` → `ErrInvalidReportAction`），
  `target_mid` 完全不校验（`listreportslogic.go:58` 透传），而 model 的语义是 `targetMid > 0` 才加条件
  （`model/pm_report.go:196-199`），所以**负 `target_mid` 等价于「不过滤」**、回整页而不是报错；
  游标侧则严格 fail-stop（不可解析一律 `ErrInvalidCursor`，不退化成第一页，`helpers.go:295-305`）。
  另一侧，`ListByCursor` 返回的任何错误都原样上抛（`listreportslogic.go:58-61`），
  既不折叠成「空页」这种看着像成功的结果，也不就地吞掉只写日志（本方法一次日志都不写，
  连故障路径也是 0 条，归因完全交给 RPC 层与网关）。
  影响：运营台把 mid 误传成负数时拿到的是全站台账，不会越权但容易误读成「这个人名下就这些」；
  从 admin HTTP 入口这条路暂时被网关的 `pmNonNeg` 拦着（`gateway/admin/internal/logic/conv_privatemessage.go:60-65`
  在 `privatemessagereportlistlogic.go:50` 调用），**直连 RPC 的调用方没有这层保护**。
  台账查询在故障时只留下调用方的错误码，服务侧无痕迹。
  收严改哪里：`listreportslogic.go` 在 `state` 校验旁边补「`target_mid < 0` → `ErrInvalidMid`」
  （0 要保留「不过滤」语义，所以不能直接套 `checkMid`，它拒 `<= 0`，见 `helpers.go:92-97`），
  与其余入口的 mid 口径统一。
  用例：`TestListReportsStateAndTargetFiltersAreAnded`（6 行矩阵、每档唯一种子值，
  同时校验下传的 `state/target_mid/cursor/ps` 四个实参位置，末尾钉负 `target_mid` 现状）、
  `TestListReportsGuardPrecedence`（主体→状态→页大小→游标四层次序，每层被拒时零数据句柄调用）、
  `TestListReportsPropagatesModelFailureVerbatim`（`errInjected` 原样上抛、恰好一次不重试、
  失败与成功路径的日志增量都必须为 0、`trace_id` 被接受但完全不改变行为）。
