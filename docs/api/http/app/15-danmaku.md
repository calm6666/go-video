# 终端面 · `/danmaku`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| danmaku 域聚合（services/danmaku/rpc/danmaku.proto） | 免鉴权 | 6 |

合计 **6** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## danmaku 域聚合（services/danmaku/rpc/danmaku.proto）（免鉴权，6 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/danmaku/post` | 发送弹幕（幂等，落库后待审核） | `postDanmaku` | `postdanmakulogic.go` |
| GET | `/danmaku/list` | 按时间轴分段拉取弹幕 | `listDanmaku` | `listdanmakulogic.go` |
| POST | `/danmaku/delete` | 删除本人弹幕（管理员可删任意，软删保留审计） | `deleteDanmaku` | `deletedanmakulogic.go` |
| POST | `/danmaku/report` | 举报弹幕 | `reportDanmaku` | `reportdanmakulogic.go` |
| POST | `/danmaku/user_block` | 屏蔽/解除屏蔽某用户或某关键词的弹幕 | `danmakuUserBlock` | `danmakuuserblocklogic.go` |
| GET | `/danmaku/user_blocks` | 本人弹幕屏蔽列表 | `listDanmakuUserBlocks` | `listdanmakuuserblockslogic.go` |

### POST `/danmaku/post` — 发送弹幕（幂等，落库后待审核）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/postdanmakuhandler.go`
- 业务实现：`gateway/app/internal/logic/postdanmakulogic.go`

请求：`ParamDanmakuPost`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ProgressMs` | `progress_ms` | form | `int64` | 是 | — | — |
| `Mode` | `mode` | form | `int32` | 否 | — | 缺省按滚动 |
| `Fontsize` | `fontsize` | form | `int32` | 否 | — | — |
| `Color` | `color` | form | `int32` | 否 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | form | `string` | 否 | — | — |
| `ClientMsgId` | `client_msg_id` | form | `string` | 否 | — | — |

响应：`DanmakuPostResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuPostData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/danmaku/list` — 按时间轴分段拉取弹幕

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listdanmakuhandler.go`
- 业务实现：`gateway/app/internal/logic/listdanmakulogic.go`

请求：`ParamDanmakuList`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `StartSeg` | `start_seg` | form | `int32` | 否 | — | — |
| `EndSeg` | `end_seg` | form | `int32` | 否 | — | — |
| `StartProgressMs` | `start_progress_ms` | form | `int64` | 否 | — | — |
| `EndProgressMs` | `end_progress_ms` | form | `int64` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `WithSelfPending` | `with_self_pending` | form | `bool` | 否 | — | — |

响应：`DanmakuListResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/danmaku/delete` — 删除本人弹幕（管理员可删任意，软删保留审计）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deletedanmakuhandler.go`
- 业务实现：`gateway/app/internal/logic/deletedanmakulogic.go`

请求：`ParamDanmakuDelete`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/danmaku/report` — 举报弹幕

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/reportdanmakuhandler.go`
- 业务实现：`gateway/app/internal/logic/reportdanmakulogic.go`

请求：`ParamDanmakuReport`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 否 | — | — |
| `Content` | `content` | form | `string` | 否 | — | — |

响应：`DanmakuReportResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/danmaku/user_block` — 屏蔽/解除屏蔽某用户或某关键词的弹幕

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/danmakuuserblockhandler.go`
- 业务实现：`gateway/app/internal/logic/danmakuuserblocklogic.go`

请求：`ParamDanmakuUserBlock`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 是 | — | 1 屏蔽用户、2 屏蔽关键词 |
| `BlockedMid` | `blocked_mid` | form | `int64` | 否 | — | — |
| `Keyword` | `keyword` | form | `string` | 否 | — | — |
| `Unblock` | `unblock` | form | `bool` | 否 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/danmaku/user_blocks` — 本人弹幕屏蔽列表

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listdanmakuuserblockshandler.go`
- 业务实现：`gateway/app/internal/logic/listdanmakuuserblockslogic.go`

请求：`ParamDanmakuUserBlocks`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=50 | — |

响应：`DanmakuUserBlocksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuUserBlocksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamDanmakuPost`

> 发送的弹幕一律先落待审/屏蔽池（AGENTS.md §8），审核结论由 moderation 经 / ApplyModerationResult 推进；网关不做内容判定，只转发与注入 mid。 / 屏蔽词管理（BlockWord/ListBlockWords）属运营能力，只在 gateway/admin 暴露。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `Aid` | `aid` | form | `int64` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `ProgressMs` | `progress_ms` | form | `int64` | 是 | — | — |
| `Mode` | `mode` | form | `int32` | 否 | — | 缺省按滚动 |
| `Fontsize` | `fontsize` | form | `int32` | 否 | — | — |
| `Color` | `color` | form | `int32` | 否 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `IdempotencyKey` | `idempotency_key` | form | `string` | 否 | — | — |
| `ClientMsgId` | `client_msg_id` | form | `string` | 否 | — | — |

### `DanmakuPostResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuPostData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDanmakuList`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Oid` | `oid` | form | `int64` | 是 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `StartSeg` | `start_seg` | form | `int32` | 否 | — | — |
| `EndSeg` | `end_seg` | form | `int32` | 否 | — | — |
| `StartProgressMs` | `start_progress_ms` | form | `int64` | 否 | — | — |
| `EndProgressMs` | `end_progress_ms` | form | `int64` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `WithSelfPending` | `with_self_pending` | form | `bool` | 否 | — | — |

### `DanmakuListResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuListData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDanmakuDelete`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 否 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDanmakuReport`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | form | `int64` | 是 | — | — |
| `ReporterMid` | `reporter_mid` | form | `int64` | 是 | — | — |
| `Reason` | `reason` | form | `int32` | 否 | — | — |
| `Content` | `content` | form | `string` | 否 | — | — |

### `DanmakuReportResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuReportData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDanmakuUserBlock`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 是 | — | 1 屏蔽用户、2 屏蔽关键词 |
| `BlockedMid` | `blocked_mid` | form | `int64` | 否 | — | — |
| `Keyword` | `keyword` | form | `string` | 否 | — | — |
| `Unblock` | `unblock` | form | `bool` | 否 | — | — |

### `ParamDanmakuUserBlocks`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Type` | `type` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=50 | — |

### `DanmakuUserBlocksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuUserBlocksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `DanmakuPostData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Pool` | `pool` | json | `int32` | 是 | — | — |
| `SegNo` | `seg_no` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Replayed` | `replayed` | json | `bool` | 是 | — | — |
| `ModerationTaskId` | `moderation_task_id` | json | `int64` | 是 | — | — |

### `DanmakuListData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Danmaku` | `danmaku` | json | `[]DanmakuInfo` | 是 | — | — |
| `SegmentCounts` | `segment_counts` | json | `[]DanmakuSegmentCount` | 是 | — | — |
| `SegmentSeconds` | `segment_seconds` | json | `int32` | 是 | — | — |
| `NextSeg` | `next_seg` | json | `int32` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `DanmakuReportData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `ReportId` | `report_id` | json | `int64` | 是 | — | — |
| `Duplicated` | `duplicated` | json | `bool` | 是 | — | — |

### `DanmakuUserBlocksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Blocks` | `blocks` | json | `[]DanmakuUserBlockInfo` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |

### `DanmakuInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | json | `int64` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `ProgressMs` | `progress_ms` | json | `int64` | 是 | — | — |
| `Mode` | `mode` | json | `int32` | 是 | — | — |
| `Fontsize` | `fontsize` | json | `int32` | 是 | — | — |
| `Color` | `color` | json | `int32` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Pool` | `pool` | json | `int32` | 是 | — | — |
| `SegNo` | `seg_no` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `DanmakuSegmentCount`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `SegNo` | `seg_no` | json | `int32` | 是 | — | — |
| `Count` | `count` | json | `int32` | 是 | — | — |

### `DanmakuUserBlockInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Type` | `type` | json | `int32` | 是 | — | — |
| `BlockedMid` | `blocked_mid` | json | `int64` | 是 | — | — |
| `Keyword` | `keyword` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/15-danmaku.md -->
