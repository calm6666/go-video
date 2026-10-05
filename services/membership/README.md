# membership

会员身份、套餐（SKU）与**权益判定**服务。全站只有这里能回答「这个用户现在到底有没有某项会员权益」。

- **拥有数据**：套餐目录与标价、套餐变更台账、权益码目录、会员身份（到期时间）、授予/变更台账、写请求幂等台账（库 `go_video_membership`，表前缀 `mb_`）。
- **提供能力**：16 个 RPC（`services/membership/rpc/membership.proto`，`membership.v1`）——套餐读 5 个（`ListPlans`/`GetPlan`/`UpsertPlan`/`SetPlanState`/`ListPlansAdmin`）、会员读 3 个（`GetMembership`/`CheckEntitlement`/`CheckEntitlements`）、会员写 3 个（`GrantMembership`/`RevokeMembership`/`SetAutoRenew`）、台账与批处理 3 个（`ListGrants`/`ListExpiringMemberships`/`ExpireMembership`）、权益码 2 个（`ListEntitlements`/`UpsertEntitlement`）。
- **对外契约**：etcd 注册键 `membership.v1.rpc`（**不带连字符**，与 `Name` 逐字相同），监听 `0.0.0.0:8160`。
- **依赖**：MySQL（硬依赖——判定必须落库）、Redis（配置已就位，**本轮读缓存未实现**，见 §7）。**不依赖 MQ、不依赖任何下游 RPC、不提供 HTTP**。
- **上下游**：`trade-order` 履约时调 `GrantMembership`/`RevokeMembership`；`playback`、`gateway/app`、`gateway/admin` 只读 `CheckEntitlement(s)`/`GetMembership`/`ListPlans`；`services/cron` 调 `ListExpiringMemberships` + `ExpireMembership`。
- **owner**：后端-商业化小组（本轮由子 agent 落地，正式 owner 待登记到 `docs/service-catalog.md`）。

## 1. 职责边界与资金语义（AGENTS.md §1 2026-09-22 修订）

本服务**不持有资金**，也不接任何真实支付渠道：

| 事项 | 事实源 | 本服务的角色 |
|---|---|---|
| 收多少钱、钱在哪个台账 | `payment`（沙箱台账） | 不参与，不存凭据，不配渠道密钥/回调 |
| 订单状态、待支付/已支付 | `trade-order` | 不参与，只在其履约回调时收 `biz_order_no`/`payment_no` **字符串引用** |
| 卖什么、多少钱、哪些端可见 | **本服务 `mb_plan`** | 唯一写入口（标价以「分」计的整数，全程无浮点） |
| 某用户此刻是不是会员 | **本服务 `mb_membership` + `mb_grant`** | 唯一写入口 |
| 某权益码代表什么能力、要哪档 | **本服务 `mb_entitlement`** | 唯一写入口 |

因此 `CheckEntitlement` 的 `granted=true` 只有一个来源：**`mb_membership` 里确实有一行 `expire_at > now` 且档位达标**，而那一行只能由 `GrantMembership` 的事务写入并在 `mb_grant` 留下台账。代码里没有任何默认值、没有「读不到就放行」或「读不到就算未开通」的分支——**DB 读失败一律上抛错误**，折叠成 `granted=false` 等于把故障伪装成「用户没买」。

禁止的越权写法：其他服务持有会员到期时间或自行判定权益；`account`/`gateway` 直读 `mb_*` 表；本服务把 `biz_order_no` 当外键去回查 `trade-order` 的库。

**数据所有权缺口（本轮未闭环）**：`account` 名片里的 `vip` 字段只是**投影**，事实源是 `mb_membership`。本轮**不发事件**（`api/events` 里没有会员变更 schema），所以该投影没有供数通道：要么由 `account` 侧读时调 `GetMembership`，要么下一轮补 outbox/MQ。当前状态是「投影存在但无人刷新」，禁止当成已交付（见 §7-2）。

记账/排障可用的不变式：

```text
mb_membership.expire_at(mid, vip_type) == 最后一次时长变更后的 after_expire_at
    （中间任何一次变更都能在 mb_grant 里按 (mid, vip_type, ctime) 倒序还原）
```

## 2. 表清单（`deploy/migrations/membership/000001_create_membership_tables.sql`）

| 表 | 作用 | 关键约束/索引 |
|---|---|---|
| `mb_plan` | 套餐（SKU）目录：档位、时长、标价、可见平台、售卖状态 | `UNIQUE(plan_code)`；`idx_state_vip_type`、`idx_state_plan_id`（终端/admin 列表）、`idx_vip_type` |
| `mb_plan_change_log` | 套餐新建/改字段/上下架的变更台账（append-only） | `UNIQUE(request_id)` 幂等锚点；`idx_plan_ctime` 反查某套餐变更史 |
| `mb_entitlement` | 权益码目录（`code` 跨服务稳定引用） | `UNIQUE(code)`；`idx_enabled_min_vip`；停用只置 `enabled=0` **不删行** |
| `mb_membership` | 会员身份：**唯一事实源**，一行 = 一个用户一个档位 | `UNIQUE(mid, vip_type)`；`idx_expire_at`、`idx_auto_renew_expire_at`（cron 扫描）。**无 `state` 列**（见 §4） |
| `mb_grant` | 授予/收回/到期台账（append-only，只 INSERT） | `UNIQUE(request_id)`；`idx_mid_ctime`、`idx_mid_vip_ctime`、`idx_biz_order_no`（退款反查）、`idx_source_ctime` |
| `mb_biz_request` | 写请求幂等台账：主键就是 `request_id` | 只服务 `SetAutoRenew`/`UpsertEntitlement`（见 §5） |

参与唯一性/精确匹配的列（`request_id`、`plan_code`、`code`、`action`、`change_type`、`api`、`subject`、`params_fingerprint`、`biz_order_no`、`payment_no`、`auto_renew_channel`、`currency`）一律**列级 `utf8mb4_bin`**；展示类文本（`name`/`description`/`reason`/`operator`）走表级 `utf8mb4_unicode_ci` 以便搜索。若唯一键落在大小写折叠的 collation 上，两个仅大小写不同的 `request_id` 会被判成同一个请求，幂等语义**静默失效**（表现为重复加时长或冲突检不出来）。

所有 `CREATE TABLE IF NOT EXISTS` 可重复执行，回滚是逆序 6 条 `DROP TABLE IF EXISTS`（写在文件头）。后续列/索引变更必须新开 `0000NN_*.sql`，禁止改本文件。

## 3. 判定口径（`CheckEntitlement` / `CheckEntitlements` / `GetMembership`）

`decideEntitlement`（`internal/logic/checkentitlementlogic.go`）是**唯一**判定实现，两个 Check 接口共用，保证同一个 `(mid, code)` 不会给出两种答案。

| 情形 | `granted` | `reason` | 附带返回 |
|---|---|---|---|
| `mid <= 0` | false | `ENTITLEMENT_MID_INVALID` | — （是**结论**不是错误：游客是正常业务态） |
| 码不在目录 / 空码 | false | `ENTITLEMENT_CODE_UNKNOWN` | — |
| 码已停用（`enabled=0`） | false | `ENTITLEMENT_CODE_DISABLED` | — （与「传错码」区分开：一个是本地关掉，一个是根本没这个码） |
| 无任何身份行 | false | `ENTITLEMENT_NO_MEMBERSHIP` | `expire_at=0` |
| 有身份行但 `expire_at <= now` | false | `ENTITLEMENT_EXPIRED` | 如实带回 `expire_at` 与 `vip_type` |
| 生效中且档位达标 | **true** | `ENTITLEMENT_GRANTED` | `expire_at`、`vip_type` |
| 生效中但档位不足 | false | `ENTITLEMENT_TIER_NOT_ENOUGH` | 实际持有档位与 `expire_at`（引导升级，不是引导开通） |

- **档位序为真**：`PREMIUM(1) < PREMIUM_PLUS(2)`，超级大会员拥有大会员的全部权益（`model.TierSufficient` 用 `>=`，不是「码上写的档位数相等」）。
- `CheckEntitlements` 单次最多 100 个码（`maxCodesPerCheck`，超出直接 `ErrInvalidQueryFilter` 而不是悄悄截断），**一次读身份 + 一次去重后的 `IN` 批量读码**，不做 N+1 查询；`decisions` 与入参顺序一一对应（重复的码重复回答）。
- `GetMembership` 的 `found` 只表示「有没有身份行」，**过期行照样返回**（配合 `server_now` 由调用方判过期）；`granted_entitlements` 仅在身份生效中时给出，是只读投影不额外授权；判定行按「生效中的最高档，全都过期则退回曾达到的最高档」挑，指定 `vip_type` 时只看那一档。目录读失败会降级成空投影且应答里看不出来（§7-11）。
- `expire_at == now` 视为**已过期**（`IsActive` 用 `>`），与 cron 的到期口径一致。

## 4. 为什么 `mb_membership` 没有 `state` 列

`expire_at` 就是状态：`expire_at > now` 为有效。再存一份 `state` 就会产生第二个事实源，而**没有任何写入方会保证它与时间同步**（没人每秒扫一遍改标志位），结果是「`state=ACTIVE` 但早已过期」这类无法自愈的漂移，判定读哪个都没法自证。因此：

- 需求里建议的 `(state, expire_at)` 索引在本表**不成立**，改为提供 `idx_expire_at(expire_at)` 与 `idx_auto_renew_expire_at(auto_renew, expire_at)`，正好覆盖 `ListExpiringMemberships` 的闭区间扫描与续费批次；
- `ExpireMembership` 因此**不改 `mb_membership` 的任何字段**，只补一条 `mb_grant.action=EXPIRE` 台账（`delta_days=0`、`before_expire_at == after_expire_at == expire_at`、`source=0`——`GrantSource` 的 1..5 全是「授予渠道」，到期不属于任何一种，填别的值就是造假）。改写 `expire_at` 会抹掉「到期于何时」，续期基准也就没了。

## 5. 幂等、状态机与写入口径

**幂等（三类锚点，全部是 DB 唯一索引，Redis 不承担幂等职责）**

- `mb_grant.request_id UNIQUE`：`GrantMembership`/`RevokeMembership`/`ExpireMembership`。
- `mb_biz_request.request_id PRIMARY KEY` + `params_fingerprint`（sha256）：`SetAutoRenew`、`UpsertEntitlement`。为什么要另开一张表——`mb_grant.action` 被契约钉死在 `GRANT/EXTEND/REVOKE/EXPIRE` 四种**时长变更**，这两类不动时长，硬塞进去就会造出一个契约里不存在的动作。
- `mb_plan_change_log.request_id UNIQUE`：`UpsertPlan`、`SetPlanState`。

统一流程：**先按 `request_id` 回查（快路径）→ 命中则比对关键参数 → 一致才返回首次结论并置 `duplicated=true`，不一致返回 `ErrRequestIdReused`**（幂等键只用于重试，不允许借它改口径）；事务内再靠唯一索引挡并发，冲突后重查区分「同键重放」与「业务标识撞车」（例：`UpsertPlan` 要分辨撞的是 `request_id` 还是 `plan_code`）。**重放检测必须排在状态机校验之前**，否则「上架成功后重试」会被看成 `ON_SALE → ON_SALE` 的非法迁移。

**到期时间换算只有一个函数**：`model.NextExpireAt(current, deltaSeconds, now)`——未过期在**原 `expire_at`** 上顺延，已过期从 `now` 重新起算，绝不从旧的过期时刻往回补（那样会让断掉两年的用户只补回一天）。收回的下限是 `now`（`revokeResult` 夹底），不制造「早于今天的负余额」——那会让下一次 `NextExpireAt` 从错误的过去时刻起算，等于白送时长。

**`mb_grant.delta_days` 记的是「请求意图」的带符号天数**（授予为正、收回为负、`clear_remaining` 为 0），真实影响一律看 `before_expire_at`/`after_expire_at`。`paid_month_count` **只增不减**：它是「历史上真实付过多少个月」的单调快照，退款回收时长不该篡改付费事实。

**套餐状态机**（`SetPlanState`，其余迁移一律 `ErrInvalidPlanStateTransition`，含「同状态再上一次」）：

```text
DRAFT → ON_SALE    ON_SALE → OFF_SALE    OFF_SALE → ON_SALE
```

- `UpsertPlan` 新建恒为 `DRAFT`；离开 `DRAFT` 之后 `vip_type`/`duration_days`/`unit_count`/价格/币种**不可改**（`ErrPlanSpecImmutable`）——改口径必须新建草稿再切换，否则改完的价格会对已下单用户追溯生效。
- `ListPlans` 终端面**恒定只出 `ON_SALE`**，`on_sale_only=false` 不放宽过滤（避免运营误勾一个参数把草稿挂上收银台）；看草稿/已下架必须走 `ListPlansAdmin`。
- 上下架与运营手工授予/体验发放/收回**必须带 `reason`**；`reason` 只写摘要，禁止凭据、手机号等 PII。日志同理：只打 `mid`/`vip_type`/`grant_id`/`request_id` 与错误，不打 reason 文本、不打 DSN。
- 乐观锁：`version` 列 CAS，不匹配返回 `ErrConcurrentUpdate`，由调用方重读重试，**不覆盖并发写入**。
- `GrantMembership` 是唯一加时长的通道：付费来源（沙箱购买/自动续费/存量迁移）必须带 `biz_order_no` 或 `payment_no`（`ErrGrantSourceNeedsOrder`）；挂套餐的授予必须与套餐档位一致且套餐非 `DRAFT`；单次 `|delta_days| <= MaxGrantDeltaDays`，越界报错不裁剪。
- `SetAutoRenew` 只翻意愿位：渠道**逐字节**只接受 `SANDBOX`（`ErrAutoRenewChannelRejected`），真实代扣协议一概不落库——记一个永不生效却会被续费 cron 当真的协议位比拒绝更危险；已过期身份不允许签约（`ErrAutoRenewUnsupported`），否则等于给 cron 留一个「凭空复活会员」的入口。
- `ExpireMembership` 的 `skipped=true` 覆盖三种情形：未到期、同一 `expire_at` 已记过 EXPIRE（`Grant.FindAction` 去重，挡住 cron 换 `request_id` 的重扫）、同一 `request_id` 重放。身份行不存在返回错误而不是 `skipped`——任务表扫到不存在的用户属于数据漂移，静默跳过会把它藏起来。
- `ListExpiringMemberships` 游标只有秒级精度，返回条数不足 `limit` 时游标回 0 表示区间已扫完。
  「同秒跨批只会重复投喂几条、宁重不漏」这个说法**只在同秒行数 < `limit` 时成立**：同一秒的到期行数
  达到 `limit` 时游标会原地不动，cron 反复拿到同一批、该秒之后的到期行永远扫不到——已由
  `TestListExpiringMembershipsCursorCannotAdvanceWithinSameSecond` 钉住，详见 §7-9。
- 分页超限**裁剪并在 reply 回显实际 `page`/`size`**（`ListGrants`/`ListPlansAdmin` 的契约带了这三个字段），不返回 500；
  `ListGrants` 的 `mid=0` 才有跨用户（admin）语义。注意 `ListPlans`/`ListEntitlements` 契约**没有**分页字段，
  后者在 model 层被写死 `LIMIT 500` 静默截断（§7-10）。

## 6. 配置

`services/membership/etc/membership.v1.yaml`（键名与 `internal/config/config.go` 一一对应，由 `internal/config/config_load_test.go` 双向锁死，含「yaml 每个顶层键都要有字段、`Membership` 段不得漏写键」的自检）：

```yaml
Name: membership.v1.rpc
ListenOn: 0.0.0.0:8160
Etcd: { Hosts: [127.0.0.1:2379], Key: membership.v1.rpc }   # 注册键不带连字符
Log: { ServiceName: membership.v1.rpc, Mode: console, Level: info }
CacheRedis: { Host: 127.0.0.1:6379, Type: node }             # 必须叫 CacheRedis，叫 Redis 会 conflict key redis
DataSource: root:root@tcp(127.0.0.1:3306)/go_video_membership?charset=utf8mb4&parseTime=true&loc=Local
Membership:
  MaxGrantDeltaDays: 3660        # 单次授予/收回上限（天），越界 InvalidArgument 不裁剪
  ExpireScanMaxLimit: 500        # 到期扫描单批上限，超限裁剪
  MaxPageSize: 100               # admin 面分页上限
  DefaultPageSize: 20
  DefaultCurrency: CNY           # 本轮唯一放开币种（价格单位：分）
  MembershipCacheTTLSeconds: 10  # 0 表示关闭缓存；本轮缓存层未实现（§7-1）
```

必须守住的细节：

- **不得加 `clientFoundRows=true`**：CAS 与幂等全部依赖 `RowsAffected = 实际变更行数`，改成匹配行数会让「版本冲突」「重复请求」判定整体失真。
- `MembershipCacheTTLSeconds` 的约束是**语义级**的：缓存只允许加速「已落库的结论」，miss 或 Redis 故障必须回源 DB，绝不允许把「缓存不可用」折叠成 `granted=false`。
- 生产 DSN 走配置中心/Secret，`etc/` 里只有本地示例值；测试会拒绝疑似明文凭据。

## 7. 已知缺口（尚未实现，禁止当成已交付）

1. **读缓存未实现**：`CacheRedis` 与 `MembershipCacheTTLSeconds` 已在配置与 `ServiceContext.Cache` 就位，但**没有任何读路径使用它**（`svcCtx.Cache` 当前无引用）。因此「缓存不可用必须回落 DB」这条不变式本轮是**平凡成立**的（每次判定都读 DB），但也意味着 `CheckEntitlement` 是纯 DB 读，播放详情页 QPS 打满时需要下一轮补缓存 + 主动失效。
2. **会员变更不发事件**：没有 outbox、没有 MQ producer。`account` 名片的 `vip` 投影、`creator-revenue` 的会员分成口径、`recommend` 的特征都拿不到增量。接线要先在 `api/events` 定 schema（含 `event_id` 去重口径），不能由本服务私自定义载荷。
3. **无 HTTP 面**：领域服务只暴露 gRPC（AGENTS.md §3/§4），终端与 admin 路由由 `gateway/app`、`gateway/admin` 聚合。
   `gateway/app` 的商业化终端路由已接线；`gateway/admin` 的 10 条会员运营路由契约、logic 与权限点均已接完
   （2026-09-22 `.api` 补丁轮补上了 `reason` 与 `plan_id`/`biz_order_no`/`payment_no` 追溯位，
   台账因此能区分「退款回收」与「运营纠错」），逐项口径以 `gateway/admin/README.md` 为准。
4. **logic 单测已补齐（2026-09-22）**：`internal/logic` 下 7 个测试文件、136 个顶层用例 + 56 个子用例
   （`validation_test.go` 纯函数表、`plan_flow_test.go` 运营面写接口、`membership_flow_test.go` 授予/回收/到期、
   `entitlement_check_test.go` 判定与批量、`catalog_read_test.go` 套餐/权益目录读侧 5 个方法、
   `membership_read_test.go` 会员读侧 3 个方法、`fakes_test.go` 内存版 model 替身 + 假事务），
   用 `model.NowUnix` 的真实时钟加偏移构造边界，不连数据库，`t.Skip` 为 0。
   读侧这一轮为 7 个原本无引用的 constructor（`GetMembership`/`GetPlan`/`ListPlans`/`ListPlansAdmin`/
   `ListEntitlements`/`ListGrants`/`ListExpiringMemberships`）补了逐个字段投影、守卫先于触库（靠替身的
   逐方法调用计数与入参快照证明，不看日志）、依赖错误原样上抛、以及「空目录 ≠ 读失败」的区分，
   并为此在 `fakes_test.go` 里按真实 SQL 口径复刻了 `ListOnSale`/`ListAdmin`/`List`/`ListExpiring`/`List`
   五个读方法（含闭区间、排序键与 `LIMIT` 截断，刻意保留截断好让 §7-10 暴露出来）。
   逐文件用例数、注入手法与断言口径见 §9「测试覆盖」。
   这一轮同时暴露并修掉了三处 logic 缺陷（不是测试缺陷）：
   - `UpsertPlan`/`UpsertEntitlement` 原本把 CAS 预检放在幂等预读**之前**，导致「首建超时后原样重试」被判成并发冲突、
     调用方分不清「我的写失败了」和「别人也在写」；现在统一成与 `SetPlanState` 一致的「幂等重放优先」；
   - 同一个 1062 有两个来源（`mb_biz_request` 主键 / `mb_entitlement.uniq_code`），原判定的 `switch` 永远先命中前者，
     并发建同码会把裸 1062 直接抛给调用方；现在按「幂等键到底记上没有」区分；
   - `UpsertPlan` 带了 `plan_id` 而 `plan_code` 是新码时一律回 `ErrPlanNotFound`，掩盖了「两个标识指向两行」这个真实成因；
     现在先回查那行，存在则给 `ErrPlanIdentifierMismatch`。
   仍未覆盖的是真库集成（唯一索引/`RowsAffected` 语义只有连 MySQL 才算验证）；
   `deploy/migrations/membership` 与 model 的列级漂移门禁已由 `model/migration_parity_test.go` 承载（2026-09-22 交付）：
   27 个门禁函数 / 15 个子用例，比对 6 表 79 列的类型、列序、15 个 `utf8mb4_bin` 字符列、14 个逐值枚举列、
   列宽 vs `guard.go` 常量、CAS 写路径的 `AND version = ?`、只追加台账禁 `mtime`/`version`，
   并用「把整数列登记进 binary 清单即 `Fatalf`」挡住了 payment 那轮踩过的假失败；
   变异验证：删掉一处 `COLLATE utf8mb4_bin` 会让三条门禁同时发红（主门禁 + 解析器下限自检）。
   该轮顺带就地补了 9 处**注释级**口径（`mb_plan_change_log.from_state`/`to_state`/`operator`、
   `mb_membership.auto_renew`/`source`、`mb_grant.vip_type`/`source`、`mb_biz_request.vip_type`、头部目标库声明），
   列类型/索引/列名未动，复验与 comment-drift 说明见 `deploy/migrations/README.md`「当前覆盖」。
   另需知道：本迁移**不写 `CHECK` 约束**（与 payment 不同，属刻意的口径差异而非疏漏），
   因此「`version` 单调」「`expire_at > 0` 才有结论」这类约束只有 model 与 logic 层在守。
5. **迁移复验**：`deploy/migrations/membership/000001_*.sql` 已于 2026-09-22 在隔离实例（`127.0.0.1:3399`）
   `up` + `status` 跑通，`go_video_membership` 里 6 张业务表建成（外加 `migrate.ps1` 自己的 `schema_migrations`），
   `mb_grant` 的 `plan_id`/`biz_order_no`/`payment_no`
   三列已按 §7-6 落地核对。真实/共享实例执行仍需显式给 host/port 与来自 Secret 的账号（`docs/commands.md` §8）。
6. **契约缺口**：本轮主 agent 已把四项补进 `.proto` 并接上实现（`./scripts/gen.ps1 -Service membership` 可复现生成物）：
   - `UpsertPlanReq.reason`（字段 16）→ 落 `mb_plan_change_log.reason`，留空才回落到 `upsertPlanLedgerReason` 摘要；`reason` 不进幂等指纹（补理由不该被判成换规格）；
   - `RevokeMembershipReq.plan_id`/`biz_order_no`/`payment_no`（字段 8/9/10）→ 落 `mb_grant` 同名列，退款回收与运营纠错因此可区分，`ListGrants` 也能按单号查回收回记录；三个字段全空仍是 `source=ADMIN_OPS` 的手工收回；
   - `EntitlementReason.ENTITLEMENT_TIER_NOT_ENOUGH`（7）→ 见 §3 结论表；
   - `ExpireMembershipReq.reason`（字段 5）→ 由 cron 任务写明「哪次任务判的到期」，留空回落摘要。
   仍开放的两项：
   - `CheckEntitlementsReq` 没有条数上限字段，本轮由服务侧硬编码 `maxCodesPerCheck=100`；
   - `GrantInfo.action` 只有四种时长动作，签约/权益码变更类写请求无处记账，因此新增了 `mb_biz_request`（不改 proto 的前提下唯一诚实的做法）。
   另：`GrantSource` 没有「订单退款」取值，收回的订单归属只由上面三个引用字段表达，运营若按 `source` 统计退款回收量会全部落在 `ADMIN_OPS`。
7. **台账无保留期/归档**：`mb_grant`、`mb_plan_change_log`、`mb_biz_request` 只增不删。`mb_biz_request` 的保留窗口**必须长于上游重试窗口**（否则重试会在窗口外二次生效），归档任务需 `services/cron` 落地。
8. **平台可见性只有套餐层**：`platform_mask` 控制「哪端能买到」，但 `CheckEntitlement` 不看平台（跨端通用），也没有「某端单独限售某权益」的口径。需要时得新增权益维度的平台位而不是在判定里写死。
9. **到期扫描游标在同秒内无法前进（活锁/漏到期）**：`internal/logic/listexpiringmembershipslogic.go:59-61`
   把游标设成本批最后一条的 `expire_at`，而 `model/membership.go:149` 的区间是
   `expire_at BETWEEN ? AND ?`（闭区间，下一批仍含该秒），排序键是 `(expire_at, membership_id)`
   （`model/membership.go:156`）。**同一秒的到期行数达到 `limit` 时游标原地不动**：cron 反复拿到同一批前两
   `limit` 行，同秒剩余行与该秒之后的到期行永远扫不到。契约（`rpc/membership.proto:341-344`）只有
   `next_expire_at_cursor`，`MembershipInfo` 也不带 `membership_id`，调用方无法自行按主键续扫。
   钉住用例：`TestListExpiringMembershipsCursorCannotAdvanceWithinSameSecond`（三行同秒 + `limit=2`，
   第二批与第一批逐行相同且游标不变）。修法方向：游标改复合 `(expire_at, membership_id)`（先改 `.proto` 再
   `./scripts/gen.ps1 -Service membership`），或 reply 回显本批末行 `membership_id` 与「区间内是否仍有同秒行」。
   注：`services/cron` 若因此反复拿到同一批，`ExpireMembership` 的 `request_id` 幂等 + 同 `expire_at`
   的 EXPIRE 台账去重会挡住重复记账，所以后果是**漏到期**而不是多扣。
   同一秒行数不超过 `limit` 时的「重复投喂几条」确实是刻意且无害的，但 `listexpiringmembershipslogic.go:34-36`
   把它写成「反过来若跳过同秒未处理的行就会漏掉真实到期。宁重不漏」——**该注释承诺的「不漏」在实现里不成立**
   （同秒行数 >= `limit` 时既没重复也没前进），§5 已按事实改写。
   另核对过：这 7 个读方法的注释里没有别处虚报鉴权/审计/幂等（`listgrantslogic.go:31-32` 明确写了本服务
   不猜调用方身份、跨用户读靠网关鉴权，属诚实的分工声明而不是空头承诺）。
10. **权益目录静默截断在 500 行**：`model/entitlement.go:176` 的 `List` 写死
    `ORDER BY min_vip_type ASC, entitlement_id ASC LIMIT 500`，而 `rpc/membership.proto:378-380` 的
    `ListEntitlementsReply` 只有 `entitlements` 一个字段（无 `total`/`page`/`size`），
    `internal/logic/listentitlementslogic.go:31,36` 又把行数原样转出——目录超过 500 个码时，
    运营面列表与 `GetMembership` 的 `granted_entitlements` 投影都会静默少给排序尾部的码，
    判定侧表现为 `CODE_UNKNOWN`（「这个码不存在」）而不是「目录被截断」。
    钉住用例：`TestListEntitlementsSilentlyTruncatesCodeAtSQLRowLimit`（seed 501 个码，只回 500 个，
    末位是 `vip.code_500`，且第 501 个确实在库里）。修法方向：契约加分页 + `total`（与
    `ListPlansAdminReply` 同口径），或至少回显 `truncated` 位。
11. **`GetMembership` 把权益目录读失败降级成空投影**：`internal/logic/getmembershiplogic.go:63-67`
    在 `Entitlement.List` 报错时只 `l.Errorf` 记日志，随后照常 `return reply, nil`，
    `GrantedEntitlements` 留 `nil`。失败方向是 fail-closed（不会凭空授权，比放行安全，与 §1 一致），
    但 `GetMembershipReply` 没有任何字段能区分「这一档确实没有可用码」与「目录读挂了」，
    详情页会把故障显示成「你没有任何权益」。**本条属「注释承诺与实现不一致」之外的降级类**，
    与 §7-1 的缓存不变式（不得把「读不到」折叠成「没有」）同源。
    钉住用例：`TestGetMembershipEntitlementProjectionFailureDegradesToEmpty`（含对照组：同一入参、
    故障撤掉后立刻投影出码，证明空列表来自被吞掉的故障）。修法方向：`GetMembershipReply` 加
    `entitlements_degraded` 位，或在 logic 里并入可重试错误（两者都是契约变更，需先改 `.proto`）。

## 8. 运行

```powershell
# 1) 迁移（见 docs/commands.md §8）
./scripts/migrate.ps1 -Action up -Service membership

# 2) 契约变更后重新生成 goctl 代码（.proto 是唯一来源，禁止手改生成物）
./scripts/gen.ps1 -Service membership
# 注意：本服务的 descriptor 路径必须是 services/membership/rpc/membership.proto（带目录），
# 裸名 membership.proto 会和 go.etcd.io/etcd/api/v3/membershippb 撞全局注册表，
# 任何同时链接 clientv3 与本 rpc 包的进程在 init 阶段 panic。
# gen.ps1 已内置这一步（见 $descriptorPrefixedProtos），不要改用裸名 protoc 手工生成。

# 3) 启动
go run ./services/membership -f services/membership/etc/membership.v1.yaml

# 4) 本服务门禁
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go build ./services/membership/... && go vet ./services/membership/...
gofmt -l services/membership   # 必须无输出
go test -p 1 -count=1 ./services/membership/...
```

## 9. 测试覆盖

离线单测（纯 Go 内存版 model 替身 + 假事务，不连 MySQL/Redis/etcd/MQ；§7-4 的注入手法说明就是本节的
方法论，此处只补可核对的数字与逐文件口径）。数字口径 `顶层/子用例`，由主代理用
`grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出。

### 1. logic 用例清单（`internal/logic`，7 个文件 `136/56`）

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **判定链（全站唯一「有没有权益」的答案来源，§1/§3）** | | | |
| `entitlement_check_test.go` | 19 | 4 | `granted=true` 只能来自库里那行 `expire_at > now`（`TestCheckEntitlementGrantedOnlyFromStoredRow`）；**读不动必须是 error 而不是 false**（`TestCheckEntitlementReadFailureIsErrorNotFalse`，本轮最关键一条）；档位不足回报实际档位与 `expire_at`（引导升级而非引导开通）；高档覆盖低档（档位序为真）；过期保留证据；生效低档压过过期高档；`mid<=0` 是结论不是错误；空码/未知码/停用码三者互不混淆且未知码优先于身份；码先 trim 再查；批量侧——`decisions` 与入参逐位对齐（重复码重复回答）、回显证据、超 100 个码拒绝而非截断、`mid` 非法对每个码都作答、任一读失败整批失败、全空白码不打目录、空码集不出结论，以及单条与批量永不给出两种答案（`TestSingleAndBatchNeverDisagree`） |
| **读侧（三种调用方各最贵的错误）** | | | |
| `membership_read_test.go` | 21 | 9 | `GetMembership` 每个字段都来自那一行、`server_now` 必须是服务端此刻、整条路径零写入且绝不改动身份行；`granted_entitlements` 是只读投影（过期身份一个码都不给）；挑行规则（生效最高档，全过期退回曾达最高档，指定 `vip_type` 只看那档）；未知用户 `found=false` 而非错误；守卫先于触库；身份读失败不等于「未开通」；目录读失败降级成空投影（§7-11 的钉住用例，含「故障撤掉立刻投影出码」的对照组）。`ListGrants` 倒序投影、过滤落到查询、`mid=0` 才有跨用户语义、非法枚举在查询前拒、分页裁剪并回显、读失败与空台账分家。`ListExpiringMemberships` 闭区间升序、满批才发游标、**同秒内游标无法前进**（§7-9 的活锁/漏到期）、超限裁 `limit` 不报错、`auto_renew_only` 过滤、区间守卫先于扫描、读失败上抛且零写入 |
| `catalog_read_test.go` | 18 | 9 | `ListPlans` 终端面恒定只出 `ON_SALE`（`on_sale_only=false` 不放宽）；平台枚举序数折成位掩码而不是当掩码用；`vip_type` 过滤与非法枚举守卫先于触库；读失败上抛。`GetPlan` 优先 `plan_id`、按 code 只走唯一索引路径、**不替调用方藏 DRAFT/OFF_SALE**、标识一致性在起事务前拒、读失败不等于 not found。`ListPlansAdmin` 带出草稿与已下架、过滤落到 model、分页裁剪回显、非法枚举拒、读失败上抛。`ListEntitlements` 保留 `enabled=0` 行（判定侧才分得出 `CODE_DISABLED` 与 `CODE_UNKNOWN`）、`enabled_only` 落到查询、空目录 ≠ 读失败、`LIMIT 500` 静默截断（§7-10 的钉住用例，seed 501 只回 500） |
| **写侧（§5 幂等与状态机的正面）** | | | |
| `membership_flow_test.go` | 29 | 15 | 授予：身份行与台账必须同事务出现、首次时长从 `now` 起算、过期身份从 `now` 重算（「续费吞掉已享受时间」的反例守卫）、未过期在原到期上顺延、付费来源无凭据一律拒且零残留、运营/体验必须带理由、非正 `delta` 与畸形标识拒、挂套餐须存在且档位一致且非 `DRAFT`、重放绝不二次加时长、同键换参数判冲突、无身份行的重放是错误、幂等预读失败上抛、CAS 未命中回滚台账、并发首提交回落重放、身份被并发建成判冲突。收回：`clear_remaining` 同时失效并解约、透支夹到 `now`（不制造负余额）、已过期收回写 no-op 台账、参数规则表、读失败上抛、重放与冲突、负 `plan_id` 归一、CAS 未命中回滚。到期：未到期 `skipped` 且不消耗 `request_id`、只补台账一个字不改身份行、`reason` 透传与回落、跨 `request_id` 去重、重放与冲突、身份规则，最后 `TestWritePathsNeverInventMembership` 锁死「写路径不得凭空造会员身份」 |
| `plan_flow_test.go` | 27 | 14 | 套餐：新建恒落 `DRAFT`（无「顺手上架」通道）、台账 `reason` 回落摘要、标识一致性、`DRAFT` 内可改价、离开 `DRAFT` 后规格冻结（`ErrPlanSpecImmutable` 且拒绝时零写入）、本接口不得改 `state`、CAS 未命中回滚台账、重放返回首次结论、重转换规格判 `ErrRequestIdReused`、并发建同码返回既有行、读失败上抛。上下架：三条合法边 + 台账、非法边（含同状态再上）、`reason` 与标识必填、CAS 与回滚、**重放检测排在状态机校验之前**、读失败上抛。权益码：新建回填幂等结论、`code` 不可改、坏目录拒、重放与冲突、首建超时后原样重试算重放（不是并发冲突）、CAS 与并发同码。`SetAutoRenew`：签约/解约、渠道与状态规则（过期身份不得签约）、重放冲突、CAS 未命中回滚幂等键 |
| **纯函数与替身自检** | | | |
| `validation_test.go` | 22 | 5 | 「不需要数据库也必须永远成立」的部分：列宽按 rune 计数（`VARCHAR(n)` 是字符数不是字节数）、`request_id` 校验与列宽同值、必填守卫拒空与超长、`delta` 越界报错不裁剪、分页裁剪并回显、`limit` 回落上限、幂等指纹覆盖 `plan_id`/`biz_order_no`/`payment_no`（漏比一个字段等于静默改台账口径）、`reason` 不进套餐指纹、`reason` 回落摘要、状态机只有三条边、`revokeResult` 不造负余额、负 `plan_id` 归一、`daysRemoved` 向上取整且不为正、小转换器、权益码字符集、签约渠道只认 `SANDBOX`、`planDraftFromReq` 校验表与文档化默认值、`specChanged` 只看冻结字段、`decideEntitlement` 七种结论矩阵；末条 `TestFakeDuplicatePredicateMatchesModel` 防止「假实现自己认一套、生产认另一套」 |
| `fakes_test.go` | 0 | 0 | 内存版 model + 假事务 + 读失败注入点，见第 4 组（无用例是刻意的：它只当被测对象的底座） |

### 2. 其他层

- `model`（1 文件 `27/15`）：`model/migration_parity_test.go` 把「结构体 db tag / SELECT 列常量 / INSERT 列清单 ↔
  `deploy/migrations/membership/000001_create_membership_tables.sql`」钉成可执行门禁，**不连数据库**（只解析 SQL 文本
  双向比对，迁移文件在测试里是只读对照物）：6 表列类型与列序、幂等键在 SQL 里确实是 `UNIQUE`/`PRIMARY KEY`
  （`mb_grant.uniq_request_id` 被换成普通 KEY 的表现不是启动失败而是「重复加会员天数」）、参与逐字节比较的字符列必须
  列级 `utf8mb4_bin`（表级是 `utf8mb4_unicode_ci`，删掉 COLLATE 后 `'ABC'`/`'abc'` 判成同一个键、幂等静默失效）、
  整数列不得被登记进 binary 清单（`TestBinaryKeyRegistryIsCharOnlyAndComplete`）、查询用到的索引必须存在、
  数值与字符枚举的取值全部写进列注释、`price_minor`/`duration_days` 之类必须是整数最小单位（出现 DECIMAL/DOUBLE 即红）、
  列宽与 `guard.go` 常量同向（常量比列宽长 = 「校验放过、写入报 1406」）、CAS 写路径带 `AND version = ?`、
  只追加台账禁 `mtime`/`version`、`NOT NULL` 列都得有写路径、本迁移无跨表外键、以及解析器下限自检
  （`TestParserActuallyParsedTheSchema`——解析空了也要红）。
- `internal/config`（1 文件 `3/1`）：`TestExampleConfigsLoad`（用真实 `conf.Load` 逐个加载 `etc/`，不退化成只 reflect
  结构体）、`TestDefaultsMatchExampleConfig`（去掉 `Membership` 段的最小配置必须得到与示例配置完全相同的领域参数，
  示例值与 `default` 标签不可能各自漂移）、`TestCacheFieldMustNotBeNamedRedis`（业务缓存字段必须叫 `CacheRedis`，
  叫 `Redis` 时代码可编译但 `conf.Load` 报 `conflict key redis`，属「线上启动才炸」那一类）。那 1 个子用例就是
  按 `etc/` 文件名逐个加载（`t.Run(filepath.Base(f))`），里面串了 `checkServiceIdentity`（服务名与 etcd 注册键
  `membership.v1.rpc`，改成带连字符不会启动失败，只会让服务发现永远为空）与 `checkMembershipBounds`
  （授予上限/扫描批量/分页口径配成 0 或负数等于关掉防线）。
- 本服务**没有** `internal/repository` 目录——六个 model 接口字段直接挂在 `svc.ServiceContext` 上（见第 4 组），
  因此也不存在该层的单测；`internal/svc/servicecontext.go`（`NewServiceContext` 起 MySQL 连接与可选 Redis）**无离线单测**；
  `internal/server/membershipserver.go`（goctl 生成的 rpc 转发壳）**无离线单测**；本服务无 `internal/consumer`、
  `internal/policy` 目录，也没有 HTTP handler（§7-3）。

### 3. 构造器级覆盖

`16/16`：探针取 `internal/logic` 全部 `New*Logic(`（16 个，与 `rpc/membership.proto` 的 16 个 rpc 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空。§7-4 记录的「7 个原本无引用的 constructor」补上后，读侧不再有装配不到的
logic。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 提供内存版 model 与假事务，注入点不改实现（AGENTS.md §4 不允许为测试放宽生产代码）：

- `svc.ServiceContext` 的六个 model 字段（`Plan`/`PlanLog`/`Entitlement`/`Membership`/`Grant`/`Request`）是接口类型，
  测试直接赋值即可，不需要连 MySQL。
- 复刻的是**语义**而不是锁：唯一键命中即返回可被 `IsDuplicate` 识别的 1062 报文（`model/errors.go` 的
  `isDuplicateErr` 按报文判定、不 import 驱动专有错误类型，所以这里必须造报文而不是造自定义错误类型）；
  CAS 条件不命中即 `RowsAffected=0`；「没有这行」返回 `(nil, nil)`、「读不动这行」返回错误——这两家的分家就是
  logic 全部结论的真值来源；回调报错时 `TransactCtx` 入口快照、整体回滚，所以「CAS 冲突/唯一键冲突不得留下半条结果」
  是真断言。
- 五个读失败哨兵（`errPlanDown`/`errLogDown`/`errLedgerDown`/`errIdentityDown`/`errRequestDown`）配合
  `errors.Is` 比对，证明依赖故障不会被折叠成「未开通 / 未找到 / 无台账」，也不靠报文关键字巧合判「同一次故障」。
- 守卫是否发生在触库之前，用假实现的**逐方法调用计数与入参快照**证明（不看日志）；投影逐字段与来源行比对
  （`assertPlanEchoed`/`assertEntitlementEchoed`/`assertMembershipEchoed`/`assertGrantEchoed`），漏映射一个字段就发红。
- 读方法（`ListOnSale`/`ListAdmin`/`List`/`ListExpiring`/`List`）按真实 SQL 口径复刻闭区间、排序键与 `LIMIT` 截断——
  截断是刻意保留的，好让 §7-10 暴露出来。
- 每个假实现只嵌入接口并覆写被测路径用到的方法，其余方法由内嵌的 nil 接口提升而来，走到未实现分支立刻 panic，
  失败是响的。
- 时钟：用例用 `model.NowUnix` 的真实时钟加相对偏移构造边界，不依赖挂钟具体读数。
- 它证明不了什么：真库唯一索引/`CHECK` 约束的实际生效（本迁移刻意不写 `CHECK`，见 §7-4 末段）、`SELECT ... FOR UPDATE`
  行锁与并发交错、`RowsAffected` 在真实驱动下是 matched 还是 changed（§6 要求 DSN 不加 `clientFoundRows=true`，
  这条只能在真库验证）、列宽与 collation 的真实行为、`ORDER BY` 同键行的顺序、Redis 缓存 TTL 与失效（本轮读路径未接
  缓存，§7-1）、gRPC 框架层与 etcd 服务发现。

### 5. 覆盖边界

- **0 条用例处于 skip**（主代理实测口径）；不要把用例文件数当成断言都在跑。
- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端。
- 迁移与真实库的列级对账只有 §9 第 2 组那条**静态文本**门禁（`model/migration_parity_test.go` 不连库）；
  迁移文件本身按 `deploy/migrations/README.md`「当前覆盖」登记为
  `membership | go_video_membership | 1 | applied`（隔离实例 `127.0.0.1:3399` 的 `up` + `status`，见 §7-5），
  真实/共享实例未执行，本节不把它升级成「已在目标库验证」。
- 会员变更不发事件（§7-2）、无 HTTP 面（§7-3）、`account.vip` 投影无供数通道，因此「投影被事件刷新」
  这类端到端链路不在离线覆盖内；台账无保留期/归档（§7-7）、平台维度权益（§7-8）同样没有用例。
- `internal/server/`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内；契约变更须先改 `.proto` 再
  `./scripts/gen.ps1 -Service membership`（AGENTS.md §4）。
- 缺口的权威登记仍是 §7「已知缺口」；本节只声明覆盖范围与口径，不复核缺口清单。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/membership/...
gofmt -l services/membership   # 必须为空
go vet ./services/membership/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。
