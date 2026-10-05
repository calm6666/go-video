# RPC · `moderation-orchestrator`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/moderation-orchestrator/rpc/moderation.proto` |
| protobuf 包 | `moderation.v1` |
| go_package | `go-video/services/moderation-orchestrator/rpc` |
| 发现用的 etcd key | `moderation.v1.rpc`（`services/moderation-orchestrator/etc/moderation.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`moderation.v1.rpc`） |
| 监听 | `8093`（`services/moderation-orchestrator/etc/moderation.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_moderation` |
| 方法数 | 7（service `ModerationOrchestrator`） |
| 网关消费方 | `app:ModerationRPC`、`admin:ModerationRPC` |

## 契约说明

> moderation-orchestrator 是审核编排服务，依据 AGENTS.md §8 稿件状态机：
>   DRAFT → UPLOADING → UPLOADED → SCANNING → TRANSCODING
>         → READY_FOR_REVIEW → APPROVED → SCHEDULED → PUBLISHED
>                            └→ REJECTED/APPEAL
> 本服务只负责创建任务、派发 worker、回写审核结论、申诉流程，
> 不直接做 OCR/ASR/图像识别，不直接把稿件置为 PUBLISHED。
> 内容所有者（video/catalog/comment/danmaku）消费 moderation.result.v1
> 推进合法状态，禁止 worker 回调直接写 PUBLISHED。

## service `ModerationOrchestrator`

> ModerationOrchestrator 审核编排服务。 / 依据 AGENTS.md §5，本服务拥有审核任务、规则、结论、申诉数据； / 依据 §8，审核结论只能推进合法状态，不能直接写 PUBLISHED。

gRPC 方法前缀：`moderation.v1.ModerationOrchestrator/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `SubmitForReview` | [`SubmitReq`](#message-submitreq) | [`TaskReply`](#message-taskreply) | 领域服务提交审核任务（创建 task 并入 MQ 待处理） |
| 2 | `GetTask` | [`TaskReq`](#message-taskreq) | [`TaskReply`](#message-taskreply) | 查询任务详情 |
| 3 | `GetResult` | [`ResultReq`](#message-resultreq) | [`ResultReply`](#message-resultreply) | 查询审核结论 |
| 4 | `ListTasks` | [`ListTasksReq`](#message-listtasksreq) | [`TasksReply`](#message-tasksreply) | 分页查询任务列表（运营后台用） |
| 5 | `SubmitAppeal` | [`AppealReq`](#message-appealreq) | [`AppealReply`](#message-appealreply) | 提交申诉 |
| 6 | `ProcessAppeal` | [`ProcessAppealReq`](#message-processappealreq) | [`AppealReply`](#message-appealreply) | 处理申诉（运营） |
| 7 | `SubmitWorkerResult` | [`WorkerResultReq`](#message-workerresultreq) | [`EmptyReply`](#message-emptyreply) | worker 调用，回写识别结果（由 moderation-worker 调用） |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `ContentType`

> 内容类型：本服务审核对象来源域

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTENT_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `CONTENT_TYPE_VIDEO` | 1 | 视频稿件（UGC/PGC 投稿） |
| `CONTENT_TYPE_CATALOG` | 2 | 版权目录条目 |
| `CONTENT_TYPE_COMMENT` | 3 | 评论 |
| `CONTENT_TYPE_DANMAKU` | 4 | 弹幕 |
| `CONTENT_TYPE_LIVE` | 5 | 直播 |

### enum `TaskState`

> 任务状态机：未审核 → 审核中 → 已结论 → 申诉中

| 值 | 编号 | 说明 |
|---|---|---|
| `TASK_STATE_UNSPECIFIED` | 0 | 未指定 |
| `TASK_STATE_PENDING` | 1 | 待处理（已入队，等 worker 拉取） |
| `TASK_STATE_PROCESSING` | 2 | 处理中（worker 已开始识别） |
| `TASK_STATE_DONE` | 3 | 已完成（已回写结论） |
| `TASK_STATE_APPEALED` | 4 | 申诉中 |
| `TASK_STATE_APPEAL_DONE` | 5 | 申诉已处理 |
| `TASK_STATE_CANCELED` | 9 | 已撤销 |

### enum `Verdict`

> 审核结论

| 值 | 编号 | 说明 |
|---|---|---|
| `VERDICT_UNSPECIFIED` | 0 | 未指定 |
| `VERDICT_PASS` | 1 | 通过 |
| `VERDICT_REVIEW` | 2 | 转人审 |
| `VERDICT_REJECT` | 3 | 拒绝 |

### message `Task`

> 审核任务

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `submission_id` | `int64` | 2 | — | 提交对象 ID（稿件/评论/弹幕等的内容主键） |
| `content_type` | [`ContentType`](#enum-contenttype) | 3 | — | 内容类型 |
| `mid` | `int64` | 4 | — | 提交用户 ID |
| `up_mid` | `int64` | 5 | — | UP 主 ID（视频场景下与 mid 可能相同） |
| `business` | `string` | 6 | — | 业务名（与 submission_id 共同定位对象） |
| `reason` | `string` | 7 | — | 提交审核原因（如：发布、举报、复审） |
| `state` | [`TaskState`](#enum-taskstate) | 8 | — | 任务状态 |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |
| `operator` | `int64` | 11 | — | 操作人（运营 ID，0 表示系统） |

### message `Result`

> 审核结论

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 关联任务 ID |
| `verdict` | [`Verdict`](#enum-verdict) | 2 | — | 结论 |
| `reason` | `string` | 3 | — | 结论原因（关键词命中、模型分数等） |
| `worker_id` | `int64` | 4 | — | worker 实例 ID（机审） |
| `reviewer` | `int64` | 5 | — | 人审员 ID（人审） |
| `ctime` | `int64` | 6 | — | 创建时间（Unix 秒） |

### message `Appeal`

> 申诉

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `appeal_id` | `int64` | 1 | — | 申诉 ID |
| `task_id` | `int64` | 2 | — | 关联任务 ID |
| `mid` | `int64` | 3 | — | 申诉人 ID |
| `content` | `string` | 4 | — | 申诉理由 |
| `final_verdict` | [`Verdict`](#enum-verdict) | 5 | — | 最终结论（处理后） |
| `final_reason` | `string` | 6 | — | 处理说明 |
| `handler` | `int64` | 7 | — | 处理人（运营 ID） |
| `ctime` | `int64` | 8 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 9 | — | 处理时间（Unix 秒） |

### message `SubmitReq`

> --- 提交审核（领域服务调用） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `submission_id` | `int64` | 1 | — | 提交对象 ID |
| `content_type` | [`ContentType`](#enum-contenttype) | 2 | — | 内容类型 |
| `mid` | `int64` | 3 | — | 提交用户 ID |
| `up_mid` | `int64` | 4 | — | UP 主 ID |
| `business` | `string` | 5 | — | 业务名 |
| `reason` | `string` | 6 | — | 提交原因 |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `TaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task` | [`Task`](#message-task) | 1 | — | — |

### message `TaskReq`

> --- 查询任务详情 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `ResultReq`

> --- 查询审核结论 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 任务 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `ResultReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `result` | [`Result`](#message-result) | 1 | — | — |

### message `ListTasksReq`

> --- 分页查询任务列表（运营后台） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 按提交用户过滤（0 不过滤） |
| `content_type` | [`ContentType`](#enum-contenttype) | 2 | — | 按内容类型过滤（0 不过滤） |
| `state` | [`TaskState`](#enum-taskstate) | 3 | — | 按状态过滤（0 不过滤） |
| `pn` | `int32` | 4 | — | 页码（从 1 开始） |
| `ps` | `int32` | 5 | — | 每页大小（最大 50） |
| `ip` | `string` | 6 | — | 调用方 IP |

### message `TasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `tasks` | [`Task`](#message-task) | 1 | repeated | 任务列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `AppealReq`

> --- 提交申诉 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 关联任务 ID |
| `mid` | `int64` | 2 | — | 申诉人 ID |
| `content` | `string` | 3 | — | 申诉理由 |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `AppealReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `appeal` | [`Appeal`](#message-appeal) | 1 | — | — |

### message `ProcessAppealReq`

> --- 处理申诉（运营） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `appeal_id` | `int64` | 1 | — | 申诉 ID |
| `handler` | `int64` | 2 | — | 处理人（运营 ID） |
| `final_verdict` | [`Verdict`](#enum-verdict) | 3 | — | 最终结论 |
| `final_reason` | `string` | 4 | — | 处理说明 |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `WorkerResultReq`

> --- worker 回写结果 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 关联任务 ID |
| `worker_id` | `int64` | 2 | — | worker 实例 ID |
| `verdict` | [`Verdict`](#enum-verdict) | 3 | — | 结论 |
| `reason` | `string` | 4 | — | 结论原因 |
| `ip` | `string` | 5 | — | 调用方 IP |
