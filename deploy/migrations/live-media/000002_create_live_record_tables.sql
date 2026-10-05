-- =====================================================================
-- live-media 服务 - 录制任务 / 录制切片（录制面，断点续录与缺口显式化）
-- =====================================================================
-- 目标库：`go_video_live_media`。
-- 用途：承载「某场次的一次录制」及其时间轴上的切片清单。对应 rpc.StartLiveRecord / StopLiveRecord /
--       ReportLiveRecordProgress / GetLiveRecordTask / ListLiveRecordTasks / ReportRecordSegment /
--       ListRecordSegments，并为 SubmitReplayTask 提供拼接素材的事实依据。
-- 数据所有者：live-media 服务（AGENTS.md §5）。record 任务通过 source_task_id 引用
--       live_transcode_task（0 表示原画源），通过 room_id / live_session_id 引用 live-room、live-ingest；
--       全部只存主键，不建跨库外键，不复制对方主数据。
-- 状态机：live_record_task.state 与 rpc.LiveRecordState、model.RecordState* 一致
--       （1 PENDING、2 RECORDING、3 STOPPING、4 STOPPED、5 FAILED、6 CANCELLED）。
--       合法迁移见 model/errors.go 的 recordTransitions：PENDING→RECORDING/FAILED/CANCELLED、
--       RECORDING→RECORDING(心跳)/STOPPING/FAILED、STOPPING→STOPPED/FAILED/CANCELLED、
--       FAILED→PENDING|RECORDING（断点续录，从 last_seq+1 继续）；STOPPED/CANCELLED 终态。
--       live_record_segment.state 与 rpc.SegmentState、model.SegmentState* 一致
--       （1 UPLOADING、2 UPLOADED、3 VERIFIED、4 MISSING、5 CORRUPT），
--       迁移表 IsValidSegmentTransition：正向 UPLOADING→UPLOADED→VERIFIED，任何未定态可判
--       MISSING/CORRUPT，MISSING 可补录回 UPLOADING/UPLOADED，VERIFIED/CORRUPT 为稳定态。
-- 缺口约定：切片序号必须能表达"空洞"——断点续录探测到的缺失用 state=4(MISSING) 显式插行
--       （model.InsertIgnoreMissing），绝不静默跳过；MISSING/CORRUPT 行不写 bucket/object_key
--       （产物本就不存在，写空引用会让回收误判为可删对象）。
-- 幂等依赖：live_record_task.uniq_request_id（登记重放）；
--       live_record_segment.uniq_record_seq(record_id, seq)（逐片登记重放：INSERT IGNORE + 状态只前进，
--       registered_at 首次登记后不回改）。
-- 派生列说明：live_record_task.segment_count / gap_count / recorded_duration_ms 由
--       model.RefreshStats 从 live_record_segment 条件聚合重算，last_seq 同时受 Worker 上报水位约束；
--       这四列是可重算投影、**非唯一事实源**，事实源是切片行本身。
-- 大文件与凭据：切片文件只在对象存储，本表只存 bucket + object_key 相对引用与 size_bytes/checksum
--       校验证据；不存签名 URL、AccessKey、STS 凭据（AGENTS.md §6）。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行。
--       live_record_segment 是只增不减的大表（3 小时直播按 10s 分片约 1080 行/小时/档位），
--       写入峰值 = 在线录制任务数 / 分片秒数；InsertIgnore 走 uniq_record_seq 冲突路径，
--       持锁时间随批量大小增长，InsertIgnoreMissing 补洞必须限制单批行数（调用侧夹取）。
--       model.PurgeByRecord 使用 `DELETE ... WHERE record_id=? AND state<>? LIMIT n`，
--       000001 的 MarkExpiredOffline 使用 `UPDATE ... LIMIT n`：两者都要求 binlog_format=ROW
--       （语句格式下主从会因 LIMIT 不确定性而漂移），且必须分批执行，禁止一次锁住整场切片。
--       RefreshStats 是带相关子查询的单行 UPDATE，只锁 live_record_task 一行，但会扫切片表：
--       必须靠 uniq_record_seq 前缀收敛，未 STOPPED 的任务不要周期性调用。
-- 回滚：DROP TABLE IF EXISTS `live_record_segment`;
--       DROP TABLE IF EXISTS `live_record_task`;
--       （回放任务通过 record_id 引用录制任务，回滚需与 000003 一并评估。）
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_record_task` (
  `record_id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '录制任务 ID（自增主键）',
  `room_id`              BIGINT       NOT NULL COMMENT '直播间引用（live-room 主键）',
  `live_session_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '直播场次引用（录制按场次建模，0 表示登记时未提供，回放将无法归属场次）',
  `source_task_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '录制来源转码任务 ID，0 表示录原画源（引用，不建外键）',
  `state`                TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 PENDING、2 RECORDING、3 STOPPING、4 STOPPED、5 FAILED、6 CANCELLED（终态）',
  `start_at`             BIGINT       NOT NULL DEFAULT 0 COMMENT '期望录制起点（Unix 秒），0 表示立即',
  `end_at`               BIGINT       NOT NULL DEFAULT 0 COMMENT '期望录制终点（Unix 秒），0 表示随场次结束',
  `record_start_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '实际开始录制时间（Unix 秒），首次进入 RECORDING 时落定，不回改',
  `record_end_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '实际结束时间（Unix 秒），只有 Worker 上报 STOPPED 才写入，0 表示未结束',
  `segment_seconds`      INT          NOT NULL DEFAULT 10 COMMENT '分片时长（秒），登记时按配置夹取（默认 10、上限 60），续录不回改',
  `last_seq`             BIGINT       NOT NULL DEFAULT 0 COMMENT '已登记的最大切片序号：断点续录锚点，服务端用 GREATEST 保证只增不减',
  `segment_count`        BIGINT       NOT NULL DEFAULT 0 COMMENT '已登记切片数（含缺口）。派生投影：可由切片表重算（RefreshStats），非唯一事实源',
  `gap_count`            BIGINT       NOT NULL DEFAULT 0 COMMENT '缺口（MISSING/CORRUPT）切片数。派生投影：可重算，非唯一事实源',
  `recorded_duration_ms` BIGINT       NOT NULL DEFAULT 0 COMMENT '有效录制时长（毫秒，VERIFIED 切片求和）。派生投影：可重算，非唯一事实源',
  `output_bucket`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '切片存储桶名（引用；凭据进 Secret/Vault）',
  `output_prefix`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '切片对象 key 前缀（相对路径，禁止含签名参数）',
  `heartbeat_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次 Worker 心跳（Unix 秒）',
  `timeout_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '心跳超时判定时刻（Unix 秒）= 心跳 + 超时秒数快照，0 表示不参与超时清扫',
  `version`              BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观并发版本号，每次推进 +1；上报方回传 expected_version',
  `reason`               TINYINT      NOT NULL DEFAULT 0 COMMENT '失败/停止原因（rpc.FailureReason 编号），0 表示未指定',
  `errno`                INT          NOT NULL DEFAULT 0 COMMENT '最近一次错误码，0 表示无错误',
  `err_msg`              VARCHAR(512) NOT NULL DEFAULT '' COMMENT '脱敏错误摘要（不含对象存储凭据与完整地址）',
  `request_id`           VARCHAR(64)  NOT NULL COMMENT '幂等键：同 request_id 重放返回同一录制任务（唯一索引）',
  `trace_id`             VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次调用的链路 ID',
  `ctime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`record_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_room_state_ctime` (`room_id`, `state`, `ctime`),
  KEY `idx_session_state` (`live_session_id`, `state`),
  KEY `idx_state_timeout` (`state`, `timeout_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='录制任务（last_seq 是断点续录锚点；计数列是可重算投影）';

CREATE TABLE IF NOT EXISTS `live_record_segment` (
  `id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（同一 record 内不保证连续，顺序按 seq）',
  `record_id`       BIGINT       NOT NULL COMMENT '所属录制任务（live_record_task 主键，本服务自有表引用，不建外键）',
  `room_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（冗余，按房间排障与回收统计）',
  `live_session_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '场次引用（冗余，按场次对账）',
  `seq`             BIGINT       NOT NULL COMMENT '切片序号，从 1 开始且在该 record_id 内唯一；序号不连续即代表时间轴存在缺口',
  `start_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '切片起点（Unix 秒）',
  `end_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '切片终点（Unix 秒）',
  `duration_ms`     BIGINT       NOT NULL DEFAULT 0 COMMENT '切片时长（毫秒），VERIFIED 行参与 recorded_duration_ms 重算',
  `state`           TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 UPLOADING、2 UPLOADED、3 VERIFIED（可参与拼接）、4 MISSING（空洞）、5 CORRUPT（永不参与拼接）',
  `bucket`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '对象存储桶名（引用）；MISSING/CORRUPT 行为空串，表示产物不存在',
  `object_key`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '切片对象 key（相对路径，不含签名与凭据）',
  `size_bytes`      BIGINT       NOT NULL DEFAULT 0 COMMENT '字节数（校验证据之一）',
  `checksum`        CHAR(64)     NOT NULL DEFAULT '' COMMENT '内容摘要（sha256 hex，校验证据，不是凭据）',
  `worker_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '登记该切片的 Worker 标识（仅审计与排障）',
  `trace_id`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次登记/推进的链路 ID',
  `registered_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '首次登记时间（Unix 秒），重放不覆盖；超期回收按此列判定',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次更新时间（Unix 秒）；本表无 ctime 列，registered_at 即首登时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_record_seq` (`record_id`, `seq`),
  KEY `idx_record_state_seq` (`record_id`, `state`, `seq`),
  KEY `idx_room_record` (`room_id`, `record_id`),
  KEY `idx_registered_at` (`registered_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='录制切片清单（回放拼接最小单位，可表达缺口；只存对象引用）';
