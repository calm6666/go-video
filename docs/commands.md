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

当前仓库使用 Go 1.25.x 与 goctl 1.10.x（本次验证环境实测 `go1.25.3` + `goctl 1.10.2`，与 `go.mod` 的 go-zero 1.9.x 配套）。团队/CI 必须固定版本；升级 goctl 时先在临时目录生成并检查差异。

## 2. Go 模块

```powershell
go mod download
go mod verify
go list -m all
go test -mod=readonly -p 1 ./...
```

依赖下载需要网络和可写 Go 缓存；离线环境使用已经缓存的模块并显式设置 `GOPROXY=off`，不能删除 `go.sum` 绕过校验。

## 3. Docker Compose 本地依赖

```powershell
./scripts/dev-up.ps1
docker compose -f deploy/docker-compose/docker-compose.yml ps
docker compose -f deploy/docker-compose/docker-compose.yml logs -f mysql
./scripts/dev-up.ps1 -Down
```

本地依赖包含 MySQL、Redis、MinIO、Redpanda（Kafka 协议）和 OpenSearch（搜索投影，见 §15）。本地账号只用于开发，生产 Secret 不得从 `.env.example` 复制到生产。

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

### 接口文档、Postman 集合与 RPC 冒烟脚本

`docs/api/`、`postman/` 和 `scripts/rpc/smoke.*` 都不是手写文件，而是同一份契约
（两个 `.api`、全部 `services/*/rpc/*.proto` 和 `routePermissions` 表）的产物：

```powershell
node scripts/gen-api-docs.mjs          # 从契约生成接口文档 / 集合 / 冒烟脚本并写盘
node scripts/gen-api-docs.mjs --check  # 只校验：漂移门禁 + 产物是否最新，过期即 exit 1
```

脚本自带的门禁，任何一道不过就整批失败：`.api` 路由与 `routes.go` 注册必须逐条一致；
每个路由的请求/响应类型和对应 logic 文件必须真实存在；`form` 标签的入参必须能编码进
查询串（结构体/map 直接报错）；GET 类请求不允许挂 `json` 字段；产物内部相对链接必须可解析。
`--check` 模式在此基础上做逐字节比对。Node 18 以上（用到 ESM 与 `String.matchAll`），无第三方依赖。

改接口文档的正确顺序仍然是：改 `.api`/`.proto` → `goctl`/`protoc` 重新生成 →
`node scripts/gen-api-docs.mjs` → `git diff`。直接编辑 `docs/api/**`、`postman/**`
或 `scripts/rpc/smoke.*` 会在下一次生成时被覆盖。

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

### descriptor 路径撞名（全局注册表）

protobuf 的全局 descriptor 注册表按**文件路径**去重，不看 package。上面这种「在 `rpc/` 目录里用裸文件名生成」的约定，会让 descriptor 路径等于裸名（如 `membership.proto`）。一旦某个依赖注册了同名裸名，同时链接两者的进程就会在 init 阶段 panic：

```text
panic: proto: file "membership.proto" is already registered
```

已确认的实例：`go.etcd.io/etcd/api/v3/membershippb` 注册了裸名 `membership.proto`，而每个 zrpc 服务都经 etcd 服务发现链接了 clientv3，所以 membership 域的 descriptor 必须带目录。处理方式是把该 proto 追加进 `scripts/gen.ps1` 的 `$descriptorPrefixedProtos`（`scripts/gen.sh` 里同名数组保持同步），生成脚本会在 goctl 之后用仓库根相对路径再跑一次 protoc：

```powershell
# 在仓库根执行；生成物仍落在 services/<svc>/rpc/ 旁边，Go 包名不变
protoc -I . --go_out=. --go-grpc_out=. `
  --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative `
  services/membership/rpc/membership.proto
```

判据：生成后 `head -6 services/<svc>/rpc/<name>.pb.go` 的 `// source:` 行就是 descriptor 路径。排查冲突：先 `go list -deps ./<pkg> | grep etcd/api/v3` 确认两边都链进来了，再用 `rg "^// source: " "$GOPATH/pkg/mod/go.etcd.io"` 比对裸名。不要靠设 `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn` 把 panic 静音——那只是让测试能跑，进程仍然带着两份冲突描述符。

### 批量生成

```powershell
./scripts/gen.ps1
```

`.proto` 是人工维护的源契约；`.pb.go`、RPC client/server 和 zrpc 框架文件必须由工具生成。修改 proto 后检查 package、go_package、字段编号、兼容性和所有消费者。若 goctl 生成了多余的嵌套 import 路径，应先修正 `go_package` 或生成参数，再提交生成结果，不能手工移动/改写生成文件。

## 6. 格式化、测试和静态检查

```powershell
gofmt -w gateway/app gateway/admin services/account services/user-profile
go test -mod=readonly -p 1 ./...
go test -mod=readonly ./services/account/...
go vet -mod=readonly ./...
go list ./...
```

**整树 `go test` 必须带 `-p 1`**（单包命令不需要）。本机 Windows 页面文件不足以同时链接多个测试包，
并发跑会在链接期报 `fork/exec ... The paging file is too small for this operation to complete.
(errno=1455)`——这是环境限制而不是被测代码失败，容易被误读成「代码坏了」。
另外 `go build ./...` 与 `gofmt -l` 都**不编译 `_test.go`**，所以「测试能跑」的唯一可信口径是
`go vet ./...` + `go test -p 1 -count=1 ./...` 两条都过（历史教训见 docs/roadmap.md 的实测表一节）。

受限环境 Go 缓存不可写时，使用仓库临时目录：

```powershell
$env:GOCACHE = (Join-Path (Get-Location) '.gotmp\gocache')
$env:GOTMPDIR = (Join-Path (Get-Location) '.gotmp\gotmp')
New-Item -ItemType Directory -Force -Path $env:GOCACHE,$env:GOTMPDIR | Out-Null
go test -mod=readonly -p 1 ./...
```

Kafka 的消费者与生产者实现都按构建标签开关，默认构建不链接 kq（启动时显式返回
`ErrKafkaRuntimeNotBuilt` 并写日志，不是静默空转），改动任一侧时必须连带标签一起验证：

```powershell
go build -tags inbox_kafka ./services/inbox/...
go build -tags notification_kafka ./services/notification/...
go vet -tags "inbox_kafka notification_kafka" ./services/inbox/... ./services/notification/...
go test -p 1 -count=1 -tags inbox_kafka ./services/inbox/...
go test -p 1 -count=1 -tags notification_kafka ./services/notification/...
# search-indexer：适配器在 internal/consumer/queue.go，kq 只在带标签文件里
go build -tags searchindexer_kafka ./services/search-indexer/...
go vet -tags searchindexer_kafka ./services/search-indexer/...
go test -p 1 -count=1 -tags searchindexer_kafka ./services/search-indexer/...
# live-room：live.state.v1 的入站消费者
go build -tags liveroom_kafka ./services/live-room/...
go vet -tags liveroom_kafka ./services/live-room/...
go test -p 1 -count=1 -tags liveroom_kafka ./services/live-room/internal/consumer/...
# live-ingest：live.state.v1 的 Outbox 发布器（生产者），循环写在服务内
go build -tags liveingest_kafka ./services/live-ingest/...
go vet -tags liveingest_kafka ./services/live-ingest/internal/publisher/
go test -p 1 -count=1 -tags liveingest_kafka ./services/live-ingest/internal/publisher/
# playback：playback.heartbeat.v1 的发布器（循环复用 common/outbox，服务内只有适配层）
go build -tags playback_kafka ./services/playback/...
go vet -tags playback_kafka ./services/playback/...
go test -p 1 -count=1 -tags playback_kafka ./services/playback/internal/publisher/
# live-media：9 个 livemedia.*.v1 的发布器（同样复用 common/outbox；PublishTopics 要与 model 的
# 9 个 EventType* 派生集合相等，缺一条那类事件就会每轮撞「没有发送通道」直到判死）
# 同一个标签与同一个 Kafka.Enabled 还覆盖本服务的 live.state.v1 消费者（internal/consumer），
# 所以这一组标签命令同时是发布侧与消费侧的验证入口，没有单独的 ConsumeEnabled
go build -tags livemedia_kafka ./services/live-media/...
go vet -tags livemedia_kafka ./services/live-media/...
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/publisher/
go test -p 1 -count=1 -tags livemedia_kafka ./services/live-media/internal/consumer/
# recommend-recall：recall.pool.published.v1 的发布器（第三个 common/outbox 使用方；本服务的
# ListPending 对 limit<=0 或 >model.MaxOutboxBatch 是报错而不是钳制，所以 BatchLimit 有构造期门禁）
go build -tags recommendrecall_kafka ./services/recommend-recall/...
go vet -tags recommendrecall_kafka ./services/recommend-recall/...
go test -p 1 -count=1 -tags recommendrecall_kafka ./services/recommend-recall/internal/publisher/
# upload：media.task.v1 的发布器（第四个 common/outbox 使用方；只有 CompleteUpload 写事件行，
# 且 PublishTopics 必须恰好等于派生出的 media.task.v1，多一个键名都拒启动）
go build -tags upload_kafka ./services/upload/...
go vet -tags upload_kafka ./services/upload/...
go test -p 1 -count=1 -tags upload_kafka ./services/upload/...
# video：content.published.v1 的发布器（第五个 common/outbox 使用方；只有可见性转换写事件行，
# 判定表见 internal/repository/contentevent.go，PublishTopics 必须恰好等于 content.published.v1）
# tag 只在 internal/publisher 换文件，所以带标签的 test 单独跑那一包即可归因
go build -tags video_kafka ./services/video/...
go vet -tags video_kafka ./services/video/...
go test -p 1 -count=1 -tags video_kafka ./services/video/internal/publisher/
# common/outbox：发布循环本体（顺序/退避/判死/启停）的用例，无构建标签
go test -p 1 -count=1 -timeout 120s ./common/outbox/...
```

2026-10-05 实测：上述各条全部 rc=0（默认构建与带标签构建同时绿）。动态用例数（`-v` 的
`--- PASS` 计数，格式 `顶层/子用例`）：live-ingest `internal/publisher` 默认 `28/34`、带标签 `30/38`；
playback `internal/publisher` 默认 `20/21`、带标签 `21/25`；live-room `internal/consumer` 33 条；
`common/outbox` `20/30`；live-media `internal/publisher` 默认 `25/37`、带标签 `27/41`，
`model` `28/4`，`internal/consumer` 默认与带标签都是 `41/60`（7 个测试文件，两侧只是
`kafkaruntime_disabled_test.go` 与 `kafkaruntime_kafka_test.go` 互换，所以总数不变），
recommend-recall `internal/publisher` 默认 `25/28`、带标签 `27/32`，
`model` `30/5`、`internal/logic` `105/110`；upload `internal/publisher` 默认 `21/21`、带标签 `22/25`，
`internal/logic` `59/28`、`internal/config` `1/1`（即 `./services/upload/...` 默认构建 131 条、
带标签跑 publisher 47 条）；video `internal/publisher` 默认 `22/21`、带标签 `23/25`，
`./services/video/...` 默认构建 `115/447`（115 顶层，其中 logic `88/310`、publisher `22/21`、
repository `4/0`、config `1/1`），带标签跑整树同样 rc=0。live-media 的 publisher/model 与 recommend-recall 这几条为 2026-10-05 复测，
upload 三条与 video 四条为同日接线当轮实测，live-media `internal/consumer` 为接线当轮复测，SKIP/FAIL 均为 0。
标签只影响是否链接 kq，
**不代表能与任何 broker 通信**。本仓库从未做过 Kafka 实机联调，
证据层级只到「可编译 / 可静态检查 / 该包单测通过」，各服务 README 的「已知缺口」都按这个口径写。

`github.com/zeromicro/go-queue/kq` 已进入 `go.mod` 直接依赖（2026-09 批次补齐 `go.sum`），
带标签构建在 `-mod=readonly` 下即可通过；消费侧标签实现见各服务 `internal/consumer/kafkaruntime_*.go`，
生产侧见 `services/live-ingest/internal/publisher/kafkaruntime_*.go`、
`services/playback/internal/publisher/kafkaruntime_*.go`、
`services/live-media/internal/publisher/kafkaruntime_*.go`、
`services/recommend-recall/internal/publisher/kafkaruntime_*.go`、
`services/upload/internal/publisher/kafkaruntime_*.go` 与
`services/video/internal/publisher/kafkaruntime_*.go`（后五者的发布循环复用 `common/outbox`，
服务内只有列映射、topic 归属与配置→参数的适配层；live-ingest 仍是迁移前自己的那份循环，差异见
[common/outbox/README.md](../common/outbox/README.md) 缺口 1）。
`-race` 在本机不可用（`CGO_ENABLED=0` 且没有 gcc），并发语义只能靠生命周期用例覆盖，
不要在任何文档或脚本里写「已跑过 race 检测」。

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

其余已落地 model/RPC 契约的服务同构（入口文件名和配置文件名见 `services/<svc>/` 与 `services/<svc>/etc/`）：
`creator`、`upload`、`asset`、`transcode`、`video`、`catalog`、`rights`、`content-fingerprint`、
`moderation-orchestrator`、`moderation-worker`、`playback`、`danmaku`、`comment`、`engagement`、
`social-graph`、`feed`、`search-indexer`、`search-query`、`risk-control`、`inbox`、`notification`、`operation`、
`audit`、`ops-config`、`cron`、`event-collector`、`spm`、`feature-store`、`recommend-recall`、`recommend-rank`、
`live-room`、`live-ingest`、`live-media`、`live-gateway`、`private-message`、`open-platform`，
以及商业化五域 `membership`、`payment`、`trade-order`、`coin`、`creator-revenue`。
全部 43 个领域服务都有可运行入口（`services/<svc>/<svc>.v1.go` + `services/<svc>/etc/<svc>.v1.yaml`；
目录名带连字符的服务入口与配置去掉连字符，如 `services/feature-store/featurestore.v1.go`，
`go run` 时以实际文件名为准），
端口分配见 [roadmap.md](roadmap.md) 的服务清单。

这些服务只提供 gRPC，没有 HTTP `.api`，因此不带 `/api/healthz` 之类的 HTTP 探活：
启动后可用 `grpcurl` 或调用方（`gateway/app`、`gateway/admin`）验证可达性；
配置加载与 `Validate()` 的可验证性由各服务 `internal/config/config_load_test.go` 覆盖。

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

每个服务的 HTTP/gRPC 端口以自身 `etc/*.yaml` 的 `Port`/`ListenOn` 为准（网关 8080/8081；
阶段 1-2 新服务占用 8100-8109，见 [docs/roadmap.md](roadmap.md) 的“实现进度”表）。
新增服务入口后要把健康检查加入部署编排，RPC 服务用 gRPC health 探针。

## 8. 数据库迁移

迁移脚本按服务放在 `deploy/migrations/<service>/NNNNNN_description.sql`，由 `scripts/migrate.ps1` 执行。
连接参数和库名取自各服务 `services/<service>/etc/*.yaml` 的 `DataSource`（同一份配置，不再维护第二处 DSN），
已应用的版本记录在该库的 `schema_migrations` 表，按文件名升序、只执行一次。

```powershell
./scripts/migrate.ps1 -Action status              # 所有服务：列出 applied/pending
./scripts/migrate.ps1 -Action up                  # 所有服务：建库（IF NOT EXISTS）并应用待执行迁移
./scripts/migrate.ps1 -Action up -Service video   # 只迁移单个服务
```

**本机验证迁移脚本必须连隔离实例，禁止直连开发者本机 3306。** 默认参数取自服务 yaml
（`127.0.0.1:3306`，`root:root`），本机装了 MySQL80 服务时会直接命中真实实例（历史事故：`ERROR 1045`，
幸而凭据不符未写入）。因此本机/CI 验证一律显式覆盖到隔离实例（本项目为 `127.0.0.1:3399`，
数据目录 `.gotmp/mysql-data`，实例用 `mysqld --initialize-insecure` 自建）：

```powershell
./scripts/migrate.ps1 -Action up -Service video -OverrideHost 127.0.0.1 -OverridePort 3399 -OverrideUser root -OverridePassword root
./scripts/migrate.ps1 -Action status -OverrideHost 127.0.0.1 -OverridePort 3399 -OverrideUser root -OverridePassword root
```

连共享实例或生产时同理必须显式给出 host/port 与从 Secret/Vault 取出的账号，库名仍按服务解析：

```powershell
./scripts/migrate.ps1 -Action up -OverrideHost <host> -OverridePort <port> -OverrideUser migrator -OverridePassword '<from-secret>'
```

Windows CMD：

```cmd
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\migrate.ps1 -Action up
```

macOS/Linux 没有 PowerShell 时，按同一规则用 mysql 客户端执行（迁移文件本身是 `CREATE TABLE IF NOT EXISTS` 幂等脚本）。
下例把端口写死成隔离实例的 `3399`，不要替换回 3306：

```bash
for svc in $(ls deploy/migrations); do
  dsn=$(grep -hoE 'tcp\(127\.0\.0\.1:3306\)/[a-z_]+' services/"$svc"/etc/*.yaml 2>/dev/null | head -1 | cut -d/ -f2)
  [ -n "$dsn" ] || dsn="go_video_${svc//-/_}"
  echo "-- $svc -> $dsn"
  mysql --default-character-set=utf8mb4 -h127.0.0.1 -P3399 -u"$DB_USER" -p"$DB_PASSWORD" \
    -e "CREATE DATABASE IF NOT EXISTS \`$dsn\` CHARACTER SET utf8mb4"
  for f in $(ls deploy/migrations/"$svc"/*.sql | sort); do
    mysql --default-character-set=utf8mb4 -h127.0.0.1 -P3399 -u"$DB_USER" -p"$DB_PASSWORD" "$dsn" < "$f"
  done
done
```

迁移文件查看：

```powershell
Get-ChildItem deploy/migrations -Recurse -File -Filter *.sql
```

约束：

- 迁移文件必须 UTF-8；PowerShell 脚本自身保存为 **UTF-8 with BOM**，否则 Windows PowerShell 5.1 会按本地代码页解析中文注释并报语法错误。
- 迁移 SQL 里的中文表/列注释依赖 `--default-character-set=utf8mb4`；`migrate.ps1` 通过字节重定向喂给 mysql 客户端，避免 PowerShell 管道转码产生乱码。
  **手工重跑迁移（`mysql ... < file.sql`）时同样必须显式带这个参数**：不带时客户端按本地代码页（GBK）解释文件里的 UTF-8 字面量，
  `op_permission.description` 等中文列会写成乱码串，且 `CONCAT` 混用 gbk 与 utf8mb4 会直接报 `ERROR 1267 Illegal mix of collations`。
  已写坏的中文行不能靠再跑一次修好（`INSERT IGNORE` 会跳过），得先删掉那些行再按上面的方式重放。
- 只追加新文件，不修改**已提交或已在共享/真实实例应用过**的迁移；需要变更时新增 `NNNNNN_*.sql`。
  尚未提交、只在隔离实例验证过的文件可以就地修正（2026-09 商业化批次即属此类），但改完必须重新按下面
  两条在隔离实例复验，并在 `deploy/migrations/README.md` 记下列了哪些就地修正。
- 本机/CI 验证只能连隔离实例（`127.0.0.1:3399`）并显式传 `-Override*`；不带覆盖参数的命令会命中服务 yaml 里的 `3306`，那是开发者的真实数据库。
- 2026-09 批次已在隔离实例上把 `deploy/migrations` 下 23 个服务全部 `applied`、`status` 无 `pending`；新加迁移后按同样方式复验。
  2026-09-22 复验覆盖到 43 个目录（新增商业化五域 `membership`/`payment`/`trade-order`/`coin`/`creator-revenue`
  与 `operation` 的 `000004` 权限点种子），覆盖清单见 [deploy/migrations/README.md](../deploy/migrations/README.md)。
  同日把 `operation` 的 `000004`（就地补 39 个权限点）与新增的 `000006_seed_op_role_grants_stage12.sql`
  在隔离实例上重放到 162 权限点 / 31 角色 / 340 绑定、0 个权限点无角色可挂、重放第二遍计数不变。
- **数据种子迁移**有三个，都在 `deploy/migrations/operation/`：
  `000004_seed_op_permission.sql` 登记 `op_permission` 的 162 个权限点（29 个 domain），
  `000005_seed_op_role_grants.sql` 与 `000006_seed_op_role_grants_stage12.sql` 按 `domain`/`action`
  **派生**建默认角色与 `op_role_permission` 绑定
  （每轮都是 `domain_*` + `readonly` + `super_admin`；不写 `op_admin_role`，谁拿角色仍是人的决策）。
  000004 的内容必须与 `gateway/admin/internal/middleware` 的 `routePermissions` 去重集合逐字相等，
  改法是先改中间件表、再在种子文件对应域补/删一行，然后跑门禁：
  `go test -p 1 -count=1 -run TestSeed ./gateway/admin/internal/middleware/`。
  漏登记不会在编译期暴露，表现为该后台写入口对所有角色 403（中间件 fail-closed）。
  两条角色绑定的成员一律由 `SELECT` 现算，门禁是
  `go test -p 1 -count=1 -run 'RoleSeed|Readonly|RolePattern|Migration|Grants' ./services/operation/model/`
  （拦：空 `domain`、派生角色名不满足 `rolePattern`、硬编码 `permission_id`/`role_id`、
  绑定排在建角色之前、种子角色 `state≠1`、越界写 `op_permission`/`op_admin_*`、
  权限点种子编号跑到角色绑定迁移之后、新增角色种子文件却没加进门禁清单）。
  **新增权限点后不会自动并入既有角色**：`migrate.ps1` 跳过已记录版本，所以要在新的迁移里
  原样重复 000005 末尾那三段派生 `SELECT`（000006 就是这么来的；就地改 000004 也一样，
  须按本节第 8 条那条带 `--default-character-set=utf8mb4` 的 `mysql < file` 手工重放）。
- 禁止直接在生产数据库执行未评审 SQL；每个迁移必须记录 forward、rollback/补偿和锁风险。
- `account`、`creator`、`user-profile` 的库名沿用 `account`/`creator`/`userprofile`（见各自 etc yaml），其余服务为 `go_video_<service>`。

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

## 15. OpenSearch 索引结构与分词

索引结构（settings + mappings + 分词器）的唯一来源是 `services/search-indexer/internal/esclient`，
禁止在 shell 或 JSON 文件里另抄一份 mapping。需要查看/复用当前结构时现场导出：

```powershell
# 打到 stdout；-pretty 便于人工 diff，-out 写文件；
# -stopwords-path / -synonyms-path 传集群容器内的绝对路径，留空表示不启用该词典
go run ./services/search-indexer/cmd/esmapping -schema-version v1 -analyzer cjk -pretty
```

分词族由服务配置 `OpenSearch.Analyzer.Kind` 决定（`cjk` 为默认且无需插件；`ik`/`smartcn`
需要集群插件；`standard` 不做中文切分）。词典文件在 `deploy/opensearch/analysis`，compose
已只读挂载到容器 `/usr/share/opensearch/config/analysis`，取值与生效方式见
[deploy/opensearch/README.md](../deploy/opensearch/README.md)。

```powershell
docker compose -f deploy/docker-compose/docker-compose.yml up -d opensearch
# 一次性验证：健康 -> 插件实测 -> 建开发索引 -> _meta 校验 -> _analyze -> 样例文档 -> 冒烟查询
./scripts/es-init.ps1 -Endpoint http://127.0.0.1:9200 -Index go_video_content_dev_v1 -Analyzer cjk
```

`scripts/es-init.ps1` 只写显式命名的物理索引：目标已是别名、索引缺少 `mappings._meta`、
或 `_meta.analyzer` 与本次参数不一致都会直接终止（只有显式 `-Force` 才继续）；
它不创建、不删除也不切换任何别名。

```powershell
# 改结构或分词族前：先跑本服务单测（mapping 与投影结构、样例文档同源校验都在这里）
go test -count=1 ./services/search-indexer/...
```

结构变更前必须递增 `OpenSearch.SchemaVersion`，再走重建 + 切别名：管理面入口是
`POST /admin/search/rebuild`（提交重建）与 `POST /admin/search/alias/switch`
（`expected_current` 乐观校验后原子切换）。禁止原地修改已上线索引的 mapping。
