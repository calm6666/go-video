# scripts

脚本只做可重复的工程操作：代码生成、格式检查、测试、迁移、启动本地依赖和构建镜像。完整命令见 [../docs/commands.md](../docs/commands.md)。脚本必须支持 dry-run 或明确目标环境，禁止把生产密钥写入脚本。

当前提供：

- `gen.ps1 [-Service <name>]`：按服务执行 goctl API/RPC 生成。
- `test.ps1`：使用仓库内临时 Go 缓存执行 `go test ./...` 和 `go vet ./...`。
- `dev-up.ps1 [-Down]`：启动或停止本地 Docker Compose 依赖。
- `migrate.ps1 -Action status|up`：迁移执行器占位，首个 schema 落地后接入固定工具。

后续补充 Linux shell 版本时，必须保持参数和安全行为一致。
