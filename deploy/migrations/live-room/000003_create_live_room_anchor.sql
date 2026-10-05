-- =====================================================================
-- live-room 服务 - 主播绑定表 live_room_anchor
-- =====================================================================
-- 用途：房间与主播的绑定关系（房主 / 联合主播 / 房管）。
--       对应 RPC MutateAnchor / ListAnchors，以及 StartLive / PrepareLive / UpdateRoomInfo
--       的「操作者是否为生效绑定主播」判定。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       mid 只是 user-profile / account 的用户主键引用，不复制昵称头像等可变主资料；
--       「谁是这个房间的房主」的唯一事实源是本表（live_room.owner_mid 是它的投影）。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room_anchor.go 的
--       liveRoomAnchorColumns 逐列一致。
-- 幂等与不变量依赖的唯一键：
--       uniq_room_mid_role (room_id, mid, role)
--           LiveRoomAnchorModel.Bind 的 INSERT ... ON DUPLICATE KEY UPDATE 冲突键。
--           「解绑后重新启用」因此复用同一行（把 state 置回 1），不产生第二行绑定。
--       uniq_active_owner (owner_room_id)
--           「一个房间同一时刻只有一个生效房主」的数据库级强制手段。
--           owner_room_id 仅在「生效 + 房主」时写 room_id，其余一律 NULL：
--           靠 MySQL 唯一索引「NULL 不参与唯一性」的语义，非房主行与已解绑行可无限多，
--           而持有同一 room_id 占位的行最多一条。因此本列必须可空，
--           改成 NOT NULL DEFAULT 0 会让所有非房主行都撞在 0 上（唯一索引立刻不可用）。
-- 软删除：解绑是 state=0，行永不物理删除——「某场直播当时谁是房管」必须能长期回答
--       （AGENTS.md §8 审计留存）。这也意味着 uniq_room_mid_role 是「同角色同人在同房间
--       最多一行」，不是「最多一个生效绑定」。
-- 已知竞态风险（留给逻辑轮，务必先读）：本表有两个唯一键，而 Bind 用的是
--       INSERT ... ON DUPLICATE KEY UPDATE。MySQL 在冲突时只更新「第一个命中的唯一键」所在行：
--       若新房主绑定时 uniq_room_mid_role 未命中但 uniq_active_owner 命中（房间内已有别的生效房主），
--       被更新的会是「原房主那一行」，表现为换房主静默失败而不是报 ErrDuplicateOwner。
--       因此 Bind 对 role=OWNER 的调用必须走 TransferOwner（先释放旧占位、再让新行占位，同一事务），
--       或在调用前先探测生效房主；把 Bind 当万能 upsert 用会写错行。此风险已在服务 README
--       「已知缺口」登记，需要维护者确认逻辑轮方案。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(id)               List/Count 的 "ORDER BY role ASC, id ASC" 尾部、绑定行去重
--       uniq_room_mid_role        Find(room_id,mid,role) / IsEnabled(room_id,mid) / Bind 冲突键
--       uniq_active_owner         FindOwner 的占位语义 + 房主唯一性
--       idx_room_role_state       FindOwner(room_id,role,state) 与 List/Count(room_id[,role][,state])
--                                 —— 不能靠 uniq_room_mid_role：它第二列是 mid，而这两类查询不给 mid
--       idx_mid_state_room        CountActiveRoomsByMid（COUNT(DISTINCT room_id)）与
--                                 ListRoomsByMid（room_id 倒序 + LIMIT）
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       TransferOwner 在一个事务里做「UPDATE 释放旧占位 → INSERT...ON DUPLICATE 占新位」，
--       两行都在同一 room_id 上；并发换房主会因 uniq_active_owner 互斥，
--       第二个事务拿到重复键并翻译成 ErrDuplicateOwner（不是故障，是可重试）。
--       事务顺序固定为「先释放再占位」，反向会在同一事务内自己撞自己的唯一键。
--       本表写入频率低（绑定/解绑是运营与主播手工动作），索引多一个不影响写放大。
-- 回滚：DROP TABLE IF EXISTS `live_room_anchor`;
--       回滚前必须先停 MutateAnchor/ListAnchors；本表被删后 live_room.owner_mid 投影
--       将无法重算（000001 的头注已声明其真值在此），故只允许与 000001 一起整体回退。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room_anchor` (
  `id`            BIGINT  NOT NULL AUTO_INCREMENT COMMENT '绑定记录 ID（主键）',
  `room_id`       BIGINT  NOT NULL COMMENT '房间 ID（live_room 主键引用）',
  `mid`           BIGINT  NOT NULL COMMENT '主播/房管用户 ID（user-profile 主键引用，不复制用户主资料）',
  `role`          TINYINT NOT NULL COMMENT '角色：0 未指定（写入即拒）、1 OWNER 房主、2 COHOST 联合主播、3 MANAGER 房管；取值与 rpc.AnchorRole 锁定',
  `state`         TINYINT NOT NULL DEFAULT 1 COMMENT '生效位：1 生效、0 已解绑（软状态，行保留作为审计证据，不物理删除）',
  `owner_room_id` BIGINT  NULL DEFAULT NULL COMMENT '生效房主占位列：仅「state=1 且 role=1」时写 room_id，其余必须为 NULL（NULL 不参与唯一性，是 uniq_active_owner 成立的前提）',
  `ctime`         BIGINT  NOT NULL DEFAULT 0 COMMENT '首次绑定时间（Unix 秒）；重新启用不刷新 ctime，只刷新 mtime',
  `mtime`         BIGINT  NOT NULL DEFAULT 0 COMMENT '最近一次状态变更时间（Unix 秒）；Bind/Unbind/TransferOwner 都刷新',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_room_mid_role` (`room_id`, `mid`, `role`),
  UNIQUE KEY `uniq_active_owner` (`owner_room_id`),
  KEY `idx_room_role_state` (`room_id`, `role`, `state`, `id`),
  KEY `idx_mid_state_room` (`mid`, `state`, `room_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='主播绑定（房主唯一性由可空占位列 + 唯一索引在 DB 层强制；解绑是软状态）';
