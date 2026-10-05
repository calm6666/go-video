-- =====================================================================
-- open-platform 服务 - 第三方应用、密钥、权限点目录与 scope 审批
-- =====================================================================
-- owner：open-platform 服务（数据所有者，AGENTS.md §5）。库：go_video_open_platform。
-- 影响：新建 4 张表（应用主体、密钥哈希、权限点目录、应用-scope 审批关系）
--       并 seed 权限点目录与全局默认配额之外的开放范围；不改动任何已有表；
--       不建跨服务外键，owner_mid/mid 只保存 account/user-profile 的主键引用。
-- 范围红线（AGENTS.md §1）：本文件 seed 的 scope 全部是内容/互动/资料类非商业化能力，
--       不存在会员、订单、支付、投币、分成、广告相关权限点；新增能力必须先过范围评审，
--       再以新迁移追加 seed，model 层的 IsForbiddenScopeCategory 会拒绝命中禁用词根的写入。
-- 回滚：DROP TABLE IF EXISTS `op_app_scope`,`op_scope`,`op_app_secret`,`op_app`;
--       回滚会使全部第三方授权失效，必须先撤销线上入口（gateway 停用开放域名）并公告；
--       op_app_secret 只存哈希，回滚不可还原任何明文密钥（明文只在签发响应中出现过）。
-- 锁风险：仅建表 + 幂等 seed（INSERT ... ON DUPLICATE KEY UPDATE 只碰目录描述列），
--       无 ALTER，无锁风险；seed 重复执行不会覆盖运营改过的 enabled/disable_reason。
-- =====================================================================

-- 应用主体表：状态机 + 乐观锁版本 + 注册幂等键。
-- Version 任何资料/状态变更都会自增，写接口要求携带读到的版本，冲突返回 ErrConcurrentUpdate。
CREATE TABLE IF NOT EXISTS `op_app` (
  `app_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '应用 ID（跨服务只传这个主键）',
  `app_key`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '公开标识（签发后不可变，签名模式入口）',
  `name`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '应用名（同一 owner 下唯一）',
  `description`    VARCHAR(500) NOT NULL DEFAULT '' COMMENT '应用简介',
  `owner_mid`      BIGINT       NOT NULL DEFAULT 0 COMMENT '归属开发者 mid（account 主键引用，不建外键）',
  `status`         TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 待审核、2 正常、3 已停用（违规/风控）、4 驳回、5 已下线（终态）',
  `redirect_uris`  VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '授权回调白名单，逗号分隔（https only，逻辑层校验内网地址）',
  `version`        INT          NOT NULL DEFAULT 1 COMMENT '乐观锁版本（资料与状态变更自增，token 校验侧可见）',
  `register_token` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '注册幂等键（客户端生成，必填且唯一）',
  `last_operator`  BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次状态变更的运营 mid（0 表示未审核）',
  `status_reason`  VARCHAR(255) NOT NULL DEFAULT '' COMMENT '状态变更原因（审计，禁止写密钥或 token）',
  `offline_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '下线时间（Unix 秒，0 表示未下线）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒，列表游标第 1 列）',
  PRIMARY KEY (`app_id`),
  UNIQUE KEY `uniq_app_key` (`app_key`),
  UNIQUE KEY `uniq_owner_name` (`owner_mid`, `name`),
  UNIQUE KEY `uniq_register_token` (`register_token`),
  KEY `idx_owner_mtime` (`owner_mid`, `mtime`, `app_id`),
  KEY `idx_status_mtime` (`status`, `mtime`, `app_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='第三方应用主体表：本表不含任何密钥列，密钥见 op_app_secret';

-- client_secret 哈希表：一行一把密钥，轮换把旧行置历史而不是删除。
-- 与 services/account 的凭证表同形（salt / hash / status 生效位 / ctime / mtime），
-- 差异（README 显式记录）：account 沿用历史 MD5(pwd+pepper+salt) 以兼容 passport 客户端，
-- 本域无历史包袱，使用 HMAC-SHA256(pepper, salt || secret)，pepper 只存在于 Secret/Vault，
-- 既不入库也不入仓库；因此数据库泄露也无法离线还原或伪造密钥。
-- expires_at 表达轮换宽限期：宽限期内旧密钥仍可验签（便于应用平滑切换），到期即失效。
CREATE TABLE IF NOT EXISTS `op_app_secret` (
  `secret_id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '密钥行 ID（主键）',
  `app_id`        BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID（op_app.app_id）',
  `salt`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '随机盐（hex），每把密钥独立生成。禁止出现在任何响应与日志',
  `hash`          CHAR(64)     NOT NULL DEFAULT '' COMMENT 'HMAC-SHA256(pepper, salt||secret) 的 hex。禁止回显',
  `status`        TINYINT      NOT NULL DEFAULT 0 COMMENT '生效位：0 生效、1 历史（与 account_secret 同形态）',
  `expires_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '失效时间（Unix 秒，0 表示按配置长期有效；轮换宽限期用本列表达）',
  `last_used_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次验签通过时间（发现长期不用的密钥）',
  `rotate_reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '轮换/吊销原因（泄露、例行轮换；审计，脱敏）',
  `operator_mid`  BIGINT       NOT NULL DEFAULT 0 COMMENT '触发变更的 mid（owner 或运营）',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '签发时间（Unix 秒）',
  `mtime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`secret_id`),
  KEY `idx_app_status` (`app_id`, `status`, `secret_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='client_secret 哈希表：明文只签发一次，任何接口与日志都不得回显 Salt/Hash';

-- 权限点目录表：逐个 scope 显式声明读/写与风险级别（最小权限的可审计载体）。
-- 主键即 scope 字符串：目录行数在百级，用业务键做主键可让审批表直接引用且无 JOIN 成本。
-- requires_user_consent 对写 scope 强制为 1（model 层 Upsert 会覆写，不给调用方留口子）。
CREATE TABLE IF NOT EXISTS `op_scope` (
  `scope`                 VARCHAR(64)  NOT NULL COMMENT '权限点标识，如 video.publish',
  `display_name`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '授权页展示名',
  `description`           VARCHAR(255) NOT NULL DEFAULT '' COMMENT '授权页说明（用户看得懂的用途描述）',
  `access`                TINYINT      NOT NULL DEFAULT 1 COMMENT '读写声明：1 只读、2 写（0 未声明一律拒绝注册）',
  `risk_level`            TINYINT      NOT NULL DEFAULT 1 COMMENT '风险级别：1 低（可默认勾选）、2 中（需显式确认）、3 高（需运营审批且不可批量授予）',
  `requires_user_consent` TINYINT      NOT NULL DEFAULT 1 COMMENT '是否需要用户逐次同意：0 否、1 是（写 scope 强制为 1）',
  `enabled`               TINYINT      NOT NULL DEFAULT 1 COMMENT '目录内是否开放：0 停用、1 开放',
  `disable_reason`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '停用原因（运营填写，审计）',
  `ctime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`scope`),
  KEY `idx_enabled_access` (`enabled`, `access`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='开放权限点目录表：只有内容/资料/互动类非商业化能力，新增必须先过范围评审';

-- 应用与 scope 的审批关系：授予与回收都是同一行的状态迁移（历史原因留在本行，
-- 问责链路由 op_app.last_operator/status_reason 与后续批次的 audit 服务承担）。
CREATE TABLE IF NOT EXISTS `op_app_scope` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `app_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID（op_app.app_id）',
  `scope`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '权限点标识（op_scope.scope）',
  `state`        TINYINT      NOT NULL DEFAULT 1 COMMENT '审批状态：1 已申请待审、2 已获批、3 已回收（保留行以便审计）',
  `requested_by` BIGINT       NOT NULL DEFAULT 0 COMMENT '申请人 mid（开发者本人）',
  `operator`     BIGINT       NOT NULL DEFAULT 0 COMMENT '审批人 mid（运营，0 表示未审批）',
  `reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '审批/回收原因（审计，脱敏）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最近状态变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_app_scope` (`app_id`, `scope`),
  KEY `idx_app_state` (`app_id`, `state`),
  KEY `idx_scope_state` (`scope`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='应用-scope 审批关系表：唯一键 (app_id,scope) 使授予与回收天然幂等';

-- ---------------------------------------------------------------------
-- 目录 seed：本期开放的 scope（全部为非商业化能力，逐个声明读写与风险）。
-- 幂等：只覆盖目录描述类列，不覆盖 enabled / disable_reason，
--       以免重跑迁移把运营的停用决策恢复成开放。
-- ---------------------------------------------------------------------
INSERT INTO `op_scope`
  (`scope`, `display_name`, `description`, `access`, `risk_level`, `requires_user_consent`, `enabled`, `disable_reason`, `ctime`, `mtime`)
VALUES
  ('profile.read',       '读取我的资料',     '读取本人的昵称、头像、等级等非敏感资料字段',            1, 1, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('profile.write',      '修改我的资料',     '代表本人修改昵称、简介等资料字段',                      2, 3, 1, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('following.read',     '读取我的关注列表', '读取本人关注列表的 mid 与基础公开资料',                 1, 2, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('favorite.read',      '读取我的收藏',     '读取本人收藏夹条目',                                    1, 2, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('favorite.write',     '代我收藏',         '代表本人新增或取消收藏',                                2, 2, 1, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('video.read',         '读取视频公开信息', '读取已发布视频的标题、简介、UP 主与播放次数等公开字段', 1, 1, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('video.stats.read',   '读取我的稿件数据', '读取本人稿件的播放与互动统计（不含他人稿件数据）',      1, 2, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('video.publish',      '代我投稿',         '代表本人上传素材并提交稿件（仍走平台审核与发布状态机）', 2, 3, 1, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('comment.read',       '读取评论',         '读取视频下的公开评论与楼中楼',                          1, 1, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('comment.write',      '代我发评论',       '代表本人发表评论、回复或删除自己的评论',                2, 3, 1, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('danmaku.read',       '读取弹幕',         '读取指定视频的弹幕池',                                  1, 1, 0, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  ('danmaku.write',      '代我发弹幕',       '代表本人在指定视频发送弹幕',                            2, 3, 1, 1, '', UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
ON DUPLICATE KEY UPDATE
  `display_name` = VALUES(`display_name`),
  `description` = VALUES(`description`),
  `access` = VALUES(`access`),
  `risk_level` = VALUES(`risk_level`),
  `requires_user_consent` = VALUES(`requires_user_consent`),
  `mtime` = VALUES(`mtime`);
