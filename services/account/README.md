# account

账号资料聚合服务。本服务是 `account` 域的数据所有者（见 [AGENTS.md §5](../../AGENTS.md)），
持有账号主表和登录标识，同时作为聚合层为其他服务提供用户基础信息查询
（Info/Card/Profile）。

> **架构约束**：领域微服务只暴露 gRPC，不提供 HTTP（AGENTS.md §3/§4）。
> 原参考仓库 `/x/internal/v3/account/*`、`/x/internal/account`（v1）与
> `/x/internal/account/v2/*` 的 HTTP 能力已迁移到网关：查询与 v1/v2 兼容路由
> 在 `gateway/app`，缓存运营路由在 `gateway/admin`，均通过本服务 gRPC 聚合。

## 职责

- **持有数据**：账号主表（mid、状态、注册时间、注册 IP）、登录标识（用户名/手机/邮箱）、账号缓存。
- **聚合查询**：`Info3`/`Infos3`/`Card3`/`Cards3`/`Profile3`/`ProfileWithStat3` 聚合 `user-profile`（昵称头像签名等级生日实名）、`social-graph`（关注数粉丝数关注列表黑名单）等下游服务。
- **写入能力**：`AddExp3`（经验值）、`AddMoral3`（道德值）委托 `user-profile` 服务持久化，account 仅负责失效缓存。
- **关系聚合**：`Relation3`/`Attentions3`/`Blacks3`/`Relations3`/`RichRelations3` 透传 `social-graph` 服务，
  按 mids 顺序补齐默认值；`RichRelations3` 的关系位掩码自 2026-10-04 起是真实读
  （下游补了 `RichRelations` RPC），但读失败仍被降成全 0（见已知缺口 H20）。
- **缓存管理**：`DelCache` RPC 供资料变更方（user-profile Outbox 发布器、运营网关）失效 Info/Card/Profile/Vip 缓存并异步回温；`updateVip` 动作触发 5 秒延迟二次失效。
- **会员查询**：`Vip3`/`Vips3` 按 AGENTS.md §1 降级返回零值，不接入会员业务。

## gRPC API

package `account.v1`，端口 8083，etcd 注册 key `account.v1.rpc`，共 30 个方法
（17 个资料聚合 + 13 个登录会话）。

| 方法 | 入参 | 出参 | 说明 |
|---|---|---|---|
| `Info3` | `MidReq{mid}` | `InfoReply{info}` | 查询单个用户基础信息 |
| `Infos3` | `MidsReq{mids}` | `InfosReply{infos}` | 批量查询用户基础信息 |
| `InfosByName3` | `NamesReq{names}` | `InfosReply{infos}` | 按用户名批量查询 |
| `Card3` | `MidReq{mid}` | `CardReply{card}` | 查询单个用户名片 |
| `Cards3` | `MidsReq{mids}` | `CardsReply{cards}` | 批量查询用户名片 |
| `Profile3` | `MidReq{mid}` | `ProfileReply{profile}` | 查询用户完整资料 |
| `ProfileWithStat3` | `MidReq{mid}` | `ProfileStatReply` | 查询带统计的资料 |
| `AddExp3` | `ExpReq` | `ExpReply{}` | 增加经验值（委托 user-profile） |
| `AddMoral3` | `MoralReq` | `MoralReply{}` | 增加道德值（委托 user-profile） |
| `Relation3` | `RelationReq` | `RelationReply` | 查询关注关系 |
| `Attentions3` | `MidReq` | `AttentionsReply` | 查询关注列表 |
| `Blacks3` | `MidReq` | `BlacksReply` | 查询黑名单 |
| `Relations3` | `RelationsReq` | `RelationsReply` | 批量查询关系 |
| `RichRelations3` | `RichRelationReq` | `RichRelationsReply` | 查询富关系位掩码（下游 RPC 已接线；读失败降成全 0，见缺口 H20） |
| `Vip3` | `MidReq` | `VipReply` | 查询会员信息（降级零值） |
| `Vips3` | `MidsReq` | `VipsReply` | 批量查询会员信息（降级零值） |
| `DelCache` | `DelCacheReq{mid,action}` | `DelCacheReply{}` | 失效缓存（updateVip 延迟二次失效） |
| `PasswordLogin` | `LoginReq` | `LoginReply` | 密码登录（用户名/手机/邮箱+密码，签发 token/refresh/csrf） |
| `CaptureLogin` | `LoginReq` | `LoginReply` | 验证码登录（手机+验证码） |
| `Register` | `RegisterReq` | `RegisterReply` | 注册（用户名+密码 或 手机/邮箱+验证码+密码） |
| `Logout` | `LogoutReq` | `DelCacheReply{}` | 登出（吊销 token） |
| `TokenInfo` | `GetTokenInfoReq` | `GetTokenInfoReply` | token 校验（供网关统一鉴权，参考 identify） |
| `CookieInfo` | `GetCookieInfoReq` | `GetCookieInfoReply` | cookie 会话校验（参考 identify） |
| `RenewToken` | `RenewTokenReq` | `RenewTokenReply` | 刷新 token（参考 passport-login /token/renew） |
| `SendCapture` | `SendCaptureReq` | `DelCacheReply{}` | 发送登录/注册/找回验证码（参考 sms 账号侧能力） |
| `CheckCapture` | `CheckCaptureReq` | `DelCacheReply{}` | 校验验证码 |
| `SetPassword` | `SetPasswordReq` | `DelCacheReply{}` | 设置/修改密码（参考 secure） |
| `ResetPassword` | `ResetPasswordReq` | `DelCacheReply{}` | 重置密码（账号找回，参考 account-recovery） |
| `CheckHistoryPassword` | `CheckHistoryPwdReq` | `CheckHistoryPwdReply` | 历史密码校验（参考 passport /history/pwd/check） |
| `LoginLogs` | `LoginLogsReq` | `LoginLogsReply` | 登录日志查询（参考 passport RPC.LoginLogs） |

## 数据模型与迁移

| 表 | 用途 | 关键字段 |
|---|---|---|
| `account` | 账号主表 | mid、status、is_tourist、created_at、reg_ip |
| `account_credential` | 登录标识 | credential_type、identifier、mid |
| `account_secret` | 登录密码哈希与历史密码 | mid、secret_type、salt、hash、status |
| `account_session` | 登录会话（token/refresh/cookie） | token、refresh_token、mid、csrf、expires、status |
| `account_login_log` | 登录/注册日志 | mid、login_type、status、reason、ip、ts |
| `account_capture_log` | 验证码发送审计 | biz、target、ip、status、ctime |

迁移脚本位于 `deploy/migrations/account/`（每个字段、每张表、每个索引均带详细
中文注释）：`000001`~`000014` 共 14 个迁移 + `README.md`（所有权边界与参考映射）。

## 目录结构

```text
services/account/
├── rpc/account.proto            gRPC 源契约（30 个方法）
├── etc/account.v1.yaml          配置示例
├── model/                       数据库实体（6 张表）
├── internal/
│   ├── config/                  配置（RpcServerConf + Redis + MySQL + PassportRSA + 下游 RPC client）
│   ├── server/                  goctl 生成的 RPC server
│   ├── logic/                   业务逻辑（30 个 RPC logic）
│   ├── repository/              仓储层（MySQL + Redis 缓存 + 登录会话 + 下游 RPC + fanout + 延迟队列）
│   └── svc/                     ServiceContext
├── account.v1.go                goctl 生成的 RPC 入口（无 HTTP server）
└── README.md
```

## 运行

```powershell
# 生成（从仓库根目录；框架文件由 goctl 生成，业务 logic/repository 手写）
./scripts/gen.ps1 -Service account

# 运行（纯 RPC，无 HTTP 端口）
go run ./services/account -f services/account/etc/account.v1.yaml

# 健康检查：使用 gRPC health 探针（grpc_health_probe -addr=127.0.0.1:8083）
```

## 已知缺口

### A. 下游接线（未配置即静默降级，且调用方看不出来）

1. **social-graph 适配器已于 2026-10-03 接线，但降级仍然对调用方不可见**：`internal/svc/serviceContext.go:37-40` 现在会在配置了 `SocialGraphRPC.Etcd.Hosts`/`Target` 时注入 `internal/repository/socialgraph_client.go` 的适配器，`Relation3`/`Relations3`/`Attentions3`/`Blacks3`/`RichRelations3` 与 `Card3`/`ProfileWithStat3` 的关注/粉丝计数因此走得到真实 RPC（此前该分支体只有一句 TODO，六个关系方法在**任何**配置下都回空默认值）。两点没有随接线消失：
   - (a) **降级形态照旧**：配置留空时 client 为 nil，`internal/repository/relation.go:14`、`:29`、`:51`、`:67`、`:85`、`:108` 的 nil 分支直接回空值 + `err=nil`；配置了但 RPC 报错时，`:18`、`:36`、`:55`、`:71`、`:93`、`:112` 把错误降成一条 Error 日志，应答仍是「没关注/不认识/计数 0」。调用方无法区分「下游没接上」「下游挂了」与「确实没关注」，应答里没有任何字段承载这个差别。
   - (b) **本服务从未在任何环境启动过**：适配器只有 `go build`/`go vet`/离线单测（见「测试覆盖」§5）三类证据；`etc/account.v1.yaml` 里新打开的 `SocialGraphRPC` 段的运行时行为（etcd 不可达时的启动表现、真实 gRPC 报文与框架层）未经验证。
   `internal/logic/fakes_test.go` 的 `withoutSocialGraph()` 现在复刻的是 (a) 的「未配置」形状，不再是唯一生产形状。
2. **user-profile 未注入时是显式失败**（对照第 1 条的静默空值）：`serviceContext.go:30-33` 只在配置了 Etcd/Target 时建适配器，`internal/repository/exp_moral.go:13-15`、`:29-31` 在未注入时返回 `ErrNotImplemented` —— 这条至少在应答里看得见，第 1 条看不见。

### B. 缓存失效与错误吞噬

3. **`DelCache` 的失败对外是成功**：`internal/repository/cache_delay.go:28` 如实返回 `[]error`，而 `internal/logic/delcachelogic.go:31-37` 只逐条打日志、恒回 `DelCacheReply{}` + `err=nil`。资料变更方（user-profile 的 Outbox 发布器、`gateway/admin` 的运营写入口）因此无法知道"缓存没删掉"，用户会继续读到旧昵称/旧头像，也没有任何重试路径。
4. **经验/道德变更后的失效失败完全静默**：`exp_moral.go:21`、`:37` 都是 `_ = r.cache.DelCache(ctx, mid)`，连日志都没有（比第 3 条更弱：第 3 条至少留下了可查的日志）。等级/资料改了而 Profile 缓存没失效，只能等 TTL。
5. **会话缓存被当成权威，吊销有窗口**：`internal/repository/login.go:224-229` 命中 `ak_<token>` 且缓存里 `status=ACTIVE` 就直接返回、不回库。因此（a）`delSessionCache`（`:213-217`）与精确吊销（`:440`）里 `_ = r.cache.Del(...)` 一旦失败，登出在 `tokenCacheTTL=600` 秒内不生效；（b）`RevokeAll`（`:552`、`:588`）只改库、无法按 token 枚举删缓存，改密/封禁后旧 token 最长 600 秒仍被判有效。代码注释承认了这个窗口（`:39-41`），但对调用方是静默的——客户端拿到"改密成功"，而同一枚旧 token 还能用十分钟。
6. **批量吊销的 DB 写失败被吞**：`login.go:552`、`:588` 的 `_ = r.sessionModel.RevokeAll(ctx, mid)` 忽略错误。最坏形态是"改密返回成功、旧会话在库里仍然 ACTIVE"，缓存过期后回库照样放行，且无人知道吊销没发生。

### C. 登录防护

7. **密码登录在本服务侧没有失败计数或锁定**：只有验证码路径有次数闸门（`login.go:476-492` 用 `captureErrKey` 计数并超限失效），`PasswordLogin` 的密码错误只写一条 `account_login_log`（`:275`），没有任何读取该记录做拦截的代码。防撞库完全依赖网关限流与 `risk-control`，account 自身不设第二道防线（这与 AGENTS.md §7「风控与行为分析分离」不冲突，但意味着单服务被直连时没有防护）。

### D. 事务与孤儿数据

8. **注册/改密的"事务"只覆盖 `account_secret`**：`model/account.go:36` 与 `model/account_credential.go:50` 的 `Insert` 根本没有 `sqlx.Session` 形参（只有 `model/secret.go:55` 有），所以账号主表与登录标识行走 `conn.ExecCtx`、不随事务回滚。后果：密码哈希写失败时会留下"账号已建、标识已占、没有秘密"的孤儿行，并占住 `uk_type_identifier`，用户重试会撞唯一键而再也注册不上同一个手机号/邮箱。钉：`TestRegisterTransactionOnlyRollsBackSecretWrites`（`internal/logic/registerlogic_test.go:345`）。修法是给两个 model 补 tx 形参并重新生成，属跨层改动。

### E. 验证码链路（发送 / 校验 / 消费）

9. **校验通过的验证码不会被消费，600 秒内可重复使用**：`checkCapture` 比对成功后直接
   `return nil`（`internal/repository/login.go:490-495`），既不删 `cap_code_<biz>_<target>`
   也不置失效标记；键的 TTL 是 `captureCodeTTL = 600`（`:44`）。后果是同一枚验证码在十分钟窗口内
   是**可重放的登录凭证**——被日志、抓包或共享设备拿到一次，就能在这段时间里换出多个会话。
   钉：`TestCheckCaptureCorrectCodePassesButIsNotConsumed`、
   `TestCaptureLoginDoesNotConsumeCodeOnSuccess`（`internal/logic/checkcapturelogic_test.go`、
   `internal/logic/captureloginlogic_test.go`）。
10. **验证码明文进 Info 日志**：`login.go:470` 的
    `logx.Infof("... code=%06d (sms not integrated)")` 把真实验证码打进日志。
    注释把它当开发联调手段，但这行没有任何环境开关，生产同样会落盘；
    配合缺口 9，日志读取者可在十分钟内直接登录该账号。AGENTS.md §7 与本文档的脱敏要求都不允许。
11. **发送与错误闸门都是 `>` 而非 `>=`，各放行一次**：
    `captureMaxSend = 5` 却写成 `times > captureMaxSend`（`:49-50`、`:460`）⇒ 单接收方单日实际可发 **6** 条；
    `captureMaxErr = 3` 却写成 `errTimes > captureMaxErr`（`:51-52`、`:482`）⇒ 第 **4** 次错码仍在校验路径上，
    第 5 次才因「超限即删码」被拒。钉：`TestSendCaptureDailyLimitOffByOne`、
    `TestCheckCaptureErrThresholdIsStrictlyGreaterThan`、`TestFourWrongAttemptsAllowedBeforeLockout`。
12. **Redis 读故障被折成「验证码无效」**：`login.go:484-487` 用 `GetInt` 的 `ok` 布尔判存在，
    而 `cache.go:336-346` 把读故障吞成 miss ⇒ 缓存抖动时用户看到的是 `ErrCaptureInvalid`，
    运维看到的是「用户输错了码」，两者都指不到真因。钉：`TestCheckCaptureRedisOutageIsSwallowedAsInvalid`。
13. **发送失败不写审计**：`login.go:465-467` 在 `SetInt` 失败时立刻 `return err`，
    而 `addCaptureLog` 在函数末尾（`:472`）⇒ 缓存写失败这一类发送被完全漏出
    `account_capture_log`，而这张表是发送频控与滥用的唯一证据面。
    钉：`TestSendCaptureAbortsOnCacheWriteFailure`。
14. **校验用原文、查 mid 用归一化值，键空间会错位**：`SendCapture` 进门先
    `strings.TrimSpace(target)`（`:453`），`CaptureLogin` 却把未归一化的 `req.Account` 原样喂给
    `checkCapture`（`:289`），随后 `findMidByAccount`（`:293`）才做归一化。
    于是带首尾空白的入参「发得到、验不过」，报出来的是错码而不是格式问题。
    钉：`TestCaptureLoginUsesRawAccountForCaptureKey`。

### F. 读侧与契约面

15. **`LoginLogs` 把 `reason` 原文外传且没有 buvid 载体**：`login.go:621` 的 `limit`
    原样下发（默认 20 / 上限 100 的夹紧只发生在 `model/login_log.go:99-106`），
    底层驱动/SQL 错误文本会经 `rpc.LoginLog.reason` 一字不改地交给调用方；
    同时表里的 `buvid`（设备指纹）在 `rpc.LoginLog` 中没有对应字段，
    「查得到却传不出」。钉：`TestLoginLogsProjectsEveryContractColumn`、
    `TestLoginLogsNoSanitisationOfReason`、`TestLoginLogsLimitIsForwardedVerbatim`。

### G. 测试覆盖边界（如实声明，决定断言强度）

16. 替身只复刻 model 层 SQL 的**语义**与唯一约束（`uk_type_identifier`、`uk_mid_type_status`、`uk_token`、`uk_refresh`、`account.PRIMARY(mid)`，见 `internal/logic/fakes_test.go:20-44` 的纪律清单），不证明 SQL 文本、列宽与索引本身。
17. `internal/repository/userprofile_client.go` 的适配器内部映射（`sexStr`、`Moral/100`、`mid=0` 防击穿哨兵）与 gRPC 报文不在离线覆盖内——用例只断到"入参原样交到 repository 看见的那个接口"。
18. `fakeCache.GetInt` 按真实实现把读故障吞成 miss（生产 `cache.go` 同口径），因此"Redis 故障会不会被误判成未登录/未命中"只能由集成环境验证；真实 TTL 到期、验证码明文与 gRPC 框架层（server/interceptor）同样不在纯单测可达范围。
19. `fakeLoginLogModel.FindByMid` 的排序按 `model/login_log.go:105-106` 的
    `ORDER BY ctime DESC, id DESC` 复刻；同秒不同 id 的对照行是这条断言的判别力来源
    （旧实现只做了 id 倒序，会让 `TestLoginLogsOrderIsCtimeDescThenIdDesc` 变成永真）。

### H. 关系面契约缺口（2026-10-04 已收口，剩下的只有降级策略）

20. **富关系的契约缺口已在 2026-10-04 关闭，`RichRelations3` 现在是真实读**：原缺口是
    `RelationAttr` 要按每个 mid 同时给出「owner→mid」「mid→owner」两个方向（都真才是 MUTUAL）
    与 BLACKED/SPECIAL 两个位，而 2026-10-03 时 social-graph 的批量入口只有单方向
    `IsFollowedBatch(mid→owners)`，反向与拉黑只有单条 RPC，special 连读 RPC 都没有，
    所以适配器只能显式回 `ErrRichRelationsUnsupported`（当时钉：
    `TestSGRichRelationsIsExplicitlyUnsupported`）。
    现在 social-graph 侧补了 `RichRelations(RichRelationsReq) returns (RichRelationsReply)`
    （`services/social-graph/rpc/socialgraph.proto:85`、`:101` 两条消息，`:187` 的 RPC 入口），
    四位由数据所有者一次算清：`internal/repository/repository.go:451` 的 `RichRelations`
    用四条 `IN (?)` 存在性查询（`model/relation_follow.go:148` `FindFollowers`、
    `model/relation_black.go:108` `FindBlacks`、`model/relation_special.go:108` `FindSpecials`
    加原有 `FindFollowings`）按位或出 `map[mid]attr`，
    任一查库失败整体报错（半张掩码表会被读成「谁都不认识」）；
    入参守卫在 `internal/logic/richrelationslogic.go:29-37`（owner≤0 → `ErrInvalidOwnerMid`、
    空 mids → 空 map 且不打库、>100 → `ErrTooManyMids`）。
    account 侧适配器只做切片与合并，不重新解释位（`socialgraph_client.go:114-139`，
    230 个 mid → 100/100/30 三片，任一片失败整体报错；钉：
    `TestSGRichRelationsChunksOverHundredMids`、`TestSGRichRelationsFailsWholeBatchOnOneChunk`）。
    **两处事实没有随接线改变**：
  - (a) **`relation_follow.attr` 列仍然恒写 0**，新 RPC 不读它，四位全部由行存在性 + `state`
    推导（`social-graph/internal/repository/repository.go:246` 的写侧不变）；
    三个列表 RPC 的 `RelationItem.attr` 也照旧按整张列表填常量
    （`listfollowinglogic.go:45` 恒 1、`listfollowerlogic.go:45` 恒 2、`listblackslogic.go:45` 恒 4），
    即「列表里看不出特别关注/互关」仍是 social-graph 自己的缺口（其 README 缺口 5）。
  - (b) **降级策略照旧是静默全 0**：`internal/repository/relation.go:91-93` 把下游错误降成一条
    Error 日志，并按 `:95-101` 逐 mid 补 0 + `err=nil`。也就是说「social-graph 挂了」与
    「谁都不认识」在应答里仍然不可区分（属缺口 1(a) 的一个实例，而不是契约缺口）。
    改成显式失败要动 `RichRelations3` 的错误语义与所有调用方，属维护者决策，本次没做。
    契约位口径（`1 FOLLOWING / 2 FOLLOWER / 4 BLACKED / 8 SPECIAL`，`3` 是派生值，
    判互关要写 `attr&1!=0 && attr&2!=0`）记在 `rpc/socialgraph.proto` 的枚举注释里，
    `RELATION_ATTR_SPECIAL` 从 5 改成 8 的修订记录也在同处（改前全仓无代码引用该常量）。

## 测试覆盖

离线单测（纯 Go，无 DB/Redis/gRPC 依赖），按被测面分四族 + 底层两族。
数字为 `grep -cE '^func Test'` / `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. 会话与凭据（登录 / 注册 / 改密 / 续期 / 注销 / 验证码）— `128/19`

| 文件 | 用例 | 覆盖的判定链 |
|---|---|---|
| `passwordloginlogic_test.go` | 17/3 | 口令校验、RSA 解密分支、失败计数缺失（缺口 C） |
| `captureloginlogic_test.go` | 11/0 | 验证码登录不消费验证码（缺口 E9） |
| `checkcapturelogic_test.go` | 8/6 | 校验三态：正确/错码/超次；Redis 读故障被折成 `ErrCaptureInvalid`（缺口 E12） |
| `sendcapturelogic_test.go` | 9/4 | 发送闸门 `>` 口径（缺口 E11）、发送失败不写日志（缺口 E13） |
| `registerlogic_test.go` | 16/1 | 注册事务只覆盖 `account_secret`（缺口 D） |
| `setpasswordlogic_test.go` / `resetpasswordlogic_test.go` | 14/0 · 14/0 | 历史口令、`pwd_history` 写入序 |
| `checkhistorypasswordlogic_test.go` | 5/3 | 历史口令比对 |
| `renewtokenlogic_test.go` / `logoutlogic_test.go` / `tokeninfologic_test.go` | 11/0 · 10/0 · 13/2 | token 续期、注销与 600 秒旧 token 窗口（缺口 B） |

### 2. 资料聚合与读侧 — `82/24`

`info3`(8/2)、`infos3`(6/1)、`infosbyname3`(8/2)、`profile3`(7/3)、
`profilewithstat3`(5/1)、`card3`(7/1)、`cards3`(10/4)、`vip3`(6/1)、`vips3`(6/2)、
`cookieinfo`(6/2)、`delcache`(6/1)、`loginlogs`(7/5)。
其中 `loginlogslogic_test.go` 是缺口 F15 的判别力来源：逐列投影、`ctime DESC, id DESC`
同秒 tiebreak、`limit` 原样下发、`reason` 不脱敏、`buvid` 无载体。

### 3. 关系面 — `27/12`

`attentions3`(5/0)、`blacks3`(5/0)、`relation3`(5/4)、`relations3`(6/4)、
`richrelations3`(6/4)。这族用例只替到 `SocialGraphClient` 接口边界（repository 看见的那一层），
两种装配都跑：注入了替身的可达形状，以及 `withoutSocialGraph()` 复刻的「未配置」降级形状（缺口 1(a)）。
适配器本身的映射/分页/切片见下一节第 5 条；`RichRelations` 从 2026-10-04 起是「真打下游、原样透传位掩码」，
两层各钉各的事：适配器层钉切片与错误外传，logic 层钉 repository 的吞错补 0（缺口 H20）。

### 4. 成长值与信用 — `13/4`

`addexp3logic_test.go`(6/3)、`addmoral3logic_test.go`(7/1)。

### 5. 底层与配置

- `internal/repository/`（3 文件 `21/0`）：`delay_queue_test.go` 3 条覆盖延迟队列去重/到期弹出/关闭；
  `login_test.go` 7 条覆盖 `saltPwd`、`genMid`、凭据类型判定、`randomHex`、明文/ RSA 口令解密与 cookie 解析；
  `socialgraph_client_test.go` 11 条覆盖 2026-10-03 接的 social-graph 适配器
  （`Relation` 映射与错误原样外传、`Relations` 按 100 切片 230→100/100/30 且任一片失败整体报错、
  `Attentions` 按 pn=1.. 翻到 total 与空页为止、超过 100 页硬停并报上限、
  `Blacks` 回 set、`Stat` 两个计数与出错外传；
  2026-10-04 富关系接线后把原来的「显式未接线」1 条换成 4 条：
  位掩码逐键原样透传且**不丢下游显式的 0**、230 个 mid 切 100/100/30 且逐片为连续切片、
  任一片失败整体报错且不得回部分结果、空 mids 零下游调用）。
  断言方式是**结构化记账**（`sgCall` 记录方法名与逐字段入参），
  替身内嵌未实现的 `socialgraph.SocialGraphClient` 接口——适配器一旦偷偷调用没建模的 RPC，
  panic 直接报在方法名上；`var _ SocialGraphClient = (*socialGraphClient)(nil)` 是编译期钉子，
  保证适配器满足生产实际注入的那条缝。
  不覆盖的部分：真实 gRPC 报文、超时/重试、etcd 寻址与 `zrpc.MustNewClient` 的启动行为。
- `internal/config/config_load_test.go`（1 文件 `2/1`）：`TestExampleConfigsLoad` 保证 `etc` 示例配置可被
  go-zero 真实加载且必填字段非空；`TestDownstreamRpcBlocksAreConfigured` 钉住两个下游 RPC 段
  （`UserProfileRPC`/`SocialGraphRPC`）在示例配置里都真的配了 Etcd/Target，且 Etcd Key 与对端服务
  自己注册的 Key 逐字一致（`socialgraph.v1.rpc`/`user-profile.v1.rpc`），
  并用零值 `Config` 作对照形态钉住「未配置＝nil 注入」这个前提。
  它不检查连通性——本仓库从未启动过任何服务。
- `internal/logic/fakes_test.go`：替身层与断言工具（`TestMain` 只做注入，不计入用例）。
- `model/`（5 文件）**无离线单测**——SQL 文本、列宽、索引只能由迁移与集成环境证明（缺口 G16）。

### 6. 门禁口径（实测）

- 构造器覆盖 **30/30**：探针取 `internal/logic` 全部 `New*Logic(`，逐个在 `*_test.go` 里查引用，无 `GAP`。
- 用例规模 **273 顶层 + 60 子用例**（logic 250+59，repository 21，config 2+1；`TestMain` 已排除）。
  2026-10-03 接 social-graph 适配器时 +9 顶层（repository 8 条 + config 1 条），
  2026-10-04 富关系真接线时 repository 再 +3 顶层（删 1 条「显式未接线」哨兵、加 4 条透传/切片/失败/空入参），
  两次都没有删改任何既有用例的期望值。
- `go test -p 1 -count=1 ./services/account/...` 全绿，**0 skip、0 fail**；`go vet ./services/account/...` 无输出。
- 上面的「子用例」是 `t.Run(` 的**静态**计数（60）。2026-10-04 用 `-v` 实跑导出的执行数是
  顶格用例 273、含各层子用例共 405 条 PASS（即表驱动循环把 60 个 `t.Run` 站点展开成 132 次执行），
  `--- SKIP` 与 `--- FAIL` 均为 0——不要把静态计数当成「跑了多少」。
- **一条随机红已在 2026-10-04 修好**：`TestInfosByName3DropsUnresolvableNames` 用 `wantOpsSet`
  比较调用序列，但 `wantOpsSet` 只对**步骤之间**排序，步骤**内部**的逗号参数仍是字符串精确比对；
  而 `InfosByName3` 把 name→mid 的 map 摊成 mids 再批量回源（`infosbyname3logic.go:40-42`），
  `userProfile.Bases:70001,70002` 的先后每次运行都不同。新增 `wantOpsArgSet`
  （`fakes_test.go:203-225`）只放宽这一维：步数、每步调用名、参数**集合**照旧精确，
  多一个 mid、少一个 mid 仍照样红。该 helper 只在本服务存在，其余四处 `wantOpsSet` 站点
  （`cards3`、`vips3`、`infos3`）覆盖的都是单参数回填步骤，不受影响；
  而 `userProfile.Members:70001,70002`、`Infos3` 的 `Bases:A,B` 这类多参数步骤
  参数来自**请求切片**、顺序确定，仍走精确的 `wantOps`。
- 断言强度约束见缺口 G：替身复刻 model 层 SQL 语义与唯一约束，不放行「查不到即成功」，
  也不得用永真断言凑数（排序、闸门边界、键派生都有对照行/对照值判别）。

## 关键约束

- 本服务只暴露 gRPC；对外 HTTP 由 `gateway/app`、`gateway/admin` 聚合。
- 缓存采用 Redis，前缀 `i3_`/`c3_`/`p3_`/`v3_` 沿用参考仓库约定；
  回填与二次失效见 `internal/repository`（fanout + 延迟队列）。
- 不引入会员订单、广告投放、创作者分成等商业化能力（`Vip3`/`Vips3` 降级零值）。
