# 终端面 · `/search`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| search 域聚合（services/search-query/rpc/searchquery.proto） | 免鉴权 | 8 |

合计 **8** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## search 域聚合（services/search-query/rpc/searchquery.proto）（免鉴权，8 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/search/query` | 关键词搜索（cursor 优先分页；引擎不可用返回明确错误） | `search` | `searchlogic.go` |
| GET | `/search/suggest` | 输入前缀联想 | `searchSuggest` | `searchsuggestlogic.go` |
| GET | `/search/hot` | 全站/分区热词快照 | `hotKeywords` | `hotkeywordslogic.go` |
| GET | `/search/config` | 该端/分区的排序与分页能力配置 | `searchConfig` | `searchconfiglogic.go` |
| GET | `/search/history` | 本人搜索历史 | `listSearchHistory` | `listsearchhistorylogic.go` |
| POST | `/search/history/delete` | 删除单个搜索历史词（需 confirm=true，物理删除） | `deleteSearchHistory` | `deletesearchhistorylogic.go` |
| POST | `/search/history/clear` | 清空本人搜索历史（需 confirm=true，物理删除） | `clearSearchHistory` | `clearsearchhistorylogic.go` |
| POST | `/search/query/report` | 上报查询行为（SPM 分析链路，query_id 幂等） | `reportQuery` | `reportquerylogic.go` |

### GET `/search/query` — 关键词搜索（cursor 优先分页；引擎不可用返回明确错误）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/searchhandler.go`
- 业务实现：`gateway/app/internal/logic/searchlogic.go`

请求：`ParamSearch`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | 1 全部/2 视频/3 用户/4 PGC |
| `ZoneId` | `zone_id` | form | `int32` | 否 | — | — |
| `Duration` | `duration` | form | `int32` | 否 | — | — |
| `Sort` | `sort` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=30 | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |
| `PublishedAfter` | `published_after` | form | `int64` | 否 | — | — |
| `PublishedBefore` | `published_before` | form | `int64` | 否 | — | — |

响应：`SearchResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/search/suggest` — 输入前缀联想

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/searchsuggesthandler.go`
- 业务实现：`gateway/app/internal/logic/searchsuggestlogic.go`

请求：`ParamSuggest`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |

响应：`SuggestResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SuggestData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/search/hot` — 全站/分区热词快照

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/hotkeywordshandler.go`
- 业务实现：`gateway/app/internal/logic/hotkeywordslogic.go`

请求：`ParamHotKeywords`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `string` | 否 | — | global 或 zone:<id> |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |

响应：`HotKeywordsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `HotKeywordsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/search/config` — 该端/分区的排序与分页能力配置

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/searchconfighandler.go`
- 业务实现：`gateway/app/internal/logic/searchconfiglogic.go`

请求：`ParamSearchConfig`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | form | `int32` | 否 | — | — |

响应：`SearchConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/search/history` — 本人搜索历史

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listsearchhistoryhandler.go`
- 业务实现：`gateway/app/internal/logic/listsearchhistorylogic.go`

请求：`ParamSearchHistory`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |

响应：`SearchHistoryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchHistoryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/search/history/delete` — 删除单个搜索历史词（需 confirm=true，物理删除）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deletesearchhistoryhandler.go`
- 业务实现：`gateway/app/internal/logic/deletesearchhistorylogic.go`

请求：`ParamDeleteSearchHistory`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `Confirm` | `confirm` | form | `bool` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

响应：`SearchHistoryDeleteResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchHistoryDeleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/search/history/clear` — 清空本人搜索历史（需 confirm=true，物理删除）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/clearsearchhistoryhandler.go`
- 业务实现：`gateway/app/internal/logic/clearsearchhistorylogic.go`

请求：`ParamClearSearchHistory`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Confirm` | `confirm` | form | `bool` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

响应：`SearchHistoryDeleteResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchHistoryDeleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/search/query/report` — 上报查询行为（SPM 分析链路，query_id 幂等）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/reportqueryhandler.go`
- 业务实现：`gateway/app/internal/logic/reportquerylogic.go`

请求：`ParamReportQuery`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `QueryId` | `query_id` | form | `string` | 是 | — | — |
| `Keyword` | `keyword` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `HitCount` | `hit_count` | form | `int64` | 否 | — | — |
| `ResultState` | `result_state` | form | `int32` | 否 | — | — |
| `LatencyMs` | `latency_ms` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `DeviceIdHash` | `device_id_hash` | form | `string` | 否 | — | — |

响应：`ReportQueryResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ReportQueryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamSearch`

> 引擎不可用/熔断时 search-query 返回明确错误码（ErrSearchUnavailable）， / 网关不得伪造空结果成功（docs/api-and-events.md §2）。ttl 由响应信封透传给客户端。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | 1 全部/2 视频/3 用户/4 PGC |
| `ZoneId` | `zone_id` | form | `int32` | 否 | — | — |
| `Duration` | `duration` | form | `int32` | 否 | — | — |
| `Sort` | `sort` | form | `int32` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=30 | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |
| `PublishedAfter` | `published_after` | form | `int64` | 否 | — | — |
| `PublishedBefore` | `published_before` | form | `int64` | 否 | — | — |

### `SearchResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSuggest`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |

### `SuggestResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SuggestData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamHotKeywords`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `string` | 否 | — | global 或 zone:<id> |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `ViewerMid` | `viewer_mid` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |

### `HotKeywordsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `HotKeywordsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSearchConfig`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |
| `ZoneId` | `zone_id` | form | `int32` | 否 | — | — |

### `SearchConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSearchHistory`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Cursor` | `cursor` | form | `string` | 否 | — | — |
| `Limit` | `limit` | form | `int32` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |

### `SearchHistoryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchHistoryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDeleteSearchHistory`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Keyword` | `keyword` | form | `string` | 是 | — | — |
| `Confirm` | `confirm` | form | `bool` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

### `SearchHistoryDeleteResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `SearchHistoryDeleteData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamClearSearchHistory`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Confirm` | `confirm` | form | `bool` | 是 | — | — |
| `RequestId` | `request_id` | form | `string` | 否 | — | — |

### `ParamReportQuery`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `QueryId` | `query_id` | form | `string` | 是 | — | — |
| `Keyword` | `keyword` | form | `string` | 否 | — | — |
| `SearchType` | `search_type` | form | `int32` | 否 | — | — |
| `Mid` | `mid` | form | `int64` | 否 | — | — |
| `HitCount` | `hit_count` | form | `int64` | 否 | — | — |
| `ResultState` | `result_state` | form | `int32` | 否 | — | — |
| `LatencyMs` | `latency_ms` | form | `int64` | 否 | — | — |
| `Platform` | `platform` | form | `string` | 否 | — | — |
| `AppVersion` | `app_version` | form | `string` | 否 | — | — |
| `DeviceIdHash` | `device_id_hash` | form | `string` | 否 | — | — |

### `ReportQueryResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ReportQueryData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `SearchData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Hits` | `hits` | json | `[]SearchHit` | 是 | — | — |
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Pn` | `pn` | json | `int32` | 是 | — | — |
| `Ps` | `ps` | json | `int32` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |
| `SafeFiltered` | `safe_filtered` | json | `bool` | 是 | — | — |
| `CacheHit` | `cache_hit` | json | `bool` | 是 | — | — |

### `SuggestData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]SuggestItem` | 是 | — | — |

### `HotKeywordsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keywords` | `keywords` | json | `[]HotKeyword` | 是 | — | — |
| `SnapshotAt` | `snapshot_at` | json | `int64` | 是 | — | — |
| `FromCache` | `from_cache` | json | `bool` | 是 | — | — |

### `SearchConfigData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DefaultSort` | `default_sort` | json | `int32` | 是 | — | — |
| `SupportedSorts` | `supported_sorts` | json | `[]int32` | 是 | — | — |
| `SupportedTypes` | `supported_types` | json | `[]int32` | 是 | — | — |
| `SupportedDurations` | `supported_durations` | json | `[]int32` | 是 | — | — |
| `PsDefault` | `ps_default` | json | `int32` | 是 | — | — |
| `PsLimit` | `ps_limit` | json | `int32` | 是 | — | — |
| `MaxOffset` | `max_offset` | json | `int32` | 是 | — | — |
| `KeywordMaxLen` | `keyword_max_len` | json | `int32` | 是 | — | — |
| `CacheTtlSeconds` | `cache_ttl_seconds` | json | `int32` | 是 | — | — |
| `EngineAvailable` | `engine_available` | json | `bool` | 是 | — | — |

### `SearchHistoryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]SearchHistoryItem` | 是 | — | — |
| `NextCursor` | `next_cursor` | json | `string` | 是 | — | — |
| `HasMore` | `has_more` | json | `bool` | 是 | — | — |

### `SearchHistoryDeleteData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Deleted` | `deleted` | json | `int64` | 是 | — | — |
| `HardDeleted` | `hard_deleted` | json | `bool` | 是 | — | — |

### `ReportQueryData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Accepted` | `accepted` | json | `bool` | 是 | — | — |
| `Deduplicated` | `deduplicated` | json | `bool` | 是 | — | — |
| `EventId` | `event_id` | json | `string` | 是 | — | — |

### `SearchHit`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `DocType` | `doc_type` | json | `string` | 是 | — | — |
| `DocId` | `doc_id` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `ContentSnippet` | `content_snippet` | json | `string` | 是 | — | — |
| `AuthorMid` | `author_mid` | json | `int64` | 是 | — | — |
| `AuthorName` | `author_name` | json | `string` | 是 | — | — |
| `ZoneId` | `zone_id` | json | `int32` | 是 | — | — |
| `CoverUrl` | `cover_url` | json | `string` | 是 | — | — |
| `ViewCount` | `view_count` | json | `int64` | 是 | — | — |
| `LikeCount` | `like_count` | json | `int64` | 是 | — | — |
| `DanmakuCount` | `danmaku_count` | json | `int64` | 是 | — | — |
| `FansCount` | `fans_count` | json | `int64` | 是 | — | — |
| `DurationSec` | `duration_sec` | json | `int32` | 是 | — | — |
| `PubTime` | `pub_time` | json | `int64` | 是 | — | — |
| `Score` | `score` | json | `float64` | 是 | — | — |
| `Highlights` | `highlights` | json | `map[string]string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |

### `SuggestItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | json | `string` | 是 | — | — |
| `Weight` | `weight` | json | `float64` | 是 | — | — |
| `Source` | `source` | json | `string` | 是 | — | — |
| `FromHistory` | `from_history` | json | `bool` | 是 | — | — |

### `HotKeyword`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | json | `string` | 是 | — | — |
| `Score` | `score` | json | `float64` | 是 | — | — |
| `SnapshotAt` | `snapshot_at` | json | `int64` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 是 | — | — |

### `SearchHistoryItem`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Keyword` | `keyword` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Platform` | `platform` | json | `string` | 是 | — | — |


<!-- file: docs/api/http/app/16-search.md -->
