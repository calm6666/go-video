# RPC · `transcode`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/transcode/rpc/transcode.proto` |
| protobuf 包 | `transcode.v1` |
| go_package | `go-video/services/transcode/rpc` |
| 发现用的 etcd key | `transcode.v1.rpc`（`services/transcode/etc/transcode.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`transcode.v1.rpc`） |
| 监听 | `8100`（`services/transcode/etc/transcode.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_transcode` |
| 方法数 | 7（service `Transcode`） |
| 网关消费方 | `app:TranscodeRPC`、`admin:TranscodeRPC` |

## 契约说明

> transcode 服务负责转码任务和播放版本管理。
> 依据 AGENTS.md §5，transcode 拥有"转码任务和播放版本"数据；
> 依据 §8，转码完成才能让稿件进入 READY_FOR_REVIEW，但本服务不直接写稿件状态。
> 本期 SubmitTask 为占位实现：仅创建 PENDING 任务，不调用 FFmpeg；
> 实际转码由独立 Worker 消费 MQ 拉起 FFmpeg（本期不实现 Worker）。

## service `Transcode`

> Transcode 转码任务与播放版本管理服务。 / 依据 AGENTS.md §5，本服务只持有转码任务和模板； / 依据 §1，不实现任何商业化能力。

gRPC 方法前缀：`transcode.v1.Transcode/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `SubmitTask` | [`SubmitTaskReq`](#message-submittaskreq) | [`TaskReply`](#message-taskreply) | 创建转码任务（PENDING 状态，本期占位不调用 FFmpeg） |
| 2 | `GetTask` | [`TaskReq`](#message-taskreq) | [`TaskReply`](#message-taskreply) | 查询任务详情 |
| 3 | `ListTasks` | [`ListReq`](#message-listreq) | [`TasksReply`](#message-tasksreply) | 分页查询任务（按 asset_id 或 state 过滤） |
| 4 | `UpdateProgress` | [`UpdateProgressReq`](#message-updateprogressreq) | [`TaskReply`](#message-taskreply) | Worker 上报进度（校验状态机：PENDING→PROCESSING→SUCCEEDED/FAILED） |
| 5 | `ListTemplates` | [`ListTemplatesReq`](#message-listtemplatesreq) | [`TemplatesReply`](#message-templatesreply) | 分页查询模板列表 |
| 6 | `GetTemplate` | [`TemplateReq`](#message-templatereq) | [`TemplateReply`](#message-templatereply) | 查询模板详情 |
| 7 | `CreateTemplate` | [`CreateTemplateReq`](#message-createtemplatereq) | [`TemplateReply`](#message-templatereply) | 运营创建模板 |

## 消息与枚举

### enum `TaskState`

> 转码任务状态机：PENDING → PROCESSING → SUCCEEDED/FAILED

| 值 | 编号 | 说明 |
|---|---|---|
| `TASK_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TASK_STATE_PENDING` | 1 | 待处理（任务已创建，等待 Worker 拉起） |
| `TASK_STATE_PROCESSING` | 2 | 处理中（Worker 已开始转码） |
| `TASK_STATE_SUCCEEDED` | 3 | 成功（转码完成，可推进稿件状态机） |
| `TASK_STATE_FAILED` | 4 | 失败（终态，需重试或人工介入） |

### message `SubmitTaskReq`

> 创建转码任务请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID（由 asset 服务分配） |
| `template_id` | `int64` | 2 | — | 转码模板 ID |
| `input_bucket` | `string` | 3 | — | 输入对象存储桶 |
| `input_key` | `string` | 4 | — | 输入对象 key |
| `output_bucket` | `string` | 5 | — | 输出对象存储桶 |
| `output_key` | `string` | 6 | — | 输出对象 key |

### message `TaskReply`

> 转码任务详情

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `asset_id` | `int64` | 2 | — | 媒资 ID |
| `template_id` | `int64` | 3 | — | 转码模板 ID |
| `input_bucket` | `string` | 4 | — | 输入对象存储桶 |
| `input_key` | `string` | 5 | — | 输入对象 key |
| `output_bucket` | `string` | 6 | — | 输出对象存储桶 |
| `output_key` | `string` | 7 | — | 输出对象 key |
| `state` | [`TaskState`](#enum-taskstate) | 8 | — | 任务状态 |
| `progress` | `int32` | 9 | — | 进度（0-100） |
| `errno` | `int32` | 10 | — | 错误码（失败时填） |
| `err_msg` | `string` | 11 | — | 错误信息（失败时填） |
| `ctime` | `int64` | 12 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 13 | — | 修改时间（Unix 秒） |

### message `TaskReq`

> 查询单个任务请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |

### message `ListReq`

> 任务分页查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 按媒资 ID 过滤（<=0 表示不过滤） |
| `state` | [`TaskState`](#enum-taskstate) | 2 | — | 按状态过滤（UNSPECIFIED 表示不过滤） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |

### message `TasksReply`

> 任务分页查询响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `tasks` | [`TaskReply`](#message-taskreply) | 2 | repeated | 任务列表 |

### message `UpdateProgressReq`

> Worker 上报进度请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `progress` | `int32` | 2 | — | 进度（0-100） |
| `state` | [`TaskState`](#enum-taskstate) | 3 | — | 目标状态（PROCESSING/SUCCEEDED/FAILED） |
| `errno` | `int32` | 4 | — | 错误码（state=FAILED 时填） |
| `err_msg` | `string` | 5 | — | 错误信息（state=FAILED 时填） |

### message `TemplateReply`

> 转码模板详情

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template_id` | `int64` | 1 | — | 模板 ID |
| `name` | `string` | 2 | — | 模板名 |
| `codec` | `string` | 3 | — | 编码器（如 h264/hevc/aac） |
| `width` | `int32` | 4 | — | 视频宽（0 表示自适应） |
| `height` | `int32` | 5 | — | 视频高（0 表示自适应） |
| `bitrate` | `int32` | 6 | — | 目标码率（kbps，0 表示自适应） |
| `fps` | `int32` | 7 | — | 帧率（0 表示跟随源） |
| `segment_seconds` | `int32` | 8 | — | HLS 分片时长（秒，0 表示不分片） |

### message `ListTemplatesReq`

> 模板分页查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pn` | `int32` | 1 | — | 页码（从 1 开始） |
| `ps` | `int32` | 2 | — | 每页大小（最大 50） |

### message `TemplatesReply`

> 模板分页查询响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `templates` | [`TemplateReply`](#message-templatereply) | 2 | repeated | 模板列表 |

### message `TemplateReq`

> 查询单个模板请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `template_id` | `int64` | 1 | — | 模板 ID |

### message `CreateTemplateReq`

> 运营创建模板请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `name` | `string` | 1 | — | 模板名 |
| `codec` | `string` | 2 | — | 编码器 |
| `width` | `int32` | 3 | — | 视频宽 |
| `height` | `int32` | 4 | — | 视频高 |
| `bitrate` | `int32` | 5 | — | 目标码率（kbps） |
| `fps` | `int32` | 6 | — | 帧率 |
| `segment_seconds` | `int32` | 7 | — | HLS 分片时长（秒） |
