-- =====================================================================
-- live-ingest 服务 - 推流密钥 / 接入节点 / 节点分配（接入控制面）
-- =====================================================================
-- 用途：签发并校验推流密钥、登记接入节点、记录「流 → 节点」分配与配额占用。
--       对应 liveingest.v1 的 IssueStreamKey / VerifyPublishAuth / RotateStreamKey /
--       RevokeStreamKey / GetStreamKey / ListStreamKeys / UpsertIngestNode /
--       ListIngestNodes / AssignIngestNode / ReleaseIngestNode / ListNodeAssignments。
-- 数据所有者：live-ingest 服务（AGENTS.md §5）。room 业务态归 live-room，本库只落
--       room_id / session_id 等引用；跨服务不建外键，禁止其他服务直接写这三张表。
-- 密钥安全：live_stream_key 只有 key_hash（SHA-256 hex）、key_ref（Secret/Vault 引用）
--       与 key_tail（末 4 位辨认串）；明文推流密钥永不入库、不入日志、不回显（AGENTS.md §6）。
-- 幂等依赖：uniq_key_hash（同一明文全局唯一）、uniq_request_id（签发/轮转重放）、
--       live_node_assignment.uniq_request_id（分配重放）——写接口冲突后回查首条并按重放返回。
-- 时间列：全部 BIGINT Unix 秒，0 表示「未发生/不适用」，不使用 DATETIME。
-- 锁风险：CREATE TABLE IF NOT EXISTS 只取元数据锁，可在线执行；不含 ALTER，不重写已有表。
--       注意 live_ingest_node.active_streams 是配额计数列，分配/释放的并发更新会锁单行，
--       与 live_node_assignment 的写入必须在同一事务内，否则容量会漂移（logic 层保证）。
-- 回滚：DROP TABLE IF EXISTS `live_node_assignment`;
--       DROP TABLE IF EXISTS `live_ingest_node`;
--       DROP TABLE IF EXISTS `live_stream_key`;
--       （反向顺序：先删分配与节点，再删密钥；已下发的明文密钥需另行走轮转流程吊销。）
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_stream_key` (
  `key_id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（跨服务只传 key_id 引用）',
  `stream_name`       VARCHAR(128) NOT NULL COMMENT '流标识（推流 URL 的 name 段，可下发客户端）',
  `key_hash`          CHAR(64)     NOT NULL COMMENT '明文推流密钥的 SHA-256 hex；鉴权只比对哈希，禁止回显',
  `key_ref`           VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'Secret/Vault 引用（明文由 Vault 侧管理，本服务不写不读）',
  `key_tail`          VARCHAR(8)   NOT NULL DEFAULT '' COMMENT '明文末 4 位，仅供主播在多个密钥间辨认，不参与鉴权',
  `room_id`           BIGINT       NOT NULL COMMENT '房间引用（live-room 主键，仅引用不写回）',
  `session_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '场次引用，0 表示未绑定场次',
  `anchor_mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '主播 ID（权限收敛与列表过滤用）',
  `protocol_mask`     INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '允许协议位图：1 RTMP、2 SRT、4 WebRTC（可组合）',
  `state`             TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 ACTIVE、2 ROTATING、3 RETIRED、4 EXPIRED、5 REVOKED',
  `version`           INT          NOT NULL DEFAULT 1 COMMENT '轮转代次，首发 1、每次轮转 +1',
  `prev_key_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '轮转来源密钥，0 表示首发',
  `rotate_to_key_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '轮转后继密钥，0 表示未轮转',
  `current_stream_id` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '当前非终态流 ID（活跃指针）；空串表示无活跃流',
  `max_streams`       INT          NOT NULL DEFAULT 1 COMMENT '并发非终态流上限（配额判定）',
  `expire_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '过期时间（Unix 秒），0 表示不过期',
  `grace_until`       BIGINT       NOT NULL DEFAULT 0 COMMENT '轮转宽限截止（Unix 秒），0 表示不适用',
  `reason`            VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最近一次状态变更原因（禁止写入密钥材料）',
  `request_id`        VARCHAR(64)  NOT NULL COMMENT '签发/轮转幂等键（必填，唯一索引即重放防线）',
  `trace_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`key_id`),
  UNIQUE KEY `uniq_key_hash` (`key_hash`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_stream_name_state` (`stream_name`, `state`, `key_id`),
  KEY `idx_room_state` (`room_id`, `state`),
  KEY `idx_anchor_state` (`anchor_mid`, `state`),
  KEY `idx_state_expire` (`state`, `expire_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='推流密钥：只存哈希与 Vault 引用，明文一次都不落库';

CREATE TABLE IF NOT EXISTS `live_ingest_node` (
  `node_id`           VARCHAR(64)  NOT NULL COMMENT '节点 ID（运维分配的稳定标识，主键即幂等键）',
  `name`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '展示名',
  `region`            VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '地理区域码（就近分配依据）',
  `protocol_mask`     INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '支持协议位图：1 RTMP、2 SRT、4 WebRTC（分配按位与匹配）',
  `endpoint_rtmp`     VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'RTMP 接入地址（可下发，不含密钥）',
  `endpoint_srt`      VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'SRT 接入地址',
  `endpoint_webrtc`   VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'WebRTC 信令地址',
  `state`             TINYINT      NOT NULL DEFAULT 3 COMMENT '状态：1 ONLINE、2 DRAINING、3 OFFLINE（默认离线，未登记的节点不进分配池）',
  `capacity_streams`  INT          NOT NULL DEFAULT 0 COMMENT '并发流配额上限（允许 -1 作为「恒真」占位比较，不参与业务）',
  `active_streams`    INT          NOT NULL DEFAULT 0 COMMENT '当前占用流数（由分配记录派生，允许与对账值短暂偏差）',
  `health_score`      TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '健康分 0~100，节点分配打分依据',
  `last_heartbeat_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '最近心跳（Unix 秒），超时由扫描器置 OFFLINE',
  `labels`            VARCHAR(255) NOT NULL DEFAULT '' COMMENT '运维标签（k=v;k=v），仅排障与人工筛选',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`node_id`),
  KEY `idx_state_health` (`state`, `health_score`),
  KEY `idx_state_heartbeat` (`state`, `last_heartbeat_at`),
  KEY `idx_region_state` (`region`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='接入节点登记表（含容量配额与健康分，注册即心跳）';

CREATE TABLE IF NOT EXISTS `live_node_assignment` (
  `assignment_id`  BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `request_id`     VARCHAR(64)  NOT NULL COMMENT '分配幂等键（必填，唯一索引即重放防线）',
  `stream_id`      VARCHAR(64)  NOT NULL COMMENT '被分配的流（live_stream 主键，仅引用）',
  `room_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '房间引用（冗余，容量对账与排障）',
  `key_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '使用的密钥 ID（引用，不含任何密钥材料）',
  `node_id`        VARCHAR(64)  NOT NULL COMMENT '分配到的接入节点',
  `protocol`       TINYINT      NOT NULL DEFAULT 0 COMMENT '接入协议枚举：1 RTMP、2 SRT、3 WebRTC（不是位图）',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 ACTIVE、2 RELEASED、3 MIGRATED；仅 ACTIVE 占用节点配额',
  `score`          INT          NOT NULL DEFAULT 0 COMMENT '分配打分快照（观测为什么选中该节点）',
  `prev_node_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '迁移来源节点，空串表示首次分配',
  `reason`         VARCHAR(255) NOT NULL DEFAULT '' COMMENT '分配原因',
  `release_reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '释放/迁移原因',
  `assigned_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '分配时间（Unix 秒）',
  `released_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '释放时间（Unix 秒），0 表示未释放',
  `trace_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`assignment_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_stream_state` (`stream_id`, `state`, `assignment_id`),
  KEY `idx_node_state` (`node_id`, `state`),
  KEY `idx_room_ctime` (`room_id`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='流→接入节点分配记录（迁移与释放保留历史行，供容量对账）';
