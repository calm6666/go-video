# catalog

版权作品目录服务，管理电影、电视剧、番剧、纪录片及其季/集结构、分区与标签。

- **拥有数据**：`catalog_work`（作品）、`catalog_season`（季）、`catalog_episode`（集）、`catalog_zone`（分区）、`catalog_tag`（标签）。
- **提供能力**：运营创建作品/季/集、上下架集、目录与分区/标签查询。
- **依赖**：`rights`（上架前版权窗口校验）、`asset`（建集/上架前媒资就绪校验）、MySQL、Redis。
- **约束**：
  - PGC 目录不等于普通 UGC 稿件；普通用户不能通过普通投稿接口发布受版权保护的整片（见 AGENTS.md §1、§5）。
  - 发布集必须通过合法状态机推进（草稿 → 上架 → 下架），回调不能直接写入终态；转换表见 `internal/logic/statemachine.go`。
  - 跨服务只通过 RPC 或版本化事件；不直连他人 MySQL/Redis，也不 import 对方内部 model。
  - 缓存：Work/Season/Episode 详情、分区树短 TTL 缓存；写操作失效对应缓存。
  - 不实现会员、订单、支付、投币、广告等商业化能力。

## 下游 RPC 与前置校验

`internal/logic/guard.go` 是唯一的跨服务前置校验入口，客户端契约定义在
`internal/repository/rpcclient.go`（接口 + 只读视图），适配器在
`rights_client.go` / `asset_client.go`。logic 层依赖接口而非 zrpc 连接，单元测试用 fake 注入。
数据访问同理经 `internal/repository`：`Repository` 的 6 个依赖（缓存 + 5 个 model）由
`NewWithDeps` 接收，生产路径只走 `New`（`NewWithDeps` 的存在唯一理由就是让 logic 单测能塞进内存替身）。

| 用例 | 下游调用 | 通过条件 |
|---|---|---|
| `CreateEpisode` | `asset.GetAsset(asset_id)` | 媒资存在且状态 ≥ `SCANNED`（已完成文件扫描与探测）且非 `FAILED` |
| `PublishEpisode` | `asset.GetAsset(ep.asset_id)` | 媒资状态为 `TRANSCODED`（AGENTS.md §8：上传完成不代表可播放） |
| `PublishEpisode` | `rights.CheckPlayable(content_id=epid, content_type=PGC, region)` | `playable=true`，即该地区存在 `state=active` 且 `start_time<=now<end_time` 的窗口 |

内容映射约定：`rights.Window.content_id = catalog_episode.epid`，`content_type = 1 (PGC)`。

`PublishEpisode` 入参 `EpisodeReq` 追加了向后兼容字段 `operator_mid`（上架审计）与
`region`（地区维度校验）；`GetEpisode`/`OfflineEpisode` 可不传。

### 校验失败语义（稳定业务错误）

| 错误 | 含义 |
|---|---|
| `catalog: invalid state transition` | 来源状态不允许流转到目标状态（含未知状态） |
| `catalog: region is required` | 请求未带 `region` 且未配置 `DefaultRegion`；地区未知不放行 |
| `catalog: rights window not granted` | rights 明确回答该地区无有效窗口（未生效/已过期/已撤权） |
| `catalog: rights checker unavailable` | rights 客户端未配置或调用失败（超时、下游 5xx） |
| `catalog: asset not found` | asset 确认 `asset_id` 不存在 |
| `catalog: asset not ready` | 媒资状态未达当前阶段要求或已 `FAILED` |
| `catalog: asset checker unavailable` | asset 客户端未配置或调用失败 |

**默认严格**：需要校验但校验器不可用时一律拒绝上架/建集。理由是版权内容误上架会导致
越权播放与合规事故，代价远高于一次误拒绝；rights/asset 恢复后运营重试即可。
`operator_mid` 缺失不阻塞流程，但按 Error 级日志记录，保证审计缺口可见。

### 配置

```yaml
RightsRPC:            # Key 必须等于下游服务的 Name
  Timeout: 2000
  Etcd: {Hosts: [127.0.0.1:2379], Key: rights.v1.rpc}
AssetRPC:
  Timeout: 2000
  Etcd: {Hosts: [127.0.0.1:2379], Key: asset.v1.rpc}
DefaultRegion: CN     # 请求未带 region 时的兜底地区；留空则必须显式传 region
DisableRightsCheck: false  # 灰度/回滚开关
DisableAssetCheck: false
```

- 两个 `Disable*Check` 开关只在显式置 `true` 时跳过对应校验，并打 `Error` 级日志，
  便于事后追溯“哪一次上架没有校验”。用于下游故障演练与紧急回滚，不作为常态配置。
- 客户端未配置（既无 `Target` 也无 `Etcd.Hosts`）时 `ServiceContext.Rights/Asset` 为 `nil`，
  行为等价于“校验器不可用”→ 拒绝，而不是静默放行。
- 业务缓存配置键是 `CacheRedis` 而不是 `Redis`：`zrpc.RpcServerConf` 内嵌了同名
  `RedisKeyConf` 字段，同名会让 go-zero 加载配置时报 `conflict key redis` 并让服务起不来。

### 灰度与回滚顺序

1. 先部署 rights/asset，再打开 catalog 的 `RightsRPC`/`AssetRPC` 配置（默认已启用校验）。
2. 灰度期观察 `catalog/guard` 日志：`版权窗口有效`/`媒资校验通过`/`precheck rejected`。
3. 回滚只需把 `DisableRightsCheck`/`DisableAssetCheck` 置 `true` 并热更配置，
   不改代码、不动数据；已上架的集不会因此被下架，需要回滚状态请走 `OfflineEpisode`。
4. 本服务不写 rights/asset 的表，因此没有跨库回滚脚本。

## 契约缺口（待下游或网关补齐）

- `rights.CheckPlayable` 只支持单内容单地区，且 `content_id` 粒度未在契约里固化
  （集 / 季 / 作品）；catalog 采用 `epid`。若需要按作品或按季批量授权，
  rights 侧要补 `ListWindowsByContents` 之类的批量接口和明确的 `content_id` 语义。
- rights/asset 用 `error`（而非结构化的 `found`/`reason` 字段）表达“不存在”，
  跨 gRPC 后只剩 `Unknown` + 文案，适配器只能按 `asset: not found` 文本识别；
  建议下游改用 `google.rpc.ErrorInfo` 或回复内的显式状态字段。
- `gateway/admin` 的上架入口目前只传 `epid`，未透传 `operator_mid`/`region`，
  审计字段会是 0 且地区依赖 `DefaultRegion` 兜底；网关接入后应逐次传入真实值。
- `PublishEpisode` 成功后投递 `content.published.v1` 事件、`OfflineEpisode` 失效
  CDN/搜索/推荐投影，仍待 Outbox 投递器与各下游落地（本服务不直接写他人投影）。

## 运行与测试

```powershell
go run ./services/catalog -f services/catalog/etc/catalog.v1.yaml
go test -mod=readonly -p 1 -count=1 ./services/catalog/...
```

- 契约变更后必须重新生成：`powershell -File scripts/gen.ps1 -Service catalog`
  （生成边界见 docs/commands.md §5/§14；`internal/server`、`rpc/*.pb.go` 禁止手改）。
- 单元测试覆盖 guard 的通过/拒绝/客户端未配置三条路径、状态机合法与非法迁移、
  配置解析与适配器归一化，全部不连接 MySQL/Redis/etcd。
- `internal/logic` 的 12 个 RPC 方法（写侧 `CreateWork`/`CreateSeason`/`CreateEpisode`/
  `PublishEpisode`/`OfflineEpisode` + 读侧 7 个）每个都有按方法用例，替身见 `fakes_test.go`。
  断言集中在三件事：守卫拒绝时一次依赖都不许碰、rights/asset 前置校验的**次序与结论**、
  副作用序列（写了几次状态、缓存失效有没有发生、读穿缓存有没有回填）。
- **覆盖边界（不要把它当成 SQL 已验证）**：替身只证明 logic 的判定与投影。`model` 包的
  5 张表 SQL（列名、`ORDER BY`、`LIMIT/OFFSET`、`RowsAffected` 语义）没有单测，仓库里也没有
  catalog 的迁移↔model 列级对账门禁；`deploy/migrations/catalog/*.sql` 与 `model` 的一致性
  目前只能靠人工比对，改表时按 docs/commands.md 的迁移流程在隔离实例复验。
- 两条容易被误改的行为已由用例钉住，改它们要连带改测试并说明理由：状态写入失败时
  **不**失效缓存（否则读侧拿到旧态的原因会被掩盖）；读路径 miss 后一律回填（包括随后被
  上架校验拒绝的那次读）。

- **部署合并**：第一阶段与 `video`、`rights` 合并为 `content` 部署单元，但保持包级领域边界与独立数据访问接口。

## 测试覆盖

离线单测（纯 Go 内存替身 + fake 下游客户端），不连接 MySQL、Redis、etcd、MQ、对象存储，也不建立真实
gRPC 连接。数字由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，
格式 `顶层/子用例`。合计 **61 顶层 / 6 子用例**（logic `55/5`、config `3/1`、repository `3/0`）；
本服务没有处于 `t.Skip` 状态的用例。上面「运行与测试」的描述与本节同源，逐文件分工见下表。

### 1. `internal/logic`（5 个文件：4 个用例文件 + `fakes_test.go` 替身层）— `55/5`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `write_logic_test.go` | 20 | 0 | 5 个写方法（`CreateWork`/`CreateSeason`/`CreateEpisode`/`PublishEpisode`/`OfflineEpisode`）每个都固定三类断言：入参守卫拒绝时**一次依赖都不许碰**（`callLog` 快照 + fake 下游计数钉死，防「先落库再校验」把脏数据写进 PGC 目录）；状态机与 rights/asset 前置校验的**次序与结论**（守卫跑在 asset 校验之前、`PublishEpisode` 必须 `TRANSCODED`、rights 明确拒绝就阻断写入）；副作用（写了几次状态、缓存失效有没有发生、返回值与入库行是否一致）。另钉住：已 `ONLINE` 的重复上架幂等、非法来源态、`OFFLINE→ONLINE` 重新过 rights、关掉 rights 开关只跳过 rights 不跳过 asset、UPDATE 失败时**不** DEL 缓存 |
| `read_logic_test.go` | 17 | 5 | 7 个读方法（`GetWork`/`GetEpisode`/`ListWorks`/`ListSeasons`/`ListEpisodes`/`ListTags`/`ListZones`）：非法 ID 与超限 `ps` 在触库前被拒；归一化后的过滤条件与分页参数**只能靠替身收到的入参**校验（返回值里看不出来）；缓存语义——miss 才查库并回填、hit 不再查库；「查无此行」映射业务 NotFound 而「读失败」必须原样上抛，两种结论不许互相冒充；`ListWorks` 的投影是 newest-first 且带 `total`，`ListZones` 走读穿缓存 |
| `guard_test.go` | 15 | 0 | `guard.go` 这条唯一跨服务前置校验入口的三分支：通过 / 明确拒绝 / **客户端未配置一律拒绝**（默认严格）；rights 下游错误映射成 `rights checker unavailable`；`DisableRightsCheck`/`DisableAssetCheck` 各自只跳过对应那一条、另一条照跑；`region` 取值优先级（请求优先于 `DefaultRegion`，两者都空则拒绝上架而不是猜地区）；媒资侧口径——建集允许 `SCANNED`、上架要求 `TRANSCODED`、`FAILED` 与 `UPLOADED` 都被拒、asset 确认 not-found 原样透传、空应答视为错误 |
| `statemachine_test.go` | 3 | 0 | `catalog_episode` 状态机的合法与非法迁移全表，外加「发布转换的目标状态必须是 `ONLINE`」这一条，钉住 §8 的「回调/运营接口不能绕过校验直接写上架态」 |
| `fakes_test.go` | 0 | 0 | 替身层：把 `repository.Repository` 的 6 个依赖（5 个 model + 缓存）换成内存实现。此前本包只有 `guard_test.go`（纯函数）与 `statemachine_test.go`（转换表），**没有任何一条用例构造过 `<X>Logic`**，于是「守卫有没有在触库前拦住」「校验不通过时有没有落库」「状态机是否真的生效」全都没有证据——这个文件就是让判定链可断言的那一层。三条纪律：读写都存/返回值拷贝（否则 logic 里 `e.State = EpStateOnline` 这类收尾赋值会反向污染库存行）、所有方法记进同一条有序 `callLog`、错误按方法名注入（一条用例里「读成功、写失败」是常见判定链，全局 err 会让测试因错误的原因通过） |

### 2. 其他层

- `internal/config`（2 个文件 `3/1`）：`config_test.go`（`2/0`）校验示例配置可被 go-zero 解析，
  并确认**「未配置即严格」的零值语义**（`Disable*Check` 缺省为 false、客户端缺省为不可用 ⇒ 拒绝）；
  `config_load_test.go`（`1/1`，子用例按 `etc/*.yaml` 文件名展开）反射递归检查必填字段，
  并守住业务缓存键必须叫 `CacheRedis` 而不是 `Redis`。
- `internal/repository`（1 个文件 `3/0`）：`rpcclient_test.go` 测两个适配器对下游契约的归一化——
  rights 的内容类型映射与判定结果投影、**空应答必须当错误**、asset 的「不存在」按
  `asset: not found` 文案识别并转换状态；通过内嵌生成的客户端接口只覆写用到的方法，不建真实连接。
- `model/`（`catalogmodel.go`、`errors.go`）**无离线单测**：5 张表的 SQL 文本、列名、`ORDER BY`、
  `LIMIT/OFFSET` 与 `RowsAffected` 语义都没有门禁。
- `internal/svc` **无离线单测**；`internal/server`、`rpc/*.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层：`content.published.v1` 投递与下架时的
  CDN/搜索/推荐投影都待 Outbox 投递器落地（见「契约缺口」），因此没有事件消费侧的用例。

### 3. 构造器级覆盖：**12/12**

探针取 `internal/logic` 全部 `New*Logic(`，共 12 个（写侧 5 + 读侧 7），`gaps:` 为空——
每个 RPC 方法都有直接驱动自身构造器的用例，不存在只有间接断言的方法。

### 4. 替身层与断言口径

- 被测路径是 `logic →` **真实 `Repository`** `→ 内存 model + 内存缓存`，跨服务校验走 `guard.go`
  的接口依赖（fake rights/asset 客户端）；所以「命中缓存还是回源、写库几次、失效有没有发生、
  下游校验排在守卫之前还是之后」都在被测路径上，而不是把 Repository 或 guard 一起 mock 掉。
- 断言口径：拒绝类用例断的是「零次依赖调用」（序列等式），不是「返回了错误」；
  写侧断「库里实际落了什么状态 + 应答与入库行是否一致」；fake 客户端的调用计数是第二重判别力。
  下游 fake 的返回类型刻意写成接口——直接把 `(*fakeAsset)(nil)` 传给接口参数会得到
  「非 nil 的接口装着 nil 指针」，`g.asset == nil` 判定就失效，用例会以 panic 而非预期错误失败。
- **它证明不了**：SQL 文本与列清单、`RowsAffected` 的 matched vs changed 差异、索引与唯一约束；
  rights/asset 的**真实 gRPC 报文**、超时与序列化——适配器用例只断到「入参原样交到接口看见的那个方法」，
  跨服务文案（`asset: not found`）在两侧是否仍一致，要靠契约测试而不是本包。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络；rights/asset 全部走 fake 客户端。
- PGC 目录只有**元数据与版权窗口判定**：本服务不持媒资内容、不签名播放地址，媒体字节既不进 MySQL
  也不进用例。`PublishEpisode` 的「媒资已 `TRANSCODED`」是对返回值做**离线判定**，
  不证明对象存储里真有产物、转码真的完成。
- **预签名链路不在本服务范围**（由 `playback` 负责签名，另有它自己的测试口径），本节不声称播放地址
  被验证过；`rights.CheckPlayable` 的窗口判定同理只断到适配器的归一化，真实窗口数据来自 rights 库。
- **回调链路只有离线判定**：`OfflineEpisode`/`PublishEpisode` 的状态写入与缓存失效被钉住，
  但 `content.published.v1` 的 Outbox 投递器未接入、下架投影（CDN/搜索/推荐）没有消费者，
  「上架真的对下游生效」在本包没有任何自动化证据（见「契约缺口」最后一条）。
- 迁移 SQL ↔ 真实库的列级对账：「运行与测试」只写了「改表时按 `docs/commands.md` 的迁移流程在隔离实例复验」，
  那是流程约定、不是执行结论；本 README 没有声明 `deploy/migrations/catalog/*.sql` 已在
  `127.0.0.1:3399` 完成列级复验，因此按**未在目标实例复验**处理，迁移登记以
  `deploy/migrations/README.md` 为权威。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内。

### 6. 验证命令

```bash
go test -mod=readonly -p 1 -count=1 ./services/catalog/...
gofmt -l services/catalog
go vet ./services/catalog/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下并发链接多个测试包会 OOM（`errno=1455`）；
  `-count=1` 关闭测试缓存，`-mod=readonly` 防止测试顺手改动 `go.mod`。
- `go vet` 期望无输出、`gofmt -l` 期望为空列表。门禁统一串行执行，本节只登记命令与口径，
  不在文档里声明执行结论。
