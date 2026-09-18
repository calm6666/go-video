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
- **领域事件**：AGENTS.md §5 Outbox 模式——业务写操作与事件同事务提交，发布器异步投递 `user.profile_updated`（→ account `DelCache` RPC 失效缓存）与 `user.moral_notice`（→ notification，待接入）。

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
- **user-profile → account**：资料更新事件（`user.profile_updated`）经 Outbox
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
