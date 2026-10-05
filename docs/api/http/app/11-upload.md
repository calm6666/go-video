# 终端面 · `/upload`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| upload 域聚合（services/upload/rpc/upload.proto） | 免鉴权 | 5 |

合计 **5** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## upload 域聚合（services/upload/rpc/upload.proto）（免鉴权，5 条）

> upload 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/upload/init` | 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位 | `initUpload` | `inituploadlogic.go` |
| GET | `/upload/url` | 为某分片获取 OSS 预签名 PUT URL（短期） | `getUploadUrl` | `getuploadurllogic.go` |
| POST | `/upload/complete` | 完成上传：校验分片清单并触发 OSS 完成分片上传 | `completeUpload` | `completeuploadlogic.go` |
| POST | `/upload/abort` | 取消上传（删除 OSS 分片） | `abortUpload` | `abortuploadlogic.go` |
| GET | `/upload/status` | 查询上传状态和已完成分片列表 | `getUploadStatus` | `getuploadstatuslogic.go` |

### POST `/upload/init` — 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/inituploadhandler.go`
- 业务实现：`gateway/app/internal/logic/inituploadlogic.go`

请求：`ParamInitUpload`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Filename` | `filename` | form | `string` | 是 | — | — |
| `Size` | `size` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Md5` | `md5` | form | `string` | 是 | — | — |
| `ChunkSize` | `chunk_size` | form | `int64` | 是 | — | — |
| `TotalChunks` | `total_chunks` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`UploadInitResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadInitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/upload/url` — 为某分片获取 OSS 预签名 PUT URL（短期）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getuploadurlhandler.go`
- 业务实现：`gateway/app/internal/logic/getuploadurllogic.go`

请求：`ParamGetUploadUrl`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `ChunkNo` | `chunk_no` | form | `int32` | 是 | — | — |
| `ChunkSize` | `chunk_size` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`UploadUrlResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadUrlData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/upload/complete` — 完成上传：校验分片清单并触发 OSS 完成分片上传

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/completeuploadhandler.go`
- 业务实现：`gateway/app/internal/logic/completeuploadlogic.go`

请求：`ParamCompleteUpload`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `Parts` | `parts` | json | `[]UploadChunkPart` | 是 | — | — |
| `Md5` | `md5` | json | `string` | 是 | — | — |
| `IP` | `ip` | json | `string` | 是 | — | — |

响应：`UploadCompleteResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadCompleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/upload/abort` — 取消上传（删除 OSS 分片）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/abortuploadhandler.go`
- 业务实现：`gateway/app/internal/logic/abortuploadlogic.go`

请求：`ParamAbortUpload`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/upload/status` — 查询上传状态和已完成分片列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getuploadstatushandler.go`
- 业务实现：`gateway/app/internal/logic/getuploadstatuslogic.go`

请求：`ParamUploadStatus`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`UploadStatusResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamInitUpload`

> upload 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Filename` | `filename` | form | `string` | 是 | — | — |
| `Size` | `size` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Md5` | `md5` | form | `string` | 是 | — | — |
| `ChunkSize` | `chunk_size` | form | `int64` | 是 | — | — |
| `TotalChunks` | `total_chunks` | form | `int32` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `UploadInitResponse`

> upload 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadInitData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamGetUploadUrl`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `ChunkNo` | `chunk_no` | form | `int32` | 是 | — | — |
| `ChunkSize` | `chunk_size` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `UploadUrlResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadUrlData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCompleteUpload`

> 分片清单是结构体数组，go-zero 的 form 绑定只能取标量（实测报 type mismatch for field "parts"）， / 因此完成上传整段走 JSON 请求体，不用 form 标签。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `Parts` | `parts` | json | `[]UploadChunkPart` | 是 | — | — |
| `Md5` | `md5` | json | `string` | 是 | — | — |
| `IP` | `ip` | json | `string` | 是 | — | — |

### `UploadCompleteResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadCompleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAbortUpload`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUploadStatus`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `UploadStatusResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `UploadStatusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `UploadInitData`

> upload 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `Bucket` | `bucket` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `UploadProtocol` | `upload_protocol` | json | `string` | 是 | — | — |
| `ChunkSize` | `chunk_size` | json | `int64` | 是 | — | — |
| `TotalChunks` | `total_chunks` | json | `int32` | 是 | — | — |
| `Instant` | `instant` | json | `bool` | 是 | — | — |

### `UploadUrlData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `ChunkNo` | `chunk_no` | json | `int32` | 是 | — | — |
| `Url` | `url` | json | `string` | 是 | — | — |
| `Method` | `method` | json | `string` | 是 | — | — |
| `Expiration` | `expiration` | json | `int64` | 是 | — | — |
| `Headers` | `headers` | json | `map[string]string` | 是 | — | — |

### `UploadChunkPart`

> 分片清单条目（对应 upload.ChunkPart）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ChunkNo` | `chunk_no` | json | `int32` | 是 | — | — |
| `Etag` | `etag` | json | `string` | 是 | — | — |

### `UploadCompleteData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `AssetId` | `asset_id` | json | `string` | 是 | — | — |
| `ObjectKey` | `object_key` | json | `string` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |
| `Md5` | `md5` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `UploadStatusData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `UploadId` | `upload_id` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |
| `UploadedSize` | `uploaded_size` | json | `int64` | 是 | — | — |
| `TotalChunks` | `total_chunks` | json | `int32` | 是 | — | — |
| `CompletedChunks` | `completed_chunks` | json | `int32` | 是 | — | — |
| `Chunks` | `chunks` | json | `[]UploadChunk` | 是 | — | — |
| `AssetId` | `asset_id` | json | `string` | 是 | — | — |

### `UploadChunk`

> 分片信息（对应 upload.Chunk）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ChunkNo` | `chunk_no` | json | `int32` | 是 | — | — |
| `Size` | `size` | json | `int64` | 是 | — | — |
| `Etag` | `etag` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/11-upload.md -->
