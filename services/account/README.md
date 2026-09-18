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
- **关系聚合**：`Relation3`/`Attentions3`/`Blacks3`/`Relations3`/`RichRelations3` 透传 `social-graph` 服务，按 mids 顺序补齐默认值。
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
| `RichRelations3` | `RichRelationReq` | `RichRelationsReply` | 查询富关系 |
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

## 关键约束

- 本服务只暴露 gRPC；对外 HTTP 由 `gateway/app`、`gateway/admin` 聚合。
- 缓存采用 Redis，前缀 `i3_`/`c3_`/`p3_`/`v3_` 沿用参考仓库约定；
  回填与二次失效见 `internal/repository`（fanout + 延迟队列）。
- 不引入会员订单、广告投放、创作者分成等商业化能力（`Vip3`/`Vips3` 降级零值）。
