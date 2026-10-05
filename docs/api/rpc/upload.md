# RPC · `upload`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/upload/rpc/upload.proto` |
| protobuf 包 | `upload.v1` |
| go_package | `go-video/services/upload/rpc` |
| 发现用的 etcd key | `upload.v1.rpc`（`services/upload/etc/upload.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`upload.v1.rpc`） |
| 监听 | `8098`（`services/upload/etc/upload.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_upload` |
| 方法数 | 5（service `Upload`） |
| 网关消费方 | `app:UploadRPC` |

## 契约说明

> upload 服务负责大文件分片上传会话生命周期与 OSS 分片预签名 URL 签发。
> 依据 AGENTS.md §6：客户端直传 OSS，服务端只签发短期预签名 URL，
> 不向客户端下发 OSS 长期密钥；完成上传只推进到 COMPLETED 并发布
> media.task.v1 事件，由 asset/transcode 接管后续状态机。
> 依据 §1：不实现商业化。

## service `Upload`

> Upload 分片上传会话服务。

gRPC 方法前缀：`upload.v1.Upload/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `InitUpload` | [`InitUploadReq`](#message-inituploadreq) | [`InitUploadReply`](#message-inituploadreply) | 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位 |
| 2 | `GetUploadUrl` | [`GetUrlReq`](#message-geturlreq) | [`GetUrlReply`](#message-geturlreply) | 为某分片获取 OSS 预签名 PUT URL（短期，默认 15 分钟有效） |
| 3 | `CompleteUpload` | [`CompleteUploadReq`](#message-completeuploadreq) | [`CompleteUploadReply`](#message-completeuploadreply) | 客户端上传完全部分片后调用，服务端校验分片清单并触发 OSS 完成分片上传； / 返回 asset_id 占位（后续 asset 服务接管），并发布 media.task.v1 事件 |
| 4 | `AbortUpload` | [`AbortUploadReq`](#message-abortuploadreq) | [`EmptyReply`](#message-emptyreply) | 取消上传（删除 OSS 分片） |
| 5 | `GetUploadStatus` | [`UploadStatusReq`](#message-uploadstatusreq) | [`UploadStatusReply`](#message-uploadstatusreply) | 查询上传状态和已完成分片列表 |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `UploadState`

> 上传会话状态

| 值 | 编号 | 说明 |
|---|---|---|
| `UPLOAD_STATE_UNSPECIFIED` | 0 | 未指定 |
| `UPLOAD_STATE_INITIALIZED` | 1 | 已初始化，等待分片上传 |
| `UPLOAD_STATE_UPLOADING` | 2 | 上传中 |
| `UPLOAD_STATE_COMPLETED` | 3 | 已完成（OSS 分片合并成功） |
| `UPLOAD_STATE_ABORTED` | 4 | 已取消 |
| `UPLOAD_STATE_FAILED` | 5 | 失败 |

### enum `ChunkState`

> 分片状态

| 值 | 编号 | 说明 |
|---|---|---|
| `CHUNK_STATE_UNSPECIFIED` | 0 | 未指定 |
| `CHUNK_STATE_PENDING` | 1 | 待上传 |
| `CHUNK_STATE_UPLOADED` | 2 | 已上传到 OSS |
| `CHUNK_STATE_VERIFIED` | 3 | 已校验（ETag 匹配） |

### message `Chunk`

> 分片信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `chunk_no` | `int32` | 1 | — | 分片序号（从 1 开始） |
| `size` | `int64` | 2 | — | 分片大小（字节） |
| `etag` | `string` | 3 | — | 分片 ETag（OSS 返回） |
| `state` | [`ChunkState`](#enum-chunkstate) | 4 | — | 分片状态 |
| `ctime` | `int64` | 5 | — | 创建时间（Unix 秒） |

### message `InitUploadReq`

> --- InitUpload --- / 初始化上传会话请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `filename` | `string` | 2 | — | 文件名 |
| `size` | `int64` | 3 | — | 文件总大小（字节） |
| `typeid` | `int32` | 4 | — | 稿件类型 ID |
| `md5` | `string` | 5 | — | 文件 MD5（用于秒传判断，可选） |
| `chunk_size` | `int64` | 6 | — | 分片大小（字节，0 由服务端决定） |
| `total_chunks` | `int32` | 7 | — | 分片总数 |
| `ip` | `string` | 8 | — | 调用方 IP |

### message `InitUploadReply`

> 初始化上传会话响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `bucket` | `string` | 2 | — | OSS bucket |
| `object_key` | `string` | 3 | — | OSS 对象 key 占位 |
| `upload_protocol` | `string` | 4 | — | 上传协议（如 "multipart"） |
| `chunk_size` | `int64` | 5 | — | 服务端确定的分片大小 |
| `total_chunks` | `int32` | 6 | — | 分片总数 |
| `instant` | `bool` | 7 | — | 是否秒传命中（true 时无需上传） |

### message `GetUrlReq`

> --- GetUploadUrl --- / 获取分片预签名 URL 请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `chunk_no` | `int32` | 2 | — | 分片序号（从 1 开始） |
| `chunk_size` | `int64` | 3 | — | 分片大小（字节） |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `GetUrlReply`

> 获取分片预签名 URL 响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `chunk_no` | `int32` | 2 | — | 分片序号 |
| `url` | `string` | 3 | — | 预签名 PUT URL（短期有效） |
| `method` | `string` | 4 | — | HTTP 方法（PUT） |
| `expiration` | `int64` | 5 | — | URL 过期时间（Unix 秒） |
| `headers` | `map<string, string>` | 6 | — | 上传需附带的 header |

### message `ChunkPart`

> --- CompleteUpload --- / 分片清单条目

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `chunk_no` | `int32` | 1 | — | 分片序号 |
| `etag` | `string` | 2 | — | OSS 返回的 ETag |

### message `CompleteUploadReq`

> 完成上传请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `parts` | [`ChunkPart`](#message-chunkpart) | 2 | repeated | 分片清单 |
| `md5` | `string` | 3 | — | 完整文件 MD5 |
| `ip` | `string` | 4 | — | 调用方 IP |

### message `CompleteUploadReply`

> 完成上传响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `asset_id` | `string` | 2 | — | asset_id 占位（asset 服务接管后回填真实 ID） |
| `object_key` | `string` | 3 | — | OSS 对象 key |
| `size` | `int64` | 4 | — | 文件大小 |
| `md5` | `string` | 5 | — | 文件 MD5 |
| `state` | [`UploadState`](#enum-uploadstate) | 6 | — | 会话状态 |

### message `AbortUploadReq`

> --- AbortUpload --- / 取消上传请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `UploadStatusReq`

> --- GetUploadStatus --- / 查询上传状态请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `UploadStatusReply`

> 查询上传状态响应

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `string` | 1 | — | 上传会话 ID |
| `state` | [`UploadState`](#enum-uploadstate) | 2 | — | 会话状态 |
| `size` | `int64` | 3 | — | 总大小 |
| `uploaded_size` | `int64` | 4 | — | 已上传大小 |
| `total_chunks` | `int32` | 5 | — | 分片总数 |
| `completed_chunks` | `int32` | 6 | — | 已完成分片数 |
| `chunks` | [`Chunk`](#message-chunk) | 7 | repeated | 分片列表 |
| `asset_id` | `string` | 8 | — | 关联 asset_id（若已完成） |
