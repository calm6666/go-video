# creator

UP 主/机构创作者身份和创作权限服务。

## 职责

- **拥有数据**：创作者认证、机构资料、投稿权限、内容配额、创作中心摘要、
  特殊用户组（up_group）、UP 特殊属性（up_special）、UP 身份属性（up_attr）、
  关注弹窗开关（up_switch）、高能联盟签约（sign_up）。
- **提供能力**：申请/审核创作者身份、认证状态、投稿授权、创作者资料卡、
  UP 主特殊属性/分组查询、UP 身份判定、关注弹窗开关、高能联盟签约查询。
- **依赖**：`account`、`user-profile`、`moderation-orchestrator`。
- **约束**：认证通过不是稿件审核通过；不得在本服务存储视频主数据。

## 目录结构

```text
services/creator/
├── rpc/creator.proto              gRPC 源契约（8 个方法）
├── etc/creator.v1.yaml            配置示例
├── model/                         5 张表的实体与查询接口
│   ├── up_group.go                特殊用户组
│   ├── up_special.go              UP 特殊属性（mid → group_id 列表）
│   ├── up_group_member.go         up_special 反向索引（group_id → mids 分页）
│   ├── up_attr.go                 UP 身份属性
│   ├── up_switch.go               关注弹窗开关
│   └── sign_up.go                 高能联盟签约
├── internal/
│   ├── config/                    配置（RpcServerConf + Redis + MySQL）
│   ├── server/                    goctl 生成的 RPC server
│   ├── logic/                     8 个 RPC logic
│   ├── repository/                MySQL + Redis 缓存
│   └── svc/                       ServiceContext
├── creator.v1.go                  RPC 入口（无 HTTP server）
└── README.md
```

## gRPC API（8 个方法）

移植自参考仓库 `openbilibili-go-common/app/service/main/up`。依据 `AGENTS.md` §5
数据所有权约束，obc up 服务的稿件相关方法（UpArcs/UpsArcs/UpsAidPubTime/
UpCount/UpsCount/AddUpPassedCache*/DelUpPassedCache*/UpBaseStats/
UpInfoActivitys 共 11 个）不移植到本服务，由 `services/video`、
`services/engagement`、`services/spm` 等域承接。

| 方法 | 用途 |
|---|---|
| `UpSpecial` | 查询单个 UP 主特殊属性 |
| `UpsSpecial` | 批量查询 UP 主特殊属性（≤100） |
| `UpGroups` | 查询所有特殊用户组 |
| `UpGroupMids` | 查询某分组下的用户（分页） |
| `UpAttr` | 查询 UP 主身份属性（0 稿件作者/1 移动投稿/2 直播 UP/3 直播白名单） |
| `SetUpSwitch` | 设置 UP 主关注弹窗开关 |
| `UpSwitch` | 查询 UP 主关注弹窗开关 |
| `GetHighAllyUps` | 查询高能联盟 UP 主签约信息 |

## 配置

参考 [etc/creator.v1.yaml](etc/creator.v1.yaml)：

- `ListenOn`：gRPC 监听地址（默认 0.0.0.0:8086）
- `Etcd`：服务注册（key: `creator.v1.rpc`）
- `Redis`：缓存（特殊属性/身份/开关/分组列表）
- `DataSource`：MySQL DSN（`creator` 库 5 张表）

## 健康检查

- gRPC 服务通过 etcd 注册可发现性。
- `repository.Ping(ctx)` 校验 Redis 连通性。
- 端到端健康检查由 `gateway/app` 或 `gateway/admin` 通过 RPC 调用验证。

## 数据表

- `up_group(id, name, tag, short_tag, font_color, bg_color, note)`
- `up_special(mid, group_id)` —— 索引 `(mid, group_id)`、`group_id`
- `up_attr(mid, from, is_author)` —— 主键 `(mid, from)`
- `up_switch(mid, from, state)` —— 主键 `(mid, from)`
- `sign_up(mid, state, begin_date, end_date)` —— 主键 `mid`

SQL 迁移由 `deploy/migrations` 统一管理。

## 测试覆盖

离线单测（纯 Go 替身，不连 MySQL/Redis/etcd，也不起 gRPC server）。数字来自
`grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 的实测导出，格式 `顶层/子用例`。
规模：`internal/logic` 6 个文件 `73/16`（5 个用例文件 + `fakes_test.go` `0/0` 替身层）、
`internal/config` 1 个文件 `1/1`；`t.Skip` 0 条。

### 1. `internal/logic` 用例清单

| 文件 | 顶层/子 | 覆盖方法 | 钉住了什么 |
|---|---|---|---|
| `internal/logic/upswitch_test.go` | 18/3 | `SetUpSwitch` / `UpSwitch` | 写后再读必须读回同一个值，而且这次「读回」是**真的回库**（不是回显入参）；`from`/`state` 枚举越界一律拒、且拒绝发生在任何依赖调用之前、不留半行；`mid<=0` 不得凭空插出一行「看起来合法」的开关；缓存方向只有「读回填 + 写失效」，失效必须是 `Upsert` 之后 `DEL`（顺序反过来就是脏读）；缓存/DB 报错 `errors.Is` 原样透传，绝不伪装成「查无此人」的默认关闭；已修的 int64 `mid` 截断有回归锁 `TestSetUpSwitchKeepsFullInt64Mid`；复现 D1、D2 |
| `internal/logic/special_test.go` | 16/3 | `UpSpecial` / `UpsSpecial` | 单条路径的回填只写「查到的那一组」，空表回填 `[]`（不是 `cache.go` 的 `{}` 空标记），两者读侧都算命中 ⇒「查无此人」同样被防击穿挡住最长 1h；缓存脏值与缓存故障都不被伪装成「该 UP 没有特殊分组」（错误原样上抛、一次都不回库）；批量路径**完全不碰缓存**（策略，`repository.go:118-125`），所以批量与单条之间不存在「批量读到旧快照」，但批量也不给单条预热；批量对非法/不存在元素是**逐条部分成功**（应答 map 里缺席），上限 100 是整批拒绝（`errTooManyMids`）且先于任何依赖调用；重复提交（含重复 mid）幂等，IN 是集合语义、既不翻倍也不写库；复现 D3 |
| `internal/logic/groups_test.go` | 18/3 | `UpGroups` / `UpGroupMids` | 两条路径都是「缓存命中就不回库、回源后按**代码选的 TTL** 回填」（分组 300s、成员 60s）；缓存里的脏 payload **不静默降级回库**：错误带 `unmarshal cache` 归属并原样上抛，一次 SQL 都不发（否则脏数据能永久掩盖 DB 侧真实形态）；空表/空页也被缓下来（防击穿），且「空」在应答里是 `[]` 而不是 nil；`total` 与 `mids` 是两件事——越界页返回空 `mids` 但保留真实 `total`；分页键 `(group_id,pn,ps)` 各自独立缓存，翻页不互相命中；复现 D5、D6 |
| `internal/logic/attr_test.go` | 11/6 | `UpAttr` | `from` 的合法域是 `0..3`（与开关域的 `0/1` 不通用），越界一律 `errInvalidFrom` 且拒绝前零依赖调用；「有行但 `is_author=0`」与「根本没行」应答都是 0，但**缓存留下的形态不同**（`"0"` vs `"{}"`：前者是一次真实查询的结果，后者是防击穿空标记），两者都不回库直到 TTL 到期；脏缓存值不被当成「默认无身份」，`Atoi` 的错误原样上抛、一次都不回库；回填/写空标记失败被忽略（`_ =`）但绝不能留下半成品值；`mid` 全程 int64，BIGINT UNSIGNED 的大 mid 不得被截断（与已修的开关截断同一类） |
| `internal/logic/highally_test.go` | 10/1 | `GetHighAllyUps` | 空 `mids` 在 logic 就短路（零依赖调用），应答是空 map 而不是 nil；批量不缓存——两次相同请求各发一次 SQL，路径上没有任何缓存调用（`cache.go:23` 的 `up:ha:%d` 至今无人使用），所以既不会读到旧快照也不会把结果预热给别的域；应答 map 的键集合＝「被查且库里真有行」的 mid 集合，缺失的 mid **缺席**、绝不出现 `state=0` 的占位值（否则调用方会把「查无此人」读成「已终止的签约」）；**不按 `state` 或时间窗过滤**，到期/终止照原样返回、判定归调用方；错误原样透传且**不带归属前缀**（`repository.go:252` 是裸 return，与 `UpSpecial`/`UpAttr` 不同），这一差异被钉住以免有人改成 `%w` 包装后调用方的错误分类悄悄变化；复现 D4 |

`internal/logic/fakes_test.go`（`0/0`）是替身与注入缝层，见第 4 组，不是漏计的用例文件。

### 2. 其他层

- `internal/config`：`config_load_test.go` `1/1` —— `TestExampleConfigsLoad` 用 `conf.Load` 真实加载
  `etc/` 下每个示例配置（循环内一条 `t.Run`），拦住「`Config` 自带的 `Redis` 字段与
  `zrpc.RpcServerConf` 内嵌的 `Redis` 同名 → 代码可编译但启动即 `conflict key redis`」这一类
  只有真实加载才暴露的问题。
- `model/`：**无离线单测**。6 张表的 SQL 文本、列宽、`sql.ErrNoRows` 翻译没有 model 层用例，
  本服务也没有 `model/migration_parity_test.go` 一类的迁移↔model 列级对账门禁。
- `internal/repository/`：**无独立离线单测**。用例通过 `repository.NewWithDeps(...)` 组装
  **真实的 Repository**（只替换它的依赖），所以缓存 key 派生、回源与 TTL 选择、失效顺序
  这些 repository 侧口径是在 `internal/logic` 用例里被**间接**执行的，`repository` 包自身
  没有测试文件。
- `internal/svc`：**无离线单测**（只做依赖装配）。
- 本服务没有 `internal/consumer`、`internal/policy` 目录（无 MQ 消费者与策略层）。

### 3. 构造器级覆盖

**8/8**：探针取 `internal/logic` 全部 `New*Logic(`（对应上面「gRPC API（8 个方法）」），
逐个在 `*_test.go` 里查引用，无缺口——8 个方法的入口都至少被一个用例调用过。

### 4. 替身层与断言口径

注入缝是 `repository.Cacher` 接口 + `repository.NewWithDeps(...)`（与 `services/comment` 同一取舍）：
`ServiceContext.Repository` 是具体类型 `*repository.Repository`，生产构造走 `repository.New`
（真 Redis + 真 MySQL），所以用例统一用**内存缓存 + 内存 model** 组装真实 Repository，
让「缓存读穿/回填/失效顺序、空标记 vs 真 miss、脏缓存、`IN` 列表口径」整条判定链都在被测路径上，
而不是把 Repository 也 mock 掉（见 `internal/repository/repository.go` 的 `Cacher` 注释）。

`internal/logic/fakes_test.go` 文件头记的四条替身纪律（`catalog`/`rights`/`playback`/`comment`
几轮踩过的坑）与复刻的语义：

1. 每次读都返回**值拷贝**——logic/repository 就地改字段不得污染库存行，否则「有没有真的落库」
   这类断言会被共享指针掩盖。
2. 副作用按**顺序**记录（`callLog`，条目 `<表>.<方法>:<键>`，缓存条目用真实 Redis key 当键），
   断的是序列而不只是次数——本服务要紧的结论全是顺序与口径类的
   （写必须 `Upsert`→`DelSwitch`、批量不得碰缓存、守卫拒绝后零依赖调用）。
3. 错误注入按方法粒度，且能指定**第几次调用**发作（`failWith` 每次都发作 / `failOn` 只在第 n 次）——
   「第二次读才炸」这类用例是缓存回填与回源支路唯一的可证伪写法。
4. 布数据（`seedXxx`/`warmXxx`）走**静默路径**、不写 `callLog`：序列断言里出现的每一项
   都是被测代码的调用。

替身的 SQL / 缓存语义逐条对齐源文件（文件头标了 file:line）：`model/*.go` 的 `WHERE`、`ORDER BY`、
`LIMIT/OFFSET`、`sql.ErrNoRows` 翻译，以及 `internal/repository/cache.go` 的 key 拼装
（`up:spec`/`up:attr`/`up:sw`/`up:groups`/`up:gm`）。

它**证明不了**：真实 SQL 文本与列名、索引是否被命中、MySQL 的行锁与唯一键行为、
真 Redis 的 TTL/过期语义（内存缓存按代码选的 TTL 记账而不是按 Redis 行为）、
驱动返回的 matched vs changed rows。表结构侧的把握只能来自迁移 SQL 与目标实例，见下一组。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL/Redis/etcd/MQ/对象存储，也不起真 gRPC server；`account`/`user-profile`/
  `moderation-orchestrator` 三个下游（见「职责」）在本包用例里不被调用，本服务当前没有跨服务出访。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有隔离实例（`127.0.0.1:3399`）
  列级复验的声明，`model/` 与 `internal/repository/` 也没有离线单测（第 2 组），
  所以「列宽够不够、`up_switch.mid` 是不是 BIGINT UNSIGNED、索引在不在」这类结论目前
  只由 `deploy/migrations/creator` 的 SQL 文本与代码注释承载，改表时必须按 `docs/commands.md`
  的迁移流程在隔离实例复跑并核对。建表应用状态以 `deploy/migrations/README.md` 的登记表为准。
- `internal/server`、`rpc/*.pb.go`、handler/server 壳与入口模板等 goctl 生成物不在单测范围内。
- 未修缺陷的权威登记在下面的第 6 组（本 README 没有独立的「已知缺口」节）。

### 6. 缺陷登记（用例按现状钉住）

注入缝与缺陷登记的逐条 file:line 见 `internal/logic/fakes_test.go` 文件头。

已修：`SetUpSwitch` 链路把 int64 `mid` 截断成 int32（logic→repository→model 三处），
而 `up_switch.mid` 是 BIGINT UNSIGNED（`deploy/migrations/creator/000004_create_up_switch.sql`）——
`mid > 2^31-1` 时写到**别人的行**、失效**别人的缓存键**，而读侧用完整 mid，于是「改完开关读回旧值」。
回归锁：`TestSetUpSwitchKeepsFullInt64Mid`（把 `int32(in.Mid)` 加回去即红，已实测）。

未修缺陷（用例按现状钉住，注释标 `pin`；修法定位到 `fakes_test.go` 的 D1–D6）：

- **D1** `UpSwitch` 回填存在「读到旧值→写缓存」竞态窗口，可把已被并发写覆盖的开关再钉成旧值，最长 24h
  （`internal/repository/repository.go:222-230`）。
- **D2** `SetUpSwitch` 在「DB 已写成功、`DelSwitch` 失败」时整体回错：调用方看到失败，库里已是新值
  （`repository.go:237-241`）。幂等重试可自愈，故不改成吞错误。
- **D3** `UpSpecial` 没有 `mid > 0` 守卫（同包其余读方法都有），非法 mid 照常触库并写进 `up:spec:0`
  （`internal/logic/upspeciallogic.go:28-37`）。
- **D4** `GetHighAllyUps` 对 `mids` 长度无上限（`UpsSpecial` 是 100），单请求可把任意长 IN 列表打到 MySQL
  （`internal/logic/gethighallyupslogic.go:29-36`）。上限值属容量决策，待与调用方一起定。
- **D5** 分组/成员/属性/特殊属性四类缓存**没有任何失效入口**（本服务无写 `up_special`/`up_group`/`up_attr`
  的 RPC）：运营面改表后最长 60s（成员）/300s（分组列表）/1h（属性、特殊属性）才可见。
- **D6** `UpGroupMids` 把分页归一化结果写回入参消息 `in.Pn`/`in.Ps`（`upgroupmidslogic.go:32-42`）：
  复用同一 request 对象的调用方会被悄悄改页码。

另外两处「形似缺陷但已确认是策略」的口径：`UpsSpecial`/`GetHighAllyUps` 批量直打 DB 不走缓存
（`repository.go:118-125`、`246-253`），且批量对非法元素是逐条部分成功（缺失的 mid 从应答 map 缺席）；
`cache.go` 里 `prefixHighAlly`/`DelSpecial` 至今无调用方。

### 7. 验证命令

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go test -p 1 -count=1 ./services/creator/...
gofmt -l services/creator            # 必须无输出
go vet ./services/creator/...        # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（errno=1455）。
契约变更后的重新生成命令见 `docs/commands.md`（禁止手改 `internal/server`、`rpc/pb`）。

## 运行

```powershell
# 本地依赖：etcd / mysql / redis 由 deploy/docker-compose 启动
go run services/creator/creator.v1.go -f services/creator/etc/creator.v1.yaml
```
