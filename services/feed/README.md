# feed

动态发布和关注流投影服务，对应参考仓库的 `dynamic/feed`。

- **拥有数据**：`feed_outbox`（个人发件箱）、`feed_inbox`（关注收件箱投影）、`feed_pin`（置顶动态）、`feed_unread`（未读计数）。
- **提供能力**：领域服务推送新动态 `PushFeed`、用户拉取关注流 `PullFeed`、查询某用户主页动态 `ListUserFeed`、置顶/取消置顶 `PinFeed`/`UnpinFeed`、未读计数 `GetUnreadCount`/`ClearUnread`、删除动态 `DeleteFeed`。
- **依赖**：Redis（关注流 ZSet、未读计数器、粉丝集合）、MySQL、MQ（消费 `content.published.v1`、`engagement.action.v1`，本期暂不引入消费者）。
- **约束**：关注流是投影，不是用户关系事实；粉丝集合由 `social-graph` 关注事件写入 Redis Set，本服务只读不写。本期采用写扩散（PushFeed 时 ZADD 到所有粉丝 ZSet），不实现大 V 限流策略。不实现投币、广告、会员或小程序。
- **合并单元**：`community = social-graph + feed + engagement`。
- **部署**：gRPC 端口 `0.0.0.0:8092`，Etcd Key `feed.v1.rpc`。

## 关键判定口径（logic 单测已钉住）

- **写扩散没有上限**：`PushFeed` 对粉丝集合里的每个 mid 都写「ZSet 成员 + `feed_inbox` 投影行 + 未读 +1（缓存与 DB 两侧）」，没有大 V 限流、没有批量阈值、没有部分失败上报——四步的错误全部被吞（缺陷 D8/D9/D10）。
- **游标是 `ctime` 而不是 (ctime, id)**：翻页窗口是 `0 <= ctime <= cursor-1`（`internal/repository/repository.go:110-115`），同秒发布的动态跨页必然漏读（D11）。
- **未读是投影，不是即时统计**：只有 `PushFeed` 在推进它，`GetUnreadCount` 只做「缓存命中即返回 / miss 回表一次」，删除动态不回退（D17），`ClearUnread` 先清缓存后清 DB、DB 失败后旧值可复活（D16）。本期不引入新键空间。
- **缓存 miss 与 cache error 是两回事**：miss（键不存在）会回源 `feed_inbox` 并预热；error 一律原样上抛，不降级（D13）。空收件箱不写负缓存（D14）。
- **置顶目前是纯写侧功能**：读侧（`PullFeed`/`ListUserFeed`）完全不查 `feed:pin`，`FeedItem` 也没有置顶字段（D18）。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，8 个文件 `71/0`）

按链路分三组：守卫、写扩散、读侧与状态位。本包用例全是顶层 `func Test`（`子` 恒为 0），
表驱动靠用例内部循环而不是 `t.Run`，因此子用例数为 0 是口径差异不是缺测。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **守卫** | | | |
| `guards_logic_test.go` | 11 | 0 | 8 个 rpc 方法的入参守卫全部拒在触库/触缓存**之前**（`wantNoCall` 保证一次依赖调用都没发生），且错误就是 `model` 包里那个哨兵本身、没被换成别的语义；`PushFeed` 接受 `otype`/`action` 为 UNSPECIFIED（D3）；`ListUserFeed` 从不读 `mid`（D6）；`ps` 静默 clamp 成 20 而不是拒绝（D5）；`PullFeed` 的超大 cursor 不会放宽扫描范围 |
| **写扩散** | | | |
| `pushfeed_logic_test.go` | 9 | 0 | fan-out 的**顺序与条数**（每个粉丝恰好「ZSet → 投影表 → 缓存计数 → DB 计数」四步）；服务端补 `ctime`；粉丝集合「缺失」与「为空」两种姿态可区分；集合读失败与作者 ZSet 写失败被吞且仍返回成功（D8/D9）；**部分扇出失败不可见**（D10，3 个粉丝 → 计数 +3 而投影只 2 行）；投影写失败后的残留；**没有幂等键**（D1）；负 `ctime` 在作者主页不可见却在关注流可见（D4） |
| **读侧与状态位** | | | |
| `pullfeed_logic_test.go` | 11 | 0 | 正常翻页可续拉不重；**同秒动态被整段跳过**（D11）；`next_cursor`/`has_more` 只由「探针」决定，整页被删时退化成空列表 + `has_more=true` + `cursor=0`（D12）；ZSet 里剩一个幽灵成员就永不触发回源（D15）；缓存 miss 才回源 DB 并预热；缓存读错误不回退（D13）；空收件箱无负缓存、每次打 DB（D14）；回源时 DB 故障上抛；逐字段投影 |
| `listuserfeed_logic_test.go` | 5 | 0 | 读的是 `feed:outbox:{vmid}`，与关注流 `feed:inbox:{mid}` 两条键空间不串；**没有任何 DB 回源**（D7）——ZSet 丢失即空白；跳过已删除/缺失成员；游标口径与 PullFeed 共用同一段代码（D11/D12 同样适用）；cursor 守卫与错误透传 |
| `pin_logic_test.go` | 14 | 0 | PinFeed 的「读校验 → DB → 缓存」三步顺序与**每步失败到底留下了什么**；归属校验只存在于 PinFeed（UnpinFeed 不查主表）；重复置顶走 `ON DUPLICATE`（不报错、不加行，`ErrPinAlreadyExists` 是死代码）；`FindOne` 与 `feed_pin.Add` 之间被并发删除 ⇒ 孤儿置顶行（D19，用并发窗口钩子复现）；**置顶集合在 8 个 rpc 里没有任何读路径**（D18，`ListPins` 零调用点）；取消置顶后空集合丢键、缓存失败让重试不可能（D20）；置顶无数量上限 |
| `unread_logic_test.go` | 11 | 0 | 域结论：**未读数是一个投影，不是即时统计**——只有 PushFeed 的 INCRBY/IncrBy 双写在推进它，8 个 rpc 里没有重算入口；计数用「缓存值 vs 表里真实行数」两个数并列断言，不用「永远返回 0」糊过去；`GetUnreadCount` 的 cache-miss ≠ cache-error（错误原样抛出，不悄悄降级成 0）；`ClearUnread` 先缓存后 DB 的顺序与 DB 失败后的复活窗口（D16）；缓存失败时 DB 不动；全新用户清一次建零值行；未读只随 fan-out 增长、不随 Pull 变化 |
| `deletefeed_logic_test.go` | 10 | 0 | 一条动态在系统里有 4 份拷贝（主表行、作者 outbox ZSet、粉丝 inbox ZSet、投影行），删除必须四份都动，用完整调用序列 + 逐份读回锁死；归属与不存在的两种拒绝；粉丝集合缺失与为空；两处 `_ =` 吞掉 ZSet 清理结果、留幽灵成员（D21）；部分粉丝清理失败；投影删除失败后的**可重跑**那一支（D22，替身按 matched rows 建模）；与 fan-out、与取关的并发窗口；删除不回退未读也不清置顶（D17） |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存缓存 + 4 个 model 替身 + 有序 callLog + 「第 N 次发作」错误注入 + 并发窗口钩子，见第 4 组 |

### 2. 其他层

- `internal/repository`（1 文件 `4/0`）：`keys_logic_test.go` 是白盒键空间门禁，直接锁源文件里的
  常量与调用形态——5 个键空间的拼接结果（`feed:inbox`/`feed:outbox`/`feed:pin`/`feed:unread`/
  `feed:followers`）、键空间互不冲突、**所有 feed 键无 TTL**、粉丝集合本服务只读。
  它补的正是 logic 用例看不见的那一半：注入缝在 `Cacher` 之下，logic 只看到 `(mid, feedID)` 级身份。
- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用真实 `conf.Load` 加载 `etc` 示例配置
  （`conflict key redis` 这类「能编译、启动即挂」只在真实加载时暴露）。
- `model/`（`feedmodel.go`、`errors.go`、`now.go`）：**无离线单测**。SQL 文本、列名、索引命中
  只能由真实 MySQL 集成测试证明。
- `internal/svc/`、`internal/server/`：**无离线单测**（goctl 生成壳与装配）。
- 本服务没有 `internal/consumer` 目录（`content.published.v1`、`engagement.action.v1`
  本期不接消费者），因此「事件被消费后投影确实前进」不在离线覆盖内。
- 合计 **76 个顶层用例、1 个子用例**（logic `71/0` + repository `4/0` + config `1/1`）；**0 个 `t.Skip`**。
- **未实现桩：无**——8 个方法全部是已实现用例，proto 里没有任何方法返回 `codes.Unimplemented`，
  `internal/logic`/`internal/repository` 也没有 `ErrNotImplemented` 字样（用例文件头已核实）。

### 3. 构造器级覆盖

`8/8`：探针取 `internal/logic` 全部 `New*Logic(`（8 个，与 RPC 方法一一对应：PushFeed / PullFeed /
ListUserFeed / PinFeed / UnpinFeed / GetUnreadCount / ClearUnread / DeleteFeed），
逐个在 `*_test.go` 里查引用，`gaps:` 为空，即每个方法的 logic 都从构造器进入被打过。

### 4. 替身层与断言口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，生产构造走 `repository.New`
（真 Redis + 真 MySQL），logic 包无处塞替身。本包用例统一用
`repository.NewWithDeps(内存缓存, nil conn, 4 个内存 model)` 组装**真实的 Repository**，
只把它的 5 个依赖换成替身，于是「写扩散 fan-out、ZSet 游标翻页、缓存 miss 回源与预热、
未读计数器与 DB 双写、置顶集合读写配对、删除时的收件箱清理」整条判定链都在被测路径上。

五条替身纪律（`internal/logic/fakes_test.go:22-37`）：读时值拷贝；写侧照抄生产 SQL 的语义
（列集合、`ON DUPLICATE` 分支、WHERE 片段、`GREATEST` 不为负、`ORDER BY`/`LIMIT`、
`FindOne`/`FindMany` 不过滤 `state`、`DeleteByFeedID` 不看 `RowsAffected`），
**未复刻的分支一律返回 `errUnexpectedDependency` 而不是假成功**；有序 callLog
（`<表|cache>.<方法>:<键>`，既数次数也断顺序；键只放调用方可见的入参，替身自增的主键不进键，
新号正确性一律「读回库里那一行」断言）；错误注入可指定第几次调用发作
（`failOn(method, nth, err)`，「只有第 2 个粉丝写失败」这类爆炸半径靠它复现）；
并发窗口用钩子（`raceBefore`/`checkRaces`，钩子没触发就判失败，避免用例前提空转）。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端；
  `internal/server`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内。
- 替身复刻的是 model 层 SQL 的**语义**，不证明 SQL、列名与索引命中；
  那部分由 `deploy/migrations/feed/000001_create_feed_tables.sql` 与人工评审承担。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有隔离实例
  （`127.0.0.1:3399`）的复验结论，本服务也没有 model ↔ DDL 的静态门禁；
  `deploy/migrations/README.md:107` 的 `applied` 口径只是「`up` + `status` 跑通」（见同文件 `:21`），
  不覆盖逐列对账。
- 驱动语义差异不模拟：feed 的 DSN 未开 `clientFoundRows`，真实 `RowsAffected` 是 **changed rows**，
  所以「同一秒内重复软删同一条动态」真库会得 0 行 → `ErrFeedNotFound`；替身按 **matched rows**
  实现，因此 D22 只有「可重跑」那一支被用例锁住（另一支只在缺陷表里登记）。
- `Cache` 真身的实现细节分支不在断言范围：`feed:followers` 集合成员 `ParseInt` 跳过非数字
  （`repository.go:80-85`）从 logic 侧不可达；`GetUnread` 用 `err == redis.Nil` 区分 miss，
  真实总返回该哨兵这一点不由用例证明。`feed:followers:{mid}` 的只读性靠 `Cacher` 契约
  不含写方法在**编译期**锁死，不需要运行时断言。
- `model.Insert` 的自增主键在替身里按「表内 max+1」分配（从 101 起只为可读性），
  真实 `LastInsertId` 行为与唯一约束冲突只能由 MySQL 证明（D1 的幂等缺口正因此无法在内存里修）。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/feed/...
gofmt -l services/feed    # 必须为空
go vet ./services/feed/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 已知缺陷（本轮登记，均已在用例里钉住当前行为）

修法语义唯一确定的已直接改生产代码（本轮只加了注入缝：`Cacher` 接口 + `New`/`NewWithDeps` 拆分，见 `internal/repository/repository.go:252-337`）；其余按现状钉住：

| # | 位置 | 后果 |
| --- | --- | --- |
| D1 | `deploy/migrations/feed/000001_create_feed_tables.sql:24-25`（`feed_outbox` 只有 `idx_mid_ctime`/`idx_forward`，没有幂等键）、`model/feedmodel.go:50-70` | 上游重试一次就多一条动态，并连带向全部粉丝再 fan-out 一份；`PushFeedReq` 已带 `source`/`operator` 却没有可用于去重的唯一约束 |
| D2 | `internal/logic/pushfeedlogic.go:48`（`if _, err :=`）+ `rpc/feed.proto` `PushFeedReply = EmptyReply` | 调用方拿不到 `feed_id`，D1 的重试没有可比对标识，事后也无法按 ID 补偿 |
| D3 | `internal/logic/pushfeedlogic.go:31-36` 只校验 mid/oid | `otype=UNSPECIFIED`、`action=UNSPECIFIED` 照样入库并进收件箱，下游按 otype 分发会拿到未知类型 |
| D4 | 同上（无 ctime 范围校验） | `ctime<0` 时作者发件箱 ZSet 收得到（负 score 合法）、粉丝收件箱被 `repository.go:115` 的 `min=0` 挡住 → 自己看得见、粉丝看不见 |
| D5 | `internal/logic/helpers.go` `normalizePs` + `repository.go:386-388` | `ps=51` 静默变 20（不是夹到 50），`model/errors.go:12` `ErrPsTooLarge` 是死代码（Grep 核实无返回点），客户端分不清「被夹」与「填错」 |
| D6 | `internal/logic/listuserfeedlogic.go:29-38` | `ListUserFeedReq.mid` 从未被读取，proto 注释承诺的「关注/粉丝/仅自己」可见性判断不存在，可读到任何人主页全部动态 |
| D7 | `repository.go:417-427`（对比 PullFeed 的 `395-412`） | 作者主页无 DB 回源：`feed:outbox:{mid}` 丢失即空白，即使 `feed_outbox` 行都在；与 D9 叠加即静默丢数据 |
| D8 | `repository.go:359-362`（`return feedID, nil`） | 粉丝集合读失败被吞且返回成功 → 整批粉丝收不到动态，调用方无从感知，也没有补偿入口 |
| D9 | `repository.go:354-357`（`_ = err`） | 作者 ZSet 写失败被吞 → 主页从此少这一条（结合 D7 无法回源） |
| D10 | `repository.go:368-379`（fan-out 四条写全被忽略） | 部分写失败被吞且返回成功，未读计数与收件箱投影可与 ZSet 成员数各自漂移（用例：3 个粉丝 → 计数 +3、投影只 2 行） |
| D11 | `repository.go:110-115` + `444-452` | 游标取「最后一条保留项的 ctime」+ 窗口 `ctime <= cursor-1` → 同 ctime 的多条动态跨页必然漏读 |
| D12 | `repository.go:434` + `444-456` | 整页都被删除/过滤时返回 `has_more=true` 而 `next_cursor=0`：照提示翻页会死循环重复第一页；手工带 cursor 才能前进，且窗口「比最老可见项更老」，比它更新的已删除项再也查不到 |
| D13 | `repository.go:391-394` | 缓存读错误直接上抛，不降级到 `feed_inbox` 投影（同文件有回源支路却没接错误）：Redis 抖动即关注流不可用 |
| D14 | `repository.go:401-403` | 空收件箱不写负缓存，每次关注流请求都打一次 DB |
| D15 | `repository.go:395`（判据是 `len(ids)==0`） | ZSet 里只要还剩一个幽灵成员就永不触发回源，缓存无法自愈；每次拉流还要多回一次表 |
| D16 | `repository.go:537-542`（先清缓存、后清 DB） | `feed_unread.Clear` 失败时缓存已归零而 DB 未动，键一旦被逐出旧未读数原封复活并被重新预热 |
| D17 | `repository.go:545-560` | 删除动态不回退未读（红点虚高且无重算入口）、不清 `feed_pin`/`feed:pin`（留下指向已删除动态的置顶行，而 `PinFeed` 此时还会拒绝，唯一出口是不查主表的 `UnpinFeed`） |
| D18 | `repository.go:487-517`、`rpc/feed.proto:37-48` | 置顶是纯写侧功能：`Repository.ListPins`/`Repository.Ping` 在 logic/server 里零调用点（Grep 核实），两个读接口都不读 `feed:pin`，`FeedItem` 无置顶字段 → 用户置顶后时间线毫无变化 |
| D19 | `repository.go:461-477`（归属校验→写库→写缓存三步无事务） | 校验通过后动态被并发删除，仍会写入置顶行，产生孤儿置顶（用例用并发窗口钩子复现） |
| D20 | `repository.go:480-485`（DB 先、缓存后） | `RemPin` 失败时 DB 已软删，重试只能拿到 `ErrPinNotFound`，`feed:pin:{mid}` 永久留一个已取消的成员 |
| D21 | `repository.go:551`、`553-558`（`_ =` 与 `if err == nil && hit`） | 粉丝集合读失败或 ZREM 失败均被吞且返回成功，全部/部分粉丝的 ZSet 幽灵成员长期残留（结合 D15 无人清理） |
| D22 | `model/feedmodel.go:104-119`（`WHERE id AND mid`，无 state 条件） | 重复删除是否成功取决于驱动返回 matched 还是 changed rows（同秒 0 行 → `ErrFeedNotFound`；跨秒 mtime 变了 → 成功），删除的幂等语义挂在秒边界上；`feed_inbox` 投影行只有这一条清理路径，同秒重试就再也修不掉 |
| D23 | `internal/logic/pushfeedlogic.go:37-47` + `rpc/feed.proto:67-69` | `source`/`operator`/`real_ip` 三个溯源字段被 logic 丢弃，`feed_outbox` 也没有对应列，D1 的幂等与事后台账追溯都缺依据 |

另有死代码：`model/errors.go:15` `ErrPinAlreadyExists`（重复置顶走 `ON DUPLICATE`，永远不会返回）。
