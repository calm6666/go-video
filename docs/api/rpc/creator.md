# RPC · `creator`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/creator/rpc/creator.proto` |
| protobuf 包 | `creator.v1` |
| go_package | `go-video/services/creator/rpc` |
| 发现用的 etcd key | `creator.v1.rpc`（`services/creator/etc/creator.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`creator.v1.rpc`） |
| 监听 | `8086`（`services/creator/etc/creator.v1.yaml` 的 `ListenOn`） |
| 数据库 | `creator` |
| 方法数 | 8（service `Creator`） |
| 网关消费方 | `app:CreatorRPC`、`admin:CreatorRPC` |

## 契约说明

> 说明：本契约移植自参考仓库 openbilibili-go-common/app/service/main/up。
> 依据 AGENTS.md §5，稿件数据归 video 服务、互动计数归 engagement/social-graph
> 等服务、活跃度统计归 spm 域，故 obc up 服务的 UpArcs/UpsArcs/UpCount/
> UpsCount/UpsAidPubTime/AddUpPassedCache*/DelUpPassedCache*/UpBaseStats/
> UpInfoActivitys 共 11 个方法不移植到本服务，由各 owner 服务承接。

## service `Creator`

> Creator 创作者身份与权限服务（移植自参考仓库 up 服务）

gRPC 方法前缀：`creator.v1.Creator/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `UpSpecial` | [`UpSpecialReq`](#message-upspecialreq) | [`UpSpecialReply`](#message-upspecialreply) | 查询单个 UP 主特殊属性 |
| 2 | `UpsSpecial` | [`UpsSpecialReq`](#message-upsspecialreq) | [`UpsSpecialReply`](#message-upsspecialreply) | 批量查询 UP 主特殊属性 |
| 3 | `UpGroups` | [`NoArgReq`](#message-noargreq) | [`UpGroupsReply`](#message-upgroupsreply) | 查询所有特殊用户组 |
| 4 | `UpGroupMids` | [`UpGroupMidsReq`](#message-upgroupmidsreq) | [`UpGroupMidsReply`](#message-upgroupmidsreply) | 查询某个分组下的所有用户 |
| 5 | `UpAttr` | [`UpAttrReq`](#message-upattrreq) | [`UpAttrReply`](#message-upattrreply) | 查询 UP 主身份属性 |
| 6 | `SetUpSwitch` | [`UpSwitchReq`](#message-upswitchreq) | [`EmptyReply`](#message-emptyreply) | 设置 UP 主关注弹窗开关 |
| 7 | `UpSwitch` | [`UpSwitchReq`](#message-upswitchreq) | [`UpSwitchReply`](#message-upswitchreply) | 查询 UP 主关注弹窗开关 |
| 8 | `GetHighAllyUps` | [`HighAllyUpsReq`](#message-highallyupsreq) | [`HighAllyUpsReply`](#message-highallyupsreply) | 查询高能联盟 UP 主签约信息 |

## 消息与枚举

### message `NoArgReq`

> 空请求（对应参考仓库 NoArgReq）

（空消息）

### message `EmptyReply`

> 空响应（对应参考仓库 NoReply）

（空消息）

### message `MidReq`

> 单个创作者请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |

### message `MidsReq`

> 批量创作者请求（最多 100 个）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表 |

### message `UpGroup`

> UP 主特殊用户组信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 分组 ID |
| `name` | `string` | 2 | — | 分组名 |
| `tag` | `string` | 3 | — | 标签名称 |
| `short_tag` | `string` | 4 | — | 标签简称 |
| `font_color` | `string` | 5 | — | 字体色 |
| `bg_color` | `string` | 6 | — | 背景色 |
| `note` | `string` | 7 | — | 备注 |

### message `UpSpecial`

> UP 主特殊属性

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `group_ids` | `int64` | 1 | repeated | 所属特殊分组 ID 列表 |

### message `UpSpecialReq`

> 单个 UP 主特殊属性请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |

### message `UpsSpecialReq`

> 多个 UP 主特殊属性请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表（最多 100） |

### message `UpSpecialReply`

> 特殊属性响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `up_special` | [`UpSpecial`](#message-upspecial) | 1 | — | 特殊属性 |

### message `UpsSpecialReply`

> 多个特殊属性响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `up_specials` | [`map<int64, UpSpecial>`](#message-upspecial) | 1 | — | mid → 特殊属性 |

### message `UpGroupsReply`

> 所有特殊分组响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `up_groups` | [`map<int64, UpGroup>`](#message-upgroup) | 1 | — | 分组 ID → 分组信息 |

### message `UpGroupMidsReq`

> 分组下用户请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `group_id` | `int64` | 1 | — | 分组 ID |
| `pn` | `int32` | 2 | — | 页码（从 1 开始） |
| `ps` | `int32` | 3 | — | 每页大小（最大 1000） |

### message `UpGroupMidsReply`

> 分组下用户响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 分组下用户 ID |
| `total` | `int32` | 2 | — | 总数 |

### message `UpAttrReq`

> UP 主身份属性请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `from` | `int32` | 2 | — | 来源：0 稿件作者、1 移动投稿作者、2 直播 UP、3 直播白名单 |

### message `UpAttrReply`

> UP 主身份属性响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `is_author` | `int32` | 1 | — | 是否有身份：0 否、1 是 |

### message `UpSwitchReq`

> 关注弹窗开关请求（设置和查询共用）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `from` | `int32` | 2 | — | 业务来源：0 播放器关注开关、1 UP 主荣誉周报退订 |
| `state` | `int32` | 3 | — | 开关状态：0 关闭、1 打开（仅 SetUpSwitch 使用） |

### message `UpSwitchReply`

> 关注弹窗开关响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | `int32` | 1 | — | 当前开关状态：0 关闭、1 打开 |

### message `HighAllyUpsReq`

> 高能联盟 UP 主请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表 |

### message `SignUp`

> 签约信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 签约 UP 主 ID |
| `state` | `int32` | 2 | — | 签约状态 |
| `begin_date` | `int64` | 3 | — | 签约开始时间（Unix 秒） |
| `end_date` | `int64` | 4 | — | 签约结束时间（Unix 秒） |

### message `HighAllyUpsReply`

> 高能联盟 UP 主响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lists` | [`map<int64, SignUp>`](#message-signup) | 1 | — | mid → 签约信息 |
