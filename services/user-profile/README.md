# user-profile

用户资料、个人空间和隐私可见性服务。本服务是 `user-profile` 域的数据所有者
（见 [AGENTS.md §5](../../AGENTS.md)），持有用户可变展示资料、经验等级、节操值、
官方认证、实名认证、属性审核与监控名单，并作为 identity 域的核心服务向
account 等其他服务提供资料聚合查询。

> **架构约束**：领域微服务只暴露 gRPC，不提供 HTTP（AGENTS.md §3/§4）。
> HTTP 入口统一由 `gateway/app`、`gateway/admin` 聚合；参考仓库 member 服务的
> `/x/internal/member/*` HTTP 路由不移植到本服务，对应能力全部以 gRPC 暴露，
> 需要 HTTP 形态时由网关做 BFF 聚合。
>
> 全量移植参考仓库 `openbilibili-go-common/app/service/main/member` 的服务功能
> （gRPC 35 个方法，含 `SetExp`/`UndoMoral`/`AddPropertyReview` 三个原 HTTP-only
> 能力的 RPC 化）；参考仓库的 block（封禁）子模块属于 risk-control 域
> （AGENTS.md §5），不移植到本服务。

## 职责

- **基础资料**：昵称/性别/头像/签名/排名/生日的查询与更新（Base/Bases/Member/Members、SetSex/SetName/SetFace/SetRank/SetBirthday/SetSign、NickUpdated/SetNickUpdated），Redis 缓存 + fanout 异步回填。
- **经验等级**：经验值读写（Exp/Level/UpdateExp/ExpLog/ExpStat），等级 0-6 由经验实时推导；当日奖励统计走 Redis 位图。
- **节操值**：Moral/MoralLog/AddMoral/BatchAddMoral，变更走事务（读-改-写 + 日志），阈值通知经 Outbox 事件异步投递。
- **官方认证**：生效信息内存快照（5 分钟刷新，变化时通知 account 失效缓存）、认证申请文档提交与查询。
- **实名认证**：验证码发放与校验、申请单、身份证生日/性别解析、成年判断、证件 RSA 密文入库与查重反查、脱敏信息。
- **属性审核**：受监控用户名单、头像/签名/昵称变更审核记录与归档。
- **领域事件**：AGENTS.md §5 Outbox 模式——业务写操作与事件同事务提交，发布器异步投递 `user.profile.updated`（→ account `DelCache` RPC 失效缓存）与 `user.moral.notice`（→ notification，待接入）。

## 数据所有权边界

- 其他服务只能通过 gRPC（etcd 注册 key `user-profile.v1.rpc`）访问本服务数据，
  禁止直连本服务的 MySQL 表或 Redis key。
- 账号主数据（mid、状态、登录标识）由 account 持有；user-profile 按 mid 维护展示资料。
- 实名证件号仅以 RSA 密文 + 带盐 MD5 哈希落库，明文只在缓存中短暂存在；私钥经 Secret/Vault 注入。
- 封禁/处罚数据属于 risk-control 域，本服务不持有（account 通过自有 status 字段表达账号封禁）。

## gRPC API

package `userprofile.v1`，端口 8085，etcd 注册 key `user-profile.v1.rpc`，共 35 个方法：

| 分组 | 方法 |
|---|---|
| 资料查询 | `Base` `Bases` `Member` `Members` `NickUpdated` |
| 资料更新 | `SetSex` `SetName` `SetFace` `SetRank` `SetBirthday` `SetSign` `SetNickUpdated` |
| 官方认证 | `SetOfficialDoc` `OfficialDoc` |
| 节操 | `Moral` `MoralLog` `AddMoral` `BatchAddMoral` `UndoMoral` |
| 经验 | `Exp` `Level` `UpdateExp` `SetExp` `ExpLog` `ExpStat` |
| 实名 | `RealnameStatus` `RealnameApplyStatus` `RealnameTelCapture` `RealnameApply` `RealnameDetail` `RealnameStrippedInfo` `MidByRealnameCard` |
| 监控 | `AddUserMonitor` `IsInMonitor` `AddPropertyReview` |

下游依赖关系：

- **account → user-profile**：account 的 `UserProfileClient` 适配器
  （`services/account/internal/repository/userprofile_client.go`）已接通，
  account 的资料/经验/节操/实名聚合走本服务 RPC。
- **user-profile → account**：资料更新事件（`user.profile.updated`）经 Outbox
  发布器调用 account 的 `DelCache` RPC 失效 account 侧 Info/Card/Profile/Vip
  缓存（替代参考仓库 databus 的 MemberService-AccountNotify 主题）。两条 RPC
  职责单一、方向不同；后续接入消息总线后可平滑改为事件驱动。

## 数据模型与迁移

共 14 张表，迁移脚本位于 `deploy/migrations/user-profile/`，每个字段、每张表、
每个索引均带详细中文注释，并注明参考仓库字段映射与回滚方式：

- `000001_create_user_base.sql`：基础资料
- `000002_create_user_exp.sql`：经验值
- `000003_create_user_flag.sql`：用户标志位
- `000004_create_user_moral.sql`：节操值
- `000005_create_user_official.sql`：官方认证三表
- `000006_create_user_monitor.sql`：监控名单
- `000007_create_user_property_review.sql`：属性变更审核
- `000008_create_realname.sql`：实名认证三表（证件密文）
- `000009_create_member_log.sql`：经验/节操变更日志
- `000010_create_member_outbox.sql`：领域事件 Outbox
- `README.md`：所有权边界、参考映射与回滚说明

## 目录结构

```text
services/user-profile/
├── rpc/userprofile.proto        gRPC 源契约（35 个方法）
├── etc/userprofile.v1.yaml      配置示例
├── model/                       14 张表的实体、常量与查询接口
├── internal/
│   ├── config/                  配置（RpcServerConf + Redis + MySQL + 实名密钥 + Outbox + AccountRPC）
│   ├── server/                  goctl 生成的 RPC server
│   ├── logic/                   业务逻辑（35 个 RPC logic）
│   ├── repository/              仓储层（MySQL + Redis 缓存 + RSA 加密 + 官方认证快照 + Outbox 发布器 + account RPC 客户端）
│   └── svc/                     ServiceContext
├── userprofile.v1.go            RPC 入口（无 HTTP server）
└── README.md
```

## 运行

```powershell
# 生成（从仓库根目录）
./scripts/gen.ps1 -Service user-profile

# 运行（纯 RPC，无 HTTP 端口）
go run ./services/user-profile -f services/user-profile/etc/userprofile.v1.yaml

# 健康检查：使用 gRPC health 探针（grpc_health_probe -addr=127.0.0.1:8085）
```

## 测试覆盖

本节只陈述**覆盖面**（哪个文件钉了哪条口径、哪些层根本没有用例），不复述任何门禁执行结果。
缺口清单的权威登记在下一节「已知缺口」。

### 1. `internal/logic`：35 个 RPC 方法的逐文件用例

只覆盖纯逻辑与内存替身，不连真实 MySQL/Redis/Kafka/etcd，全部离线。
28 个 `*_test.go`，其中 26 个含 `Test*`（`fakes_test.go` 是共用替身层、`realnamekeys_test.go` 是实名簇
脚手架，两者顶层用例为 0 属正常），合计 **232 个顶层用例 / 116 个 `t.Run` 子用例 / 0 个 `t.Skip`**。
方法名与文件不是一对一：`Bases` 在 `baselogic_test.go`、`Level` 在 `explogic_test.go`、
`ExpStat` 在 `exploglogic_test.go`、`Members` 在 `memberlogic_test.go`、`MoralLog` 在
`morallogic_test.go`、`IsInMonitor` 在 `nickupdatedlogic_test.go`、`SetExp` 与 `UpdateExp` 同在
`setexplogic_test.go`、`RealnameApplyStatus` 在 `realnamestatuslogic_test.go`、
`RealnameStrippedInfo` 在 `realnamedetaillogic_test.go`。下表按域分五组（括号内是方法数）。

**A. 读侧 12 个方法（60 个顶层用例 / 13 个子用例）**

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `baselogic_test.go` | 11/4 | `Base`/`Bases` | 「查无此人」是**结论**不是错误：返回 `mid=0` 哨兵并把它**写回缓存**（防击穿），只有下游真故障才允许报错；miss 必须回源 + 回填 TTL=3600 且回填内容能被下一轮读出（`baseCachePayload` 用同名 JSON tag 刻意耦合，生产改字段名 → 用例立刻红）；批量只把 miss 的 mid 交给 `FindMany`、库里没有的补占位不跳过、重复 mid 合成一条、超 100 个 mid 在触缓存之前就被拒；缓存故障吞成 miss 后照常回源且不报错 |
| `explogic_test.go` | 9/2 | `Exp`/`Level` | `Exp` 与 `Level` 的**唯一**差别是 `now_exp` 是否回填（`sexp` 标志），等级/下界/下一级阈值必须一致（把 `sexp` 传错是这类接口最典型的越权/漏字段故障）；阈值逐档钉：入库单位是「分 × `ExpMulti`」，199 分仍 1 级、200 分才升 2 级、满级 `next_exp=-1`、负经验向零截断不 panic 也不变满级；命中不回源、miss 回填 TTL=86400、经验表无行按 0 处理（0 合法故不做防击穿哨兵）；`exp_<mid>` 与 `bs_<mid>` 两把 key 互不串 |
| `exploglogic_test.go` | 11/0 | `ExpLog`/`ExpStat` | 一张 `member_log` 按 `log_type` 隔离（经验 11 / 节操 12），串味会让用户在「经验记录」页看到扣分明细；7 天窗口 + `status=0` + `ts DESC, id DESC` + 1000 行上限 + 内容解析失败整行跳过 + 空结果是非 nil 切片；`ExpStat` 是纯位图读但**不降级**（`GetBit` 故障如实上抛，与 `BaseInfo`/`Moral` 的「吞成 miss」口径相反），key 期望一律写**字面量**（`ea_login_`/`ea_shareClick_`/`ecoin_`）而非用替身常量拼接，分片 `mid/10000` 与位偏移 `mid%10000` 各测一刀 |
| `memberlogic_test.go` | 12/2 | `Member`/`Members` | 聚合接口的「局部失败怎么算」：单用户查无此人 → `ErrMemberNotExist` 且不再读经验（错误要早、代价要小）；经验读失败 → 零值等级 + 不报错；批量无该 mid 补 `mid=0` 占位而**不跳过**（与单查口径的不对称是有意为之，改一处要让另一处红）；批量经验整批失败 → 所有人零值 `LevelInfo` 但不报错；官方认证只来自**构造期载入的内存快照**，每次请求不得再查 `user_official`；冷启动快照逐 mid 入 Outbox |
| `morallogic_test.go` | 9/3 | `Moral`/`MoralLog` | 节操单位 1/100（基准 7000=70.00、上限 10000），「查无节操记录」回落 7000 且**不是错误**（做成错误会让新注册用户资料页全挂）；命中判定靠 `payload.mid != 0`（没有 `Cached` 哨兵），所以回填必须带请求 mid，否则永远命中不了；缓存读故障时**照样尝试回填**（`BaseInfo` 会跳过），这条同服务内的口径差异先按事实钉住；`MoralLog` 与 `ExpLog` 同表互查一次 |
| `nickupdatedlogic_test.go` | 8/2 | `NickUpdated`/`IsInMonitor` | 两个「布尔结论」接口最贵的失效模式是**把「查不到」说成「是」**（误报 true 会锁死改名入口 / 让普通用户被风控收紧策略）：无行、位未置、已软删除三种情况一律 `false` 且**不是错误**；只有真故障才允许 `err != nil`，且此时 `reply` 必须为 nil（logic 不得把 false 当成功返回）；两者都**没有缓存层**，将来加了缓存就是口径变更，必须改这里 |

**B. 资料写侧 8 个方法（45 / 26）**

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `setnamelogic_test.go` | 7/4 | `SetName` | `setBaseTx` 的共用不变量：单列 UPSERT 不许顺手改别的列（一次改名把头像/签名清掉是线上事故）、`user.profile.updated` 事件与写**同事务**（事件行拿得到 tx 会话）、提交成功后才失效 `bs_<mid>` 且失效失败仍返回成功（行为哨兵）；空串清空、无行按 DDL 默认值建行；`mid`、`VARCHAR(64)` 长度、空白串、`remote_ip` **一律不校验/不使用**，缺的校验不虚构，一律钉成现状 |
| `setfacelogic_test.go` | 5/4 | `SetFace` | 动作位必须是 `updateFace`（account 侧据此决定失效哪份缓存）；头像 URL **一个字符都不校验**——非 https、`data:`、`javascript:`、带签名参数的 OSS 长期地址全部照写（AGENTS.md §6 的那条防线完全落在网关与前端）；读侧没有默认头像兜底（`model.URLNoFace` 常量存在但全仓无引用）；恰好一条 `updateFace` 事件 |
| `setsexlogic_test.go` | 5/4 | `SetSex` | 动作位是 `updatePersonInfo` 而非 `updateUname`；值域只有 DDL 注释里的 `{0,1,2}` 但本层不校验，99/-1 原样交给 SQL（列是 TINYINT UNSIGNED，真库会 1264，替身按 int64 存，所以钉的是「本层不校验」而不是「值合法」）；`0` 是「保密」这个**合法结论**不是「未传」，必须真的写进去 |
| `setbirthdaylogic_test.go` | 6/4 | `SetBirthday` | 与 SetName 共用不变量，另钉 birthday 的值域特殊性：`birthday` 是**有符号** BIGINT、默认 `-28800`、负值合法且被当哨兵用；与 `SetBase` 的口径不一致（`SetBase` 把 `Birthday==0` 归一成 `-28800`，本入口完全没有那层归一）；没有任何「只允许设置一次」检查；越界值会经 `BaseInfo` 一路穿透到前端渲染 |
| `setsignlogic_test.go` | 5/4 | `SetSign` | 动作位 `updatePersonInfo`；签名是最典型的富文本注入面——控制字符、HTML、Markdown、零宽字符、超长（DDL `sign` VARCHAR(255)）全部**原样入库**，不裁剪、不转义、不入审核；空串=清空（不是拒绝也不是保留旧值）；写侧不影响他人缓存 |
| `setranklogic_test.go` | 6/4 | `SetRank` | 这批里**权限语义最重**的写入口：`rank >= 10000` 是本服务 `checkExpMember` 判定「是否会员」的**唯一**判据，而本方法对调用方身份、值域、这个语义跳变一律不设约束；跨方法后果被显式钉住（一次 `SetRank` 就能让 `SetExp` 从 `ErrUserNoMember` 变成成功）；`rank` 是 BIGINT UNSIGNED DEFAULT 5000，但 logic/repository 层零校验、参数原样下传 |
| `setnickupdatedlogic_test.go` | 5/1 | `SetNickUpdated` | 最短链路：一步 `INSERT INTO user_flag ... ON DUPLICATE KEY UPDATE flag = flag OR ?`；由此产生的现状——**无事务、无 Outbox 事件、无任何缓存失效**（标志位变更在事件流里完全不可见，下游无法据此收敛行为）；位或天然幂等且**只能置位不能复位**；没有「必须先真的改过昵称」的因果校验 |
| `addusermonitorlogic_test.go` | 6/1 | `AddUserMonitor` | `ON DUPLICATE KEY UPDATE ... is_deleted = 0` 的幂等 + 复位软删除；不校验 mid 是否存在，「名单归属」只能钉成「以传入 mid 为主键的一条记录」；`operator` 完全由调用方自填、可任意伪造；**加入监控名单这一步不留任何审计痕迹**（无 member_log、无事件、无事务），而监控名单恰恰决定「资料变更是否强制进审核」；只影响目标 mid |

**C. 节操与经验写侧 5 个方法（34 / 45）**

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `addmorallogic_test.go` | 10/14 | `AddMoral` | 本批最复杂、也是唯一**有真守卫 + 走事务 + 带阈值通知**的写链：`origin` 合法性与 `NeedReason` 守卫在任何触库之前；正负边界与**越界钳制而非拒绝**；跌破基准才写 `recover_date`；每次变更恰好一行日志且日志与变更同事务（日志失败 → 变更回滚）；**重放会双记**（无幂等键）；`delMoralCache` 的错误被 `logx.Errorf` 吞掉；阈值通知的触发/不触发全集（`reasonType` 只有 1 弹幕 / 2 评论会触发，各再开一个事务），通知文案不带原始违规文本 |
| `batchaddmorallogic_test.go` | 9/12 | `BatchAddMoral` | 与单个 `UpdateMoral` 共用 `updateMoralTx`，但三点差异逐个钉：整批**一个事务**（任一行失败 → 整批失败、应答里是 nil map）；事务后的失效/通知走 `for mid := range beforeMap`，顺序随机，故尾部只做集合断言 + 「每个 mid 恰好失效一次」；批量日志 content **少一个 `mid` 键**（8 键 vs 7 键）。另钉两件事：**没有批量上限**（200 个 mid 照单全收），重复 mid 既不去重也不报错——扣两次、日志两行，但前值被后一次覆盖，跨档那次的通知因此被吞 |
| `undomorallogic_test.go` | 8/9 | `UndoMoral` | 节操域里唯一**没有幂等键、也没有权限入参**的写链：撤销语义是「反向 delta 重放到**当前值**」而非回到当年值（中间发生过别的变更就拿不到原值）；台账只有两行（原行 `MarkRevoked` 改 `status=1` 后从 `status=0` 过滤里消失，新行是反向变更且 content 的 8 个键里没有一个指回被撤销的原 `log_id`）；`repository/moral.go` 里那条「复制原日志、置已撤销」的撤销台账因为复用 `(log_type, log_id)` 唯一键，在真库永远 1062 并被吞——`TestUndoMoralRevokedAuditRowNeverLands` 把「这个设计从未生效」钉成事实；重复撤销没有幂等保护（`FindByLogID` 不过滤 status），每撤一次就再退一次；`MarkRevoked` 失败留两条生效行；content 解析失败留半态 |
| `setexplogic_test.go` | 7/10 | `SetExp`/`UpdateExp` | 这批写方法里唯一真带前置守卫的：`checkExpMember`（`mid<1` → `ErrRequestErr`、查无此人 → `ErrMemberNotExist`、`base.Rank<10000` → `ErrUserNoMember`）必须发生在碰 `exp_` **之前**，守卫用例因此断言「轨迹里没有任何 `exp.` 调用」；目标是**绝对赋值**不是增量，`count` 是 float64 → 小数向零截断、`count<0` 算出负 target（DDL 是 BIGINT UNSIGNED 会拒，替身按 int64 存，钉的是「本层不校验」）；`UpdateExp` 有 `count==0 → return nil` 而 `SetExp` **没有**（count=0 会真清零并留下一条日志），两处口径差分别钉；`addExpLog` 的错误被丢弃 → 经验改了但审计丢了仍是成功；整条链路**不在事务里**也**不发 Outbox** |

**D. 实名簇 7 个方法（73 / 13）**

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `realnameapplylogic_test.go` | 21/1 | `RealnameApply` | 全服务唯一**写入证件号**的入口，断言面按「明文号绝不允许出现在返回值 / 日志 / 落库列 / Redis 载荷」逐条铺：`realname_apply.card_num` 必须是能用同一把 PEM 解回原号的**密文**；Redis 侧用 `blobOf` 取**历史载荷**（成功路径末尾会删 key，只查终态会漏）；日志用 `logtest` 收口，且同一用例先断言「确实捕获到了那行日志」以免捕获通道失效造成假判；**次序本身也是断言**：验证码 → 读实名信息 → 查重 → 才落图片 → 加密 → 写申请单，图片先落库意味着格式/加密错误会留下孤儿图片行，blast radius 用 `countPrefix`/行数显式钉住而不是只看错误码。另钉：错验证码的 reset/`err_times` 递增与二次错误跳过 reset、超次锁定、pending/pass 拒而 rejected 放行、跨 mid 绑定拒绝、号串以 `=` 结尾时绕过格式检查把**明文**写库（缺陷哨兵）、超长号加密失败且不回显、replay 累积 pending 行、repository 设的 `ctime` 从未落到行上 |
| `realnamedetaillogic_test.go` | 20/2 | `RealnameDetail`/`RealnameStrippedInfo` | 一对对明文态度**相反**的方法：`RealnameDetail` 按契约把解密后的明文当返回值（重点在解密链与降级——解不开时 `Card` 为空、`Gender` 回落 unknown 而不是报错），`RealnameStrippedInfo` 的存在意义就是**不含姓名与证件号**（除字段口径外还把 reply 的字符串形态整体扫一遍）；15 位号的 `19` 前缀分支、非身份证类型保持 unknown、女号解析、手持照三条件（主站渠道 + 申请单通过态 + `hand_img>0`）各测一刀；`realnameInfo` 内部被读**两遍**，缓存故障时就是两次 SELECT；`TestRealnameDetailParseFailureLeaksPlaintextCardIntoInfoLog` 把「解析失败路径把明文写进 INFO 日志」钉成可执行记录 |
| `realnamestatuslogic_test.go` | 14/3 | `RealnameStatus`/`RealnameApplyStatus` | 最轻的两个读，却坐在**同一个缓存条目** `keyRealname` 上，所以两者用例共同回答三件事：状态口径（三态压成 0、只有 Pass 是 1；四态与驳回原因只有 `ApplyStatus` 给得出）、**谁先读谁写缓存**（载荷里躺着 `real_card` 明文与 reason，任一方法的回填都会被另一个命中）、降级面（缓存故障只多吃一次 SELECT，读库失败才整体报错）；TTL 内第二次调用不碰库、**陈旧缓存优先于库行**；状态查询这一路也会把明文号写进 Redis（`TestRealnameStatusStatusOnlyCallStillPutsPlaintextCardIntoCache`）；未知状态码原样透传 |
| `midbyrealnamecardlogic_test.go` | 10/1 | `MidByRealnameCard` | 实名簇里唯一**不读缓存**、也唯一把明文号串当入参的读口，断言面全在哈希与映射方向：`cardMD5` 的键是 salt + 小写号串 + 证件类型 + 国家（盐未导出，故用 `md5Oracle` 让**生产实现自己**给出哈希再回塞进行里，少拼一维就红；映射方向/状态过滤/大小写/去重都与 oracle 无关）；出口键是**调用方给的原号串**不是哈希；只统计 pending 与 pass 的绑定；重复号串不去重直接回；空/nil 不产生 SQL；空格属于哈希的一部分；一次请求 = 一条 SELECT（实现里没有分批、没有上限，靠用例钉住现状）；已 warming 的 `realname_info` 缓存不参与本方法 |
| `realnametelcapturelogic_test.go` | 8/6 | `RealnameTelCapture` | **只写 Redis**：一次发码全程 0 次 MySQL、0 事务、0 事件，所以断言面就是缓存键值本身 + 日志 + 应答三处；配额口径是 `times > 5`——24 小时内放行的是**第 1..6 次**、第 7 次才拒，且拒时一次写都不做（连错误计数都不清）；6 位随机验证码是**一次性凭据**，应答里没有、MySQL 里没有，唯一藏身处是 `realname_cap_code_<mid>`（TTL 600s），而 repository 把它**明文写进 INFO 日志**（`TestRealnameTelCaptureCodeIsLoggedInPlaintext`）；次数为负（手改/半写）先归零再放行；发新码会清零「验证码错误次数」= 给爆破窗口续期；写码失败被吞（连返回值都没有） |

**E. 资料审核与官方认证 3 个方法（20 / 19）**

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
| --- | --- | --- | --- |
| `addpropertyreviewlogic_test.go` | 10/10 | `AddPropertyReview` | **old 与 new 的存储形态不对称**：旧头像经 `facePath` 归一成 URL path（域名与 query 全丢），新值却是调用方传什么存什么（含 `?auth_key=` 这类短期签名参数），于是同一行两列口径不同、带令牌的 URL 永久留在 `user_property_review.new`；**守卫的位置与形状**：extra 的 JSON 闸门发生在触库之前（一次依赖都不许碰），property 白名单却坐在 `base.FindOne` **之后**，`State`/`Property` 由 int32 直接截成 int8（259 会被当成「昵称」、300 会被当成 44 档）；重复提交按现状钉（第二次把第一次的待审行 `state 0→3` 归档再追加，表上无唯一键所以行数只增；若提交非 0 状态则 `Archive` 的 `WHERE state = 0` 根本命中不到，重复提交就是两条待审）；`InMonitor`/`Archive` 失败被吞、接口仍成功——归档失败=攒出重复待审、监控读失败=受监控用户的变更被记成非监控 |
| `officialdoclogic_test.go` | 3/4 | `OfficialDoc` | 提交件的唯一读出口，钉的是**出口形态**：认证附加资料（联系人、联系电话、邮箱、地址、统一社会信用代码、营业执照、身份证明）存在 `user_official_doc.extra` 的**明文 JSON** 里（`model/official.go` 只做 `json.Marshal`，无脱敏无加密），`OfficialDoc` 再逐字段原样端出；与之相反，生效表 `user_official` 与内存快照**完全不在这条链路上**（两张表两套语义：提交件 vs 生效件），所以序列里一次 `official.*` 都不许出现；这条链路**没有缓存**，每次调用一次 SELECT、重复调用不合并；无 `mid<=0` 守卫 |
| `setofficialdoclogic_test.go` | 7/5 | `SetOfficialDoc` | 本批唯一会**落联系人隐私**的写入口，主断言是存储形态：`OfficialExtra` 经 `String()` **原样明文**写进 `user_official_doc.extra`（TEXT 列），既没脱敏也没加密，同一份信用代码再写一次进键值表 `user_official_doc_addit`（「明文进、明文出、两次写」）；`Validate` 必填守卫发生在任何触库之前且错误是 `ErrRequestErr`；`state` 被服务端强制成「待审核」、入参一律作废；驳回原因保留但 extra 整体替换；`Role`/`Realname` int8 截断；不碰其它表、不失效任何缓存 |

三个跨文件的口径值得单列（都在上面的用例里可核对）：

1. 第 4 轮（审核 / 认证 / 撤销 / 发码）的主断言面是**敏感原文的存储形态与出口形态**——
   `user_official_doc.extra` 明文、`OfficialDoc` 明文端出、验证码明文进 INFO 日志、撤销台账因复用
   `log_id` 永远写不进库。这些现状被逐条钉住并登记在下面的「已知缺口」，隐私/审计类还标了
   「改生产代码前不要动这条断言的期望」。测试数据一律用明显假值
   （`13800000000`、`000000000000000000`、`seed-contact@example.invalid`、校验位自洽的示例号）。
2. 剩余写侧方法在收口时暴露出的共性口径是**三层故障分类**：原始错误上抛
   （`base.FindOne`/`review.Add`/`TransactCtx` 内所有步骤）、换成不透明哨兵
   （`ErrSubmitOfficialDocFailed`，原始 SQL 错误只剩日志）、就地吞掉只写日志
   （附加表写入、InMonitor/Archive、`MarkRevoked`、`setCaptureCode`——最后这个连返回值都没有）。
   用例用调用序列 + 落库行 + 应答字段区分这三层，**不靠日志文本判定成功**。
3. 断言集中在三件事：守卫拒绝时一次依赖都不许碰（`wantNoCall`）、投影逐字段
   （种子值全部互不相同，走错数据源立刻可见）、副作用序列**按顺序**（`wantOps`：先读缓存还是先查库、
   回源后回填了什么 TTL、批量接口 `FindMany` 只拿到哪几个 mid、经验读失败时 Member 有没有退化）。
   错误一律 `errors.Is` 到注入的哨兵。

### 2. 其它层

| 层 | 文件 | 顶层/子 | 钉住了什么 |
| --- | --- | --- | --- |
| `model/` | `model_test.go` | 5/0 | `BuildLevel` 的 0–6 级与满级全部阈值分支、`SexStr` 性别映射、`UserFlag` 位运算、`OfficialExtra` JSON 往返、`OfficialDoc.Validate` 必填集——**只测纯函数，model 的 SQL 与列名一条都没测** |
| `internal/repository/` | `outbox_test.go`、`realname_test.go` | 6/0 + 6/0 | Outbox 发布器：`user.profile.updated` 经 account 的 `DelCache` 成功投递、RPC 失败退避重试、超最大次数标记失败、无 account 客户端/未知事件类型、节操通知尽力而为、`Start`/`Close` 生命周期；实名纯函数：15/18 位号解析生日与性别、成年判定、`isIDCard` 校验位、`cardMD5` 三元组、RSA 加解密往返与坏密钥分支 |
| `internal/config/` | `config_load_test.go` | 1/1 | 遍历 `etc/*.yaml` 用**真实** `conf.Load` 载入：本仓库出现过 Config 自带 `Redis` 字段与 `zrpc.RpcServerConf` 内嵌的 `Redis` 同名，代码可编译但启动即报 `conflict key redis`，这条只能靠真加载器钉 |
| `internal/svc` | — | **无离线单测** | 只做装配；`ServiceContext` 的构造在 logic 用例里通过真实 Repository + 内存替身被间接走到 |
| `internal/server` | — | **无离线单测** | goctl 生成的 gRPC handler 外壳，逐方法转调 logic，不含判定（见第 5 组边界） |

本服务没有 `internal/consumer`、`internal/policy` 目录；Outbox 发布器住在 `internal/repository` 内，
上表 `outbox_test.go` 就是它的唯一离线覆盖。

### 3. 构造器级覆盖：**35/35**

`internal/logic` 下 35 个 logic 构造器与用例逐个对齐，探针的缺口清单为空——即每个方法的构造器都被
实例化并至少跑过一条真实断言，不存在「建了对象但什么都没断言」的空壳用例。

### 4. 替身层与断言口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，而 `repository.New` 会连真
Redis + 真 MySQL，还会拉起官方认证刷新协程与 Outbox 发布协程，测试无处塞替身。因此
`internal/repository` 暴露 `Cacher` 接口（9 个 Redis 原语：`Ping/GetJSON/SetJSON/GetInt/SetInt/Del/GetBit/Incr/Expire`）
+ `NewWithDeps(cache, conn, 14 个 model, Options{IMGURLTemplate, Cryptor, Async})`；
生产路径仍只走 `New`（`New` 内部调 `NewWithDeps`）。logic 用例据此组装**真实 Repository**，
只把依赖换成内存替身，所以缓存回源、防击穿哨兵、等级换算、批量部分命中、事务与 Outbox
整条判定链都在被测路径上，而不是把 Repository 也 mock 掉。

两条设计约束值得后来者注意：

1. `Cacher` 只给**原语**、不给组合方法。key 派生（`bs_%d`/`exp_%d`/`moral_%d`/`ea_%s_%d_%d`/
   `ecoin_%d_%d`、分片 `mid/10000`、位偏移 `mid%10000`）与 TTL 选择（3600/86400）留在
   Repository 侧——它们正是业务口径，必须可被断言。原先 `*Cache` 上的组合方法
   （`delBaseCache`/`statCache`/`incrCaptureTimes`…）因此迁成了 `*Repository` 方法，语义逐字未改。
2. `fanout` 异步回填在测试装配里被换成 `syncRunner`（`Options.Async` 为空时默认如此），
   生产是入队异步、测试是就地执行：时序不同、内容一致，回填调用能稳定出现在断言序列里。

`internal/logic/fakes_test.go` 记了五条替身纪律（值拷贝、按真实 DDL 口径分配主键与默认值
`rank=5000`/`birthday=-28800`、按顺序记录 `<pkg>.<method>:<key>` 轨迹、布数据走静默
`put`/`warm` 路径且不写轨迹、按方法粒度注入错误）。最容易踩的还是第四条：
`newStore` 会清空构造期 `loadOfficial` 留下的轨迹（那是装配不是被测调用），
需要断言装配期行为本身时改用 `newRawStore`。

第 1 轮变异探针（改坏生产规则看用例是否真的红）四组：
绕过 `BaseInfo` 的回填门槛 `if cacheOK` ⇒ 4 个用例红（`回源：调用序列 = [... → base.FindOne:20002], want [... → cache.SetJSON:bs_20002/3600]`）；
绕过 `Member` 的 `if base.Mid == 0` ⇒ `查无此人：错误 = nil, want member not exist`；
绕过 `Moral` 的 `if payload.Mid != 0` ⇒ 3 个用例红（`节操缓存命中：moral = 100, want 8888`）。
探针后 `cp` 还原，并确认 `grep -rn "false &&" services/user-profile/` 为空。
第四组是**反向结论**：把 `Level` 的 `BuildLevel(count, false)` 翻成 `true` 用例不红，
因为该函数返回时 `NowExp` 被显式写死 0——那个参数在 `Level` 调用点上无行为差异（登记见「已知缺口」）。

**这套替身证明不了**：SQL 文本本身（列名、占位符个数、`WHERE` 与 `JOIN` 的真意）、MySQL 的
类型与越界行为（TINYINT UNSIGNED 的 1264、`VARCHAR` 静默截断、`ON DUPLICATE KEY` 的真实冲突键集合）、
Redis 的过期与内存语义、真 gRPC 链路（account 的 `DelCache` 是替身）、并发与事务隔离级别。
`GetInt` 的替身不返回错误——真实 `Cache.GetInt` 本来就把故障吞成 miss，造出错误分支等于测不存在的代码。
`ExpStat` 的 key 含 `time.Now().Day()`，布景与断言同源于用例开头取的那一次，跨零点抖动会让期望整体偏移
（每天最多 1 次、窗口亚秒级），未做补偿。
实名簇的测试密钥**已经不是占位 PEM**：`fakes_test.go` 的 `mustTestCardKeyPEM` 在包初始化时真的生成一把
RSA-2048（PKIX 公钥 + PKCS#1 私钥，与 `CardCryptor` 的解析分支逐一对应），`realnamekeys_test.go`
据此提供 `encryptCard`/`decryptCard` 回读，用例能证明「库里的确是可解的密文」而不是「一串看不出内容的字」。
需要「解密必然失败」分支时用另一把不配的密钥，不要靠坏 PEM 触发。

### 5. 覆盖边界（如实声明）

- **不连真实依赖**：无 MySQL、无 Redis、无 etcd、无 MQ、无对象存储。所有跨服务调用
  （Outbox 投递到 account 的 `DelCache`）都是接口替身。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有声明在隔离实例
  `127.0.0.1:3399` 跑过 `deploy/migrations/user-profile/` 的落地复验，仓库里也**没有**
  user-profile 的迁移↔model 列级对账门禁（同类门禁见 payment/creator-revenue 等服务的
  `model/migration_parity_test.go`）。改表时按 docs/commands.md 的迁移流程在隔离实例复验，
  不要在读完本节后就假定列名/宽度一致。
- **`model/*.go` 的 SQL 与列名无单测**：model 层只有 5 个纯函数用例（第 2 组）。
- **goctl 生成的外壳不在范围内**：`internal/server`、`internal/svc`、`rpc/` 的生成代码
  只做转发与序列化，按仓库约定不为它们写用例（`.proto` 才是真源）。
- 本节**未复核**缺口清单：缺口的权威登记在下一节「已知缺口」，那里逐条给出现状 + 影响 + 收严位置。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/user-profile/...   # -p 1：Windows 下并行测试包会撞页面文件上限（errno=1455）
gofmt -l services/user-profile                       # 期望无输出
go vet ./services/user-profile/...
```

上述命令由评审者自行执行；本节不声称它们当前的结果。

## 已知缺口

四轮用例把下列事实**钉成了可执行的记录**，但**未修改任何生产代码**——
（1~12 由前三轮登记，13~21 由第 4 轮「审核 / 官方认证 / 撤销 / 实名发码」登记）
逐条给出「现状 + 影响 + 若要收严应改哪里」，供后续轮次与评审裁决：

1. **`Exp`/`Level`/`Moral`/`MoralLog`/`ExpLog`/`ExpStat`/`NickUpdated`/`IsInMonitor` 都没有 `mid<=0` 守卫**：
   只有 `BaseInfo`（含继承它的 `Member`）与两个批量接口守门。`Moral(0)` 会返回基线值 7000 且**带上
   `mid=0`**，调用方无法与真实用户区分；`Exp(-7)` 会照查 `exp_-7`、照回填 `exp_-7`（TTL 86400）。
   用例 `TestBasesDoesNotInventMidZeroGuard`、`TestExpAndLevelDoNotGuardNonPositiveMid`、
   `TestMoralDoesNotGuardNonPositiveMid`、`TestExpStatDoesNotGuardNonPositiveMid`、
   `TestNickUpdatedDoesNotGuardNonPositiveMid`、`TestIsInMonitorDoesNotGuardNonPositiveMid`
   按现状断言（**不是**期望值），加固点在各方法入口。
2. **`BaseInfo` 的 `cacheOK` 分支在生产上不可达**：`*Cache.GetJSON` 把读故障与反序列化失败一律
   吞成 miss 并返回 `nil`，所以 `cacheOK=false` 永不成立。本轮只能靠替身显式注入错误才走进该分支
   （`TestBaseCacheFaultDegradesToDBWithoutError`）。若要区分「缓存坏」与「缓存无值」，得让
   `GetJSON` 上抛错误，但那会同时改变 `Moral`/`BatchBaseInfo` 的降级口径，需整体评审。
3. **`Moral` 的缓存命中判定不看 mid**：命中条件是 `payload.Mid != 0`，而 `BaseInfo` 用的是独立的
   `Cached` 哨兵字段。一旦 `moral_%d` 下写入的值与 key 的 mid 不一致（人工修数、跨 key 误写），
   `Moral(90002)` 会原样返回他人节操值且不回源。`TestMoralCacheIsTrustedBlindlyEvenForAnotherMid`
   把这条盲区固定下来；收严方式是比对 `payload.Mid == mid`。**同一服务内两套命中口径**也在此处。
4. **冷启动会给每个官方认证 mid 各写一条 Outbox**：`NewWithDeps` 把 `officials` 预置为**空 map
   而非 nil**，构造期那次 `loadOfficial` 因此把每条生效认证都判成「新增」，逐条 `enqueueProfileUpdated`。
   进程重启即产生 N 条 `user.profile.updated`（account 侧 `DelCache` 幂等，无害但会瞬时推高 outbox）。
   参考仓库只在后台协程里首次装载、`origin==nil` 时静默返回。修法：构造期不预置 map，或首次装载
   跳过通知。用例 `TestColdStartSnapshotEnqueuesProfileUpdatedPerMid` 记录现状。
5. **一个坏 mid 会沉掉整批查询**：`BatchBaseInfo` 在 `FindMany` 失败时返回「已命中的部分 + 错误」，
   而 `Bases`/`Members` 的 logic 层见到错误就丢弃整个 reply（`TestBasesDropsPartialResultOnDBFailure`、
   `TestMembersPropagatesBaseFailure`）。与单条路径不对称：`Member` 遇经验读失败是**退化**
   （`TestMemberDegradesWhenExpFails`：LevelInfo 置零、资料照返），`Members` 遇 base 读失败是**全弃**。
   若要「部分可用」语义，应在 logic 层保留已命中的 bases 并单独标注失败原因。
6. **`Members` 无法区分「查无此人」与「查到了」**：单个 `Member` 对 mid=0 报 `ErrMemberNotExist`，
   批量则给缺失 mid 造一个 `Mid=0` 的占位条目、不报错、不裁剪（`TestMembersAggregatesInBaseThenExpOrder`）。
   批量结果长度也不等于请求长度（重复 mid 会被压成一个 key）。
7. **`BatchBaseInfo`/`exps` 都不做 mid 去重**：`[30201,30201,30201]` 会发出 3 次 `GetJSON`、
   `FindMany` 参数含 3 个重复值、回填 3 次同样的 key，而 map 里只有 1 条。同时 **100 上限是在去重前计数**，
   即 101 个重复 mid 会被 `ErrMemberOverLimit` 拒。见 `TestBasesKeepsDuplicateMidsAsOneEntry`。
8. **`ExpStat` 内部两套故障口径**：三个 `GetBit` 的错误直接上抛（整个请求失败），
   投币计数的 `GetInt` 却把故障吞成 0（表现为「今天没投币」）。见 `statCache`；
   用例 `TestExpStatBitFailurePropagatesWithoutDegrading` 与 `TestExpStatCoinStaysZeroWhenOnlyCoinMissing`
   分别钉住两半。统计类读接口更一致的做法是全部降级为零值。
9. **`Level` 传给 `BuildLevel` 的 `sexp=false` 在调用点上没有行为差异**：`Level` 返回值里
   `NowExp` 被显式写成 `0`，第三个返回值丢进 `_`。第 1 轮变异探针把它翻成 `true` 用例全绿——
   这不是覆盖不够，而是该参数在此路径确为冗余（同时 `NowExp: 0` 是冗余的第二次置零）。
10. **负经验会算出负 `nowExp`**：`exp/ExpMulti` 向零截断，`-150` 变 `-1` 级内值，落在 0 级
    但 `Exp.NowExp=-1`。边界表 `TestExpAndLevelLevelBoundaries` 含 `-99/-150/-1000000` 三行，
    固化的是**当前截断语义**。写侧 `SetExp` 已在第 2 轮把「负 count 不校验、原样下传 model」
    钉成 `TestSetExpNegativeCountIsNotGuarded`，与本条同源（DDL 是 `BIGINT UNSIGNED`，真库会 1264 越界）。
11. **`model/*.go` 的 SQL 与列名仍无单测**，仓库里也没有 user-profile 的迁移↔model 列级对账门禁；
    本轮替身只复刻 SQL 的**语义**（`status=0`、7 天窗口、`ORDER BY ts DESC, id DESC`、`LIMIT 1000`、
    唯一键、`RowsAffected`），不证明语句本身。改表时按 docs/commands.md 在隔离实例复验。
12. **实名轮的 PEM 地雷已解除**（原登记项，现记录为已处置）：`fakes_test.go` 里那两个结构占位
    常量已删除，改为 `fakes_test.go:1679` 的 `testPubPEM, testPrivPEM = mustTestCardKeyPEM()`
    ——包初始化时真的生成 RSA-2048（PKIX 公钥 + PKCS#1 私钥）。今后若有人把测试密钥改回字面量
    PEM，「解密失败」分支会被误读成业务结论，见 `realnameapplylogic_test.go:469` 的提醒。

### 第 4 轮（审核 / 官方认证 / 撤销 / 实名发码）登记

13. **认证附加资料全程明文存储、明文出口**（隐私红线，改生产代码前不要动这两组断言的期望）：
    联系人、联系电话、邮箱、地址、统一社会信用代码、营业执照、身份证明经
    `model.OfficialExtra.String()`（`model/official.go:125-131`，只有 `json.Marshal`，无脱敏无加密）
    原样写进 `user_official_doc.extra`（`TEXT NOT NULL`，见
    `deploy/migrations/user-profile/000005_create_user_official.sql:64`），读侧
    `Repository.OfficialDoc` 再逐字段端出去（`internal/repository/official.go:81-95`）。
    同一份信用代码还会第二次落进键值表 `user_official_doc_addit`（`official.go:54-58`）。
    用例：`TestSetOfficialDocWritesPlaintextExtraAndForcesPendingState`、
    `TestSetOfficialDocCreditCodeDualWrite`、`TestOfficialDocProjectsEveryFieldFromPlaintextExtra`。
    修法方向：附加资料按字段分级加密或脱敏存储，`OfficialDoc` 出口只回摘要；
    与 `AddPropertyReview` 的带令牌 URL 是同一类问题（见缺口 18）。
14. **一次性实名验证码明文进 INFO 日志**（本批最高价值隐私结论）：
    `internal/repository/realname.go:339` 的
    `logx.Infof("... send capture mid=%d code=%06d ...", mid, capture)` 把 `:337`
    `rand.Intn(900000)+100000` 生成的 6 位码打进 INFO 级日志。该码在应答里没有、MySQL 里没有，
    唯一藏身处是 `realname_cap_code_<mid>`（TTL 600s），于是任何有日志读权限的人
    （含采集链路与保留期内的所有下游）都能凭 mid 直接通过实名手机第二步校验。
    用例：`TestRealnameTelCaptureCodeIsLoggedInPlaintext`（⚠ 改生产代码前不要动期望）。
    修法方向：日志只留 mid 与「已下发」事实或摘要；notification 真实下发接入后删除该行开发态日志。
15. **撤销台账这条设计从未生效过**：`internal/repository/moral.go:208-217` 用
    `LogID: useLog.LogID`（`:212`，与被撤销的原行同一个 id）再插一条 `content.status=已撤销` 的行，
    而 `member_log` 有 `UNIQUE KEY uk_log_type_log_id (log_type, log_id)`
    （`deploy/migrations/user-profile/000009_create_member_log.sql:46`），
    因此每次撤销必然 1062，且错误只进 `logx.Errorf`（`:216`）不上抛、接口照样返回成功。
    后果：库里查不到「谁在什么时候撤销了这条」的独立记录，撤销人只活在反向变更那条台账的
    `operater` 字段里，而那个字段是调用方自报的（见缺口 21）。
    用例：`TestUndoMoralRevokedAuditRowNeverLands`（⚠ 同上）。
    修法方向：为撤销生成新 `log_id`，并在 content 里加一列指回原 `log_id`。
16. **撤销链路有四条互相放大的口径问题**：
    (a) **不幂等**——`FindByLogID` 不按 `status` 过滤（`model/log.go:119-127`），
    `UndoMoral` 也不看 `useLog.Status`，同一个 `log_id` 撤销 N 次就退回 N 次（直到上限钳制）；
    (b) `MarkRevoked` 失败被吞（`moral.go:199-201`）后，原行仍是有效状态、反向变更又新写一条有效
    记录，用户台账里同时躺着「扣 1000」和「退 1000」两条都算数的记录，且缺口 (a) 被放大；
    (c) **content 解析失败留下永久半状态**——`moral.go:219-230` 直接 `ParseInt` 三个键，
    失败时原行状态**已经改完**（`status=1` 被读侧过滤、看不到）而 `UpdateMoral` 没跑（退不回），
    重试还是同一个错误；
    (d) **撤销语义是反向 delta 重放到当前值**，不是恢复当年的 `to_moral`
    （`moral.go:239` `Delta = fromMoral - toMoral`），中间发生过别的变更时撤销后拿不到原值。
    另有一条：撤销一个节操行已被清掉的用户，`updateMoralTx` 见 nil 就 `TxInit(7000)`
    （`moral.go:257-261`，`model.DefaultMoral=7000`），凭空造出一行且与历史真实值无关。
    用例：`TestUndoMoralRepeatIsNotIdempotent`、`TestUndoMoralMarkRevokedFailureLeavesTwoActiveRows`、
    `TestUndoMoralContentParseFailuresLeaveHalfState`、`TestUndoMoralReplaysReverseDeltaOnCurrentValue`、
    `TestUndoMoralCreatesMoralRowForUserWithoutOne`。
    修法方向：撤销走 CAS（`WHERE status=0`）+ 新生成 log_id；content 解析前先校验结构；
    `MarkRevoked` 与反向变更同事务失败即回滚。
17. **`SetOfficialDoc` 的三处静默语义**：
    (a) `Role`/`Realname` 由 `int32` 直接截成 `int8`（`official.go:23`、`:27`），越界值不是被拒而是
    **换成另一个值**通过——259 会变成合法的「企业认证」；
    (b) UPSERT 列集（`model/official.go:204-208`）**不含 `reject_reason`**，所以驳回原因跨次提交活下来，
    而 `extra` 是**整列替换**，第二次提交没带的字段静默消失（无字段级合并）——同一行两列命运相反；
    (c) 入参 `state` 一律作废、强制 `OfficialStateWait`（`official.go:22`），
    附加表写失败被 `logx.Errorf` 吞（`:55-58`）、主表写失败换成 `ErrSubmitOfficialDocFailed`
    上抛（`:50-53`，原始 SQL 错误只剩日志，调用方无法分辨故障种类）；
    全程不开事务、不发领域事件、不失效缓存（提交 ≠ 生效，生效件在 `user_official` 另路写），
    审核方只能靠轮询主表发现新单。
    用例：`TestSetOfficialDocGuardTable`、`TestSetOfficialDocRoleAndRealnameTruncateToInt8`、
    `TestSetOfficialDocPreservesRejectReasonButReplacesExtra`、`TestSetOfficialDocDownstreamFailures`、
    `TestSetOfficialDocTouchesNothingElse`。
18. **`AddPropertyReview` 的 old/new 口径不对称 + 归档只认 `state=0`**：
    (a) 头像的旧值经 `facePath`（`internal/repository/util.go:71-77`，只取 `u.Path`）归一成 URL path，
    域名与 query 全丢，新值却是调用方传什么存什么——于是同一行两列口径不同，
    且带 `?auth_key=` 的短期签名 URL **永久留在** `user_property_review.new`；
    (b) `State`/`Property` 同样是 `int8` 直截（`internal/logic/addpropertyreviewlogic.go:43-44`），
    property=259 被判成「昵称」并顺利过白名单；
    (c) 守卫位置不对称：`extra` 的 JSON 闸门在触库之前（`addpropertyreviewlogic.go:33-39`，
    且返回的是 `encoding/json` 原生错误、**没有**换成 `ErrRequestErr`），
    property 白名单却坐在 `base.FindOne` 之后（`official.go:136` → `:141-156`）；
    (d) 归档语句是 `UPDATE ... WHERE mid=? AND property=? AND state = 0`
    （`model/property.go:103-107`），提交非 0 状态时命不中上一条待审行，
    而表上只有 `PRIMARY KEY(id)` 与 `idx_mid_property`、**没有唯一键**
    （`deploy/migrations/user-profile/000007_create_user_property_review.sql:51-53`），
    于是重复提交就是多条同态待审行、行数只增不减，接口一路返回成功；
    `InMonitor` 与 `Archive` 的失败被吞（`official.go:157-163`）——归档失败即「攒出两条待审」，
    监控读失败即「受监控用户的变更被记成非监控」，两者都不会让调用方重试。
    用例：`TestAddPropertyReviewNewFaceKeepsSignedQuery`（现状哨兵：new 侧一旦也归一成 path 本用例必须变红）、
    `TestAddPropertyReviewStateAndPropertyTruncateToInt8`、
    `TestAddPropertyReviewUnknownPropertyGuardRunsAfterDBRead`、
    `TestAddPropertyReviewExtraGuardIsBeforeAnyDependency`、
    `TestAddPropertyReviewRepeatSubmission`、`TestAddPropertyReviewNonPendingStateNeverArchived`、
    `TestAddPropertyReviewSwallowedFailures`、`TestAddPropertyReviewSoftDeletedMonitorCountsAsFree`。
19. **发码链路的写侧故障与配额口径**：
    (a) `setCaptureCode`/`setCaptureTimes` 走的是**无返回值**的 `Cacher.SetInt`
    （`internal/repository/cache.go:81`，封装见 `:229-232` 只能 `return nil`），
    Redis 写不进去时接口仍报成功、库里根本没有码，用户永远过不了校验且服务端无任何错误信号；
    `Incr`/`Del` 的错误却原样上抛（`realname.go:343`、`:346`），此时码其实已经在缓存里活着；
    (b) 配额判定是 `times > 5`（`:334`），24 小时内放行的是**第 1..6 次**、第 7 次才拒，
    被拒时一次写都不做（连错误计数都不清、不覆盖已下发的码）；
    (c) 计数为负（手改/半写）时**先归零再照常发码**（`:328-333`），负值不会锁死反而被洗白成 0 起步；
    (d) 每次发码都把「验证码错误次数」清零（`:346`）——重复要码等于给爆破窗口续期
    （发码 6 次/24h × 每轮可再猜的错码次数）。
    用例：`TestRealnameTelCaptureSwallowsCodeWriteFailure`、`TestRealnameTelCaptureSendQuotaBoundary`、
    `TestRealnameTelCaptureNegativeCounterIsResetThenSends`、
    `TestRealnameTelCaptureRepeatOverwritesCodeAndResetsBruteForceCounter`、
    `TestRealnameTelCaptureColdStartSequence`、`TestRealnameTelCaptureDownstreamErrorsPropagateVerbatim`。
20. **`RealnameTelCapture` 与 `OfficialDoc` 都没有 mid 守卫，且认证提交件没有缓存层**：
    发码侧 logic 与 repository 都不查 mid（`realname.go:323-350` 全程只按 mid 拼键），
    也不查会员是否存在，于是 `mid=0`/负数照样占一个发码桶并写出真码；
    校验侧 `RealnameTelCaptureCheck` 同样只认 mid，所以未登录/越权调用方可**为任意他人 mid**
    反复要码、把对方手里的码覆盖掉。读侧 `OfficialDoc`（`official.go:63-97`）与 `Base`/`Moral`
    不同，**一次缓存都不走**，每次调用一条 SELECT、重复调用不合并，`mid<=0` 也照查；
    `extra` 列被写坏时是**降级**不是错误（`model/official.go:134-140` `ParseExtra` 吞掉
    `Unmarshal` 错误），接口仍 200、16 个附加字段全空，调用方无法与「用户没填」区分。
    用例：`TestRealnameTelCaptureHasNoMidGuard`、`TestUndoMoralNoInputGuardAndNoStoreTouch`、
    `TestOfficialDocHasNoCacheAndNoMidGuard`、`TestOfficialDocMissingRowAndBadExtra`、
    `TestAddPropertyReviewMissingBaseAndNonPositiveMid`。
21. **审计里的「谁」和「从哪来」都由调用方自报**：`UndoMoral(logID, remark, operator)` 只有
    三句自由文本入参（无幂等键、无权限入参），`operator`/`remark` 原样进 content 并经
    `toUserLogReplies` 的 `Content` 出口端给调用方（`internal/repository/util.go:50`）；
    反向变更台账里的 IP 抄的是**被撤销那次变更**的 `useLog.IP`（`moral.go:234`），
    不是撤销操作者的 IP。加上缺口 15（独立撤销记录永远写不进库），
    AGENTS.md §8「删除/下架保留审计证据」在本服务这条链路上既可留空也可伪造。
    用例：`TestUndoMoralRevokedAuditRowNeverLands`、`TestUndoMoralPropagatesTransactionFailures`。
    修法方向：与 gateway/admin 的 `adminActorGate` 同一做法，由网关按会话渲染 operator
    并在 service 侧要求可信调用方。

## 移植边界与已知事项

- **HTTP 路由不移植**：参考仓库 `/x/internal/member/*` 全部以 gRPC 等价暴露；
  需要对外 HTTP 形态时由 gateway/app、gateway/admin 做 BFF 聚合。
- **block 子模块不移植**：BlockInfo/BlockBatchInfo/BlockBatchDetail 与封禁表
  （block_user 等）属于 risk-control 域，由该服务后续承接。
- **支付宝实名渠道不移植**：参考仓库 `realname_alipay_apply` 仅经 gorpc 暴露，
  本项目 gRPC 契约不包含，待需求明确后评审。
- **SMS 下发待接入**：实名验证码当前写入 Redis 并记录日志（开发联调），
  notification 服务落地后替换为真实短信下发。
- **分表策略**：参考仓库 user_base/user_exp 按 mid%100 分表，本项目起步单表；
  需分表时仅切换 model 层表名，字段与索引不变。
- **生成器说明**：goctl 1.10.2 对 source_relative + 自定义 go_package 别名布局
  生成的 import 别名缺失（`rpc_userprofilev1` 引用无别名 import），属已知生成器
  问题；`scripts/gen.ps1` 已内置归一化步骤（别名替换、删除重复入口与客户端
  包装目录），生成后请勿手工改动 server 文件。
- **Outbox 发布器为单实例轮询**：多实例部署时需替换为独立发布进程或引入租约，
  避免重复投递（account 侧按 event_id 幂等，重复投递无害）。
- **非事务事件写是「尽力投递」**：`Repository.enqueueProfileUpdated`（官方认证快照变化的后台任务用）
  自己开一个只含 Outbox 行的事务，失败只 `logx.Errorf` 不向上抛——与 `enqueueProfileUpdatedTx`
  的「业务写 + 事件同事务」不同，这里没有业务行需要保护，但**事件确实可能丢**。
  当前调用点无补偿队列；若认证快照要成为强一致事实，应改成失败落错误台账并由清扫任务重放。
