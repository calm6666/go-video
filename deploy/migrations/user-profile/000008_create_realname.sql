-- =====================================================================
-- user-profile 服务 - 实名认证三表迁移（实名信息/申请单/证件照）
-- =====================================================================
-- 用途：
--   realname_info       用户实名认证信息（证件号以 RSA 密文入库）
--   realname_apply      实名认证申请单（含证件照图片 ID）
--   realname_apply_img  实名申请证件照（对象存储 token 路径）
-- 数据所有者：user-profile 服务。隐私合规：证件号仅以密文 + MD5 哈希落库，
--   明文只在缓存中短暂存在；/privacy 等接口必须按查看者权限裁剪。
-- 参考映射：移植自参考仓库 member 服务 realname_info / realname_apply /
--   realname_apply_img 三张表；参考仓库支付宝渠道（realname_alipay_apply）
--   不在本项目范围（见服务 README 移植边界）。
-- 回滚：DROP TABLE IF EXISTS realname_apply_img;
--        DROP TABLE IF EXISTS realname_apply;
--        DROP TABLE IF EXISTS realname_info;
-- 锁风险：仅建表；uk_mid/uk_card_md5/idx_card_md5 空库建索引无锁风险。
-- =====================================================================

-- 实名认证信息（每用户一条，随申请/审核更新）
CREATE TABLE IF NOT EXISTS `realname_info`
(
    -- id 自增主键
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',

    -- mid 用户 ID（唯一）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（唯一，关联 user_base.mid）',

    -- channel 实名渠道：0 主站、1 支付宝（支付宝渠道暂不启用）
    `channel` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '实名渠道：0 主站、1 支付宝（暂不启用）',

    -- realname 真实姓名
    `realname` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '真实姓名',

    -- country 国家：0 中国
    `country` SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '国家：0 中国',

    -- card_type 证件类型：0 身份证
    `card_type` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '证件类型：0 身份证',

    -- card 证件号 RSA 公钥加密后的 base64 密文（RSA-2048 PKCS1v15）
    `card` VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '证件号 RSA 公钥加密后的 base64 密文（明文不落库）',

    -- card_md5 证件号哈希：MD5(salt + 小写证件号 + 证件类型 + 国家)，用于查重与反查
    `card_md5` CHAR(32) NOT NULL DEFAULT '' COMMENT '证件号哈希（MD5(salt+小写证件号+类型+国家)），用于查重与反查',

    -- status 流程状态：0 审核中、1 通过、2 驳回、3 未申请
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 3 COMMENT '实名流程状态：0 审核中、1 通过、2 驳回、3 未申请',

    -- reason 驳回原因（status=2 时回填）
    `reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '驳回原因（status=2 时回填）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_mid` (`mid`),
    -- 查重与反查索引（status in (0,1) 过滤）
    UNIQUE KEY `uk_card_md5` (`card_md5`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务实名认证信息表：证件号仅存 RSA 密文与 MD5 哈希，明文不落库。';

-- 实名认证申请单（多次申请保留历史，查询取最新一条）
CREATE TABLE IF NOT EXISTS `realname_apply`
(
    -- id 自增主键
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',

    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 realname_info.mid）',

    -- realname 真实姓名
    `realname` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '真实姓名',

    -- country 国家：0 中国
    `country` SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '国家：0 中国',

    -- card_type 证件类型：0 身份证
    `card_type` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '证件类型：0 身份证',

    -- card_num 证件号 RSA 密文（同 realname_info.card）
    `card_num` VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '证件号 RSA 公钥加密后的 base64 密文',

    -- card_md5 证件号哈希（同 realname_info.card_md5）
    `card_md5` CHAR(32) NOT NULL DEFAULT '' COMMENT '证件号哈希（MD5(salt+小写证件号+类型+国家)）',

    -- hand_img 手持证件照图片 ID（关联 realname_apply_img.id，0 表示未提交）
    `hand_img` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '手持证件照图片 ID（关联 realname_apply_img.id，0 表示未提交）',

    -- front_img 证件正面照图片 ID
    `front_img` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '证件正面照图片 ID（关联 realname_apply_img.id）',

    -- back_img 证件背面照图片 ID
    `back_img` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '证件背面照图片 ID（关联 realname_apply_img.id）',

    -- status 流程状态：0 审核中、1 通过、2 驳回、3 未申请
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '实名流程状态：0 审核中、1 通过、2 驳回、3 未申请',

    -- operator 审核操作人
    `operator` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '审核操作人',

    -- operator_id 审核操作人 ID
    `operator_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审核操作人 ID',

    -- operator_time 审核时间（Unix 秒）
    `operator_time` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审核时间（Unix 秒）',

    -- remark 审核备注
    `remark` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '审核备注',

    -- remark_status 备注状态（保留字段，参考仓库语义）
    `remark_status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '备注状态（保留字段，参考仓库语义）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 查询最新申请单：按 mid + id 倒序
    KEY `idx_mid` (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务实名认证申请单表：多次申请保留历史，审核状态与证件照引用。';

-- 实名申请证件照（IMGData 为对象存储 token 路径）
CREATE TABLE IF NOT EXISTS `realname_apply_img`
(
    -- id 自增主键，被 realname_apply.hand_img/front_img/back_img 引用
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（被 realname_apply 的证件照字段引用）',

    -- img_data 图片 token 路径（/idenfiles/<token>.txt），展示时拼接 CDN URL 模板
    `img_data` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '图片 token 路径（/idenfiles/<token>.txt，展示时拼接 CDN URL 模板）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务实名申请证件照表：对象存储 token 路径引用。';
