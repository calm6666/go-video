# engagement

点赞/点踩、收藏夹、分享上报等社区互动服务。

- **拥有数据**：点赞关系事实（`thumbup_like`）、可重算计数（`thumbup_stat`）、
  收藏项与收藏夹（`favorite_item`/`favorite_folder`）、分享日志（`share_log`）。
- **提供能力**：幂等点赞/取消/点踩、收藏夹增删查、收藏增删查与状态查询、
  分享上报（按天幂等）、计数批量查询。
- **依赖**：Redis（影子计数器 + 两个 60s 短缓存）、MySQL。
  本服务不调用任何其他服务的 RPC，也不收发 MQ。
- **约束**：不实现投币、支付或任何商业化余额（AGENTS.md §1）；
  互动计数**不得**同步刷新推荐/热度等下游投影（AGENTS.md §5），
  写路径只落本域 5 张表 + `eng:` 命名空间的缓存 key。

> 与早先版本的说明纠偏：本服务没有「不感兴趣」独立接口（`state=2` 的点踩由 `Like` 承载），
> 也没有播放历史（属于 `playback` 服务）；`deploy/migrations/engagement/` 里也没有历史表。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/Kafka/etcd）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，18 个文件 `138/23`）

按域分四组：点赞/点踩与计数、收藏项、收藏夹、分享与跨方法哨兵。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **点赞 / 点踩与计数** | | | |
| `likelogic_test.go` | 21 | 1 | 守卫顺序与「拒在触库前」；首次点赞锁「查旧状态 → 写关系行 → 增计数 → 影子计数器 → 回读真值」五步**有序序列**；重复点赞幂等短路只发两条 SELECT（一次写都没有）；点赞→取消→再点净值锁成 1；改点踩只挪一张票；信任库内状态而非客户端意图；未知 `action` 当取消；缺计数行时取消得到负数（缺陷 #8）；`ctime` 恒 0（缺陷 #2）；写路径不碰别域表；每个依赖各注入一次失败并断库里**实际残留**（`stat.Incr` 失败时关系行已落地）；缓存计数器写失败被吞 |
| `haslikelogic_test.go` | 5 | 3 | `business → mid → 空列表 → 超限` 四档顺序；空列表是短路成功但**必须不触库**；`state` 逐字段投影；重复 `message_ids` 去重；`stateToRPC` 对枚举外原始值的兜底 |
| `itemlikeslogic_test.go` | 6 | 1 | `business → message_id → ps` 守卫（`pn<=0` 归一成 1，表里单独一条合法用例证明归一生效）；mid+ctime 投影；`last_mid` 跳过上一页；空对象只发一条 COUNT；锁的是**实际** SQL 行为 `ORDER BY mid ASC`，不是接口注释里的「按点赞时间倒序」（缺口 12） |
| `userlikeslogic_test.go` | 7 | 1 | `business → mid → ps`（含 51 超限）守卫；只统计 `state=1`；total 是跨页全量而非剩余可翻页数；无点赞用户 total=0；纯读零写；model 失败上抛 |
| `statslogic_test.go` | 9 | 2 | 守卫次序 `business → 空列表短路 → 超限`，三者一次依赖都不碰；计数 + 用户态逐字段投影；没有计数行的对象整条丢掉（缺陷 #11）；`mid=0` 跳过用户态查询；`origin_id` 单值；两条查询各自失败上抛；纯读 |
| `multistatslogic_test.go` | 8 | 2 | 覆盖全部入参分支（没有「非法即报错」守卫，只有 nil 短路、空记录短跳、跳过 nil record、>100 超限），每个用例断 `callLog` 为空或只含预期查询以证明短路真的短路；逐 record 投影 `origin_id`；忽略 mid；按 message_id 去重；N+1 现状（缺陷 #10） |
| `rawstatlogic_test.go` | 6 | 1 | `business → message_id` 两条守卫（origin_id 不设守卫）；六个字段全投影；缺行时回显入参 ID；`origin_id=0` 合法且单独成键；只读本域 |
| `updatecountlogic_test.go` | 9 | 2 | 只有 business 与 message_id 两条守卫（origin_id、两个 change、operator 都不校验）；累加语义与 `number` 保留；DB 与影子计数器同向移动；**不幂等**（重试加两次，缺陷 #3）；计数行不存在时修正丢失且 DB/Redis 永久背离；忽略 `operator`/`ip`；只动本域 |
| **收藏项** | | | |
| `addfavlogic_test.go` | 6 | 1 | 三条守卫的顺序与「不触库、不触缓存」；`fid=0`（默认夹哨兵）在 AddFav 契约下算伪成功必须拒；写行 + 刷缓存；`favorite_item` 唯一键不含 fid ⇒ 换收藏夹是 UPDATE 而不是插第二行；已取消行复活；model 失败上抛、缓存失败被吞 |
| `delfavlogic_test.go` | 8 | 1 | fid 不校验（取消允许不带夹子，model 的兜底 UPDATE 正是为此）；标记取消 + 刷缓存；缺行与重复取消都幂等且保持状态；错 fid 走兜底 UPDATE；只动本 tp 行；失败上抛 / 缓存被吞 |
| `isfavoredlogic_test.go` | 10 | 1 | mid→oid 两条守卫都在读缓存之前；命中与负缓存两条短路路径不查库；miss 回源并回填 true/false 两条分别成列；`state` 已取消的行报未收藏；**tp 参与缓存键**；缓存读错与 model 失败上抛、回填失败被吞 |
| `isfavoredslogic_test.go` | 5 | 3 | mid→空列表短路→超 100 三档（超限时复用 `ErrTooManyMessageIDs`，文案与实际受限的 `oids` 错位）；只回带命中的行；**绕过缓存**；oid 去重；model 失败上抛 |
| **收藏夹** | | | |
| `addfolderlogic_test.go` | 7 | 1 | mid 先于 name 的守卫（空名夹属必须拒的脏数据）；写行 + 失效列表缓存且失效半径只归本人 mid；`private` 按给定值保留；**配额未实现**（`ErrFolderLimitExceeded` 是死代码，缺口 8）；model 失败上抛、缓存失败被吞 |
| `delfolderlogic_test.go` | 8 | 1 | 守卫顺序 fid 先于 mid——与 AddFav/DelFav 相反，锁住这个顺序避免「顺手统一」时改错语义；软删本人夹 + 失效缓存；他人夹与不存在夹被拒；重复删仍成功；**不级联 `favorite_item`**（缺陷 #6）；失败上抛 / 缓存被吞 |
| `userfolderslogic_test.go` | 12 | 1 | 只有 `vmid<=0` 一条守卫，mid 不校验 ⇒ 单独一条「mid 非法但 vmid 合法」用例锁它走「查他人收藏夹」分支而不是报错；本人 miss 回源并回填、hit 不查库、空列表写负缓存；他人夹隐藏 private；脏缓存报错不回源；只读本域；`tp`/`oid`/`all_count` 三个字段被忽略（缺陷 #12） |
| **分享与跨方法哨兵** | | | |
| `addsharelogic_test.go` | 7 | 1 | oid 先于 mid 的守卫顺序；记一次并回 `COUNT`；`share_log` 唯一键 `(oid, mid, tp, day)` ⇒ 同日重放只计一次、跨日各计一条、不同用户各计一条；Insert 与 Count 失败各自上抛 |
| `favtargetcheck_test.go` | 4 | 0 | 本包唯一的跨方法行为哨兵：`AddFav` 落库前**一次前置读都没有**（不存在的目标 / 别人的夹 / 自己已软删的夹都照写），`DelFav` 按 mid 寻址因此不会越权删别人的行；三条现状各带 `TODO(缺陷)`，修好即转红 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存缓存 + 5 个 model 接口替身 + 断言助手与有序轨迹，见第 4 组 |

域内不变量里最要紧的三条，本包都按 `deploy/migrations/engagement/*.sql` 的唯一键来断言：

- `thumbup_like` UNIQUE `(business, mid, message_id)` ⇒ 重复 `Like` 走
  `TestLikeRepeatedLikeIsIdempotent`（第二次只有两条 SELECT，一次写都没有）；
  取消不存在的行走 `TestLikeCancelWithoutAnyRecordIsSilentNoop`（幂等、无错）；
  点赞→取消→再点赞的净值由 `TestLikeCancelAfterLikeThenLikeAgainNetsOne` 锁成 1。
- `share_log` UNIQUE `(oid, mid, tp, day)` ⇒ 同日重复上报只计一次
  （`TestAddShareIsIdempotentWithinDay`），跨日各计一条（`TestAddShareCountsAgainNextDay`）。
- `favorite_item` UNIQUE `(mid, oid, tp)`，**fid 不在唯一键里** ⇒ 换收藏夹是 UPDATE 而不是
  插第二行（`TestAddFavRejectsDifferentFolderAsSecondRow`）；重复取消收藏幂等
  （`TestDelFavRepeatedCancelKeepsState`）。
- 但 `fid` 的**归属**与**存在性**都没有任何校验（下面已知缺口 14 与 16）：迁移里
  `favorite_item` 对 `fid` 只有 `KEY idx_mid_fid`、没有外键，`AddFav` 全程不读
  `favorite_folder`，所以「塞进别人的夹子」「写进自己已软删的夹子」「收藏一个不存在的作品」
  都会成功。`favtargetcheck_test.go` 把这三种现状钉成行为哨兵（各自带 `TODO(缺陷)` 注释）：
  `TestAddFavAcceptsNonexistentTarget_KnownGap`、`TestAddFavAcceptsForeignFolder_KnownGap`、
  `TestAddFavAcceptsDeletedFolder`；`TestDelFavIssuesNoExistenceOrOwnershipCheck` 则记录
  「取消动作按 mid 寻址，因此不会越权删别人的行」这一半好消息。

越域检查由 `TestLikeDoesNotTouchOtherDomainTables`、`TestUpdateCountOnlyTouchesOwnDomain`、
`TestRawStatOnlyReadsOwnDomain`、`TestUserFoldersOnlyReadsOwnDomain`、`TestUserLikesIsPureRead`
`TestStatsIsPureRead` 承担：写路径不得触碰别的域的表与别的命名空间的缓存 key，
读路径一次写都不许发生。

### 2. 其他层

- `model`（1 文件 `23/6`）：只有 `migration_parity_test.go`，即下小节的逐列门禁。
- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用真实 `conf.Load` 加载 `etc` 示例配置——
  `conflict key redis` 这类「能编译、启动即挂」的事故只在真实加载时暴露。
- `internal/repository`、`internal/svc`、`internal/server`：**无离线单测**
  （logic 用例经 `NewWithDeps` 组装真实 Repository 间接打到装配路径，
  但 `*Cache` 真身的 Redis 命令与 TTL 没有直接用例；`internal/server` 是 goctl 生成壳）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（不调任何其他服务的 RPC，也不收发 MQ）。
- **0 个 `t.Skip`**：本节用例都在跑，没有占位 skip。

#### 2.1 model ↔ DDL 逐列门禁（`model/migration_parity_test.go`，`23/6`）

上面的 logic 用例全部跑在内存替身上，替身跟着 model 走，所以**看不见 schema**。
这个门禁补的就是那一层：它不连数据库，只做「解析 `deploy/migrations/engagement/*.sql`
的 DDL + 反射 `model` 结构体 `db` 标签 + 用 `go/ast` 抽出 model 里每一条 SQL 字面量」，
然后双向逐列比对。覆盖 `model` 实际用到的 5 张表
（`thumbup_like`、`thumbup_stat`、`favorite_item`、`favorite_folder`、`share_log`），
并把 `share_stat` 作为「有 DDL 无 model」显式登记（见已知缺口 13），一共 6 张表 50 列 11 个索引；
比对方向包括表名存在性、列名与可空性、Go 字段宽度与列类型、手写列名是否存在于 DDL、
`ON DUPLICATE KEY UPDATE`/`INSERT IGNORE` 的冲突键是否真的是 PRIMARY/UNIQUE KEY、
唯一键列组合与幂等注释/查询是否同序、字符列清单与 DDL 类型、占位符数量与实参数量、
投影列与结构体字段顺序、以及每条 NOT NULL 列是否都有写路径。

它**不能**证明（如实声明，别把它当成 schema 已验证）：

- 不连库，所以证不了迁移**是否已执行**、也证不了 MySQL 解析器认可不认可这条 SQL
  （语法合法性靠人读 + 在隔离实例上跑迁移复验；本服务**尚未登记**这类复验结论，见第 5 组）；
- 不比字符集/排序规则与索引物理属性：DDL 里没有任何 `COLLATE`，因此
  `uniq_business_mid_message` 大小写不敏感（`archive`/`Archive` 会撞同一唯一键）这类事实
  只能停在已知缺口 18，门禁不判；也不判索引列序（`utf8` 前缀长度）、`USING BTREE`、表选项；
- 不比 `internal/repository` 与 Redis：缓存 key、TTL、影子计数器与 DB 的一致性不在范围内；
- 不比 logic 语义：它只看 model 里那条 SQL 字符串，替身对 `ON DUPLICATE KEY UPDATE`
  的**行为复刻**是否等价仍由 `internal/logic` 的用例负责；
- 投影顺序对齐的是**结构体字段顺序**，不是 DDL 列顺序（`favorite_folder.count` 在 DDL 里
  排在 `ctime`/`mtime` 之前、在结构体里排在最后，这是合法的，因为投影逐列点名）。

历史留档（2026-09-22 的非永真证明，改坏后 `diff` 与备份一致；只作证据记录，不是本轮门禁结论）：把
`model/share_log.go` 里那条 `INSERT IGNORE INTO share_log (oid, mid, tp, day, ctime)` 的列名
改回缺口 1 的 `type`，`go test ./services/engagement/...` 立刻三条断言全红且都点名 `share_log`：
`TestHandWrittenSQLColumnsExistInMigration`（`引用了不存在的列 "type"（DDL 实际列：[id oid mid tp day ctime]）`）、
`TestEveryNotNullColumnHasWritePath`（`share_log.tp: NOT NULL 列没有任何 INSERT/SET 写过它`）、
`TestUpsertAndInsertIgnoreConflictKeysAreRealKeys`（`冲突键 uniq_oid_mid_tp_day 的列 tp 没出现在 INSERT 列清单里`）。
门禁自身另有八处「解析器是否还有效」的反脆弱线，任何一条不达标就直接 `FailNow` 而不是悄悄少比：
`TestParserActuallyParsedTheSchema`（6 表 / 50 列 / 11 索引 / 5 唯一键 / 6 主键）、
`collectModelStatements`（至少 20 条 SQL 字面量）、`TestHandWrittenSQLColumnsExistInMigration`
（至少比对 100 处列名引用）、`TestInsertColumnListArityAndPlaceholders`（5 条 INSERT、至少 15 条
可比实参数的语句）、`TestUpsertAndInsertIgnoreConflictKeysAreRealKeys`（4 条 UPSERT/IGNORE）、
`TestFullRowProjectionsMatchStructOrder`（至少 8 条整行投影）、`TestStatementsSpreadOverEveryModelFile`
（逐表语句条数）、`TestModelFileInventoryIsExact`（model 目录的文件清单，新增/删除 .go 都要显式登记）。

### 3. 构造器级覆盖

`16/16`：探针取 `internal/logic` 全部 `New*Logic(`（16 个，与 `rpc/engagement.proto` 的 16 个方法
一一对应），逐个在 `*_test.go` 里查引用，`gaps:` 为空，即每个方法的 logic 都从构造器进入被打过。
`favtargetcheck_test.go` 是跨 `AddFav`/`DelFav` 的行为哨兵，不额外新增构造器。

### 4. 替身层与断言口径

每个写方法至少四组断言：**守卫表**（表驱动 + `wantNoCall` 证明拒在触库之前）、
**逐字段投影**（布景值互不相同，防串位）、**下游失败传播**（每个依赖各注入一次，
断言错误透出且不返回半截响应）、**域内不变量**（幂等、计数增减方向、越域检查）。
调用序列按**顺序**断言（`wantOps`，格式 `<替身>.<方法>:<键>`），不是只数次数；
`MultiStats` 因 `for range map` 本身乱序才降级用 `wantOpsUnordered`（只放宽顺序，不放宽集合与重数）。

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，因此
`internal/repository` 暴露 `Cacher` 接口 + `NewWithDeps(cache, likeMd, statMd, favItemMd,
favFolder, shareMd)`；生产路径仍只走 `New(rds, conn)`（`New` 就是 `NewWithDeps` 的一层装配）。
5 个 model 本来就是接口，所以本服务不需要为它们新增生产接口；`conn` 也不再是 `Repository`
的字段——没有任何一处直连 SQL。logic 用例据此组装**真实的 Repository**，只把它的 6 个依赖
换成内存替身，于是「幂等判定、计数增减方向、缓存读穿与回填、软删兜底、按天去重」整条链路
都在被测路径上，而不是把 Repository 整个 mock 掉。

`internal/logic/fakes_test.go` 守四条替身纪律：每次读返回值拷贝（否则「计数有没有真落库」
会被共享指针掩盖）；主键按真实 SQL 的自增口径分配（入参带的 ID 一律忽略）；副作用按顺序
记录；错误按方法粒度注入（`st.stat.failWith("Incr", errBoom)` + `errors.Is`）。第四条最容易
踩坑：**布数据必须走静默写入路径**（`seedLike`/`seedStat`/`seedFavItem`/`seedFolder`/`seedShare`/
`warmFolders`/`warmIsFavored` 直接写 map，不记轨迹），所以所有序列断言都从 0 数起。

替身复刻的是 model 层 SQL 的**语义**，不是 SQL 本身：`ON DUPLICATE KEY UPDATE` 的字段子集
（点赞冲突只覆盖 state/mtime、收藏冲突只覆盖 state/fid/mtime）、`INSERT IGNORE` ⇒ `added=false`、
`UPDATE` 未命中 0 行且不报错、`state=1`/`state=0` 过滤、`LIMIT ? OFFSET ?` 切片、
`ORDER BY ctime DESC` 与 `ORDER BY mid ASC`、`favorite_folder` INSERT 把 `state`/`count` 钉成 0、
`favorite_item.Del` 的两段式兜底（兜底那条 SQL 在接口边界上看不见，改用 `fallbacks` 计数断言）。

#### 4.1 断言强度的历史探针（变异探针）

2026-09-22 两轮探针的记录：改坏生产规则看用例是否真的红，四组全部被抓，探针后 `diff` 与备份一致后还原。
只作证据记录，**不是本轮门禁结论**：

1. `Repository.Like` 的「状态相同即幂等返回」短路改成 `if false && oldState == l.State` ⇒
   `TestLikeRepeatedLikeIsIdempotent`、`TestLikeCancelWithoutAnyRecordIsSilentNoop`、
   `TestLikeNoopReadReturnsExistingCounts`、`TestLikePropagatesNoopReadFailure`、
   `TestLikeStatIncrFailureLeavesRelationRow`、`TestLikeFinalReadFailureStillConverges` 全红
   （序列多出 `like.Upsert → stat.Incr → cache.Incr*`）；
2. 同处的「撤销旧状态贡献」`case 1: likeDelta--` 改成 `likeDelta++` ⇒
   `TestLikeCancelAfterLikeThenLikeAgainNetsOne`（`净值：取消后 = 2, want 0`）、
   `TestLikeSwitchFromLikeToDislikeMovesTheSingleVote`（`一张票不会凭空变两张 = 9, want 7`）、
   `TestLikeTrustsStoredStateNotClientIntent`、`TestLikeUnknownActionBehavesAsCancel`、
   `TestLikeCancelWithoutStatRowGoesNegative` 全红；
3. `Repository.Stats` 的 `if mid > 0` 改成 `if false && mid > 0` ⇒
   `TestStatsProjectsCountsAndUserState`（`调用序列 = [stat.FindMany:…]` 少了 `like.FindStates`）、
   `TestStatsDropsObjectsWithoutStatRow`、`TestStatsPropagatesStateQueryFailure`
   （`错误 = nil, want …boom`）、`TestStatsIsPureRead`、`TestStatsRejectsGuardsBeforeTouchingDeps` 全红；
4. `UserLikes` 的分页守卫 `if in.Ps <= 0 || in.Ps > 50` 改成 `if false && (…)` ⇒
   `TestUserLikesRejectsGuardsBeforeTouchingDeps` 的 `ps 为 0`/`ps 为负`/`ps 51 超限` 三条子用例红
   （`错误 = nil, want engagement: ps exceeds 50`）。

同日续轮另做四组（覆盖新哨兵与读侧分支，探针后同样 `diff` 与备份一致后还原）：

1. 再打 `Repository.Like` 的幂等短路（同上面第 1 组，为验证续轮基线未漂）⇒ 6 个用例红，
   `TestLikeRepeatedLikeIsIdempotent` 报出「第二次起多出 `like.Upsert → stat.Incr:…:0/0 →
   cache.Incr*`」的完整序列差；
2. `Repository.Like` 的「撤销旧状态贡献」`case 1: likeDelta--` 改成 `likeDelta++` ⇒ 5 个用例红，
   `likelogic_test.go:201: 净值：取消后 = 2, want 0`、`likelogic_test.go:266: 点赞改点踩：
   一张票不会凭空变两张 = 9, want 7`、`likelogic_test.go:339: 缺陷 #8：LikeNumber 变成负数 = 1, want -1`；
3. 给 `Repository.AddFav` 前面加一次 `r.favFolder.ListByUser(...)`（模拟「以后补上归属校验」）⇒
   `favtargetcheck_test.go:57`、`:92` 两条新哨兵**和** 4 条已交付 `AddFav` 用例一起红
   （序列多出 `folder.ListByUser:7:7`），证明新哨兵不是永真断言、且修复落地时会如期转红；
4. `IsFavoreds` 的批量上限 `if len(in.Oids) > 100` 改成 `if false && …` ⇒
   `isfavoredslogic_test.go:34: 101 个 oid 超限：错误 = nil, want engagement: message_ids count exceeds 100` 红。

### 5. 覆盖边界（如实声明）

- 用例不连接 MySQL/Redis/etcd/MQ，也不起 gRPC 服务端；`internal/server`、`rpc/*.pb.go` 等
  goctl 生成壳不在单测范围内。
- **logic 用例发现不了 schema↔model 的列名漂移**——替身两边都跟着 model 走，本包正是因此漏过已知缺口 #1
  （`share_log` 的 `type`/`tp`，2026-09-23 已修，编号保留）；该盲区现由
  `model/migration_parity_test.go` 的逐列门禁补上（见 2.1 小节），logic 用例本身仍然不承担这个职责；
- 替身**不做事务/回滚**，本服务的 `Repository` 本来也没把「关系行 + 计数」放进同一事务
  （缺陷 #1），用例只断言失败后库里**实际残留**了什么；
- Redis key 前缀（`eng:`）留在未导出的 `*Cache` 里，`Cacher` 按结构化入参划分，
  替身里的 `lc:`/`dc:`/`fl:`/`fav:` 只是内部寻址串，不用于断言真实键空间；
- 墙钟不可注入（`model.NowUnix()`/`time.Now()`）：布景一律给显式 `ctime`/`mtime`，
  由 model 内部取 now 的字段只做 `isRecentUnix`（±120s）级断言；
  分享上报的「今天」用替身的 `dayLock` 钉住，才能确定性复现按天幂等；
- `ListByMid`/`ListByItem` 在真实 DB 里是「COUNT + SELECT」两条语句，替身合成一个接口方法，
  所以「total 与 items 取自两次非事务读、并发下可能不一致」在内存里测不出来（已知缺口 #11）；
- `UserFolders`/`Stats`/`IsFavoreds` 里 `if f == nil { continue }` 之类的防御分支，
  替身永远不会返回 nil 元素，因此**不可达**，本包没有为它们写用例。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有登记隔离实例
  （`127.0.0.1:3399`）的复验结论，2.1 小节的门禁是**静态文本比对**，不接触任何实例；
  字符集/排序规则、索引物理属性与 `COLLATE` 缺口的结论仍停在已知缺口 18。
  `deploy/migrations/README.md:104` 把本服务登记为 `applied`，其口径只是「`up` + `status` 跑通」
  （见同文件 `:21`），与逐列对账是两件事。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/engagement/...
gofmt -l services/engagement    # 必须为空
go vet ./services/engagement/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 已知缺口

按严重度排序。1–15 的编号与用例里的 `缺陷 #N` 对应（个别条目跨多个编号，如缺口 6 对应 `#5`/`#14`）；
16、17、18 是近两轮新登记，用例里改用「已知缺口 N」标注以免与既有的 `缺陷 #14`（脏缓存不回源）撞号。
18 的三条内容是 `model/migration_parity_test.go` 里的豁免登记表（`unboundedChar`、
`unenumeratedUniqEnum`、`deferredIndexes`）逐条引用的对象：条目从 DDL 删除时豁免会报错，
所以这些编号与门禁是双向绑定的。

1. **`share_log` 的列名漂移（2026-09-23 已修复，编号保留）**：迁移
   `deploy/migrations/engagement/000003_create_share.sql` 声明的列是 `tp`，
   而 `model/share_log.go` 的 SQL 曾写 `type`（`INSERT IGNORE INTO share_log (oid, mid, type, day, ctime)`
   与 `WHERE oid = ? AND type = ?`）⇒ `AddShare` 在真实 schema 下必然报错。
   内存替身实现的是 model 接口、两边都跟着 model 走，所以**本包用例发现不了这条**，
   它靠 README 登记 + 人工比对才被查出。修复方向取「改 model 侧对齐 DDL」（与 `favorite_item`/
   `share_stat` 同为 `tp`），迁移文件未改、无需重跑。防复发的逐列比对门禁已落地：
   `model/migration_parity_test.go`（不连库，解析 DDL + 反射 `db` 标签 + `go/ast` 抽取每条 SQL 字面量），
   把上面那条 INSERT 的列名改回 `type` 会同时点亮三条断言，失败信息直接点名 `share_log`。
2. **`Repository.Like` 没有把「关系行 + 计数」放进同一事务**（缺陷 #1）：`Upsert` 成功后
   `statMd.Incr` 失败时，点赞关系已经落地、计数却是旧的，且接口返回错误。
   表现：客户端重试时因为「状态已相同」而短路，**计数永远补不回来**
   （`TestLikeStatIncrFailureLeavesRelationRow` 锁的就是这个现象）。
   修法是把两步放进 `TransactCtx`，或在 `oldState == state` 的短路分支里校验计数是否需要同步。
3. **`Like` 落库不带 `ctime`**（缺陷 #2）：`likelogic.go` 构造 `model.ThumbupLike` 时没填
   `Ctime`/`Mtime`，而 `HasLike`/`UserLikes` 返回给客户端的 `Time` 正是 `ctime` ⇒
   点赞时间恒为 0。`Upsert` 的 SQL 也不含 `ctime=VALUES(ctime)`，所以补错值也不会被覆盖修正。
4. **取消/点踩时缺计数行会得到负数**（缺陷 #8）：`thumbup_stat.Incr` 是
   `INSERT … VALUES(likeDelta, …) ON DUPLICATE KEY UPDATE like_number = like_number + VALUES(…)`，
   没有 `GREATEST(0, …)` 兜底。一行 `state=1` 的关系记录配不上计数行时（数据被清理或
   跨库迁移），取消点赞会把 `like_number` 写成 `-1` 并原样返回给客户端。
5. **运营修正计数既无幂等也无审计**（缺陷 #3、#13）：`UpdateCount` 的 `UpdateChange` 只有
   `UPDATE` 没有 `INSERT`，且忽略 `RowsAffected` ⇒ 计数行不存在时「修正成功」返回给运营、
   实际一位都没落；紧接着 Redis 影子计数器却被推进，DB 与 Redis 就此永久背离。
   同一请求重试会把修正加两次（`TestUpdateCountIsNotIdempotent`）。`in.Operator`/`in.Ip`
   从未被读取，所以谁改的、改了几次都查不到。`SET` 子句也不含 `mtime`，靠 `mtime`
   做增量同步的作业看不到这次修正。
6. **缓存写侧全部静默，读侧却会整请求失败**（缺陷 #5、#14）：
   `SetFolders`/`DelFolders`/`SetIsFavored`/`IncrLikeCount`/`IncrDislikeCount` 的返回值一律 `_ =`
   丢弃（后果：失效失败 ⇒ 本人列表在 TTL(60s) 内看不到新夹；回填失败 ⇒ 白读一次穿库）；
   反过来 `GetFolders` 报错或载荷不是合法 JSON 时直接判定整个请求失败、**不回源查库**，
   于是一次 Redis 抖动或一条写坏的 value 就能让收藏夹接口 500 满 60 秒。
7. **`DelFolder` 不级联 `favorite_item`**（缺陷 #6）：夹软删后，夹内条目的 `fid` 仍指向已删夹、
   `state` 仍为 0，`IsFavored` 照样报「已收藏」。另外没有任何路径维护
   `favorite_folder.count`（`Add` 的 SQL 把它钉成 0、`AddFav`/`DelFav` 都不改它），
   所以 `Folder.Count` 是个恒为 0 的
   快照位——客户端若按它渲染「共 N 条」会一直是 0。
8. **`ErrFolderLimitExceeded` 是死代码**（缺陷 #4）：注释与 `model/errors.go` 都写着
   「每人最多 100 个收藏夹」，但 `AddFolder` 既不发 COUNT 也不校验（见
   `TestAddFolderQuotaNotEnforced_KnownGap`）。错误定义本身也有冗余与文案错位：
   `ErrInvalidMessage` 与 `ErrInvalidMessageID` 是同一条消息的两个变量，
   `ErrTooManyIDs`（`ids count exceeds 50`）与 `ErrInconsistentIDs` 全服务无人使用，
   而 `IsFavoreds` 超限时复用的是 `ErrTooManyMessageIDs`（文案写 `message_ids`，
   实际限制的是 `oids`）。
9. **`MultiStats` 收了 `mid` 却从不查用户态、且是 N+1**（缺陷 #9、#10）：
   每个对象单发一次 `statMd.FindOne`（100 条记录 = 100 次 SELECT），而本可以按业务
   分组各发一次 `FindMany`；`LikeState` 因此恒为 `STATE_UNSPECIFIED`。
   `Stats` 与 `MultiStats` 对「没有计数行的对象」的处理还不一致：`Stats` 整条丢掉
   （缺陷 #11，连带用户已点赞的状态也丢，新投稿的点赞按钮会退化成未点赞），
   `MultiStats` 补一条零值记录。
10. **`UserFolders` 的三个请求字段无实现**（缺陷 #12）：`tp`/`oid`/`all_count`（以及 `otype`）
    从头到尾没被读过，proto 注释承诺的「收藏到哪个夹」提示与「全部分类计数」都不存在。
11. **分页 total 与 items 取自两次非事务读**：`ListByMid`/`ListByItem` 先 `COUNT(*)` 再
    `SELECT … LIMIT/OFFSET`，并发写时同一页可能报出对不上的总数；替身把两条语句合成
    一个接口方法，所以这条**在内存测试里不可观察**，只能靠读代码登记。
    `ListByItem` 的 COUNT 还刻意不带 `last_mid` 条件（total 是「全部点赞人数」而非「剩余可翻页数」），
    而 `ItemLikesReply` 里没有 total 字段，客户端拿不到这个数。
12. **契约字段与注释的偏差**：`ItemLikes` 的接口注释写「按点赞时间倒序」，
    实际 SQL 是 `ORDER BY mid ASC`（用例 `TestItemLikesProjectsMidAndCtime` 锁的是**实际**行为）；
    `LikeReply.LikeNumber` 的注释说「DB 值 + Redis 计数器」，实现只返回 DB 值（缺陷 #7）；
    `StatsReq.Ip`、`LikeReq.Ip`、`UserLikesReq.Ip` 等审计字段全部未使用。
13. **`share_stat` 表有 schema 无实现**：`000003_create_share.sql` 建了
    `share_stat(oid, tp, count)` 且带 `uniq_oid_tp`，但没有任何 model/logic 读写它，
    `AddShare` 的返回值是实时 `COUNT(*) FROM share_log`。分享数上量后要么接这张聚合表，
    要么给它加缓存，现在两者都没有。
    逐列门禁对此的处理是**显式登记而不是跳过**：`migration_parity_test.go` 里 `share_stat` 的
    `tableSpec.row` 为 nil、`modellessReason` 必须写「见 README 已知缺口 13」，DDL 侧的结构性
    断言（列/类型/唯一键/索引名/注释/时间列口径）照跑；`TestModellessTablesAreRegisteredAndStillUnused`
    反向盯着「model 一旦开始用这张表却没同步登记 row ⇒ 红」，`TestEveryNotNullColumnHasWritePath`
    则因为「无写路径」正是本条缺口的内容而显式跳过它并打印原因。也就是说：这条缺口不会
    让门禁变绿变松，只会在被真正补上时要求同步改动。
14. **收藏/点赞不校验对象是否存在**：`AddFav`/`Like` 直接落库，不与 `catalog`/`video` 校验，
    所以已删除投稿的互动行会永久留在 `thumbup_like`/`favorite_item` 里并计入 `thumbup_stat`；
    `IsFavored` 随后还会对同一个不存在的对象报「已收藏」。本服务没有任何外部 RPC 依赖，
    这一取舍要由 `catalog` 的下架事件或离线清理作业兜住。
    现状由 `favtargetcheck_test.go` 的 `TestAddFavAcceptsNonexistentTarget_KnownGap` 钉住
    （断言 `AddFav` 的完整调用轨迹只有三条、一次前置读都没有）。
    纠偏一条早期登记：`ErrVideoGone`/`ErrVideoNotFound` 在 engagement 里**不是「死代码」，
    而是根本不存在**——`model/errors.go` 没有定义任何「目标不存在/已下架」哨兵，
    所以真要加校验时得连错误值一起补。
15. **`UserLikes` 不做归属校验，且当前调用方也没做**：`mid` 来自调用方，本服务不比对会话，
    所以任何能访问 `EngagementRPC` 的调用方都能列举他人的点赞历史（隐私面）。
    实测 `gateway/app` 也没兜住：`internal/handler/routes.go` 里 `/engagement` 那一组
    只带 `rest.WithPrefix("/engagement")`，没有 `rest.WithMiddlewares`/JWT
    （同文件另一组路由是有中间件的，所以这不是写法限制），
    而 `types.ParamUserLikes.Mid` 是 `form:"mid"` 的查询参数并原样透传给 RPC ⇒
    **现在**就能匿名拉取任意用户的点赞列表。修法在网关侧（挂鉴权并以会话 mid 覆盖请求 mid），
    本服务若要自己兜住则需在 RPC 里增加「调用方身份」这一独立入参，而不是复用 `mid`。
16. **`AddFav` 不校验 `fid` 的归属与状态**（本轮新登记）：`fid` 只校验 `> 0`，全程不读
    `favorite_folder`，迁移里 `favorite_item` 对该列也只有普通索引 `idx_mid_fid`、没有外键。
    后果有三种，都被 `favtargetcheck_test.go` 钉成现状哨兵：
    ① `TestAddFavAcceptsForeignFolder_KnownGap`：mid=7 能把收藏写进 mid=999 的 `fid` 下面
    （他人的夹子本身不受影响，因为列表按 mid 过滤，污染的是自己这行的 `fid` 语义）；
    ② `TestAddFavAcceptsDeletedFolder`：`fid` 指向自己**已软删**的夹子照样写入，
    而 `UserFolders` 过滤 `state=1` ⇒ 这条收藏在任何收藏夹视图里都不出现，
    但 `IsFavored` 仍报「已收藏」，两处口径互相矛盾（与已知缺口 7 是同一处缺陷的两个方向：
    删夹不管存量条目，新藏又能写进已删夹）；
    ③ 结合 model 层缺口 17，这些孤儿行没有任何接口能读出来或清掉。
    修法在本服务内部即可完成（`favorite_folder` 就属于本域）：`AddFav`/`DelFav` 前校验
    「`fid` 属于 `mid` 且 `state=0`」（`fid=0` 的默认夹除外），修复时 ①② 两条用例会红，
    按注释改成断言 `ErrFolderNotFoundOrForbidden`。
17. **`favorite_item` 没有「按收藏夹列举」的查询，整个契约里也没有「收藏夹内容」接口**：
    `model.FavoriteItemModel` 只有 `Add`/`Del`/`IsFavored`/`IsFavoreds` 四个方法，
    `IsFavoreds` 还只返回 `oid → bool` 而不带 `fid`；`favorite_item.fid` 因此**只写不读**。
    客户端能建夹、能删夹、能列夹、能问「某对象是否已收藏」，却没有任何 RPC 能列出
    「某个收藏夹里有哪些收藏」——这正是参考实现里 `GetFavorite`/`ListUserFavorites` 承担的能力，
    本服务契约（`rpc/engagement.proto` 的 16 个方法）没有对应位置。
    要接这个能力需要新增 RPC 方法 + `favorite_item` 的按 `(mid, fid)` 分页查询 + 对应迁移索引，
    属于契约变更，不在测试轮范围内，登记给维护者决策。
18. **字符列的长度与排序规则没有任何一层把关**（逐列门禁登记为 `unboundedChar` 豁免，
    共 5 条：`thumbup_like.business`、`thumbup_stat.business`、`favorite_folder.name`/`description`/`cover`）：
    ① DDL 侧 `business VARCHAR(64)`、`name VARCHAR(100)`、`description`/`cover VARCHAR(500)`，
    而 logic 侧只有 `in.Business == ""`（`likelogic.go:44` 等 7 处）与 `in.Name == ""`
    （`addfolderlogic.go:34`）这一类非空判断，全服务没有任何 `len(字符串字段)` 守卫
    （只有 `len(in.MessageIds)`/`len(in.Oids)` 这类条数上限）⇒ 超长的 business/夹名会一路走到
    MySQL，在严格模式下变成 `Data too long for column`，接口返回 500 而不是 400。
    ② 三张表的表选项都是 `DEFAULT CHARSET=utf8mb4` 且**全仓没有一处 `COLLATE`**，
    于是排序规则取 MySQL 版本的字符集默认值（8.0 是 `utf8mb4_0900_ai_ci`，大小写与重音都不敏感）。
    `business` 是 `uniq_business_mid_message` 与 `uniq_business_origin_message` 的首列，
    所以 `'archive'` 与 `'Archive'` 在唯一键上是**同一行**、在 Go 侧比较与 Redis key 上是**两个值**：
    同一业务名换个大小写就可能既撞库又不命中缓存。修法要么在 logic 加长度与取值白名单，
    要么在 DDL 显式声明 `COLLATE utf8mb4_bin`（后者是行为变更，要单独评审并配数据核对）。
    同一条豁免还盖住两处文档缺口：`share_log.tp`/`share_stat.tp` 的列注释只写「目标类型」、
    没有逐值枚举（实际取值只在 `rpc/engagement.proto`：2 视频、11 ugv 视频、12 音频），
    而 `favorite_item.tp`/`otype` 与两处 `state` 都写了枚举，所以这不是风格差异而是漏写；
    另外 `favorite_item.idx_oid_tp (oid, tp)` 目前没有任何 model 查询走它（与缺口 17 同源），
    登记为 `deferredIndexes`，将来真有「按对象列举收藏者」的查询时豁免会反过来要求删除。
    `favorite_folder` **完全没有 UNIQUE KEY**（缺口 8 的配额未实现是同一件事的另一面），
    门禁以 `noUniqueKeyReason` 登记；一旦 DDL 补上唯一键而 model 侧没同步幂等口径，该用例会红。
