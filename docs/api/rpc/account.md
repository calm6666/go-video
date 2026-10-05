# RPC · `account`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/account/rpc/account.proto` |
| protobuf 包 | `account.v1` |
| go_package | `go-video/services/account/rpc` |
| 发现用的 etcd key | `account.v1.rpc`（`services/account/etc/account.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`account.v1.rpc`） |
| 监听 | `8083`（`services/account/etc/account.v1.yaml` 的 `ListenOn`） |
| 数据库 | `account` |
| 方法数 | 30（service `Account`） |
| 网关消费方 | `app:AccountRPC`、`admin:AccountRPC` |

## 契约说明

>

## service `Account`

gRPC 方法前缀：`account.v1.Account/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `Info3` | [`MidReq`](#message-midreq) | [`InfoReply`](#message-inforeply) | 查询单个用户基础信息 |
| 2 | `Infos3` | [`MidsReq`](#message-midsreq) | [`InfosReply`](#message-infosreply) | 批量查询用户基础信息 |
| 3 | `InfosByName3` | [`NamesReq`](#message-namesreq) | [`InfosReply`](#message-infosreply) | 按用户名批量查询 |
| 4 | `Card3` | [`MidReq`](#message-midreq) | [`CardReply`](#message-cardreply) | 查询单个用户名片 |
| 5 | `Cards3` | [`MidsReq`](#message-midsreq) | [`CardsReply`](#message-cardsreply) | 批量查询用户名片 |
| 6 | `Profile3` | [`MidReq`](#message-midreq) | [`ProfileReply`](#message-profilereply) | 查询用户完整资料 |
| 7 | `ProfileWithStat3` | [`MidReq`](#message-midreq) | [`ProfileStatReply`](#message-profilestatreply) | 查询带统计的资料 |
| 8 | `AddExp3` | [`ExpReq`](#message-expreq) | [`ExpReply`](#message-expreply) | 增加经验值 |
| 9 | `AddMoral3` | [`MoralReq`](#message-moralreq) | [`MoralReply`](#message-moralreply) | 增加道德值 |
| 10 | `Relation3` | [`RelationReq`](#message-relationreq) | [`RelationReply`](#message-relationreply) | 查询关注关系 |
| 11 | `Attentions3` | [`MidReq`](#message-midreq) | [`AttentionsReply`](#message-attentionsreply) | 查询关注列表 |
| 12 | `Blacks3` | [`MidReq`](#message-midreq) | [`BlacksReply`](#message-blacksreply) | 查询黑名单 |
| 13 | `Relations3` | [`RelationsReq`](#message-relationsreq) | [`RelationsReply`](#message-relationsreply) | 批量查询关系 |
| 14 | `RichRelations3` | [`RichRelationReq`](#message-richrelationreq) | [`RichRelationsReply`](#message-richrelationsreply) | 查询富关系 |
| 15 | `Vip3` | [`MidReq`](#message-midreq) | [`VipReply`](#message-vipreply) | 查询会员信息 |
| 16 | `Vips3` | [`MidsReq`](#message-midsreq) | [`VipsReply`](#message-vipsreply) | 批量查询会员信息 |
| 17 | `DelCache` | [`DelCacheReq`](#message-delcachereq) | [`DelCacheReply`](#message-delcachereply) | 失效指定用户的缓存（资料变更方调用：user-profile 等下游服务通过本方法 / 通知 account 失效 Info/Card/Profile/Vip 缓存，action=updateVip 时 / 额外触发 5 秒延迟二次失效） |
| 18 | `PasswordLogin` | [`LoginReq`](#message-loginreq) | [`LoginReply`](#message-loginreply) | ==================== 登录与会话 ==================== / 密码登录（登录标识：用户名/手机/邮箱 + 密码） |
| 19 | `CaptureLogin` | [`LoginReq`](#message-loginreq) | [`LoginReply`](#message-loginreply) | 验证码登录（手机 + 验证码） |
| 20 | `Register` | [`RegisterReq`](#message-registerreq) | [`RegisterReply`](#message-registerreply) | 注册（用户名+密码 或 手机+验证码+密码） |
| 21 | `Logout` | [`LogoutReq`](#message-logoutreq) | [`DelCacheReply`](#message-delcachereply) | 登出（吊销 token） |
| 22 | `TokenInfo` | [`GetTokenInfoReq`](#message-gettokeninforeq) | [`GetTokenInfoReply`](#message-gettokeninforeply) | token 校验（参考 identify.GetTokenInfo，供网关统一鉴权） |
| 23 | `CookieInfo` | [`GetCookieInfoReq`](#message-getcookieinforeq) | [`GetCookieInfoReply`](#message-getcookieinforeply) | cookie 会话校验（参考 identify.GetCookieInfo） |
| 24 | `RenewToken` | [`RenewTokenReq`](#message-renewtokenreq) | [`RenewTokenReply`](#message-renewtokenreply) | 刷新 token（参考 passport-login /token/renew） |
| 25 | `SendCapture` | [`SendCaptureReq`](#message-sendcapturereq) | [`DelCacheReply`](#message-delcachereply) | 发送登录/注册/找回验证码（参考 sms 服务的账号侧验证码能力） |
| 26 | `CheckCapture` | [`CheckCaptureReq`](#message-checkcapturereq) | [`DelCacheReply`](#message-delcachereply) | 校验验证码（参考 passport-login /captcha/check） |
| 27 | `SetPassword` | [`SetPasswordReq`](#message-setpasswordreq) | [`DelCacheReply`](#message-delcachereply) | 设置/修改密码（参考 secure 服务；改密需旧密码） |
| 28 | `ResetPassword` | [`ResetPasswordReq`](#message-resetpasswordreq) | [`DelCacheReply`](#message-delcachereply) | 重置密码（账号找回，参考 account-recovery；验证码校验后重置） |
| 29 | `CheckHistoryPassword` | [`CheckHistoryPwdReq`](#message-checkhistorypwdreq) | [`CheckHistoryPwdReply`](#message-checkhistorypwdreply) | 历史密码校验（参考 passport /history/pwd/check） |
| 30 | `LoginLogs` | [`LoginLogsReq`](#message-loginlogsreq) | [`LoginLogsReply`](#message-loginlogsreply) | 登录日志查询（参考 passport RPC.LoginLogs 与 /x/internal/passport/records/loginlog） |

## 消息与枚举

### message `Card`

> 用户名片

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 用户名 |
| `sex` | `string` | 3 | — | 性别 |
| `face` | `string` | 4 | — | 头像 URL |
| `sign` | `string` | 5 | — | 个人签名 |
| `rank` | `int32` | 6 | — | 排名 |
| `level` | `int32` | 7 | — | 用户等级 |
| `silence` | `int32` | 8 | — | 是否被禁言：0 否、1 是 |
| `vip` | [`VipInfo`](#message-vipinfo) | 9 | — | 会员信息 |
| `pendant` | [`PendantInfo`](#message-pendantinfo) | 10 | — | 头像挂件 |
| `nameplate` | [`NameplateInfo`](#message-nameplateinfo) | 11 | — | 勋章 |
| `official` | [`OfficialInfo`](#message-officialinfo) | 12 | — | 官方认证 |

### message `Info`

> 用户基础信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 用户名 |
| `sex` | `string` | 3 | — | 性别 |
| `face` | `string` | 4 | — | 头像 URL |
| `sign` | `string` | 5 | — | 个人签名 |
| `rank` | `int32` | 6 | — | 排名 |

### message `Profile`

> 用户完整资料

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 用户名 |
| `sex` | `string` | 3 | — | 性别 |
| `face` | `string` | 4 | — | 头像 URL |
| `sign` | `string` | 5 | — | 个人签名 |
| `rank` | `int32` | 6 | — | 排名 |
| `level` | `int32` | 7 | — | 用户等级 |
| `join_time` | `int32` | 8 | — | 注册时间（Unix 时间戳） |
| `moral` | `int32` | 9 | — | 道德值 |
| `silence` | `int32` | 10 | — | 是否被禁言 |
| `email_status` | `int32` | 11 | — | 邮箱绑定状态 |
| `tel_status` | `int32` | 12 | — | 手机绑定状态 |
| `identification` | `int32` | 13 | — | 实名认证状态 |
| `vip` | [`VipInfo`](#message-vipinfo) | 14 | — | 会员信息 |
| `pendant` | [`PendantInfo`](#message-pendantinfo) | 15 | — | 头像挂件 |
| `nameplate` | [`NameplateInfo`](#message-nameplateinfo) | 16 | — | 勋章 |
| `official` | [`OfficialInfo`](#message-officialinfo) | 17 | — | 官方认证 |
| `birthday` | `int64` | 18 | — | 生日（Unix 时间戳，0 表示未设置） |
| `is_tourist` | `int32` | 19 | — | 是否游客：0 否、1 是 |

### message `LevelInfo`

> 等级信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cur` | `int32` | 1 | — | 当前等级 |
| `min` | `int32` | 2 | — | 当前等级最小经验值 |
| `now_exp` | `int32` | 3 | — | 当前经验值 |
| `next_exp` | `int32` | 4 | — | 升级所需经验值 |

### message `VipInfo`

> 会员信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `type` | `int32` | 1 | — | 会员类型：0 无、1 月度、2 年度 |
| `status` | `int32` | 2 | — | 会员状态：0 无、1 正常 |
| `due_date` | `int64` | 3 | — | 到期时间（Unix 时间戳） |
| `vip_pay_type` | `int32` | 4 | — | 付费类型 |

### message `PendantInfo`

> 头像挂件信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pid` | `int32` | 1 | — | 挂件 ID |
| `name` | `string` | 2 | — | 挂件名称 |
| `image` | `string` | 3 | — | 挂件图片 URL |
| `expire` | `int64` | 4 | — | 到期时间 |

### message `NameplateInfo`

> 勋章信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `nid` | `int32` | 1 | — | 勋章 ID |
| `name` | `string` | 2 | — | 勋章名称 |
| `image` | `string` | 3 | — | 勋章图片 URL |
| `image_small` | `string` | 4 | — | 勋章小图 URL |
| `level` | `string` | 5 | — | 勋章等级 |
| `condition` | `string` | 6 | — | 获取条件 |

### message `OfficialInfo`

> 官方认证信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `role` | `int32` | 1 | — | 认证角色 |
| `title` | `string` | 2 | — | 认证标题 |
| `desc` | `string` | 3 | — | 认证描述 |

### message `MidReq`

> 单个 mid 请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `real_ip` | `string` | 2 | — | 请求来源 IP |

### message `MidsReq`

> 批量 mid 请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表 |
| `real_ip` | `string` | 2 | — | 请求来源 IP |

### message `NamesReq`

> 用户名列表请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `names` | `string` | 1 | repeated | 用户名列表 |
| `real_ip` | `string` | 2 | — | 请求来源 IP |

### message `ExpReq`

> 经验值变更请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `exp` | `double` | 2 | — | 经验值变更量 |
| `operater` | `string` | 3 | — | 操作者 |
| `operate` | `string` | 4 | — | 操作类型 |
| `reason` | `string` | 5 | — | 变更原因 |
| `real_ip` | `string` | 6 | — | 请求来源 IP |

### message `MoralReq`

> 道德值变更请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `moral` | `double` | 2 | — | 道德值变更量 |
| `oper` | `string` | 3 | — | 操作者 |
| `reason` | `string` | 4 | — | 变更原因 |
| `remark` | `string` | 5 | — | 备注 |
| `real_ip` | `string` | 6 | — | 请求来源 IP |

### message `RelationReq`

> 关系查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 ID |
| `owner` | `int64` | 2 | — | 目标用户 ID |
| `real_ip` | `string` | 3 | — | 请求来源 IP |

### message `RelationsReq`

> 批量关系查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 当前用户 ID |
| `owners` | `int64` | 2 | repeated | 目标用户 ID 列表 |
| `real_ip` | `string` | 3 | — | 请求来源 IP |

### message `RichRelationReq`

> 富关系查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner` | `int64` | 1 | — | 目标用户 ID |
| `mids` | `int64` | 2 | repeated | 用户 ID 列表 |
| `real_ip` | `string` | 3 | — | 请求来源 IP |

### message `InfoReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `info` | [`Info`](#message-info) | 1 | — | — |

### message `InfosReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `infos` | [`map<int64, Info>`](#message-info) | 1 | — | — |

### message `CardReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `card` | [`Card`](#message-card) | 1 | — | — |

### message `CardsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cards` | [`map<int64, Card>`](#message-card) | 1 | — | — |

### message `ProfileReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `profile` | [`Profile`](#message-profile) | 1 | — | — |

### message `ProfileStatReply`

> 带统计的资料回复

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `profile` | [`Profile`](#message-profile) | 1 | — | 用户资料 |
| `level_info` | [`LevelInfo`](#message-levelinfo) | 2 | — | 等级信息 |
| `coins` | `double` | 3 | — | 硬币数量 |
| `following` | `int64` | 4 | — | 关注数 |
| `follower` | `int64` | 5 | — | 粉丝数 |

### message `RelationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `following` | `bool` | 1 | — | 是否关注 |

### message `AttentionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `attentions` | `int64` | 1 | repeated | 关注列表 |

### message `BlacksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `black_list` | `map<int64, bool>` | 1 | — | 黑名单 |

### message `RelationsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `relations` | [`map<int64, RelationReply>`](#message-relationreply) | 1 | — | 关系列表 |

### message `RichRelationsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `rich_relations` | `map<int64, int32>` | 1 | — | 富关系 |

### message `VipReply`

> 会员信息回复

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `type` | `int32` | 1 | — | 会员类型 |
| `status` | `int32` | 2 | — | 会员状态 |
| `due_date` | `int64` | 3 | — | 到期时间 |
| `vip_pay_type` | `int32` | 4 | — | 付费类型 |

### message `VipsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `vips` | [`map<int64, VipReply>`](#message-vipreply) | 1 | — | 会员信息列表 |

### message `ExpReply`

（空消息）

### message `MoralReply`

（空消息）

### message `DelCacheReq`

> 缓存失效请求（服务间调用，参考 HTTP /cache/del 与 /cache/clear 的 RPC 化）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `action` | `string` | 2 | — | 变更动作：updateVip 会额外入延迟队列二次失效 |

### message `DelCacheReply`

> 缓存失效响应

（空消息）

### message `LoginReq`

> ==================== 登录与会话（移植自 passport/passport-auth/identify） ==================== / 登录请求（密码登录与验证码登录共用；password 为 RSA 加密后的 base64， / 开发模式未配置密钥时按明文处理）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `account` | `string` | 1 | — | 登录标识：用户名/手机号/邮箱 |
| `password` | `string` | 2 | — | 密码（RSA 公钥加密后的 base64） |
| `login_type` | `int32` | 3 | — | 登录方式：1 密码、2 验证码 |
| `capture_code` | `string` | 4 | — | 验证码（login_type=2 时必填） |
| `ip` | `string` | 5 | — | 登录来源 IP |
| `device` | `string` | 6 | — | 设备标识 |
| `buvid` | `string` | 7 | — | 设备 BUVID |

### message `LoginReply`

> 登录响应（成功签发 token 与 refresh_token）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `token` | `string` | 2 | — | access token（30 天有效） |
| `refresh_token` | `string` | 3 | — | 刷新令牌（90 天有效） |
| `csrf` | `string` | 4 | — | CSRF token |
| `expires` | `int64` | 5 | — | token 过期时间（Unix 秒） |

### message `RegisterReq`

> 注册请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `account` | `string` | 1 | — | 注册标识：用户名/手机号/邮箱 |
| `password` | `string` | 2 | — | 密码（RSA 公钥加密后的 base64） |
| `capture_code` | `string` | 3 | — | 手机/邮箱注册时的验证码 |
| `ip` | `string` | 4 | — | 注册来源 IP |

### message `RegisterReply`

> 注册响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 新用户 ID |
| `login` | [`LoginReply`](#message-loginreply) | 2 | — | 注册成功后直接签发的登录态 |

### message `LogoutReq`

> 登出请求（吊销指定 token）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | `string` | 1 | — | 要吊销的 access token |

### message `GetTokenInfoReq`

> token 校验请求（参考 identify.GetTokenInfo）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | `string` | 1 | — | access token |
| `buvid` | `string` | 2 | — | 设备 BUVID |

### message `GetTokenInfoReply`

> token 校验响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `is_login` | `bool` | 1 | — | 是否登录（有效） |
| `mid` | `int64` | 2 | — | 用户 ID |
| `csrf` | `string` | 3 | — | CSRF token |
| `expires` | `int64` | 4 | — | 过期时间（Unix 秒） |

### message `GetCookieInfoReq`

> cookie 会话校验请求（参考 identify.GetCookieInfo）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cookie` | `string` | 1 | — | 请求 Cookie 原文（如 'SESSDATA=xxx;sid=yyy'） |

### message `GetCookieInfoReply`

> cookie 会话校验响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `is_login` | `bool` | 1 | — | 是否登录（有效） |
| `mid` | `int64` | 2 | — | 用户 ID |
| `csrf` | `string` | 3 | — | CSRF token |
| `expires` | `int64` | 4 | — | 过期时间（Unix 秒） |

### message `RenewTokenReq`

> 刷新 token 请求（参考 passport-login /token/renew）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `refresh_token` | `string` | 1 | — | 刷新令牌 |
| `ip` | `string` | 2 | — | 请求来源 IP |

### message `RenewTokenReply`

> 刷新 token 响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | `string` | 1 | — | 新 access token |
| `csrf` | `string` | 2 | — | 新 CSRF token |
| `expires` | `int64` | 3 | — | 新 token 过期时间（Unix 秒） |

### message `SendCaptureReq`

> 发送登录/注册/找回验证码请求（参考 sms 服务的账号侧验证码能力）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `biz` | `int32` | 1 | — | 业务：1 登录、2 注册、3 账号找回 |
| `target` | `string` | 2 | — | 接收方：手机号 |
| `ip` | `string` | 3 | — | 请求来源 IP |

### message `CheckCaptureReq`

> 校验验证码请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `biz` | `int32` | 1 | — | 业务：1 登录、2 注册、3 账号找回 |
| `target` | `string` | 2 | — | 接收方：手机号 |
| `capture_code` | `string` | 3 | — | 验证码 |

### message `SetPasswordReq`

> 设置/修改密码请求（参考 secure 服务；需登录态，改密需旧密码）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID（由网关从登录态注入） |
| `old_password` | `string` | 2 | — | 旧密码（首次设置密码时为空；RSA 加密后的 base64） |
| `new_password` | `string` | 3 | — | 新密码（RSA 加密后的 base64） |
| `ip` | `string` | 4 | — | 请求来源 IP |

### message `ResetPasswordReq`

> 重置密码请求（账号找回，参考 account-recovery；验证码校验后重置）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `account` | `string` | 1 | — | 登录标识：手机号 |
| `capture_code` | `string` | 2 | — | 找回验证码 |
| `new_password` | `string` | 3 | — | 新密码（RSA 加密后的 base64） |
| `ip` | `string` | 4 | — | 请求来源 IP |

### message `CheckHistoryPwdReq`

> 历史密码校验请求（参考 passport /history/pwd/check）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `password` | `string` | 2 | — | 待校验密码（RSA 加密后的 base64，可逗号分隔多个） |

### message `CheckHistoryPwdReply`

> 历史密码校验响应（result 为逗号分隔的 0/1 序列，与入参一一对应）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `result` | `string` | 1 | — | 命中序列：如 "0,1" |

### message `LoginLogsReq`

> 登录日志查询请求（参考 passport RPC.LoginLogs）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `limit` | `int32` | 2 | — | 返回条数上限（默认 20，最大 100） |

### message `LoginLog`

> 单条登录日志

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `ip` | `string` | 2 | — | 登录来源 IP |
| `ts` | `int64` | 3 | — | 登录时间（Unix 秒） |
| `login_type` | `int32` | 4 | — | 登录方式：1 密码、2 验证码、3 注册 |
| `status` | `int32` | 5 | — | 结果：0 成功、1 失败 |
| `reason` | `string` | 6 | — | 失败原因 |
| `device` | `string` | 7 | — | 设备标识 |

### message `LoginLogsReply`

> 登录日志查询响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `logs` | [`LoginLog`](#message-loginlog) | 1 | repeated | 日志列表（时间倒序） |
