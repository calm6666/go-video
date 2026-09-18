# common

`common` 只放稳定、无业务归属的公共基础能力。允许范围严格遵循 [AGENTS.md §6](../AGENTS.md)：错误码、日志、trace、go-zero 客户端封装、Redis/MQ/OSS 适配、ID、校验、幂等和事件 envelope。

**禁止放入用户、视频、评论、版权等业务实体、业务状态机和跨服务 DAO。** 公共包变更要评估所有服务的兼容性，避免把 `common` 变成共享业务单体。

## 设计原则

1. **零业务依赖**：只依赖标准库、go-zero 公共能力与必要的外部高性能库，不引入领域 model。
2. **优先复用**：日志和 trace 直接复用 go-zero 的 `logx`/`trace`，不重复造轮子；ID 生成等专用能力直接使用业界标准库（如 `github.com/oklog/ulid/v2`）。
3. **可测可读**：每个子包必须有 `README.md`（说明职责、API、依赖），并提供单元测试覆盖核心路径。
4. **变更纪律**：修改任何代码前先改或新增对应 `README.md`，保持文档与实现同步。

## 子包清单

| 子包 | 职责 | 主要依赖 |
|---|---|---|
| [httpresponse](httpresponse) | HTTP 统一响应信封（`code`/`message`/`data`/`ttl`）和错误处理器 | go-zero `rest/httpx` |
| [ecode](ecode) | 业务错误码注册表，与 `httpresponse` 联动，提供 `Code`/`Message`/`EqualError` | 标准库 |
| [strings](strings) | 字符串切片工具（`JoinInts`/`SplitInts` 等） | 标准库 |
| [timeutil](timeutil) | 时间与 Duration 工具（MySQL timestamp 扫描、`Shrink` 截断超时） | 标准库 |
| [idgen](idgen) | ULID 风格 ID 生成器，用于 `event_id`、`idempotency_key`、`request_id` | `github.com/oklog/ulid/v2` |
| [idempotency](idempotency) | 幂等键构造与状态校验工具，满足 [AGENTS.md §5](../AGENTS.md) 写接口幂等要求 | 标准库、`go-video/common/idgen` |
| [eventenvelope](eventenvelope) | 领域事件信封类型与构造器，对齐 [docs/api-and-events.md §4](../docs/api-and-events.md) | 标准库、`go-video/common/idgen`、`go-video/common/timeutil` |
| [validation](validation) | 通用输入校验（分页、size、长度、枚举、cursor） | 标准库 |
| [errgroup](errgroup) | 带并发 worker 池与 panic recover 的 errgroup，首个错误即取消 | 标准库 |
| [fanout](fanout) | 异步任务执行器，worker+buffer 模型，满队列返回 `ErrFull`，优雅关闭 | 标准库、go-zero `logx` |
| [pool](pool) | 通用对象池（接口+Slice/List 实现、idle 超时清理、Active 上限） | 标准库、`go-video/common/timeutil` |
| [pipeline](pipeline) | 按 Split 分片聚批管道，MaxSize/Interval 触发批量回调 | 标准库、`go-video/common/timeutil` |
| [ratelimit](ratelimit) | 限流器：CoDel 队列、Vegas 自适应、Vegas+CoDel 自适应限流、令牌桶 | 标准库、`golang.org/x/time/rate` |
| [counter](counter) | 线程安全计数器（原子、gauge、滑动窗口、CounterGroup） | 标准库 |
| [dsn](dsn) | DSN 解析并绑定到 struct，validator 校验 | 标准库、`github.com/go-playground/validator/v10`、`go-video/common/timeutil` |
| [deploy](deploy) | 部署环境标识（dev/fat1/uat/pre/prod、region/zone），从 flag/env 读取 | 标准库 |
| [flagvar](flagvar) | 自定义 flag.Value，把逗号分隔字符串解析为切片 | 标准库 |
| [chinese](chinese) | 中文简繁转换（OpenCC 方案，基于 cedar Trie 字典前缀匹配） | `github.com/go-ego/cedar`、go-zero `logx` |
| [netutil](netutil) | 本机内外网 IPv4 获取、IPv4 与 uint32 互转 | 标准库 |
| [feature](feature) | 运行时特性开关（feature gate），支持 flag 解析与默认值 | 标准库 |

## 使用约定

- 服务在 `svc.ServiceContext` 中组合所需 `common/*` 包，不在 handler 中直接使用 ecode 注册。
- 错误码必须先用 `ecode.Register` 注册消息，再通过 `ecode.New` 申请；禁止运行时动态创建未注册码。
- 事件生产者使用 `eventenvelope.New` 构造事件，`event_id` 由 `idgen` 生成，保证全局唯一与时间有序。
- 所有写接口的幂等键通过 `idempotency` 包构造，避免业务侧手拼字符串。

## 变更影响

修改 `common/*` 时必须：

1. 先更新对应子包的 `README.md`，说明新增/变更的 API、依赖与兼容性。
2. 评估所有引用该子包的服务（grep `go-video/common/`）。
3. 删除字段前至少经过一个兼容窗口；新增字段优先可选/向后兼容。
4. 提交信息使用 `chore(common/<sub>): ...`，与业务变更分开。
