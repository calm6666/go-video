-- =====================================================================
-- upload 服务 - 领域事件 Outbox 表（media.task.v1）
-- =====================================================================
-- 目标库：`go_video_upload`（由 scripts/migrate.ps1 按服务目录建库并写入 schema_migrations）。
-- 用途：承载「某次分片上传已完成、OSS 对象已就位」这一事实，供 asset/transcode/content-fingerprint
--       侧后续接管（转码、截图、字幕、指纹任务的派发依据）。
--       对应 CompleteUpload 的写入路径：会话推进 COMPLETED 与事件行必须同事务提交，
--       由 internal/publisher 的发布循环按 id 升序投递到 `media.task.v1`。
-- 数据所有者：upload 服务（AGENTS.md §5）。本表是本服务自有事实；
--       asset_id 只是 asset 侧主键的占位引用（`asset-placeholder:<upload_id>`，真实 ID 由 asset 回填
--       upload_session.asset_id，不由本表持有），mid 是 account 主键引用，
--       均不建跨库外键、不复制对方主数据。
-- 列语义：event_type/schema_version 两列共同决定 topic（`eventenvelope.Topic` 现场拼出，
--       不在配置里重复写 topic 字面量）；aggregate_type 恒 `upload_session`、aggregate_id 恒 upload_id，
--       后者同时是分区键，因此同一会话的事件必然落在同一分区；payload 是 common/eventenvelope.Envelope
--       的完整 JSON，列与 payload 的同源性由 publisher 的 CheckRow 反查（不一致即判死，不重试）。
-- 状态机：state 取值与 model.OutboxState* 严格一致（0 待发布、1 已发布、2 失败），编号不可重排。
--       只有 0 会被 ListPending 取出；2 之后的解冻没有人工放行接口（见服务 README「已知缺口」）。
-- 幂等依赖：uniq_event_id 保证同一 event_id 不会被写两行。该列用列级 `utf8mb4_bin` 且**不给默认值**
--       （deploy/migrations/README.md「字符序约定」）：表级默认的 `_ci` 排序规则会折叠大小写，
--       让两个只差大小写的 event_id 撞上同一唯一键，表现为静默丢事件且不报错；
--       而 `DEFAULT ''` 会让第二行「忘了填 event_id」的写入直接撞唯一键，不如让它按 NOT NULL 无默认报错。
--       注意本约束**不是**「同一会话只发一个事件」：会话完成门槛由 upload_session.state 承担
--       （COMPLETED 后 CompleteUpload 直接返回 ErrUploadCompleted），因此重放完成请求不会产生第二个事件行。
--       消费方仍必须按 event_id 去重：本表没有租约列，多副本发布会把同一行投两次。
-- 大文件与凭据：payload 只存对象引用（bucket/object_key）与文件摘要（md5），
--       禁止写入预签名 URL、AccessKey/SecretKey 等任何长期凭据（AGENTS.md §6）。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行；新建空表，索引无在线重建成本。
--       Insert 在 CompleteUpload 的事务内（与三条 upload_session 更新同批），
--       事务必须只含「状态推进 + Outbox」，禁止在事务内调用 OSS/MinIO 接口。
--       ListPending 走 idx_state_next_retry 前缀扫描并带 LIMIT，不做全表扫描。
-- 容量：每次成功完成上传产生一行，且本服务没有清理任务，已发布行会无限增长。
--       归档/清理策略见服务 README「已知缺口」，本文件不做 DELETE 或分区（迁移只承担 DDL）。
-- 回滚：DROP TABLE IF EXISTS `upload_outbox`;
--       （回滚前必须确认发布循环已停：表消失后 publisher 的 ListPending 会持续报错，
--        且已投递但未被消费的事件无法重放，本表是唯一的事前记录点。）
-- =====================================================================
CREATE TABLE IF NOT EXISTS `upload_outbox` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按此升序保证同会话顺序）',
  `event_id`       CHAR(26) COLLATE utf8mb4_bin NOT NULL COMMENT '事件唯一 ID（ULID，消费者据此幂等）：唯一键列，逐字节比较，不给默认值',
  `event_type`     VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型：media.task',
  `schema_version` INT             NOT NULL DEFAULT 1 COMMENT '事件 schema 版本',
  `aggregate_type` VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '聚合根类型：upload_session',
  `aggregate_id`   VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '聚合根 ID（upload_id，同时是分区键）',
  `payload`        MEDIUMTEXT      NOT NULL COMMENT '事件信封完整 JSON（common/eventenvelope.Envelope）',
  `state`          TINYINT         NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布、1 已发布、2 失败（超最大重试）',
  `retry_count`    INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '已重试次数（用于指数退避）',
  `next_retry_at`  BIGINT          NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，0 表示可立即投递）',
  `last_error`     VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '最近一次投递错误',
  `occurred_at`    BIGINT          NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒，与信封 occurred_at 对应）',
  `ctime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒，发布器更新状态时刷新）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_state_next_retry` (`state`, `next_retry_at`),
  KEY `idx_event_type_ctime` (`event_type`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='upload 领域事件 Outbox 表：CompleteUpload 事务内写入，发布器异步投递（幂等、退避重试、失败人工处理）';
