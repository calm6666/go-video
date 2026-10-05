-- =====================================================================
-- membership 服务 - 会员域全部表（套餐 / 变更台账 / 权益码 / 会员身份 / 授予台账 / 写请求幂等）
-- =====================================================================
-- 用途：落地 AGENTS.md §1（2026-09-22 修订后纳入的商业化范围）中的「会员」域，
--       为 services/membership 的 16 个 RPC 提供持久化。
--
-- 目标库：`go_video_membership`（库名取自 services/membership/etc/membership.v1.yaml 的 DataSource；
--   建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
--
-- 数据所有者：membership 服务（AGENTS.md §5）。
--   其他服务（trade-order / payment / playback / gateway / account）禁止直连本目录的表，
--   只能通过 membership.v1.rpc 调用；特别是 account 名片里的 vip 字段只是投影，
--   事实源是这里的 mb_membership + mb_grant。
--
-- 资金语义（务必别误读）：本服务不持有资金。
--   mb_plan.price_minor/prom_price_minor 只是**标价**（单位：分，整数），
--   实际收钱由 trade-order 建单、payment 走沙箱台账受理，履约时才调 GrantMembership。
--   因此 mb_grant 是「权益是否生效」的唯一事实源：没有未过期的授予结果行，
--   CheckEntitlement 必须判定未开通，不存在默认放行。
--
-- 索引与查询口径：
--   1. mb_membership uniq_mid_vip_type(mid, vip_type)：一个用户一个档位一行，
--      同时是「按 mid 读我的会员」的前缀索引（GetMembership / CheckEntitlement）。
--   2. 到期扫描走 idx_expire_at(expire_at) 与 idx_auto_renew_expire_at(auto_renew, expire_at)，
--      对应 ListExpiringMemberships 的闭区间扫描。
--      本表**没有** state 列：会员状态是 expire_at 与服务端 now 的比较结果（proto 注释口径），
--      若再存一份 state 就会与时间不一致（没人写就没有过期推进），因此不给 (state, expire_at)。
--      唯一的例外是 cron 的 ExpireMembership：它只写台账与快照，不改判口径。
--   3. mb_grant idx_mid_ctime(mid, ctime)：台账分页按 (mid, ctime) 倒序；
--      idx_mid_vip_ctime 支撑「某档位该用户的变更史」，idx_biz_order_no 支撑退款反查。
--   4. 唯一性判定列一律列级 COLLATE utf8mb4_bin：request_id / plan_code / code /
--      action / change_type / api / subject / params_fingerprint / biz_order_no / payment_no /
--      auto_renew_channel / currency。这些值在 Go 侧是逐字节比较的标识符，若被
--      utf8mb4_unicode_ci 折叠大小写，两个仅大小写不同的 request_id 会被判为同一个请求，
--      幂等语义静默失效（表现为「重复加时长」或「冲突检不出来」）。
--      展示类文本（name/description/reason/operator）保留表级 utf8mb4_unicode_ci 以便搜索。
--
-- 回滚（按依赖逆序，本服务表之间无外键，任何顺序都可）：
--   DROP TABLE IF EXISTS `mb_biz_request`;
--   DROP TABLE IF EXISTS `mb_grant`;
--   DROP TABLE IF EXISTS `mb_membership`;
--   DROP TABLE IF EXISTS `mb_entitlement`;
--   DROP TABLE IF EXISTS `mb_plan_change_log`;
--   DROP TABLE IF EXISTS `mb_plan`;
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，首次执行建空表，重复执行只产生 note 不改动数据，
--   不锁既有表、不回填历史行，可在业务运行期安全执行。
--   后续任何列/索引变更必须新增 0000NN_*.sql（ALTER TABLE 会重建索引并短暂无损，
--   大表需评估 gh-ost/pt-osc），禁止修改本文件。
-- =====================================================================

-- 套餐（SKU）目录。state 见 model.PlanState*，档位见 model.VipType*。
-- 改价不走上下架接口：必须新建 DRAFT 版本再切换，避免对已下单用户追溯生效。
CREATE TABLE IF NOT EXISTS `mb_plan` (
  `plan_id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '套餐 ID（主键）',
  `plan_code`            VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL COMMENT '对外稳定编码（唯一，下单用它而不是 plan_id）',
  `name`                 VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '套餐展示名',
  `description`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '套餐描述',
  `vip_type`             TINYINT      NOT NULL COMMENT '档位：1 大会员、2 超级大会员',
  `duration_days`        INT          NOT NULL COMMENT '单个售卖单位时长（天）：月卡 31、季卡 93、年卡 366',
  `unit_count`           INT          NOT NULL DEFAULT 1 COMMENT '一次购买包含几个 duration_days',
  `price_minor`          BIGINT       NOT NULL DEFAULT 0 COMMENT '原价标价（最小货币单位：分），整数不用浮点',
  `prom_price_minor`     BIGINT       NOT NULL DEFAULT 0 COMMENT '促销价标价（分），0 表示无促销',
  `currency`             CHAR(3)      COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种代码，本轮固定 CNY；逐字节比较',
  `platform_mask`        INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '可见平台位掩码：Android 1、iOS 2、Harmony 4、Desktop 8、Web 16',
  `auto_renew_supported` TINYINT      NOT NULL DEFAULT 0 COMMENT '是否支持签约自动续费：0 否、1 是',
  `state`                TINYINT      NOT NULL DEFAULT 1 COMMENT '售卖状态：1 DRAFT、2 ON_SALE、3 OFF_SALE',
  `version`              BIGINT       NOT NULL DEFAULT 1 COMMENT 'CAS 版本号，SetPlanState/UpsertPlan 的 expected_version 比这个',
  `created_by`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '创建者（网关按会话渲染的运营身份）',
  `updated_by`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后修改者',
  `ctime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`plan_id`),
  UNIQUE KEY `uniq_plan_code` (`plan_code`),
  KEY `idx_state_vip_type` (`state`, `vip_type`),
  KEY `idx_state_plan_id` (`state`, `plan_id`),
  KEY `idx_vip_type` (`vip_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='会员套餐目录（标价，不含资金状态）';

-- 套餐变更台账：上下架与改价都要能回溯到人、理由和前后值（AGENTS.md §8 审计证据）。
-- request_id 唯一索引同时充当 UpsertPlan/SetPlanState 的幂等键；
-- params_fingerprint 是「同 request_id 换参数」的判定依据，冲突必须报错而不是静默生效。
CREATE TABLE IF NOT EXISTS `mb_plan_change_log` (
  `log_id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `plan_id`               BIGINT       NOT NULL COMMENT '套餐 ID（跨表只存主键，不建外键）',
  `change_type`           VARCHAR(16)  COLLATE utf8mb4_bin NOT NULL COMMENT '变更类型：UPSERT 内容/价格、STATE 上下架',
  `from_state`            TINYINT      NOT NULL DEFAULT 0 COMMENT '变更前套餐售卖状态：0 无前态（首次新建）、1 DRAFT、2 ON_SALE、3 OFF_SALE',
  `to_state`              TINYINT      NOT NULL DEFAULT 0 COMMENT '变更后套餐售卖状态：0 未改变、1 DRAFT、2 ON_SALE、3 OFF_SALE',
  `from_price_minor`      BIGINT       NOT NULL DEFAULT 0 COMMENT '变更前原价（分）',
  `to_price_minor`        BIGINT       NOT NULL DEFAULT 0 COMMENT '变更后原价（分）',
  `from_prom_price_minor` BIGINT       NOT NULL DEFAULT 0 COMMENT '变更前促销价（分）',
  `to_prom_price_minor`   BIGINT       NOT NULL DEFAULT 0 COMMENT '变更后促销价（分）',
  `operator`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '操作者（网关按会话渲染的运营身份；用户自助为 "user"）',
  `reason`                VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更理由（禁止写入凭据与 PII）',
  `request_id`            VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL COMMENT '幂等键（唯一）',
  `params_fingerprint`    CHAR(64)     COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '关键参数 sha256 hex，重放时比对',
  `ctime`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`log_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_plan_ctime` (`plan_id`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='套餐上下架/改价变更台账';

-- 权益码目录：全站唯一会员权益口径的载体。
-- 未知码不得放行（判定为 CODE_UNKNOWN），下线码判定为 CODE_DISABLED，
-- 因此 enabled=0 的行必须保留而不是删掉——删了就退化成「未知」，运营无法区分。
CREATE TABLE IF NOT EXISTS `mb_entitlement` (
  `entitlement_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `code`           VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL COMMENT '权益码（唯一，例如 vip.high_bitrate），逐字节比较',
  `name`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '权益展示名',
  `description`    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '权益描述',
  `min_vip_type`   TINYINT      NOT NULL COMMENT '达标档位：1 大会员、2 超级大会员（更高档自动满足）',
  `enabled`        TINYINT      NOT NULL DEFAULT 1 COMMENT '是否生效：1 生效、0 下线',
  `version`        BIGINT       NOT NULL DEFAULT 1 COMMENT 'CAS 版本号',
  `updated_by`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后修改者',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`entitlement_id`),
  UNIQUE KEY `uniq_code` (`code`),
  KEY `idx_enabled_min_vip` (`enabled`, `min_vip_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='会员权益码目录（判定口径事实源）';

-- 用户会员身份：一个 (mid, vip_type) 一行，是 mb_grant 的当前态投影。
-- expire_at 是唯一到期口径：<= 服务端 now 即已过期；没有这行就是从未开通。
CREATE TABLE IF NOT EXISTS `mb_membership` (
  `membership_id`        BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`                  BIGINT      NOT NULL COMMENT '用户 ID',
  `vip_type`             TINYINT     NOT NULL COMMENT '档位：1 大会员、2 超级大会员',
  `start_at`             BIGINT      NOT NULL DEFAULT 0 COMMENT '首次开通时间（Unix 秒），只写一次',
  `expire_at`            BIGINT      NOT NULL DEFAULT 0 COMMENT '到期时间（Unix 秒），<= now 即已过期',
  `auto_renew`           TINYINT     NOT NULL DEFAULT 0 COMMENT '自动续费签约位：0 未签约、1 已签约（沙箱只记意愿，不建真实代扣协议）',
  `auto_renew_channel`   VARCHAR(32) COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '签约渠道标识，未签约为空；逐字节比较避免 SANDBOX/sandbox 混写',
  `auto_renew_signed_at` BIGINT      NOT NULL DEFAULT 0 COMMENT '最近一次签约/解约时间（Unix 秒）',
  `source`               TINYINT     NOT NULL DEFAULT 0 COMMENT '最近一次变更来源：0 尚无变更、1 沙箱订单履约、2 沙箱自动续费、3 运营手工、4 体验会员、5 存量迁移（见 rpc.GrantSource）',
  `paid_month_count`     INT         NOT NULL DEFAULT 0 COMMENT '累计付费月数快照，单调不减（运营赠送不计入）',
  `version`              BIGINT      NOT NULL DEFAULT 1 COMMENT 'CAS 版本号，写接口按它做乐观锁',
  `ctime`                BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`membership_id`),
  UNIQUE KEY `uniq_mid_vip_type` (`mid`, `vip_type`),
  KEY `idx_expire_at` (`expire_at`),
  KEY `idx_auto_renew_expire_at` (`auto_renew`, `expire_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户会员身份当前态（(mid,vip_type) 唯一）';

-- 授予台账：权益生效的唯一凭据。CheckEntitlement 读的就是这张表投影出的未过期身份行，
-- 因此它是审计与判定的双重事实源，禁止 DELETE（撤销要写 action=REVOKE 的行）。
CREATE TABLE IF NOT EXISTS `mb_grant` (
  `grant_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`              BIGINT       NOT NULL COMMENT '用户 ID',
  `vip_type`         TINYINT      NOT NULL COMMENT '档位：1 大会员、2 超级大会员',
  `action`           VARCHAR(16)  COLLATE utf8mb4_bin NOT NULL COMMENT '动作：GRANT/EXTEND/REVOKE/EXPIRE（契约钉死，新增即改契约）',
  `delta_days`       INT          NOT NULL DEFAULT 0 COMMENT '本次影响天数，带符号：授予为正、收回为负',
  `plan_id`          BIGINT       NOT NULL DEFAULT 0 COMMENT '关联套餐，0 表示无（运营手工/迁移）',
  `source`           TINYINT      NOT NULL DEFAULT 0 COMMENT '来源：0 未设置、1 沙箱订单履约、2 沙箱自动续费、3 运营手工、4 体验会员、5 存量迁移（见 rpc.GrantSource）',
  `biz_order_no`     VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT 'trade-order 订单号引用（只存主键，不建外键、不回查其库）',
  `payment_no`       VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT 'payment 资金流水号引用（同上）',
  `before_expire_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '变更前到期时间（Unix 秒），首次开通为 0',
  `after_expire_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '变更后到期时间（Unix 秒），立即失效为 <= now',
  `operator`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '运营工号/服务身份；用户自助为 "user"，cron 为 "cron"',
  `request_id`       VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL COMMENT '幂等键（唯一），命中重放必须回查首次结果',
  `reason`           VARCHAR(255) NOT NULL DEFAULT '' COMMENT '台账摘要（禁止写入凭据与 PII）',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`grant_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_mid_vip_ctime` (`mid`, `vip_type`, `ctime`),
  KEY `idx_biz_order_no` (`biz_order_no`),
  KEY `idx_source_ctime` (`source`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='会员授予/变更台账（权益生效的唯一凭据，只增不删）';

-- 写请求幂等台账：只服务「不动时长、因此不属于 mb_grant 四种 action」的写用例
-- （SetAutoRenew / UpsertEntitlement）。主键就是幂等键，同 mb_grant.request_id 的语义。
-- 保留期由 services/cron 按 ctime 清理（窗口必须长于上游重试窗口，见 README）。
CREATE TABLE IF NOT EXISTS `mb_biz_request` (
  `request_id`         VARCHAR(64) COLLATE utf8mb4_bin NOT NULL COMMENT '幂等键（主键），逐字节比较',
  `api`                VARCHAR(48) COLLATE utf8mb4_bin NOT NULL COMMENT '用例名：SetAutoRenew / UpsertEntitlement',
  `subject`            VARCHAR(96) COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '作用对象标识："mid:<mid>:vip:<n>" 或权益码',
  `mid`                BIGINT      NOT NULL DEFAULT 0 COMMENT '涉及用户（无则 0）',
  `vip_type`           TINYINT     NOT NULL DEFAULT 0 COMMENT '涉及档位：0 不涉及档位、1 大会员、2 超级大会员',
  `result_id`          BIGINT      NOT NULL DEFAULT 0 COMMENT '首次生效的结果主键（entitlement_id 等），0 表示无',
  `params_fingerprint` CHAR(64)    COLLATE utf8mb4_bin NOT NULL COMMENT '关键参数 sha256 hex：同 request_id 不同参数一律报冲突',
  `operator`           VARCHAR(64) NOT NULL DEFAULT '' COMMENT '操作者；用户自助为 "user"',
  `ctime`              BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒），cron 按它清理',
  PRIMARY KEY (`request_id`),
  KEY `idx_api_ctime` (`api`, `ctime`),
  KEY `idx_mid` (`mid`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='会员写请求幂等台账（不动时长的写用例）';
