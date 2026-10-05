-- =====================================================================
-- live-ingest 服务 - 状态事件 / 事件 Outbox / CDN 回调留证（事件与集成面）
-- =====================================================================
-- 用途：live.state.v1 的事件事实来源 + 事务性 Outbox + 厂商回调留证。
--       对应 ListStreamEvents / GetEventPublishCheckpoint / RetryFailedEvents / VerifyCdnCallback。
-- 数据所有者：live-ingest 服务（AGENTS.md §5）。live-room 只消费事件、不回写本库；
--       跨服务只传 room_id / stream_id / node_id 业务主键，不建外键。
-- 事件契约：live_stream_event 与 live_ingest_outbox 必须在同一事务内随状态迁移写入
--       （AGENTS.md §5「状态变更与事件同事务」）。事件 append-only，不可 UPDATE：
--       补偿只能追加新 seq 的事件。发布由独立发布器按 outbox.id 升序投递，本服务不等 MQ。
-- 幂等与反乱序（三重唯一索引，缺一不可）：
--       uniq_event_id      —— event_id（ULID）全局唯一，消费方按它去重；
--       uniq_stream_seq    —— 同一 stream_id 的 seq 只能有一个事件，重复占号必然冲突；
--       uniq_report_id     —— 同一次上报只产生一个事件（report_id 为 NULL 的内部事件
--                              不受约束，MySQL 唯一索引允许多个 NULL）。
-- 隐私与密钥：live_cdn_callback 只存签名、来源 IP、原始参数的 SHA-256 摘要，
--       绝不存厂商签名原文、密钥或明文 IP（AGENTS.md §7）；uniq_nonce 是防重放的最终防线。
--       回调只能「建议」状态（suggest_state），推进仍须走 ReportStreamState 的合法迁移。
-- 时间列：全部 BIGINT Unix 秒；三张表均为高频写入，归档窗口见服务 README。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行。
--       live_stream_event / live_ingest_outbox 只在业务事务里 INSERT（追加，冲突即回滚重试）；
--       发布器 UPDATE 按主键单行（state + mtime），RetryFailedEvents 带 LIMIT 避免长事务；
--       Prune/归档走 occurred_at 索引 + LIMIT 分批，禁止无界 DELETE。
-- 回滚：DROP TABLE IF EXISTS `live_cdn_callback`;
--       DROP TABLE IF EXISTS `live_ingest_outbox`;
--       DROP TABLE IF EXISTS `live_stream_event`;
--       ⚠ 先确认发布器已停：删掉 outbox 会丢失未投递事件，live-room 侧只能靠
--         ListStreamEvents 的 seq 游标补偿，历史事件一旦删除不可恢复。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_stream_event` (
  `id`                  BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（落库顺序，发布器按 outbox.id 投递）',
  `event_id`            VARCHAR(64)  NOT NULL COMMENT '事件唯一 ID（ULID，消费方幂等去重锚点）',
  `stream_id`           VARCHAR(64)  NOT NULL COMMENT '所属流（live_stream 主键）',
  `room_id`             BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（消费方按此路由到 live-room）',
  `session_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '场次引用，0 表示未绑定场次',
  `seq`                 BIGINT       NOT NULL COMMENT '该流单调递增序号（消费方据此拒绝乱序回退）',
  `from_state`          TINYINT      NOT NULL DEFAULT 0 COMMENT '迁移前状态，0 仅用于占位（正常事件必为 1~4）',
  `to_state`            TINYINT      NOT NULL COMMENT '迁移后状态：1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED',
  `node_id`             VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件发生时的接入节点',
  `interruption_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '关联断流区间，0 表示与断流无关',
  `interrupted_seconds` BIGINT       NOT NULL DEFAULT 0 COMMENT '本次中断秒数（关闭断流区间的事件携带）',
  `stop_reason`         TINYINT      NOT NULL DEFAULT 0 COMMENT '停流原因（仅 STOPPED 事件非 0），取值同 live_stream.stop_reason',
  `report_id`           VARCHAR(64)  NULL DEFAULT NULL COMMENT '上报幂等键；NULL 表示服务端内部迁移（扫描器/健康触发）',
  `source`              VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '事件来源：entry/health/cdn/admin/sweeper，排障定位推动方',
  `reason`              VARCHAR(255) NOT NULL DEFAULT '' COMMENT '原因摘要（禁止写入明文密钥或厂商签名）',
  `occurred_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒），消费方以此判定新旧',
  `trace_id`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '落库时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  UNIQUE KEY `uniq_stream_seq` (`stream_id`, `seq`),
  UNIQUE KEY `uniq_report_id` (`report_id`),
  KEY `idx_room_ctime` (`room_id`, `ctime`),
  KEY `idx_occurred` (`occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='流状态迁移事件（live.state.v1 的事实来源，append-only）';

CREATE TABLE IF NOT EXISTS `live_ingest_outbox` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按此升序投递，保证同流顺序）',
  `event_id`       VARCHAR(64)  NOT NULL COMMENT '事件 ID，与 live_stream_event 同源（唯一索引）',
  `event_type`     VARCHAR(64)  NOT NULL DEFAULT 'live.state' COMMENT '事件类型（本服务恒为 live.state）',
  `schema_version` INT          NOT NULL DEFAULT 1 COMMENT 'schema 版本，topic 由 event_type + 版本拼成 live.state.v1',
  `aggregate_type` VARCHAR(32)  NOT NULL DEFAULT 'live_stream' COMMENT '聚合根类型',
  `aggregate_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '聚合根 ID（= stream_id）',
  `stream_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '流 ID（冗余，按流对账事件位点）',
  `room_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（消费方路由）',
  `seq`            BIGINT       NOT NULL DEFAULT 0 COMMENT '该流事件序号（与 live_stream_event.seq 一致）',
  `payload`        TEXT         NOT NULL COMMENT 'common/eventenvelope.Envelope 完整 JSON（消费方按信封字段解析）',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '发布状态：1 PENDING（含退避）、2 PUBLISHED、3 FAILED（等运营放行）',
  `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数（指数退避依据，超上限转 FAILED）',
  `next_retry_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒），0 表示可立即投递',
  `last_error`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误（脱敏，不含密钥与厂商凭证）',
  `occurred_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒），滞后度与归档按此列',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，与状态迁移同事务）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；state=2 时即发布时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_state_retry` (`state`, `next_retry_at`, `id`),
  KEY `idx_state_occurred` (`state`, `occurred_at`),
  KEY `idx_stream_id` (`stream_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='live.state.v1 事务性 Outbox（同事务写入、异步投递、可重试）';

CREATE TABLE IF NOT EXISTS `live_cdn_callback` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `nonce`             VARCHAR(64)  NOT NULL COMMENT '回调随机串（唯一索引 = 防重放最终防线，重复写入必然冲突）',
  `domain`            VARCHAR(191) NOT NULL DEFAULT '' COMMENT '推流域名（须在 Cdn.PublishDomains 白名单内）',
  `event_type`        VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '厂商回调事件名（publish/publish_done 等，原样留证）',
  `stream_name`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '回调解析出的流标识（不含密钥段）',
  `stream_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '归属流 ID，空串表示尚未解析出流',
  `key_id`            BIGINT       NOT NULL DEFAULT 0 COMMENT '关联密钥 ID（引用，不含密钥材料），0 表示未解析',
  `room_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（归属房间，0 表示未解析）',
  `signature_hash`    CHAR(64)     NOT NULL DEFAULT '' COMMENT '回调签名的 SHA-256 hex（不存签名原文）',
  `client_ip_hash`    CHAR(64)     NOT NULL DEFAULT '' COMMENT '来源 IP 的 SHA-256 hex（不存明文 IP）',
  `raw_params_digest` CHAR(64)     NOT NULL DEFAULT '' COMMENT '原始参数的 SHA-256 摘要（排障比对用，不存参数明文）',
  `verify_result`     TINYINT      NOT NULL DEFAULT 0 COMMENT '判定：0 未判定、1 通过、2 签名不符、3 时间戳越窗、4 重放、5 域名未绑定、6 流不存在',
  `suggest_state`     TINYINT      NOT NULL DEFAULT 0 COMMENT '建议迁移到的流状态（0 表示无建议）；推进仍须走 ReportStreamState',
  `handled`           TINYINT      NOT NULL DEFAULT 0 COMMENT '0 未处理、1 入口已据此推进、2 已忽略（判定不通过或被重放）',
  `reason`            VARCHAR(255) NOT NULL DEFAULT '' COMMENT '判定说明（不含密钥与厂商凭证）',
  `occurred_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '回调自带时间戳（Unix 秒），与 server_time 偏差超窗判越界',
  `verified_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '服务端判定时间（Unix 秒）',
  `trace_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '落库时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_nonce` (`nonce`),
  KEY `idx_stream_occurred` (`stream_id`, `occurred_at`),
  KEY `idx_name_ctime` (`stream_name`, `ctime`),
  KEY `idx_occurred` (`occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='CDN/入口回调留证与鉴权结果（只存摘要，防重放；回调不直接改状态）';
