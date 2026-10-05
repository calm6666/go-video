# RPC · `playback`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/playback/rpc/playback.proto` |
| protobuf 包 | `playback.v1` |
| go_package | `go-video/services/playback/rpc` |
| 发现用的 etcd key | `playback.v1.rpc`（`services/playback/etc/playback.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`playback.v1.rpc`） |
| 监听 | `8102`（`services/playback/etc/playback.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_playback` |
| 方法数 | 4（service `Playback`） |
| 网关消费方 | `app:PlaybackRPC` |

## 契约说明

> playback 服务：播放会话与播放授权（短期签名地址）的领域服务。
> 依据 AGENTS.md §5，playback 只拥有 playback_session/playback_progress/playback_outbox；
> 稿件、媒资、转码版本和版权窗口分别归 video/asset/transcode/rights，本服务不复制其主数据，
> 只记录"某用户对某个可播放版本发起过一次受控播放"这一事实。
> 依据 §6，签发地址必须是带过期时间的 CDN 防盗链地址，客户端不得拿到对象存储长期密钥。
> 媒资/转码结果由调用方（gateway/app）解析成 object_key 后传入，本服务只对它签名。

## service `Playback`

> Playback 播放授权与播放会话服务。 / 依据 AGENTS.md §5/§6：PGC 必须经 rights 校验版权窗口；签发地址短期有效； / 心跳只写本服务表 + Outbox，异步进入 event-collector/spm，不在请求线程更新播放量。

gRPC 方法前缀：`playback.v1.Playback/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `GetPlaybackToken` | [`GetPlaybackTokenReq`](#message-getplaybacktokenreq) | [`GetPlaybackTokenReply`](#message-getplaybacktokenreply) | 签发短期防盗链播放地址并创建播放会话（request_id 幂等） |
| 2 | `VerifyPlaybackToken` | [`VerifyPlaybackTokenReq`](#message-verifyplaybacktokenreq) | [`VerifyPlaybackTokenReply`](#message-verifyplaybacktokenreply) | 边缘/网关回源校验 auth_key 与会话有效期，并按会话首次放行累加播放计数 |
| 3 | `ReportHeartbeat` | [`ReportHeartbeatReq`](#message-reportheartbeatreq) | [`ReportHeartbeatReply`](#message-reportheartbeatreply) | 上报播放心跳：幂等更新进度，并在同一事务写 playback.heartbeat 事件（Outbox） |
| 4 | `GetSession` | [`GetSessionReq`](#message-getsessionreq) | [`GetSessionReply`](#message-getsessionreply) | 查询播放会话与进度详情（管理后台/排障） |

## 消息与枚举

### enum `ContentType`

> 内容类型。 / 注意：编号与 rights 契约的 ContentType 不一致（rights 是 1=PGC、2=UGC）， / 跨服务调用必须经过 internal/repository 的显式映射函数，禁止 int32 直转。

| 值 | 编号 | 说明 |
|---|---|---|
| `CONTENT_TYPE_UNSPECIFIED` | 0 | 未指定 |
| `CONTENT_TYPE_UGC` | 1 | UGC/PUGC 稿件（content_id=aid，vid=bvid） |
| `CONTENT_TYPE_PGC` | 2 | 版权内容的一集（content_id=episode_id） |

### enum `Platform`

> 客户端平台（AGENTS.md §1/§6：Android、iOS、HarmonyOS、桌面端；不支持小程序）。

| 值 | 编号 | 说明 |
|---|---|---|
| `PLATFORM_UNSPECIFIED` | 0 | 未指定 |
| `PLATFORM_ANDROID` | 1 | Android |
| `PLATFORM_IOS` | 2 | iOS |
| `PLATFORM_HARMONY` | 3 | HarmonyOS |
| `PLATFORM_DESKTOP` | 4 | 电脑客户端 |

### enum `SessionState`

> 播放会话状态（与 model 常量一致）。

| 值 | 编号 | 说明 |
|---|---|---|
| `SESSION_STATE_UNSPECIFIED` | 0 | 未指定 |
| `SESSION_STATE_ACTIVE` | 1 | 有效（可回源） |
| `SESSION_STATE_EXPIRED` | 2 | 已过期（回源校验时发现超过 expire_at） |
| `SESSION_STATE_REVOKED` | 3 | 已撤销（版权撤回、风控处置） |

### message `EmptyReply`

> 空响应

（空消息）

### message `GetPlaybackTokenReq`

> --- 签发播放地址 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `content_type` | [`ContentType`](#enum-contenttype) | 1 | — | 内容类型 |
| `content_id` | `int64` | 2 | — | 内容 ID：UGC=aid、PGC=episode_id |
| `vid` | `string` | 3 | — | UGC bvid（可选，仅排障与日志关联） |
| `object_key` | `string` | 4 | — | object_key 可播放版本的 CDN 相对路径（如 /ugc/12/34/56.m3u8），由调用方从 / asset/transcode 解析后传入；playback 不读媒资表，只对它签名（AGENTS.md §5）。 |
| `mid` | `int64` | 5 | — | 观看者用户 ID，0 表示游客 |
| `platform` | [`Platform`](#enum-platform) | 6 | — | 客户端平台 |
| `app_version` | `string` | 7 | — | 客户端版本号 |
| `region` | `string` | 8 | — | 地区代码（PGC 必传，用于版权窗口校验） |
| `request_id` | `string` | 9 | — | 幂等键：同一 request_id 重放返回同一会话 |
| `trace_id` | `string` | 10 | — | 调用方透传 trace_id |

### message `GetPlaybackTokenReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `string` | 1 | — | 播放会话 ID（ULID） |
| `play_url` | `string` | 2 | — | 带 auth_key 的短期签名播放地址 |
| `auth_key` | `string` | 3 | — | 独立返回签名串，供边缘节点排障比对 |
| `expire_at` | `int64` | 4 | — | 授权过期时间（Unix 秒，绝对时间） |
| `ttl` | `int64` | 5 | — | 建议客户端缓存秒数 |
| `key_id` | `string` | 6 | — | 签名 KeyID，便于 CDN 密钥轮换排障 |

### message `VerifyPlaybackTokenReq`

> --- 边缘/网关回源校验 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `string` | 1 | — | 播放会话 ID |
| `uri` | `string` | 2 | — | 被请求的资源路径（auth_key 与其绑定） |
| `auth_key` | `string` | 3 | — | 客户端携带的签名串：ts-rand-uid-md5hash |
| `client_ip` | `string` | 4 | — | 回源节点/客户端 IP（仅日志脱敏使用，不落库、不入事件） |

### message `VerifyPlaybackTokenReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allow` | `bool` | 1 | — | 是否放行 |
| `expire_at` | `int64` | 2 | — | 会话过期时间（Unix 秒） |
| `deny_reason` | `string` | 3 | — | 拒绝原因码：session_not_found/session_expired/ |
| `play_count` | `int64` | 4 | — | bad_auth_format/sign_mismatch/auth_key_revoked/uri_mismatch / 该内容累计播放计数（Redis，首次回源时 +1） |

### message `ReportHeartbeatReq`

> --- 播放心跳 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `string` | 1 | — | 播放会话 ID |
| `position_ms` | `int64` | 2 | — | 当前播放位置（毫秒） |
| `duration_ms` | `int64` | 3 | — | 内容总时长（毫秒） |
| `buffer_count` | `int32` | 4 | — | 累计卡顿次数 |
| `avg_bitrate` | `int64` | 5 | — | 累计平均码率（bps） |
| `last_error` | `int32` | 6 | — | 最近一次播放错误码（0 表示无错误） |
| `trace_id` | `string` | 7 | — | 调用方透传 trace_id |

### message `ReportHeartbeatReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | 写入 outbox 的事件 ID（ULID），下游按此幂等 |
| `accepted_at` | `int64` | 2 | — | 服务端受理时间（Unix 秒） |
| `max_position_ms` | `int64` | 3 | — | 幂等落库后的最大播放位置（乱序心跳不回退进度） |

### message `GetSessionReq`

> --- 会话详情（管理/排障） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `string` | 1 | — | 播放会话 ID |

### message `PlaybackProgressInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 自增主键 |
| `session_id` | `string` | 2 | — | 会话 ID（唯一） |
| `content_type` | `int32` | 3 | — | 内容类型，参见 ContentType |
| `content_id` | `int64` | 4 | — | 内容 ID |
| `vid` | `string` | 5 | — | UGC bvid |
| `mid` | `int64` | 6 | — | 观看者用户 ID |
| `position_ms` | `int64` | 7 | — | 服务端记录的最大播放位置（毫秒） |
| `duration_ms` | `int64` | 8 | — | 内容总时长（毫秒） |
| `buffer_count` | `int32` | 9 | — | 累计卡顿次数 |
| `avg_bitrate` | `int64` | 10 | — | 累计平均码率（bps） |
| `last_error` | `int32` | 11 | — | 最近一次播放错误码 |
| `ctime` | `int64` | 12 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 13 | — | 修改时间（Unix 秒） |

### message `PlaybackSessionInfo`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session_id` | `string` | 1 | — | 会话 ID |
| `content_type` | `int32` | 2 | — | 内容类型，参见 ContentType |
| `content_id` | `int64` | 3 | — | 内容 ID |
| `vid` | `string` | 4 | — | UGC bvid |
| `mid` | `int64` | 5 | — | 观看者用户 ID（0 游客） |
| `platform` | `int32` | 6 | — | 客户端平台，参见 Platform |
| `app_version` | `string` | 7 | — | 客户端版本号 |
| `region` | `string` | 8 | — | 地区代码 |
| `object_key` | `string` | 9 | — | 签名目标对象路径 |
| `uri` | `string` | 10 | — | 签名使用的 URI 路径（auth_key 与其绑定） |
| `request_id` | `string` | 11 | — | 幂等键 |
| `expire_at` | `int64` | 12 | — | 过期时间（Unix 秒） |
| `state` | `int32` | 13 | — | 会话状态，参见 SessionState |
| `trace_id` | `string` | 14 | — | 签发时的 trace_id |
| `ctime` | `int64` | 15 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 16 | — | 修改时间（Unix 秒） |

### message `GetSessionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `session` | [`PlaybackSessionInfo`](#message-playbacksessioninfo) | 1 | — | 会话主体（不存在时为 null） |
| `progress` | [`PlaybackProgressInfo`](#message-playbackprogressinfo) | 2 | — | 该会话的播放进度（无心跳时为 null） |
| `latest_progress` | [`PlaybackProgressInfo`](#message-playbackprogressinfo) | 3 | — | 同一观看者对该内容的最近一次进度（跨会话对比，用于排障"为什么没有从头播放"）。 |
