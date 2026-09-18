# common/idempotency

幂等键构造与状态校验工具，满足 [AGENTS.md §5](../../AGENTS.md) 写接口幂等要求。

## 职责

- 由业务主键 + 请求类型生成稳定的幂等键（hex SHA-256）。
- 提供 `State` 类型跟踪幂等记录的执行状态：`pending`/`succeeded`/`failed`。
- 提供 `Result` 类型封装幂等命中后的回放结果（原响应或原错误）。

## 依赖

- `go-video/common/idgen`：用于生成客户端未提供幂等键时的默认值。

## API

| 符号 | 说明 |
|---|---|
| `type Key struct` | 携带原始业务键与稳定哈希的幂等键 |
| `func NewKey(parts ...string) Key` | 用业务字段拼接生成 Key，内部做 hex SHA-256 |
| `func (k Key) String() string` | 返回稳定哈希（64 字符 hex） |
| `func (k Key) Raw() string` | 返回业务字段原始拼接（仅用于日志，不持久化） |
| `func GenerateKey() string` | 客户端未提供时生成默认幂等键（`idem_<ULID>`） |
| `type State string` | `StatePending`/`StateSucceeded`/`StateFailed` |
| `func ParseState(s string) (State, error)` | 解析存储中的状态字符串 |
| `type Result struct` | 幂等命中后的回放数据 |
| `func NewHit(state State, response []byte, errCode int, errMessage string) Result` | 构造命中结果 |
| `func NewMiss() Result` | 构造未命中结果 |
| `func (r Result) Hit() bool` | 是否命中已有结果 |
| `func (r Result) State() State` | 原始状态 |
| `func (r Result) Response() []byte` | 原始响应体（命中时回放） |
| `func (r Result) ErrCode() int` | 原始业务错误码（命中时回放） |
| `func (r Result) ErrMessage() string` | 原始错误消息 |
| `func (r Result) Err() error` | 失败状态转为 error 供回放 |

## 使用示例

```go
import "go-video/common/idempotency"

// 在写接口的 logic 层
key := idempotency.NewKey("video.publish", submissionID, clientRequestID)
// 查询幂等表，若命中则回放；否则插入 pending 并执行业务
if clientKey == "" {
    clientKey = idempotency.GenerateKey() // idem_01J9HQA3KX7P2R0V1QTN8D5R4M
}
```

## 实现约定

- Key 哈希算法固定为 SHA-256，hex 编码小写；变更需新函数引入。
- `StateFailed` 允许客户端重试，业务侧决定是否清除 failed 记录。
- 幂等表的实际存储由各服务的 repository 实现，本包只提供类型与构造逻辑。
- 不直接访问数据库或 Redis，仅提供纯计算与序列化。

## 状态语义

| State | 含义 | 命中行为 |
|---|---|---|
| `pending` | 业务执行中 | 客户端应等待或返回 409 Conflict |
| `succeeded` | 业务成功完成 | 回放原始响应 |
| `failed` | 业务执行失败 | 允许重试，需先清除或更新记录 |

## 兼容性

`State` 字符串值与 `Key` 哈希格式一旦发布即冻结。新增状态值必须通过新常量引入，不重命名旧值。
