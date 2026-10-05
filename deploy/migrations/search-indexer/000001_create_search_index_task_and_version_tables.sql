-- =====================================================================
-- search-indexer 服务 - 索引重建任务表 / 索引版本登记表
-- =====================================================================
-- 数据所有者：search-indexer 服务（AGENTS.md §5）。其它服务禁止直接读写本目录的表；
--   重建任务的唯一入口是 SearchIndexer RPC（SubmitRebuildTask / GetRebuildTask /
--   ListRebuildTasks / SwitchAlias / GetIndexHealth），由 services/operation 或
--   services/cron 通过 gRPC 调用，不共享库表。
--   search-query 只读 OpenSearch 查询别名，不读本目录任何表。
--
-- 库名：go_video_search_indexer（取自 services/search-indexer/etc/searchindexer.v1.yaml
--   的 DataSource；建库由 scripts/migrate.ps1 按 DSN 执行，本文件不写 CREATE DATABASE）。
--
-- 表与写入路径（列名/条件严格对应 services/search-indexer/model/*.go 的 db tag 与 SQL）：
--   1. search_index_task     —— 零停机重建任务（SearchIndexTask，taskColumns）。
--        · 幂等：uniq_request_id + INSERT IGNORE，同一 request_id 重复提交返回同一任务
--          （model.Insert 靠 RowsAffected=0 判定 Duplicated，不做覆盖写）。
--        · 抢占：ClaimNext 用 WHERE state='pending' ORDER BY id ASC LIMIT 1 取候选，
--          再用 WHERE task_id=? AND state='pending' 做 CAS 更新，多实例只有一个成功。
--        · 断点：cursor_value 保存下一片 content_id 区间下界（十进制字符串），
--          进程退出时终态写 failed，重提任务从断点续跑，不跳过任何区间。
--   2. search_index_version  —— 「哪个物理索引承接写入/查询别名」的唯一登记处
--        （SearchIndexVersion，versionColumns）。不依赖 OpenSearch 别名做隐式推断。
--        · uniq_index_name + INSERT IGNORE 保证并发首次写入只收敛到一个目标索引。
--        · SwitchActive 在事务内 SELECT ... FOR UPDATE 读当前 active 行做
--          expected_current 乐观校验，再原子改 state。
--
-- 索引取舍（一律以代码里的 WHERE / ORDER BY 为准）：
--   - search_index_task.idx_state_id 同时服务 ClaimNext（state 等值 + id 升序）和
--     List（可选 state 等值 + id<? ORDER BY id DESC）；不建 ctime 索引，列表只按 id 分页。
--   - search_index_version.idx_alias_state 服务 FindActive（alias,state 等值 + id 倒序）、
--     ListByAlias（alias 等值 + id 倒序）与 ListAll（ORDER BY alias ASC, id DESC）。
--   - 「一个别名同一时刻只有一行 active」不建 uniq(alias) 唯一键：本表按别名保留
--     retiring/history 全量版本（ListByAlias 是回滚依据），唯一性由 SwitchActive 的
--     行锁 CAS 保证，这与 model 注释一致。
--
-- 影响：仅新增本服务的 2 张表，不改动任何既有表、不改数据、不写 schema_migrations
--   （版本记录由 scripts/migrate.ps1 维护）；不建跨服务外键（只存 mid/task_id 等业务主键）。
-- 回滚：
--   DROP TABLE IF EXISTS `search_index_version`;
--   DROP TABLE IF EXISTS `search_index_task`;
--   回滚前必须先停 search-indexer 进程（重建执行器与消费 worker 在同一个服务进程内），
--   否则任务行会持续写入已删除的表；OpenSearch 物理索引与别名不受本表删除影响，
--   但登记表丢失后 SwitchAlias 无法再做乐观校验，需人工按 GetIndexHealth 结果重建登记。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

-- 索引重建任务（零停机重建的执行与断点记录）
CREATE TABLE IF NOT EXISTS `search_index_task` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（列表分页与抢占顺序按此）',
  `task_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '任务 ID（idgen.Prefixed("sit")，对外暴露）',
  `scope`        VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '重建范围：full/partition/content_type',
  `scope_value`  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '范围取值：partition 为 "min-max"，content_type 为 1/2/3（ValidateScope 归一化后落库）',
  `state`        VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '任务状态：pending/running/succeeded/failed/canceled（终态不可回退，UpdateProgress/Finish 带 state 条件）',
  `cursor_value` VARCHAR(32)  NOT NULL DEFAULT '1' COMMENT '续跑游标：下一片 content_id 半开区间下界（十进制字符串，初值 1）',
  `total`        BIGINT       NOT NULL DEFAULT 0 COMMENT '预计处理总量（首个切片起累计，0 表示未知）',
  `processed`    BIGINT       NOT NULL DEFAULT 0 COMMENT '已处理文档数（_reindex 累计 total）',
  `failed`       BIGINT       NOT NULL DEFAULT 0 COMMENT '失败/版本冲突文档数（不阻断任务，供健康核对）',
  `target_index` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '目标物理索引（<alias>_<schema>_<unix>，重建写入后切别名）',
  `alias`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '查询别名（Options.Alias 归一化，空则默认别名）',
  `operator`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '提交人（管理员账号或 cron job 名，审计用）',
  `request_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '提交幂等键（调用方生成，重复提交返回同一任务）',
  `last_error`   VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '最近一次失败原因（sanitizeError 脱敏截断 500 字符，不含堆栈与凭据）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，每次游标/终态推进刷新）',
  `started_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '开始执行时间（Unix 秒，ClaimNext 抢占成功时写入）',
  `finished_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '结束时间（Unix 秒，Finish 写终态时写入）',
  PRIMARY KEY (`id`),
  -- 对外暴露的 task_id 定位（FindOne）与 CAS 终态更新（WHERE task_id=? AND state=?）
  UNIQUE KEY `uniq_task_id` (`task_id`),
  -- 提交幂等：INSERT IGNORE 命中此键即返回既有任务，绝不产生第二个目标索引
  UNIQUE KEY `uniq_request_id` (`request_id`),
  -- ClaimNext：WHERE state='pending' ORDER BY id ASC；List：WHERE [state=?] AND id<? ORDER BY id DESC
  KEY `idx_state_id` (`state`, `id`),
  -- 排障与运维盘点：按别名查看未收口任务
  KEY `idx_alias_state` (`alias`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-indexer 索引重建任务（执行状态机与断点游标，request_id 幂等）';

-- 物理索引版本登记（写入/查询别名的唯一事实登记处）
CREATE TABLE IF NOT EXISTS `search_index_version` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（同别名多版本按 id 倒序取最新）',
  `alias`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '查询别名（search-query 读取；OpenSearch 侧别名同名）',
  `index_name`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '物理索引名（全局唯一，重建每轮新名）',
  `schema_version` VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '文档结构版本（OpenSearchConf.SchemaVersion，如 v1；变更 mapping 时递增并走新索引+切别名）',
  `doc_count`      BIGINT       NOT NULL DEFAULT 0 COMMENT '文档数快照（Health 巡检与 SwitchAlias 时刷新，-1 不入库）',
  `state`          VARCHAR(16)  NOT NULL DEFAULT 'retiring' COMMENT '状态：active（承接写入与查询）/retiring（已切下，观察期）/history（可清理）',
  `created_by`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '创建来源：bootstrap（首写自动建）/重建 task_id/switch/运维',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，状态与 doc_count 刷新时更新）',
  PRIMARY KEY (`id`),
  -- 并发首写收敛：EnsureActiveIndex 靠此唯一键 + INSERT IGNORE 判定「谁登记的索引生效」
  UNIQUE KEY `uniq_index_name` (`index_name`),
  -- FindActive：WHERE alias=? AND state='active' ORDER BY id DESC LIMIT 1（含 FOR UPDATE 变体）
  -- ListByAlias：WHERE alias=? ORDER BY id DESC；ListAll：ORDER BY alias ASC, id DESC
  KEY `idx_alias_state` (`alias`, `state`, `id`),
  -- 观察期结束后按状态挑出可转 history / 可清理的索引
  KEY `idx_state_mtime` (`state`, `mtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='search-indexer 物理索引版本登记（零停机别名切换与回滚依据）';
