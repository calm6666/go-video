# scripts

脚本只做可重复的工程操作：代码生成、格式检查、测试、迁移、启动本地依赖和构建镜像。完整命令见 [../docs/commands.md](../docs/commands.md)。脚本必须支持 dry-run 或明确目标环境，禁止把生产密钥写入脚本。

当前提供：

- `gen.ps1 [-Service <name>]`：按服务执行 goctl API/RPC 生成，并清理 zrpc 多余产物
  （`<svc>/` client 包装目录、与 `.api` 单入口冲突的 `<svc>.v1.go`/`etc/<svc>.v1.yaml`）。
  客户端一律用 `rpc/<svc>_grpc.pb.go` 里的 `New<Service>Client`。
  脚本里的 `$descriptorPrefixedProtos`（当前是 `membership.proto`）会在 goctl 之后再用
  `protoc -I .` 从仓库根生成一次，使其 descriptor 路径带上目录，避免与依赖注册的裸名
  在全局 protoregistry 撞名后 init panic；原因和排查方式见 [../docs/commands.md](../docs/commands.md) §5。
- `gen.sh [service]` / `gen.cmd [service]`：同一生成流程的 Linux/macOS 版本与 CMD 包装
  （`gen.cmd` 只是转调 `gen.ps1`）。`gen.sh` 的清理与 descriptor 列表必须与 `gen.ps1` 同步。
- `test.ps1`：使用仓库内临时 Go 缓存执行 `go test -p 1 ./...` 和 `go vet ./...`
  （整树测试必须 `-p 1`，否则在 Windows 上会在链接期撞页面文件上限，原因见
  [../docs/commands.md](../docs/commands.md) §6）。
- `dev-up.ps1 [-Down]`：启动或停止本地 Docker Compose 依赖（含 OpenSearch）。
- `migrate.ps1 -Action status|up [-Service <name>]`：按 `deploy/migrations/<service>` 执行 MySQL 迁移，
  默认从 `services/<service>/etc/*.yaml` 的 `DataSource` 取连接参数；已应用记录在各库的 `schema_migrations`。
  ⚠ 该脚本会真的写库：验证时必须用 `-OverrideHost/-OverridePort/...` 指向隔离实例，禁止默认连本机 3306。
- `gen-api-docs.mjs`：接口文档/Postman/RPC 冒烟的**唯一生成入口**。从 `gateway/*/**.api`、`services/*/rpc/*.proto`
  和 `gateway/admin` 的 `routePermissions` 表解析出全部接口，按域分组写出 `docs/api/`、`postman/`、`scripts/rpc/`。
  `node scripts/gen-api-docs.mjs` 生成并写盘；`--check` 只校验（四道门禁：`.api` 路由与 `routes.go` 逐条一致、
  请求/响应类型与 logic 文件真实存在、`form` 入参可编码进查询串且 GET 不带 `json` 字段、产物内部链接可解析）
  且不写文件。产物是生成物，手工改动会被下次生成覆盖。
- `rpc/smoke.sh` / `rpc/smoke.ps1`：由上一条目生成的 43 服务 / 589 方法 `grpcurl` 冒烟脚本，按服务分节。
  **本仓从未执行过**（没有运行中的服务与 etcd），只作联调入口，不构成通过/失败断言；详见 [rpc/README.md](rpc/README.md)。
- `es-init.ps1`：OpenSearch 索引结构与分词验证 + 样例文档灌入（只写显式命名的物理索引，
  不碰别名）。索引结构由 `go run ./services/search-indexer/cmd/esmapping` 现场导出。

后续补充 Linux shell 版本时，必须保持参数和安全行为一致。
