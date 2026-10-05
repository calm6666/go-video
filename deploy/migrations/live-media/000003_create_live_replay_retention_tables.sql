-- =====================================================================
-- live-media 服务 - 回放拼接任务 / 回放资产引用 / 回收任务 / 领域事件 Outbox
-- =====================================================================
-- 目标库：`go_video_live_media`。
-- 用途：承载「回放拼接任务的登记与进度」「回放产物与 asset/稿件的引用关系（含 video 侧投影）」
--       「回收任务的意图与计数证据」与「本服务领域事件的 Outbox」。
--       对应 rpc.SubmitReplayTask / ReportReplayProgress / GetReplayTask / ListReplayTasks /
--       BindReplayAsset / ApplyReplayContentState / ListReplayAssetRefs /
--       SubmitRetentionTask / ReportRetentionResult / GetRetentionTask / ListRetentionTasks。
-- 数据所有者：live-media 服务（AGENTS.md §5）。
--       asset_id 是 asset 服务媒资主键引用、aid/bvid 是 video 服务稿件主键引用、
--       room_id/live_session_id 是 live-room 引用、record_id/replay_id 是本服务自有主键；
--       全部只存引用，**不建跨库外键、不写 asset_meta / video_submission 的任何列**。
-- 关键边界（AGENTS.md §5/§8，硬约束）：
--       回放必须走普通视频的审核与发布链路（asset.RegisterAsset → video.CreateSubmission →
--       moderation-orchestrator.SubmitForReview），本文件任何表都没有"发布"写入入口：
--       live_replay_asset_ref.review_state / published_at / review_state_at 是 video 事实状态在
--       本库的**只读投影**，只由 ApplyReplayContentState 或 content.published.v1 消费者写入，
--       方向单一 video → live-media；live_replay_task.state=6(COMPLETED) 只能由该投影驱动。
-- 状态机：live_replay_task.state 与 rpc.ReplayState、model.ReplayState* 一致
--       （1 PENDING、2 MERGING、3 UPLOADING、4 REGISTERED、5 REVIEW_SUBMITTED、6 COMPLETED、
--       7 FAILED、8 CANCELLED）；合法迁移见 replayTransitions，其中 REVIEW_SUBMITTED→COMPLETED
--       只由投影通道触发，Worker 上报不得直达 COMPLETED。
--       live_retention_task.state 与 rpc.RetentionState 一致（1 PENDING、2 RUNNING、
--       3 SUCCEEDED、4 FAILED、5 CANCELLED，后三者终态），retentionTransitions 只允许
--       PENDING→RUNNING/CANCELLED/FAILED、RUNNING→SUCCEEDED/FAILED/CANCELLED。
-- 幂等依赖：live_replay_task.uniq_request_id（提交重放）；
--       live_replay_asset_ref.uniq_replay_id（一场回放一条引用，绑定重放走 ON DUPLICATE 且
--       不覆盖投影列）、uniq_asset_id / uniq_aid（一个媒资或稿件不能被两条回放串用）；
--       live_retention_task.uniq_request_id（删除意图不重复排队）；
--       live_media_outbox.uniq_event_id（消费者按 event_id 去重的落库前提）。
-- 投影/派生列说明：live_replay_task.segment_count / gap_count / duration_ms 是提交时按
--       live_record_segment 条件聚合落的快照，可由切片表重算，**非唯一事实源**；
--       live_replay_asset_ref.review_state / published_at / review_state_at / last_event_id / source
--       是外部事实投影，可由 content.published.v1 重放修复，非唯一事实源；
--       live_retention_task.scanned / deleted / skipped 是 Worker 覆盖写的执行证据（purge=0 时
--       deleted 恒为 0），不得二次累加。
-- 大文件与凭据：output_bucket/output_key、bucket/object_key 只存对象存储相对引用，
--       回放文件与切片文件不进 MySQL；签名与访问凭据进 Secret/Vault（AGENTS.md §6）；
--       last_error/err_msg 均截断入库且必须脱敏，title/description 只是透传给稿件的文本快照，
--       本服务不做内容审核判定。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行。
--       live_replay_asset_ref 的三条唯一键使 BindReplayAsset 在并发下必然有一方拿到
--       ErrAssetRefConflict，属预期行为，不要在业务里重试到成功。
--       live_record_segment 之外的回收删除路径（PurgeByRecord / PurgePublished）使用
--       `DELETE ... LIMIT n`，要求 binlog_format=ROW 并分批执行，禁止一次锁住大表；
--       live_media_outbox 只 INSERT + 按主键条件 UPDATE，发布器轮询走 idx_state_retry，
--       积压时先把 published_at 老数据 PurgePublished 掉再排查发布器，不要手工改 state。
--       回收任务执行事务里对 live_replay_asset_ref 的 MarkRetentionState 是单行条件更新，
--       与切片删除分批放在同一 Worker 循环里时，每批一个事务，避免长事务持锁。
-- 回滚：DROP TABLE IF EXISTS `live_media_outbox`;
--       DROP TABLE IF EXISTS `live_retention_task`;
--       DROP TABLE IF EXISTS `live_replay_asset_ref`;
--       DROP TABLE IF EXISTS `live_replay_task`;
--       ⚠ 删 live_media_outbox 会丢失未投递事件，下游（live-gateway 档位投影、运营告警）只能靠
--       对账任务重建；删 live_replay_asset_ref 会切断「回放 ↔ 稿件」证据链，回放审核追溯将不可回答，
--       生产环境禁用 DROP 回滚，改为反向迁移新增列/表或备份后恢复。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_replay_task` (
  `replay_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '回放任务 ID（自增主键）',
  `room_id`        BIGINT       NOT NULL COMMENT '直播间引用（live-room 主键）',
  `live_session_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '场次引用（回放归属场次）',
  `record_id`      BIGINT       NOT NULL COMMENT '素材来源录制任务（live_record_task 主键，本服务自有引用）',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 PENDING、2 MERGING、3 UPLOADING、4 REGISTERED、5 REVIEW_SUBMITTED、6 COMPLETED（仅投影驱动）、7 FAILED、8 CANCELLED',
  `from_seq`       BIGINT       NOT NULL DEFAULT 1 COMMENT '拼接区间起始切片序号（登记时归一化为 1）',
  `to_seq`         BIGINT       NOT NULL DEFAULT 0 COMMENT '拼接区间结束切片序号（登记时归一化为录制 last_seq）',
  `segment_count`  BIGINT       NOT NULL DEFAULT 0 COMMENT '参与拼接的有效（VERIFIED）切片数。派生快照：可由切片表重算，非唯一事实源',
  `gap_count`      BIGINT       NOT NULL DEFAULT 0 COMMENT '区间内缺口切片数。派生快照：可重算，非唯一事实源；allow_gaps=0 时登记必为 0',
  `start_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '回放覆盖区间起点（Unix 秒）',
  `end_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '回放覆盖区间终点（Unix 秒）',
  `duration_ms`    BIGINT       NOT NULL DEFAULT 0 COMMENT '拼接后时长（毫秒），Worker 上报后覆盖写',
  `allow_gaps`     TINYINT      NOT NULL DEFAULT 0 COMMENT '是否允许切片缺口：0 不允许（默认）、1 允许（拼接必须跳过缺口并留证）',
  `output_bucket`  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '回放产物桶名（引用；凭据进 Secret/Vault）',
  `output_key`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '回放产物对象 key（相对路径，不含签名）',
  `asset_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '媒资引用（asset 服务主键），0 表示尚未登记媒资',
  `aid`            BIGINT       NOT NULL DEFAULT 0 COMMENT '稿件引用（video 服务主键），0 表示尚未建稿；本服务不推进稿件状态',
  `bvid`           VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '稿件 bvid 冗余展示字段，事实源仍是 video',
  `anchor_mid`     BIGINT       NOT NULL DEFAULT 0 COMMENT '回放稿件归属主播 mid（video.CreateSubmission 的投稿人引用）',
  `title`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '回放标题快照（透传给稿件，本服务不做审核判断）',
  `description`    VARCHAR(2048) NOT NULL DEFAULT '' COMMENT '回放简介快照（透传）',
  `version`        BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观并发版本号，每次推进 +1；上报方回传 expected_version',
  `reason`         TINYINT      NOT NULL DEFAULT 0 COMMENT '失败原因（rpc.FailureReason 编号），6 表示上游（asset/video/moderation）拒绝；0 表示未指定',
  `errno`          INT          NOT NULL DEFAULT 0 COMMENT '最近一次错误码，0 表示无错误',
  `err_msg`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '脱敏错误摘要（不含凭据与完整地址）',
  `request_id`     VARCHAR(64)  NOT NULL COMMENT '幂等键：同 request_id 重放返回同一回放任务（唯一索引）',
  `trace_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次调用的链路 ID',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`replay_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_record_range` (`record_id`, `from_seq`, `to_seq`, `state`),
  KEY `idx_room_state_ctime` (`room_id`, `state`, `ctime`),
  KEY `idx_session_state` (`live_session_id`, `state`),
  KEY `idx_anchor_state` (`anchor_mid`, `state`),
  KEY `idx_asset` (`asset_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='回放拼接任务（登记与进度；无发布写入入口，COMPLETED 只由投影驱动）';

CREATE TABLE IF NOT EXISTS `live_replay_asset_ref` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `room_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（live-room 主键，仅引用）',
  `live_session_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '场次引用',
  `replay_id`         BIGINT       NOT NULL COMMENT '回放任务 ID（live_replay_task 主键，天然键：一场回放一条引用）',
  `record_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '素材来源录制任务引用',
  `asset_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '媒资引用（asset 主键，事实源在 asset 服务）',
  `aid`               BIGINT       NOT NULL DEFAULT 0 COMMENT '稿件引用（video 主键，事实源在 video 服务）',
  `bvid`              VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '稿件 bvid 冗余展示值',
  `anchor_mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '主播引用（按主播查回放列表）',
  `bucket`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '产物桶名（引用）',
  `object_key`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '产物对象 key（相对路径，不含签名与凭据）',
  `duration_ms`       BIGINT       NOT NULL DEFAULT 0 COMMENT '产物时长（毫秒）',
  `segment_from_seq`  BIGINT       NOT NULL DEFAULT 0 COMMENT '引用行的切片区间起点（拼接证据，来自动作时刻的回放任务快照）',
  `segment_to_seq`    BIGINT       NOT NULL DEFAULT 0 COMMENT '切片区间终点',
  `gap_count`         BIGINT       NOT NULL DEFAULT 0 COMMENT '区间缺口数快照。派生值，可重算，非唯一事实源',
  `review_state`      TINYINT      NOT NULL DEFAULT 0 COMMENT '只读投影：0 未同步、1 审核中、2 驳回、3 video 侧已发布、4 video 侧已下架、5 video 侧已删除；只由投影通道写入',
  `review_state_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '投影同步时刻（Unix 秒），用于拒绝旧事件覆盖新投影；0 表示未同步',
  `retention_state`   TINYINT      NOT NULL DEFAULT 0 COMMENT '引用行生命周期：0 正常、1 待回收、2 已回收（产物已删，引用行留证不可回退为正常）',
  `published_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT 'video 侧发布时间投影（Unix 秒），GREATEST 单调，0 表示未发布',
  `last_event_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次驱动本行投影的事件 ID（按 event_id 幂等，重放不再刷新）',
  `source`            VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '投影来源：content.published.v1 / video.rpc / manual',
  `request_id`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '绑定动作的幂等键（同值重放返回成功，不同值撞唯一键）',
  `trace_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次绑定/投影同步的链路 ID',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '首次绑定时间（Unix 秒），ON DUPLICATE 不回改',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_replay_id` (`replay_id`),
  UNIQUE KEY `uniq_asset_id` (`asset_id`),
  UNIQUE KEY `uniq_aid` (`aid`),
  KEY `idx_room_session` (`room_id`, `live_session_id`),
  KEY `idx_anchor_id` (`anchor_mid`, `id`),
  KEY `idx_review_state` (`review_state`, `id`),
  KEY `idx_retention_state` (`retention_state`, `id`),
  KEY `idx_record` (`record_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='回放产物与 asset/稿件的引用关系（含 video 侧只读投影，构成回放走普通发布链路的证据链）';

CREATE TABLE IF NOT EXISTS `live_retention_task` (
  `retention_id`  BIGINT       NOT NULL AUTO_INCREMENT COMMENT '回收任务 ID（自增主键；Worker 按此升序领取，先登记先执行）',
  `target_kind`   TINYINT      NOT NULL DEFAULT 0 COMMENT '回收对象：1 录制切片、2 回放产物、3 直播分发残留档位',
  `room_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用，0 表示全局扫描（全局扫描必须给 expire_before 收敛范围）',
  `target_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '指定回收对象主键（按 target_kind 分别是 segment 自增 id / replay_asset_ref id / output_id），0 表示按 expire_before 批量',
  `expire_before` BIGINT       NOT NULL DEFAULT 0 COMMENT '只回收该时刻（Unix 秒）之前到期的对象，0 表示不按时间收敛',
  `purge`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0 只登记并置待回收标记（deleted 恒为 0）、1 允许 Worker 真删对象存储引用',
  `batch_limit`   INT          NOT NULL DEFAULT 100 COMMENT '单次处理上限快照（配置夹取到 1..500），Worker 分批删除据此限流',
  `state`         TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 PENDING、2 RUNNING、3 SUCCEEDED、4 FAILED、5 CANCELLED（后三者终态）',
  `scanned`       INT          NOT NULL DEFAULT 0 COMMENT '扫描命中行数（Worker 覆盖写，不累加）',
  `deleted`       INT          NOT NULL DEFAULT 0 COMMENT '实际删除行数（purge=0 时恒为 0；Worker 覆盖写，不累加）',
  `skipped`       INT          NOT NULL DEFAULT 0 COMMENT '跳过行数（仍被引用/状态不允许，如 UPLOADING 切片不删）',
  `reason`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '回收原因（审计必填：超期/切片被回放吸收/房间删除），不得为空',
  `operator`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '提交者标识（system / 运营账号），审计归因',
  `version`       BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观并发版本号，每次推进 +1；上报方回传 expected_version',
  `fail_reason`   TINYINT      NOT NULL DEFAULT 0 COMMENT '失败原因（rpc.FailureReason 编号），4 表示对象存储失败；0 表示未指定',
  `errno`         INT          NOT NULL DEFAULT 0 COMMENT '最近一次错误码，0 表示无错误',
  `err_msg`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '脱敏错误摘要（不含堆栈与凭据）',
  `request_id`    VARCHAR(64)  NOT NULL COMMENT '幂等键：同 request_id 重放不重复排队删除意图（唯一索引）',
  `trace_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次调用的链路 ID',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '登记时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`retention_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_target_state` (`target_kind`, `target_id`, `state`),
  KEY `idx_state_id` (`state`, `retention_id`),
  KEY `idx_kind_state` (`target_kind`, `state`),
  KEY `idx_room_state` (`room_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='回收任务（先登记意图、再执行、最后留计数证据，禁止边查边删）';

CREATE TABLE IF NOT EXISTS `live_media_outbox` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（发布器按此升序投递，保证同一聚合顺序）',
  `event_id`       CHAR(26)     NOT NULL COMMENT '事件唯一 ID（ULID，消费者据此幂等去重）',
  `event_type`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件类型：livemedia.transcode.state.changed / livemedia.record.state.changed / livemedia.record.stopped / livemedia.record.gap.detected / livemedia.stream.output.online / livemedia.stream.output.offline / livemedia.replay.review.submitted / livemedia.replay.content.state.changed / livemedia.retention.finished',
  `schema_version` INT          NOT NULL DEFAULT 1 COMMENT '事件 schema 版本，Topic = event_type + ".v" + 版本',
  `aggregate_type` VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '聚合根类型：live_transcode_task / live_stream_output / live_record_task / live_record_segment / live_replay_task / live_replay_asset_ref / live_retention_task',
  `aggregate_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '聚合根主键的字符串形式',
  `room_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '冗余房间维度：排障时按房间查事件流水，不必解析 payload',
  `payload`        MEDIUMTEXT   NOT NULL COMMENT '事件信封完整 JSON（common/eventenvelope.Envelope）；只放主键/状态/时间事实，禁止写入拉流地址、签名参数与对象存储凭据',
  `state`          TINYINT      NOT NULL DEFAULT 0 COMMENT '发布状态：0 待发布（含退避中）、1 已发布、2 超过最大重试转人工（编号与 playback_outbox、model.OutboxState* 一致）',
  `retry_count`    INT          NOT NULL DEFAULT 0 COMMENT '已重试次数（指数退避由发布器计算，超上限置 state=2）',
  `next_retry_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒），0 表示可立即投递',
  `last_error`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误（截断并脱敏，不含堆栈、SQL 片段与凭据）',
  `occurred_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '事件发生时间（Unix 秒），与信封 occurred_at 对应',
  `published_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT '投递成功时间（Unix 秒），0 表示未投递；PurgePublished 按此列归档',
  `trace_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '产生事件的调用 trace_id',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，与业务变更同事务写入）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_state_retry` (`state`, `next_retry_at`, `id`),
  KEY `idx_aggregate` (`aggregate_type`, `aggregate_id`),
  KEY `idx_room_ctime` (`room_id`, `ctime`),
  KEY `idx_state_published` (`state`, `published_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='live-media 领域事件 Outbox（与业务写同事务提交、发布器异步投递、按 event_id 幂等）';
