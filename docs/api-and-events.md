# API、RPC 与事件契约

## 1. API 分层

### 1.1 统一响应信封

所有 HTTP 成功响应和业务错误响应都必须包含以下字段：

| 字段 | 类型 | 规则 |
|---|---|---|
| `code` | integer | `0` 表示成功；非零为稳定业务错误码，不能用 HTTP 状态码替代 |
| `message` | string | 成功固定 `ok`；错误消息可读、脱敏，不暴露 SQL、Token、堆栈或供应商密钥 |
| `data` | object | 成功数据对象；无数据必须为 `{}`，不能省略或返回 `null` |
| `ttl` | integer | 客户端缓存秒数；默认 `0` 表示不缓存，不能把权限/播放签名响应设置成长 TTL |

示例：

```json
{"code":0,"message":"ok","data":{"status":"ok"},"ttl":0}
```

`.api` 中定义具体的 `*Data` 和 `*Response` 类型，再使用 goctl 生成 types/handler；错误由 `common/httpresponse` 注册到 go-zero `httpx`，禁止在 handler 中拼接不一致的 JSON。RPC 不使用此 HTTP 信封，仍采用 protobuf 返回值和 gRPC status。

- `gateway/app` 暴露 `/api/v1/...`，负责端版本、鉴权、限流、风控和聚合。
- `gateway/admin` 暴露 `/admin/v1/...`，负责管理员鉴权、RBAC、限流和响应聚合；业务逻辑在 `services/operation` 等领域服务。
- 服务内部 RPC 使用 protobuf，方法名表达领域动作，例如 `CreateSubmission`、`GetPlaybackToken`，不要暴露数据库 CRUD 作为公共协议。
- 管理接口使用独立权限域和路径，不把管理员字段混入普通客户端响应。
- 客户端支持 Android、iOS、HarmonyOS、桌面端；不创建小程序专用 API。

## 2. 版本和兼容性

- HTTP 路径、protobuf package 和事件名带 `v1`；新增字段优先可选/向后兼容，禁止复用已删除字段编号。
- 删除字段前至少经过一个兼容窗口；消费者先兼容新旧字段，生产者再切换，最后清理旧字段。
- API 响应统一包含业务 code、message、data 和 ttl；trace_id 通过响应头或网关扩展字段传递，内部错误不能泄漏 SQL、Token 或供应商密钥。
- 分页使用 cursor 优先；列表接口限制 page size，避免深分页拖垮数据库。

## 3. 通用请求约束

写请求建议包含 `request_id` 或 `idempotency_key`；服务端用业务唯一键和状态版本双重防重。所有远程调用设置 deadline；重试只用于明确幂等的查询或带幂等键的写入。

## 4. 事件 Envelope

```json
{
  "event_id": "01J...",
  "event_type": "content.published",
  "schema_version": 1,
  "occurred_at": "2026-08-24T00:00:00Z",
  "producer": "video",
  "trace_id": "...",
  "aggregate_type": "submission",
  "aggregate_id": "...",
  "payload": {}
}
```

必填字段用于去重、追踪、路由和版本判断。事件 payload 不放完整身份证、手机号、Token、原始 IP 等敏感数据；需要关联时使用受控 ID。

## 5. 核心事件

| 事件 | 生产者 | 主要消费者 | 说明 |
|---|---|---|---|
| `media.task.v1` | upload/asset | transcode/fingerprint | 转码、截图、字幕、指纹任务 |
| `content.published.v1` | video/catalog/rights | search/recommend/spm/inbox | 发布、下架、过期 |
| `engagement.action.v1` | engagement/social-graph | event-collector/spm/recommend/inbox | 点赞、收藏、关注、分享 |
| `moderation.result.v1` | moderation | video/catalog/comment/danmaku | 机审、人审、申诉结论 |
| `playback.heartbeat.v1` | playback | event-collector/spm | 播放进度和质量指标 |
| `live.state.v1` | live-room/live-ingest | live-gateway/live-media/inbox | 开播、断流、下播 |
| `notification.request.v1` | 各领域服务 | notification | 模板化通知请求 |

SPM 只消费用户行为和内容事件以服务视频推荐，不产生广告相关事件。

## 6. Outbox 与消费状态

领域事务同时写业务表和 `outbox_event`；发布器成功后记录发送时间和次数。消费者维护 `consumer_offset` 或业务处理表，状态至少包含 `received/processing/succeeded/retry/dead_letter`。任何重复、乱序或迟到消息都不能破坏最终状态。
