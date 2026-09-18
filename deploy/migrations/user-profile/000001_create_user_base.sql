-- =====================================================================
-- user-profile 服务 - 用户基础资料表迁移
-- =====================================================================
-- 用途：记录用户可变展示资料（昵称/性别/头像/签名/排名/生日）。
-- 数据所有者：user-profile 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC Base/Bases/Member/Members 与 HTTP /member/* 接口访问。
-- 参考映射：移植自参考仓库 member 服务 user_base_%02d（按 mid%100 分 100 张表）。
--   本项目起步阶段数据量有限采用单表设计；如需分表，按 mid%100 命名
--   user_base_00..99 并在 model 层切换表名，字段与索引定义不变。
-- 回滚：DROP TABLE IF EXISTS user_base;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_base`
(
    -- mid 用户 ID，主键，由 account 服务（idgen）分配
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，由 account/idgen 分配，全局唯一）',

    -- name 昵称；SetName 时同步触发 updateUname 事件通知 account 失效缓存
    `name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '用户昵称（最长 64 字符）',

    -- sex 性别：0 保密、1 男、2 女；对外经 SexStr 转换为中文展示
    `sex` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '性别：0 保密、1 男、2 女',

    -- face 头像 URL；空表示未设置，查询时替换为默认头像（URLNoFace）
    `face` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '头像 URL（空表示未设置，读取时替换默认头像）',

    -- sign 个人签名
    `sign` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '个人签名',

    -- rank 排名，默认 5000；经验值操作要求 rank>=10000（UserNoMember 校验）
    `rank` BIGINT UNSIGNED NOT NULL DEFAULT 5000 COMMENT '用户排名（默认 5000；rank>=10000 才允许经验值操作）',

    -- birthday 生日 Unix 秒，-28800 表示未设置（默认值 DefaultTime）
    `birthday` BIGINT NOT NULL DEFAULT -28800 COMMENT '生日（Unix 秒，-28800 表示未设置）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户基础资料表：昵称、性别、头像、签名、排名、生日。user-profile 持有，禁止跨服务直接读写。';
