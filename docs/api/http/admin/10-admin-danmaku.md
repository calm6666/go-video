# 运营面 · `/admin/danmaku`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| danmaku 域运营路由 | 免鉴权 | 1 |
| danmaku 域写入口（受 AdminPermission 保护） | AdminPermission | 1 |
| danmaku 域运营增量 | AdminPermission | 1 |

合计 **3** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## danmaku 域运营路由（免鉴权，1 条）

> 只开放屏蔽词管理：用户级屏蔽（UserBlock/ListUserBlocks）属于终端用户自助能力，
> 已在 gateway/app 暴露，运营后台不重复开放（AGENTS.md §3 网关职责边界）。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/danmaku/block_words` | 分页查询屏蔽词（scope/oid/only_enabled 过滤） | `listBlockWords` | `listblockwordslogic.go` |

### GET `/admin/danmaku/block_words` — 分页查询屏蔽词（scope/oid/only_enabled 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listblockwordshandler.go`
- 业务实现：`gateway/admin/internal/logic/listblockwordslogic.go`

请求：`ParamListBlockWords`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `int32` | 否 | — | — |
| `Oid` | `oid` | form | `int64` | 否 | — | — |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

响应：`DanmakuBlockWordsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuBlockWordsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## danmaku 域写入口（受 AdminPermission 保护）（AdminPermission，1 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/danmaku/block_word` | 屏蔽词新增/停用/删除（action 1/2/3，operator_mid 必填） | `danmaku:block-word` / `update` | `blockWord` | `blockwordlogic.go` |

### POST `/admin/danmaku/block_word` — 屏蔽词新增/停用/删除（action 1/2/3，operator_mid 必填）

- 权限口径：AdminPermission · 权限点 `danmaku:block-word` / `update`
- goctl 入口：`gateway/admin/internal/handler/blockwordhandler.go`
- 业务实现：`gateway/admin/internal/logic/blockwordlogic.go`

请求：`ParamBlockWord`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Action` | `action` | json | `int32` | 是 | — | — |
| `Word` | `word` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`DanmakuBlockWordResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuBlockWordResultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## danmaku 域运营增量（AdminPermission，1 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/danmaku/delete` | 运营删除任意弹幕（admin=true，reason 落 op_log 审计） | `danmaku:item` / `delete` | `adminDeleteDanmaku` | `admindeletedanmakulogic.go` |

### POST `/admin/danmaku/delete` — 运营删除任意弹幕（admin=true，reason 落 op_log 审计）

- 权限口径：AdminPermission · 权限点 `danmaku:item` / `delete`
- goctl 入口：`gateway/admin/internal/handler/admindeletedanmakuhandler.go`
- 业务实现：`gateway/admin/internal/logic/admindeletedanmakulogic.go`

请求：`ParamAdminDeleteDanmaku`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListBlockWords`

> scope 传 0 表示不按作用域过滤；ps 上限 100（与 danmaku 服务分页口径一致）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Scope` | `scope` | form | `int32` | 否 | — | — |
| `Oid` | `oid` | form | `int64` | 否 | — | — |
| `OnlyEnabled` | `only_enabled` | form | `bool` | 否 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `OperatorMid` | `operator_mid` | form | `int64` | 是 | — | — |

### `DanmakuBlockWordsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuBlockWordsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamBlockWord`

> danmaku 域请求参数 / action：1 新增或重新启用、2 停用（保留行）、3 物理删除词条； / scope：1 全局、2 分区/单稿件（此时 oid 必填）； / operator_mid 必须 > 0，danmaku 服务以此作为审计主体并拒绝匿名写。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Action` | `action` | json | `int32` | 是 | — | — |
| `Word` | `word` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 否 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `DanmakuBlockWordResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `DanmakuBlockWordResultData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAdminDeleteDanmaku`

> 删除任意弹幕（admin=true）与屏蔽词管理同域；举报队列由 moderation 处理，不在此开放。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Dmid` | `dmid` | json | `int64` | 是 | — | — |
| `OperatorMid` | `operator_mid` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `DanmakuBlockWordsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Words` | `words` | json | `[]DanmakuBlockWordItem` | 是 | — | — |

### `DanmakuBlockWordResultData`

> BlockWord 写操作结果：state 为处理后的状态（1 生效、0 停用）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WordId` | `word_id` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `DanmakuBlockWordItem`

> 屏蔽词条目（与 danmaku.v1.BlockWordInfo 对齐） / scope：1 全局、2 分区/单稿件（scope=2 时 oid 有效）；state：1 生效、0 停用

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `WordId` | `word_id` | json | `int64` | 是 | — | — |
| `Word` | `word` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `int32` | 是 | — | — |
| `Oid` | `oid` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/10-admin-danmaku.md -->
