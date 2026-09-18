# 命令手册

本文是项目命令的唯一集中维护位置。其它文档只说明流程和约束；命令、参数和执行目录以本文为准。本文同时给出 PowerShell、Windows CMD 和 macOS/Linux 终端命令，示例默认从仓库根目录执行。

“增量生成/增量更新”指只根据本次修改的 `.api` 或 `.proto` 重生成对应服务的框架文件；生成器不会把业务逻辑写入 handler、logic、repository 或 consumer。生成前应确认工作区已有修改，生成后必须检查差异。

## 1. 工具版本和环境

```powershell
go version
goctl --version
protoc --version
go env GOPATH GOMOD GOMODCACHE
docker version
docker compose version
```

当前骨架使用 Go 1.25.x 和 goctl 1.9.x 规划。团队/CI 必须固定版本；升级 goctl 时先在临时目录生成并检查差异。

## 2. Go 模块

```powershell
go mod download
go mod verify
go list -m all
go test -mod=readonly ./...
```

依赖下载需要网络和可写 Go 缓存；离线环境使用已经缓存的模块并显式设置 `GOPROXY=off`，不能删除 `go.sum` 绕过校验。

## 3. Docker Compose 本地依赖

```powershell
./scripts/dev-up.ps1
docker compose -f deploy/docker-compose/docker-compose.yml ps
docker compose -f deploy/docker-compose/docker-compose.yml logs -f mysql
./scripts/dev-up.ps1 -Down
```

本地依赖包含 MySQL、Redis、MinIO 和 Redpanda（Kafka 协议）。本地账号只用于开发，生产 Secret 不得从 `.env.example` 复制到生产。

## 4. go-zero API 生成和验证

### 验证 API 源文件

```powershell
goctl api validate -api gateway/app/api/app.api
goctl api validate -api gateway/admin/api/admin.api
```

领域服务（account、user-profile）只提供 gRPC（无 `.api`），契约校验为 proto
编译（`protoc` 经 `scripts/gen.ps1` 调用）。

### 生成单个 API 服务

```powershell
goctl api go -api gateway/app/api/app.api -dir gateway/app
goctl api go -api gateway/admin/api/admin.api -dir gateway/admin
```

### 批量生成

```powershell
./scripts/gen.ps1
./scripts/gen.ps1 -Service account
```

Windows CMD：

```cmd
scripts\gen.cmd
scripts\gen.cmd account
```

macOS/Linux：

```bash
chmod +x scripts/gen.sh
./scripts/gen.sh
./scripts/gen.sh account
```

API 源修改后，必须先验证，再生成，再检查 `git diff`。handler、路由、types、config、ServiceContext 和入口模板属于生成边界，不得手写或把业务逻辑塞进去。

## 5. go-zero RPC/protobuf 生成

### 单个 RPC

```powershell
Push-Location services/account/rpc
goctl rpc protoc account.proto `
  --go_out=. `
  --go-grpc_out=. `
  --go_opt=paths=source_relative `
  --go-grpc_opt=paths=source_relative `
  --zrpc_out=.. `
  --module (go list -m)
Pop-Location
```

团队应把 goctl/protoc 版本和参数固定到生成脚本；不得为了修复 import 路径手工移动或修改生成文件。若当前 goctl 版本生成了不符合仓库 import 约定的目录，先调整 `go_package`、源文件路径或工具版本，再重新生成。

### 批量生成

```powershell
./scripts/gen.ps1
```

`.proto` 是人工维护的源契约；`.pb.go`、RPC client/server 和 zrpc 框架文件必须由工具生成。修改 proto 后检查 package、go_package、字段编号、兼容性和所有消费者。若 goctl 生成了多余的嵌套 import 路径，应先修正 `go_package` 或生成参数，再提交生成结果，不能手工移动/改写生成文件。

## 6. 格式化、测试和静态检查

```powershell
gofmt -w gateway/app gateway/admin services/account services/user-profile
go test -mod=readonly ./...
go test -mod=readonly ./services/account/...
go vet -mod=readonly ./...
go list ./...
```

受限环境 Go 缓存不可写时，使用仓库临时目录：

```powershell
$env:GOCACHE = (Join-Path (Get-Location) '.gotmp\gocache')
$env:GOTMPDIR = (Join-Path (Get-Location) '.gotmp\gotmp')
New-Item -ItemType Directory -Force -Path $env:GOCACHE,$env:GOTMPDIR | Out-Null
go test -mod=readonly ./...
```

项目脚本：

```powershell
./scripts/test.ps1
```

## 7. 启动 go-zero 服务

```powershell
go run ./gateway/app -f gateway/app/etc/app.yaml
go run ./gateway/admin -f gateway/admin/etc/admin.yaml
go run ./services/account -f services/account/etc/account.v1.yaml
go run ./services/user-profile -f services/user-profile/etc/userprofile.v1.yaml
```

构建：

```powershell
go build -o bin/gateway-app.exe ./gateway/app
go build -o bin/gateway-admin.exe ./gateway/admin
go build -o bin/account.exe ./services/account
go build -o bin/user-profile.exe ./services/user-profile
```

健康检查：

```powershell
Invoke-WebRequest http://127.0.0.1:8080/api/healthz -UseBasicParsing
Invoke-WebRequest http://127.0.0.1:8081/admin/healthz -UseBasicParsing
```

account 与 user-profile 均为纯 RPC 服务（无 HTTP，遵循 AGENTS.md §3/§4），
健康检查使用 gRPC health 探针：

```powershell
grpc_health_probe -addr=127.0.0.1:8083
grpc_health_probe -addr=127.0.0.1:8085
```

当前 gateway/app、gateway/admin 提供可运行 HTTP 入口，account、user-profile
提供可运行 RPC 入口；其它服务要先由 goctl 生成入口和 API/RPC，再加入启动编排。

## 8. 数据库迁移

当前迁移脚本是占位流程：

```powershell
./scripts/migrate.ps1 -Action status
./scripts/migrate.ps1 -Action up
```

首个服务 schema 落地后，固定迁移工具和连接参数，再将命令接入脚本。迁移文件查看：

```powershell
Get-ChildItem deploy/migrations -Recurse -File
```

禁止直接在生产数据库执行未评审 SQL；每个迁移必须记录 forward、rollback/补偿和锁风险。

## 9. Docker Compose 服务

```powershell
docker compose -f deploy/docker-compose/docker-compose.yml config
docker compose -f deploy/docker-compose/docker-compose.yml up -d
```

当前服务 Dockerfile 仍待各服务实现后补齐；不要将不存在的 Dockerfile 构建命令加入 CI。

## 10. Kubernetes 校验和部署

```powershell
kubectl apply --dry-run=client -f deploy/k8s/
kubectl diff -f deploy/k8s/
kubectl apply -f deploy/k8s/
kubectl rollout status deployment/<service> -n go-video
kubectl logs deployment/<service> -n go-video --tail=200
```

生产部署必须先 staging，Secret 使用外部 Secret/Vault；不要把本地密码写入 manifest。

## 11. 代码生成差异检查

```powershell
git diff -- services gateway/app gateway/admin
git status --short
./scripts/gen.ps1
git diff --exit-code -- services gateway/app gateway/admin
```

最后一条只适合“生成后工作区应无变化”的 CI 检查；开发者修改 `.api`/`.proto` 后应预期生成文件产生差异并提交。

## 12. 排障命令

```powershell
go env GOPROXY GOSUMDB GOCACHE GOTMPDIR
docker compose -f deploy/docker-compose/docker-compose.yml ps
docker compose -f deploy/docker-compose/docker-compose.yml logs --tail=200 <service>
Get-NetTCPConnection -LocalPort 8080 -ErrorAction SilentlyContinue
Get-Process go, app, admin, account -ErrorAction SilentlyContinue
```

排障时先保留错误日志、trace_id、服务版本、配置版本和事件 ID；不要通过关闭鉴权、删库、删除死信或手改生成代码“解决”问题。

## 13. 命令与生成纪律

- 所有 goctl 命令必须从仓库根目录执行，或在命令中使用明确的 `-api`/`-dir` 路径。
- 生成前确认目标目录内的业务逻辑不会被覆盖；生成后立即运行格式化、测试和差异检查。
- 不手写 handler、路由、types、RPC client/server、ServiceContext、`.pb.go` 或 go-zero 入口模板。
- 允许手写：`.api`、`.proto`、业务 `logic`、`repository`、`consumer`、领域策略、迁移、事件 schema、配置样例和测试。
- 如果生成器无法表达需求，先调整源契约或增加明确的手写扩展点，不直接改生成文件。

## 14. 修改 API/RPC 后的增量更新

修改 `.api`（网关）或 `.proto`（领域服务）后按“校验、生成、格式化、测试、
差异检查”执行。网关 API 增量更新命令如下（将 `gateway/app` 替换为目标网关）：

PowerShell API：

```powershell
goctl api validate -api gateway/app/api/app.api
goctl api go -api gateway/app/api/app.api -dir gateway/app
gofmt -w gateway/app
go test -mod=readonly ./gateway/app/...
go vet -mod=readonly ./gateway/app/...
git diff -- gateway/app
```

领域服务 RPC（将 `account` 替换为目标服务；推荐直接使用 `scripts/gen.ps1`）：

```powershell
./scripts/gen.ps1 -Service account
gofmt -w services/account
go test -mod=readonly ./services/account/...
go vet -mod=readonly ./services/account/...
git diff -- services/account
```

Windows CMD API：

```cmd
goctl api validate -api gateway\app\api\app.api
goctl api go -api gateway\app\api\app.api -dir gateway\app
gofmt -w gateway\app
go test -mod=readonly ./gateway/app/...
go vet -mod=readonly ./gateway/app/...
git diff -- gateway\app
```

macOS/Linux API：

```bash
goctl api validate -api gateway/app/api/app.api
goctl api go -api gateway/app/api/app.api -dir gateway/app
gofmt -w gateway/app
go test -mod=readonly ./gateway/app/...
go vet -mod=readonly ./gateway/app/...
git diff -- gateway/app
```

脚本方式：PowerShell 使用 `./scripts/gen.ps1 -Service <svc>`，Windows CMD 使用
`scripts\gen.cmd <svc>`，macOS/Linux 使用 `./scripts/gen.sh <svc>`。
生成后禁止手工移动或修改 `.pb.go`、handler、路由、types、ServiceContext 和
RPC client/server；生成文件内容与 proto `go_package` 保持一致（不带包名别名，
goctl 生成 `rpc.` 引用，无需任何手工修正）。
