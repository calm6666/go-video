# RPC 接口冒烟脚本

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

契约真源：services/*/rpc/*.proto（43 份）。
覆盖 43 个服务、589 个 RPC 方法；先按消费面分大节，节内再按服务分节。

状态：本脚本**从未在本仓执行过**——这里没有运行中的服务进程、没有 etcd，
维护者机器上的 MySQL 也不允许被自动化触碰。它只是把「每个方法怎么调」固化成
可复用入口；请求体是 proto 字段骨架，**不含业务前置数据**，绝大多数调用会因
记录不存在/参数校验失败而返回错误，这属于预期。不要把它当成通过/失败断言。

前置：`grpcurl` 在 PATH（或 GRPCURL 环境变量指到可执行文件），目标服务已启动，
并且用 `-d` 里的占位值换成真实存在的主键才能观察到成功分支。
脚本靠 reflection 解析方法名：服务的 services/*/*.v1.go 只在 Mode 为 dev/test 时
reflection.Register（示例 etc/*.yaml 都是 Mode: dev，本地默认可用）。
若目标进程是 Mode: pro，反射不会注册，需要改成
grpcurl -import-path services/<svc>/rpc -proto <svc>.proto 的显式模式。

| 脚本 | 说明 |
|---|---|
| `smoke.sh` | Bash + grpcurl（plaintext，走 reflection；`Mode: pro` 的进程要改加 `-import-path -proto`） |
| `smoke.ps1` | PowerShell 等价实现，Windows 本地开发用 |

覆盖：43 个服务 / 589 个方法。
契约里没有任何流式方法（proto 无 `stream`），所以每条都是单次调用。

## 分节口径（先按消费面，再按服务）

一个大节以 `# @@ 消费面：… @@` 横幅开始，节内每个服务一个 `----------` 小节日。
归属不是手工归类，而是读两个网关的 `etc/*.yaml` 与 `internal/config/*.go`：
看哪个网关的哪个 RPC 配置字段引用了这个服务的注册 etcd key。

| 大节 | 服务 | 方法 | 服务清单 |
|---|---|---|---|
| 终端面与运营面都消费 | 18 | 245 | `account`、`catalog`、`coin`、`comment`、`creator`、`creator-revenue`、`danmaku`、`inbox`、`live-room`、`membership`、`moderation-orchestrator`、`notification`、`payment`、`private-message`、`trade-order`、`transcode`、`user-profile`、`video` |
| 只有终端面消费 | 6 | 55 | `engagement`、`feed`、`playback`、`search-query`、`social-graph`、`upload` |
| 只有运营面消费 | 17 | 278 | `asset`、`audit`、`cron`、`event-collector`、`feature-store`、`live-gateway`、`live-ingest`、`live-media`、`open-platform`、`operation`、`ops-config`、`recommend-rank`、`recommend-recall`、`rights`、`risk-control`、`search-indexer`、`spm` |
| 两个网关都不引用（服务间 / worker / 尚未接线） | 2 | 11 | `content-fingerprint`、`moderation-worker` |

只跑一个大节：按横幅切出来再执行（横幅用 `@@` 包起来，不会被 proto 注释里的 `====` 误匹配）

```bash
awk '/^# @@ 消费面：/{p=0} /^# @@ 消费面：.*只有运营面消费/{p=1} p' scripts/rpc/smoke.sh | bash
```

逐方法的字段定义见 [docs/api/rpc/](../../docs/api/rpc/README.md)（同一套分节口径）。

## 与单测的分工

- Go 侧单测（`services/*/internal/logic/*_test.go`）用替身驱动，是 CI 门禁的一部分，能真的失败；
- 本脚本是**联调工具**，需要真实进程与 etcd，输出只作观察记录；
- 两者都跑过之前，任何 README 都不应写「RPC 已实机联调」。
