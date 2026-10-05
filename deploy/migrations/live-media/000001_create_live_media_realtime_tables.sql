-- =====================================================================
-- live-media 服务 - 直播转码任务 / 分发输出（实时链路面）
-- =====================================================================
-- 目标库：`go_video_live_media`（由 scripts/migrate.ps1 按服务目录建库并写入 schema_migrations）。
-- 用途：承载「一次直播场次里某个码率档位的一条转码生命周期」与「该档位可对外分发的产物登记」。
--       对应 rpc.StartLiveTranscode / StopLiveTranscode / RetryLiveTranscode / CancelLiveTranscode /
--       ReportLiveTranscodeProgress / GetLiveTranscodeTask / ListLiveTranscodeTasks /
--       UpsertStreamOutput / OfflineStreamOutput / ListStreamOutputs。
-- 数据所有者：live-media 服务（AGENTS.md §5）。本文件两张表是本服务自有事实；
--       room_id / live_session_id 只是 live-room、live-ingest 的主键引用，template_id 只是 transcode
--       模板主键引用，均**不建跨库外键、不复制对方主数据**（断流/开播状态由 live.state.v1 事件驱动，
--       本服务不回写 live-room 任何表）。
-- 状态机：live_transcode_task.state 取值与 rpc.LiveTranscodeState、model.TranscodeState* 严格一致
--       （1 PENDING、2 RUNNING、3 STOPPING、4 STOPPED、5 FAILED、6 CANCELLED），编号不可重排。
--       合法迁移见 model/errors.go 的 transcodeTransitions：
--       PENDING→RUNNING/FAILED/CANCELLED、RUNNING→RUNNING(心跳)/STOPPING/FAILED、
--       STOPPING→STOPPED/FAILED/CANCELLED、FAILED→PENDING（仅 RetryLiveTranscode）；
--       STOPPED/CANCELLED 是终态。实现一律走「条件 UPDATE + RowsAffected」，禁止读-改-写两步更新。
-- 幂等依赖：live_transcode_task.uniq_request_id（同一 request_id 重放返回同一任务，不重复拉起）；
--       live_stream_output.uniq_output_natural（同一天然键重复登记只刷新一行，不产生第二路档位）。
-- 派生列说明：live_stream_output 无 version 列，state 本身即 CAS 条件；
--       live_transcode_task.progress 是 Worker 采样值（RUNNING 时表健康度），可被后续上报覆盖，
--       不作为任何判定依据，属可重算投影、非唯一事实源。
-- 大文件与凭据：source_ref 只存短期拉流标识或流名，禁止写入带签名的完整地址、AccessKey 或密钥；
--       bucket/object_key/cdn_domain 只是对象存储与 CDN 的相对引用，签名与访问凭据一律进 Secret/Vault，
--       短期播放地址由 live-gateway/playback 侧现算（AGENTS.md §6）。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行。
--       UpdateState 是按主键 + state IN (...) + version = ? 的单行更新，命中 uniq_request_id/主键，
--       行锁范围最小；ReportLiveTranscodeProgress 是直播期间最高频写入（每任务每心跳一行一次），
--       事务必须只含「状态推进 + Outbox」，禁止在事务内调用 CDN/FFmpeg/厂商接口。
--       ListTimedOut 走 idx_state_timeout 前缀扫描并带 LIMIT，不做全表扫描。
-- 回滚：DROP TABLE IF EXISTS `live_stream_output`;
--       DROP TABLE IF EXISTS `live_transcode_task`;
--       （任务表被 live_record_task.source_task_id 与 live_stream_output.task_id 引用，
--        回滚需与 000002/000003 一并评估，否则历史录制会留下指向已删任务的悬挂主键引用。）
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_transcode_task` (
  `task_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '任务 ID（自增主键）',
  `room_id`         BIGINT       NOT NULL COMMENT '直播间引用（live-room 主键，仅引用不建外键）',
  `live_session_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '直播场次引用（live-room/live-ingest 主键），0 表示登记时未提供场次',
  `template_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '转码模板引用（transcode 主键）；只存 ID，档位参数以本行快照为准',
  `bitrate_level`   TINYINT      NOT NULL DEFAULT 0 COMMENT '码率档位枚举：1 原画、2 1080p、3 720p、4 480p、5 360p、6 纯音频',
  `protocol`        TINYINT      NOT NULL DEFAULT 0 COMMENT '输出协议枚举：1 HLS、2 HTTP-FLV、3 RTMP、4 ARTC',
  `source_ref`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '拉流源引用（流标识或短期地址，禁止存签名参数与凭据）',
  `anchor_mid`      BIGINT       NOT NULL DEFAULT 0 COMMENT '主播 mid（仅审计归因，不做任何商业化判断）',
  `state`           TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 PENDING、2 RUNNING、3 STOPPING、4 STOPPED、5 FAILED、6 CANCELLED（终态）',
  `progress`        TINYINT      NOT NULL DEFAULT 0 COMMENT '进度/健康度采样 0-100（RUNNING 时为源流健康度），可被覆盖的投影值，非唯一事实源',
  `attempt`         INT          NOT NULL DEFAULT 0 COMMENT '已执行次数（RetryLiveTranscode 时 +1，受 max_attempts 约束）',
  `max_attempts`    INT          NOT NULL DEFAULT 3 COMMENT '重试上限快照（登记时取自配置 LiveMedia.DefaultMaxAttempts，之后不回改历史行）',
  `started_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '首次实际启动时间（Unix 秒），0 表示尚未拉起',
  `stopped_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '实际停止时间（Unix 秒），0 表示未停止',
  `heartbeat_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次 Worker 心跳时间（Unix 秒），超时判定输入',
  `timeout_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '心跳超时判定时刻（Unix 秒）= 心跳 + 超时秒数快照，0 表示不参与超时清扫',
  `version`         BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观并发版本号，每次状态推进 +1；上报方必须回传 expected_version',
  `reason`          TINYINT      NOT NULL DEFAULT 0 COMMENT '失败/停止原因：1 超时、2 源流丢失、3 Worker 异常、4 存储、5 CDN、6 上游拒绝、7 人工、8 数据缺口；0 表示未指定',
  `errno`           INT          NOT NULL DEFAULT 0 COMMENT '最近一次错误码，0 表示无错误',
  `err_msg`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '脱敏错误摘要（禁止含完整拉流地址、签名参数与 SQL 片段）',
  `request_id`      VARCHAR(64)  NOT NULL COMMENT '幂等键：同 request_id 重放返回同一任务（唯一索引即重放防线）',
  `trace_id`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次调用的链路 ID',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`task_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_room_session_level` (`room_id`, `live_session_id`, `bitrate_level`, `protocol`, `state`),
  KEY `idx_room_state_ctime` (`room_id`, `state`, `ctime`),
  KEY `idx_session_state` (`live_session_id`, `state`),
  KEY `idx_template_state` (`template_id`, `state`),
  KEY `idx_state_timeout` (`state`, `timeout_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='直播转码任务与状态机（真实 FFmpeg 由 Worker 拉起，本表只登记与回报）';

CREATE TABLE IF NOT EXISTS `live_stream_output` (
  `output_id`        BIGINT       NOT NULL AUTO_INCREMENT COMMENT '分发输出 ID（自增主键）',
  `room_id`          BIGINT       NOT NULL COMMENT '直播间引用（live-room 主键，仅引用）',
  `live_session_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '直播场次引用，0 表示登记时未提供',
  `task_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '产生该输出的转码任务 ID，0 表示源流直出不经转码（引用，不建外键）',
  `bitrate_level`    TINYINT      NOT NULL DEFAULT 0 COMMENT '码率档位枚举：1 原画、2 1080p、3 720p、4 480p、5 360p、6 纯音频',
  `protocol`         TINYINT      NOT NULL DEFAULT 0 COMMENT '分发协议枚举：1 HLS、2 HTTP-FLV、3 RTMP、4 ARTC',
  `bucket`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '对象存储桶名（引用；凭据进 Secret/Vault，不入库）',
  `object_key`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT 'playlist/流路径的相对 key（禁止写签名 URL 与查询参数）',
  `cdn_domain`       VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'CDN 域名（配置项，非密钥）',
  `width`            INT          NOT NULL DEFAULT 0 COMMENT '下发时刻的宽度快照（不是 transcode 模板镜像，模板变更不回写历史行）',
  `height`           INT          NOT NULL DEFAULT 0 COMMENT '下发时刻的高度快照',
  `bitrate_kbps`     INT          NOT NULL DEFAULT 0 COMMENT '下发时刻的码率快照（kbps）',
  `fps`              INT          NOT NULL DEFAULT 0 COMMENT '下发时刻的帧率快照',
  `state`            TINYINT      NOT NULL DEFAULT 1 COMMENT '在线态：1 在线、2 已下线（终态，可被回收任务清理）；本表无 version 列，state 即 CAS 条件',
  `offline_reason`   TINYINT      NOT NULL DEFAULT 0 COMMENT '下线原因（rpc.FailureReason 编号）：1 到期、2 源流丢失、7 人工等；0 表示仍在线',
  `online_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '首次上线时间（Unix 秒），重新上线不回改（审计事实）',
  `offline_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '下线时间（Unix 秒），0 表示仍在线',
  `online_expire_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '在线有效期（Unix 秒），0 表示由断流事件下线；过期清扫写 offline_reason=1',
  `request_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次登记的幂等键；不建唯一索引——同一天然键允许反复上下线',
  `trace_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次登记/下线的链路 ID',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '首次登记时间（Unix 秒），UPSERT 不回改',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`output_id`),
  UNIQUE KEY `uniq_output_natural` (`room_id`, `live_session_id`, `bitrate_level`, `protocol`),
  KEY `idx_room_state` (`room_id`, `state`),
  KEY `idx_session_state` (`live_session_id`, `state`),
  KEY `idx_state_expire` (`state`, `online_expire_at`),
  KEY `idx_task` (`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='直播分发输出/码率梯（实时链路，断流即下线，与回放发布状态无关）';
