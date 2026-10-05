# RPC · `danmaku`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/danmaku/rpc/danmaku.proto` |
| protobuf 包 | `danmaku.v1` |
| go_package | `go-video/services/danmaku/rpc` |
| 发现用的 etcd key | `danmaku.v1.rpc`（`services/danmaku/etc/danmaku.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`danmaku.v1.rpc`） |
| 监听 | `8103`（`services/danmaku/etc/danmaku.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_danmaku` |
| 方法数 | 9（service `Danmaku`） |
| 网关消费方 | `app:DanmakuRPC`、`admin:DanmakuRPC` |

## 契约说明

> 说明：本服务是时间轴弹幕领域（对应参考仓库 dm/dm2 的领域层与实现层）。
> 依据 AGENTS.md §5，弹幕与评论分库分表：danmaku 不复用 comment 的实时事务，
> 也不引用 comment 的 model；两者只共享 oid 语义。
> 依据 §8，机审结论落库前弹幕不得对所有人可见：PostDanmaku 落库状态固定为
> 待审核/屏蔽池，只有 ApplyModerationResult 通过合法状态迁移后才能进入普通池。
> 消息名统一使用 DanmakuInfo/BlockWordInfo 等，避免与 service Danmaku 同名冲突。

## service `Danmaku`

> Danmaku 时间轴弹幕服务。 / 数据所有权见 AGENTS.md §5：时间轴消息、屏蔽词、用户屏蔽、举报和分片索引 / 全部归本服务；审核结论只能由 moderation-orchestrator 通过 / ApplyModerationResult 推进合法状态。

gRPC 方法前缀：`danmaku.v1.Danmaku/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `PostDanmaku` | [`PostDanmakuReq`](#message-postdanmakureq) | [`PostDanmakuReply`](#message-postdanmakureply) | 发送弹幕：校验 → 限流 → 屏蔽词过滤 → 落待审状态 → 提交机审 → 计数 |
| 2 | `ListDanmaku` | [`ListDanmakuReq`](#message-listdanmakureq) | [`ListDanmakuReply`](#message-listdanmakureply) | 按 oid + 时间分段批量拉取弹幕（段缓存命中优先，miss 回源 MySQL 并回填） |
| 3 | `DeleteDanmaku` | [`DeleteDanmakuReq`](#message-deletedanmakureq) | [`EmptyReply`](#message-emptyreply) | 删除弹幕（本人或管理员，软删除并保留 op_log 审计） |
| 4 | `ReportDanmaku` | [`ReportDanmakuReq`](#message-reportdanmakureq) | [`ReportDanmakuReply`](#message-reportdanmakureply) | 举报弹幕（写本地举报表，供 moderation 拉取，不直连其库） |
| 5 | `BlockWord` | [`BlockWordReq`](#message-blockwordreq) | [`BlockWordReply`](#message-blockwordreply) | 运营侧屏蔽词增删改 |
| 6 | `ListBlockWords` | [`ListBlockWordsReq`](#message-listblockwordsreq) | [`ListBlockWordsReply`](#message-listblockwordsreply) | 运营侧屏蔽词分页查询 |
| 7 | `UserBlock` | [`UserBlockReq`](#message-userblockreq) | [`EmptyReply`](#message-emptyreply) | 用户级屏蔽（屏蔽某用户或某关键词的弹幕） |
| 8 | `ListUserBlocks` | [`ListUserBlocksReq`](#message-listuserblocksreq) | [`ListUserBlocksReply`](#message-listuserblocksreply) | 用户级屏蔽列表 |
| 9 | `ApplyModerationResult` | [`ApplyModerationResultReq`](#message-applymoderationresultreq) | [`ApplyModerationResultReply`](#message-applymoderationresultreply) | 回写审核结论，按合法状态机推进（moderation.result.v1 消费者入口） |

## 消息与枚举

### message `EmptyReply`

> 空响应

（空消息）

### enum `DanmakuMode`

> 弹幕展示模式（与 danmaku.mode 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `MODE_UNSPECIFIED` | 0 | 未指定（服务端按滚动处理） |
| `MODE_SCROLL` | 1 | 滚动弹幕 |
| `MODE_BOTTOM` | 2 | 底部弹幕 |
| `MODE_TOP` | 3 | 顶部弹幕 |
| `MODE_COLOR` | 4 | 彩色弹幕（仅表示渲染样式，不做会员/付费判定） |
| `MODE_ADVANCED` | 5 | 高级弹幕（反向/代码弹幕等，需人审放行） |

### enum `DanmakuState`

> 弹幕状态机（与 danmaku.state 列一致，取值不可变更）

| 值 | 编号 | 说明 |
|---|---|---|
| `STATE_NORMAL` | 0 | 正常：审核通过后对所有人可见 |
| `STATE_PENDING` | 1 | 待审核：仅发送者本人可见 |
| `STATE_FOLDED` | 2 | 折叠：命中屏蔽词或被降权，仅发送者本人可见 |
| `STATE_DELETED` | 3 | 已删除：软删除，保留审计证据 |
| `STATE_REJECTED` | 4 | 审核驳回：机审/人审拒绝 |

### enum `DanmakuPool`

> 弹幕池（与 danmaku.pool 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `POOL_UNSPECIFIED` | 0 | 未指定 |
| `POOL_NORMAL` | 1 | 普通池：参与时间轴下发 |
| `POOL_REVIEW` | 2 | 审核池：等待机审/人审结论 |
| `POOL_BLOCK` | 4 | 屏蔽池：命中屏蔽词，只对发送者自己回显 |

### enum `ModerationVerdict`

> 审核结论（与 moderation.result.v1 payload 对齐，取值与 moderation.v1.Verdict 一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `VERDICT_UNSPECIFIED` | 0 | 未指定，服务端拒绝 |
| `VERDICT_PASS` | 1 | 通过：进入普通池 |
| `VERDICT_REVIEW` | 2 | 转人审：保持待审核 |
| `VERDICT_REJECT` | 3 | 拒绝：置为驳回 |

### enum `BlockWordScope`

> 屏蔽词作用域（与 danmaku_blockword.scope 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `SCOPE_UNSPECIFIED` | 0 | 未指定 |
| `SCOPE_GLOBAL` | 1 | 全局屏蔽词 |
| `SCOPE_OID` | 2 | 分区/单稿件屏蔽词（需带 oid） |

### enum `BlockWordAction`

> 屏蔽词管理动作（运营侧，需 operator_mid 权限字段）

| 值 | 编号 | 说明 |
|---|---|---|
| `BLOCK_WORD_UNSPECIFIED` | 0 | 未指定，服务端拒绝 |
| `BLOCK_WORD_ADD` | 1 | 新增或重新启用 |
| `BLOCK_WORD_DISABLE` | 2 | 停用（保留行，便于审计） |
| `BLOCK_WORD_DELETE` | 3 | 物理删除词条（保留 op_log 由运营后台负责） |

### enum `UserBlockType`

> 用户级屏蔽类型（与 danmaku_user_block.type 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `USER_BLOCK_UNSPECIFIED` | 0 | 未指定 |
| `USER_BLOCK_MID` | 1 | 屏蔽某用户发送的全部弹幕 |
| `USER_BLOCK_KEYWORD` | 2 | 屏蔽包含某关键词的弹幕 |

### message `DanmakuInfo`

> 弹幕主体（DB 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 弹幕 ID |
| `oid` | `int64` | 2 | — | 内容主键（视频 aid / 直播 room_id） |
| `aid` | `int64` | 3 | — | 稿件 ID（归档与联表投影用） |
| `mid` | `int64` | 4 | — | 发送者用户 ID |
| `progress_ms` | `int64` | 5 | — | 时间轴位置（毫秒） |
| `mode` | `int32` | 6 | — | 展示模式，参见 DanmakuMode |
| `fontsize` | `int32` | 7 | — | 字号 |
| `color` | `int32` | 8 | — | RGB 颜色整数值 |
| `content` | `string` | 9 | — | 弹幕正文 |
| `state` | `int32` | 10 | — | 状态，参见 DanmakuState |
| `pool` | `int32` | 11 | — | 弹幕池，参见 DanmakuPool |
| `seg_no` | `int32` | 12 | — | 时间分段号 = progress_ms / segment_ms |
| `ctime` | `int64` | 13 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 14 | — | 修改时间（Unix 秒） |

### message `PostDanmakuReq`

> --- 发送 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 内容主键 |
| `aid` | `int64` | 2 | — | 稿件 ID（可缺省为 0，服务端按 oid 处理） |
| `mid` | `int64` | 3 | — | 发送者用户 ID（由 gateway 注入，本服务不解析 token） |
| `progress_ms` | `int64` | 4 | — | 时间轴位置（毫秒） |
| `mode` | [`DanmakuMode`](#enum-danmakumode) | 5 | — | 展示模式 |
| `fontsize` | `int32` | 6 | — | 字号，缺省按服务端默认 |
| `color` | `int32` | 7 | — | 颜色，缺省 0xFFFFFF |
| `content` | `string` | 8 | — | 弹幕正文 |
| `idempotency_key` | `string` | 9 | — | 幂等键，客户端重试必须复用同一个值 |
| `client_msg_id` | `string` | 10 | — | 客户端消息 ID（idempotency_key 为空时退化为幂等键） |
| `trace_id` | `string` | 11 | — | 链路追踪 ID |

### message `PostDanmakuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 弹幕 ID（幂等重放时返回原值） |
| `state` | `int32` | 2 | — | 落库状态 |
| `pool` | `int32` | 3 | — | 落库弹幕池 |
| `seg_no` | `int32` | 4 | — | 时间分段号 |
| `ctime` | `int64` | 5 | — | 创建时间 |
| `replayed` | `bool` | 6 | — | true 表示命中幂等键、未产生新写入 |
| `moderation_task_id` | `int64` | 7 | — | 机审任务 ID（0 表示未提交） |

### message `ListDanmakuReq`

> --- 时间轴拉取 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `oid` | `int64` | 1 | — | 内容主键 |
| `viewer_mid` | `int64` | 2 | — | 查看者用户 ID（用于用户级屏蔽过滤，0 表示游客） |
| `start_seg` | `int32` | 3 | — | 起始分段号（与 start_progress_ms 二选一） |
| `end_seg` | `int32` | 4 | — | 结束分段号（含） |
| `start_progress_ms` | `int64` | 5 | — | 起始时间轴毫秒；> 0 时优先于 start_seg |
| `end_progress_ms` | `int64` | 6 | — | 结束时间轴毫秒；> 0 时优先于 end_seg |
| `limit` | `int32` | 7 | — | 本次最多返回条数，服务端会截断到上限 |
| `with_self_pending` | `bool` | 8 | — | 是否附带本人待审/折叠弹幕（发送后回显用） |

### message `SegmentCount`

> 分段计数（供客户端决定预取窗口）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `seg_no` | `int32` | 1 | — | 分段号 |
| `count` | `int32` | 2 | — | 段内可见弹幕数 |

### message `ListDanmakuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `danmaku` | [`DanmakuInfo`](#message-danmakuinfo) | 1 | repeated | 弹幕列表，按 progress_ms 升序 |
| `segment_counts` | [`SegmentCount`](#message-segmentcount) | 2 | repeated | 各分段计数 |
| `segment_seconds` | `int32` | 3 | — | 服务端分段秒数，客户端据此计算分段号 |
| `next_seg` | `int32` | 4 | — | 下一窗口起始分段号（时间轴翻页用） |
| `cache_hits` | `int32` | 5 | — | 命中段缓存的分段数（观测指标） |

### message `DeleteDanmakuReq`

> --- 删除 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 弹幕 ID |
| `mid` | `int64` | 2 | — | 操作者用户 ID |
| `admin` | `bool` | 3 | — | 是否管理员（管理员可删任意弹幕） |
| `reason` | `string` | 4 | — | 删除原因（写入 op_log 审计） |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `ReportDanmakuReq`

> --- 举报 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 被举报弹幕 ID |
| `reporter_mid` | `int64` | 2 | — | 举报者用户 ID |
| `reason` | `int32` | 3 | — | 举报原因码 |
| `content` | `string` | 4 | — | 举报补充说明 |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `ReportDanmakuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `report_id` | `int64` | 1 | — | 举报记录 ID（重复举报返回既有记录） |
| `duplicated` | `bool` | 2 | — | true 表示此前已举报过 |

### message `BlockWordInfo`

> --- 屏蔽词管理（运营侧） --- / 屏蔽词条目

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `word_id` | `int64` | 1 | — | 词 ID |
| `word` | `string` | 2 | — | 屏蔽词 |
| `scope` | `int32` | 3 | — | 作用域，参见 BlockWordScope |
| `oid` | `int64` | 4 | — | scope=SCOPE_OID 时的内容主键 |
| `state` | `int32` | 5 | — | 1 生效、0 停用 |
| `operator` | `int64` | 6 | — | 最近操作运营 ID |
| `ctime` | `int64` | 7 | — | 创建时间 |
| `mtime` | `int64` | 8 | — | 修改时间 |

### message `BlockWordReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `action` | [`BlockWordAction`](#enum-blockwordaction) | 1 | — | 动作 |
| `word` | `string` | 2 | — | 屏蔽词（去空格后 1~64 字符） |
| `scope` | [`BlockWordScope`](#enum-blockwordscope) | 3 | — | 作用域 |
| `oid` | `int64` | 4 | — | scope=SCOPE_OID 时必填 |
| `operator_mid` | `int64` | 5 | — | 操作者（管理员/运营），必须 > 0 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `BlockWordReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `word_id` | `int64` | 1 | — | 词 ID |
| `state` | `int32` | 2 | — | 处理后的状态 |

### message `ListBlockWordsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `scope` | [`BlockWordScope`](#enum-blockwordscope) | 1 | — | 作用域过滤（SCOPE_UNSPECIFIED 表示不过滤） |
| `oid` | `int64` | 2 | — | 分区过滤 |
| `only_enabled` | `bool` | 3 | — | 仅返回生效词 |
| `pn` | `int32` | 4 | — | 页码（从 1 开始） |
| `ps` | `int32` | 5 | — | 每页大小（最大 100） |
| `operator_mid` | `int64` | 6 | — | 操作者（管理员/运营），必须 > 0 |

### message `ListBlockWordsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `words` | [`BlockWordInfo`](#message-blockwordinfo) | 1 | repeated | 词列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `UserBlockInfo`

> --- 用户级屏蔽 --- / 用户屏蔽条目

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | 记录 ID |
| `mid` | `int64` | 2 | — | 所属用户 |
| `type` | `int32` | 3 | — | 类型，参见 UserBlockType |
| `blocked_mid` | `int64` | 4 | — | 被屏蔽用户 |
| `keyword` | `string` | 5 | — | 被屏蔽关键词 |
| `state` | `int32` | 6 | — | 1 生效、0 已解除 |
| `ctime` | `int64` | 7 | — | 创建时间 |
| `mtime` | `int64` | 8 | — | 修改时间 |

### message `UserBlockReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 操作用户（本人屏蔽列表） |
| `type` | [`UserBlockType`](#enum-userblocktype) | 2 | — | 屏蔽类型 |
| `blocked_mid` | `int64` | 3 | — | type=USER_BLOCK_MID 时必填 |
| `keyword` | `string` | 4 | — | type=USER_BLOCK_KEYWORD 时必填 |
| `unblock` | `bool` | 5 | — | true 表示解除屏蔽 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `ListUserBlocksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 所属用户 |
| `type` | [`UserBlockType`](#enum-userblocktype) | 2 | — | 类型过滤（USER_BLOCK_UNSPECIFIED 表示不过滤） |
| `pn` | `int32` | 3 | — | 页码 |
| `ps` | `int32` | 4 | — | 每页大小（最大 100） |

### message `ListUserBlocksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `blocks` | [`UserBlockInfo`](#message-userblockinfo) | 1 | repeated | 屏蔽列表 |
| `total` | `int32` | 2 | — | 总数 |

### message `ApplyModerationResultReq`

> --- 审核结论回写（moderation.result.v1 消费者入口） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 弹幕 ID |
| `task_id` | `int64` | 2 | — | moderation 任务 ID |
| `verdict` | [`ModerationVerdict`](#enum-moderationverdict) | 3 | — | 审核结论 |
| `reason` | `string` | 4 | — | 结论原因（关键词命中、模型分数等） |
| `operator` | `int64` | 5 | — | 处理人（0 表示系统/机审） |
| `event_id` | `string` | 6 | — | moderation.result.v1 的 event_id，用于消费去重 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `ApplyModerationResultReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `dmid` | `int64` | 1 | — | 弹幕 ID |
| `state` | `int32` | 2 | — | 迁移后的状态 |
| `pool` | `int32` | 3 | — | 迁移后的弹幕池 |
| `applied` | `bool` | 4 | — | false 表示重复投递或无需迁移 |
| `message` | `string` | 5 | — | 说明（重复投递/终态等） |
