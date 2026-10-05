# 数据库迁移

迁移按服务隔离：`deploy/migrations/<service>/000001_description.sql`，`<service>` 与 `services/<service>` 目录同名。
执行方式和参数见 [docs/commands.md](../../docs/commands.md) §8（`scripts/migrate.ps1`）。

## 约定

- 一个服务一个目录，一个目录内按 `NNNNNN_动词_表名.sql` 升序追加；已应用的文件不再修改，需要变更就新增一号。
- 每个文件头部说明用途、数据所有者、回滚语句和锁风险；一张表一段 `CREATE TABLE IF NOT EXISTS`，可重复执行。
- 只允许服务自己读写本目录建出的表（AGENTS.md §5）；跨服务只存对方的业务主键引用，不建外键跨库。
- 大文件、对象存储地址和凭据不入库；`bucket`/`object_key` 只存引用，密钥进 Secret/Vault。
- 高频计数表（`*_stat`、`feed_unread`）是投影，允许从事实表或事件重算，不作为唯一事实源。
- 幂等写入依赖唯一索引 + `INSERT ... ON DUPLICATE KEY UPDATE`，建表时必须写出对应的 `UNIQUE KEY`。
- 文件保存为 UTF-8（中文注释不得用本地代码页保存）。
- 已应用版本记录在每个库的 `schema_migrations` 表，由 `scripts/migrate.ps1` 维护，不手工改写。
- `gateway/app`、`gateway/admin` 不拥有领域数据，因此没有迁移目录（AGENTS.md §3）。

## 当前覆盖

库名取自 `services/<service>/etc/*.yaml` 的 `DataSource`；迁移文件数只计 `*.sql`，不含各目录的 `README.md`。
「复验」列：`applied` = 已在隔离实例（`127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）逐个 `up` + `status` 跑通；
`pending` = 文件已写好但**尚未在任何实例执行过**（含隔离实例），上线前必须按 `docs/commands.md` §8
显式指定 host/port 执行；表格里的文件数是磁盘上的 `*.sql` 计数，与是否已执行无关。
下表 43 个目录中，阶段 1-2/3-5 的 38 个于 2026-09-21 全量为 `applied` 且 `status` 无 `pending`
（阶段 1-2 的 24 个服务在 2026-09-20 复验，阶段 3-5 的 14 个新服务在 2026-09-21 补跑；
`upload` 目录当时是 2 个文件、全量 applied，2026-10-05 新增的第 3 个文件未执行，见下表该行；
`video` 同理，当时 3 个文件全量 applied，2026-10-05 为 `content.published.v1` 新增的第 4 个文件未执行）；
商业化 5 个目录（`membership`/`payment`/`trade-order`/`coin`/`creator-revenue`）与 `operation` 的
`000004` 权限点种子在 2026-09-22 补跑，其中种子文件额外验证了「重复执行不产生新行」；
同日新增的 `operation/000005_seed_op_role_grants.sql`（派生角色与绑定）也在同一隔离实例上
`applied` + 手工重跑一次，口径见下面「数据种子迁移」一节。
**同日 open-platform 运营面接入时的再次就地修正 + 复验**：`000004` 追加 14 行 `domain='openplatform'`
（文件从 109 行扩到 123 行，权限点仍只就地追加这一个文件，不拆成 `000005` 的第二个权限点种子；
`000005` 这个号后来归**角色种子**占用，见下面第 2 条），按上面同一条命令重跑后隔离实例的
`op_permission` 由 **96 行 → 123 行**（`COUNT(DISTINCT resource, action)` 同为 123，14 行 openplatform
全部落库，中文描述列 `SELECT` 回读无乱码）。**96 这个数是本轮复验发现的真实漂移**：上一轮文档写「已按
109 行验证」，但库里缺的正是 spm（5）与 feature-store（8）两组——`schema_migrations` 按文件名记账，
`000004` 已记为 applied，`migrate.ps1 -Action up` 会直接跳过，所以**改就地修正过的种子文件后必须按
第 8 节那条 `mysql < file` 手工重跑**，`status` 显示 applied 不等于内容已是最新。
同一文件随后又被全仓门禁抓到**头部三处失真**（`TestMigrationFilesCarryOwnerAndRollback` 因缺少 `owner`
声明而失败；顺带修正了「routePermissions 110 条 / 去重 109 组」的过期计数——当时是 125 条 / 123 组，
以及回滚注释里的 `domain` 白名单漏了 `openplatform`，照原文执行会留下 14 行删不掉）。
这三处都只改注释、不改任何语句，因此重跑一次确认仍为 123 行、无新行。
**同日阶段 1-2 运营面补鉴权那一轮**（41 条写/敏感路由挂上 `AdminPermission`）又就地追加 000004 一次
（123 → 162 行，新增 13 个 domain），并新增 `000006_seed_op_role_grants_stage12.sql` 做第二轮角色并入。
本轮在隔离实例上的手工重放**踩到了客户端字符集这个坑**：`mysql` 不带 `--default-character-set=utf8mb4`
时按 GBK 解释文件里的 UTF-8 字面量，39 行中文描述写成乱码入库，随后 000006 的 `CONCAT` 直接报
`ERROR 1267 Illegal mix of collations (gbk_chinese_ci,COERCIBLE) and (utf8mb4_unicode_ci,IMPLICIT)`。
`migrate.ps1` 本身一直带这个参数（`Invoke-MigrationFile`），所以只有绕过脚本手工重跑才会中招。
修复方式只能是把那 39 行删掉再带正确字符集重放（`INSERT IGNORE` 不会覆盖已存在的乱码行）；
重放后 162/31/340、`op_admin_role` 仍 0、再跑一遍计数不变，细节见下面「数据种子迁移」一节。
**同轮做了一次全量头部对账**（当时 116 个 `*.sql`；阶段 1-2 那一轮之后同样的对账在 118 个文件上重跑，
命名/`owner`/回滚段落仍零缺项）：文件命名全部符合 `NNNNNN_verb_tables.sql`，
每条 `CREATE TABLE` 的表名都出现在同文件的回滚 `DROP TABLE` 清单里；缺项是 6 个早期文件
（`comment/000001`、`engagement/000001~000003`、`upload/000002`、`video/000003`）没有数据所有者声明、
其中前四个连回滚段落都没有，已按本节约定补齐（同样只改注释）。
注意自动化门禁 `services/operation/model/migration_sync_test.go` 只扫 `deploy/migrations/operation`，
且认的是字面量 `owner`；其余 42 个目录用中文「数据所有者」措辞，因此头部完整性目前靠这种对账人工把关，
不在门禁覆盖范围内。
表数含各库的 `schema_migrations` 台账表。
2026-09-22 另做了一次整体对账：43 个目录 ↔ 隔离实例上 43 个库一一对应，且每个库的实际表数
都恰好等于该目录 SQL 里 `CREATE TABLE` 语句数 + 1（`schema_migrations`），无缺表、无多表。
同日复核把这条从**计数级**升级为**表名级**：从各服务 `etc/*.yaml` 的 `DataSource` 取库名，
把 43 个目录里 `CREATE TABLE IF NOT EXISTS` 的 233 个表名逐条与 `information_schema.tables`
的 `table_schema`+`table_name` 做集合差，双向零差异（缺表 0、多表 0，唯一多余项就是每库一张
`schema_migrations`）；同时校出「覆盖表第 2 列库名 ↔ yaml 库名」「第 3 列文件数 ↔ 磁盘 `*.sql` 数」
也零漂移（43/43）。**2026-10-05 新增的两条 outbox 迁移是这条结论当前的例外**：
`upload/000003_create_upload_outbox.sql` 与 `video/000004_create_video_outbox.sql` 已写好但从未在任何实例执行，
因此隔离实例缺 `go_video_upload.upload_outbox`、`go_video_video.video_outbox` 这 2 张表，
按下表 `pending` 标记理解。本轮按同一条口径重新导出磁盘计数：43 个目录里非注释行的
`CREATE TABLE IF NOT EXISTS` 表名共 **234 个**（去重后仍是 234，即无跨目录重名），
而上面那句「233 个表名」是 2026-09-22 那次探针的实测值；两个数相差 1 而不是 2，
说明**要么 09-22 的集合已经含其中一张、要么两次计数口径有细微差别**，本轮没有连库、无法归因，
登记为待复验项：**这两条迁移执行后必须重跑表名级集合差探针，而不是照抄 233/234 任一数字**。
注意 `account`/`creator`/`user-profile` 三个早期移植服务的库名沿用
`account`/`creator`/`userprofile`，不带 `go_video_` 前缀，用前缀过滤库名的探针会漏掉它们。
同日 `payment`/`membership`/`trade-order`/`creator-revenue` 四个 `000001` 因新增的 model 漂移门禁补了**注释级**修补
（目标库声明、枚举逐值、单位「分」、退款去向、唯一性口径与索引对齐），
列类型、列名、索引、约束一律未动。这些文件**未提交、也未在任何共享实例应用过**，属就地修正范围；
每个改过的文件都在隔离实例用一次性 scratch 库（`go_video_payment_parity`、`go_video_*_scratch`）
从零建表复验：`mysql --default-character-set=utf8mb4` 客户端字符集必须显式给出，否则中文注释会按连接默认字符集
二次编码存进 catalog（实测踩过一次，dump 出来是乱码而文件本身是好的），复验后即删该库，
`go_video_*` 正式库未被改动。**因此隔离实例上 2026-09-22 之前建好的那三个库仍是修补前的列注释文本**
（结构与索引完全一致，`schema_migrations` 不记 checksum 所以 `status` 照旧 `applied`）；
要让它们与文件逐字一致，只能按 `docs/commands.md` §8 就地 `ALTER ... MODIFY COLUMN` 刷注释，或重建该库。
**同日 event_type 命名与信封契约收口那一轮又做了三处注释级修补**：`live-media/000003`（`live_media_outbox.event_type`
的枚举注释改成点号分段名，并补上此前漏写的第 9 个 `livemedia.record.state.changed`）、
`user-profile/000004`、`user-profile/000010`（`user.profile_updated`→`user.profile.updated`、
`user.moral_notice`→`user.moral.notice`）。原因不是风格：`common/eventenvelope` 只接受小写字母/数字/点号，
旧的下划线名会让**每一次 Outbox 写入连同业务事务一起回滚**（详见 `services/live-media/README.md` §3 与
`docs/api-and-events.md` §5）。这三处同样不动列类型/索引/约束，因此**不开新的版本号**；
隔离实例上这两张表的列注释仍是改名前的文本（`schema_migrations` 不记 checksum，`status` 照旧 `applied`），
按同一条 `ALTER ... MODIFY COLUMN` 才能刷齐。两表行数实测均为 **0**（`live_media_outbox` 0、
`userprofile.member_outbox` 0），所以**不存在需要回填的历史事件行**，也没有外部订阅方按旧名在消费。
表与 SQL 的一致性由可再生成的门禁守着，不靠人工比对：
`services/<svc>/model/migration_parity_test.go`（`payment`/`coin`/`membership`/`trade-order`/`creator-revenue` 各有其一）
纯解析 `deploy/migrations/<svc>/*.sql` + 反射 struct tag，不连库，钉住列双向覆盖、类型相容、
字符列 `utf8mb4_bin`、枚举逐值、列宽 vs 代码常量、CAS/只追加台账、迁移头自述与卫生规则；
跑法 `go test -p 1 -count=1 ./services/<svc>/model/...`。**注意整数列不得登记进 binary 列清单**
（`mid`/`state` 这类唯一键列没有排序规则一说，照抄字符列规则会造成假失败）。
**在真实/共享实例上执行仍需按 `docs/commands.md` §8 显式给出 host/port 与来自 Secret 的账号。**

| 目录 | 库名 | 迁移文件数 | 复验 |
|---|---|---|---|
| `account` | `account` | 6 | applied |
| `asset` | `go_video_asset` | 2 | applied |
| `audit` | `go_video_audit` | 3 | applied |
| `catalog` | `go_video_catalog` | 1 | applied |
| `coin` | `go_video_coin` | 1 | applied |
| `comment` | `go_video_comment` | 1 | applied |
| `content-fingerprint` | `go_video_content_fingerprint` | 1 | applied |
| `creator` | `creator` | 5 | applied |
| `creator-revenue` | `go_video_creator_revenue` | 1 | applied |
| `cron` | `go_video_cron` | 2 | applied |
| `danmaku` | `go_video_danmaku` | 2 | applied |
| `engagement` | `go_video_engagement` | 3 | applied |
| `event-collector` | `go_video_event_collector` | 2 | applied |
| `feature-store` | `go_video_feature_store` | 6 | applied |
| `feed` | `go_video_feed` | 1 | applied |
| `inbox` | `go_video_inbox` | 2 | applied |
| `live-gateway` | `go_video_live_gateway` | 2 | applied |
| `live-ingest` | `go_video_live_ingest` | 3 | applied |
| `live-media` | `go_video_live_media` | 3 | applied |
| `live-room` | `go_video_live_room` | 8 | applied |
| `membership` | `go_video_membership` | 1 | applied |
| `moderation-orchestrator` | `go_video_moderation` | 1 | applied |
| `moderation-worker` | `go_video_moderation_worker` | 1 | applied |
| `notification` | `go_video_notification` | 5 | applied |
| `open-platform` | `go_video_open_platform` | 3 | applied |
| `operation` | `go_video_operation` | 6 | applied |
| `ops-config` | `go_video_ops_config` | 4 | applied |
| `payment` | `go_video_payment` | 1 | applied |
| `playback` | `go_video_playback` | 1 | applied |
| `private-message` | `go_video_private_message` | 2 | applied |
| `recommend-rank` | `go_video_recommend_rank` | 5 | applied |
| `recommend-recall` | `go_video_recommend_recall` | 6 | applied |
| `rights` | `go_video_rights` | 1 | applied |
| `risk-control` | `go_video_risk_control` | 2 | applied |
| `search-indexer` | `go_video_search_indexer` | 2 | applied |
| `search-query` | `go_video_search_query` | 1 | applied |
| `social-graph` | `go_video_social_graph` | 1 | applied |
| `spm` | `go_video_spm` | 3 | applied |
| `trade-order` | `go_video_trade_order` | 1 | applied |
| `transcode` | `go_video_transcode` | 1 | applied |
| `upload` | `go_video_upload` | 3 | `000001`~`000002` applied；`000003` pending |
| `user-profile` | `userprofile` | 10 | applied |
| `video` | `go_video_video` | 4 | `000001`~`000003` applied；`000004` pending |

`account`/`creator`/`user-profile` 的库名沿用历史命名，其余为 `go_video_<service>`（下划线）。
`services/*` 中未出现在上表的服务尚无自有表（纯 RPC 编排、外部存储或尚未落地 model）；
落地 model 时必须同时在本目录补迁移，再改配置和代码。
上表当前有两条非 `applied`，都是 2026-10-05 为「事务内写 Outbox 事件」新建的表：

1. `upload/000003_create_upload_outbox.sql`：`upload` 服务把「完成上传」写成「四写同事务 +
   `media.task.v1` 事件行」后需要这张表落 Outbox，列结构与 `playback`/`live-media`/`recommend-recall`
   的 outbox 表同形（含 `uniq_event_id` 与 `idx_state_next_retry`），唯一差异是 `event_id` 按本节下面
   「字符序约定」写成列级 `utf8mb4_bin` 且不给默认值。
   `go_video_upload` 缺这一张表就是 upload 发布器现在跑不起来的直接原因：未执行前 `internal/publisher`
   只能以 default build（未链接发送端）存在，且 `CompleteUpload` 的四写事务会在 `outbox.Insert` 上报错。
2. `video/000004_create_video_outbox.sql`：`video` 服务把「改变对外可见性的状态转换」写成
   「状态 + 审计 + `content.published.v1` 事件行」同事务后需要这张表。列与四条键（`PRIMARY KEY(id)`、
   `uniq_event_id`、`idx_state_next_retry`、`idx_event_type_ctime`）与上一条 `upload/000003` 完全同名同形，
   同样按「字符序约定」给 `event_id` 加列级 `utf8mb4_bin` 且不给默认值。
   未执行时的失败面比 upload 更宽：`TransitionState` 与 `DeleteSubmission` 复用同一条事务路径
   （`services/video/internal/repository/repository.go:184`），因此 `SCHEDULED→PUBLISHED`、
   `PUBLISHED→OFFLINE/EXPIRED/DELETED`、`OFFLINE→PUBLISHED/DELETED` 这些**发布与下架转换会整笔回滚**
   （状态与审计一起退写，因为事件行与它们同事务），而 `DRAFT→UPLOADING` 一类非可见性转换不写事件、
   不受影响。也就是说「先跑迁移，再上线代码」在 video 上是硬约束，不是建议。
   两条的部署顺序相同：上线先 `up` 迁移、再发代码；回滚先停发布循环与写事件的代码、再 `DROP TABLE`。

数据种子迁移有三条，都在 `operation/` 下：

1. `000004_seed_op_permission.sql` 不建表，只把 `gateway/admin` 权限中间件 `routePermissions` 要求的
   162 个 `(resource, action)` 权限点（29 个 domain）登记进 `op_permission`（缺行 = 后台写入口对所有角色 403）。
   该文件的权限点集合必须与中间件表去重后的集合逐字相等，新增/删除入口时先改 `routePermissions`
   再在此补/删同一组，一致性由 `gateway/admin/internal/middleware/seed_permission_test.go` 双向断言把守。
   阶段 1-2 运营面补鉴权那一轮就地追加了 39 行（13 个新 domain：`account`/`member`/`video`/`catalog`/
   `rights`/`moderation`/`transcode`/`danmaku`/`search`/`risk`/`comment`/`notify`/`inbox`），123 → 162。
2. `000005_seed_op_role_grants.sql` 与 3. `000006_seed_op_role_grants_stage12.sql` 只往
   `op_role`/`op_role_permission` 追加**派生**默认角色：角色名与成员一律由 `op_permission.domain`/`action`
   现算（每轮都是 `domain_*` + `readonly` + `super_admin`），本目录不维护域名↔角色的手工对照表。
   它们**不写** `op_admin_role`/`op_admin_user`——「谁拿哪个角色」是运营决策，不由迁移代劳。
   隔离实例 2026-09-22 复验（两条合起来重放到最终态）：**162 权限点 / 31 角色（29 个 `domain_*` + `readonly`
   + `super_admin`）/ 340 条绑定**，`op_admin_role` 仍 0 行；0 个权限点处于「无任何角色可挂」，
   0 条绑定指向不存在的角色或权限点；`readonly` 覆盖 16 个点（= 库里 `action='read'` 的点全在），
   `super_admin` 覆盖 162 个；两个种子文件各再手工重跑一次，计数不变（幂等成立），中文描述列回读无乱码。
   一致性由 `services/operation/model/role_seed_test.go` 把守（含「绑定必须由 SELECT 现算、
   角色 INSERT 必须早于绑定 INSERT、种子角色 `state=1`、越界写他表即失败、
   权限点种子编号不得跑到角色绑定迁移之后、门禁清单与实际角色种子文件数相等」）。
   为什么 000006 必须存在而不是改 000005：`migrate.ps1` 按文件名记账且跳过已应用版本，
   已跑过 000005 的库不会重新并入新权限点——所以在 000004 就地追加权限点后，
   既有 `domain_*` 角色不会自动长出新点，必须新开一个编号更高的迁移原样重复那三段派生 `SELECT`。
   后续每加一批权限点都按这个模式走：权限点就地追加到 000004，角色并开放在新的高编号迁移，
   下一个可用编号从 `000007` 起。

### 字符序约定

表级默认沿用仓库多数派 `utf8mb4_unicode_ci`；**幂等键、事件 ID、去重键、token/摘要指纹这类参与唯一性判定的列必须列级
`utf8mb4_bin`**。`_ci` 折叠大小写与重音，会让两个只差大小写的 `event_id`/`dedup_key`/`token_hash` 撞上同一唯一键，
表现为静默丢事件或串号，且不报错。新表若偏离此约定，须在文件头说明理由。
**存量漂移（2026-10-05 实测，不改已应用的迁移文件）**：全仓有 24 处 `event_id`/`dedup_key`/`token_hash`/`fingerprint`
列声明未带列级 `utf8mb4_bin`，其中含三张既有 Outbox 表（`playback_outbox`、`live_media_outbox`、
`userprofile.member_outbox`）与消费者去重表（`inbox`/`notification`/`search-indexer` 的 `event_id`）。
可复现探针：
`grep -rniE "^\s*\`(event_id|dedup_key|token_hash|fingerprint)\`" deploy/migrations/*/*.sql | grep -viE "utf8mb4_bin" | wc -l`。
这些列的实际取值是 ULID（大写 Crockford base32，字母表不含大小写歧义对），因此**当前不构成真实串号**，
但约定只对新表生效这一点必须写清楚：`upload/000003` 是本条约定的第一个落实者，
后续要按 `ALTER ... MODIFY COLUMN` 单独开迁移刷齐存量，不能就地改老文件。

