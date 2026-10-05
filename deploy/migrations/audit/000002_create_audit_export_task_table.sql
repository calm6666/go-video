-- =====================================================================
-- audit 服务 - 审计导出任务表
-- =====================================================================
-- 数据库：go_video_audit。数据所有者：audit 服务。
--
-- 为什么导出是「任务」而不是同步查询：
--   一次合规导出可能覆盖数十万条、要按游标翻页很久。同步 RPC 会长时间占住数据库连接
--   与进程内存，还会被 gRPC 超时打断留下半成品文件。因此契约拆成
--   CreateAuditExport（提交任务）→ RunAuditExportTask（由 services/cron 分批推进）
--   → GetAuditExport（换短期签名下载地址）。本服务不内置 worker（AGENTS.md §3）。
--
-- 表与代码对应关系（列名取自 services/audit/model/auditexporttask.go 的 db tag 与 SQL 字符串）：
--   audit_export_task  model/auditexporttask.go ExportTask
--     Insert          —— ON DUPLICATE KEY UPDATE mtime = mtime + RowsAffected == 0
--                          → ErrTaskExists（幂等提交，配合 uniq_request_id）
--     Claim           —— UPDATE ... SET state='running' WHERE task_id=? AND state=?
--                          条件更新代替分布式锁，两个 cron 实例只有一个能拿到
--     AddProgress     —— 累加 row_count、推进 last_seq 游标（心跳）
--     Finish          —— WHERE state='running'，重复结束不会覆盖已定终态
--     TransitionState —— 通用乐观迁移（如 succeeded → expired）
--
-- 唯一键与幂等设计：
--   1. uniq_request_id(request_id)：同一申请幂等落库。没有它，前端重复点击就会产出
--      两个任务、两份对象、两次导出审计，成本与合规口径都会乱。
--   2. 不建 (chain_key, seq) 之类索引：本表不参与哈希链。
--
-- 索引取自真实查询路径：
--   * idx_state_ctime(state, ctime)：ListAuditExports 按状态过滤 + 「还有哪些 pending」的运维巡检。
--   * idx_operator_ctime(operator_id, ctime)：「我申请的导出列表」（控制台默认视图）。
--   * idx_state_expire(state, expire_at)：收敛到期对象（succeeded 且 expire_at < now → expired），
--     这是唯一能高效定位「该删对象了」的路径。
--   排序统一 ORDER BY task_id DESC（自增主键即时间倒序），因此不另建 ctime 单列索引。
--
-- 敏感信息约束（AGENTS.md §7）：
--   * filter_json 是查询条件快照，写入前经 model.LooksLikePII 扫描；
--     导出条件本身不能成为新的泄露面（例如按手机号筛审计）。
--   * bucket / object_key 只存引用。密钥只从环境变量指向的 Secret 读取，
--     绝不入库、不入配置字面量（见 etc/audit.v1.yaml 的 Storage.AccessKeyRef）。
--   * err_msg 是脱敏文本：只允许业务原因，禁止堆栈、SQL 片段、连接串。
--   * 导出的文件内容本身落在对象存储里，字段仍按主表的摘要口径输出，不含明文 PII。
--
-- owner：平台治理（audit 服务）；影响范围：新增 1 张表。
-- 回滚：
--   DROP TABLE IF EXISTS `audit_export_task`;
--   回滚只丢失「任务台账」，不影响 audit_entry 的证据链；已上传的对象需按 bucket 生命周期
--   规则另行清理（本表是它们的唯一台账，DROP 前先把未过期对象清单导出）。
-- 锁风险：新建空表，不锁既有表。运行期写热点在「同一 task_id 的条件更新」上，
--   行数与操作者数同阶（后台导出是低频动作），无需额外分片。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `audit_export_task` (
  `task_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（列表按此倒序）',
  `request_id`      VARCHAR(64)  NOT NULL COMMENT '提交幂等键（CallContext.request_id），重复提交返回原任务',
  `operator_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '申请管理员 ID（引用 operation，不复制其资料）',
  `caller_service`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '申请来源服务名',
  `filter_json`     VARCHAR(2048) NOT NULL DEFAULT '' COMMENT '查询条件快照（JSON，写入前过 PII 扫描；时间范围必填所以不可能导出全表）',
  `format`          VARCHAR(16)  NOT NULL DEFAULT 'csv' COMMENT '导出格式：csv / json',
  `state`           VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '状态：pending / running / succeeded / failed / expired / canceled（合法性见 model.CanExportTransition，终态无出边）',
  `row_count`       BIGINT       NOT NULL DEFAULT 0 COMMENT '已导出行数（AddProgress 累加，终态即总数）',
  `object_size`     BIGINT       NOT NULL DEFAULT 0 COMMENT '导出文件字节数',
  `expire_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '对象计划删除时间（Unix 秒）：配置 Export.ObjectTTLSeconds',
  `file_hash`       CHAR(64)     NOT NULL DEFAULT '' COMMENT '导出文件 sha256hex：下载方据此自证文件未被替换',
  `err_msg`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '失败原因（脱敏文本，不含堆栈/SQL/连接串）',
  `trace_id`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '提交时的链路 ID',
  `last_seq`        BIGINT       NOT NULL DEFAULT 0 COMMENT '断点游标（已导出的最大 entry_id）：推进者崩溃后从此续传，避免同一任务重复写文件',
  `bucket`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '对象存储桶名（只存引用，凭据在 Secret/Vault）',
  `object_key`      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '对象键（只存引用，签名地址即时生成不落库）',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '提交时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '最后更新时间（Unix 秒）',
  `started_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '首次进入 running 的时间（IF(started_at=0, now, started_at) 保证只写一次）',
  `finished_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '进入 succeeded/failed 的时间',
  PRIMARY KEY (`task_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_operator_ctime` (`operator_id`, `ctime`),
  KEY `idx_state_expire` (`state`, `expire_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='审计导出任务表（异步任务台账：条件更新代替分布式锁，只存对象引用不存凭据）';
