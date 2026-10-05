# social-graph

用户关系链服务，对应参考仓库 `openbilibili-go-common/app/service/main/relation` 领域。
本服务是「关系」域的数据所有者（见 [AGENTS.md §5](../../AGENTS.md)），只持有关系事实、
关系计数快照、特别关注与黑名单四类数据。

> **架构约束**：领域微服务只暴露 gRPC，不提供 HTTP（AGENTS.md §3/§4）。
> 面向端的 `/social/*` 由 `gateway/app` 聚合（见下方「谁在用这个服务」）。

## 职责

- **关注关系**：`Follow`/`Unfollow`（幂等，靠 `(mid, follower_mid)` 唯一索引）、
  单点与批量「是否关注」判定、关注/粉丝分页列表。
- **计数快照**：`Stat` 返回关注数/粉丝数；Redis 计数器优先，miss 时回读 `relation_stat`。
- **名单（拉黑）**：`AddBlack`（拉黑时自动取关本人对对方的关注）、`DelBlack`、
  `IsBlacked`、`ListBlacks`。
- **特别关注**：`AddSpecial`（前置条件是已关注）、`DelSpecial`。
- **不做的事**：不生成动态/收件箱（那是 `feed`）、不判定账号存在性与状态（调用方负责，
  本服务不依赖 `account` RPC）、不写推荐结果、不持有任何用户可变成品资料、
  不实现投币/支付/会员等商业化能力。

## 数据所有权边界

对齐 AGENTS.md §5「关系/动态/互动 → `social-graph` / `feed` / `engagement`，
**禁止的做法：同步更新所有计数和推荐结果**」：

- **拥有 4 张表**：`relation_follow`、`relation_black`、`relation_special`、`relation_stat`
  （库 `go_video_social_graph`）。其他服务只能走本服务 gRPC
  （etcd 注册 key `socialgraph.v1.rpc`），禁止直连这些表或本服务的 `sg:*` Redis key。
- **计数确实是「不同步全量」**：`relation_stat` 只是展示投影，
  迁移脚本自己就这么写（`deploy/migrations/social-graph/000001_create_relation_tables.sql:51`
  「计数为展示投影，可由关系表重算（不作为唯一事实源）」）。列表与点查一律回关系表，
  不读计数（`internal/repository/repository.go:324-336`、`internal/logic/listfollowinglogic.go:35`）。
- **与约束不符的一处**：AGENTS.md §5 要求「事务内写业务数据和 Outbox 记录」，
  而本服务**根本没有事务、也没有 Outbox**。`Repository` 不持有 `sqlx.SqlConn`
  （`internal/repository/repository.go:205-213`，注释明确写了「本服务全部是单表单语句写，
  没有 TransactCtx 事务」），`Follow` 是「1 条关系 UPSERT + 4 次 Redis 写 + 2 条计数 UPDATE」
  共 7 步无原子性的写入（`internal/repository/repository.go:245-274`）。
  详见「已知缺口 3」。
- **不复制主资料**：`RelationItem` 只回 `mid`/`ctime`/`attr` 三个字段
  （`rpc/socialgraph.proto:79-83`），昵称头像由网关向 user-profile 聚合。

## gRPC API

package `socialgraph.v1`，端口 8091，etcd 注册 key `socialgraph.v1.rpc`，共 14 个方法。
分组轴取自 `rpc/socialgraph.proto` 自身的四段注释（`--- 关注 ---` / `--- 计数 ---` /
`--- 黑名单 ---` / `--- 特别关注 ---`），并按读写拆开：

| 分组 | 方法 | 幂等/守卫口径 |
|---|---|---|
| 关注写侧 | `Follow` `Unfollow` | 幂等：重复关注/取关不重复增减计数（`followlogic.go:47`、`unfollowlogic.go:38`）；双侧 `mid>0` + 自操作守卫（`followlogic.go:30-38`） |
| 关注点查 | `IsFollowing` `IsFollowedBatch` | 单点走缓存、批量直连 DB（`repository.go:300-326`）；批量上限 100、空列表短路（`isfollowedbatchlogic.go:32-37`） |
| 批量关系位掩码 | `RichRelations` | 2026-10-04 新增。四条 `IN (?)` 存在性查询合成 `map[mid]attr`（follow 双向 + black + special，`repository.go:451`）；上限 100、空 mids 短路在上限判定之前（`richrelationslogic.go:32-37`）；**不读 `relation_follow.attr` 列**、不碰缓存与计数快照；任一查库失败整体报错，不回半张表 |
| 关注列表投影 | `ListFollowing` `ListFollower` | `ps>50` 直接拒（`listfollowinglogic.go:32-34`），`pn<1`/`ps<1` 在 model 侧钳制 |
| 计数 | `Stat` | 计数器双命中才免回库；`whisper` 本期恒 0（`statlogic.go:37`） |
| 名单写侧 | `AddBlack` `DelBlack` | 拉黑自动取关本人→对方一条（`repository.go:379`）；`AddBlack` 有自操作守卫、`DelBlack` 没有（`delblacklogic.go:28-38`） |
| 名单点查 | `IsBlacked` | **不走缓存**，每次一条 SELECT（`repository.go:396-402`）；结论复用 `RelationReply.following` 字段（`isblackedlogic.go:40`） |
| 名单列表投影 | `ListBlacks` | 只回 `black_mid`/`ctime`/`attr`（`listblackslogic.go:41-47`） |
| 特别关注 | `AddSpecial` `DelSpecial` | 前置「必先关注」，未关注返回 `ErrSpecialNeedFollow`（`repository.go:417-419`） |

**仍没有的方法**（不要按参考仓库同名 RPC 来找）：`Attentions`、`Blacks`、`GetFollowAll`
不在本契约里——account 侧需要的是「整份关注列表」与「黑名单集合」，本服务只提供分页
`ListFollowing`/`ListBlacks`，切片与翻页由调用方适配器完成。
`RichRelations` 已于 2026-10-04 落地（此前只有枚举没有 RPC），位口径与修订记录写在
`rpc/socialgraph.proto:16-34` 的 `RelationAttr` 注释里：attr 是**按位或的掩码**
（`1 FOLLOWING / 2 FOLLOWER / 4 BLACKED / 8 SPECIAL`，`3 MUTUAL` 是 1|2 的派生值），
判互关要写 `attr&1!=0 && attr&2!=0` 而不是 `attr==3`（同时被拉黑时是 7）。
列表接口仍只在 attr 上填本列表那一位（见「已知缺口 5」）。

### 谁在用这个服务

- **终端面（gateway/app）**：12 条路由，全部在 `/social` 前缀下。
  `POST /social/follow|/unfollow`、`GET /social/is_following|/following|/follower|/stat`
  （`gateway/app/api/app.api:1215-1242`）；
  `POST /social/black/add|/black/del|/special/add|/special/del`、
  `GET /social/black/check|/black/list`（`gateway/app/api/app.api:2741-2768`）。
  投影在 `gateway/app/internal/logic/conv_social.go`。
  **两个 `@server` 块都没有 middleware**，且 `mid` 是客户端 form 参数
  （`gateway/app/api/app.api:1142-1146`）——见「已知缺口 7」。
- **运营面（gateway/admin）**：**零引用**。`grep -rn "socialgraph" gateway/admin/` 无命中，
  本服务目前不进后台。
- **其他领域服务**：`account` 声明了 `SocialGraphClient` 接口
  （`services/account/internal/repository/repository.go:65-79`）并被 6 个读方法调用，
  但适配器至今没接线（见「已知缺口 8」）。

## 数据模型与迁移

单文件迁移 `deploy/migrations/social-graph/000001_create_relation_tables.sql`
（`deploy/migrations/README.md:129` 标记为 applied），4 张表全部软删除（`state=1`），
行保留用于幂等重放：

| 表 | 关键列 | 索引 | 迁移行号 |
|---|---|---|---|
| `relation_follow` | `mid`（关注发起方）、`follower_mid`（**被关注者**）、`attr`、`state`、`ctime`、`mtime` | `PRIMARY(id)`、`UNIQUE uniq_mid_follower(mid, follower_mid)`、`idx_mid_state_ctime`、`idx_follower_state_ctime` | `:9-21` |
| `relation_black` | `mid`、`black_mid`、`state` | `PRIMARY(id)`、`UNIQUE uniq_mid_black(mid, black_mid)`、`idx_black_mid_state` | `:25-35` |
| `relation_special` | `mid`、`special_mid`、`state` | `PRIMARY(id)`、`UNIQUE uniq_mid_special(mid, special_mid)` | `:39-48` |
| `relation_stat` | `mid`、`following`、`follower`、`whisper`（均 `BIGINT`，**有符号**） | `PRIMARY(id)`、`UNIQUE uniq_mid(mid)` | `:53-63` |

回滚逐表 `DROP TABLE IF EXISTS`，写在各表注释里。

2026-10-04 新增的 `RichRelations` **没有带迁移**：四条批量查询全部落在已有索引上——
`follow.FindFollowings` 走 `uniq_mid_follower(mid, follower_mid)`、
`follow.FindFollowers` 走 `idx_follower_state_ctime(follower_mid, state, ctime)` 的前缀、
`black.FindBlacks` 走 `uniq_mid_black(mid, black_mid)`、`special.FindSpecials` 走
`uniq_mid_special(mid, special_mid)`，谓词都是「等值前缀 + `state` + `IN (?)`」，
上限 100 由 logic 挡住（`richrelationslogic.go:35`），所以不需要新索引或新列。

命名口径要注意：`follower_mid` 在本服务是**被关注者**，不是粉丝
（`model/relation_follow.go:12-18`）。因此 `ListFollower` 投影的是 `r.Mid`（粉丝本人），
`ListFollowing` 投影的是 `r.FollowerMid`（被关注的人）——方向没写反，
用例 `TestFollowingAndFollowerDirectionsAreNotSwapped` 钉住了这一条。

Redis key（`internal/repository/repository.go:29-38`）：
`sg:fs:<mid>` 关注集合 SET、`sg:fr:<mid>` 粉丝时间轴 ZSET、
`sg:fc:<mid>`/`sg:rc:<mid>` 关注数/粉丝数计数器，回刷 TTL 常量 `cacheTTLStat = 3600`。

## 配置与运行

- 示例配置：`etc/socialgraph.v1.yaml`。`Name`/`Etcd.Key` 都是 `socialgraph.v1.rpc`，
  `ListenOn: 0.0.0.0:8091`。
- 键名注意：业务缓存必须写 `CacheRedis` 而不是 `Redis`——`zrpc.RpcServerConf` 内嵌了同名
  字段，写 `Redis` 会让配置加载直接失败（`internal/config/config.go:14-15` 有注释，
  并由 `internal/config/config_load_test.go:15` 的用例守着）。
- 环境变量：`DataSource`（MySQL DSN，`etc/socialgraph.v1.yaml:14`）、
  `CacheRedis.Host`（`:9-11`）；生产值经配置中心/Secret 注入，仓库里只有本地示例值。
- 该 DSN **未开 `clientFoundRows`**（同 `etc` 行），依赖 `RowsAffected` 的分支要注意
  changed-rows 语义。
- 无 MQ、无 consumer 目录（`internal/` 只有 `config/ logic/ repository/ server/ svc/`）、
  无定时任务；本服务不生产也不消费任何领域事件。
- 健康检查：纯 RPC 服务，**没有 HTTP 端口**，用 gRPC 探针
  （`grpc_health_probe -addr=127.0.0.1:8091`）。AGENTS.md §4 的 `/api/healthz` 只属于网关。

```powershell
# 生成（仓库根目录；修改 rpc/socialgraph.proto 后必须执行）
./scripts/gen.ps1 -Service social-graph

# 运行
go run ./services/social-graph -f services/social-graph/etc/socialgraph.v1.yaml
```

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，15 个文件 `109/46`）

按域分四组：关注与取关、特别关注、黑名单、读侧查询与计数。
14 个方法各有一张 `Test<方法>Guards` 表驱动守卫用例。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **关注与取关** | | | |
| `followlogic_test.go` | 13 | 3 | 首次关注写关系行并把两条计数**各 +1 恰好一次**（期望轨迹 9 条、delta 方向逐字段核对）；重复关注不二次 INSERT 也不二次 +1；取关后重新关注恢复行但**不刷 ctime**；只查「操作方是否拉黑了对方」这一个方向、反向不拦（缺口 1）；软删的黑名单行不挡关注；黑名单查询失败上抛；`statMd.Incr` 失败时关系行已提交（缺口 3 的半写状态）；计数快照缺失时懒建；4 次缓存写失败全部被吞仍返回成功 |
| `unfollowlogic_test.go` | 9 | 3 | 取关是软删且保留行 + 两条计数各 -1 恰好一次；无记录时幂等成功；重复取关**不二次 -1**；计数写失败时关系与缓存已先行（缺口 3）；`relation_special` 位残留成孤儿（缺口 2）；快照缺失时建零值行；缓存写失败被吞 |
| **特别关注** | | | |
| `addspeciallogic_test.go` | 10 | 3 | 「必先关注」的两条判定来源（缓存 hit 只写 special 行、cold 读穿并回填）；幂等不二次落库；软删恢复不刷 ctime；**完全不看黑名单**（缺口 1）；取关后残留旧标记时 AddSpecial 拒绝但不清理（缺口 2）；回填写失败被吞；失败传播 |
| `delspeciallogic_test.go` | 6 | 4 | 守卫且**无自发保护**；软删位保留 ctime 只刷 mtime；无行/已取消两种幂等口径；清位绝不影响关注与计数；它是唯一能清掉「取关后孤儿位」的入口且**不校验关注状态**（缺口 2）；失败传播 |
| **黑名单** | | | |
| `addblacklogic_test.go` | 10 | 3 | 写黑名单行 + **只删发起方向的关注**（完整十步有序轨迹）；重复拉黑不重写行也不二次 -1；软删恢复不刷 ctime；目标本来没关注时只写黑名单行；被拉黑方仍能反向关注（缺口 1）；自动取关那一步失败时黑名单行已提交；缓存写失败被吞、`Del` 失败留下「仍显示已关注」的脏读窗口 |
| `delblacklogic_test.go` | 7 | 4 | 守卫且**无自发保护**；软删保留 ctime、刷新 mtime；取消拉黑**绝不复活**当初自动取关的那条关注；取消后可以重新关注（与 Follow 的配对口径）；无行/已取消两种姿态都是幂等成功；失败传播 |
| **读侧查询与计数** | | | |
| `isfollowinglogic_test.go` | 8 | 5 | 守卫（无自发保护）；缓存 hit 的 true/false 两条路径都跳过 DB；miss 读穿并回填；**负回填不建关注集合键**；查询不查黑名单；失败传播 |
| `isfollowedbatchlogic_test.go` | 6 | 3 | 守卫含批量上限与空列表的**先后次序**；空 owners 在上限判定之前短路；逐键投影；owners 去重；不校验 owner 取值；本域口径「批量只走 DB、不碰缓存」；失败传播 |
| `richrelationslogic_test.go` | 7 | 4 | 2026-10-04 新增的 `RichRelations`（四位关系掩码批量读）：守卫顺序是 owner>0 → 空 mids 短路 → >100 拒答；空 mids 在「超限」判定之前返回空 map 且零调用；恰好 100 个放行且**四条查询各一次**（有序轨迹逐条核对 `follow.FindFollowings`→`follow.FindFollowers`→`black.FindBlacks`→`special.FindSpecials`）；**四个位是独立事实**：用例布出 1/2/3/6/8/15 六种组合与「软删行不计入」的对照，互关不吞拉黑位、特别关注不等于关注位；**位号与 proto 枚举逐字对齐**（`attr` 里出现的是 `RELATION_ATTR_*` 常量值而不是本地字面量）；mids 去重；不校验 mid 取值（0/负数原样进 IN 列表）；四条查询**任一条失败即整体报错并回 nil**，且失败之后的查询不再发出（四条子用例各钉一条截断轨迹）；全程不碰缓存与 stat 表 |
| `isblackedlogic_test.go` | 6 | 1 | 守卫（无自发保护）；答的是「mid 是否拉黑了 owner」这一**不对称**方向；软删行不可见；**无缓存**，重复调用每次打库；失败传播 |
| `statlogic_test.go` | 8 | 3 | 守卫；两个计数器都 hit 时不查库；任一 miss 会连带把 hit 那一路的值一起丢掉（现状口径）；快照缺失时不回填；**悄悄关注位恒为 0**；回填写失败被吞；能看到 Follow 造成的增量；失败传播 |
| `listfollowinglogic_test.go` | 8 | 4 | 守卫含「只挡上界不挡下界」的一边倒钳制；三列逐字段投影；排序口径是 `ctime DESC` 而非 `mtime`；分页无重叠且并集等于全集；空结果只发 COUNT 不发第二条查询；软删/他人行不可见；`attr` 恒为硬编码值，忽略特别关注与互相关；失败传播 |
| `listfollowerlogic_test.go` | 5 | 3 | 粉丝行投影取的是 `mid` 列（关注发起方）而不是被关注者；`following`/`follower` **方向没写反**的交叉对账；排序与分页钳制；软删不可见；失败传播 |
| `listblackslogic_test.go` | 6 | 3 | 投影取 `black_mid` 列（不是发起方 mid）；`ctime DESC`；分页钳制与无重叠；软删行与「别人拉黑我」的反向行不可见、黑名单与关注表互不串台；空结果只发 COUNT；失败传播 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存缓存 + follow/black/special/stat 四个 model 替身 + 有序轨迹与断言助手，见第 4 组 |

### 2. 其他层

- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用真实 `conf.Load` 加载 `etc` 示例配置，
  守住「业务缓存必须写 `CacheRedis` 而不是 `Redis`」这条启动即挂的坑（见「配置与运行」）。
- `model/`（`relation_follow.go`、`relation_black.go`、`relation_special.go`、`relation_stat.go`、
  `errors.go`、`now.go`）：**0 个测试文件，无离线单测**。
- `internal/repository/`、`internal/svc/`、`internal/server/`：**无离线单测**
  （logic 用例经 `repository.NewWithDeps` 组装真实 Repository 间接打到装配路径，
  但 `*Cache` 真身与 Redis 命令没有直接用例；`internal/server` 是 goctl 生成壳）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（不生产也不消费领域事件，见「配置与运行」）。
- 合计 **110 个顶层用例、47 个子用例**（logic `109/46` + config `1/1`）；**0 个 `t.Skip`**。

### 3. 构造器级覆盖

`14/14`：探针取 `internal/logic` 全部 `New*Logic(`（14 个，与 `rpc/socialgraph.proto` 的 14 个方法
一一对应：Follow / Unfollow / AddSpecial / DelSpecial / AddBlack / DelBlack / IsFollowing /
IsFollowedBatch / RichRelations / IsBlacked / Stat / ListFollowing / ListFollower / ListBlacks），
逐个在 `*_test.go` 里查引用，`gaps:` 为空，即每个方法的 logic 都从构造器进入被打过。

### 4. 替身层与断言口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，生产构造走 `repository.New`
（真 Redis + 真 MySQL），测试无处塞替身。本包用例统一用
`repository.NewWithDeps(内存缓存, 内存 followMd/blackMd/statMd/specialMd)` 组装**真实的 Repository**，
只把它的 5 个依赖换成替身，于是「幂等判定 → 计数增量方向」「缓存读穿/回填」「计数器 miss 回刷」
整条判定链都在被测路径上，而不是把 Repository 也 mock 掉。

断言集中在三件事：守卫拒绝时**一次依赖都不许碰**、投影逐字段（种子值互不相同）、
副作用序列**按顺序**（`callLog` 记 `<pkg>.<method>:<key>`）——
关系服务要紧的结论正是「第二次关注有没有真的再发 INSERT」「取关时 -1 落在谁的计数上」
「拉黑时先写黑名单还是先取关」，见 `internal/logic/fakes_test.go:12-27`。

`fakes_test.go` 的四条替身纪律：读时值拷贝；副作用按顺序记录（断序列而不是只断次数）；
错误注入按**语句**粒度（`failWith("UpsertSelect", err)`）——本服务每个 Upsert/Delete 都是
「SELECT 旧状态 + 条件写」两条语句，只给一个开关就分不出「读了没写」和「写了半截」；
布数据走静默写入路径（`seedFollow`/`seedStat`/`cache.warmFollowing`），轨迹从 0 数起。

复刻的 SQL 语义：三张关系表都以迁移里的 `UNIQUE KEY (mid, …)` 建键；Upsert 复刻
「先 SELECT state，oldState==newState 直接返回不发 INSERT，否则
`INSERT … ON DUPLICATE KEY UPDATE state,mtime`（**不含 ctime**）」；Delete 复刻
「SELECT → 无行返回 -1 → 已是 1 不再 UPDATE → 否则 UPDATE state=1」；列表复刻
「`pn<1→1`、`ps∉[1,50]→20`、COUNT 为 0 时不发第二条查询、`ORDER BY ctime DESC LIMIT/OFFSET`」；
Redis 侧复刻 SISMEMBER+EXISTS 的 hit 口径、SREM 清空即删 key、INCRBY 建键、SETEX 覆盖。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ，也不起 gRPC 服务端；`internal/server`、`rpc/*.pb.go`
  等 goctl 生成壳不在单测范围内。
- `model/` **0 个测试文件**。替身只复刻 `model` 层 SQL 的**语义**（WHERE 过滤、
  `ORDER BY ctime DESC`、`state=0`、`LIMIT/OFFSET` 钳制、唯一键、`RowsAffected` 口径），
  **不证明 SQL 与列名本身**；仓库里也没有 social-graph 的迁移↔model 列级对账门禁。
- 迁移脚本的执行状态登记在 `deploy/migrations/README.md:129`（`applied`，
  口径见同文件 `:21`：隔离实例 `127.0.0.1:3399` 上 `up` + `status` 跑通）；
  **但这不等于 model ↔ DDL 的列级对账已在目标实例复验**——本服务既没有列级静态门禁，
  离线用例也不连库，`RowsAffected` 的 matched/changed 语义仍取决于 DSN 是否开 `clientFoundRows`
  （见「配置与运行」）。
- 真实 `*Cache`（`SismemberCtx`/`ExistsCtx`/`IncrbyCtx`/`SetexCtx`/`ZaddCtx` 与其 TTL）
  没有被测，所以 3600s TTL、ZSet member 的十进制编码、`parseInt64` 遇脏值的报错路径
  都不在断言范围内（`internal/logic/fakes_test.go:29-31` 已自陈）。
- 唯一索引冲突由 MySQL 保证，替身按二元组直接建键，**无法证明约束存在**。
- 并发下「SELECT 旧状态 → 条件写」的竞态没有用例，因为替身是单线程的；
  下方缺口 3 的半写状态只能靠注入故障断言残留数据。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/social-graph/...
gofmt -l services/social-graph    # 必须为空
go vet ./services/social-graph/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 已知缺口

以下条目全部由本轮实际 Read/Grep 确认，每条给现状、影响、收严位置和 `file:line`。
标注「用例」的，改生产代码前请连同断言一起改。

1. **黑名单只挡一个方向，拉黑不是双向可见性约束**。`Follow` 只查
   `IsBlacked(in.Mid, in.FollowerMid)`，即「**操作方**是否拉黑了对方」
   （`internal/logic/followlogic.go:39-46`），从不查反方向；`AddBlack` 也只删
   `mid→blackMid` 一条关注，注释里明确写了不主动删反向
   （`internal/repository/repository.go:377-379`）。
   于是 **B 把 A 拉黑之后，A 仍然能 Follow B、还能 AddSpecial B**，
   `relation_follow` 里就此躺着一条被拉黑方单向关注的记录。
   影响：拉黑语义在写入侧不成立，`feed`/`comment` 若按「双边不可见」理解关系就会错。
   若要收严：`followlogic.go:39` 之后补 `IsBlacked(in.FollowerMid, in.Mid)`，
   `addspeciallogic.go:38` 之前补同样两向检查。
   用例：`TestFollowIgnoresReverseBlacklist`、`TestAddBlackDoesNotBlockReverseFollow`、
   `TestAddSpecialIgnoresBlacklist`。
2. **`Unfollow` 不清 `relation_special`，会留下孤儿特别关注标记**。
   `Repository.Unfollow` 只动 follow 表和计数（`internal/repository/repository.go:277-297`），
   从不查/删 `relation_special`；`AddSpecial` 的前置条件「必先关注」因此在取关后失效，
   而 `DelSpecial` 又不校验关注关系、能把孤儿标记清掉（`repository.go:429-435`）。
   影响：读侧会把「未关注却特别关注」当成有效状态发出去。2026-10-04 的 `RichRelations`
   让这个形态第一次变得**可读**：这种行返回 `attr=8`（只有 SPECIAL 位、没有 FOLLOWING 位），
   proto 注释据此明确写了「特别关注不保证 FOLLOWING 位为真，要两者都判」
   （`rpc/socialgraph.proto:96-98`），用例 `TestRichRelationsComposesFourIndependentBits`
   把 3007 这一行钉成 `attr==8`。也就是说本期选择的是**如实投影既有数据**，
   而不是在 RPC 里掩盖孤儿标记。
   若要收严：`Unfollow` 里连带 `specialMd.Delete`，或在 `AddSpecial` 之外加一条不变量清扫任务。
   用例：`TestUnfollowLeavesSpecialFlagOrphaned`、`TestDelSpecialClearsOrphanFlagWithoutCheckingFollow`、
   `TestAddSpecialAfterUnfollowRefusesButLeavesStaleFlag`。
3. **完全没有事务，计数与关系可以永久分叉**（AGENTS.md §5 的正面冲突）。
   `Repository` 不持有 `conn`（`internal/repository/repository.go:205-213`），
   `Follow` 的 7 步写入（UPSERT → 2 个计数器 INCR → SET/ZADD → 2 条 `statMd.Incr`）
   没有一条包在事务里（`repository.go:245-274`）。`statMd.Incr` 失败时函数返回 error，
   **但关系行已经提交了**，接口整体报错、调用方重试又会被幂等判定成「已关注、计数不变」
   （`repository.go:250-254`）而不再补计数。`Unfollow` 同构（`repository.go:277-297`）。
   影响：`relation_stat` 与 `relation_follow` 的差值只增不减，且**没有任何对账机制**
   （没有 consumer、没有 cron，见「配置与运行」）；迁移注释说的「可由关系表重算」目前无人重算。
   若要收严：`Repository` 恢复 `conn` 并把「关系 UPSERT + 两条计数」放进同一个
   `TransactCtx`，或补一个 `COUNT(*)` 回填任务。
   用例：`TestFollowStatIncrementFailureLeavesHalfWrittenState`、
   `TestUnfollowStatFailureLeavesRelationAndCacheAhead`。
4. **Redis 写失败一律静默吞掉，且计数器键不重建**。`Follow`/`Unfollow` 里 4 次缓存写
   全是 `_ = r.cache.XXX(...)`（`internal/repository/repository.go:256-265`、`:286-289`），
   `IncrFollowingCount` 失败连注释都只是「缓存失败不阻塞业务，由 DB 计数兜底」
   （`repository.go:256-259`）——而缺口 3 已经说明 DB 侧同样可能半写，两层兜底互相指望。
   更具体的一条：`AddBlack` 里 `DelFollowing` 失败后，关注集合缓存仍留着那条 mid
   （`repository.go:288`），下一次 `IsFollowing` 缓存命中就直接返回「仍关注」且不回库
   （`repository.go:305-307`）。
   影响：缓存与真值的偏差没有自愈路径（`DelFollowSet` 零调用方，见缺口 6）。
   用例：`TestAddBlackCacheDelFailureLeavesFollowingCacheAhead`、
   `TestFollowSucceedsWhenCacheWritesFail`、`TestUnfollowSucceedsWhenCacheWritesFail`、
   `TestStatBackfillWriteFailuresAreSwallowed`。
5. **列表投影的 `attr` 是硬编码常量，`relation_follow.attr` 列与 `RelationAttr` 的
   MUTUAL/SPECIAL 两个枚举值没有生产者**。`ListFollowing` 恒写
   `RELATION_ATTR_FOLLOWING`（`internal/logic/listfollowinglogic.go:45`）、
   `ListFollower` 恒写 `FOLLOWER`（`internal/logic/listfollowerlogic.go:45`），
   既不读 `r.Attr` 也不查 `relation_special`；写侧 `Follow` 建的
   `RelationFollow` 从不赋 `Attr`，于是 INSERT 永远是 0
   （`repository.go:246` → `model/relation_follow.go:74-76`，列定义见迁移 `:13`）。
   影响：客户端从关注列表里**看不出谁被特别关注**，也看不出互关。
   2026-10-04 之后 `RelationAttr` 不再是「只有枚举没有生产者」——新增的 `RichRelations`
   RPC 是四个位唯一真正的生产者（`repository.go:451` 按行存在性合成掩码，
   `relation_follow.attr` 列依旧不参与，写侧仍是 0），
   proto 注释也已改写成「attr 是按位或的掩码」的口径（`rpc/socialgraph.proto:16-34`）。
   剩下的仍是本条：列表投影那三处常量填法不改，列表就只携带自己那一位。
   若要收严：`ListFollowing` 左连 `relation_special` 得到 SPECIAL/MUTUAL 位，
   或者让调用方改用 `RichRelations` 取位。
   用例：`TestListFollowingAttrIsHardcodedIgnoresSpecialAndMutual`。
6. **粉丝时间轴 ZSET 只写不读，是无 TTL 的内存泄漏面**；另有两处死代码。
   `Cache` 对 `sg:fr:<mid>` 只有 `AddFollower`/`DelFollower` 两个写方法
   （`internal/repository/repository.go:100-112`），**全仓没有任何 ZSCORE/ZRANGE 读取**
   （`grep -rn "Zrange\|Zrevrange\|Zscore" services/social-graph/` 无命中），
   列表查询一律走 DB（`repository.go:328-336` 注释也承认「ZSet 作为后续优化留接口」）。
   同类：`Cache.DelFollowSet`（`repository.go:94-98`）与
   `RelationFollowModel.DeleteByMid`（`model/relation_follow.go:42-43`、`:207-214`）
   在生产代码里零调用方（后者注释还写着「拉黑场景下批量取关」，但拉黑走的是 `Unfollow`）。
   影响：大 V 的 `sg:fr:<mid>` 随粉丝数单调增长且永不回收。
   若要收严：要么删掉 ZSET 读写与两个死方法，要么把 `ListFollower` 真正切到 ZSET 并加 TTL。
   用例：`TestNegativeBackfillDoesNotCreateFollowSetKey`（只覆盖到「不凭空建 key」）。
7. **终端网关的 `mid` 由客户端 form 自报，`/social` 全部 12 条路由一个中间件都没有**。
   `ParamFollow{Mid int64 \`form:"mid"\`}`（`gateway/app/api/app.api:1142-1146`）被原样透传进
   `FollowReq.Mid`（`gateway/app/internal/logic/followlogic.go:39-43`），
   而本服务只校验 `Mid > 0` 与 `Mid != FollowerMid`（`followlogic.go:30-38`），
   无法区分「谁在操作」。`/social` 的两个 `@server` 块
   （`gateway/app/api/app.api:1215-1217`、`:2741-2743`）都没有 `middleware:` 行，
   `gateway/app` 全仓唯一一处 `rest.WithMiddlewares` 坐在
   `gateway/app/internal/handler/routes.go:88`（`AppkeyVerify`，只挂 `/account` 部分路由）。
   影响：**任何未登录调用方都能替任意 mid 关注/取关/拉黑/设特别关注**，
   `real_ip` 同样是自报字段、本服务全程不读。
   若要收严：网关按会话渲染 `mid`（与 gateway/admin 的 `adminOperatorID` 同一做法，
   见 `gateway/admin/internal/logic/adminsubject.go:98-108`），并在 `/social` 分组挂鉴权中间件。
8. **调用方 `account` 的六个关系方法已于 2026-10-04 全部接上真实读，剩下的缺口是「降级静默」**。
   `account` 的 `SocialGraphClient` 六方法接口（`services/account/internal/repository/repository.go:65-79`）
   由 `services/account/internal/repository/socialgraph_client.go` 实现，
   `NewServiceContext` 在配置了 `SocialGraphRPC` 时注入（`serviceContext.go:37-40`）；
   `Relation`/`Relations`/`Attentions`/`Blacks`/`Stat`/`RichRelations` 六个方法映射到本服务的
   `IsFollowing`/`IsFollowedBatch`/`ListFollowing`/`ListBlacks`/`Stat`/`RichRelations`，
   并按本服务的上限自己切片分页（owners 与 mids 每片 ≤100、ps=50 翻到 total 或空页、100 页硬停）。
   `RichRelations` 曾在 2026-10-03 因为本契约缺批量反向与 special 读入口而被适配器显式拒绝
   （`ErrRichRelationsUnsupported` + 一条「恒回未接线」的哨兵用例）；
   2026-10-04 本服务补上 `RichRelations` RPC（`rpc/socialgraph.proto:187`）之后，
   那条拒绝与哨兵都被替换成真实调用，account 侧改钉「切片 / 合并 / 逐键透传 / 任一片失败整体报错」。
   **这条历史没有把降级本身修掉**，它落在 `account` 那一侧：
   - **未配置形态静默**：`SocialGraphRPC` 留空时 client 为 nil，六个方法全部命中
     `r.socialGraph == nil` 的降级分支（如 `relation.go:14-16`、富关系是 `:85-90`）
     返回「没关注、不认识、计数 0」且不报错。
   - **RPC 报错同样静默**：`relation.go:18`、`:36`、`:55`、`:71`、`:91-93`、`:112`
     把错误降成一条 Error 日志后照样补默认值，于是「social-graph 挂了」与
     「谁都不认识」在 account 的应答里不可区分（AGENTS.md §9 说的「不得吞掉错误」正指这类分支）。
     若要收严：让 account 的关系读分支返回显式的 not-configured / upstream 错误，
     或在应答里带一个降级标记——属 account 与网关的口径决策，本服务不动。
   - 另外，本仓库没有任何服务被启动过，这两次接线都只有 `go build`/`go vet`/离线单测三类证据。
9. **`Stat` 的降级会丢掉另一个已经命中的计数器，且快照缺失时永不回填**。
   `Repository.Stat` 只有在**两个计数器都命中**时才短路
   （`internal/repository/repository.go:341-352`）；任一 miss 就整体回库，
   把另一个刚从 Redis 读到的新鲜值丢掉、改用 DB 旧值。而 `statMd.Find` 返回 nil
   （该 mid 从没被写过）时是 `return 0, 0, nil`（`repository.go:358-360`），
   **既不回填也不写快照行**，于是这个 mid 每次 `Stat` 都白打一条 SELECT。
   影响：热用户的一次 Redis 抖动就把计数回退成 DB 快照值；`relation_stat` 缺行的用户
   构成一个永久的读放大点。
   若要收严：按 `hit`/`hitR` 分别取值，并对 nil 快照回填 `(0,0)`。
   用例：`TestStatOneMissDropsTheHitCounter`、`TestStatMissingSnapshotDropsHitCounterAndNeverBackfills`。
10. **`Incr` 的「UPDATE 0 行 ⇒ 当作行不存在」在没有唯一约束的窗口里不成立，且负增量不钳制**。
    `model.RelationStatModel.Incr` 先 `UPDATE ... SET following = following + ?, follower = follower + ?`，
    `RowsAffected == 0` 才走 INSERT 分支（`model/relation_stat.go:53-83`）。
    两个问题：(a) 未开 `clientFoundRows`（`etc/socialgraph.v1.yaml:14`）时，
    「行存在但本次增量为 0/0」也返回 0 行 → 进 INSERT 分支撞 `uniq_mid`；
    `repository.go` 的注释称该分支不可达（`internal/logic/fakes_test.go:33-34` 记录了这个前提），
    目前确实只有 ±1，一旦新增 0/0 调用点就会静默走 `ON DUPLICATE KEY UPDATE`。
    (b) UPDATE 分支**没有任何非负钳制**（`relation_stat.go:57-58`，列是 signed `BIGINT`，
    迁移 `:56-57`），所以历史上「先取关后关注」的乱序写入或人工修数可以把 `following` 写成负数，
    `Stat` 会原样透出。
    影响：无越权无泄露，但计数字段的取值域没被写入侧守住。
    若要收严：UPDATE 加 `AND following + ? >= 0` 或改用 `GREATEST(0, following + ?)`，
    并把 INSERT 分支换成不依赖 `RowsAffected` 的 `INSERT ... ON DUPLICATE KEY UPDATE`。
    用例：`TestUnfollowCreatesZeroedSnapshotWhenMissing`、
    `TestFollowCreatesStatRowsLazilyWhenSnapshotMissing`。
11. **入参守卫有两处不对称，且 model 里的分页钳制分支不可达**。
    (a) 自操作守卫只存在于写侧的 `Follow`/`Unfollow`/`AddBlack`/`AddSpecial`
    （`followlogic.go:36`、`unfollowlogic.go:35`、`addblacklogic.go:35`、`addspeciallogic.go:35`），
    `DelBlack`/`DelSpecial`/`IsBlacked`/`IsFollowing` 都没有
    （`delblacklogic.go:28-38`、`delspeciallogic.go:28-38`、`isblackedlogic.go:28-34`、
    `isfollowinglogic.go:28-34`），`IsFollowing(mid, mid)` 会返回 `false`、
    `DelBlack(mid, mid)` 会打一条 SELECT。
    (b) `IsFollowedBatch` 不校验 `owners` 里的元素值（`isfollowedbatchlogic.go:28-43`），
    0 与负数会原样进 `IN (?)`。
    (c) `ps > 50` 在 logic 层就被拒（`listfollowinglogic.go:32-34` 等三处），
    于是 model 里 `if ps < 1 || ps > 50 { ps = 20 }` 的 **`>50` 半边永远走不到**
    （`model/relation_follow.go:145-147`、`:178-180`、`model/relation_black.go:108-110`）。
    影响：没有数据风险，但同一服务里三套守卫口径，收严时容易漏改。
    用例：`TestDelBlackHasNoSelfGuard`、`TestDelSpecialHasNoSelfGuard`、
    `TestIsBlackedHasNoSelfGuard`、`TestIsFollowingHasNoSelfGuard`、
    `TestIsFollowedBatchDoesNotValidateOwnerValues`、`TestListFollowingClampsPaginationInModel`。
12. **`IsBlacked` 复用 `RelationReply.following` 字段承载「是否被拉黑」**。
    proto 为拉黑点查没有单独消息，logic 直接 `return &rpc.RelationReply{Following: ok}`
    （`internal/logic/isblackedlogic.go:40`），字段名与语义相反；
    网关 `/social/black/check` 也照样叫 `SocialIsFollowingResponse`
    （`gateway/app/api/app.api:2755`）。
    影响：仅可读性与误用风险（`TestIsBlackedAnswersAboutMidBlockingOwner` 钉住了真实语义）。
    收严方式：改 `rpc/socialgraph.proto` 加 `BlackRelationReply{black bool}` 并重新生成，
    属**破坏性契约变更**，需与网关同批提交。
