# 终端面 · `/video`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| video 域聚合（services/video/rpc/video.proto） | 免鉴权 | 4 |
| video 增量：稿件编辑与删除 | 免鉴权 | 2 |

合计 **6** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## video 域聚合（services/video/rpc/video.proto）（免鉴权，4 条）

> video 域路由

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/video/submissions` | 创建稿件（DRAFT 状态） | `createSubmission` | `createsubmissionlogic.go` |
| GET | `/video/submissions/:aid` | 查询稿件详情 | `getSubmission` | `getsubmissionlogic.go` |
| GET | `/video/submissions` | 分页查询稿件（按 mid 或 typeid 过滤） | `listSubmissions` | `listsubmissionslogic.go` |
| POST | `/video/submissions/:aid/transition` | 推进稿件状态机（发布/删除等，校验合法转换） | `transitionState` | `transitionstatelogic.go` |

### POST `/video/submissions` — 创建稿件（DRAFT 状态）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/createsubmissionhandler.go`
- 业务实现：`gateway/app/internal/logic/createsubmissionlogic.go`

请求：`ParamCreateSubmission`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 是 | — | — |
| `Desc` | `desc` | form | `string` | 是 | — | — |
| `Cover` | `cover` | form | `string` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Tag` | `tag` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/video/submissions/:aid` — 查询稿件详情

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/getsubmissionhandler.go`
- 业务实现：`gateway/app/internal/logic/getsubmissionlogic.go`

请求：`ParamVideoAid`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/video/submissions` — 分页查询稿件（按 mid 或 typeid 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/listsubmissionshandler.go`
- 业务实现：`gateway/app/internal/logic/listsubmissionslogic.go`

请求：`ParamListSubmissions`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`VideoSubmissionsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/video/submissions/:aid/transition` — 推进稿件状态机（发布/删除等，校验合法转换）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/transitionstatehandler.go`
- 业务实现：`gateway/app/internal/logic/transitionstatelogic.go`

请求：`ParamTransition`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Target` | `target` | form | `int32` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## video 增量：稿件编辑与删除（免鉴权，2 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/video/submissions/:aid/update` | 编辑稿件元信息（空字段表示不更新，须为所有者） | `updateSubmission` | `updatesubmissionlogic.go` |
| POST | `/video/submissions/:aid/delete` | 删除稿件（软删，状态机推进到 DELETED） | `deleteSubmission` | `deletesubmissionlogic.go` |

### POST `/video/submissions/:aid/update` — 编辑稿件元信息（空字段表示不更新，须为所有者）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/updatesubmissionhandler.go`
- 业务实现：`gateway/app/internal/logic/updatesubmissionlogic.go`

请求：`ParamUpdateSubmission`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 否 | — | — |
| `Desc` | `desc` | form | `string` | 否 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `Typeid` | `typeid` | form | `int32` | 否 | — | — |
| `Tag` | `tag` | form | `string` | 否 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/video/submissions/:aid/delete` — 删除稿件（软删，状态机推进到 DELETED）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/deletesubmissionhandler.go`
- 业务实现：`gateway/app/internal/logic/deletesubmissionlogic.go`

请求：`ParamDeleteSubmission`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamCreateSubmission`

> video 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 是 | — | — |
| `Desc` | `desc` | form | `string` | 是 | — | — |
| `Cover` | `cover` | form | `string` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Tag` | `tag` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `VideoSubmissionResponse`

> video 域响应信封

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamVideoAid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamListSubmissions`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `VideoSubmissionsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamTransition`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Target` | `target` | form | `int32` | 是 | — | — |
| `Operator` | `operator` | form | `string` | 是 | — | — |
| `Reason` | `reason` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamUpdateSubmission`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Title` | `title` | form | `string` | 否 | — | — |
| `Desc` | `desc` | form | `string` | 否 | — | — |
| `Cover` | `cover` | form | `string` | 否 | — | — |
| `Typeid` | `typeid` | form | `int32` | 否 | — | — |
| `Tag` | `tag` | form | `string` | 否 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ParamDeleteSubmission`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `VideoSubmissionData`

> video 域响应数据载荷

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Submission` | `submission` | json | `VideoSubmission` | 是 | — | — |

### `VideoSubmissionsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Submissions` | `submissions` | json | `[]VideoSubmission` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `VideoSubmission`

> 稿件详情（对应 video.Submission）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Desc` | `desc` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `Typeid` | `typeid` | json | `int32` | 是 | — | — |
| `Tag` | `tag` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/08-video.md -->
