# creator

UP 主/机构创作者身份和创作权限服务。

## 职责

- **拥有数据**：创作者认证、机构资料、投稿权限、内容配额、创作中心摘要、
  特殊用户组（up_group）、UP 特殊属性（up_special）、UP 身份属性（up_attr）、
  关注弹窗开关（up_switch）、高能联盟签约（sign_up）。
- **提供能力**：申请/审核创作者身份、认证状态、投稿授权、创作者资料卡、
  UP 主特殊属性/分组查询、UP 身份判定、关注弹窗开关、高能联盟签约查询。
- **依赖**：`account`、`user-profile`、`moderation-orchestrator`。
- **约束**：认证通过不是稿件审核通过；不得在本服务存储视频主数据。

## 目录结构

```text
services/creator/
├── rpc/creator.proto              gRPC 源契约（8 个方法）
├── etc/creator.v1.yaml            配置示例
├── model/                         5 张表的实体与查询接口
│   ├── up_group.go                特殊用户组
│   ├── up_special.go              UP 特殊属性（mid → group_id 列表）
│   ├── up_group_member.go         up_special 反向索引（group_id → mids 分页）
│   ├── up_attr.go                 UP 身份属性
│   ├── up_switch.go               关注弹窗开关
│   └── sign_up.go                 高能联盟签约
├── internal/
│   ├── config/                    配置（RpcServerConf + Redis + MySQL）
│   ├── server/                    goctl 生成的 RPC server
│   ├── logic/                     8 个 RPC logic
│   ├── repository/                MySQL + Redis 缓存
│   └── svc/                       ServiceContext
├── creator.v1.go                  RPC 入口（无 HTTP server）
└── README.md
```

## gRPC API（8 个方法）

移植自参考仓库 `openbilibili-go-common/app/service/main/up`。依据 `AGENTS.md` §5
数据所有权约束，obc up 服务的稿件相关方法（UpArcs/UpsArcs/UpsAidPubTime/
UpCount/UpsCount/AddUpPassedCache*/DelUpPassedCache*/UpBaseStats/
UpInfoActivitys 共 11 个）不移植到本服务，由 `services/video`、
`services/engagement`、`services/spm` 等域承接。

| 方法 | 用途 |
|---|---|
| `UpSpecial` | 查询单个 UP 主特殊属性 |
| `UpsSpecial` | 批量查询 UP 主特殊属性（≤100） |
| `UpGroups` | 查询所有特殊用户组 |
| `UpGroupMids` | 查询某分组下的用户（分页） |
| `UpAttr` | 查询 UP 主身份属性（0 稿件作者/1 移动投稿/2 直播 UP/3 直播白名单） |
| `SetUpSwitch` | 设置 UP 主关注弹窗开关 |
| `UpSwitch` | 查询 UP 主关注弹窗开关 |
| `GetHighAllyUps` | 查询高能联盟 UP 主签约信息 |

## 配置

参考 [etc/creator.v1.yaml](etc/creator.v1.yaml)：

- `ListenOn`：gRPC 监听地址（默认 0.0.0.0:8086）
- `Etcd`：服务注册（key: `creator.v1.rpc`）
- `Redis`：缓存（特殊属性/身份/开关/分组列表）
- `DataSource`：MySQL DSN（`creator` 库 5 张表）

## 健康检查

- gRPC 服务通过 etcd 注册可发现性。
- `repository.Ping(ctx)` 校验 Redis 连通性。
- 端到端健康检查由 `gateway/app` 或 `gateway/admin` 通过 RPC 调用验证。

## 数据表

- `up_group(id, name, tag, short_tag, font_color, bg_color, note)`
- `up_special(mid, group_id)` —— 索引 `(mid, group_id)`、`group_id`
- `up_attr(mid, from, is_author)` —— 主键 `(mid, from)`
- `up_switch(mid, from, state)` —— 主键 `(mid, from)`
- `sign_up(mid, state, begin_date, end_date)` —— 主键 `mid`

SQL 迁移由 `deploy/migrations` 统一管理。

## 运行

```powershell
# 本地依赖：etcd / mysql / redis 由 deploy/docker-compose 启动
go run services/creator/creator.v1.go -f services/creator/etc/creator.v1.yaml
```
