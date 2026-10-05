-- =====================================================================
-- live-gateway 服务 - 广播审计流水 与 断线重连票据审计/撤销名单
-- =====================================================================
-- 库名：go_video_live_gateway（见 services/live-gateway/etc/livegateway.v1.yaml 的 DataSource）
-- 数据所有者：live-gateway 服务。本文件两张表都是「只存摘要与结论」的审计表，
--   不是任何在线判定的事实源，也不是业务主数据。
--
-- 为什么不存逐条连接状态（本文件最重要的约束）：
--   * 实时连接真值在 Redis：租约、心跳、房间订阅成员、广播去重窗口、票据的「能不能换」有效位。
--   * 本文件两张表只回答两类无法从 Redis 重算的问题：
--     live_gw_broadcast_log    —— 「这条消息为什么没到？」（受理/丢弃/越权/重复都必须可解释）
--     live_gw_reconnect_ticket —— 「谁在什么时候签发/使用/撤销了哪张票」（含撤销名单）
--   * 因此两张表都没有 conn 级别的行：一条连接活多久会产生多少次心跳，
--     落库规模会把主库打挂（proto 头部注释与 services/live-gateway/README.md 同一条约束）。
--
-- 投影可重算性：
--   两张表都**不参与下发判定**，清空后的后果只是「历史审计丢失 + 撤销名单需由 Redis 承担」，
--   业务事实不丢；Redis 去重窗口（分钟级 TTL）仍在，重复投递仍会被拦。
--   正因如此它们的保留期可以短（广播流水默认 30 天，见 config.BroadcastLogRetentionDays），
--   并且不需要备份到与配置表同等的等级。
--
-- 幂等与唯一性设计：
--   1. live_gw_broadcast_log.uniq_room_message(room_id, message_id)：广播幂等的持久防线。
--      同一 (房间, 消息) 只留首行——重复投递不追加行（否则审计表会被上游重放灌满）；
--      model.Insert 命中冲突返回 (0, nil)，调用方据此回 duplicated=true。
--      重复计数的实时口径由 Redis 去重窗口承担，所以「Redis 观测的重复数 > DB duplicated 行数」
--      是设计如此，不是数据不一致。
--   2. live_gw_reconnect_ticket.uniq_ticket_hash：Redeem 路径客户端只带票据原文，
--      服务端只存 sha256 摘要（CHAR(64)），按摘要反查。**票据原文永不落库**——
--      它是可重放的 HMAC 凭据，落明文等于把重连能力交给 DBA、备份链路与任何读到该表的人。
--   3. ticket.uniq_ticket_id / uniq_request_id：签发幂等（request_id 冲突回读首次结果，
--      不产生第二张票）与撤销定位。
--   4. 票据是一次性凭据：Consume 的 WHERE 同时带 state=ISSUED + room_id + mid + expire_at>?，
--      三元组不匹配直接 0 行 → 调用方回读翻译成 ErrTripletMismatch（越权），
--      绝不「消费成功」。USED 与 REVOKED 互斥且均不可逆（model.ticketTransitions）。
--   5. 这两张表都没有 version 列：model 对它们只调用 bumpVersion=false 的 conditionalUpdate
--      或直接 INSERT，状态推进的并发安全靠「条件 UPDATE 的 WHERE 里带 state」表达，
--      不需要额外乐观锁位；加 version 反而会让读代码的人误以为存在外部并发协议。
--
-- 索引取舍（审计/票据表按写入热路径最小化二级索引）：
--   * broadcast_log 是写频率最高的表（每条广播一行），只保留：唯一键 + room_id 前缀的
--     时间轴索引（List/CountByRoomSince 共用）+ event_id 定位 + ctime（PurgeBefore 批量删）。
--     未单独建 (room_id, sender_mid)：sender_mid 过滤属低频取证查询，
--     由 idx_room_ctime 收窄 room_id 后回表足够，多一个二级索引会让每次广播多写一棵 B+ 树。
--   * ticket 的 idx_room_mid_state 同时服务 RevokeByRoomMid / CountUnusedByRoomMid / List；
--     idx_state_expire 服务 MarkExpired 清扫。
--   * RevokeByRoomMid / MarkExpired / PurgeBefore 都是带 LIMIT 的批量语句，
--     目的就是在「票据风暴」或积压清理时不一次锁住过多行。
--
-- 商业化边界（AGENTS.md §1）：kind 枚举只含社区与运行事件，本表不承载会员、
--   投币、支付、广告相关的消息类别或字段。
--
-- 回滚：
--   DROP TABLE IF EXISTS `live_gw_reconnect_ticket`;
--   DROP TABLE IF EXISTS `live_gw_broadcast_log`;
--   （回滚前停写广播路径；撤销名单丢失时，必须先清空 Redis 票据有效位再重建，
--     否则已签发的在用票据会失去撤销能力。）
--
-- 锁风险：仅建表（CREATE TABLE IF NOT EXISTS），可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
--   提示：live_gw_broadcast_log 增长最快，禁止用无 WHERE 的 DELETE 清理，
--   必须走 model.PurgeBefore 的分批 DELETE ... LIMIT，并在低峰执行。
-- =====================================================================

-- 回滚：DROP TABLE IF EXISTS `live_gw_broadcast_log`;
CREATE TABLE IF NOT EXISTS `live_gw_broadcast_log` (
  `id`                    BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键（List 按 id DESC 取最近流水）',
  `message_id`            VARCHAR(64) NOT NULL COMMENT '广播幂等键（调用方或服务端生成；房间维度唯一，禁止服务端补随机值绕过）',
  `room_id`               BIGINT      NOT NULL COMMENT '房间 ID（live-room 主键，仅引用）；审计按房间查，List 不给 room_id 直接拒绝以免全表扫',
  `kind`                  TINYINT     NOT NULL COMMENT '消息类别：1 弹幕、2 房间状态、3 系统、4 社区互动、5 审核处置、6 主播提词（无商业化类别）',
  `sender_mid`            BIGINT      NOT NULL DEFAULT 0 COMMENT '发送者 mid（仅引用，系统消息为 0）；不复制用户资料',
  `sender_role`           TINYINT     NOT NULL DEFAULT 0 COMMENT '发送者角色（服务端判定结果，不是客户端自报值）：1 观众、2 主播、3 房管、4 运营、5 内部服务',
  `event_id`              VARCHAR(64) NOT NULL DEFAULT '' COMMENT '上游系统事件 ID（ForwardSystemEvent 来源）；非事件投递为空串，故不建唯一键',
  `payload_digest`        CHAR(64)    NOT NULL DEFAULT '' COMMENT '载荷 SHA-256 hex 前缀（长度由 config.PayloadDigestBytes 决定）；正文永不入库',
  `payload_bytes`         INT         NOT NULL DEFAULT 0 COMMENT '载荷字节数（配合 digest 判断是否被截断/过大）',
  `fanout_nodes`          INT         NOT NULL DEFAULT 0 COMMENT '本次扇出节点数（DRAINING 节点不计入）',
  `targeted_connections`  INT         NOT NULL DEFAULT 0 COMMENT '估算命中的连接数（Redis 读数；0 不必然代表失败，可能房间无人）',
  `state`                 TINYINT     NOT NULL COMMENT '结论：1 已下发、2 已丢弃、3 越权拒绝、4 重复丢弃',
  `drop_reason`           TINYINT     NOT NULL DEFAULT 0 COMMENT '原因码（rpc.DropReason）：state=1 时恒为 1 OK，其余必须有可解释原因（model.Insert 强校验）',
  `source_service`        VARCHAR(32) NOT NULL DEFAULT '' COMMENT '来源服务名（live-room/danmaku/moderation/gateway-app 等，审计归因）',
  `trace_id`              VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`                 BIGINT      NOT NULL DEFAULT 0 COMMENT '受理时间（Unix 秒）；保留期外由 PurgeBefore 分批清理',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_room_message` (`room_id`, `message_id`),
  KEY `idx_room_ctime` (`room_id`, `ctime`),
  KEY `idx_room_event` (`room_id`, `event_id`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='广播/单播/事件转发审计流水（append-only，只存摘要不存正文；不参与下发判定，可设短保留期）';

-- 回滚：DROP TABLE IF EXISTS `live_gw_reconnect_ticket`;
CREATE TABLE IF NOT EXISTS `live_gw_reconnect_ticket` (
  `id`             BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `ticket_id`      VARCHAR(32) NOT NULL COMMENT '票据 ID（ULID）：撤销与审计主键，客户端换取时不作为定位依据（定位用 hash）',
  `ticket_hash`    CHAR(64)    NOT NULL COMMENT 'sha256(票据原文) hex（唯一索引）。**绝不存票据明文**：票据是 HMAC 签名的一次性重连凭据',
  `room_id`        BIGINT      NOT NULL COMMENT '绑定房间 ID（live-room 主键，仅引用）；Redeem 时 room_id 必须一致，否则判越权',
  `mid`            BIGINT      NOT NULL DEFAULT 0 COMMENT '绑定 mid（仅引用）；0 表示游客票据（批量撤销按 room+mid 时 mid<=0 被 model 拒绝）',
  `conn_id`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '签发时的连接标识（排障：断线重连是否换过连接）',
  `node_id`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '签发时的承载节点（重连落到不同节点时可对账）',
  `role`           TINYINT     NOT NULL DEFAULT 1 COMMENT '签发时判定的角色（换取租约时沿用此值，不再接受客户端重新声明；model.Insert 再兜一次 NormalizeRole）',
  `state`          TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 ISSUED 未使用、2 USED 已换取、3 REVOKED 已撤销、4 EXPIRED 已过期（USED/REVOKED 互斥且不可逆）',
  `issued_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '签发时间（Unix 秒）',
  `expire_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '过期时间（Unix 秒）；必须 > issued_at，否则 model.Insert 拒绝',
  `used_at`        BIGINT      NOT NULL DEFAULT 0 COMMENT '使用时间（Unix 秒），0 表示未使用（与各 *_at「0=未设置」的契约一致）',
  `lease_id`       VARCHAR(32) NOT NULL DEFAULT '' COMMENT '签发所依托的原租约 ID（无租约不得发票据，此列恒非空是签发前提）',
  `new_lease_id`   VARCHAR(32) NOT NULL DEFAULT '' COMMENT '换取到的新租约 ID（state=USED 时回填，串起「断开→重连」链路）',
  `issue_reason`   VARCHAR(32) NOT NULL DEFAULT '' COMMENT '签发场景：normal/reconnect/room_switch',
  `revoked_reason` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '撤销原因（审计必填）：banned/kicked/room_closed/risk',
  `revoked_by`     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '撤销操作者（运营账号或内部服务标识）',
  `request_id`     VARCHAR(64) NOT NULL COMMENT '签发幂等键（唯一索引；冲突回读既有票据，不产生第二张票）',
  `trace_id`       VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；Consume/Revoke/MarkExpired 均写入（conditionalUpdate bumpVersion=false 仍刷 mtime）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_ticket_id` (`ticket_id`),
  UNIQUE KEY `uniq_ticket_hash` (`ticket_hash`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_room_mid_state` (`room_id`, `mid`, `state`),
  KEY `idx_state_expire` (`state`, `expire_at`),
  KEY `idx_lease_id` (`lease_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='断线重连票据审计与撤销名单（只存摘要；「能不能换」的实时判定在 Redis，本表回答谁签发/谁撤销）';
