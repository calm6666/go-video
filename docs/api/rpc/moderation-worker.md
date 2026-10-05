# RPC · `moderation-worker`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/moderation-worker/rpc/worker.proto` |
| protobuf 包 | `moderation.worker.v1` |
| go_package | `go-video/services/moderation-worker/rpc` |
| 发现用的 etcd key | `moderation-worker.v1.rpc`（`services/moderation-worker/etc/moderation.worker.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`moderation-worker.v1.rpc`） |
| 监听 | `8094`（`services/moderation-worker/etc/moderation.worker.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_moderation_worker` |
| 方法数 | 5（service `ModerationWorker`） |
| 网关消费方 | 未被两个网关的 etcd key 引用（服务间调用或本轮未接线） |

## 契约说明

> 说明：本契约定义 moderation-worker 的对外 RPC。
> 数据所有权：worker_task（识别任务执行记录，关联 orchestrator task_id）。
> 调用方向：moderation-orchestrator → 本服务，被动接收任务，不主动调用其它服务。
> 依据 AGENTS.md §8，Worker 不做最终发布决策；回调只能推进合法状态。
> 本期 Run* 方法为占位实现，返回空结果 + TODO 注释，真实算法后续接入。

## service `ModerationWorker`

> ModerationWorker 内容识别 Worker 服务。 / 被动接收 moderation-orchestrator 调用。 / 本期 Run* 方法占位返回空结果 + TODO，真实算法（OCR/ASR/图像/音频）后续接入。

gRPC 方法前缀：`moderation.worker.v1.ModerationWorker/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RunOCR` | [`RunTaskReq`](#message-runtaskreq) | [`TaskResultReply`](#message-taskresultreply) | 执行 OCR 文本识别（本期占位） |
| 2 | `RunASR` | [`RunTaskReq`](#message-runtaskreq) | [`TaskResultReply`](#message-taskresultreply) | 执行 ASR 语音识别（本期占位） |
| 3 | `RunImage` | [`RunTaskReq`](#message-runtaskreq) | [`TaskResultReply`](#message-taskresultreply) | 执行图像识别（本期占位） |
| 4 | `RunAudio` | [`RunTaskReq`](#message-runtaskreq) | [`TaskResultReply`](#message-taskresultreply) | 执行音频识别（本期占位） |
| 5 | `GetTaskResult` | [`TaskResultReq`](#message-taskresultreq) | [`TaskResultReply`](#message-taskresultreply) | 查询任务执行结果 |

## 消息与枚举

### enum `CapabilityType`

> 识别能力类型

| 值 | 编号 | 说明 |
|---|---|---|
| `CAPABILITY_UNSPECIFIED` | 0 | 未指定 |
| `CAPABILITY_OCR` | 1 | 文本识别 OCR |
| `CAPABILITY_ASR` | 2 | 语音识别 ASR |
| `CAPABILITY_IMAGE` | 3 | 图像识别 |
| `CAPABILITY_AUDIO` | 4 | 音频识别 |

### enum `TaskState`

> 任务执行状态

| 值 | 编号 | 说明 |
|---|---|---|
| `TASK_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TASK_STATE_PENDING` | 1 | 待执行 |
| `TASK_STATE_RUNNING` | 2 | 执行中 |
| `TASK_STATE_SUCCEEDED` | 3 | 成功 |
| `TASK_STATE_FAILED` | 4 | 失败 |
| `TASK_STATE_TIMEOUT` | 5 | 超时 |

### message `RunTaskReq`

> Run* 方法公共请求（由 moderation-orchestrator 触发）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `string` | 1 | — | orchestrator 任务 ID（外部追踪号） |
| `worker_task_id` | `string` | 2 | — | worker 内部任务 ID（幂等键；为空时由 worker 生成） |
| `capability` | [`CapabilityType`](#enum-capabilitytype) | 3 | — | 识别能力（必须与调用的 Run* 方法一致） |
| `media_uri` | `string` | 4 | — | 媒体访问 URI（OSS/COS 路径或预签名 URL） |
| `duration_ms` | `int64` | 5 | — | 媒体时长（毫秒，0 表示未知） |
| `params` | `map<string, string>` | 6 | — | 算法参数（如 language、model_version、采样间隔） |
| `timeout_ms` | `int64` | 7 | — | 单任务超时（毫秒，0 表示使用默认） |
| `trace_id` | `string` | 8 | — | 调用方 trace_id（透传给算法侧） |

### message `ResultSegment`

> 识别结果片段（OCR/ASR/图像/音频通用）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `start_ms` | `int64` | 1 | — | 起始时间（毫秒，静态内容为 0） |
| `end_ms` | `int64` | 2 | — | 结束时间（毫秒） |
| `text` | `string` | 3 | — | 识别文本（OCR/ASR/音频转写） |
| `confidence` | `float` | 4 | — | 置信度 [0,1] |
| `label` | `string` | 5 | — | 命中标签（图像/音频识别用） |
| `extra` | `string` | 6 | — | 附加结构化 JSON（如 bbox、说话人 ID 等） |

### message `TaskResultReply`

> Run* 方法返回的执行结果（也是 GetTaskResult 的响应）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `worker_task_id` | `string` | 1 | — | worker 内部任务 ID |
| `task_id` | `string` | 2 | — | orchestrator 任务 ID |
| `capability` | [`CapabilityType`](#enum-capabilitytype) | 3 | — | 识别能力 |
| `state` | [`TaskState`](#enum-taskstate) | 4 | — | 最终状态 |
| `algorithm_version` | `string` | 5 | — | 算法版本（本期占位为空） |
| `elapsed_ms` | `int64` | 6 | — | 执行耗时（毫秒） |
| `segments` | [`ResultSegment`](#message-resultsegment) | 7 | repeated | 结构化结果片段 |
| `error_message` | `string` | 8 | — | 失败原因（state=FAILED/TIMEOUT 时填） |

### message `TaskResultReq`

> 结果查询请求（按 worker_task_id 优先，否则按 task_id 反查）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `worker_task_id` | `string` | 1 | — | worker 内部任务 ID（优先） |
| `task_id` | `string` | 2 | — | orchestrator 任务 ID（worker_task_id 为空时使用） |
