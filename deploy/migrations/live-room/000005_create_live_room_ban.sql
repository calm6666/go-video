-- =====================================================================
-- live-room 服务 - 禁播记录表 live_room_ban
-- =====================================================================
-- 用途：对房间下达/解除禁播的事实记录（谁、何时、多久、什么理由，以及谁解除的）。
--       对应 RPC BanRoom / LiftBan / ListRoomBans，以及 PrepareLive 的 not_banned 检查项。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       本表存「禁播处置」这一业务事实；违规判定的依据与规则明细归 moderation-orchestrator
--       与 risk-control，本表只留 operator_mid 和原因摘要，不复制结论、不存命中规则。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room_ban.go 的
--       liveRoomBanColumns 逐列一致。
--       注意本表**没有 mtime 列**：它是「一次性下达、终态不可回改」的处置记录，
--       model 的 Lift/ExpireDue 只写 lifted_at / state，从未更新过 mtime。
--       保留 ctime 而不补 mtime，是为了让「这张表不能原地改写历史」这件事在结构上可见。
-- 留存口径：解除（state=2）与到期（state=3）都是软状态，行永不物理删除，
--       这样「谁在什么时候以什么理由禁了哪个房间、又是谁解除的」可长期审计（AGENTS.md §8）。
-- 时间语义：start_at/end_at 一律 BIGINT Unix 秒。
--       end_at=0 表示永久禁播（ban_type=2），只能由 LiftBan 解除；
--       到期扫描一律带 end_at>0 前置条件（ExpireDue / ListDue / live_room.ban_until 扫描），
--       所以 0 永不会被当成「早已过期」。
-- 取值校验：ban_type 1 临时 / 2 永久（临时必须 end_at>start_at>0，永久必须 end_at=0，
--       由 model.Insert 前置校验并返回 ErrBanTypeInvalid / ErrBanDurationRequired）；
--       state 1 生效 / 2 已解除 / 3 已过期，迁移矩阵见 model.CanBanRecordTransition。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(ban_id)            FindOne / Lift(ban_id AND state=1) / List 的 ban_id 倒序
--       idx_room_state_end         FindActiveByRoom(room_id AND state=1 AND (end_at=0 OR end_at>now))
--       idx_mid_state_end          HasActiveByMid(mid AND state=1 AND (end_at=0 OR end_at>now))
--                                  —— PrepareLive 每次开播都会走它，属于热路径
--       idx_state_end              ExpireDue / ListDue(state=1 AND end_at>0 AND end_at<=now)
--                                  cron 到期扫描必须能从索引直接收敛，不能扫全表
--       ListRoomBans 的 room_id/mid/state 三个可选过滤分别复用上面三条索引。
-- 唯一键说明：本表刻意不设唯一键。一个房间可以被多次禁播—解除—再禁播，
--       每次处置都必须留下一行；「当前是否被禁播」由 idx_room_state_end 上的
--       state=1 且未到期判定得出，而不是靠唯一约束。
--       BanRoom 的重复提交防护不在本表，而在 live_room_idempotency 的 uniq_dedup_key（000008）。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       ExpireDue 是 UPDATE ... WHERE state=1 AND end_at<=? ORDER BY ban_id ASC LIMIT ?
--       —— 有 LIMIT 的批量更新，按 ban_id 升序取批以避免一次锁全表；
--       但 UPDATE 的 ORDER BY 依赖 idx_state_end 命中，若优化器改走全表扫描会锁到扫描过的行，
--       因此 cron 必须在低峰执行并配 model.SweepBatchLimit（默认 200）。
--       Lift 是主键单行更新（带 state=1 条件），并发重复解除只会返回 false 让调用方按幂等重放处理。
-- 回滚：DROP TABLE IF EXISTS `live_room_ban`;
--       回滚后 000001 的 ban_until 投影列失去事实源（无法重算），
--       且 LiftBan/PrepareLive 的 not_banned 检查会失去依据，只允许与 000001 一起整体回退。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room_ban` (
  `ban_id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '禁播记录 ID（主键；ListRoomBans 按它倒序，BanRoomReply 重放返回原值）',
  `room_id`           BIGINT       NOT NULL COMMENT '被禁房间 ID（live_room 主键引用）',
  `mid`               BIGINT       NOT NULL DEFAULT 0 COMMENT '被禁主播 ID（下达那一刻 live_room.owner_mid 的快照，之后换房主不改写本列）',
  `ban_type`          TINYINT      NOT NULL COMMENT '禁播类型：1 临时（end_at 必须 > start_at）、2 永久（end_at 必须为 0）；0 由 model 直接拒绝',
  `reason`            VARCHAR(255) NOT NULL DEFAULT '' COMMENT '禁播原因（运营内部说明，不下发终端；禁止写入举报人标识、手机号等敏感信息）',
  `start_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '生效时间（Unix 秒）；临时禁播未传时由 model 取当下',
  `end_at`            BIGINT       NOT NULL DEFAULT 0 COMMENT '结束时间（Unix 秒），0 表示永久禁播（到期扫描一律带 end_at>0，故 0 不会被误判为已过期）',
  `state`             TINYINT      NOT NULL DEFAULT 1 COMMENT '记录状态：1 生效、2 已人工解除、3 到期失效；2/3 为终态，不可回改',
  `operator_mid`      BIGINT       NOT NULL COMMENT '下达处置的运营/系统 ID，必须 > 0（model.ErrOperatorRequired）；系统自动处置约定用固定的系统账号 ID',
  `lift_operator_mid` BIGINT       NOT NULL DEFAULT 0 COMMENT '解除人 ID，0 表示未解除（到期失效同样保持 0，因为没有人执行解除）',
  `lift_reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '解除原因（运营内部说明，不下发终端）',
  `lifted_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '解除时间（Unix 秒），0 表示未人工解除；到期失效不写本列，以便区分「人解除的」与「自己到期的」',
  `trace_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '下达时的链路追踪 ID',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '下达时间（Unix 秒）；本表无 mtime —— 处置记录不可原地改写，只能追加新记录',
  PRIMARY KEY (`ban_id`),
  KEY `idx_room_state_end` (`room_id`, `state`, `end_at`),
  KEY `idx_mid_state_end` (`mid`, `state`, `end_at`),
  KEY `idx_state_end` (`state`, `end_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='禁播处置记录（追加型审计事实，解除/到期为软终态，行不物理删除；无唯一键，一房间可多次处置）';
