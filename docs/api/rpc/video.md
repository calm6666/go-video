# RPC · `video`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/video/rpc/video.proto` |
| protobuf 包 | `video.v1` |
| go_package | `go-video/services/video/rpc` |
| 发现用的 etcd key | `video.v1.rpc`（`services/video/etc/video.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`video.v1.rpc`） |
| 监听 | `8095`（`services/video/etc/video.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_video` |
| 方法数 | 8（service `Video`） |
| 网关消费方 | `app:VideoRPC`、`admin:VideoRPC` |

## 契约说明

> 说明：video 服务持有 UGC/PUGC 稿件、版本与发布状态。
> 依据 AGENTS.md §1，不实现商业化（会员、订单、支付、投币、广告）；
> 依据 §5，本服务只写自己的 video_submission/video_version/video_audit_log，
> 不直接改 catalog、recommend、engagement 等其他服务的可变数据。
> 依据 §8，TransitionState 只能推进合法状态，不能直接写入 PUBLISHED。

## service `Video`

> Video 稿件主服务。 / 依据 AGENTS.md §5，本服务只写 video_submission、video_version、video_audit_log； / 依据 §8，状态推进严格走合法状态机，禁止直接写 PUBLISHED。

gRPC 方法前缀：`video.v1.Video/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `CreateSubmission` | [`CreateSubmissionReq`](#message-createsubmissionreq) | [`SubmissionReply`](#message-submissionreply) | 创建稿件（DRAFT 状态），返回新稿件 |
| 2 | `GetSubmission` | [`SubmissionReq`](#message-submissionreq) | [`SubmissionReply`](#message-submissionreply) | 查询稿件详情 |
| 3 | `ListSubmissions` | [`ListReq`](#message-listreq) | [`SubmissionsReply`](#message-submissionsreply) | 分页查询稿件（按 mid 或 typeid 过滤） |
| 4 | `UpdateSubmission` | [`UpdateSubmissionReq`](#message-updatesubmissionreq) | [`SubmissionReply`](#message-submissionreply) | 更新稿件元信息（仅 DRAFT 可改） |
| 5 | `DeleteSubmission` | [`SubmissionReq`](#message-submissionreq) | [`EmptyReply`](#message-emptyreply) | 删除稿件（状态流转到 DELETED，保留审计） |
| 6 | `TransitionState` | [`TransitionReq`](#message-transitionreq) | [`SubmissionReply`](#message-submissionreply) | 推进稿件状态机（校验合法转换，非法转换返回 ErrInvalidStateTransition） |
| 7 | `ListByState` | [`ListByStateReq`](#message-listbystatereq) | [`SubmissionsReply`](#message-submissionsreply) | 按状态查询稿件（运营/系统用） |
| 8 | `GetPlayableSource` | [`PlayableSourceReq`](#message-playablesourcereq) | [`PlayableSourceReply`](#message-playablesourcereply) | 查询稿件当前可播放版次（aid → asset_id，供网关解析播放来源） |

## 消息与枚举

### enum `SubmissionState`

> 稿件状态枚举

| 值 | 编号 | 说明 |
|---|---|---|
| `STATE_UNSPECIFIED` | 0 | 未指定 |
| `STATE_DRAFT` | 1 | 草稿 |
| `STATE_UPLOADING` | 2 | 上传中 |
| `STATE_UPLOADED` | 3 | 上传完成 |
| `STATE_SCANNING` | 4 | 扫描中 |
| `STATE_TRANSCODING` | 5 | 转码中 |
| `STATE_READY_FOR_REVIEW` | 6 | 待审核 |
| `STATE_REJECTED` | 7 | 审核驳回 |
| `STATE_APPEAL` | 8 | 申诉中 |
| `STATE_APPROVED` | 9 | 审核通过 |
| `STATE_SCHEDULED` | 10 | 定时发布 |
| `STATE_PUBLISHED` | 11 | 已发布 |
| `STATE_OFFLINE` | 12 | 下架 |
| `STATE_EXPIRED` | 13 | 过期 |
| `STATE_DELETED` | 14 | 删除 |

### message `EmptyReply`

> 空响应

（空消息）

### message `Submission`

> 稿件信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `mid` | `int64` | 2 | — | 投稿用户 ID |
| `title` | `string` | 3 | — | 标题 |
| `desc` | `string` | 4 | — | 简介 |
| `cover` | `string` | 5 | — | 封面 URL |
| `typeid` | `int32` | 6 | — | 分区 ID |
| `tag` | `string` | 7 | — | 标签（逗号分隔） |
| `state` | [`SubmissionState`](#enum-submissionstate) | 8 | — | 当前状态 |
| `ctime` | `int64` | 9 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 10 | — | 修改时间（Unix 秒） |

### message `VideoVersion`

> 稿件版本信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `version` | `int64` | 2 | — | 版本号 |
| `asset_id` | `string` | 3 | — | 关联媒资 ID |
| `state` | [`SubmissionState`](#enum-submissionstate) | 4 | — | 版本状态 |
| `ctime` | `int64` | 5 | — | 创建时间（Unix 秒） |

### message `CreateSubmissionReq`

> --- 创建稿件 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 投稿用户 ID |
| `title` | `string` | 2 | — | 标题 |
| `desc` | `string` | 3 | — | 简介 |
| `cover` | `string` | 4 | — | 封面 URL |
| `typeid` | `int32` | 5 | — | 分区 ID |
| `tag` | `string` | 6 | — | 标签 |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `SubmissionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `submission` | [`Submission`](#message-submission) | 1 | — | 稿件详情 |

### message `SubmissionReq`

> --- 查询稿件详情 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `mid` | `int64` | 2 | — | 调用方用户 ID（鉴权与所有者校验） |
| `ip` | `string` | 3 | — | 调用方 IP |

### message `ListReq`

> --- 分页查询稿件 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 按投稿人过滤（0 不限） |
| `typeid` | `int32` | 2 | — | 按分区过滤（0 不限） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `SubmissionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `submissions` | [`Submission`](#message-submission) | 2 | repeated | 稿件列表 |

### message `UpdateSubmissionReq`

> --- 更新稿件元信息 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `mid` | `int64` | 2 | — | 调用方用户 ID（必须是所有者） |
| `title` | `string` | 3 | — | 标题（空表示不更新） |
| `desc` | `string` | 4 | — | 简介（空表示不更新） |
| `cover` | `string` | 5 | — | 封面 URL（空表示不更新） |
| `typeid` | `int32` | 6 | — | 分区 ID（0 表示不更新） |
| `tag` | `string` | 7 | — | 标签（空表示不更新） |
| `ip` | `string` | 8 | — | 调用方 IP |

### message `TransitionReq`

> --- 推进状态机 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `target` | [`SubmissionState`](#enum-submissionstate) | 2 | — | 目标状态 |
| `operator` | `string` | 3 | — | 操作人（运营/系统/回调来源） |
| `reason` | `string` | 4 | — | 变更原因 |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `ListByStateReq`

> --- 按状态查询稿件 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`SubmissionState`](#enum-submissionstate) | 1 | — | 状态过滤 |
| `pn` | `int32` | 2 | — | 页码（从 1 开始） |
| `ps` | `int32` | 3 | — | 每页大小（最大 50） |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `PlayableSourceReq`

> --- 查询可播放来源 --- / PlayableSourceReq 供 gateway/app 在签发播放地址前解析 aid → asset_id。 / 依据 AGENTS.md §5，稿件与媒资版次的映射（video_version）归本服务所有， / playback/asset/transcode 不得自行猜测；playback 只接收网关解析出的 object_key。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `mid` | `int64` | 2 | — | 请求者用户 ID（0 游客；仅日志与排障，不做起点鉴权） |
| `ip` | `string` | 3 | — | 调用方 IP |

### message `PlayableSourceReply`

> PlayableSourceReply 返回当前可对外播放的版次。 / 只有稿件处于 PUBLISHED 且最新版次带非空 asset_id 才返回来源； / 定时发布（SCHEDULED）在状态机推进为 PUBLISHED 之前不可播放， / 对它返回 video: submission not playable，避免网关绕过 §8 提前放流。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `aid` | `int64` | 1 | — | 稿件 ID |
| `version` | [`VideoVersion`](#message-videoversion) | 2 | — | 当前可播放版次（version.asset_id 为媒资 ID） |
| `submission_state` | [`SubmissionState`](#enum-submissionstate) | 3 | — | 稿件状态（便于网关区分排障原因） |
