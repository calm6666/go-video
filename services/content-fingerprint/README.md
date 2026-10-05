# content-fingerprint

音视频指纹、重复投稿和版权匹配服务。

- **拥有数据**：`fingerprint_task`（指纹抽取任务）、`fingerprint_record`（指纹记录）。
  - 任务字段：`task_id`/`asset_id`/`fp_type`/`video_key`/`audio_key`/`state`/`ctime`。
  - 记录字段：`asset_id`/`fp_type`/`key`/`hash`/`ctime`。
  - `fp_type`：1=video、2=audio。
- **提供能力**：`SubmitTask` 创建指纹任务（PENDING）、`GetTask`/`ListTasks` 查询、
  `UpdateTaskResult` 由 Worker 回写指纹并推进 PENDING→SUCCEEDED/FAILED、
  `MatchByFingerprint`/`MatchByAsset` 指纹相似匹配。
- **依赖**：MySQL、Redis、Etcd；消费 `media.task.v1` 事件（指纹任务派发，本期占位）。
- **约束**：仅持有指纹事实和匹配候选；不复制媒资主数据；不主动调用下游 RPC；
  匹配结果作为审核证据供 `moderation-orchestrator` 使用，不直接下架内容。
- **占位说明**：`SubmitTask` 只建任务，不调用真实指纹算法；
  `MatchByFingerprint`/`MatchByAsset` 本期返回空列表 + TODO 注释，待后续接入指纹检索引擎。

## 测试覆盖

离线单测（纯 Go 内存替身），不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络。
数字由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
合计 **49 顶层 / 1 子用例**（logic `48/0` + config `1/1`）；本服务没有处于 `t.Skip` 状态的用例。
每个方法都是同一套四件套：守卫表（拒绝时 `callLog` 必须为空，证明校验发生在碰到任何依赖之前）、
happy path 逐字段投影（要求值可区分，不允许「非空即通过」）、每个依赖逐一注错的失败传播
（错误原样透出、不许留半成品、不许假成功）、再加下面的域不变量。

### 1. `internal/logic`（7 个文件：6 个用例文件 + `fakes_test.go` 替身层）— `48/0`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `submittask_test.go` | 7 | 0 | `asset_id`/`fp_type` 枚举守卫先于触库；任务落库是 `PENDING` 且 `ctime`/`mtime` 由 model 取 now，**`task_id` 必须回填成自增主键**（一条裸 UPDATE，跨服务引用靠它，没回填就是脏数据）；提交后 `GetTask` 读回同一行；video/audio 是两条独立任务；同 `(asset_id, fp_type)` 重复提交被 `uniq_asset_fptype` 挡住并如实报错、logic 不伪造「幂等成功」（缺口 8）；`TestSubmitTaskIgnoresTaskIDBackfillFailure` 钉住回填失败被丢弃的连锁后果（缺口 2） |
| `updatetaskresult_test.go` | 11 | 0 | 目标状态只能 `SUCCEEDED`/`FAILED`、不能自我回退或悬空，且 `task_id` 守卫先于状态守卫；**只有 `SUCCEEDED` 才写 `fingerprint_record`**，`FAILED` 的半成品 key 只留在任务行、绝不进版权比对源；次序是「先推进任务 → 再落指纹事实 → 最后失效任务缓存」，任何一步失败都不留后续副作用；同 `(asset_id, fp_type)` 重复上报靠 `ON DUPLICATE KEY UPDATE` 幂等（只剩一行、主键不换、key/hash/ctime 被替换）；非 `PENDING` 任务直接 `ErrIllegalState` 且序列停在 `task.FindOne`；两条吞错的现状（指纹写失败、缓存失效失败）各有一条用例钉住并标为疑似缺陷（缺口 1、3） |
| `gettask_test.go` | 9 | 0 | 缓存命中必须以缓存为准、不得再查库，miss 才回源并回填**同一行**；「任务不存在」是 `ErrTaskNotFound` 而不是空 reply，且**不写负缓存**（否则任务刚建好就被 5 分钟里的空结果盖掉）；缓存读失败/解码失败必须是错误、不能被降级成「不存在」（把「Redis 挂了」读成「没有这个任务」是危险的降级方向）；解码失败的脏缓存不自愈（只钉现状，缺口 4）；枚举投影带未知码回落口径 |
| `listtasks_test.go` | 8 | 0 | `ps` 上限守卫是**排他**的（50 放行、51 由 logic 拒）且发生在触库之前；过滤条件走 model 层编号（rpc 枚举与 model 常量不同值时靠 `taskStateToModel` 映射），`pn`/`ps` 原样透传、钳制在 model；列表顺序就是 model 的 `ORDER BY ctime DESC`，logic 不重排；越界页 `total>0` 而行为空、空集返回非 nil 切片；下游失败整体报错，不返回 `total=0` 的「看起来没有数据」伪成功 |
| `matchbyfingerprint_test.go` | 7 | 0 | 守卫顺序 `fp_key → fp_type → top_n` 全在触库之前；`top_n<=0` 回落默认 10、`>50` 直接拒、恰好 50 放行（防一次拉爆检索）；生产实现是占位（`model.FindByKey` 不查库、恒空）⇒「命中列表为空」是**不判定为侵权**的降级方向，但依赖失败必须是错误、绝不能降级成 `(空列表, nil)`（那等于把「检索挂了」上报成「这条内容干净」）；`Score` 恒 0 ⇒ 调用方不得据此判侵权、也不得以为已排序；`fp_key` 长度为 1 也能过守卫、顺序原样透传不重排（钉的是现状不是愿望） |
| `matchbyasset_test.go` | 6 | 0 | 与上一条的分工：本条按 `asset_id` 读**该媒资自己的指纹事实**（本期即「自匹配」）；`asset_id` 守卫在触库之前；`fp_type` 三态语义——`UNSPECIFIED=0` 不限定类型（video+audio 都回）、1/2 精确过滤、未知枚举也落到 0（这条最容易写反）；只回本 asset 的行、绝不串到别的媒资（版权证据不能张冠李戴）；无指纹时返回空而非 nil；`TestMatchByAssetReturnsEmptyKeyRecordAsHit` 钉住「`key`/`hash` 为空串的存量行被当命中」这一现状；依赖失败必须是错误 |
| `fakes_test.go` | 0 | 0 | 替身层与装配缝（`NewWithDeps` + 内存缓存/两个 model + `fakeConn`），`top=0 sub=0` 是正常形态；口径见第 4 组 |

### 2. 其他层

- `internal/config`：1 个文件 `1/1` —— `config_load_test.go` 逐个加载 `etc/*.yaml`（子用例按文件名展开），
  反射递归断 `*DataSource` 与 `redis.RedisConf.Host` 非空；业务缓存键必须叫 `CacheRedis`，
  叫 `Redis` 会与 `zrpc.RpcServerConf` 内嵌字段撞车、服务启动即报 `conflict key redis`。
- `internal/repository`（`Cacher`/`NewWithDeps` 注入缝的宿主）**无离线单测**——只被 logic 用例经由它跑，
  真实 `*Cache` 的 `SETEX`/`GET`/`DEL` 不在被测路径上。
- `model/`（`fingerprintmodel.go`、`errors.go`、`now.go`）**无离线单测**：两张表的 SQL 文本、列名、
  占位符、`ON DUPLICATE KEY UPDATE` 与 `ORDER BY`/`LIMIT/OFFSET` 都没有门禁，仓库里也没有
  本服务的迁移↔model 列级对账门禁。
- `internal/svc` **无离线单测**；`internal/server`、`rpc/*.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层：README 声明消费 `media.task.v1`，
  但服务里没有 consumer（缺口 9），指纹抽取的实际执行方（Worker）无人派发，
  所以「事件 ⇒ 建任务 ⇒ 抽指纹 ⇒ 回写」这条链只有 `SubmitTask`/`UpdateTaskResult` 的 RPC 面被单独测过。

### 3. 构造器级覆盖：**6/6**

探针取 `internal/logic` 全部 `New*Logic(`，共 6 个（`SubmitTask`、`GetTask`、`ListTasks`、
`UpdateTaskResult`、`MatchByFingerprint`、`MatchByAsset`，即「提供能力」列出的六个 rpc），`gaps:` 为空——
每个 RPC 方法都有直接驱动自身构造器的用例。

### 4. 注入缝与替身口径

`Repository` 的缓存依赖是具体类型 `*Cache`，因此 `internal/repository` 暴露
`Cacher` 接口 + `NewWithDeps(cache Cacher, conn sqlx.SqlConn, taskMd FingerprintTaskModel,
recMd FingerprintRecordModel)`；生产路径仍只走 `New(rds, conn)`。logic 用例据此组装
**真实 Repository**，只把 4 个依赖（缓存、两个 model、`sqlx.SqlConn`）换成内存替身，
所以缓存读穿回填、`task_id` 与主键对齐、状态推进后才写指纹事实、任务缓存失效
这些判定链整条都在被测路径上，而不是把 Repository 也 mock 掉。
`conn` 不能省：`SubmitTask` 用 `conn.ExecCtx` 直接把 `task_id` 回填成自增主键，
那条裸 SQL 也在被测路径上。`Cacher` 只列 `Repository` 真正调用的方法
（`Ping` 与 Task 的读/写/删）；`Get*/Set*Match*` 四个方法目前没有调用点，
放进接口只会强迫替身实现死代码，所以等 Match 缓存真接线时再加。

`internal/logic/fakes_test.go` 记了四条替身纪律（值拷贝、按真实 SQL 口径分配主键与唯一键、
按顺序记录调用轨迹 `<pkg>.<method>:<key>`、按方法粒度注入错误），其中最容易踩坑的还是
**布数据必须走静默写入路径**（`put`/`warm`），否则序列断言会把布景调用也算进去。
两处刻意的严格性：`fakeConn` 只认那条回填 SQL 的**原文**，收到任何其他 SQL 直接 panic；
唯一键复刻是全套的——包括"默认值 0 也占 `uniq_task_id` 槽位"，
所以绕过回填第二次提交会撞 `uniq_task_id`（这正是缺陷 2 的表现，见下）。

### 5. 覆盖边界（如实声明）

- 用例不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络；没有 Worker、没有 FFmpeg、
  没有指纹检索引擎参与。服务声明消费的 `media.task.v1` 在本服务里没有 consumer（缺口 9），
  所以「事件 ⇒ 建任务」这一段不在被测路径上，两端各自被 RPC 用例单独覆盖。
- **媒体二进制不进 MySQL，也不进用例**：两张表只存哈希与键引用
  （`fingerprint_record.key`/`hash`、任务的 `video_key`/`audio_key`），替身里同样只有字符串字段，
  没有任何字节流路径；真实指纹算法既没有被存储，也没有被执行。
- 指纹链路只有**离线判定**被验证：`SubmitTask` 只建 `PENDING` 任务、不调用指纹算法；
  `MatchByFingerprint` 走的 `model.FindByKey` 是恒返回 `nil, nil` 的占位实现。
  因此用例钉住的是守卫顺序、状态门槛、副作用序列与投影口径，**不是**「指纹真的抽出来了」
  「命中真的存在」；对象存储侧的预签名 URL 生成与回调验签/回写整条链路都不在被测路径上。
- 替身只复刻 model 层 SQL 的**语义**（`ON DUPLICATE KEY UPDATE` 替换内容不换主键、状态门槛、
  `ORDER BY` 方向、`sql.ErrNoRows → (nil, nil)`），不证明 SQL 本身；`model/*.go` 的列名、
  占位符与 `LIMIT/OFFSET` 仍无单测，仓库里也没有迁移↔model 列级对账门禁。
  `ListTasks` 的断言是「透传参数 + 替身按同一口径切片」的双重核对，真实翻页仍需隔离实例复验。
  `fakeRecordModel.FindByKey` 默认与生产占位实现一致（恒 `nil, nil`），
  `searchEnabled` 开关只为钉住 logic 的投影与顺序契约，不代表检索已经存在。
- 不覆盖真 Redis 行为：`Cacher` 替身不复刻 `SETEX`/`GET`/`DEL` 的编码与过期语义，
  所以 300 秒（`cacheTTLTask`）与 60 秒（`cacheTTLMatch`）的**真实 TTL**、
  以及失效失败后的旧快照窗口（缺口 3）都只被断言到「调用发生/未发生」这一层。
  `fp:match:k:%s`、`fp:match:a:%d` 两个 Match 缓存键在 `Repository` 里没有任何调用点（缺口 5），
  因此也不有用例覆盖它们——这是死代码，不是漏测。
- 迁移 SQL ↔ 真实库的列级对账：本 README 没有声明过在隔离实例 `127.0.0.1:3399` 复验，
  因此按**未在目标实例复验**处理，也不得据此声称已验证；迁移文件的存在与登记只以
  `deploy/migrations/README.md` 为权威。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内；改契约先改
  `rpc/fingerprint.proto` 再执行统一生成（`docs/commands.md`）。

2026-09-23 变异探针（改坏生产规则看用例是否真的红）三组均被抓：
绕过 `MatchByFingerprint` 的 `top_n > 50` 上限 ⇒ 边界用例报
`top_n=51（上限 +1）：错误 = nil, want content-fingerprint: top_n exceeds 50`；
绕过 `UpdateTaskResult` 的 `state == SUCCEEDED` 门槛 ⇒ 副作用序列少掉两条 `rec.Upsert`、
幂等用例的指纹内容没被替换；绕过 `SubmitTask` 的 `task_id` 回填 ⇒ 提交序列少一步、
回读 `content-fingerprint: task not found`、第二个任务撞 `uniq_task_id`。
探针后已还原（`grep -rn 'false[[:space:]]*&&' services/content-fingerprint/` 无残留）并复跑为绿。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/content-fingerprint/...
gofmt -l services/content-fingerprint
go vet ./services/content-fingerprint/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下，并发链接多个测试包会因内存耗尽失败（`errno=1455`）；
  `-count=1` 关闭测试缓存。
- 口径说明：`go vet` 期望无输出、`gofmt -l` 期望为空列表。五道门禁由主代理统一串行执行，
  本节只登记命令与口径，不在文档里代为声明执行结论。

## 已知缺口

1. **`UpdateTaskResult` 吞掉指纹写入失败**（`internal/repository`：`_ = err`）：
   任务已推进为 SUCCEEDED，但 `fingerprint_record` 可能一行都没写进去。
   调用方看到"抽取成功"，而重复投稿/版权比对拿不到指纹事实——**这是假成功，
   也是本域最危险的缺口**。状态回写与事实写入不在同一事务里，
   要么合并成一条事务，要么让 Upsert 失败连带回退状态。
2. **`SubmitTask` 忽略 `task_id` 回填失败**：那条 `UPDATE ... SET task_id = id` 的错误被丢弃。
   回填失败时行留在 `task_id=0`，于是本次返回的 task_id 永远查不到，
   而下一次提交（`task_id` 默认 0）还会撞 `uniq_task_id`——一条失败会连锁污染后续提交。
   用例 `TestSubmitTaskIgnoresTaskIDBackfillFailure` 钉的就是这个后果。
3. **`UpdateTaskResult` 忽略缓存失效失败**（`_ = r.cache.DelTask`）：失效失败后
   旧的 PENDING 快照最多还会被读 `cacheTTLTask`（300 秒），且**没有自愈路径**
   （`GetTask` 命中缓存就不回源）。Worker 因此可能反复重跑同一个"看起来还在 PENDING"的任务。
4. **`GetTask` 的坏缓存不自愈**：命中但 JSON 反序列化失败会持续报
   `GetTask unmarshal cache`，直到 TTL 到期或被下一次 `DelTask` 清掉；
   缓存读错误同样直接失败、不回源 DB（读路径 fail-closed，是有意选择，但缺少降级开关）。
5. **`fp:match:*` 是死 key**：`Cache` 有四个 Match 缓存方法和 `cacheTTLMatch = 60`，
   但 `Repository` 里没有任何调用点，logic 也不缓存匹配结果，
   与 `MatchByFingerprint` 的注释「Match 结果缓存 1 分钟」不符。
6. **`MatchByFingerprint` 只有空占位**：model 的 `FindByKey` 恒返回 `nil, nil`，
   所以命中列表恒空、`Score` 恒 0。这不是"没有侵权"而是"没有检索"，
   而 `MatchReply` 没有 `available`/`reason` 之类字段可供调用方区分二者；
   在接上检索引擎并定下阈值之前，本服务**不能**单独作为 AGENTS.md §8 的放行依据。
7. **命中列表没有排序/裁剪契约**：logic 按依赖返回的原样输出（顺序即依赖顺序，
   `TestMatchByFingerprintKeepsDependencyOrderWithoutRanking` 钉住），`top_n` 目前只是透传参数。
8. **`SubmitTask` 重复提交返回裸 1062**：`(asset_id, fp_type)` 已存在时应幂等返回既有任务，
   现在把 `Duplicate entry for key 'uniq_asset_fptype'` 直接抛给调用方，Worker 重试会一直失败。
9. **`media.task.v1` 消费者未接入**：README 声明消费该事件，`SubmitTask` 的 TODO 也这么写，
   但服务里没有 consumer——任务只能靠调用方手工 `SubmitTask`，
   指纹抽取的实际执行方（Worker）目前无人派发。
