# RPC · `asset`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/asset/rpc/asset.proto` |
| protobuf 包 | `asset.v1` |
| go_package | `go-video/services/asset/rpc` |
| 发现用的 etcd key | `asset.v1.rpc`（`services/asset/etc/asset.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`asset.v1.rpc`） |
| 监听 | `8099`（`services/asset/etc/asset.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_asset` |
| 方法数 | 10（service `Asset`） |
| 网关消费方 | `admin:AssetRPC` |

## 契约说明

>

## service `Asset`

> Asset 媒资元数据与生命周期服务。 / 依据 AGENTS.md §5，asset 拥有原文件、封面、字幕、截图元数据； / 不持有稿件发布状态，不把大文件放入 MySQL，不返回 OSS 长期公开 URL。

gRPC 方法前缀：`asset.v1.Asset/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RegisterAsset` | [`RegisterAssetReq`](#message-registerassetreq) | [`AssetReply`](#message-assetreply) | 上传完成后登记媒资；返回 asset_id |
| 2 | `GetAsset` | [`AssetReq`](#message-assetreq) | [`AssetReply`](#message-assetreply) | 查询单个媒资元数据 |
| 3 | `ListAssets` | [`ListReq`](#message-listreq) | [`AssetsReply`](#message-assetsreply) | 分页查询媒资列表（可按 mid 或 state 过滤） |
| 4 | `UpdateAssetMeta` | [`UpdateAssetReq`](#message-updateassetreq) | [`AssetReply`](#message-assetreply) | 更新媒资元数据（transcode 完成回调写入 duration/width/height/codec） |
| 5 | `TransitionState` | [`TransitionReq`](#message-transitionreq) | [`AssetReply`](#message-assetreply) | 推进媒资状态机（UPLOADED→SCANNED→TRANSCODED） |
| 6 | `AddCover` | [`AddCoverReq`](#message-addcoverreq) | [`CoverReply`](#message-coverreply) | 添加封面 |
| 7 | `ListCovers` | [`AssetReq`](#message-assetreq) | [`CoversReply`](#message-coversreply) | 查询某媒资的封面列表 |
| 8 | `AddSubtitle` | [`AddSubtitleReq`](#message-addsubtitlereq) | [`SubtitleReply`](#message-subtitlereply) | 添加字幕 |
| 9 | `ListSubtitles` | [`AssetReq`](#message-assetreq) | [`SubtitlesReply`](#message-subtitlesreply) | 查询某媒资的字幕列表 |
| 10 | `AddScreenshot` | [`AddScreenshotReq`](#message-addscreenshotreq) | [`ScreenshotReply`](#message-screenshotreply) | 添加截图 |

## 消息与枚举

### enum `AssetState`

> 媒资状态机。依据 AGENTS.md §8： /   UPLOADED → SCANNED → TRANSCODED / asset 不负责稿件发布状态（PUBLISHED 由 video 服务推进）。

| 值 | 编号 | 说明 |
|---|---|---|
| `STATE_UNSPECIFIED` | 0 | 未指定 |
| `STATE_UPLOADED` | 1 | 上传完成，待扫描 |
| `STATE_SCANNED` | 2 | 文件扫描与探测完成 |
| `STATE_TRANSCODED` | 3 | 转码完成，可投入发布 |
| `STATE_FAILED` | 4 | 处理失败 |

### message `AssetReply`

> 媒资元数据

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID |
| `upload_id` | `int64` | 2 | — | 关联 upload ID |
| `mid` | `int64` | 3 | — | 上传用户 ID |
| `bucket` | `string` | 4 | — | 对象存储桶 |
| `object_key` | `string` | 5 | — | 对象键 |
| `size` | `int64` | 6 | — | 文件大小（字节） |
| `md5` | `string` | 7 | — | 文件 MD5 |
| `duration` | `int64` | 8 | — | 时长（毫秒，transcode 回填写入） |
| `width` | `int32` | 9 | — | 视频宽（transcode 回填写入） |
| `height` | `int32` | 10 | — | 视频高（transcode 回填写入） |
| `codec` | `string` | 11 | — | 编码（transcode 回填写入） |
| `state` | [`AssetState`](#enum-assetstate) | 12 | — | 当前状态 |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 14 | — | 修改时间（Unix 秒） |

### message `EmptyReply`

> 空响应

（空消息）

### message `RegisterAssetReq`

> --- 媒资登记与查询 --- / RegisterAsset：upload 完成后由 upload 服务调用， / 创建 asset_meta 记录，关联 upload_id 与 OSS 路径；返回 asset_id。 / 本期不立即触发转码，转码由 video 服务直接调用 transcode 触发（占位）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `upload_id` | `int64` | 1 | — | upload 服务返回的上传 ID |
| `mid` | `int64` | 2 | — | 上传用户 ID |
| `bucket` | `string` | 3 | — | 对象存储桶 |
| `object_key` | `string` | 4 | — | 对象键 |
| `size` | `int64` | 5 | — | 文件大小（字节） |
| `md5` | `string` | 6 | — | 文件 MD5 |
| `ip` | `string` | 7 | — | 调用方 IP |

### message `AssetReq`

> 单个媒资查询

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID |
| `ip` | `string` | 2 | — | 调用方 IP |

### message `ListReq`

> 分页列表查询：可按 mid 或 state 过滤

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 按上传用户过滤（0 表示不过滤） |
| `state` | [`AssetState`](#enum-assetstate) | 2 | — | 按状态过滤（0 表示不过滤） |
| `pn` | `int32` | 3 | — | 页码（从 1 开始） |
| `ps` | `int32` | 4 | — | 每页大小（最大 50） |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `AssetsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `total` | `int32` | 1 | — | 总数 |
| `items` | [`AssetReply`](#message-assetreply) | 2 | repeated | 媒资列表 |

### message `UpdateAssetReq`

> 元数据更新：由 transcode 完成回调更新 duration/width/height/codec

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID |
| `duration` | `int64` | 2 | — | 时长（毫秒） |
| `width` | `int32` | 3 | — | 视频宽 |
| `height` | `int32` | 4 | — | 视频高 |
| `codec` | `string` | 5 | — | 编码 |
| `ip` | `string` | 6 | — | 调用方 IP |

### message `TransitionReq`

> 状态推进

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 媒资 ID |
| `to_state` | [`AssetState`](#enum-assetstate) | 2 | — | 目标状态 |
| `ip` | `string` | 3 | — | 调用方 IP |

### message `CoverReply`

> --- 封面 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cover_id` | `int64` | 1 | — | 封面 ID |
| `asset_id` | `int64` | 2 | — | 关联媒资 ID |
| `bucket` | `string` | 3 | — | 对象存储桶 |
| `object_key` | `string` | 4 | — | 对象键 |
| `width` | `int32` | 5 | — | 宽 |
| `height` | `int32` | 6 | — | 高 |
| `ctime` | `int64` | 7 | — | 创建时间（Unix 秒） |

### message `AddCoverReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 关联媒资 ID |
| `bucket` | `string` | 2 | — | 对象存储桶 |
| `object_key` | `string` | 3 | — | 对象键 |
| `width` | `int32` | 4 | — | 宽 |
| `height` | `int32` | 5 | — | 高 |
| `ip` | `string` | 6 | — | 调用方 IP |

### message `CoversReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`CoverReply`](#message-coverreply) | 1 | repeated | 封面列表 |

### message `SubtitleReply`

> --- 字幕 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `sub_id` | `int64` | 1 | — | 字幕 ID |
| `asset_id` | `int64` | 2 | — | 关联媒资 ID |
| `lang` | `string` | 3 | — | 语言代码（如 zh-CN、en-US） |
| `bucket` | `string` | 4 | — | 对象存储桶 |
| `object_key` | `string` | 5 | — | 对象键 |
| `ctime` | `int64` | 6 | — | 创建时间（Unix 秒） |

### message `AddSubtitleReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 关联媒资 ID |
| `lang` | `string` | 2 | — | 语言代码 |
| `bucket` | `string` | 3 | — | 对象存储桶 |
| `object_key` | `string` | 4 | — | 对象键 |
| `ip` | `string` | 5 | — | 调用方 IP |

### message `SubtitlesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`SubtitleReply`](#message-subtitlereply) | 1 | repeated | 字幕列表 |

### message `ScreenshotReply`

> --- 截图 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `shot_id` | `int64` | 1 | — | 截图 ID |
| `asset_id` | `int64` | 2 | — | 关联媒资 ID |
| `bucket` | `string` | 3 | — | 对象存储桶 |
| `object_key` | `string` | 4 | — | 对象键 |
| `timestamp` | `int64` | 5 | — | 截图时间点（毫秒） |
| `ctime` | `int64` | 6 | — | 创建时间（Unix 秒） |

### message `AddScreenshotReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `asset_id` | `int64` | 1 | — | 关联媒资 ID |
| `bucket` | `string` | 2 | — | 对象存储桶 |
| `object_key` | `string` | 3 | — | 对象键 |
| `timestamp` | `int64` | 4 | — | 截图时间点（毫秒） |
| `ip` | `string` | 5 | — | 调用方 IP |
