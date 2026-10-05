# api

存放需要跨服务共享的 protobuf、事件 schema、错误码协议和兼容性说明。服务内部 API 优先放在对应 `services/<service>/api` 或 `rpc`，不要把所有接口集中成一个不可维护的大协议文件。

目录建议：

```text
api/
├── proto/<domain>/v1/*.proto
├── events/<domain>/v1/*.json 或 *.proto
└── README.md
```

`proto/` 和 `events/` 目前不存在：git 不跟踪空目录，也不预建占位骨架。出现第一个真正跨服务共享的
契约或事件 schema 时再创建对应子目录，并把版本（`v1`）写进路径。

当前跨服务契约的实际落点（截至 2026-09-21，均已在仓库里、可编译、有测试）：

| 契约 | 位置 | 说明 |
|---|---|---|
| 领域事件统一信封 | `common/eventenvelope` | `event_id`/`event_type`/`schema_version`/`occurred_at`/`producer`/`trace_id`/`aggregate_*`/载荷，对齐 [docs/api-and-events.md](../docs/api-and-events.md) §4；生产者序列化后与业务事务一起写 Outbox |
| 事件类型与 Topic 命名 | `docs/api-and-events.md` | 单一清单，不散落成多个 schema 文件 |
| 服务自有 RPC 契约 | `services/<service>/rpc/*.proto` | 只被调用方 import 生成 client，不在本目录复制一份 |
| 消费方本地载荷结构 | 如 `services/notification/internal/policy/eventpayload.go` | 信封内的领域载荷，只有出现第二个独立生产者/消费者共用同一载荷时，才提升到 `api/events/<domain>/v1/` |
