# RPC · `content-fingerprint`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/content-fingerprint/rpc/fingerprint.proto` |
| protobuf 包 | `fingerprint.v1` |
| go_package | `go-video/services/content-fingerprint/rpc` |
| 发现用的 etcd key | `content-fingerprint.v1.rpc`（`services/content-fingerprint/etc/fingerprint.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`content-fingerprint.v1.rpc`） |
| 监听 | `8101`（`services/content-fingerprint/etc/fingerprint.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_content_fingerprint` |
| 方法数 | 6（service `ContentFingerprint`） |
| 网关消费方 | 未被两个网关的 etcd key 引用（服务间调用或本轮未接线） |

## 契约说明

> content-fingerprint 服务契约。
> 数据所有权：fingerprint_task（指纹抽取任务）、fingerprint_record（指纹记录）。
> 任务状态机：PENDING → SUCCEEDED / FAILED（由 Worker 回调推进）。
> 依据 AGENTS.md §5，本服务仅持有指纹事实与匹配候选，不复制媒资主数据，
> 不直接调用下游 RPC；匹配结果作为审核证据供 moderation-orchestrator 使用。
> 依据 §8，回调只能推进合法状态，不能绕过审核直接下架内容。

## service `ContentFingerprint`

> ContentFingerprint 指纹和重复/侵权匹配服务。 / 依据 AGENTS.md §5，本服务仅持有指纹事实与匹配候选， / 不复制媒资主数据，不调用下游 RPC；匹配结果作为审核证据。

gRPC 方法前缀：`fingerprint.v1.ContentFingerprint/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `SubmitTask` | [`SubmitTaskReq`](#message-submittaskreq) | [`TaskReply`](#message-taskreply) | 创建指纹任务（PENDING）；本期不调用真实指纹算法，只建任务。 |
| 2 | `GetTask` | [`TaskReq`](#message-taskreq) | [`TaskReply`](#message-taskreply) | 查询单个任务详情 |
| 3 | `ListTasks` | [`ListReq`](#message-listreq) | [`TasksReply`](#message-tasksreply) | 分页查询任务（按 asset_id 或 state 过滤） |
| 4 | `UpdateTaskResult` | [`UpdateResultReq`](#message-updateresultreq) | [`TaskReply`](#message-taskreply) | Worker 回写任务结果（PENDING→SUCCEEDED/FAILED），同时写入 fingerprint_record |
| 5 | `MatchByFingerprint` | [`MatchReq`](#message-matchreq) | [`MatchReply`](#message-matchreply) | 按指纹 key 查询匹配的 asset 列表（本期占位：返回空列表） |
| 6 | `MatchByAsset` | [`AssetReq`](#message-assetreq) | [`MatchReply`](#message-matchreply) | 按 asset_id 查询其指纹的所有匹配（本期占位：返回空列表） |

## 消息与枚举

### enum `FpType`

> 指纹类型

| 值 | 编号 | 说明 |
|---|---|---|
| `FP_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `FP_TYPE_VIDEO` | 1 | 视频指纹 |
| `FP_TYPE_AUDIO` | 2 | 音频指纹 |

### enum `TaskState`

> 任务状态

| 值 | 编号 | 说明 |
|---|---|---|
| `TASK_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TASK_STATE_PENDING` | 1 | 待处理（任务已创建，等待 Worker 计算） |
| `TASK_STATE_SUCCEEDED` | 2 | 成功（Worker 已回写指纹结果） |
| `TASK_STATE_FAILED` | 3 | 失败（Worker 计算失败） |

### message `SubmitTaskReq`

> 提交指纹任务请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 关联媒资 ID |
| `fp_type` | [`FpType`](#enum-fptype) | 2 | — | 指纹类型 |
| `trace_id` | `string` | 3 | — | 调用方 trace_id（用于事件溯源） |
| `operator` | `string` | 4 | — | 操作方（系统/Worker 标识） |

### message `TaskReply`

> 任务详情回复

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `asset_id` | `int64` | 2 | — | 关联媒资 ID |
| `fp_type` | [`FpType`](#enum-fptype) | 3 | — | 指纹类型 |
| `video_key` | `string` | 4 | — | 视频指纹 key（SUCCEEDED 后由 Worker 回写） |
| `audio_key` | `string` | 5 | — | 音频指纹 key（SUCCEEDED 后由 Worker 回写） |
| `state` | [`TaskState`](#enum-taskstate) | 6 | — | 任务状态 |
| `ctime` | `int64` | 7 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 8 | — | 修改时间（Unix 秒） |

### message `TaskReq`

> 查询单个任务请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |

### message `ListReq`

> 分页查询任务请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 可选：按 asset_id 过滤（0 表示不过滤） |
| `state` | [`TaskState`](#enum-taskstate) | 2 | — | 可选：按状态过滤（UNSPECIFIED 表示不过滤） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |

### message `TasksReply`

> 任务列表回复

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `tasks` | [`TaskReply`](#message-taskreply) | 2 | repeated | 任务列表 |

### message `UpdateResultReq`

> Worker 回写任务结果请求（PENDING → SUCCEEDED / FAILED）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `video_key` | `string` | 2 | — | 视频指纹 key（FAILED 时可空） |
| `audio_key` | `string` | 3 | — | 音频指纹 key（FAILED 时可空） |
| `video_hash` | `string` | 4 | — | 视频指纹哈希（用于精确比对） |
| `audio_hash` | `string` | 5 | — | 音频指纹哈希 |
| `state` | [`TaskState`](#enum-taskstate) | 6 | — | 目标状态：SUCCEEDED 或 FAILED |
| `operator` | `string` | 7 | — | 操作方（Worker 标识） |

### message `MatchReq`

> 按指纹 key 查询匹配请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `fp_key` | `string` | 1 | — | 指纹 key |
| `fp_type` | [`FpType`](#enum-fptype) | 2 | — | 指纹类型 |
| `top_n` | `int32` | 3 | — | 返回前 N 条（默认 10，最大 50） |

### message `MatchItem`

> 单条匹配命中

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 命中媒资 ID |
| `fp_key` | `string` | 2 | — | 命中指纹 key |
| `fp_hash` | `string` | 3 | — | 命中指纹哈希 |
| `score` | `double` | 4 | — | 相似度评分（0~1，本期占位为 0） |
| `fp_type` | [`FpType`](#enum-fptype) | 5 | — | 指纹类型 |

### message `MatchReply`

> 匹配回复

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`MatchItem`](#message-matchitem) | 1 | repeated | 命中列表（本期占位返回空） |

### message `AssetReq`

> 按 asset_id 查询其指纹的所有匹配请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID |
| `fp_type` | [`FpType`](#enum-fptype) | 2 | — | 可选：限定指纹类型 |
