# rights

版权权利合同与播放窗口服务。本期只实现合同/窗口 CRUD、可播放性校验和过期推进，
不实现会员、订单、支付、投币等商业化能力。

- **拥有数据**：`rights_contract`（合同：版权方/标题/签订/生效/到期/地区）、
  `rights_window`（时间窗口：关联合同/内容/地区/起止时间/状态）。
- **提供能力**：合同与窗口的创建/查询，按 `content_id+region` 校验可播放性，
  手动过期窗口，查询即将过期窗口供 cron 推进。
- **依赖**：MySQL、Redis。不依赖其他服务 RPC；被动供 `catalog`、`playback` 调用。
- **与 catalog 关系**：`catalog` 创建版权条目后，通过本服务为 `content_id` 创建播放窗口。
- **与 playback 关系**：`playback` 在签发播放地址前必须调用 `CheckPlayable` 二次校验，
  过期/撤权窗口不再返回可播放结果。
- **与 cron/operation 关系**：`cron` 调用 `ListExpiring` 拉取即将过期窗口，
  通过 `ExpireWindow` 推进状态；运营也可手动调用 `ExpireWindow` 撤权。
- **约束**：发布任务和播放鉴权都必须再次检查窗口，不能只依赖一次发布时校验；
  `CheckPlayable` 结果短缓存（默认 120s），窗口过期/撤权时强制失效。

## RPC 方法

| 方法 | 说明 |
| --- | --- |
| `CreateContract` | 运营创建合同 |
| `GetContract` | 查询单个合同 |
| `ListContracts` | 分页查询合同 |
| `CreateWindow` | 为内容创建时间窗口（关联合同） |
| `GetWindow` | 查询单个窗口 |
| `ListWindows` | 按 content_id 或 contract_id 查询窗口 |
| `CheckPlayable` | 校验内容在某地区是否可播放 |
| `ExpireWindow` | 手动过期窗口（运营/cron） |
| `ListExpiring` | 查询即将过期的窗口（cron 用） |

## 运行与测试

```powershell
go run ./services/rights -f services/rights/etc/rights.v1.yaml
go test -mod=readonly -p 1 ./services/rights/...
```

- 契约变更后必须重新生成：`powershell -File scripts/gen.ps1 -Service rights`
  （生成边界见 docs/commands.md §5/§14；`internal/server`、`rpc/*.pb.go` 禁止手改）。
- `internal/logic` 的 9 个 RPC 方法每个都有按方法用例（`checkplayable_test.go` 覆盖判定与过期链，
  `crud_logic_test.go` 覆盖合同/窗口的创建与读侧），全部不连接 MySQL/Redis/etcd。
  逐文件清单、替身口径与覆盖边界见下面「测试覆盖」节。
- 已知缺口（用例只钉住现状，不代表设计正确）：`CreateWindow` 与 `CheckPlayable` 都不校验
  `content_type` 枚举，`CONTENT_TYPE_UNSPECIFIED(0)` 既能建窗口也能被同类型判定命中，
  形成一条与 PGC/UGC 平行的暗道；`ListContracts`/`ListWindows` 的 `ps` 由 logic 拒绝、
  `model` 里还有一份「超限回落默认值」的兜底，经 logic 入口走不到那条分支。

## 测试覆盖

离线单测（不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`（已排除 `TestMain`）
与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，3 个文件 `43/0`）

按「判定链」与「CRUD」两份文件分组，替身文件单列：

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| `checkplayable_test.go` | 16 | 0 | `CheckPlayable` + `ExpireWindow` 两条链，用例一律从 logic 入口进入、以替身轨迹与缓存内容作断言（真正的判定链在 `repository.CheckPlayable`：缓存 → DB → 时间比较 → 状态推进）。钉住误播/误拒两个代价最高的方向：不可播是**结论**（`playable=false, err=nil`）而不是错误，DB 故障必须是错误、不能被吞成 false（否则上游 catalog/playback 把「下游挂了」当成「没版权」）；缓存 key 必须含 `content_type` 与 `region`（跨地区授权互不覆盖）；只有「已过期」窗口才允许被推进为 expired，未生效与已撤权都不改状态；命中缓存（含负缓存）时一次 DB 都不碰，缓存读故障降级回源而不是报错 |
| `crud_logic_test.go` | 27 | 0 | 合同/窗口 7 个方法（CreateContract/GetContract/ListContracts/CreateWindow/GetWindow/ListWindows/ListExpiring）。钉住：守卫一律发生在触库之前（参数非法不许先查一次库再拒——写放大 + 掩盖真实原因）；窗口创建必须先过「关联合同生效中」，合同校验失败时**一行都不落**（孤儿窗口会让 CheckPlayable 在无合同支撑下判可播）；写成功才动缓存（插入失败不得失效 `rights:chk:*` 也不得推进状态，插入成功必须失效旧结论否则新窗口要等 TTL）；读错误不得被当成 NotFound；列表过滤组合是 AND、`state` 未指定含全部状态、`ListExpiring` 只选 ACTIVE 且按到期时间升序且空结果不是错误；`content_type` 未指定（枚举 0）当前也能建窗口并被同类型判定命中（钉住现状，见上一节缺口） |
| `fakes_test.go` | 0 | 0 | 替身集合：用 `repository.NewWithDeps` 组装**真实 Repository**，见第 4 组 |

### 2. 其他层

- `internal/repository`（1 文件 `4/0`）：`cachecodec_test.go` 钉 `CheckPlayable` 缓存的**线格式**
  ——这是 Redis 里跨进程读写的唯一形状（logic 只看见 bool/int64），解析错位会把「有版权」读成「没版权」
  且不报错：key 分隔 contentType/region（含 region 带冒号的形状）、编解码往返、
  非法值一律降级为「miss」（`ok=false, err=nil`）让调用方回源，既不当成不可播也不报错；
  playable 字节未知时 fail-closed。
- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用 `conf.Load` 真实加载 `etc/` 下示例 yaml。
- `model/`（`rightsmodel.go`/`errors.go`/`now.go`）**无离线单测**：列名、占位符、`LIMIT/OFFSET`
  分页与状态门槛的 SQL 文本都只能由迁移与集成环境证明。
- `internal/svc/`：**无离线单测**（装配点被 logic 用例经 `NewWithDeps` 间接经过）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录，也没有 HTTP handler（只暴露 gRPC）。

### 3. 构造器级覆盖

`9/9`：探针取 `internal/logic` 全部 `New*Logic(`（9 个，与 RPC 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空。

### 4. 替身层与断言口径

- 为什么要注入缝：`ServiceContext.Repository` 是具体类型 `*repository.Repository`，
  生产构造走 `repository.New`（真 Redis + 真 MySQL），测试无处塞替身。因此本包用例统一用
  `repository.NewWithDeps(内存缓存, 内存 contractMd, 内存 windowMd)` 组装**真实的 Repository**，
  只把它的 3 个依赖换成替身——判定链（缓存/DB/状态推进）整条都在被测路径上，而不是把 Repository 也 mock 掉；
  生产路径仍然只走 `New`。
- 四条替身纪律：① 每次读都返回值拷贝（否则「有没有真的调用 UpdateState」会被共享指针掩盖）；
  ② 写入按真实 SQL 的口径处理主键（`Insert` 忽略入参 ID、自增分配并返回，与 model 的
  INSERT 列表不含主键 + `LastInsertId` 一致）；③ 副作用按**顺序**记录（callLog），断言序列而不是只断次数
  （rights 最要紧的结论是「先查谁、回填了什么、有没有推进状态」）；④ 错误注入按方法粒度
  （`faultInjector.failWith("FindActive...", err)`），不用全局 err。
- 它证明不了什么：替身只复刻 model 层 SQL 的**语义**（`state` 过滤、`ORDER BY` 方向、`RowsAffected`），
  不证明 SQL 本身，**也不做 `LIMIT/OFFSET` 切片**——所以 `pn/ps` 的断言是**透传值**而不是切片结果；
  真实索引命中、列宽、驱动返回的 matched vs changed rows、Redis 真实 TTL 与到期都在离线覆盖之外。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端。
- **0 条用例处于 skip**（主代理实测口径）；不要把用例文件数当成断言都在跑。
- 本服务**没有** `model/migration_parity_test.go`，因此不存在「迁移 SQL ↔ 真实库列级对账」这道门禁；
  `deploy/migrations/README.md` 的「当前覆盖」把 `rights | go_video_rights | 1 | applied` 登记为在隔离实例
  `127.0.0.1:3399` 跑通 `up` + `status`，但那是迁移可执行性、不是列级对账；真实/共享实例未执行，
  本 README 没有记录更细的复验结论，因此不得按「已验证」处理。改表时按 docs/commands.md 的迁移流程复验。
- `internal/server/rightsserver.go`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内。

### 6. 验证命令

```sh
go test -p 1 -count=1 ./services/rights/...      # 上面「运行与测试」的 -mod=readonly -p 1 同义
gofmt -l services/rights                          # 必须为空
go vet ./services/rights/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

