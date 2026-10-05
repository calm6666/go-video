# comment

视频、直播和动态评论及回复服务。

- **拥有数据**：评论、楼中楼回复、举报、置顶/折叠状态和审核状态。
- **提供能力**：发布、删除、回复、排序、置顶、举报、黑名单过滤。
- **依赖**：`account`、`user-profile`、`moderation-orchestrator`、`video`/`live-room`。
- **约束**：评论与弹幕分库存储；发布先过风控/审核策略；查询按查看者过滤。

> 顶部三条声明里，「黑名单过滤」与「发布先过风控/审核」两条**目前只有约定、没有实现**：
> 查询侧不接查看者（`viewer_mid` 被解析后被丢弃），发布侧 `state` 由调用方随意传（可直发 `STATE_NORMAL` 跳过审核），
> 且全服务没有 consumer/Outbox 承接审核结论。详见「已知缺口」1、2、3。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/Kafka/etcd）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，8 个文件 `57/20`）

按域分三组：写侧（发布/删除/举报）、读侧（列表/楼中楼/计数）、状态位（置顶）。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **写侧** | | | |
| `postcomment_test.go` | 7 | 3 | 守卫顺序 `oid → mid → content → root/parent` 一致性且全部拒在触库/触缓存之前；行 + 响应逐字段投影；回复保留 root/parent 且**不动任何计数**；同一请求重放**会双写**两行不同 `rpid`（缺口 5 的哨兵）；空白/超长内容无校验（缺口 6）；Insert 失败上抛且缓存零污染、三处缓存写失败被吞 |
| `deletecomment_test.go` | 8 | 6 | 本人删 → 软删 + 定向清 5 步链路（保留 `content`/`oid`/`mid`/`ctime` 与计数，`mtime` 被写成 `mid` 这条 bug 也照抄，缺口 15）；管理员越权删、非本人删**零副作用**；目标不存在的两种姿态；失败上抛；删后 `CommentStats` 与 `ListComments` 口径复核（孤儿回复仍计数，缺口 14） |
| `reportcomment_test.go` | 6 | 1 | 守卫（`rpid`/`reporter_mid`）在触库前；只写审计行且 7 字段逐一断言（含 `trace_id` 透传）、**不碰 comment 表与缓存**；重复举报重复插行（缺口 12）；`reason`/`content` 无长度校验；存储失败上抛；空响应无字段 |
| **读侧** | | | |
| `listcomments_test.go` | 13 | 2 | 7 条守卫、`pn<=0→1` 与 `sort` 归一（未识别枚举一律降级 hot）、12 字段逐字段投影、热/时间两种排序给出的顺序不同、分页无重叠且并集等于全集、二次读命中缓存（键逐字符比对）、**缓存跨查看者共享**（缺口 1 的哨兵）、脏 payload 必须报错不回源、读/存储失败上抛、回填失败被吞、空页也回填 |
| `listreplies_test.go` | 7 | 1 | 守卫（`root`/`ps`）、`ctime ASC` + 全字段投影、`pn` 归一、分页无重叠、**二级回复仍留在同一楼层**（root 而非 parent 决定归属）、不校验 root 是否存在、存储失败上抛；全程 `cache.*` 调用数为 0（楼中楼无缓存，缺口 18） |
| `commentstats_test.go` | 8 | 3 | 守卫、`tp` 完全不校验、miss 时按可见性重算并回填（`total=7`/`root_total=3` 刻意不同）、hit 时**不查库**（缓存里的 99/98 直接胜出，走 COUNT 会变 7/3）、PENDING 被算作可见（缺口 13）、读/查失败上抛且失败路径不留脏缓存、回填失败被吞、删除后缓存被清 ⇒ 下一次重新计数 |
| **状态位** | | | |
| `pincomment_test.go` | 8 | 4 | 8 条守卫（含三条内联错误文案）、置顶会顶掉旧置顶、**不失效本目标列表缓存**（缺口 9 的哨兵）、取消置顶只作用于 PINNED 行（不洗 FOLDED/PENDING/DELETED）、目标不存在时静默顶掉旧置顶、跨目标置顶把已删评论复活成可见置顶（缺口 10）、存储失败上抛、缓存失败被吞 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存缓存 + `comment`/`comment_report` 两个 model 替身 + 断言助手与有序轨迹，见第 4 组 |

### 2. 其他层

- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用真实 `conf.Load` 加载 `etc` 示例配置——
  本仓库出现过 Config 自带 `Redis` 字段与 `zrpc.RpcServerConf` 内嵌的 `Redis` 同名，
  代码可编译但启动即报 `conflict key redis`，只有真实加载才暴露。
- `model/`（`comment.go`、`comment_report.go`、`errors.go`）：**无离线单测**。SQL 文本、列名、
  占位符与索引只能由迁移与真实实例证明（见缺口 5 与第 5 组）。
- `internal/repository/`：**无离线单测**。键拼装、失效半径与软删归属判定由 logic 用例
  经真实 `Repository` 间接打到，但 `Cache` 真身（`SetexCtx`/`Scan`/`Del`）没有直接用例。
- `internal/svc/`、`internal/server/`：**无离线单测**（goctl 生成壳与装配）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（审核结论无人消费，见缺口 3）。
- **0 个 `t.Skip`**：本节用例都在跑，没有占位 skip。

### 3. 构造器级覆盖

`7/7`：探针取 `internal/logic` 全部 `New*Logic(`（7 个，与 RPC 方法一一对应：
`PostComment`/`DeleteComment`/`ListComments`/`ListReplies`/`PinComment`/`ReportComment`/`CommentStats`），
逐个在 `*_test.go` 里查引用，`gaps:` 为空，即每个方法的 logic 都从构造器进入被打过。

### 4. 替身层与断言口径

`svc.ServiceContext.Repository` 是**具体类型** `*repository.Repository`，测试没有插替身的地方，
所以按 catalog/rights/playback 的既有口径在 `internal/repository` 上开门缝：
把缓存依赖抽成 `Cacher` 接口（`Ping`/`GetList`/`SetList`/`SetOne`/`DelOne`/`GetStats`/`SetStats`/
`InvalidateListByOid`/`DelStats`，生产由 `*Cache` 实现），并新增
`NewWithDeps(cache Cacher, commentMd model.CommentModel, reportMd model.CommentReportModel) *Repository`；
生产路径仍只走 `New(rds, conn)`（`New` 内部调 `NewWithDeps`），`svc` 与装配语义未改。
原 `Repository.conn sqlx.SqlConn` 字段是死重（comment 全服务不用事务），随门缝一并删掉；
`NewWithDeps` 因此只需 3 个参数而不是 playback 的 6 个。

logic 用例据此组装**真实 Repository**，只把它的 3 个依赖换成内存替身，
所以缓存回源、键拼装、失效范围、软删归属判定这些链路都在被测路径上，而不是把 Repository 也 mock 掉。

`fakes_test.go` 记了四条替身纪律：读时值拷贝（否则「有没有真落库」被共享指针掩盖）、
按真实 SQL 口径分配主键（从 500（comment）/ 700（report）起，所以布景 rpid 必须避开 501+，
轨迹里的 ID 才是确定的）、按顺序记录调用轨迹（断序列而不是只断次数：要紧的结论是
「发布后失效了哪些 key」「拒绝删除时有没有留下半截失效」「置顶清掉了谁的置顶」）、
按方法粒度注入错误；外加 playback 那轮补的第四条——**布数据必须走静默写入路径**
（`put`/`warmList`/`warmStats`），否则序列断言会把布景调用也算进去。
Redis 键与 SQL 语义按 `repository.go` / `model/comment.go` 逐字复刻
（`cmt:list:%d:%d:%s:%d:%d`、`cmt:1:%d`、`cmt:st:%d:%d`、`state NOT IN (2,5)`、`ORDER BY state=3 DESC`、
`SET state=2, mtime=mid` 这个 bug 也照抄），键一漂移用例就红。

断言口径：拒绝类用例一律断「一次依赖都没碰」（`wantNoCall`）而不是「返回了错误」；
投影类用例铺互不相同的种子值逐字段比对；写侧断**完整有序序列**（`wantOps`，
格式 `<替身>.<方法>:<键>`）；缓存类断言逐字符比对键名。

历史留档（2026-09-22 变异探针三组，非本轮门禁结论）：改坏生产规则 ⇒ 用例转红 ⇒ 还原复跑。
绕过 `PostComment` 的 `InvalidateListByOid` ⇒ 写侧链路与「缓存失败必须被容忍」两条同时红；
删掉 `ListComments` 的 `in.Ps <= 0` 分支 ⇒ 表驱动里 4 个 `ps` 边界用例同时红；
绕过 `CommentStats` 的缓存命中分支 ⇒ 「命中不查库」的 total/root_total/调用序列三条同时红。
第四组（把 `DeleteComment` 的 `in.Admin` 改成常量）被权限分类器拦下未执行。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/Kafka/etcd，也不起 gRPC 服务端；`internal/server` 与 `rpc/*.pb.go`
  等 goctl 生成壳不在单测范围内。
- 替身只复刻 model 层 SQL 的**语义**，不证明 SQL 本身：`model/*.go` 的列名与占位符仍无单测
  （要证明得引入 sqlmock，超出本轮范围）；本服务也没有 model ↔ 迁移的列级对账门禁
  （`model/` 无测试文件，仓库里只有 engagement、spm 等少数服务建了这种门禁）。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有隔离实例
  （`127.0.0.1:3399`）的复验结论，所以「`rpid` 是否真的有 AUTO_INCREMENT」、
  「`uniq` 约束是否存在」都还只是代码意图（缺口 5）。
  `deploy/migrations/README.md:98` 把本服务登记为 `applied`，其口径只是「`up` + `status` 跑通」
  （见同文件 `:21`），不覆盖 model ↔ DDL 的逐列对账。
- `Cache` 真身（`SetexCtx`/`Scan`/`Del`）没被测，因此 TTL 与 `SCAN`/`KEYS` 的实际行为
  不在断言范围内（缺口 11 的选型问题只能读代码确认）。
- 并发与 TOCTOU（缺口 16 的「先 `FindOne` 后 `SoftDelete`」窗口）没有用例，替身是单线程的。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/comment/...
gofmt -l services/comment    # 必须为空
go vet ./services/comment/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 已知缺口

按「现象 → 定位 → 影响」登记。**本轮只登记，不改生产语义**；
标 ⛑ 的由现有用例把它当前行为固定成哨兵（改名即红），后来者修的时候顺手解绑。

1. **`viewer_mid` 被接收后被丢弃** ⛑：`ListCommentsReq.ViewerMid` 在
   `internal/logic/listcommentslogic.go` 里从未被读，缓存键也不含它
   （`TestListCommentsCacheIsSharedAcrossViewers` 断言了这一点）。
   因此 README 顶部「查询按查看者过滤」目前不成立：拉黑某用户后仍会看到其评论。
2. **发布不接风控/审核，`state` 由调用方随意传**：`postcommentlogic.go:54` 直接
   `State: in.State`，既不校验取值范围也不强制 `STATE_PENDING`。
   客户端传 `state=0` 即可跳过审核直发，违反 AGENTS.md §8。定位：logic + `model.Insert`。
3. **审核回调 / 状态推进的消费者不存在**：`comment` 全服务无 consumer、无 `TransactCtx`、无 Outbox，
   因此 `STATE_REJECTED`/`STATE_FOLDED` 在生产里**没有任何写入路径**
   （唯一会改 `state` 的代码是 `SoftDelete` 写 2 与 `SetPinned` 写 0/3）。
   举报写的是本地审计快照，`moderation-orchestrator` 收不到任何东西，
   「举报 → 审核 → 折叠/驳回」闭环只能靠手写 SQL 推进。
4. **无 Outbox ⇒ 也就无「两写拆分」**：本轮按任务要求专门核了「业务表写与事件写是否同一事务」，
   答案是**没有事件写**，所以该项不适用；真正的后果是被 @ 者、被回复者、举报回执都无人通知。
   接入 Outbox 时必须按 AGENTS.md §5 与 `comment` 行同一事务。
5. **`PostComment` 不幂等** ⛑：无幂等键、无 `ON DUPLICATE`、无去重缓存，
   同一请求重放插入两行不同 `rpid`（`TestPostCommentReplayIsNotIdempotent`）。
   `rpid` 是 `BIGINT NOT NULL` 主键但 `Insert` 走 AUTO_INCREMENT 语义（不写 `rpid` 列）——
   迁移 SQL 注释说「rpid 为全局雪花 ID」，两处口径矛盾，需在真机确认 `rpid` 是否有 AUTO_INCREMENT，
   否则插入会因主键无默认值直接失败（本替身无法证明，见覆盖边界）。
6. **`content`/`reason` 无长度与空白校验** ⛑：`comment.content` 是 `VARBINARY(9000)`、
   `comment_report.content` 是 `VARCHAR(500)`，但 logic 侧只挡空串。
   超长内容由 MySQL 抛 1406 ⇒ gRPC 500，而不是干净的 4xx；纯空白串（`" "`、`"\n"`）可入库。
   定位：`postcommentlogic.go:38`、`reportcommentlogic.go`，
   用例 `TestPostCommentHasNoContentLengthOrBlankGuard`、`TestReportCommentHasNoReasonOrLengthGuard`。
7. **`root`/`parent` 不校验存在性与归属**：只挡「有 root 无 parent」这一种组合
   （`postcommentlogic.go:42`）。可把回复挂到不存在的 root、或别人的 root 上，
   从而往任意楼层里塞内容并影响 `root_total`/`total` 口径。`ListReplies` 也不校验 root 存在
   （`TestListRepliesDoesNotValidateRootExistence`）。
8. **`reply_count` / `like_count` 快照是死字段** ⛑：`IncrReplyCount` 全仓**零调用**，
   `IncrLikeCount` 在 comment 服务内也**零调用**（无 consumer），
   `Insert` 又把两列硬写 0（`model/comment.go:67`）。
   后果：`ListComments` 的「热评」排序 `ORDER BY like_count DESC` 实际按恒 0 排 ⇒ 退化成 ctime 序；
   `CommentInfo.reply_count` 恒 0。定位：`model/comment.go:225,231`、`repository.go:315`。
9. **`PinComment` 失效错了缓存键** ⛑：`repository.go:288` 传的是 `InvalidateListByOid(ctx, oid, 0)`，
   tp 恒 0，而列表键是 `cmt:list:<oid>:<tp>:<sort>:<pn>:<ps>`（视频 tp=1）。
   模式对不上 ⇒ 置顶后列表页在 30s TTL 内仍返回旧顺序（`TestPinCommentDoesNotInvalidateListCacheForItsTarget`）。
   顺带：置顶也不动 `cmt:st:*`。
10. **`SetPinned` 跨目标且不校验状态** ⛑：`model/comment.go:185-203` 的
    `UPDATE comment SET state = 3 WHERE rpid = ?` 不带 `oid` 条件 ⇒ 能把别的作品的评论置顶，
    并把 `state=2`（已删除）/`4`（待审）的行**复活成可见的 PINNED**；
    取消置顶则无脑 `SET state = 0`，会连带把该行的原始状态信息丢掉
    （`TestPinCommentAcceptsForeignTargetAndRevivesDeletedRow`、`TestPinCommentUnpinOnlyAffectsPinnedRows`）。
    另外 `admin_mid` 只做 `>0` 守卫、从不传给仓储 ⇒ 置顶在本服务内**完全没有鉴权**
    （`pincommentlogic.go:28` 的注释把责任推给 gateway）。
11. **`InvalidateListByOid` 用 `KEYS` 而非 `SCAN`**：`repository.go:154` 调 `c.rds.KeysCtx`，
    Redis 的 `KEYS` 是 O(整个 keyspace) 且**阻塞主线程**的命令。热门作品下每次发帖/删帖都触发一次，
    生产实例键多时会拖慢整个 Redis。全仓检索确认 comment 是**唯一**用 `KeysCtx` 的服务：
    其它服务（`inbox`、`recommend-rank`、`video`）的失效都靠**可推导的确定键名**批量删，
    不需要扫 keyspace，所以这是本服务的选型问题而非仓库惯例。
    修法要么把 `sort/pn/ps` 收进一个 per-oid 的索引集合再按集合删，要么改 `SCAN` 游标。
    替身按前缀匹配复刻，因此键语义被测到、命令选型没被测到。
12. **举报不去重、不推进状态** ⛑：`comment_report` 无 `(rpid, reporter_mid)` 唯一约束，
    重复举报重复插行（`TestReportCommentReplayDuplicatesRow`）；举报也不会把评论转 `PENDING`。
13. **`CommentStats` 把待审/折叠算作可见** ⛑：`CountByTarget` 用 `state NOT IN (2, 5)`
    （`model/comment.go:211,217`），于是 `PENDING(4)`/`FOLDED(1)` 都进计数，
    而 `ListComments` 用同一条件的 `root = 0` 集；两个口径**内部一致但对外不同名同义**：
    `CommentStats.total` 含楼中楼回复、`ListComments.total` 只数根评论 ⇒
    客户端拿到「7 条评论」却只列出 3 条。定位：`model/comment.go:208-223`。
    另外 `TestCommentStatsCountsPendingAsVisible` 固定了「待审可见」这一行为。
14. **删根评论会留下孤儿回复**：`SoftDelete` 只改单行，不级联 `root`。
    删掉某条根评论后它的回复仍 `state` 可见：`ListComments` 已经空了、`root_total` 归 0，
    但 `CommentStats.total` 仍把孤儿回复计进去（用例里 1 条 → `total=1` 而列表 0 条），
    定位：`model/comment.go:79-100`；断言见 `TestDeleteCommentKeepsCountsConsistent`。
    修法要么级联软删 `root = ?`，要么 `total` 也改成 `root = 0` 口径（与缺口 13 同源）。
15. **`mtime` 被写成 `mid`**：`model/comment.go:83,88` 的
    `UPDATE comment SET state = 2, mtime = ?` 绑定的参数是 `mid` 而不是时间戳 ⇒
    修改时间变成用户 ID（一个 6 位数字 vs 10 位 Unix 秒）。
    本服务把这条 bug 原样复刻进替身并断言（`TestDeleteCommentByOwnerSoftDeletesAndInvalidatesCaches`），
    以便修法落地时用例立刻变红确认。
16. **`SoftDelete` 的 admin 分支不检查 `RowsAffected`**：`model/comment.go:80-85` 直接返回 `err`，
    影响 0 行也算成功。存在性靠 `Repository.DeleteComment` 的前置 `FindOne` 兜住，
    但那一读与更新之间有 TOCTOU 窗口。
17. **`Cache` 写侧吞错、读侧上抛**：`_ = r.cache.SetOne/InvalidateListByOid/DelStats/DelOne/SetList`
    全部丢弃错误（写侧宽容是对的），但 `GetList`/`GetStats` 失败直接让读接口 500，
    而不是降级回源（`TestListCommentsPropagatesCacheReadFailure` 固定了当前行为）。
    Redis 抖动会放大成评论列表不可用。
18. **`prefixReplies`/`keyReplies` 是死键**：`repository.go:21,35` 定义了 `cmt:rl:%d:%d:%d`，
    但 `ListReplies` 走的是裸 model（`repository.go:278-280`），楼中楼完全没有缓存
    （`wantCount "cache." 0` 断言）。热点楼中楼每次穿透到 MySQL。
19. **迁移 SQL 的 `state` 列注释与 proto 枚举编号不一致**：
    `deploy/migrations/comment/000001_create_comment.sql:14` 写的是
    「0 正常、1 审核中、2 删除、3 待审、4 屏蔽」，而 `rpc.CommentState` 是
    0 NORMAL / 1 FOLDED / 2 DELETED / 3 PINNED / 4 PENDING / 5 REJECTED。
    SQL 里的 `NOT IN (2,5)`、`state = 3` 是按 **proto** 编号写的，注释会误导后来者按注释取值。
    该文件在本轮可改范围之外，仅登记。
20. **`floor` 列无人写入也无人读取**：`Insert` 的列清单没有 `floor`（恒为默认 0），
    `CommentInfo` 也没有对应字段 ⇒ 「楼层」概念只有 DB 里一个空列。
    楼中楼归属实际靠 `root` 实现（`TestListRepliesReplyOfReplyStaysInSameFloor`）。
