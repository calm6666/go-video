# common/eventenvelope

领域事件信封类型与构造器，对齐 [docs/api-and-events.md §4](../../docs/api-and-events.md) 的事件规范，供 Outbox 表写入和事件发布器使用。

## 职责

- 定义事件信封结构：`event_id`/`event_type`/`schema_version`/`occurred_at`/`producer`/`trace_id`/`aggregate_type`/`aggregate_id`/`payload`。
- 提供 `New` 构造器，自动填充 `event_id`、`occurred_at` 与可空字段。
- 提供 JSON 序列化/反序列化，便于写入 outbox 表和投递到 MQ。
- 提供字段必填校验，防止发布残缺事件。

## 依赖

- `go-video/common/idgen`：用于生成 `event_id`（ULID 字符串）。
- `go-video/common/timeutil`：用于 `occurred_at` 的 RFC3339 格式化。

## API

| 符号 | 说明 |
|---|---|
| `type Envelope struct` | 事件信封，所有字段对齐 docs/api-and-events.md |
| `func New(producer, eventType, aggregateType, aggregateID string, schemaVersion int, payload json.RawMessage, traceID string) (*Envelope, error)` | 构造并校验事件 |
| `func (e *Envelope) MarshalJSON() ([]byte, error)` | 序列化为 outbox 表存储格式 |
| `func (e *Envelope) UnmarshalJSON(b []byte) error` | 反序列化用于消费者 |
| `func (e *Envelope) Validate() error` | 检查必填字段、schema_version 正数、payload 非 nil |
| `func Topic(eventType string, schemaVersion int) string` | 生成 `producer.event.v1` 风格的 Topic 名 |

## 字段约束

| 字段 | 必填 | 说明 |
|---|---|---|
| `event_id` | 是 | ULID 字符串，26 字符 |
| `event_type` | 是 | 形如 `content.published`，点分小写 |
| `schema_version` | 是 | 正整数，从 1 开始 |
| `occurred_at` | 是 | RFC3339 UTC 时间戳 |
| `producer` | 是 | 生产者服务名，如 `video`、`engagement` |
| `trace_id` | 否 | W3C trace_id；无链路时为空 |
| `aggregate_type` | 是 | 聚合类型，如 `submission`、`user` |
| `aggregate_id` | 是 | 聚合主键，业务字符串 |
| `payload` | 是 | JSON 编码的业务数据；可为 `{}` |

## 使用示例

```go
import (
    "encoding/json"
    "go-video/common/eventenvelope"
)

type publishedPayload struct {
    SubmissionID string `json:"submission_id"`
}

payload, _ := json.Marshal(publishedPayload{SubmissionID: "sub_123"})
event, err := eventenvelope.New(
    "video",
    "content.published",
    "submission",
    "sub_123",
    1,
    payload,
    traceID,
)
if err != nil { return err }
raw, _ := event.MarshalJSON()
// 写入 outbox 表，发布器读取后投递到 MQ
_ = raw
```

## 实现约定

- `payload` 必须为 `json.RawMessage`，避免结构耦合；调用方负责编码。
- 事件字段顺序、命名与 [docs/api-and-events.md §4](../../docs/api-and-events.md) 严格对齐。
- 反序列化失败返回错误，不静默丢弃字段。
- 不直接访问 MQ 或数据库；本包只提供类型与序列化。

## Topic 命名

`Topic(eventType, schemaVersion)` 返回 `producer.event.vN` 形式：

- `Topic("content.published", 1)` → `"content.published.v1"`
- producer 由 `eventType` 的第一段隐含表达，调用方负责一致使用。

## 兼容性

`Envelope` 的 JSON 字段名一旦发布即冻结。新增字段必须为可选（`omitempty`），且不删除已有字段编号；删除字段需先经过一个兼容窗口。
