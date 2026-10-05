# RPC · `rights`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/rights/rpc/rights.proto` |
| protobuf 包 | `rights.v1` |
| go_package | `go-video/services/rights/rpc` |
| 发现用的 etcd key | `rights.v1.rpc`（`services/rights/etc/rights.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`rights.v1.rpc`） |
| 监听 | `8097`（`services/rights/etc/rights.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_rights` |
| 方法数 | 9（service `Rights`） |
| 网关消费方 | `admin:RightsRPC` |

## 契约说明

> rights 服务：版权合同与播放窗口的领域服务。
> 依据 AGENTS.md §1，本期只实现授权校验、合同/窗口 CRUD 和过期下架，
> 不实现会员、订单、支付等商业化能力。
> 依据 §5，rights 拥有 rights_contract 与 rights_window 两张表，
> catalog/playback 通过本服务 RPC 校验可播放性，过期窗口不再返回可播放结果。

## service `Rights`

> rights 版权合同与播放窗口服务。 / 依据 AGENTS.md §1，不实现会员、订单、支付、投币等商业化； / 依据 §5，rights 只写 rights_contract/rights_window，不复制 catalog 视频主数据； / 依据 §8，过期/撤权窗口不再返回可播放结果，由 cron 或运营通过 ExpireWindow 推进。

gRPC 方法前缀：`rights.v1.Rights/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CreateContract` | [`CreateContractReq`](#message-createcontractreq) | [`ContractReply`](#message-contractreply) | 运营创建合同 |
| 2 | `GetContract` | [`ContractReq`](#message-contractreq) | [`ContractReply`](#message-contractreply) | 查询单个合同 |
| 3 | `ListContracts` | [`ListReq`](#message-listreq) | [`ContractsReply`](#message-contractsreply) | 分页查询合同 |
| 4 | `CreateWindow` | [`CreateWindowReq`](#message-createwindowreq) | [`WindowReply`](#message-windowreply) | 为内容创建时间窗口（关联合同） |
| 5 | `GetWindow` | [`WindowReq`](#message-windowreq) | [`WindowReply`](#message-windowreply) | 查询单个窗口 |
| 6 | `ListWindows` | [`ListWindowsReq`](#message-listwindowsreq) | [`WindowsReply`](#message-windowsreply) | 按 content_id 或 contract_id 查询窗口 |
| 7 | `CheckPlayable` | [`CheckReq`](#message-checkreq) | [`CheckReply`](#message-checkreply) | 校验内容在某地区是否可播放（窗口有效） |
| 8 | `ExpireWindow` | [`WindowReq`](#message-windowreq) | [`WindowReply`](#message-windowreply) | 手动过期窗口（运营/cron） |
| 9 | `ListExpiring` | [`ListExpiringReq`](#message-listexpiringreq) | [`WindowsReply`](#message-windowsreply) | 查询即将过期的窗口（cron 用） |

## 消息与枚举

### enum `ContractState`

> 合同状态

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTRACT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `CONTRACT_STATE_ACTIVE` | 1 | 生效中 |
| `CONTRACT_STATE_TERMINATED` | 2 | 已终止 |

### enum `WindowState`

> 窗口状态

| 值 | 编号 | 说明 |
|---|---|---|
| `WINDOW_STATE_UNSPECIFIED` | 0 | 未指定 |
| `WINDOW_STATE_ACTIVE` | 1 | 有效 |
| `WINDOW_STATE_EXPIRED` | 2 | 已过期 |
| `WINDOW_STATE_REVOKED` | 3 | 已撤权 |

### enum `ContentType`

> 内容类型

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTENT_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `CONTENT_TYPE_PGC` | 1 | 版权内容（电影/电视剧/番剧） |
| `CONTENT_TYPE_UGC` | 2 | UGC 稿件 |

### message `Contract`

> --- 合同 --- / 合同实体

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `contract_id` | `int64` | 1 | — | 合同 ID |
| `owner_id` | `int64` | 2 | — | 版权方 ID |
| `title` | `string` | 3 | — | 合同标题 |
| `sign_date` | `int64` | 4 | — | 签订日期（Unix 秒） |
| `start_date` | `int64` | 5 | — | 生效日期（Unix 秒） |
| `end_date` | `int64` | 6 | — | 到期日期（Unix 秒） |
| `regions` | `string` | 7 | repeated | 授权地区代码列表 |
| `state` | [`ContractState`](#enum-contractstate) | 8 | — | 合同状态 |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |

### message `CreateContractReq`

> 创建合同请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner_id` | `int64` | 1 | — | 版权方 ID |
| `title` | `string` | 2 | — | 合同标题 |
| `sign_date` | `int64` | 3 | — | 签订日期（Unix 秒） |
| `start_date` | `int64` | 4 | — | 生效日期（Unix 秒） |
| `end_date` | `int64` | 5 | — | 到期日期（Unix 秒） |
| `regions` | `string` | 6 | repeated | 授权地区代码列表 |
| `operator` | `string` | 7 | — | 操作人（运营/系统） |
| `ip` | `string` | 8 | — | 调用方 IP |

### message `ContractReply`

> 合同响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `contract` | [`Contract`](#message-contract) | 1 | — | 合同详情 |

### message `ContractReq`

> 单个合同查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `contract_id` | `int64` | 1 | — | 合同 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `ListReq`

> 分页查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `owner_id` | `int64` | 1 | — | 版权方 ID（可选，0 表示不筛选） |
| `state` | [`ContractState`](#enum-contractstate) | 2 | — | 合同状态（可选） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `ContractsReply`

> 合同列表响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `contracts` | [`Contract`](#message-contract) | 2 | repeated | 合同列表 |

### message `Window`

> --- 窗口 --- / 窗口实体

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `window_id` | `int64` | 1 | — | 窗口 ID |
| `contract_id` | `int64` | 2 | — | 关联合同 ID |
| `content_id` | `int64` | 3 | — | 内容 ID |
| `content_type` | [`ContentType`](#enum-contenttype) | 4 | — | 内容类型 |
| `region` | `string` | 5 | — | 授权地区代码 |
| `start_time` | `int64` | 6 | — | 窗口开始时间（Unix 秒） |
| `end_time` | `int64` | 7 | — | 窗口结束时间（Unix 秒） |
| `state` | [`WindowState`](#enum-windowstate) | 8 | — | 窗口状态 |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |

### message `CreateWindowReq`

> 创建窗口请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `contract_id` | `int64` | 1 | — | 关联合同 ID |
| `content_id` | `int64` | 2 | — | 内容 ID |
| `content_type` | [`ContentType`](#enum-contenttype) | 3 | — | 内容类型 |
| `region` | `string` | 4 | — | 授权地区代码 |
| `start_time` | `int64` | 5 | — | 窗口开始时间（Unix 秒） |
| `end_time` | `int64` | 6 | — | 窗口结束时间（Unix 秒） |
| `operator` | `string` | 7 | — | 操作人 |
| `ip` | `string` | 8 | — | 调用方 IP |

### message `WindowReply`

> 窗口响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `window` | [`Window`](#message-window) | 1 | — | 窗口详情 |

### message `WindowReq`

> 单个窗口查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `window_id` | `int64` | 1 | — | 窗口 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `ListWindowsReq`

> 窗口列表查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容 ID（可选） |
| `contract_id` | `int64` | 2 | — | 合同 ID（可选） |
| `content_type` | [`ContentType`](#enum-contenttype) | 3 | — | 内容类型（可选） |
| `state` | [`WindowState`](#enum-windowstate) | 4 | — | 窗口状态（可选） |
| `pn` | `int32` | 5 | — | 页码 |
| `ps` | `int32` | 6 | — | 每页大小（最大 50） |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `WindowsReply`

> 窗口列表响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `windows` | [`Window`](#message-window) | 2 | repeated | 窗口列表 |

### message `CheckReq`

> --- 可播放性校验 --- / 可播放性校验请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_id` | `int64` | 1 | — | 内容 ID |
| `content_type` | [`ContentType`](#enum-contenttype) | 2 | — | 内容类型 |
| `region` | `string` | 3 | — | 地区代码 |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `CheckReply`

> 可播放性校验响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `playable` | `bool` | 1 | — | 是否可播放 |
| `window_id` | `int64` | 2 | — | 命中的窗口 ID（不可播放时为 0） |
| `end_time` | `int64` | 3 | — | 命中窗口的结束时间（Unix 秒，供调用方做缓存 TTL 上限） |

### message `ListExpiringReq`

> --- 过期与即将过期 --- / 即将过期窗口查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `within_seconds` | `int32` | 1 | — | 距离过期的秒数窗口（默认 3600） |
| `pn` | `int32` | 2 | — | 页码 |
| `ps` | `int32` | 3 | — | 每页大小（最大 100） |
| `ip` | `string` | 4 | — | 调用方 IP |
