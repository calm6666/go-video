# danmaku

视频和直播时间轴弹幕服务，对应参考仓库的 `dm/dm2`。

- **拥有数据**：时间轴消息、屏蔽词、用户屏蔽、审核结果和分片索引。
- **提供能力**：批量拉取、实时发送、颜色/位置约束、反刷和审核联动。
- **依赖**：`playback`/`live-gateway`、Redis/MQ、`moderation-orchestrator`、`risk-control`。
- **约束**：实时通道不能依赖评论同步事务；消息按视频/时间分片并设置生命周期。

## 已知缺口（logic 单测读生产代码时确认，均需契约或设计裁决后再改）

下列三条由 `internal/logic/blockword_test.go` 的用例**钉住当前行为**（不是期望行为）；
修法落地时相应用例必须改成断言「正确结果」，否则会静默保留旧口径。

1. **改全局屏蔽词不会立即影响各分区视图**。`dm:bw:<oid>` 存的是 `ListEnabled(oid)` 的结果
   （= 全局词 + 本分区词，见 `model/danmaku_blockword.go` 的 `ListEnabled`），全局词被复制进每个
   分区缓存；而 `invalidateBlockWordCache`（`internal/repository/repository.go:437-441`）对全局改动
   只删 `dm:bw:0`（`internal/repository/cache.go:312` 的 `DelBlockWords`）。
   后果：新增全局辱骂词后各分区最长 `BlockWordCacheTTLSeconds`（etc 缺省 300s）继续漏拦，
   停用/删除后最长 300s 继续误拦。可行修法要么给分区键加全局 epoch 版本，要么按模式扫描失效，
   两者都改缓存键布局 ⇒ 需要设计裁决。钉住用例：`TestBlockWordGlobalChangeLeavesPartitionCachesStale`。
2. **`uniq_word` 与 scope/oid 语义冲突**。词条以 `word` 全局唯一，`SCOPE_GLOBAL` 与 `SCOPE_OID`
   争同一个键：对已存在的全局词执行分区 ADD 会就地改掉它的 scope
   （`TestBlockWordAddOnExistingGlobalWordDemotesIt`），DISABLE/DELETE 则完全忽略请求里的 scope/oid、
   按行自身作用域处理（`blockwordlogic.go:73`、`:91` 传的是 `existing.Oid`，
   见 `TestBlockWordDisableIgnoresRequestedScope`）。是否符合运营预期需要产品裁决。
3. **DELETE 成功后回复 `state=DISABLED`**（`blockwordlogic.go:99`）：行已物理删除，调用方无法从应答
   区分「停用」和「删除」。修法要动 `BlockWordReply.state` 取值口径（model 只有 enabled/disabled 两态）
   ⇒ 属契约变更。钉住用例：`TestBlockWordDeleteRemovesRowButRepliesDisabled`。

另有契约缺口（不在本服务可修范围）：`danmaku.proto` 的 `UserBlockReq` 未暴露屏蔽时长档位
（`model/danmaku_user_block.go` 支持 1 天/7 天/永久），`DanmakuStat.like_count` 有 schema 无写侧。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，10 个文件 `120/69`）

按链路分四组：发送与删除、审核联动、读侧、运营写侧。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **发送与删除** | | | |
| `postdanmaku_test.go` | 25 | 6 | 「参数守卫 → 令牌桶 → 幂等重放 → 分钟窗口 → 屏蔽词 → 落库 → 送审」的**处理顺序本身是被测契约**（每个用例断完整有序序列，不只是次数）；内容按 rune 计的长度边界；必须先给 client key 才碰数据、否则回落到 `client_msg_id`；展示字段归一；屏蔽词命中折进 BLOCK 池、加空格的变体仍命中、scope 只作用于自己的 oid、词库缓存跨发送复用、缓存坏了降级回 DB、超长词表绕过缓存；幂等重放既不二次插入也不二次扣配额，上次送审失败则重放会重新送审；并发重复依赖唯一索引；机审放行直接发布；审核不可用时**绝不伪造 submitted**、送审失败时行保持未发布、task_id 回填失败仍照常应答；限流分别打在 mid 与 oid 维度且 mid 先报；未配置的窗口永不触发；下游失败上抛 |
| `deletedanmaku_test.go` | 11 | 5 | 「归属校验 → 幂等判定 → 状态机合法性 → 主表 CAS + op_log **同事务** → 提交后才调段计数与段缓存」的链路；非本人拒绝且**零副作用**；管理员分支靠 `in.Admin` 显式打开、留痕角色记成 RoleAdmin；软删保留 content/oid/mid/ctime/幂等键一个都不丢；状态矩阵钉住哪些状态可删、脏状态才报非法迁移；目标不存在与 CAS 未命中（并发更新）两种姿态分开；段计数不为负（`GREATEST`）；`reason` 超长无守卫；下游失败上抛 |
| `reportdanmaku_test.go` | 12 | 13 | 守卫（`dmid` 优先）发生在任何依赖调用之前；目标弹幕必须存在；自举报门禁；逐字段落库；举报**只落 `danmaku_report` 一张表**——不改弹幕状态/池、不写 op_log、不开事务、不动缓存；同 `(dmid, reporter_mid)` 只留一行（重放不二次写、并发撞唯一索引后回查既有行）；按举报者与目标双向隔离；现状哨兵：接受任意状态的弹幕（悬空举报）、`content`/`reason` 不校验、没有频次与风控、举报者身份完全来自请求体、不留任何审计轨迹；下游失败一律原始上抛（本方法没有可降级分支） |
| **审核联动** | | | |
| `applymoderationresult_test.go` | 13 | 22 | `moderation.result.v1` 的入口链路：参数守卫 → 结论映射 → 弹幕存在性 → 事件去重快查 → 状态机门禁 → 主表 CAS + op_log 同事务 → 提交后才调段计数与段列表缓存。四条不变量：只有 `VERDICT_PASS` 能把弹幕放进普通池对所有人可见、越界结论在碰任何依赖前被拒；带 `event_id` 的重复投递被挡下且不产生第二次状态写/留痕/段计数；非法迁移（已删除、枚举外脏状态）返回错误，绝不「顺手洗成可见」；事务内任一步失败整体回滚，缓存与派生侧失败只降级但失效调用必须仍然发生。另钉四条现状：成功迁移的 `applied` 恒 false、幂等判定只看 state 不看 pool、段缓存/段计数写失败被就地吞、CAS 未命中后重读失败时下游原始错误顶替了可重试哨兵 |
| **读侧** | | | |
| `listdanmaku_test.go` | 17 | 6 | 时间轴窗口解析 → 段缓存读穿 → 本人待审回显 → 用户级屏蔽过滤 → 段计数；配置缺失时回落到 max-seg 窗口；逐 RPC 字段投影；`limit` 夹到 `MaxListLimit`、段全 miss 时把 limit 下发进 DB 查询；**只下发 `state=NORMAL 且 pool=NORMAL`**（五种状态齐铺）；`self_pending` 只回显 viewer 自己的隐藏行且必须有 viewer；应用查看者的用户级屏蔽、游客跳过屏蔽查询、无行时不发屏蔽查询、空屏蔽视图被当缓存命中；段计数失败**只降级不阻断拉取**；缓存写失败不影响下发；下游失败上抛 |
| `listblockwords_test.go` | 8 | 4 | 匿名读被拒（`operator_mid>0`）且拒在查库之前；作用域过滤映射（UNSPECIFIED 不过滤 / GLOBAL / OID）与分页参数**原样下传**（夹紧只发生在 model，两层不许各自造默认值）；model 行到 rpc 逐字段对齐、不漏列不串列；只启用词过滤；空结果是非 nil 切片；`SCOPE_OID` 不带 oid 时列出所有分区；**不碰发送侧词库缓存**（`dm:bw:*`）；失败上抛 |
| `listuserblocks_test.go` | 9 | 4 | 行归属：必须以 `mid` 为硬过滤，甲的清单里绝不出现乙的行；`total` 与列表口径**故意不一致**（total 含 `state=0` 已解除，列表只回 `state=1`）⇒「total>0 却一页翻不出来」被钉成期望值；类型过滤映射；分页参数原样下传；信任请求体 supplied 的 mid；纯读，不碰 `dm:ub:*` 与发送侧任何缓存；逐字段投影；失败上抛 |
| **运营写侧** | | | |
| `blockword_test.go` | 13 | 5 | 匿名写被拒（`operator_mid>0`）且 operator 列如实记下**最近一次**操作者；64 字符边界正好取在限值上；全局词落行并失效 `dm:bw:0`、分区词失效两个视图；**全局改动不清分区视图**（已知缺口 1 的哨兵）；`uniq_word` 上重复 ADD 走 upsert 不产生新行、停用后重新启用不换 ID；对已存在的全局词做分区 ADD 会就地改掉它的 scope（缺口 2）；DISABLE 保留行作审计证据、且完全忽略请求里的 scope/oid（缺口 2）；DISABLE/DELETE 对不存在词条的姿态；并发在写前移除；DELETE 物理删行却回复 `DISABLED`（缺口 3）；下游失败上抛 |
| `userblock_test.go` | 12 | 4 | 归属列 `mid` 恒等于请求者本人——不能替别人建屏蔽、也不能清别人名单；缓存失效只打在**本人**的 `dm:ub:<mid>` 上（打到被屏蔽者身上是越权）；关键词长度边界；自发屏蔽被拒；type 与目标列自洽：keyword 类型丢掉外来 `blocked_mid`、mid 类型丢掉 keyword（唯一键里 `0`/`''` 是占位值，写脏就串键）；`uniq_mid_target` 上屏蔽/解除都是 upsert（同目标重复提交只更新 state，不换主键、不新增行）；解除是翻 `state` 并保留行作锚点、解除未知目标会建一条 off 行；不同目标不串键、不同归属互相隔离；下游失败上抛 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存缓存 + 内存 conn + 6 个 model 替身 + Options，按 `NewWithDeps` 组装真实 Repository，见第 4 组 |

### 2. 其他层

- `internal/policy`（4 文件 `22/8`）——纯函数层，不含任何依赖：
  - `blockword_test.go` `6/2`：归一化规则（全角折叠、大小写统一、空白/零宽/填充标点丢弃，
    这些正是屏蔽词绕过的主要手段）、匹配、最长词优先、不可用词被忽略、归一化变体去重、
    nil filter 的行为。绕过样本用 `\u` 转义书写，避免 BOM 触发 Go 词法错误。
  - `ratelimit_test.go` `2/1`：固定窗口判定的边界——`Count` 是 Redis INCR 后的值（含本次），
    **等于阈值必须放行，阈值加一才拒绝**；判定不改动入参。
  - `segment_test.go` `6/3`：时间轴毫秒 → 分段号从 0 开始、按 `SegmentSeconds` 左闭右开切分、
    单调、区间枚举与非法入参的确定性回退。
  - `statemachine_test.go` `8/2`：合法迁移表逐条校验（期望值在这里**显式重写一遍**，
    让「改表必须同时改测试」，避免误删迁移路径后测试跟着实现一起漂移）、非法迁移、
    DELETED 是终态、`States` 覆盖全表、verdict → target 映射、只有 PASS 才公开、
    各 verdict 目标可达、初始状态。
- `internal/config`（1 文件 `2/0`）：`config_test.go` 校验 `etc/danmaku.v1.yaml` 能被完整解析、
  端口与服务名与 `docs/service-catalog.md` 的登记值一致，以及最小 yaml 的默认值填充；
  只解析配置，不连接 MySQL/Redis/etcd。
- `model/`（6 个 model 文件）、`internal/repository/`、`internal/svc/`、`internal/server/`：
  **无离线单测**。
- 本服务没有 `internal/consumer` 目录：`moderation.result.v1` 的事件消费者待接线，
  `ApplyModerationResult` 目前是「被 gRPC 入口打到的实现」，离线用例不覆盖 MQ 投递与重试/死信。
- 合计 **144 个顶层用例、77 个子用例**（logic `120/69` + policy `22/8` + config `2/0`）；
  **0 个 `t.Skip`**。

### 3. 构造器级覆盖

`9/9`：探针取 `internal/logic` 全部 `New*Logic(`（9 个，与 `rpc/danmaku.proto` 的 9 个方法一一对应：
PostDanmaku / ListDanmaku / DeleteDanmaku / ReportDanmaku / UserBlock / ListUserBlocks /
BlockWord / ListBlockWords / ApplyModerationResult），逐个在 `*_test.go` 里查引用，`gaps:` 为空，
即 9 个方法的 logic 都从构造器进入被打过（`ApplyModerationResult` 与 `ReportDanmaku`
分别由 `applymoderationresult_test.go`、`reportdanmaku_test.go` 直接驱动）。

### 4. 替身层与断言口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`，生产构造走 `repository.New`
（真 Redis + 真 MySQL），测试无处塞替身。本包用例统一用
`repository.NewWithDeps(内存缓存, 内存 conn, 6 个内存 model, Options)` 组装**真实的 Repository**，
只把它的 8 个依赖换成替身，于是「段缓存读穿/空段回填、屏蔽词降级、分钟窗口计数、
幂等键回查、主表 CAS + op_log 同事务、段计数派生」整条判定链都在被测路径上，
而不是把 Repository 也 mock 掉。

四条替身纪律：每次读返回值拷贝（logic 里的写回如 `d.Dmid = dmid`、`log.Dmid = ...`
不得污染库存行）；副作用按**顺序**记录（要紧的结论是「op_log 与主表 CAS 是不是同一个事务、
失效缓存有没有排在事务之后、重复投递有没有二次加段计数」）；错误注入按方法粒度；
布数据走静默写入路径（`put`/`warm`，不记 callLog），因此轨迹断言直接从 0 数起。

比 rights/playback 多出来的两条：分钟窗口 key 含 `now.Unix()/60` 而 logic 传的是真 `time.Now()`
（不可注入），布防用 `cache.freezeWindows()` 把桶冻成常量并返回该常量，否则跨分钟边界会让
密度用例随机变绿变红；自增主键按表分段起算（danmaku 从 101、blockword 从 501 …），
这样「新写入那一行的 ID」可以直接出现在精确序列期望里，不必回读。

断言口径：写侧用例断言**完整调用序列**（不是次数前缀），布景不写序列因此期望从 0 数；
一个用例内多次发被测调用时必须按片段取（`log.opsFrom(from)`），否则后面的子用例会算进前一个的调用。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端；
  `internal/server`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内。
- 替身只复刻 model 层 SQL 的**语义**（唯一索引冲突、CAS 门槛、`GREATEST` 不为负、
  `ORDER BY + LIMIT/OFFSET` 分页、空段命中），不证明 SQL 与列名本身。
- Redis 的 `SETEX`/`INCR`/TTL 与 JSON 编解码由真实 `*Cache` 承担，logic 用例只断言
  「读/写/失效发生的顺序」，**不验证 TTL 秒数**；分钟桶算法本身属于 `*Cache` 实现，
  不在 logic 断言范围内。
- 事务原子性由 MySQL 保证，替身只断言「主表 CAS 与 op_log 拿到同一个非 nil 事务会话、
  且只开一个事务、失败时确实回滚」；`errStateCasMiss` 在 repository 里是 `err ==` 身份比较，
  依赖 go-zero `TransactCtx` 不包装 fn 返回值，替身按不包装实现。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有隔离实例
  （`127.0.0.1:3399`）的复验结论，本服务也没有 model ↔ DDL 的静态门禁；
  `deploy/migrations/README.md:103` 的 `applied` 口径只是「`up` + `status` 跑通」（见同文件 `:21`）。
- 消费者链路（MQ 投递、去重、退避重试、死信）没有用例——本服务无 `internal/consumer`，
  事件接入后 `ApplyModerationResult` 的幂等与重试语义需要补投喂型用例。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/danmaku/...
gofmt -l services/danmaku    # 必须为空
go vet ./services/danmaku/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

