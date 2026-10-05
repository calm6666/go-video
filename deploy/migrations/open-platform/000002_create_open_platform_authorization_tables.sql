-- =====================================================================
-- open-platform 服务 - OAuth 授权码、授权关系（撤销位点）与令牌
-- =====================================================================
-- owner：open-platform 服务（数据所有者，AGENTS.md §5）。库：go_video_open_platform。
-- 影响：新建 3 张表（授权码、授权关系、令牌），不改动任何已有表；
--       不建跨服务外键，mid/app_id 只保存 account/op_app 的主键引用。
-- 凭证存储：三张表全部只存 salt + 哈希（HMAC-SHA256(pepper, salt||明文)），
--       pepper 来自 Secret/Vault 不入库；明文只在签发响应中出现一次，
--       禁止出现在任何查询响应、日志与事件里（与 op_app_secret 同一口径）。
-- 回滚：DROP TABLE IF EXISTS `op_token`,`op_grant`,`op_auth_code`;
--       回滚等价于「全部第三方授权失效」，必须先停用 gateway 开放域入口并公告；
--       op_grant 是撤销位点的唯一载体，回滚前必须完成授权关系导出（合规要求）。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
-- 后续清理：短期凭证按 README 记录的留存策略由 cron 物理清理
--       （op_auth_code 过期即清、op_token 到期归档后清），本文件不预置清理任务行。
-- =====================================================================

-- 授权码表：短期（AuthCodeTTLSeconds，默认 60s）+ 一次性消费。
-- used_at 从 0 翻到非 0 是 CAS，保证同一 code 只换出一次 token；
-- 重放（used_at 已非 0）累加 replay_count 供风控告警，并返回 ErrAuthCodeUsed。
CREATE TABLE IF NOT EXISTS `op_auth_code` (
  `code_id`               BIGINT       NOT NULL AUTO_INCREMENT COMMENT '授权码行 ID（主键）',
  `app_id`                BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID（op_app.app_id）',
  `mid`                   BIGINT       NOT NULL DEFAULT 0 COMMENT '授权用户 mid（account 主键引用）',
  `salt`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '随机盐（hex）。禁止出现在响应与日志',
  `hash`                  CHAR(64)     NOT NULL DEFAULT '' COMMENT '授权码哈希（hex），换码入口。禁止回显',
  `scope`                 VARCHAR(512) NOT NULL DEFAULT '' COMMENT '用户已同意的 scope 快照（逗号分隔、升序），换码时直接继承',
  `redirect_uri`          VARCHAR(512) NOT NULL DEFAULT '' COMMENT '签发时回调地址（换码需一致，防授权码被转移）',
  `state`                 VARCHAR(128) NOT NULL DEFAULT '' COMMENT '客户端 state，仅用于回显与排障',
  `grant_id`              BIGINT       NOT NULL DEFAULT 0 COMMENT '预建的授权关系 ID（撤销位点连贯）',
  `expires_at`            BIGINT       NOT NULL DEFAULT 0 COMMENT '过期时间（Unix 秒）',
  `used_at`               BIGINT       NOT NULL DEFAULT 0 COMMENT '消费时间（0 表示未消费）',
  `consumed_by_token_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '消费本 code 签发的 token 行 ID（0 表示未消费）',
  `replay_count`          INT          NOT NULL DEFAULT 0 COMMENT '重放尝试次数（>0 说明 code 可能泄露，需告警）',
  `ctime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '签发时间（Unix 秒，签发限频统计按本列）',
  `mtime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`code_id`),
  UNIQUE KEY `uniq_code_hash` (`hash`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_app_mid_ctime` (`app_id`, `mid`, `ctime`),
  KEY `idx_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='OAuth 授权码表：只存哈希，短期一次性消费，重放即告警';

-- 授权关系表：一个（应用，用户）一行，同时是「撤销位点」。
-- 唯一键 (app_id, mid) 是刻意的：重新授权复用同一行并覆盖 scope 快照、清空 revoked_at，
-- 避免出现「同一用户对同一应用有多份并行授权」导致撤销漏网。
-- revoked_at 语义：token 校验时若 token.ctime <= revoked_at 即拒绝（即使某行未被逐条标记），
-- 与 op_token.state 双保险，保证「撤销立即生效」。
CREATE TABLE IF NOT EXISTS `op_grant` (
  `grant_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '授权关系 ID（跨服务引用主键）',
  `app_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID',
  `mid`              BIGINT       NOT NULL DEFAULT 0 COMMENT '授权用户 mid',
  `scope`            VARCHAR(512) NOT NULL DEFAULT '' COMMENT '用户同意的 scope 快照（逗号分隔、升序；token 只继承不扩大）',
  `status`           TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 有效、2 已撤销（revoked_at 才是真值，本列供展示与索引）',
  `consent_given`    TINYINT      NOT NULL DEFAULT 0 COMMENT '用户显式同意标记：0 从未同意、1 已同意（服务端不接受隐式同意）',
  `consent_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次同意时间（Unix 秒）',
  `current_token_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '当前有效 token 链头（轮换时前移；0 表示从未换出 token）',
  `rotate_seq`       BIGINT       NOT NULL DEFAULT 0 COMMENT '轮换代数（观测 refresh 是否被异常高频轮换）',
  `last_code_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次消费的授权码（审计链：grant ← code ← token）',
  `revoked_at`       BIGINT       NOT NULL DEFAULT 0 COMMENT '撤销位点（Unix 秒，0 表示未撤销）',
  `revoke_reason`    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '撤销原因（审计，脱敏）',
  `revoke_operator`  BIGINT       NOT NULL DEFAULT 0 COMMENT '撤销触发者：用户本人 mid 或运营 mid',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '首次授权时间（Unix 秒）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒，列表游标第 1 列）',
  PRIMARY KEY (`grant_id`),
  UNIQUE KEY `uniq_app_mid` (`app_id`, `mid`),
  KEY `idx_mid_mtime` (`mid`, `mtime`, `grant_id`),
  KEY `idx_app_mtime` (`app_id`, `mtime`, `grant_id`),
  KEY `idx_mid_revoked` (`mid`, `revoked_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='用户对第三方应用的授权关系表，兼作撤销位点（撤销立即生效的第二道保险）';

-- 令牌表：一行 = 一次签发的 access + refresh 组合，全部只存哈希。
-- 轮换链：RefreshAccessToken 先把旧行 CAS 置 ROTATED，再插入新行并前移 op_grant.current_token_id，
-- parent_token_id 记录来源；旧 refresh 值再次出现即判定重放，logic 撤销整条 grant。
CREATE TABLE IF NOT EXISTS `op_token` (
  `token_id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'token 行 ID（主键，Introspect 可按此查询）',
  `grant_id`            BIGINT       NOT NULL DEFAULT 0 COMMENT '所属授权关系（撤销位点锚点）',
  `app_id`              BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID（冗余，避免每次校验回查 grant）',
  `mid`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '授权用户 mid（冗余；0 预留给未来的应用级凭证，本期不签发）',
  `grant_type`          TINYINT      NOT NULL DEFAULT 1 COMMENT '签发方式：1 授权码换发、2 refresh 轮换（本期只开放这两种）',
  `access_salt`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'access token 盐（hex）。禁止出现在响应与日志',
  `access_hash`         CHAR(64)     NOT NULL DEFAULT '' COMMENT 'access token 哈希（hex），校验入口。禁止回显',
  `refresh_salt`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'refresh token 盐（hex）',
  `refresh_hash`        CHAR(64)     NOT NULL DEFAULT '' COMMENT 'refresh token 哈希（hex），轮换入口',
  `scope`               VARCHAR(512) NOT NULL DEFAULT '' COMMENT '实际授予 scope 快照（可能小于申请值；刷新只允许收窄）',
  `access_expires_at`   BIGINT       NOT NULL DEFAULT 0 COMMENT 'access 过期时间（Unix 秒）',
  `refresh_expires_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT 'refresh 过期时间（Unix 秒）',
  `state`               TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 有效（仍需比对 grant 位点与应用状态）、2 已轮换、3 已撤销、4 已过期（惰性归档）',
  `parent_token_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '轮换来源行（0 表示由授权码首发）',
  `rotated_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '被轮换时间（Unix 秒，0 表示未轮换）',
  `revoked_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '被撤销时间（Unix 秒，0 表示未撤销）',
  `revoke_reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '撤销/轮换原因（审计，脱敏）',
  `last_used_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次校验通过时间（限频写，避免每请求写库）',
  `ctime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '签发时间（Unix 秒，与 grant.revoked_at 比对）',
  `mtime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '最近状态变更时间（Unix 秒）',
  PRIMARY KEY (`token_id`),
  UNIQUE KEY `uniq_access_hash` (`access_hash`),
  UNIQUE KEY `uniq_refresh_hash` (`refresh_hash`),
  KEY `idx_grant_token` (`grant_id`, `token_id`),
  KEY `idx_app_state` (`app_id`, `state`),
  KEY `idx_mid_state` (`mid`, `state`),
  KEY `idx_state_expires` (`state`, `access_expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='access/refresh 令牌表：只存哈希与轮换链，明文只在签发响应出现一次';
