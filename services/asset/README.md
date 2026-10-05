# asset

媒资元数据与生命周期服务。拥有原文件、封面、字幕、截图的**元数据**，向 upload / transcode / video / catalog
提供登记、查询、状态推进能力。纯 gRPC 服务，无 HTTP 端口。

## 职责

- 登记媒资：上传完成后由 upload 链路调用 `RegisterAsset` 建 `asset_meta`，关联 `upload_id` 与对象存储引用，初始状态 `UPLOADED`。
- 查询媒资：`GetAsset`（详情，走 Redis 读穿）、`ListAssets`（分页，直连 DB）、`ListCovers`、`ListSubtitles`。
- 回写探测结果：transcode 完成后调用 `UpdateAssetMeta` 写 `duration/width/height/codec`。
- 推进媒资状态机：`TransitionState`，`UPLOADED → SCANNED → TRANSCODED`，任一非终态可 `→ FAILED`。
- 登记投递产物：封面 / 字幕 / 截图。

不做的事：不持有文件内容、不派发转码任务（本期不走 MQ，由 video 直调 transcode）、不写稿件发布状态、
不生成对象存储签名 URL。

## 数据所有权边界

| 拥有的表 | 迁移 | 主键 |
| --- | --- | --- |
| `asset_meta` | `deploy/migrations/asset/000001_create_asset_meta.sql:7` | `asset_id` |
| `asset_cover` | `deploy/migrations/asset/000002_create_asset_attachment_tables.sql:7` | `cover_id` |
| `asset_subtitle` | 同上 `:22` | `sub_id` |
| `asset_screenshot` | 同上 `:35` | `shot_id` |

- **只存引用不存内容**：四张表都只有 `bucket` + `object_key`（`000001:11-12`、`000002:10-11/26-27/38-39`），
  大文件永不入 MySQL。
- **不持有稿件状态**：`asset_meta` 的列清单里没有 `aid`/`bvid`，也没有指向 `video` 服务的外键
  （`000001:8-21`）。媒资状态与稿件发布状态分离（AGENTS.md §8）。
- **不返回长期 URL**：`AssetReply` 等应答只带 `bucket`/`object_key`，供客户端换取 playback 短期签名；
  本服务代码里不存在任何签名/预签名/过期时间逻辑（已按字段名反射钉住，见「测试口径」）。
- **不依赖下游 RPC**：`internal/svc/servicecontext.go` 只建 Redis 与 MySQL 连接，没有 zrpc client。
  反向依赖是 catalog 调 `GetAsset` 读状态（`services/catalog/internal/repository/asset_client.go`）。
- **读放大由 Redis 兜**：只缓存 Asset 详情，TTL 60 秒；列表与封面/字幕列表不缓存
  （`services/asset/internal/repository/repository.go:26-29`、`:172-175`）。

## gRPC API

`services/asset/rpc/asset.proto:161-185` 声明 10 个 rpc，按域分四组（实现与线上一致，注册点在
`services/asset/internal/server/assetserver.go`）：

**1. 媒资本体（登记 / 读取 / 回写）**

| rpc | 语义 | 关键入参校验 |
| --- | --- | --- |
| `RegisterAsset` | 建 `asset_meta`，返回 `asset_id`，状态 `UPLOADED` | `upload_id>0`、`mid>0`、`bucket`、`object_key`（`registerassetlogic.go:32-43`） |
| `GetAsset` | 单条详情，缓存优先 | `asset_id>0`（`getassetlogic.go:29`） |
| `ListAssets` | 分页 + `mid`/`state` 过滤，返回 `total` | `ps ∈ [1,50]`，越界直接拒（`listassetslogic.go:32-34`） |
| `UpdateAssetMeta` | transcode 回写探测结果并失效缓存 | `asset_id>0`（`updateassetmetalogic.go:29`） |

**2. 生命周期状态机**

| rpc | 语义 |
| --- | --- |
| `TransitionState` | 先读当前状态（可能命中缓存，`transitionstatelogic.go:36`），再按 `model.CanTransition` 判定推进（`repository.go:188`） |

**3. 附件：封面与字幕**（读写成对）

| rpc | 语义 | 关键入参校验 |
| --- | --- | --- |
| `AddCover` / `ListCovers` | 封面登记 / 按 `asset_id` 列举 | `asset_id>0`、`bucket`、`object_key`（`addcoverlogic.go:31-39`） |
| `AddSubtitle` / `ListSubtitles` | 字幕登记 / 按 `asset_id` 列举 | 额外要求 `lang` 非空（`addsubtitlelogic.go:33-35`） |

**4. 截图（只写）**

| rpc | 语义 |
| --- | --- |
| `AddScreenshot` | 登记某画面时间点的截图；`timestamp >= 0`（`addscreenshotlogic.go:39-41`）。无对应读接口，见缺口 9 |

## 数据模型与迁移

- 迁移：`deploy/migrations/asset/000001_create_asset_meta.sql`、`000002_create_asset_attachment_tables.sql`。
  表引擎 InnoDB、`utf8mb4`，`asset_id/cover_id/sub_id/shot_id` 为 `AUTO_INCREMENT`。
- 索引：`asset_meta` 上 `idx_mid_ctime`、`idx_state_ctime`、`idx_md5`、`idx_upload`（`000001:23-26`，
  其中 `idx_upload` 与 `idx_md5` 都是**非唯一** KEY）；`asset_cover.idx_asset`（`000002:16`）；
  `asset_subtitle` 上唯一键 `uniq_asset_lang (asset_id, lang)`（`000002:30`）；
  `asset_screenshot.idx_asset_timestamp`（`000002:43`）。
- 结构体与 SQL 在 `services/asset/model/assetmodel.go`（一个文件承载四个 model，接口 + `default*Model`）。
  列表 SQL 的排序：`asset_id DESC`（`:117`）、`cover_id ASC`（`:206`）、`sub_id ASC`（`:260`）。
- 状态常量与迁移表：`model/errors.go:23-29`（`UPLOADED=1`/`SCANNED=2`/`TRANSCODED=3`/`FAILED=4`）、
  `model/errors.go:33-38`（`validTransitions`，`FAILED` 是终态）。业务错误 sentinel 集中在 `model/errors.go:6-18`。
- 缓存键：`asset:a:<asset_id>`，TTL 60 秒（`internal/repository/repository.go:27-28`）。
- 无 `aid`/`bvid`，无软删列，无版本号列。

## 目录结构

```
services/asset/
├── asset.v1.go                 # 入口：-f etc/asset.v1.yaml，注册 AssetServer
├── etc/asset.v1.yaml           # 监听 0.0.0.0:8099，etcd 注册键 asset.v1.rpc
├── internal/config/            # Config：RpcServerConf + CacheRedis + DataSource
├── internal/svc/               # ServiceContext{Config, Repository}
├── internal/repository/        # Cache + Cacher + Repository（4 个 model 的统一入口）
├── internal/logic/             # 10 个 rpc 的 logic + helpers.go 投影
├── internal/server/            # goctl 生成，只读
├── model/                      # assetmodel.go（4 表）+ errors.go（sentinel/状态机）
└── rpc/                        # asset.proto + 生成产物，只读
```

## 运行方式

```bash
# 从仓库根目录
go run ./services/asset -f services/asset/etc/asset.v1.yaml
```

- 端口 `8099`，etcd 服务名 `asset.v1.rpc`（`etc/asset.v1.yaml:2,6`）。
- 配置字段：`CacheRedis`（不能叫 `Redis`，会与 `zrpc.RpcServerConf` 内嵌的 `RedisKeyConf` 冲突，
  `internal/config/config.go:15`）、`DataSource`（库名 `go_video_asset`，`etc/asset.v1.yaml:14`）。
- 纯 gRPC，无 HTTP 探活；Dev/Test 模式下注册 gRPC reflection（`asset.v1.go:31-33`），可用 `grpcurl` 验证。
- 生成（改 `rpc/asset.proto` 后，按 `docs/commands.md` §5 的约定在 `services/asset/rpc` 目录内执行
  `goctl rpc protoc asset.proto --go_out=. --go-grpc_out=. --go_opt=paths=source_relative
  --go-grpc_opt=paths=source_relative --zrpc_out=.. --module (go list -m)`）。
  `internal/server/`、`rpc/*.pb.go`、`rpc/*_grpc.pb.go` 是生成物，只读；带
  `// Code scaffolded by goctl. Safe to edit.` 标记的只有 `internal/config/config.go:1` 与
  `internal/svc/servicecontext.go:1`，`internal/logic/*logic.go` 全部手写。

## 测试口径

本节写**装配缝与替身纪律**；逐文件的用例清单、分层覆盖与验证命令见下面的「测试覆盖」节，两节数字同源。

本包覆盖 10 个 rpc 的**全部 logic 构造器**（10/10）：
读侧 `GetAsset`、`ListAssets`、`ListCovers`、`ListSubtitles`，
写侧 `RegisterAsset`、`UpdateAssetMeta`、`TransitionState`、`AddCover`、`AddSubtitle`、`AddScreenshot`。
`internal/logic` 是 **76 个顶层用例 / 18 个 `t.Run` 子用例**，0 条 `t.Skip`；
`go test -p 1 -count=1 ./services/asset/...` 还会带上 `internal/config` 的 1/1，合计 77 / 19。
全部内存替身，不连 MySQL、Redis、对象存储。

写侧文件与分工：`registerasset_test.go`、`updateassetmeta_test.go`、`transitionstate_test.go`、
`addcover_test.go`、`addsubtitle_test.go`、`addscreenshot_test.go`；
跨文件共用的替身能力（`snapshotByPK`/`row`/`rowByLang`、`shotLine`、`wantUnixWindow`、
`op*` 轨迹词条）都在 `fakes_test.go`，装配缝没有再动，生产代码一行没改。

```bash
go test -p 1 -count=1 ./services/asset/...
```

（Windows 下必须带 `-p 1`，否则链接器报 `errno=1455`。）

### 注入缝

唯一改到的生产文件是 `internal/repository/repository.go`，加了三处、没动任何判定：

1. `Cacher` 接口（`:75-81`）+ `var _ Cacher = (*Cache)(nil)`（`:83`）。只声明 `Repository` 真正调用的
   4 个方法，`*Cache` 原样满足它——生产实现换的只是字段的静态类型（`:89` `cache *Cache` → `cache Cacher`）。
2. `NewWithDeps(cache, metaMd, coverMd, subMd, shotMd)`（`:115-127`），显式注入 5 个依赖。
3. `New(rds, conn)` 改为委托 `NewWithDeps`（`:98-108`），参数顺序、model 构造、`repo.conn = conn`
   与加缝之前逐字等价。

**被替代的是 Redis 客户端与 DB 连接，不是 Repository**：测试注入的仍是真实 `Repository`，
所以「详情读穿回填、回写后失效、列表不缓存」这条链路整体在被测路径上，而不是被 mock 掉。
`NewWithDeps` 故意不收 `conn`——`Repository.conn` 只在 `:106` 被写、没有任何读点，理由写在 `:112-114`。

### 替身纪律（`internal/logic/fakes_test.go`）

- 读接口一律**值拷贝**，测试改返回值不会影响仓储内部状态。
- 主键/唯一键语义镜像真实 SQL：`asset_id` 自增、`(asset_id, lang)` 冲突返回带 `1062` 的
  `*mysql.SQLError` 文本、`UpdateMeta`/`UpdateState` 在**按主键匹配不到行**时返回裸
  `model.ErrAssetNotFound`（生产 SQL 判的是 `RowsAffected == 0`，而驱动默认上报**改变**的行数，
  这条差异替身不建模，见缺口 17）。
- 每次调用记一条有序痕迹（`<pkg>.<Method>:<key>`），种子走静默路径，因此断言从第 0 条开始。
- 轨迹词条由 `fakes_test.go` 的 `op*` 构造器**单边定义**：替身落轨迹用它们，写侧用例拼期望也用它们，
  `TestTrajectoryVocabularyMatchesFakes` 再把替身实际落下的 15 条词条与构造器逐字对账一次
  （读侧旧用例保留内联字面量，构成第二重交叉校验）。
- 分方法故障注入：`failWith("FindOne", errBoom)`，用于钉「原样上抛 / 被哨兵替换 / 只写日志后吞掉」三类。
- **排序期望不手抄字面量**：`ORDER BY` 列名从 `model/assetmodel.go` 正则抽出（`modelsql_test.go:49`），
  期望序列由 `orderIDs(t, clause, ids)` 用注册过的比较器算出（`fakes_test.go` `clauseLess`）。
  模型层改了排序方向而比较器没跟着改，抽取门禁立刻红。

### 钉住的行为结论（摘）

- `GetAsset`：`asset_id<=0` 零依赖调用；缓存命中**不会**回查 DB，因此会返回最陈旧 60 秒的探测结果；
  命中时**不校验** payload 里的 `asset_id` 与请求键是否一致；缓存 JSON 损坏直接失败（不降级读 DB、
  脏 key 保留 → 不自愈）；空串按 miss 处理；miss 才 `FindOne` 并回填整行；缺行返回裸
  `model.ErrAssetNotFound` 且**不写负缓存**；Redis 读错误原样上抛；回填写失败被 `_ =` 吞掉但请求仍成功；
  `state` 是裸数值强转（`helpers.go:26`），库里脏值 7/-1 会原样出现在应答里。
- `ListAssets`：`ps` 边界 50 放行 / 51 拒绝；`pn<1` 的归一化发生在 `ps` 校验**之前**（被拒的请求 `in.Pn` 也已被就地改写）；
  `mid`/`state` 只在 `>0` 时进 WHERE，所以 `state=7` 是等值过滤（结果为空）而 `state=-1` 等于不过滤；
  `mid` 是可选过滤、不是归属校验；`total==0` 时不发第二条 SELECT；COUNT 与取行的失败都原样上抛。
- `ListCovers`/`ListSubtitles`：按主键升序、`lang` 跟随主键；不校验 `asset_id` 是否存在（未知媒资返回空列表 + `nil`），
  这是 `addcoverlogic.go:29` 明文声明的现状，用例把它钉成契约而不是编一个「应当报错」的期望。
- 应答面：反射遍历 7 个 reply 类型，字段名不得含 `url/uri/sign/secret/token/expire/accesskey/cdn/endpoint/host`；
  同时断言 `bucket`/`object_key` **确实**暴露，避免用「全空」蒙过断言。
- 跨服务文案契约：`ErrAssetNotFound.Error() == "asset: not found"`，catalog 靠这段字面量识别不存在
  （`services/catalog/internal/repository/asset_client.go:21`）。

**写侧（本轮新增）**

- `RegisterAsset`：守卫顺序 `upload_id → mid → bucket → object_key`，四条同时坏只报第一条，被拒时
  **一次依赖都不碰**；主键由 INSERT 分配（入参没有 asset_id），初始 `state=UPLOADED`、`ctime==mtime`、
  探测四列留空。登记路径是**预热**缓存而不是失效（`repository.go:144`），且缓存写失败被 `_ =` 吞掉、
  请求仍成功。没有幂等键：同 `upload_id` 重放得到两个 asset_id、两行、两条各自独立的缓存（缺口 2）。
  `size`/`md5`/空白引用列一律不校验（缺口 15）。
- `TransitionState`：按 `model.validTransitions` 列全 5 条合法边与 14 条非法边。合法边的依赖序列
  恰好 6 条：读缓存→查库→回填→`UPDATE state`→DEL→回读；非法边只走前三条，`UPDATE` 一次都不发、
  库存 `state`/`mtime` 原封不动，返回**未包装**的 `ErrInvalidTransition`。`to_state` 只判
  `== UNSPECIFIED`，所以 7/-1 是「非法迁移」而不是「非法状态」。`from` 取自可能陈旧 60 秒的缓存
  且 UPDATE 无 CAS：库里已 `TRANSCODED` 时能被缓存里的 `UPLOADED` 合法化成 `SCANNED`，
  **状态被回退一格且应答自洽**（缺口 3）。
- `UpdateAssetMeta`：不读缓存，三步 `UPDATE → DEL → 回读`；可改列面只有 `duration/width/height/codec`
  加 model 层刷新的 `mtime`，状态/归属/引用/ctime 一律不动。空请求把已探测结果**抹成零值**、
  负值与 `FAILED` 终态一律放行（缺口 14）。缺行返回裸 `ErrAssetNotFound` 且**不 DEL 缓存**；
  `UPDATE` 失败或 `DEL` 失败都让陈旧缓存继续服务，后者连日志都没有（缺口 13）。
- `AddCover`/`AddSubtitle`/`AddScreenshot`：归属键是**入参 `asset_id`**（字幕再加 `lang`），
  附件主键一律由 INSERT 分配；三条路径的依赖序列都**只有一条 INSERT**——
  存在性/归属前置检查根本不存在（缺口 6），也不碰详情缓存。重复插入行为各不相同：
  `asset_cover`/`asset_screenshot` 无唯一键 ⇒ 追加新行（缺口 16）；`asset_subtitle` 撞
  `uniq_asset_lang` ⇒ 裸 1062 文本上抛，既不翻译成本域哨兵、也不覆盖原行（缺口 8）。
  守卫覆盖面也各有洞：`width/height` 不校验、`lang` 只判非空（纯空格合法、`zh_CN` 与 `zh-CN` 两行）、
  `timestamp` 只判 `<0` 且不判上界。
- 契约门禁：`rpc.AssetServer` 的导出方法集恰好是那 10 个，截图**没有**任何读 rpc（缺口 9 的前提）。

### 覆盖边界（如实声明）

- 写侧 6 个方法**已有用例**（本包 76 个顶层用例中，写侧 6 个方法与轨迹对账共 42 个），
  但结论只到「一次调用打了哪几条依赖、按什么顺序、失败后内存库存/缓存留下什么形态」这一层：
  不证明 SQL 文本、列清单、唯一键与非唯一 KEY 约束本身，
  那部分靠五处源码/契约门禁（`modelsql_test.go` 三条 + `TestTransitionStateUpdateSQLHasNoStateGuard` +
  `TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally`，另加 `TestScreenshotIsWriteOnlyInServiceContract`
  这条 rpc 契约门禁与 `TestTrajectoryVocabularyMatchesFakes` 这条词条对账）。
- 「同一秒内重放同一份探测结果会被驱动判成未找到」（`RowsAffected` 上报改变行数）在内存替身里
  **测不到**：替身按匹配行数建模（见缺口 17）。
- `lang` 大小写变体是否撞唯一键取决于实例排序规则（utf8mb4 默认 `*_ai_ci`），替身按字节比较，
  所以本包只锁「同字节串冲突」这一半（见缺口 8）。
- 缓存键格式、TTL 数值、`ORDER BY` 文本只有源码门禁（`modelsql_test.go` 三个用例），没有行为门禁——
  生产路径的 `*Cache` 在测试里被替身换掉，真实 `SETEX`/`GET`/`DEL` 不在被测路径上。
- 本轮**没有**做过变异测试（改坏生产代码验证用例是否变红）：该类探测被工具策略拦下。
  用例的判别力来自边界对照（合法 5 边 vs 非法 14 边、命中 vs miss、`to_state` 0 vs 7、
  有键冲突 vs 无键追加）、前提断言（布景顺序必须与期望顺序不同，否则 `t.Fatalf`）
  与「守卫后零依赖调用」的序列等式，不来自「已知会红」的破坏实验。

## 测试覆盖

离线单测（纯 Go 内存替身），不连 MySQL、Redis、对象存储、MQ。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。合计 **77 顶层 / 19 子用例**
（logic `76/18` + config `1/1`），本服务 0 条 `t.Skip`。装配缝与替身纪律的完整口径在上一节「测试口径」，
两节数字同源、不另立说法。

### 1. `internal/logic`（11 个文件：10 个用例文件 + `fakes_test.go` 替身层）— `76/18`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `getasset_test.go` | 13 | 2 | 缓存命中**不回查库**（因而会返回最陈旧 60 秒的探测结果），且命中时不校验 payload 里的 `asset_id` 与请求键一致；脏 payload fail-closed（不回源、脏键保留 ⇒ 不自愈），空串按 miss；miss 才 `FindOne` 并回填整行；缺行返回**裸** `model.ErrAssetNotFound` 且不写负缓存；`"asset: not found"` 这段文案是 catalog 依赖的线上契约；`state` 是裸数值强转（库里 7/-1 原样出现在应答里）；反射遍历 7 个 reply 类型断字段名不含 `url/uri/sign/secret/token/expire/accesskey/cdn/endpoint/host`，同时断 `bucket`/`object_key` **确实**透出（不许用「全空」蒙过断言） |
| `listassets_test.go` | 10 | 4 | `ps` 边界 50 放行 / 51 拒绝；`pn<1` 的归一化发生在 `ps` 校验**之前**（被拒请求的 `in.Pn` 也已被就地改写）；`mid`/`state` 只在 `>0` 时进 WHERE ⇒ `state=7` 是等值过滤（结果为空）而 `state=-1` 等于不过滤；`mid` 是可选过滤而不是归属校验；排序以 model 的 `ORDER BY` 为唯一事实源；`total==0` 时不发第二条 SELECT；列表完全不碰缓存；COUNT 与取行的失败都原样上抛 |
| `listattachments_test.go` | 8 | 0 | `ListCovers`/`ListSubtitles` 的共同口径：只按 `asset_id` 过滤、不缓存、**不校验媒资是否存在**（未知媒资返回空列表 + `nil`，这是 `addcoverlogic.go:29` 明文声明的现状，用例把它钉成契约）；差别只在排序主键（`cover_id ASC` / `sub_id ASC`）与字幕多投影一个 `lang` |
| `registerasset_test.go` | 7 | 1 | 四条守卫的**先后与边界**且拒绝时一次依赖都不碰；登记行初始形态（`state=UPLOADED`、探测四列留空、`ctime==mtime`、主键由 INSERT 分配而入参没有 `asset_id`）；登记路径是**预热**缓存而不是失效，且缓存写失败被 `_ =` 吞掉、请求仍成功；没有幂等键 ⇒ 同 `upload_id` 重放得到两个 `asset_id`、两行、两条各自独立的缓存（缺口 2）；`size`/`md5`/空白引用列一律不校验（缺口 15） |
| `updateassetmeta_test.go` | 8 | 3 | 主键守卫；可改列面只有 `duration/width/height/codec` 加 model 刷新的 `mtime`，状态/归属/引用/`ctime` 一律不动；**不读**缓存的三步 `UPDATE → DEL → 回读`；空请求把已探测结果抹成零值、负值与 `FAILED` 终态一律放行（缺口 14）；缺行返回裸哨兵且**不 DEL 缓存**；UPDATE 失败或 DEL 失败都让陈旧缓存继续服务，后者连日志都没有（缺口 13）；`TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally` 是对该 UPDATE 语句的源码门禁 |
| `transitionstate_test.go` | 8 | 5 | 按 `model/errors.go` 的 `validTransitions` 列全 **5 条合法边与 14 条非法边**（不凭记忆挑几条）；合法边依赖序列恰好 6 条（读缓存→查库→回填→`UPDATE state`→DEL→回读），非法边只走前三条、`UPDATE` 一次都不发、库存 `state`/`mtime` 原封不动、返回**未包装**的 `ErrInvalidTransition`；`to_state` 只判 `== UNSPECIFIED` ⇒ 7/-1 是「非法迁移」而不是「非法状态」；`from` 取自可能陈旧 60 秒的缓存且 UPDATE 无 CAS ⇒ 已 `TRANSCODED` 的行被缓存里的 `UPLOADED` 合法化成 `SCANNED`，**状态回退一格而应答自洽**（缺口 3）；幽灵缓存行时写失败但不失效；DEL 失败被吞后状态推进对读侧不可见；`TestTransitionStateUpdateSQLHasNoStateGuard` 钉住语句里没有 state 条件；本方法没有归属入参 |
| `addcover_test.go` | 6 | 1 | 守卫顺序 `asset_id → bucket → object_key`；归属键是**入参 `asset_id`**、附件主键由 INSERT 分配，且应答里的 `cover_id` 能在库里查到同一行；整条路径的依赖序列**只有一条 INSERT** ⇒ 存在性/归属前置检查根本不存在（缺口 6）、也不碰详情缓存；`asset_cover` 只有非唯一 `KEY idx_asset` ⇒ 同 `(asset_id, object_key)` 重放是**追加新行**（缺口 16）；`width`/`height` 不校验 |
| `addsubtitle_test.go` | 6 | 1 | 守卫顺序 `asset_id → lang → bucket → object_key`（`lang` 判在 bucket 之前）；归属键 `(asset_id, lang)`；重复插入撞 `uniq_asset_lang` ⇒ 按真实 1062 建模，而 logic 与 model 都不翻译 ⇒ 调用方拿到裸驱动文本，既不是「已存在」业务码也不是覆盖成功（缺口 8）；唯一键作用域确实是 `(asset_id, lang)`；`lang` 只判非空（纯空格合法、`zh_CN` 与 `zh-CN` 并存两行） |
| `addscreenshot_test.go` | 6 | 1 | 四条守卫的先后（`timestamp` 判在最后、且在 bucket/object_key 之后）与归属键；`idx_asset_timestamp` 是非唯一 KEY ⇒ 同 `(asset_id, timestamp)` 重放追加新行、两张除主键外逐字相同；`timestamp` 只判 `<0` 不判上界；`TestScreenshotIsWriteOnlyInServiceContract` 是对 `rpc.AssetServer` 导出方法集的契约门禁——截图**没有任何读 rpc**（缺口 9 的前提） |
| `modelsql_test.go` | 3 | 0 | **源码文本对账**，兜住三件替身在行为上测不到的事：model 的 `ORDER BY` 方向（本包所有排序/分页期望由它派生，SQL 一改对账先红）、生产 `*Cache` 的 key 拼装与 TTL 60 秒（被测路径里被替身整个换掉，无法从行为观察）、「空串按 miss」「回填失败就地吞掉」两条判定。对账**不是**行为验证 |
| `fakes_test.go` | 1 | 0 | 唯一用例 `TestTrajectoryVocabularyMatchesFakes`：把替身实际落下的 15 条轨迹词条与 `op*` 构造器逐字对账，防止期望串与替身各说各话（读侧旧用例的内联字面量是第二重交叉校验）。其余内容是替身与装配工具，不贡献用例数 |

### 2. 其他层

- `internal/config`：1 个文件 `1/1` —— `config_load_test.go` 逐个加载 `etc/*.yaml`（子用例按文件名展开），
  反射递归断 `*DataSource` 与 `redis.RedisConf.Host` 非空；缓存字段必须叫 `CacheRedis`
  （`internal/config/config.go:15`），叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌的 `RedisKeyConf` 撞车、启动即挂。
- `internal/repository`（`Cacher`/`NewWithDeps` 注入缝的宿主）**无离线单测**——只被 logic 用例经由它跑；
  它的真实 `*Cache`（`SETEX`/`GET`/`DEL`）不在被测路径上。
- `model/`（`assetmodel.go`、`errors.go`、`now.go`）**无离线单测**：四张表的 SQL 文本、列清单、
  占位符个数与索引/唯一键只能由 `modelsql_test.go` 的源码对账与集成环境侧证。
- `internal/svc` **无离线单测**；`internal/server/assetserver.go`、`rpc/*.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层（本期不走 MQ、不派发转码任务，见「职责」）。

### 3. 构造器级覆盖：**10/10**

探针取 `internal/logic` 全部 `New*Logic(`，共 10 个，与「gRPC API」节声明的 10 个 rpc 一一对应，`gaps:` 为空——
包括截图与三个附件方法在内，每个 rpc 都有直接驱动自身构造器的用例，不存在只有间接断言的方法。

### 4. 替身层与断言口径（与「测试口径」同源）

- 被测路径是 `logic →` **真实 `Repository`** `→ 内存缓存 + 内存 model`：换掉的只是 Redis 客户端与 DB 连接，
  所以「先读缓存还是先查库、命中要不要回填、空结果要不要占位、列表走不走缓存、下游错误原样上抛还是换哨兵」
  整条链留在被测路径上。
- 断言口径：轨迹词条由 `op*` 构造器单边定义并与替身落下的词条对账；排序期望**不手抄字面量**，
  由 `modelsql_test.go` 抽出的 `ORDER BY` 子句 + `orderIDs` 比较器现算；每次调用记一条
  `<pkg>.<Method>:<key>` 有序痕迹，布景走静默路径所以序列从 0 数起；故障按方法粒度注入。
- **它证明不了**：SQL 文本、列清单与唯一键/非唯一 KEY 本身；`RowsAffected` 的 matched vs changed 差异
  （替身按匹配行数建模，故「同一秒重放同一份探测结果被驱动判成未找到」这条**测不到**，见缺口 17）；
  `lang` 大小写变体是否撞唯一键取决于实例排序规则（替身按字节比较，本包只锁「同字节串冲突」那一半）；
  缓存键格式、TTL 数值、`ORDER BY` 文本只有源码门禁、没有行为门禁。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、对象存储、etcd、MQ，也不依赖网络；真实 Redis 的 `SETEX`/`GET`/`DEL` 与
  真库事务/回滚未被驱动。
- 媒体内容既不进 MySQL、也不进用例：四张表只有 `bucket` + `object_key`（大文件永不入 MySQL），
  本服务也不生成任何签名 URL——「应答不带 URL/凭据」是由 `getasset_test.go` 的反射门禁钉住的**代码事实**，
  不是与真实对象存储的联调结论。
- 因此与预签名/回调相关的只有**离线判定**：`RegisterAsset` 的关联形态、`UpdateAssetMeta` 的探测列覆盖、
  `TransitionState` 的状态机门槛与失效序都在被测路径上；而「文件真的扫过、真的转过码、
  探测数字来自真实 ffprobe、Worker 回调真的会发生」一概不成立也不被断言
  （`ctime`/`mtime` 由 `model/now.go` 取本地时钟）。
- `GetAsset` 命中不查库 ⇒ 用例断言的是「最陈旧 60 秒」这一**离线结论**；真实 TTL 到期、真实 Redis 故障
  与真实脏键的自愈路径都不在覆盖内（缺口 13 的窗口只能由集成环境验证）。
- 迁移 SQL ↔ 真实库的列级对账：本节与「数据模型与迁移」都没有声明在隔离实例 `127.0.0.1:3399` 做过列级复验，
  因此按**未在目标实例复验**处理；迁移文件与登记以 `deploy/migrations/README.md` 为权威，
  本包没有 model↔DDL 的自动对账门禁。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/asset/...
gofmt -l services/asset
go vet ./services/asset/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下并发链接多个测试包会 OOM（`errno=1455`）；`-count=1` 关闭测试缓存。
- `go vet` 期望无输出、`gofmt -l` 期望为空列表。门禁统一串行执行，本节只登记命令与口径，不声明执行结论。

## 已知缺口

每条给出现状、影响、收严位置、以及我亲自 Read/Grep 核对过的 `file:line`。

1. **写侧 6 个方法没有任何构造器用例**（本轮已收口，编号保留以免引用漂移）
   现状：`RegisterAsset`/`UpdateAssetMeta`/`TransitionState`/`AddCover`/`AddSubtitle`/`AddScreenshot`
   各自有用例文件，10/10 构造器都被测试调用，未改任何生产代码。
   残留事实：补测试不消灭校验缺口——写侧「缺什么校验」已逐条落到缺口 2/3/6/8/13/14/15/16/17。
   用例：`registerasset_test.go`、`updateassetmeta_test.go`、`transitionstate_test.go`、
   `addcover_test.go`、`addsubtitle_test.go`、`addscreenshot_test.go`。
   定位：`services/asset/internal/logic/registerassetlogic.go:31`、`updateassetmetalogic.go:28`、
   `transitionstatelogic.go:29`、`addcoverlogic.go:30`、`addsubtitlelogic.go:29`、`addscreenshotlogic.go:29`。

2. **`RegisterAsset` 无幂等键，重复上传会产生多行 `asset_meta`**
   现状：`upload_id` 上只有非唯一 `KEY idx_upload`，model 层也没做「按 upload_id 先查再插」，
   两次重放各自拿到一个新 `asset_id` 并各自预热一条缓存。
   影响：upload 回调重试即产生重复媒资，且两行会各自推进状态机，下游取到哪条不确定。
   收严：迁移加 `UNIQUE KEY uniq_upload (upload_id)`，`Insert` 走 `INSERT ... ON DUPLICATE KEY UPDATE`，
   logic 层把重复识别为「返回既有 asset_id」。
   用例：`TestRegisterAssetDoesNotDedupSameUploadID`（钉现状：两条 INSERT 之间零次读、两行并存；
   收严后本用例连同期望一起改）。
   定位：`deploy/migrations/asset/000001_create_asset_meta.sql:26`、
   `services/asset/internal/logic/registerassetlogic.go:56`。

3. **`TransitionState` 的 `from` 取自可能陈旧 60 秒的缓存，且 UPDATE 无 CAS**
   现状：logic 先 `GetAsset`（缓存优先）拿 `cur.State` 当 `from`；`UpdateState` 的 WHERE 只有 `asset_id`。
   影响：并发推进会丢失一方（后写覆盖前写），缓存陈旧时甚至允许 `UPLOADED → TRANSCODED` 这类库里实际不合法的跳迁。
   已被用例钉住的形态：库里已 `TRANSCODED` 而缓存仍写 `UPLOADED` 时，`→SCANNED` 被判定合法并**写成功**，
   状态回退一格，而应答读的是回写后的行、与库存自洽 ⇒ 调用方看不出丢写。
   收严：`from` 改为强制读 DB（或 `UPDATE ... WHERE asset_id=? AND state=?` 并按 `RowsAffected==0` 判定冲突），
   `Repository.TransitionState` 的判定不依赖调用方传入的 `from`。
   用例：`TestTransitionStateTakesFromFromCacheAndCanRewindTheRow`（行为）、
   `TestTransitionStateUpdateSQLHasNoStateGuard`（源码门禁，锁住 SQL 里没有 CAS 条件这件事）。
   定位：`services/asset/internal/logic/transitionstatelogic.go:36`、
   `services/asset/internal/repository/repository.go:188`、`services/asset/model/assetmodel.go:145-147`。

4. **详情缓存不自愈、也不向 DB 降级**
   现状：命中但 JSON 损坏 → 直接返回 `GetAsset unmarshal cache: ...`，不回源、脏 key 留在 Redis 里；
   Redis 读错误 → 原样上抛，同样不回源。
   影响：一条坏缓存能让该 asset 持续 60 秒不可读（且写侧回填错误被吞，`repository.go:168`），
   Redis 抖动直接放大成媒资读接口 500。
   收严：unmarshal 失败与 Redis 错误都 `DelAsset` 后回源；回填失败至少 `logx.Errorf` 留痕。
   用例：`TestGetAssetCorruptCachePayloadFailsClosed`、`TestGetAssetCacheReadErrorPropagatesRaw`、
   `TestGetAssetBackfillFailureIsSwallowed`（当前钉的是现状，收严时要同步改期望）。
   定位：`services/asset/internal/repository/repository.go:150-159`、`:168`。

5. **无负缓存，不存在的 asset_id 每次都打到 DB**
   现状：`FindOne` 返回 nil 时直接抛哨兵，不写任何缓存标记。
   影响：对媒资 ID 的存在性探测（或上游轮询未落库的 ID）会全量穿透到 MySQL。
   收严：为 miss 写一个短 TTL（≤5s）的空标记，`GetAsset` 读到空标记直接返回哨兵。
   用例：`TestGetAssetMissingRowReturnsRawSentinel`（钉「不写负缓存」的现状）。
   定位：`services/asset/internal/repository/repository.go:161-168`。

6. **`asset_id` 引用完整性无人校验，媒资与稿件的关联结构性缺失**
   现状：`AddCover`/`AddSubtitle`/`AddScreenshot` 只校验 `asset_id>0`，不查 `asset_meta` 是否存在；
   三张附件表也没有外键；`asset_meta` 无 `aid`/`bvid` 列，缓存命中的详情也不复核 payload 与 key 一致。
   影响：可以造出挂在不存在媒资下的封面/字幕/截图；「哪个稿件用哪个媒资」只能由 video 侧持有，
   asset 无法自检一致性，脏引用只能在读取端表现为空列表或错内容。
   收严：附件表加 `FOREIGN KEY (asset_id) REFERENCES asset_meta(asset_id)`（或写前 `FindOne` 探针），
   缓存 payload 至少校验 `AssetID` 与 key 相同。
   附带事实：`addcoverlogic.go:29` 的注释声称「由 DB 外键或上层调用顺序保证」，但两个迁移脚本里
   **没有任何 FOREIGN KEY 定义**，注释与 schema 不一致，会误导后来的改动者以为 DB 层在兜底。
   用例：`TestListCoversFiltersByAssetAndDoesNotCheckAssetExists`、
   `TestListSubtitlesFiltersByAssetAndDoesNotCheckAssetExists`、`TestGetAssetCacheHitDoesNotCheckPayloadAssetID`、
   写侧 `TestAddCoverAcceptsAssetIDThatDoesNotExist`、`TestAddSubtitleWritesRowOwnedByAssetIDAndLang`
   （序列里只有 1 条 INSERT、`meta.*` 计数为 0 就是「无前置读」的行为证据）、
   `TestAddScreenshotAcceptsAssetIDThatDoesNotExistAndUngroundedTimestamp`、
   归属面 `TestTransitionStateHasNoOwnershipInput`（`TransitionReq` 没有身份字段，任何用户的媒资都能被推进）。
   定位：`services/asset/internal/logic/addcoverlogic.go:29`、
   `deploy/migrations/asset/000002_create_asset_attachment_tables.sql:16,30,43`、
   `deploy/migrations/asset/000001_create_asset_meta.sql:8-21`。

7. **「媒资不存在」跨 gRPC 只剩文案，没有稳定错误码**
   现状：`ErrAssetNotFound` 是 `errors.New` 出来的普通错误，经 gRPC 后变成 `Unknown` + 文本；
   catalog 用 `strings.Contains(err, "asset: not found")` 识别。
   影响：改一句错误文案就会静默打断 catalog 的存在性判断（退化成「下游故障」）。
   收严：改用 `google.golang.org/grpc/status` + `errcode`/`details.ErrorInfo` 携带稳定 reason，
   调用方按 reason 分支；同时 asset 侧保留一个契约用例锁住文案。
   用例：`TestGetAssetNotFoundTextIsTheWireContract`（当前是唯一的护栏）。
   定位：`services/asset/model/errors.go:7`、`services/catalog/internal/repository/asset_client.go:21`。

8. **`AddSubtitle` 撞 `uniq_asset_lang` 时把裸 1062 抛给调用方**
   现状：model 层只把驱动错误包进 `fmt.Errorf("asset_subtitle Insert: %w", err)`，logic 层不做重复语言判定。
   影响：同一 `asset_id` 重复登记同语言字幕时，上游拿到 MySQLError 文本而非稳定业务错误，
   无法区分「参数错」与「已存在」；换语言重登记也无法表达「替换」意图。
   收严：识别 1062 → 返回 `ErrSubtitleLangExists`（或 `INSERT ... ON DUPLICATE KEY UPDATE object_key/mtime` 明确覆盖语义）。
   用例：`TestAddSubtitleDuplicateLangReturnsRawMySQL1062`（钉现状：错误链里只有 1062 文本、
   不被翻译成本域任何哨兵、原行未被覆盖、库里仍只有一行），
   `TestAddSubtitleUniqueKeyIsScopedToAssetAndLang`（判别力对照：键只在 `(asset_id, lang)` 组合上生效），
   `TestAddSubtitleDoesNotValidateOrNormalizeLang`（`" "` 合法、`zh_CN` 与 `zh-CN` 并存）。
   未覆盖的一半：替身按**字节**比较 `lang`，而 `utf8mb4` 默认排序规则大小写/重音不敏感，
   所以「`ZH-CN` 在生产会不会撞上 `zh-CN`」这里测不出来，收严时要连唯一键的排序规则一起定。
   定位：`services/asset/model/assetmodel.go:243-248`、`deploy/migrations/asset/000002_create_asset_attachment_tables.sql:30`。

9. **截图只写不读，产物无法被消费**
   现状：`AssetScreenshotModel` 接口只有 `Insert`；`rpc/asset.proto` 的 `service Asset` 里没有 `ListScreenshots`。
   影响：转码产出的截图落库后没有任何读路径，选封面/预览图只能靠上游自己记账，表实际是只进不出的堆积。
   收严：model 加 `ListByAsset`（按 `idx_asset_timestamp` 排序）+ proto 加 `ListScreenshots`，重新生成后补读侧用例。
   用例：`TestScreenshotIsWriteOnlyInServiceContract`（契约门禁：`rpc.AssetServer` 的导出方法集恰好 10 个、
   截图侧只有 `AddScreenshot`）——补上读 rpc 时该用例先红，就是提醒同时改掉本条。
   定位：`services/asset/model/assetmodel.go:283-285`、`services/asset/rpc/asset.proto:161-185`。

10. **`ListAssets` 没有归属约束，`mid` 只是可选过滤**
    现状：`mid<=0` 时 WHERE 直接跳过该条件，跨用户全表分页；logic 层也不从会话注入 `mid`。
    影响：这是给运营面/admin 用的接口，但若被 app 网关直接暴露，任何调用方可枚举全站媒资。
    收严：契约上分两个 rpc（`ListMyAssets` 强制 `mid`、`AdminListAssets` 才允许跨用户），
    或在 model 层把 `mid<=0` 视为非法。
    用例：`TestListAssetsMidIsOptionalFilterNotOwnershipCheck`（钉现状）。
    定位：`services/asset/model/assetmodel.go:93-100`、`services/asset/internal/logic/listassetslogic.go:35`。

11. **缓存键 / TTL / SQL 文本只有源码门禁**
    现状：三个 `modelsql_test.go` 用例用正则从源码抽 `prefixAsset`、`cacheTTL`、`ORDER BY`，
    再与替身侧常量对齐；生产 `*Cache` 的真实 Redis 调用不在被测路径。
    影响：`SetexCtx` 参数写错、key 拼错、`redis.Nil` 分支回归这类问题，行为用例抓不到，只有改常量时才会红。
    收严：给 `Cache` 补一个基于 `miniredis`（若允许引入）或 `redis.Redis` mock 的集成层用例；
    或把 key/TTL 的构造抽成纯函数并在真实 `Cache` 上跑。
    用例：`TestModelOrderByClausesStillMatchFakes`、`TestProductionCacheKeyPatternStillMatchesFake`、
    `TestCacheMissAndBackfillJudgementsUnchanged`。
    定位：`services/asset/internal/repository/repository.go:27-28`、`:48-60`。

12. **`Repository.conn` 是写了没人读的死字段**
    现状：`New` 里赋值（`:106`），但 `Repository` 全部方法都通过 4 个 model 访问 DB，没有一处 `r.conn` 调用点。
    影响：字段本身无害，但它让「加 `NewWithDeps` 时要不要带 conn」这个判断变得模糊；
    真出现直连 SQL 时，走 `NewWithDeps` 的构造会拿到 nil 并在运行时炸。
    收严：要么删掉该字段（同时删 `New` 里的赋值），要么让 `NewWithDeps` 显式收 `conn`；现在按「保留生产语义」选择了前者之外的最小改动并在 `:112-114` 留了注释。
    用例：无需行为用例，属结构清理。
    定位：`services/asset/internal/repository/repository.go:90`、`:106`。

13. **写侧的缓存失效只在 UPDATE 成功之后发生，且失败被就地吞掉**
    现状：`UpdateAssetMeta`/`TransitionState` 都把 `DelAsset` 放在 UPDATE **成功之后**（`repository.go:182`、`:194`），
    且两处都是 `_ = r.cache.DelAsset(...)`：UPDATE 失败时根本不执行失效，失效失败时不报错、不重试、连日志都没有。
    影响：(a) 库里行已消失但缓存还在的「幽灵 key」永远无法被写路径清掉，读侧继续返回不存在的媒资、
    推进继续失败，形成 60 秒以上的稳定错误循环；(b) 一次 transcode 回写失败后，陈旧探测结果对读侧长期可见，
    而调用方拿到的是「成功」应答。
    收严：`DelAsset` 移到 UPDATE 之前（或失败分支里也删），删除返回值并至少 `logx.Errorf` 留痕；
    幽灵 key 由「回读为 nil ⇒ DEL」兜住。
    用例：`TestUpdateAssetMetaMissingRowReturnsRawSentinel`（冷/幽灵两种失败残留）、
    `TestUpdateAssetMetaDBFailureKeepsStaleCacheServing`、`TestUpdateAssetMetaDelFailureIsSwallowed`、
    `TestTransitionStateMissingRowReturnsSentinelAndKeepsPhantomCache`、
    `TestTransitionStateDelFailureIsSwallowedAndStaleStateKeepsServing`。
    定位：`services/asset/internal/repository/repository.go:182`、`:194`、
    `services/asset/internal/logic/transitionstatelogic.go:44`。

14. **`UpdateAssetMeta` 无字段增量语义、无值域校验、无状态前置**
    现状：proto3 分不出「没传」与「零值」，logic 只判 `asset_id > 0`（`updateassetmetalogic.go:29`），
    model 的 UPDATE 无条件把 `duration/width/height/codec` 四列一起写（`assetmodel.go:127-142`），
    并且完全不查 `state`。
    影响：一次只带 `asset_id` 的重放会把已写入的探测结果**抹成零值**（媒资仍是 `TRANSCODED`，时长却是 0）；
    负时长/负宽高/纯空格 codec 原样入库；`FAILED` 终态也能被回写，等于绕开状态机改历史。
    收严：要么按列增量（`IFNULL(?, duration)` 或 proto 侧 `optional`/field_mask），
    要么在 logic 加值域校验（`duration >= 0`、`width/height > 0`、`codec` 非空白）并限定可回写的状态集合。
    用例：`TestUpdateAssetMetaWritesProbeColumnsAndInvalidatesCache`（列面）、
    `TestUpdateAssetMetaEmptyRequestWipesExistingProbeResults`（抹零）、
    `TestUpdateAssetMetaAcceptsNegativeValuesOnTerminalAsset`（负值 + 终态）、
    `TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally`（源码门禁：SET 列面无条件、无 IFNULL）。
    定位：`services/asset/internal/logic/updateassetmetalogic.go:29-32`、
    `services/asset/model/assetmodel.go:127-142`。

15. **`RegisterAsset` 的守卫不覆盖 `size`/`md5`，也不清洗空白**
    现状：四条守卫只看 `upload_id/mid/bucket/object_key` 是否为正数/非空串（`registerassetlogic.go:32-43`）。
    负数 `size`、空 `md5`、纯空格 `bucket`/`object_key` 都能通过，进 INSERT 后原样出现在应答与预热缓存里。
    影响：`size` 是 `BIGINT NOT NULL DEFAULT 0`（`000001:13`），负数是一眼可辨的垃圾输入，
    下游（分片、带宽、发布前校验）只能各自再兜一遍；`md5` 是 `CHAR(32)`（`000001:14`）并配了
    `KEY idx_md5`（`000001:25`），但本服务**没有任何按 md5 的查询点**，空 md5 行既查不出、
    又会在将来加判重时彼此相撞。
    收严：`size > 0`、`md5` 长度 32 且十六进制、`bucket`/`object_key` `strings.TrimSpace` 后非空再入库。
    用例：`TestRegisterAssetDoesNotValidateSizeMd5OrWhitespace`（钉现状）。
    定位：`services/asset/internal/logic/registerassetlogic.go:32-43`、
    `deploy/migrations/asset/000001_create_asset_meta.sql:13-14,25`。

16. **`asset_cover`/`asset_screenshot` 无唯一键：重复登记只追加，且截图无法被读出来清理**
    现状：`asset_cover` 只有非唯一 `KEY idx_asset`（`000002:16`），`asset_screenshot` 只有
    非唯一 `KEY idx_asset_timestamp`（`000002:43`），logic 也不做「先查再插」或按 `object_key` 去重。
    影响：转码/封面重试一次，同一 `object_key` 就是多行封面（客户端会看到重复图），
    同一画面时间点是多张截图；截图侧还没有任何读 rpc（缺口 9），堆积既看不见也清不掉。
    收严：`asset_cover` 加 `UNIQUE KEY uniq_asset_object (asset_id, object_key)`（或加 `kind`/`seq` 显式表达多图），
    `asset_screenshot` 加 `UNIQUE KEY uniq_asset_timestamp (asset_id, timestamp)` 并把 INSERT 改成覆盖语义。
    用例：`TestAddCoverRepeatedCallsAppendRowsWithoutDedup`、
    `TestAddScreenshotRepeatedSameTimestampAppendsRows`（两行除主键外逐字相同）。
    定位：`services/asset/internal/logic/addcoverlogic.go:40-48`、`addscreenshotlogic.go:42-49`、
    `deploy/migrations/asset/000002_create_asset_attachment_tables.sql:16,43`。

17. **「未找到」判定依赖 `RowsAffected`，驱动上报的是改变行数 ⇒ 同值重放被误判**
    现状：`UpdateMeta`/`UpdateState` 都用 `if aff == 0 { return ErrAssetNotFound }`
    （`assetmodel.go:138-140`、`:155-157`）表达「行不存在」。go-sql-driver 默认不带 `CLIENT_FOUND_ROWS`，
    上报的是**被改变**的行数，而两条 UPDATE 都会把 `mtime` 写成 `nowUnix()`（`model/now.go:6-8`）。
    影响：同一秒内重放同一份探测结果 / 同一次状态推进时，行内容完全没变 ⇒ `RowsAffected == 0`
    ⇒ 调用方收到 `asset: not found`，把「幂等重放」误读成「媒资被删了」（叠加缺口 7 的文案判定，
    catalog 一侧会直接判成不存在）。
    收严：改用 `UPDATE ... WHERE asset_id = ?` + `RowsAffected` 只作冲突信号，
    或者按 matched rows 判定（DSN 加 `clientFoundRows=true`，或先 `FindOne` 确认存在再写）。
    用例：**行为用例测不到**——本包替身按匹配行数建模（缺行才返回哨兵），所以内存里两次同值回写都成功；
    现状由源码门禁 `TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally` 锁住
    「两处 `aff == 0` 判定 + UPDATE 一定写 mtime」这两件事，替身口径差异写在 `fakes_test.go` 头部。
    定位：`services/asset/model/assetmodel.go:127-159`、`services/asset/model/now.go:6-8`、
    `services/asset/internal/logic/updateassetmetalogic.go:34`。
