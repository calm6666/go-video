# common/idgen

ID 生成器，用于 `event_id`、`idempotency_key`、`request_id`、`trace_id` 等需要全局唯一且时间有序的标识符。

## 职责

- 生成 26 字符 ULID 字符串（Crockford Base32，时间有序），与 [docs/api-and-events.md §4](../../docs/api-and-events.md) 事件 `event_id` 示例 `01J...` 对齐。
- 生成短随机 ID（Base64URL，无 padding）。
- 生成带前缀的命名 ID（例如 `evt_`、`req_`、`idem_`）。
- 提供 Generator 接口和测试钩子，便于替换实现。

## 依赖

- [`github.com/oklog/ulid/v2`](https://github.com/oklog/ulid)：业界标准实现，提供 Lock-free 单调 entropy、并发安全和 Crockford Base32 编码。

## API

| 符号 | 说明 |
|---|---|
| `func ULID() (string, error)` | 生成 26 字符 ULID 字符串 |
| `func MustULID() string` | 同上，panic on error；仅用于初始化或不可恢复路径 |
| `func Short(n int) (string, error)` | 生成 n 字节随机 Base64URL 字符串（无 padding） |
| `func Prefixed(prefix string) (string, error)` | 生成 `prefix_<ULID>` 形式 ID |
| `type Generator interface` | `ULID()/Short(n)/Prefixed(prefix)`，可被替换实现 |
| `func NewGenerator(r io.Reader) Generator` | 用给定熵源构造生成器（默认 `crypto/rand`） |
| `func SetDefault(g Generator)` | 替换包级默认生成器；测试用 |

## ULID 实现说明

ULID 规范：<https://github.com/ulid/spec>

- 时间戳：48-bit Unix 毫秒（前 10 字符）
- 熵：80-bit 随机或单调序列（后 16 字符）
- 编码：Crockford Base32，字符集 `0123456789ABCDEFGHJKMNPQRSTVWXYZ`

使用 `ulid.Monotonic` entropy 保证：

1. **同一毫秒内单调递增**：entropy 内部维护最后时间戳与序列号，同毫秒内序列 +1。
2. **进程内并发安全**：`ulid.Monotonic` 内部使用 Lock-free 自旋。
3. **跨进程唯一性**：首次调用读取 80-bit 随机熵（`crypto/rand`），冲突概率可忽略。

## 使用示例

```go
import "go-video/common/idgen"

eventID, _ := idgen.ULID()              // "01J9HQA3KX7P2R0V1QTN8D5R4M"
idemKey, _ := idgen.Prefixed("idem")    // "idem_01J9HQA3KX7P2R0V1QTN8D5R4M"
reqID, _ := idgen.Short(16)             // 22 字符 Base64URL
```

## 实现约定

- `MustULID` 仅在不可恢复路径调用；业务路径必须处理 error。
- 默认熵源是 `crypto/rand`；测试可通过 `NewGenerator(strings.NewReader(...))` 注入确定性熵。
- 包级默认生成器可被 `SetDefault` 替换；测试结束必须恢复 `nil`。

## 兼容性

ULID 字符串格式一旦发布即冻结，未来变更必须保持 26 字符长度与 Crockford 字符集。修改内部算法需通过新函数名引入，不破坏现有调用。
