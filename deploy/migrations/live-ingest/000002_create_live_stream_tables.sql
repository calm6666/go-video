-- =====================================================================
-- live-ingest 服务 - 推流会话 / 断流区间 / 健康采样（流状态面）
-- =====================================================================
-- 用途：承载「一次推流会话」的状态机与观测数据。对应 ReportStreamState / GetStreamState /
--       ListStreams / CloseStream / ReportStreamHealth / GetStreamHealth / ListStreamInterruptions。
-- 数据所有者：live-ingest 服务（AGENTS.md §5）。live_stream.state 是接入侧事实，
--       live-room 的开播/直播中/已结束等业务态由 live.state.v1 事件投影得到；
--       本库只有 room_id / session_id 引用，不写 live-room 任何表，也不建跨服务外键。
-- 状态机：state 取值与 rpc.StreamState、live-room ReportStreamStateReq.stream_state 严格一致
--       （1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED）。迁移只允许
--       IDLE→PUBLISHING/STOPPED、PUBLISHING→INTERRUPTED/STOPPED、INTERRUPTED→PUBLISHING/STOPPED，
--       STOPPED 为终态；实现走条件 UPDATE + RowsAffected（禁止先读后写）。
-- 幂等依赖：live_stream.uniq_publish_request（同一鉴权请求只建一条流）、
--       live_stream_interruption.uniq_start_event（一个事件只开一个断流区间）、
--       live_stream_health_report.uniq_report_id（采样上报重放直接回放）。
-- seq 语义：live_stream.seq 是该流 live.state.v1 事件的单调序号，只在状态迁移成功时 +1，
--       与 live_stream_event.(stream_id, seq) 唯一索引共同构成反乱序依据。
-- 时间列：全部 BIGINT Unix 秒；publish_started_at=0 表示从未真正推流。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行。
--       ApplyTransition / LockByID(FOR UPDATE) 会锁 live_stream 单行，事务必须短：
--       只包含「状态迁移 + 事件 + Outbox」，不得在事务内调用外部 CDN/厂商接口。
--       live_stream_health_report 写入量最大（每流每采样窗口一行），归档见服务 README。
-- 回滚：DROP TABLE IF EXISTS `live_stream_health_report`;
--       DROP TABLE IF EXISTS `live_stream_interruption`;
--       DROP TABLE IF EXISTS `live_stream`;
--       （事件与 Outbox 表在 000003，回滚需一并处理，否则 outbox 会残留指向已删流的位点。）
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_stream` (
  `stream_id`                 VARCHAR(64)  NOT NULL COMMENT '推流会话 ID（ULID，主键）；下播重推开新 ID，断流重连复用同一 ID',
  `key_id`                    BIGINT       NOT NULL DEFAULT 0 COMMENT '使用的密钥 ID（引用，绝不冗余任何密钥材料）',
  `stream_name`               VARCHAR(128) NOT NULL DEFAULT '' COMMENT '流标识冗余（排障与回调反查）',
  `room_id`                   BIGINT       NOT NULL COMMENT '房间引用（live-room 主键，仅引用）',
  `session_id`                BIGINT       NOT NULL DEFAULT 0 COMMENT '场次引用，0 表示未绑定场次',
  `anchor_mid`                BIGINT       NOT NULL DEFAULT 0 COMMENT '主播 ID（列表权限收敛）',
  `protocol`                  TINYINT      NOT NULL DEFAULT 0 COMMENT '接入协议枚举：1 RTMP、2 SRT、3 WebRTC',
  `node_id`                   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '当前接入节点，空串表示未分配',
  `state`                     TINYINT      NOT NULL DEFAULT 1 COMMENT '流状态：1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED（编号不可重排）',
  `seq`                       BIGINT       NOT NULL DEFAULT 0 COMMENT '当前事件序号（单调递增，建档为 0，每次合法迁移 +1）',
  `publish_request_id`        VARCHAR(64)  NOT NULL COMMENT '建档来源的鉴权幂等键（必填，唯一索引即重放防线）',
  `publish_started_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '首次进入 PUBLISHING 的时间（Unix 秒），0 表示从未真正推流',
  `state_changed_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次状态变更时间（Unix 秒）',
  `last_heartbeat_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近心跳/上报时间（Unix 秒），断流扫描依据',
  `interrupted_total_seconds` BIGINT       NOT NULL DEFAULT 0 COMMENT '本次推流累计中断秒数（断流区间关闭时累加）',
  `interruption_count`        INT          NOT NULL DEFAULT 0 COMMENT '本次推流累计断流次数',
  `stop_reason`               TINYINT      NOT NULL DEFAULT 0 COMMENT '停流原因：1 主播下播、2 心跳超时、3 健康危险、4 密钥吊销、5 运营停流、6 密钥过期；0 表示未停流',
  `stop_detail`               VARCHAR(255) NOT NULL DEFAULT '' COMMENT '停流原因摘要（禁止写入明文密钥或厂商签名）',
  `health_state`              TINYINT      NOT NULL DEFAULT 4 COMMENT '健康判定：1 HEALTHY、2 DEGRADED、3 CRITICAL、4 NO_DATA（建档默认 4）',
  `health_reported_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最近健康上报时间（Unix 秒），超窗判 NO_DATA',
  `video_bitrate_bps`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近视频码率（bps）',
  `audio_bitrate_bps`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近音频码率（bps）',
  `fps_x100`                  INT          NOT NULL DEFAULT 0 COMMENT '最近帧率 ×100（避免小数列）',
  `packet_loss_ppm`           INT          NOT NULL DEFAULT 0 COMMENT '最近丢包率（百万分比）',
  `trace_id`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '建档时的链路 ID',
  `ctime`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '建档时间（Unix 秒）',
  `mtime`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`stream_id`),
  UNIQUE KEY `uniq_publish_request` (`publish_request_id`),
  KEY `idx_room_state_ctime` (`room_id`, `state`, `ctime`),
  KEY `idx_name_state_ctime` (`stream_name`, `state`, `ctime`),
  KEY `idx_key_state` (`key_id`, `state`),
  KEY `idx_state_heartbeat` (`state`, `last_heartbeat_at`),
  KEY `idx_anchor_state` (`anchor_mid`, `state`),
  KEY `idx_node_state` (`node_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='推流会话与状态机（seq 是 live.state.v1 的防乱序位点）';

CREATE TABLE IF NOT EXISTS `live_stream_interruption` (
  `interruption_id`    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `stream_id`          VARCHAR(64)  NOT NULL COMMENT '所属流（live_stream 主键）',
  `room_id`            BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（冗余，按房间统计断流）',
  `episode_no`         INT          NOT NULL DEFAULT 1 COMMENT '该流第几次断流，从 1 递增（同事务内取 MAX+1）',
  `node_id`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '断流时的接入节点（重连可能换节点）',
  `started_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '断流开始时间（Unix 秒）',
  `ended_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '断流结束时间（Unix 秒），0 表示仍在中断中',
  `duration_seconds`   BIGINT       NOT NULL DEFAULT 0 COMMENT '本次中断时长（秒），关闭时按 ended_at-started_at 落定',
  `end_reason`         TINYINT      NOT NULL DEFAULT 0 COMMENT '结束原因：1 重连成功、2 宽限期耗尽、3 主动停流；0 表示未结束',
  `reconnect_attempts` INT          NOT NULL DEFAULT 0 COMMENT '期间重连尝试次数（鉴权重试但未续流的次数）',
  `start_event_id`     VARCHAR(64)  NOT NULL COMMENT '开启本区间的 live.state.v1 event_id（唯一索引，重放不重复开）',
  `end_event_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '关闭本区间的 event_id，空串表示未关闭',
  `reason`             VARCHAR(255) NOT NULL DEFAULT '' COMMENT '原因摘要（不含密钥材料）',
  `ctime`              BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`              BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`interruption_id`),
  UNIQUE KEY `uniq_start_event` (`start_event_id`),
  UNIQUE KEY `uniq_stream_episode` (`stream_id`, `episode_no`),
  KEY `idx_stream_open` (`stream_id`, `ended_at`, `started_at`),
  KEY `idx_room_started` (`room_id`, `started_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='断流与重连区间（ended_at=0 为开放区间，扫描器兜底关闭）';

CREATE TABLE IF NOT EXISTS `live_stream_health_report` (
  `id`                    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `report_id`             VARCHAR(64)  NULL DEFAULT NULL COMMENT '上报幂等键；NULL 表示内部采样（唯一索引允许多个 NULL，故不冲突）',
  `stream_id`             VARCHAR(64)  NOT NULL COMMENT '所属流（live_stream 主键）',
  `node_id`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '采样来源接入节点',
  `video_bitrate_bps`     BIGINT       NOT NULL DEFAULT 0 COMMENT '视频码率（bps）',
  `audio_bitrate_bps`     BIGINT       NOT NULL DEFAULT 0 COMMENT '音频码率（bps）',
  `fps_x100`              INT          NOT NULL DEFAULT 0 COMMENT '帧率 ×100',
  `packet_loss_ppm`       INT          NOT NULL DEFAULT 0 COMMENT '丢包率（百万分比）',
  `rtt_ms`                BIGINT       NOT NULL DEFAULT 0 COMMENT '往返时延（毫秒）',
  `sample_window_seconds` INT          NOT NULL DEFAULT 0 COMMENT '该采样点覆盖的窗口秒数',
  `health_state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '该采样点判定：1 HEALTHY、2 DEGRADED、3 CRITICAL、4 NO_DATA',
  `occurred_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '采样时间（Unix 秒），窗口聚合与归档按此列',
  `trace_id`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '落库时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_report_id` (`report_id`),
  KEY `idx_stream_occurred` (`stream_id`, `occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='流健康采样点（原始事实，live_stream 只留最新一帧；按 occurred_at 归档）';
