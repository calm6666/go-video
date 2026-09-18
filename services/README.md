# services

本目录包含所有领域服务。服务按数据所有权划分，逻辑服务可以在第一阶段合并进程，但必须保留独立包边界、API/RPC 契约和数据访问接口。

每个服务至少包含 `README.md`；实现时必须先维护 `.api`/`.proto` 等源契约，再使用 goctl 生成 `api/`、`rpc/`、`internal/`、`model/`、`etc/` 和入口框架。除业务 `logic`、`repository`、`consumer`、领域策略和测试外，不得手写或修改生成代码。服务只能写自己的数据库/schema，跨服务通过 RPC/API 或事件通信。

服务详细职责见 [../docs/service-catalog.md](../docs/service-catalog.md)。
