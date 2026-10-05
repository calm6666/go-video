# transcode

转码任务与播放版本管理服务（go-zero gRPC）。

- **拥有数据**：`transcode_task`（转码任务）和 `transcode_template`（转码模板）。
  - 任务字段：task_id/asset_id/template_id/input_bucket/input_key/output_bucket/output_key/state/progress/errno/err_msg/ctime/mtime。
  - 模板字段：template_id/name/codec/width/height/bitrate/fps/segment_seconds。
- **提供能力**：创建转码任务、查询任务详情与分页、Worker 上报进度；
  模板侧只有「创建 + 查询详情 + 分页」三个 RPC（无更新/删除接口，重名靠 `uniq_name` 拦）。
- **依赖**：MySQL、Redis、对象存储（OSS）、消息队列（MQ）；本期不直接依赖 FFmpeg。
- **与 asset/video 关系**：
  - 消费 `media.task.v1` 事件（由 asset 等媒体服务派发）拉起任务；
  - 转码任务完成（`SUCCEEDED`）后由上层 `video` 服务推进稿件状态机至 `READY_FOR_REVIEW`，transcode 不直接写稿件状态；
  - `content.published.v1` 事件由 `video` 服务发布，本服务不发布。
- **状态机**：`PENDING → PROCESSING → SUCCEEDED/FAILED`。`UpdateProgress` 校验合法转换，终态（`SUCCEEDED`/`FAILED`）不可再变更。
- **约束**：
  - `SubmitTask` 本期占位：仅创建 `PENDING` 任务并返回，不调用 FFmpeg；
  - 实际转码由独立 Worker 消费 MQ 拉起 FFmpeg 实现（本期不实现 Worker）；
  - Worker 只能回调推进状态，不能直接发布稿件；
  - 任务幂等、可取消、资源隔离；缓存短 TTL（任务）/ 长 TTL（模板），变更时刷新。
    > 上述两条承诺**当前均未成立**：`SubmitTask` 没有幂等键（已知缺口 3），
    > 模板缓存既没有写入侧也没有失效路径（已知缺口 11）。

## 测试覆盖（不连 MySQL/Redis/etcd/MQ/对象存储，不依赖网络）

离线单测（纯 Go 内存替身）。数字由 `grep -cE '^func Test'`（已排除 `TestMain`）与
`grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。合计 **73 顶层 / 14 子用例**
（logic `72/13` + config `1/1`）；本服务没有处于 `t.Skip` 状态的用例。

### 1. `internal/logic`（8 个文件：7 个按方法的用例文件 + `fakes_test.go` 替身层）— `72/13`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `submittask_test.go` | 8 | 3 | 三段守卫的顺序（asset → template → 字符串）与「被拒时一个依赖都没碰」；一次 INSERT、应答 13 字段与库存行逐字相等（`task_id` 来自 `LastInsertId` 而不是 0）；无幂等键 ⇒ 同一请求重放必得两条任务；不做任何存在性校验、不校验空白与长度；INSERT 失败原样上抛且不消耗自增号 |
| `updateprogress_test.go` | 15 | 3 | 状态机门槛（终态在守卫读里被检出 ⇒ 零写入；`ErrInvalidTransition` 只有脏 `state` 的行才可达）；`PENDING→PROCESSING` 的六步轨迹；**同一个并发窗口的两侧**——插队发生在守卫读之前会被检出、发生在之后则丢失更新并被判成功，且校验读命中 60s 缓存时库里已是终态也照样放行；`progress` 可回退、`SUCCEEDED` 可与进度不一致、漏传 `errno` 抹掉既有失败原因；UPDATE 失败不删缓存、回读为 `nil` 却返回全零成功应答 |
| `gettask_test.go` | 9 | 0 | 任务侧 cache-aside 读穿的**顺序**（先缓存、再库、最后回填）与回填载荷就是库存那一行的 JSON；命中不查库、缓存里的旧进度胜出；查无此行返回哨兵且**不写负缓存**；脏缓存直接报错、既不回源也不清键；缓存读失败不降级回源；回填失败被 `_ =` 吞掉 |
| `gettemplate_test.go` | 11 | 0 | 与 `GetTask` 同构的读穿序，另单独钉住三处差异：键前缀 `tc:tpl:`（与 `tc:task:` 同 id 也互不可见）、失败哨兵是 `ErrTemplateNotFound`、模板侧**没有失效路径**所以脏值窗口是 600s 而非 60s；应答不带时间列（proto 字段漂移哨兵） |
| `listtasks_test.go` | 12 | 2 | `asset_id`/`state` 两个过滤条件「`<=0`/`=0` 即不过滤」的真实后果（非正 `asset_id` 等于查全表）；`ORDER BY task_id DESC`；`COUNT` 为 0 时不发第二条 SQL；`ps` 上限两侧与 `pn/ps` 兜底发生在 model；越界页返回空列表但 `total` 照报；SELECT 失败时 `total` 被丢弃；全程不碰缓存 |
| `listtemplates_test.go` | 10 | 2 | `ORDER BY template_id ASC`（与任务列表方向**相反**，任一条写反都会被对照用例抓到）；`COUNT` 的轨迹键没有任何条件位（`ListTemplatesReq` 只有 pn/ps）；空表只发一条 COUNT 且应答是空数组不是 null；列表完全不碰缓存 ⇒ **与 `GetTemplate` 对同一模板给出两个码率**（缺口 11 的核心断言） |
| `createtemplate_test.go` | 7 | 3 | 唯一一条守卫是 `name != ""`，因此同时钉「合法侧确实落库并回显自增 id」与「全空格/超长 name、非法档位原样入库」；`uniq_name` 冲突没被翻译成本域哨兵 ⇒ 返回驱动原始 1062 文本且不落第二行（并钉住冲突那次已消耗自增号 ⇒ 下一条是 903）；创建阶段一条缓存调用都不发；应答是投影而不是库存指针 |
| `fakes_test.go` | 0 | 0 | 替身层与断言工具（`wantSeq`、`st.task(id)`/`st.tpl(id)`、插队钩子与 `checkHooks`），不贡献用例数；装配口径见第 4 组 |

### 2. 其他层

- `internal/config`：1 个文件 `1/1` —— `config_load_test.go` 逐个加载 `etc/*.yaml`（子用例按文件名展开），
  反射递归断言 `*DataSource` 与 `redis.RedisConf.Host` 非空；缓存字段必须叫 `CacheRedis`，
  与 `zrpc.RpcServerConf` 内嵌的同名字段撞车会让服务启动即报 `conflict key redis`。
- `internal/repository`（`Cacher`/`NewWithDeps` 注入缝的宿主）**无离线单测**——只被 logic 用例经由它跑。
- `model/`（`transcodemodel.go`、`errors.go`）**无离线单测**：动态 `WHERE 1=1`、两条相反的 `ORDER BY`、
  `RowsAffected==0 ⇒ ErrTaskNotFound` 这些只有语义复刻，SQL 文本与列名无门禁。
- `internal/svc` **无离线单测**；`internal/server`、`rpc/*.pb.go`、`rpc/*_grpc.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层：消费 `media.task.v1` 的 Worker 本期不实现
  （见开头约束与缺口 1），因此「MQ 消息 ⇒ 建任务 ⇒ FFmpeg ⇒ 回调进度」这条链的任何一段都既没有代码也没有测试。

### 3. 构造器级覆盖：**7/7**

探针取 `internal/logic` 全部 `New*Logic(`，共 7 个（`SubmitTask`、`GetTask`、`ListTasks`、`UpdateProgress`、
`ListTemplates`、`GetTemplate`、`CreateTemplate`），`gaps:` 为空——七个 RPC 方法都有直接驱动自身构造器的用例，
不存在只有间接断言的方法。

### 4. 替身层与断言口径

- 注入缝：`internal/repository/repository.go` 的 `Cacher` 接口（`:111-118`）与 `NewWithDeps(...)`
  （`:139-142`），`New` 仍走 `NewCache(rds)`（`:131-134`），生产路径未变。用例用
  `NewWithDeps(内存缓存, fakeConn, 内存 taskMd/tplMd)` 组装**真实的 Repository**，只替换它的 4 个依赖，
  因此 cache-aside 读穿/回填/失效、终态判定读的是缓存还是库、UPDATE 有没有 CAS 都落在被测路径上
  （口径同 comment / rights / playback）。
- 五条替身纪律（`fakes_test.go` 头部）：每次读返回值拷贝；副作用按顺序记进 `callLog`，断序列而不是次数；
  错误注入按**依赖.方法**粒度（`st.fail("transcode_task.UpdateProgress", err)`）而不是一个全局 err；
  布数据走静默写入路径（`seedTask`/`seedTemplate`/`warmTaskCache`），所以序列从 0 数起；
  并发窗口用 `before(key, 第 n 次调用, fn)` 钩子复现，用例结尾 `checkHooks` 保证钩子真的触发过，
  否则断言在空转。
- Redis 键名（`tc:task:%d` / `tc:tpl:%d`，`repository.go:26-27`）与「miss 返回 `hit=false` 且无错误」
  逐字复刻，不一致时缓存命中类用例即红；`List` 的 `pn<1→1`、`ps<1||ps>50→20` 夹紧、动态 `WHERE 1=1`、
  任务 DESC 与模板 ASC、`COUNT` 先判 0、`RowsAffected==0 → ErrTaskNotFound`、`uniq_name` 冲突同语义复刻。
- 断言集中在三处：**完整调用序列**（表达「拒绝时一个依赖都没碰」「COUNT 为 0 不发第二条 SELECT」
  「UPDATE 之后先删缓存再回源」）；**库里实际残留什么**（表达「无幂等 ⇒ 两行」「无 CAS ⇒ 别人的写入被覆盖」
  「无条件 SET ⇒ `errno`/`err_msg` 被清零」）；**应答与库存行的逐字段投影**（任务 13 字段 / 模板 8 字段），
  新增 proto 字段时必须显式决定是否回填。
- **它证明不了**：SQL 文本、列名、索引与约束本身（要证明得引入 sqlmock，超出本轮范围）；
  TTL 常量藏在 `*Cache` 里（`repository.go:29-30`）、`Cacher` 签名不带 TTL，因此**没有用例覆盖秒数**。

### 5. 覆盖边界（如实声明）

- 用例不连接 MySQL、Redis、etcd、MQ、对象存储，也不依赖网络；不驱动 FFmpeg（本期无 Worker）。
- **不验证 `LIMIT/OFFSET` 切片的正确性**：fake 里的 `offset=(pn-1)*ps` 是把
  `model/transcodemodel.go:85,208` 的算式抄了一遍，该算式写错用例不会变红；翻页「不重不漏」
  只证明到与本文件同一套算术自洽。
- 不覆盖真 Redis 的 `SETEX`/`DEL` 编码、`Repository.Ping`（`repository.go:145-147`）与
  `Cache.DelTemplate`（`repository.go:94-97`，全仓零调用方）。
- changed-rows 口径（缺口 14）在纯内存里证明不了：替身按 matched-rows 复刻，因此**没有**对应用例，
  只有下面的缺口登记表。
- 转码回调链路只有**离线判定**被验证：`UpdateProgress` 的状态门槛、覆盖行为与调用序是被测结论；
  「Worker 真的转完了」「进度数字来自真实 FFmpeg」「MQ 投递与重试」都不在覆盖内，
  任务状态由调用方自报（`submittasklogic.go:40-53`，`ctime`/`mtime` 可伪造）。
- 迁移 SQL ↔ 真实库的列级对账：本 README 没有声明过在隔离实例 `127.0.0.1:3399` 复验，
  因此按**未在目标实例复验**处理，也不得据此声称已验证；迁移文件的存在与登记只以
  `deploy/migrations/README.md` 为权威。
- `internal/server`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内；改契约先改
  `rpc/transcode.proto` 再执行统一生成（`docs/commands.md`）。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/transcode/...
gofmt -l services/transcode
go vet ./services/transcode/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下，并发链接多个测试包会因内存耗尽失败（`errno=1455`）；
  `-count=1` 关闭测试缓存。
- 口径说明：`go vet` 期望无输出、`gofmt -l` 期望为空列表。五道门禁统一串行执行，
  本节只登记命令与口径，不在文档里代为声明执行结论。

## 已知缺口（logic 单测读生产代码时确认，均需契约或设计裁决后再改）

下列条目 **2~13** 由 `internal/logic` 用例**钉住当前行为**（不是期望行为），用例名见每条末尾；
修法落地时相应用例必须改成断言「正确结果」，否则会静默保留旧口径。
条目 **1** 是契约层登记项（没有可钉住的校验代码），条目 **14** 在纯内存里证明不了、只登记不钉住。
`file:line` 均为登记时实际读过的代码位置。

1. **写侧两个方法没有任何身份/归属校验，且本仓内没有调用方**。`SubmitTaskReq`（`rpc/transcode.proto:23`）
   与 `UpdateProgressReq`（`rpc/transcode.proto:69`）没有 MID、operator、token 之类的字段，
   logic 也不校验任务归属（`internal/logic/submittasklogic.go:30-39`、`internal/logic/updateprogresslogic.go:30-41`）：
   任何能连到 `:8100` 的进程都能给任意 `asset_id` 建任务、把任意 `task_id` 推到 SUCCEEDED，
   进而满足 §8 的 `READY_FOR_REVIEW` 前提。读侧目前只在网关有约束
   （`gateway/admin/internal/handler/routes.go:303-316` 给 `POST /admin/transcode/templates` 挂了
   `AdminPermission`），RPC 面本身无约束。另注：全仓（含 gateway）找不到
   `Transcode.SubmitTask`/`Transcode.UpdateProgress` 的调用点——Worker 未实现 ⇒ 这两条写路径
   只有裸 RPC 暴露面，没有真实的可信调用方可依赖。
2. **空 bucket/key 报的是 `ErrInvalidState`**。`submittasklogic.go:37-39` 用状态哨兵表达「字符串参数缺失」，
   调用方按 `ErrInvalidState` 分支处理时会把「忘了填 output_key」误读成状态非法。
   用例：`TestSubmitTask守卫先于触库`（钉住哨兵本身）、`TestSubmitTask守卫顺序是asset再template再字符串`。
3. **`SubmitTask` 既不幂等也不校验存在性**。`transcode_task` 除主键外没有任何唯一约束
   （`deploy/migrations/transcode/000001_create_transcode_tables.sql:22-25` 只有三个普通 KEY），
   `submittasklogic.go:55` 只做一次 INSERT，也不查 `template_id`/`asset_id` 是否存在：
   同一请求重放必得两个 `task_id`（同一份媒资转两遍），指向不存在模板的孤儿任务也能建成功。
   用例：`TestSubmitTask重放会创建两条任务`、`TestSubmitTask不做任何存在性校验`；
   钉住的对照：`TestSubmitTask创建PENDING任务并回填自增主键`。
   修法要加幂等锚点（如 `uniq(asset_id, template_id, input_key)`）⇒ 需 schema 裁决。
4. **`UpdateProgress` 的丢失更新检测不到**。校验读是 `Repository.GetTask`
   （`internal/logic/updateprogresslogic.go:44`），既不带事务 session 也不锁行；
   UPDATE 只有主键条件、没有 `AND state = ?` 的 CAS（`model/transcodemodel.go:126`）。
   因此「守卫读之后、UPDATE 之前」落地的终态会被无条件覆盖并返回成功
   （例：Worker B 刚落地的 FAILED + `errno=137` 被 Worker A 的 SUCCEEDED 抹掉，失败原因丢失）。
   用例：`TestUpdateProgress终态在守卫读之后被插队则丢失更新被判成功`；
   对照（同一份数据落在守卫读**之前**会被检出并零写入）：`TestUpdateProgress终态在守卫读里被检出则零写入`。
   同一缺口还有第二条触发路径：守卫读命中 60s 缓存（`updateprogresslogic.go:44` → `repository.go:162-183`），
   库里已是终态也照样放行 ⇒ `TestUpdateProgress的校验读走缓存所以终态可被回退`。
   修法：校验读走 session，或 `UPDATE ... WHERE task_id=? AND state IN (1,2)` 后按 `RowsAffected==0` 重试。
5. **`asset_id<=0` 是「查全表」而不是「查不到」**。`model/transcodemodel.go:90-93` 的
   `if assetID > 0` 让 0/-1/-5001 一律不加 WHERE，于是 `ListTasks` 会返回**所有媒资**的任务
   （含别人的 `input_key`/`output_key`）。用例：`TestListTasks非正asset_id等于不过滤`；
   对照：`TestListTasks按asset过滤且倒序返回`（`asset_id=11` 只返回 2 行）。
   现状影响面有限：app 侧唯一调用方总会传具体 assetID
   （`gateway/app/internal/logic/playbacksource.go:57-62`），但运营面与直连 RPC 无此保证。
6. **分页兜底有一半不可达**。`listtaskslogic.go:29-31` 与 `listtemplateslogic.go:29-31` 先拒
   `ps<0 || ps>50`，于是 `model/transcodemodel.go:82-84,205-207` 的 `ps > 50 → 20` 分支永远走不到；
   而 `ps=0` 被当作合法值兜底成**每页 20 条**（不是空页也不是报错），`pn<=0` 兜底成第 1 页。
   错误文案 `transcode: ps exceeds 50` 对负数也是同一句。
   用例：`TestListTasks的ps上限两侧`、`TestListTasks的pn与ps兜底发生在model`、
   `TestListTemplates的ps上限两侧`、`TestListTemplates的pn与ps兜底发生在model`。
7. **`ErrInvalidTransition` 只有脏数据行才可达**。`model/errors.go:36-46` 的白名单只放过
   `state IN (1,2)`，而 logic 侧已保证目标 state ∈ {2,3,4}、`model.IsTerminal` 已先拦掉 3/4
   （`updateprogresslogic.go:52-59`），所以合法写入永远不会命中这条哨兵。
   可达的唯一前提是库里 `state` 是脏值——而 `transcode_task.state` 的列默认值就是 0
   （`000001_create_transcode_tables.sql:16`），且这种行**任何按状态过滤的查询都取不到**
   （`state > 0` 才加条件，见缺口 5 的同一段代码），只能被人工 UPDATE/DELETE 解救。
   用例：`TestUpdateProgress对脏state的行才报ErrInvalidTransition`（对照同请求打到 `state=1` 即成功）、
   `TestListTasks的state等于0无法表达未指定态`。
8. **`UpdateProgress` 可以「成功」但返回全零应答**。`repository.go:192-198` 在 UPDATE 之后
   直接 `return r.taskMd.FindOne(...)`，而 model 查无此行返回 `(nil, nil)`
   （`model/transcodemodel.go:66-76`），logic 再交给 `toTaskReply(nil)`
   （`internal/logic/convert.go:10-13`）⇒ 应答 `task_id=0`、`state=UNSPECIFIED`、`err=nil`。
   调用方看到成功，但推进稿件状态机拿到的是空任务。
   用例：`TestUpdateProgress回读为nil却返回空应答成功`；对照（行仍在时是完整应答）：
   `TestUpdateProgress从PENDING推到PROCESSING留下六步轨迹`。修法：回读为 nil 时返回
   `ErrTaskNotFound`（属语义收敛，需与 Worker 侧重试策略一起定）。
9. **`UpdateProgress` 写侧不校验进度语义，且五个列无条件覆盖**。`updateprogresslogic.go:34-36`
   只判 `0<=progress<=100`，所以进度能从 90 退回 10；`state=SUCCEEDED` 不要求 `progress=100`
   （可以只转完 3% 就报成功）；`model/transcodemodel.go:126` 的 SET 总会把 `errno`/`err_msg`
   写成请求里的值，Worker 漏传即等于**抹掉既有失败原因**。
   用例：`TestUpdateProgress进度可以回退`、`TestUpdateProgress成功态可以与进度不一致`、
   `TestUpdateProgress漏传errno会抹掉既有失败原因`（两条子用例覆盖「不传 ⇒ 清零」与「带完整值 ⇒ 覆盖」）。
   另注：`mtime` 由 logic 取本地时钟（`updateprogresslogic.go:61`），不是数据库时间。
10. **三处缓存副作用被静默吞掉，脏缓存也没有清键路径**。回填失败 `_ =`（`repository.go:180`、
    `repository.go:226`）、失效失败 `_ =`（`repository.go:196`）都不影响返回值；而缓存命中但
    JSON 坏掉时 `repository.go:167-169,213-215` 直接上抛，既不回源也不删键。
    后果分两级：任务侧最长 60s 恒定 500（下一次 `UpdateProgress` 的 `DelTask` 才会救回），
    模板侧 600s 内**没有任何 RPC 能救**（缺口 11）。
    用例：`TestGetTask回填失败被吞`、`TestGetTask脏缓存直接报错既不回源也不清键`、
    `TestUpdateProgress删缓存失败被吞`、`TestUpdateProgress的UPDATE失败时不删缓存`、
    `TestGetTemplate回填失败被吞`、`TestGetTemplate脏缓存报错后无人能清键`。
    判别性对照：`TestGetTask缓存读失败时不降级回源`（Redis 抖动会把读接口整体打挂，不降级）。
11. **模板缓存没有失效路径，且与列表读口径不一致**。`repository` 的类注释写着「模板详情长 TTL
    （600s），创建时刷新」（`repository.go:4`），但 `CreateTemplate` 一条缓存调用都不发
    （`repository.go:232-238`），`Cache.DelTemplate`（`repository.go:94-97`）全仓零调用方、
    也没进 `Cacher`（`repository.go:111-118`）。于是 `GetTemplate` 的 600s 窗口内只能读旧值，
    而 `ListTemplates` 走裸 SQL（`repository.go:203-205`）⇒ **同一 template_id 在两个读接口上
    给出两个码率**，且没有任何接口能消掉这个分歧（人工改 `transcode_template` 时同样命中）。
    用例：`TestListTemplates与GetTemplate对同一模板给出不同码率`、
    `TestGetTemplate缓存里的旧码率胜出`、`TestCreateTemplate后详情读仍需回源一次`、
    `TestCreateTemplate落库并回显自增id`（钉住创建阶段 `cache.*` 调用数为 0）。
12. **`CreateTemplate` 只有一条守卫，重名的 1062 没被翻译**。`createtemplatelogic.go:29-31` 只判
    `name != ""`：`codec` 空串/乱码、`width`/`height`/`bitrate`/`fps`/`segment_seconds` 的负数与
    21 亿全原样入库，全空格 name 也算合法；`name` 超长只能由 MySQL 决定（列是 VARCHAR(128)，
    `000001_create_transcode_tables.sql:33`，strict 模式下 error 1406、非 strict 静默截断 ⇒ 替身证明不了）。
    重名时 `model/transcodemodel.go:180` 只做 `%w` 包装，返回的是驱动原始文本
    （`Error 1062 ... uniq_name`，`000001_create_transcode_tables.sql:43`），
    没有任何「已存在」域哨兵 ⇒ 运营面区分不了「重名」和「库挂了」，只能透传 500。
    用例：`TestCreateTemplate只校验name非空`（末尾含空 name 的对照）、
    `TestCreateTemplate的空白与超长name都入库`、`TestCreateTemplate重名返回驱动原始错误且不落第二行`
    （同时钉住「冲突那次已消耗自增号 ⇒ 下一次是 903」）。
    同一侧缺口在读侧的那一半：`ListTemplatesReq` 只有 pn/ps 两个字段（`rpc/transcode.proto:90`），
    模板列表的 COUNT 没有任何条件位（`model/transcodemodel.go:211-212`），
    所以运营面只能全表翻页、按名字找档位得自己扫页
    （用例：`TestListTemplates的COUNT无条件位`）。
13. **模板详情应答不带时间列，任务详情带**。`rpc.TemplateReply` 只有 8 个字段
    （`rpc/transcode.proto:78-87`），而 `transcode_template` 有 `ctime`/`mtime`
    （`000001_create_transcode_tables.sql:40-41`）⇒ 运营面无法从任何 RPC 判断档位是什么时候建的；
    反过来 `transcode_task` 的 `ctime`/`mtime` 由调用方自报（`submittasklogic.go:40-53`，可伪造），
    模板侧却由 model 取时钟（`model/transcodemodel.go:175,178`，不可伪造）——同一服务两套口径。
    用例：`TestGetTemplate应答不带时间列`（proto 字段漂移哨兵）、
    `TestCreateTemplate落库并回显自增id`（钉住 `ctime == mtime` 且落在当前时刻）。
    补齐要动 `TemplateReply` ⇒ 属契约变更。
14. **同秒重复上报完全相同的进度，在真库上会被判「任务不存在」（未钉住，仅登记）**。
    本服务 DSN 未开 `clientFoundRows`（`etc/transcode.v1.yaml:14`），MySQL 的 `RowsAffected`
    按 changed rows 计数，`model/transcodemodel.go:131-137` 却把 `0` 翻译成 `ErrTaskNotFound`；
    于是「Worker 在同一秒重发一条与库里完全一致的进度」这条合法路径会拿到 not-found。
    纯内存替身按 matched-rows 复刻（否则要靠墙钟恰好同秒，用例不稳定），因此**没有**用例钉住它，
    修法与缺口 4 的 CAS 改造可以一起做（改用 `clientFoundRows`，或按 `mtime` 显式判定）。

