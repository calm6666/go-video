-- =====================================================================
-- live-gateway 服务 - 房间路由投影 与 接入/广播配额配置
-- =====================================================================
-- 库名：go_video_live_gateway（见 services/live-gateway/etc/livegateway.v1.yaml 的 DataSource）
-- 数据所有者：live-gateway 服务（AGENTS.md §5「关系/动态/互动」之外的实时链路自有域）。
--   本库只保存「重启后需要重建的路由投影」和「运营配置的判定依据」两类数据。
--   room_id 只是对 live-room 的引用、mid 只是对 user-profile 的引用，
--   禁止在本库复制房间状态、用户资料等他人主数据，也不建跨库外键（AGENTS.md §5）。
--
-- 数据分层（services/live-gateway/rpc/livegateway.proto 头部是本服务的第一约束）：
--   * 在线状态真值在 Redis：连接租约、房间订阅成员、心跳计数、广播去重与限流窗口、
--     重连票据的实时有效位，全部易失、可重建，一律不落本库。
--   * 本文件两张表存在的唯一理由：
--     live_gw_room_route   —— 节点进程重启后要恢复「这个房间的广播第一跳在哪个节点」；
--     live_gw_access_quota —— 运营配置的接入/QPS/TTL/载荷上限，是判定的持久依据。
--   * 逐条连接状态（在线表、心跳明细、订阅成员列表）绝不写入 MySQL：
--     这类数据高频、易失、可由 Redis 与客户端重连恢复，落库只会拖垮主库。
--
-- 投影可重算性：
--   live_gw_room_route 是**投影**而非事实源。整表清空后的后果是「房间路由在下一个
--   JoinRoom/AcquireConnectionLease 时重新登记」，不丢业务事实；
--   反过来 live_gw_access_quota 是**配置真值**，清空会丢失全部运营降配（含风控封禁配额），
--   因此两张表的备份与回滚策略不同，不能一起 truncate。
--
-- 幂等与唯一性设计：
--   1. live_gw_room_route 以 room_id 作主键（非自增）：每房间一行，
--      并发首次登记由主键冲突兜住，model.Register 冲突后回读按同一规则判定，不产生重复路由。
--   2. live_gw_access_quota.uniq_scope(scope, scope_id)：一个作用域只有一行配置，
--      model.Create 命中冲突时返回 ErrVersionConflict，强制「先读后带版本更新」，
--      杜绝运营盲写覆盖他人配置。
--   3. live_gw_access_quota.uniq_request_id：写接口幂等键，重放按 FindByRequestID 回读首次结果。
--   4. version 列（两表都有）：乐观并发。model 的 conditionalUpdate 统一 version = version + 1，
--      所有状态推进都是「条件 UPDATE + RowsAffected」，禁止读-改-写两步更新（AGENTS.md §5）。
--
-- 索引取舍（不追求覆盖所有过滤组合）：
--   * replica_nodes 是 JSON 列，JSON_CONTAINS 半条件未建 multi-valued 函数索引：
--     该写法要求 MySQL 8.0.17+，而本仓库 deploy 镜像仅锁到 8.0；且该过滤只出现在
--     运营/发布排障的 ListRoomRoutes 与 DrainRoomRoute 前置查询上（低频、小结果集），
--     为它牺牲版本兼容性不值得。主节点条件由 idx_primary_state 覆盖。
--   * 广播与票据表的高频查询索引放在 000002 文件说明。
--
-- 回滚：
--   DROP TABLE IF EXISTS `live_gw_access_quota`;
--   DROP TABLE IF EXISTS `live_gw_room_route`;
--   （回滚前必须先停写：路由登记来自 JoinRoom/Acquire 热路径，配额写入来自运营面。
--     删表期间 Redis 仍是在线状态真值，服务可用性不受影响，但节点重启后将无路由可恢复。）
--
-- 锁风险：仅建表（CREATE TABLE IF NOT EXISTS），可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
--   提示：live_gw_room_route 是热路径写入表（每次房间内「首个」连接登记一次，
--   同房间后续连接只读），为其加列/加索引需评估在线 DDL 窗口。
-- =====================================================================

-- 回滚：DROP TABLE IF EXISTS `live_gw_room_route`;
CREATE TABLE IF NOT EXISTS `live_gw_room_route` (
  `room_id`       BIGINT      NOT NULL COMMENT '房间 ID（live-room 主键，仅引用不复制房间数据）；每房间一行的主键',
  `primary_node`  VARCHAR(64) NOT NULL COMMENT '主承接节点标识（WS 接入层上报的 node_id），房间广播的第一跳',
  `replica_nodes` JSON        NOT NULL COMMENT '副本节点 JSON 字符串数组（大房间分片广播）；无副本必须写 []，MySQL JSON 列不接受空串',
  `shard_count`   INT         NOT NULL DEFAULT 1 COMMENT '广播分片数（>=1，model.Register 对 <1 强制抬到 1）',
  `state`         TINYINT     NOT NULL DEFAULT 1 COMMENT '路由状态：1 SERVING 承接、2 DRAINING 排空中（只出不进）、3 OFFLINE 已下线',
  `version`       BIGINT      NOT NULL DEFAULT 1 COMMENT '乐观并发版本；DrainRoomRoute 必须回传 expected_version，conditionalUpdate 自增',
  `drain_reason`  VARCHAR(64) NOT NULL DEFAULT '' COMMENT '最近一次排空/下线原因：node_shutdown/deploy/scale（审计必填）',
  `updated_by`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '最后修改者：system 或运营账号标识',
  `trace_id`      VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，0 表示未设置）',
  `mtime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；conditionalUpdate 每次写入',
  PRIMARY KEY (`room_id`),
  KEY `idx_state_room` (`state`, `room_id`),
  KEY `idx_primary_state` (`primary_node`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='房间→节点路由投影（可重算：节点重启/整表清空后由首个 JoinRoom 重新登记；连接数不落本表，实时读 Redis）';

-- 回滚：DROP TABLE IF EXISTS `live_gw_access_quota`;
CREATE TABLE IF NOT EXISTS `live_gw_access_quota` (
  `id`                 BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `scope`              TINYINT     NOT NULL COMMENT '作用域：1 GLOBAL、2 NODE、3 ROOM、4 USER（model.QuotaScopeChain 定义继承链）',
  `scope_id`           BIGINT      NOT NULL DEFAULT 0 COMMENT 'GLOBAL 恒为 0；NODE=node_id 哈希；ROOM=room_id（仅引用）；USER=mid（仅引用）',
  `scope_key`          VARCHAR(64) NOT NULL DEFAULT '' COMMENT '可读标识（如 node_id 原文），仅用于展示与排障，判定一律以 scope+scope_id 为准',
  `max_connections`    INT         NOT NULL DEFAULT 0 COMMENT '该作用域最大连接数；0 表示继承上一层，实时计数在 Redis',
  `broadcast_qps`      INT         NOT NULL DEFAULT 0 COMMENT '广播 QPS 上限；0 继承（最终兜底 config.LiveGateway.DefaultRoomBroadcastQps）',
  `danmaku_qps`        INT         NOT NULL DEFAULT 0 COMMENT '弹幕转发 QPS 上限；0 继承（个体降配走 USER 层）',
  `lease_ttl_seconds`  INT         NOT NULL DEFAULT 0 COMMENT '连接租约 TTL；0 用默认；非 0 也会被 Max/MinLeaseTTLSeconds 夹取',
  `ticket_ttl_seconds` INT         NOT NULL DEFAULT 0 COMMENT '重连票据 TTL；0 用默认；非 0 被 MaxTicketTTLSeconds 夹取',
  `max_payload_bytes`  INT         NOT NULL DEFAULT 0 COMMENT '单条广播/单播载荷上限；0 继承；超限 drop_reason=PAYLOAD_TOO_LARGE',
  `allow_guest`        TINYINT     NOT NULL DEFAULT 0 COMMENT '游客（mid=0）接入：0 继承上层、1 允许。注意：当前无法表达「显式禁止」，需要收紧时只能改 GLOBAL 层（见服务 README 已知缺口）',
  `version`            BIGINT      NOT NULL DEFAULT 1 COMMENT '乐观并发版本；expected_version=0 表示新建，行已存在则返回 ErrVersionConflict',
  `updated_by`         VARCHAR(64) NOT NULL COMMENT '最后修改者（运营账号，必填，审计）',
  `request_id`         VARCHAR(64) NOT NULL COMMENT '写接口幂等键（必填，唯一索引）',
  `trace_id`           VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`              BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`              BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_scope` (`scope`, `scope_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='接入与广播配额配置（配置真值，不可重算，禁止随路由表一起 truncate；只存上限不存计数器）';
