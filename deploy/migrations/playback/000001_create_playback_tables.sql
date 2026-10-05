-- =====================================================================
-- playback 服务 - 播放会话、播放进度与领域事件 Outbox
-- =====================================================================
-- 库名：go_video_playback（见 services/playback/etc/playback.v1.yaml 的 DataSource）
-- 数据所有者：playback 服务（AGENTS.md §5「播放授权」）。
--   本库只保存播放会话、签名授权事实与播放进度；稿件、媒资、转码版本和
--   版权窗口仍分别归 video/asset/transcode/rights，禁止在本库复制其主数据。
-- 影响范围：仅新增三张表，不改动任何既有表，可独立应用。
-- 回滚：
--   DROP TABLE IF EXISTS `playback_outbox`;
--   DROP TABLE IF EXISTS `playback_progress`;
--   DROP TABLE IF EXISTS `playback_session`;
-- 锁风险：全部为新建空表，索引在空表上建立，无在线锁风险。
--   playback_session/playback_progress 是写多读多的大表，容量治理（按 ctime 归档、
--   只保留 30~90 天）留给后续分区或清理任务，本期不做（见服务 README「已知缺口」）。
-- 说明：不使用跨服务外键（AGENTS.md §5 禁止跨库引用），内容一致性靠 RPC 与事件保证。
-- =====================================================================

-- 播放会话表：一次播放授权 = 一行。
-- 写入方：Playback.GetPlaybackToken。读取方：Playback.VerifyPlaybackToken（回源校验）、
-- Playback.GetSession（管理/排障）。
-- 幂等：uniq_request_id 保证同一 request_id 重放只产生一个会话；request_id 由 logic
--   强制必填，因此不会出现多行空串相互冲突的情况。
CREATE TABLE IF NOT EXISTS `playback_session` (
  `session_id`   CHAR(26)      NOT NULL COMMENT '播放会话 ID（ULID，主键，客户端凭此上报心跳）',
  `content_type` TINYINT       NOT NULL DEFAULT 0 COMMENT '内容类型：1 UGC 稿件、2 PGC 集（与 rights 契约编号不同）',
  `content_id`   BIGINT        NOT NULL DEFAULT 0 COMMENT '内容 ID：UGC=aid、PGC=episode_id',
  `vid`          VARCHAR(32)   NOT NULL DEFAULT '' COMMENT 'UGC bvid（排障用，可为空串）',
  `mid`          BIGINT        NOT NULL DEFAULT 0 COMMENT '观看者用户 ID，0 表示游客',
  `platform`     TINYINT       NOT NULL DEFAULT 0 COMMENT '客户端平台：1 android、2 ios、3 harmony、4 desktop',
  `app_version`  VARCHAR(32)   NOT NULL DEFAULT '' COMMENT '客户端版本号',
  `region`       VARCHAR(16)   NOT NULL DEFAULT '' COMMENT '地区代码（PGC 版权窗口校验依据）',
  `object_key`   VARCHAR(512)  NOT NULL DEFAULT '' COMMENT '媒资对象 key（由调用方从 asset/transcode 解析后传入）',
  `uri`          VARCHAR(512)  NOT NULL DEFAULT '' COMMENT '签名绑定的 URI 路径（auth_key 与其绑定，防止挪用）',
  `request_id`   VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '签发请求幂等键（唯一索引）',
  `expire_at`    BIGINT        NOT NULL DEFAULT 0 COMMENT '授权过期时间（Unix 秒，同时是 CDN auth_key 的 ts）',
  `state`        TINYINT       NOT NULL DEFAULT 1 COMMENT '会话状态：1 有效、2 已过期、3 已撤销',
  `trace_id`     VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '签发时的 trace_id（跨服务排障）',
  `ctime`        BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`session_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_content` (`content_type`, `content_id`, `ctime`),
  -- 过期/撤销扫描与「有效会话还有多少」的运维查询走该索引
  KEY `idx_state_expire` (`state`, `expire_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='播放会话表：记录谁在什么时间窗内获得哪个对象的播放授权（不含任何密钥或签名串）';

-- 播放进度表：一个会话一行，心跳按 session_id 幂等 upsert。
-- position_ms 只前进不回退（GREATEST），客户端乱序/重试不会破坏断点。
-- 读取方：Playback.GetSession；后续断点续播投影可由 gateway/app 直接查询本服务 RPC。
CREATE TABLE IF NOT EXISTS `playback_progress` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `session_id`   CHAR(26)        NOT NULL COMMENT '播放会话 ID（唯一索引，心跳幂等键）',
  `content_type` TINYINT         NOT NULL DEFAULT 0 COMMENT '内容类型：1 UGC、2 PGC（冗余自会话，便于按内容聚合）',
  `content_id`   BIGINT          NOT NULL DEFAULT 0 COMMENT '内容 ID',
  `vid`          VARCHAR(32)     NOT NULL DEFAULT '' COMMENT 'UGC bvid',
  `mid`          BIGINT          NOT NULL DEFAULT 0 COMMENT '观看者用户 ID，0 表示游客',
  `position_ms`  BIGINT          NOT NULL DEFAULT 0 COMMENT '服务端记录的最大播放位置（毫秒，断点）',
  `duration_ms`  BIGINT          NOT NULL DEFAULT 0 COMMENT '内容总时长（毫秒，客户端上报）',
  `buffer_count` INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '累计卡顿次数',
  `avg_bitrate`  BIGINT          NOT NULL DEFAULT 0 COMMENT '累计平均码率（bps）',
  `last_error`   INT             NOT NULL DEFAULT 0 COMMENT '最近一次播放错误码，0 表示无错误',
  `ctime`        BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT          NOT NULL DEFAULT 0 COMMENT '最后更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_session_id` (`session_id`),
  -- 断点核对：某用户对某内容的最近进度
  KEY `idx_mid_content_mtime` (`mid`, `content_type`, `content_id`, `mtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='播放进度与播放质量表（playback.heartbeat.v1 事件的落库事实，按 session_id 幂等）';

-- 领域事件 Outbox 表：与 playback_progress 在同一事务写入（AGENTS.md §5）。
-- 发布器按 id 升序轮询 state=0 且 next_retry_at 到期的记录投递 MQ，
-- Topic = event_type + '.v' + schema_version（当前只有 playback.heartbeat.v1），
-- 消费者按 event_id 幂等去重，失败指数退避，超过上限置 state=2 转人工处理。
CREATE TABLE IF NOT EXISTS `playback_outbox` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按此升序保证顺序）',
  `event_id`       CHAR(26)        NOT NULL DEFAULT '' COMMENT '事件唯一 ID（ULID，消费者据此幂等）',
  `event_type`     VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '事件类型：playback.heartbeat',
  `schema_version` INT             NOT NULL DEFAULT 1 COMMENT '事件 schema 版本',
  `aggregate_type` VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '聚合根类型：playback_session',
  `aggregate_id`   VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '聚合根 ID（session_id）',
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
  COMMENT='playback 领域事件 Outbox 表：业务事务内写入，发布器异步投递（幂等、退避重试、死信人工处理）';
