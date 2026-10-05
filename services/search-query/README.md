# search-query

面向客户端的搜索查询服务（只读检索侧）。

- **拥有数据**：`search_history`（用户搜索历史）、`search_query_log`（查询行为摘要）、
  `search_hot_keyword`（热词快照，投影）、`search_block_word`（屏蔽词字典）、
  `search_outbox`（领域事件）。库名 `go_video_search_query`。
- **提供能力**：关键词搜索（视频/用户/版权作品）、联想、热词、搜索历史读写、
  查询行为上报、查询配置下发。
- **依赖**：OpenSearch（只读别名，由 `search-indexer` 维护）、Redis（结果/热词/屏蔽词缓存、
  联想词典、计数）、MySQL、`common/eventenvelope`。
- **约束**：限制关键词长度、分页深度与请求频率；索引不可用时给出**明确降级**，
  绝不返回“空结果冒充成功”。索引是投影，不是事实源；本服务不写索引、不切别名。

## RPC 方法

`rpc/searchquery.proto` 的 `SearchQuery`（go-zero 生成，客户端由网关调用）：

| 方法 | 说明 | 主要错误 |
|---|---|---|
| `Search` | 关键词检索，cursor 优先分页，返回高亮片段 | `ErrInvalidKeyword`、`ErrInvalidCursor`、`ErrCursorMismatch`、`ErrDeepPage`、`ErrSearchUnavailable`、`ErrAliasMissing`、`ErrQueryRejected` |
| `Suggest` | 联想（Redis 词典优先，不足时引擎前缀回源） | `ErrSearchUnavailable`（两个候选源都不可用时） |
| `HotKeywords` | 热词榜（快照投影，空快照返回空列表 + `snapshot_at=0`，不编造榜单） | DB 错误原样上抛 |
| `GetSearchConfig` | 下发分页/长度限制、默认排序与 `EngineAvailable` | 配置缺失即报错 |
| `ListSearchHistory` / `DeleteSearchHistory` / `ClearSearchHistory` | 登录用户历史（删除为物理 DELETE，清空需 `confirm=true`） | `ErrInvalidMid`、`ErrConfirmRequired` |
| `ReportQuery` | 查询行为上报（`query_id` 幂等），同事务写 outbox 事件 | `ErrInvalidQueryID`、`ErrInvalidKeyword` |

## 索引契约

**读写两侧的字段词汇目前不一致，真实往返前必须先做契约对齐**（本轮只补了索引结构与分词脚本，
未改动本服务的查询模型，因为它牵涉已发布的 `searchquery.v1` RPC 字段）。

| 本服务使用（openbilibili 词汇） | search-indexer 实际 mapping | 处置 |
|---|---|---|
| `doc_type`（`video`/`user`/`pgc` 字符串） | `content_type`（1 UGC / 2 PGC / 3 直播，整型） | 需要类型翻译；索引里**没有 user 文档**，`DocTypeUser` 无对应投影 |
| `doc_id` | `content_id`（文档主键为 `<content_type>_<content_id>`） | 需要改名与主键拼装 |
| `intro` | `description` | 改名 |
| `zone_id` / `zone_name` | `typeid` / `type_name` | 改名 |
| `pub_time` | `publish_at` | 改名 |
| `hot_score` | `heat.heat_score`（int32） | 改为嵌套路径 |
| `view_count` / `like_count` / `danmaku_count` | `heat.view_count` / `heat.like_count` / `heat.danmaku_count` | 改为嵌套路径 |
| `fans_count` | 索引里没有（事实源 social-graph） | 要么由 search-indexer 增加投影字段，要么从排序/展示里移除 |
| `state`（字符串） | `state`（integer 1..5，仅 `2` 可检索） | 需要状态枚举翻译 |

- 查询别名 = `OpenSearch.Alias`，**必须等于** `search-indexer` 的 `OpenSearch.IndexPrefix`
  （默认 `go_video_content`）：重建流程只原子切换那一个别名，独立只读别名当前无人维护，
  会把查询留在正在退役的物理索引上。
- 分词由索引 mapping 决定：写入用 `go_video_title`、查询用 `go_video_search`，
  本服务的 `multi_match` 不显式传 analyzer，因此读写分词天然同源。
  中文召回效果依赖 `Analyzer.Kind`，词典与验证见 [deploy/opensearch/README.md](../../deploy/opensearch/README.md)。
- 文档字段与 `internal/repository/dsl.go` 的 `Field*` 常量一致；mapping 与写入实现归
  `search-indexer`（`go run ./services/search-indexer/cmd/esmapping` 可导出当前结构），
  两侧改字段必须同步评审。
- 区间语义：时长用 `gte` + `lt`（上界不含）；发布时间用 `gte` + `lte`（上界含端点）。
  注意字段名仍按上表的老词汇书写，对齐后需一并改名。

## 查询 DSL 与安全

- 关键词一律进入 `multi_match.query` 的**字符串值**，由 `encoding/json` 转义；
  不使用 `query_string`/`simple_query_string` 等“值即语法”查询，因此 Lucene 操作符
  （`+ - = && || > < ! ( ) { } [ ] ^ " ~ * ? : \`）不会被引擎解析成语法，也无法闭合括号注入子句。
- 仓库层单测对请求体做**结构检查**（子句 key 白名单、关键词只出现在 value 位置），
  见 `internal/repository/dsl_test.go`。
- 入口再做一次关键词规范化：移除控制字符、把 `<`/`>` 替换为空格（避免客户端富文本渲染
  时被注入自定义标记），见 `internal/repository/keyword.go`。
- 屏蔽词命中时不查询引擎，返回 `safe_filtered=true` 的空结果（这是唯一“成功且零结果”的场景）。

## 分页

- `cursor` 优先，`pn/ps` 仅为旧端兼容；游标内嵌查询指纹，翻页时改筛选条件报
  `ErrCursorMismatch` 而不是串页。
- 双重深分页保护（在读写缓存之前判定，被拒的翻页不消耗缓存与引擎配额）：
  `Search.MaxOffset`（默认 900，起始 offset 上限）与引擎 `index.max_result_window`
  （`from+size` 上限），超限返回 `ErrDeepPage`，引导客户端用更窄的筛选条件。

## 缓存与降级（红线）

结果缓存 key = `sq:res:v1:{查询指纹}:{offset}`（指纹含关键词、全部筛选、排序与页大小，
不含 offset）；缓存的是引擎投影、不含用户态数据，因此不同 `mid` 可共享同一 key。

`Repository.Search` 的判定顺序（实现见 `internal/repository/repository.go`）：

1. **引擎未配置**（`Endpoints`/`Alias` 缺失，`EngineAvailable()=false`）：一律返回
   `model.ErrSearchUnavailable`，即使 Redis 里还有副本也不返回。配置缺失是运维错误，
   用历史缓存撑着会把故障藏起来。
2. **引擎已配置但熔断打开**（`EngineHealthy()=false`，即 `esclient` 的 breaker 正在拒绝请求）：
   - 结果缓存**未过期**且 `Search.DegradeEnabled=true`：返回缓存，并置
     `SearchOutcome.Degraded=true`。网关据此返回 `ttl=0`（不写客户端缓存、不放大陈旧副本），
     监控据 error 日志与标记告警。
   - `DegradeEnabled=false`：不兜底，直接返回引擎错误。
3. **引擎健康**：缓存命中直接返回（`Degraded=false`，不消耗引擎配额）；未命中才请求引擎，
   引擎失败时若并发请求刚写入未过期副本，同样按 2 的降级规则处理，否则原样返回引擎错误。

补充事实：

- 命中缓存**不会续期**（只有引擎成功才写缓存），所以故障期最多 `CacheTTLSeconds`（默认 30s）
  就会回到未命中分支并再次调用引擎，熔断器的半开探活不会被“缓存命中”饿死。
- 熔断状态由 `esclient.Client.Healthy()` 提供：`doJSON` 在被 breaker 拒绝时置位、
  在请求真正跑通时清零，恢复完全交给 breaker，不做额外探测。
- 引擎自报 `timed_out`、分片失败（`_shards.failed>0`）都按失败处理，不返回半份结果。
- 缓存读失败只记日志、不阻断查询（可用性优先），但绝不被算作命中。
- 屏蔽词表加载失败时出口过滤退化为“不过滤”，主链路 `IsBlockedKeyword` 仍硬失败，
  安全边界不因缓存故障放宽。

## 事件（AGENTS.md §7）

`ReportQuery` 在同一事务内写 `search_query_log` + `search_outbox`（topic `search.query.v1`，
payload schema_version=1），保证“有日志必有事件”。消费者按 `event_id` 幂等去重，
下游是 `event-collector`/`spm`，**不含任何广告或商业化语义**。
重复上报 `query_id` 返回 `Deduplicated=true` 并回填首次的 `event_id`。
Redis 关键词/当日计数是旁路投影，失败只记日志。

## 配置与运行

```bash
# 从仓库根目录
go run ./services/search-query -f services/search-query/etc/searchquery.v1.yaml   # 监听 0.0.0.0:8107
```

- 表结构：`deploy/migrations/search-query/000001_create_search_query_tables.sql`
  （幂等 `CREATE TABLE IF NOT EXISTS`，回滚为 `DROP TABLE`；用户数据回滚前需按隐私流程处理）。
  本地执行入口是 `scripts/migrate.ps1`（本期未在共享实例执行）。
- 生产密码/Token/引擎口令从配置中心或 Secret 注入，示例配置必须留空。
- Redis key 命名空间 `sq:`（`sq:res`/`sq:hot`/`sq:bw`/`sq:sug`/`sq:cnt`），只属于本服务。

## 生成边界

`rpc/searchquery.proto` 是源文件；`rpc/*.pb.go`、`internal/server`、入口与 `types` 由
`goctl`/`protoc` 生成，禁止手改。契约变更后从仓库根目录执行：

```powershell
powershell -File scripts/gen.ps1 -Service search-query
```

自定义逻辑只写在 `internal/logic`、`internal/repository`、`internal/esclient`（手写客户端）。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL / Redis / OpenSearch / etcd，也不需要网络）。
数字为 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
本服务 0 条用例处于 `t.Skip` 状态。

### 1. `internal/logic`（9 个文件含 `fakes_test.go`）— `63/42`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `search_test.go` | 17/13 | 检索主链路的判定顺序与红线：守卫先于任何依赖（轨迹为空）；**深分页在读写缓存之前拒绝，不消耗缓存与引擎配额**；逐字段投影 + 规范化关键词贯穿引擎/缓存/日志三侧；筛选与排序原样进 DSL；游标翻页与 `max_result_window` 上限、`has_more` 判定、页大小夹取；**屏蔽词命中是唯一「成功且零结果」的场景**，屏蔽判定失败是硬错误（不许降级为「不过滤」）；引擎失败绝不伪装成空成功、真·空结果仍是成功；缓存命中不消耗引擎且不续期、降级 TTL 口径、并发写缓存后引擎失败的回落、缓存关闭时零次 Redis；历史记录写入是 best-effort 且受开关闸 |
| `reportquerylogic_test.go` | 9/6 | 上报三层一致：坏入参先拒（零轨迹）；`search_query_log` 列 + `search_outbox` 列 + 事件信封 payload 逐字段对照且同事务；`result_state` 映射落到存储；关键词在每一侧同源规范化；`query_id` 命中唯一索引时 `INSERT IGNORE` 不加行（并以 count=2 钉住「Redis 计数仍被重复累加」的现状，见已知缺口）；回填查不到时仍报 `Deduplicated`；游客与缺失可选标识；依赖故障传播；`device_id_hash` 只进事件不落列 |
| `hotkeywords_test.go` | 7/10 | 榜单不编造：守卫先于依赖；快照逐字段投影；scope 归一同时到达两层；缓存命中跳过快照表；**屏蔽词是在截断之后应用**（两个方向都钉，防改序无人发现）；缓存/DB/屏蔽词表三侧失败口径；`ttl` 随快照与配置双源 |
| `getsearchconfig_test.go` | 7/7 | 能力下发即纯进程内（显式断「不碰任何替身」）：坏请求先拒；每个能力字段全投影；`sortFields` 只为用户搜索展开（与 logic 侧校验同源）；默认排序回落；**下发值与 `etc` yaml 逐字一致**；`EngineAvailable` 表达「是否已配置」而非健康 |
| `suggest_test.go` | 8/3 | 联想三源合流权重序；词典不足时引擎前缀回源；命中屏蔽前缀是零成本成功；**出口按屏蔽集再过滤一次**（与整词主链路口径不同）；词典/引擎/历史三侧失败语义；屏蔽判定失败是硬错误；TTL 随缓存开关 |
| `listsearchhistorylogic_test.go` | 6/1 | 历史读侧：守卫先于依赖；逐字段投影 + `(mtime,id)` DESC 读取口径；keyset 翻页不重叠不遗漏；同秒按 id 倒序 tiebreak；「关闭」与「空」两种空可辨识；读失败上抛 |
| `deletesearchhistorylogic_test.go` | 5/1 | 只擦 `(mid, keyword_hash)`、他人 2 行逐行核对未受影响；幂等且不受 state 门槛；失败上抛；**按规范化词的精确字节删除而不是大小写折叠**（替身按字节匹配，只证明本层不折叠） |
| `clearsearchhistorylogic_test.go` | 4/1 | `confirm` 门禁先于依赖；`DeleteAll` 只擦 `mid=?`；幂等且不受 state 门槛；删除失败上抛 |
| `fakes_test.go` | 0/0（替身层） | 见第 4 组 |

### 2. 其他层（同口径实测）

- `internal/repository` — **4 文件 `39/1`**：`repository_test.go`(22/1) 覆盖 `Repository.Search` 的
  三段判定（未配置引擎一律显式失败、即使 Redis 有副本也不返回；熔断打开且未过期且 `DegradeEnabled`
  才兜底并置 `Degraded`；健康时命中缓存不耗引擎）、缓存键的 offset+指纹双维、缓存读故障回落引擎、
  深分页与页边界、`timed_out`/分片失败按失败处理、`_gte` 关系的近似总数、引擎错误码映射、
  DSL 方言下发、屏蔽词表读失败不 panic 且来源是 DB；`dsl_test.go`(9) 钉**查询体构造**：必须有关键词、
  分页与 `_source`、筛选与空筛选省略、**不使用 `query_string`/`simple_query_string` 因此 Lucene
  操作符只落在 value 位置（子句 key 白名单）**、排序与高亮、高亮标签取自配置、前缀查询体、
  查询指纹稳定性；`cursor_test.go`(8) 游标往返/首页/指纹不符拒/畸形拒/负 offset 拒/版本必须校验/
  keyset 往返/`pn→offset`。
- `internal/config` — **1 文件 `2/0`**：`config_test.go` 的 `TestExampleYamlMatchesStruct` 钉 `etc` 示例
  与结构体逐字段同源，`TestHighlightTagsDefaults` 钉高亮标签默认值。
- `model/`（4 张表 + outbox）**无离线单测**；`internal/esclient/`（手写 HTTP 客户端与 breaker）
  **无离线单测**；`internal/svc/` **无离线单测**；`internal/server/`、`rpc/*.pb.go` 是生成壳，不在单测范围。

### 3. 构造器级覆盖

`internal/logic` 的 8 个 RPC 构造器 **8/8** 有构造器级用例（探针 `PROBE 8 gaps:` 后为空，无缺口）。

### 4. 替身层与断言口径

生产构造 `repository.New` 走真 Redis + 真 MySQL + 真 OpenSearch，无处塞替身，因此两个包的用例都用
`repository.NewWithDeps(内存引擎, 内存缓存, fakeConn, cfg, 5 个 model 替身)` 组装**真实的 Repository**，
只替换它的依赖——关键词规范化、指纹与游标、深分页保护、DSL 构造、缓存/降级判定、屏蔽词出口过滤、
历史幂等写、上报同事务这些判定链全部落在被测路径上。为测试新增的唯一生产侧声明是
`repository.CachedResult`（`cache.go` 里未导出载荷的导出别名）。

四条替身纪律：① 每次读返回值拷贝（含缓存载荷与 model 行），共享指针不得掩盖「有没有真的落库/写缓存」；
② 写入按真实 SQL 口径处理主键与唯一键——`Insert`/`Upsert` 忽略入参 ID、自增分配，
`(mid, keyword)` / `query_id` / `event_id` 冲突按各自 `ON DUPLICATE` / `INSERT IGNORE` 语义，
查无此行返回 `(nil, nil)`（与 model 一致，不返回 `ErrNoRows`）；③ 副作用按**顺序**记录（callLog），
断言序列而非次数——要紧的结论是「先拦屏蔽词还是先打引擎」「日志与事件是否同一事务」「降级时有几次引擎调用」；
④ 布数据走静默路径（`seed*`/`warm*`/`put`，不记轨迹）。

它证明不了什么：替身只复刻 model 层 SQL 的**语义**（`state` 门槛、keyset 条件、`ORDER BY` 方向、
`LIMIT` 截断、`INSERT IGNORE` 的 0 行受影响），不证明 SQL 文本、列名与占位符本身；
真实的 `ORDER BY mtime DESC, id DESC` 与 `state=0` 过滤只由替身镜像；
`fakeConn.TransactCtx` 记录 `tx.Rollback` 但**不撤销**已写入的内存行，所以事务失败只断言
「未 Commit + 已 Rollback + 错误传出 + 无事件行」，行的原子消失由 MySQL 保证；
替身按字节精确匹配（等价 `BINARY` 排序规则），复现不了 `keyword` 列 `*_ci` 的大小写折叠（见「已知缺口」）；
熔断器与 Redis ZSET 由替身按同语义模拟，不验证真实实现。

### 5. 覆盖边界

- 用例不连接 MySQL / Redis / OpenSearch（Elasticsearch 兼容层）/ etcd / 对象存储。
- **查询体构造 ≠ 检索效果**：DSL 用例断的是请求体结构与字段词汇，引擎的真实召回、排序与
  中文分词效果完全未验证；读写两侧字段词汇仍不一致（见「索引契约」一节），对齐前不存在端到端结论。
- 仓库没有 search-query 的迁移↔model 列级对账门禁，`model` 包亦无测试文件；
  迁移 SQL 与真实库的列级对账**未在目标实例复验**（本 README「配置与运行」记的是
  「本期未在共享实例执行」，未声明隔离实例 `127.0.0.1:3399` 的复验结论）。
- `internal/server`、`rpc/*.pb.go`、`internal/esclient`、`internal/svc` 不在离线覆盖内。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/search-query/...
go vet ./services/search-query/...
gofmt -l services/search-query              # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（`errno=1455`），
测试门禁一律串行跑包（见 docs/commands.md）。
`gofmt` 的存量偏离由「已知缺口」末条登记（`internal/logic/search_test.go` 2 处未收口），
本轮未重测，以该条为准。

## 已知缺口

- `search_outbox` 尚无发布器投递（缺的是发布器实现：`go-queue v1.2.2` 已是 `go.mod:9` 的直接 require，
  依赖侧不阻塞，见 2026-10-04 复核），事件只保证“已可靠产生”。
- `search_hot_keyword` 快照的生产与 `PruneStale` 清理、`search_query_log` 的保留期截断
  （默认按 90 天规划）都需要 `services/cron`/离线聚合接入，本服务查询链路不写榜单。
- `search_block_word` 的写入口归 `services/operation`，评审后接入；当前由运维直接维护数据。
- 熔断健康状态（`EngineHealthy`）尚未通过 RPC/健康检查暴露给外部，只在 `Search` 判定与
  error 日志中体现；`GetSearchConfig` 返回的 `EngineAvailable` 表示的是“是否已配置”。
- 索引 mapping 与各端搜索词表未在 `docs/` 固化，目前以 `dsl.go` 常量与本文件为契约来源。

本轮测试补测时**新登记**的生产缺陷（只登记、未改代码，等裁决）：

- 历史擦除可能静默失效：`search_history` 的唯一索引是 `uniq_mid_keyword(mid, keyword)`，
  `keyword VARCHAR(128)` 未指定 `COLLATE`（utf8mb4 默认在 MySQL 8 为 `*_ci`），
  而 `DeleteKeyword`/`FindByKeyword` 按 `keyword_hash`（sha256 逐字节）定位。
  大小写/重音异形的同一个词在库里折叠成一行后，删除会返回 `deleted=0` 的成功，
  用户「擦了但没擦掉」——隐私口径问题。修法二选一：唯一键改走 `keyword_hash`，
  或显式 `COLLATE utf8mb4_bin`。
- 运营标记会被用户自己抹掉：`Upsert` 的 `ON DUPLICATE KEY UPDATE ... state = VALUES(state)`
  配合 `RecordHistory` 恒传 `HistoryStateNormal`，导致 state=1（标记）/2（待清理）的行
  在用户再搜一次同一词后回到 state=0 重新下发。
- 重复上报会重复计数：`query_id` 命中唯一索引时 `INSERT IGNORE` 不新增行，
  但 `ReportQuery` 之后仍无条件调 `IncrQueryCounters`，热词榜的 Redis 投影按重复次数累加，
  与「幂等不产生新事实」的口径不一致（`reportquerylogic_test.go` 以 count=2 钉住现状，
  修复后该断言会变红）。
- 脱敏列未校验格式：`validateOpaqueID` 只挡空白/控制字符/长度，
  不带空白的原始 IP（`203.0.113.9:51234`）可原样写进 `ip_hash`
  （`TestReportQueryDoesNotGuessOpaqueIdentifierFormat` 登记，非背书）。
- `platform` 校验按 `ToLower` 放行、入库却保留原样，`Android`/`android` 两形态可共存于同一列。
- 文档与实现冲突：`model/errors.go` 注释称 `HistoryStateFlagged`「仍可见」，
  实际 `ListByKeyset` 带 `state = 0` 条件（迁移 SQL 注释与代码一致，注释是错的）。
- `ReportQuery` 的 `len(resultState) > maxResultStateLen` 守卫不可达（`resultStateOf`
  只返回 4 个定长常量），属死代码。
- 质量门禁存量：`internal/logic/search_test.go` 不符合 gofmt（2 处），
  本轮按纪律未改动该文件；`fakes_test.go` 的 2 处 gofmt 偏离已在本轮修正（纯空白，无语义变化）。
