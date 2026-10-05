-- =====================================================================
-- creator-revenue 服务 - 创作者分成：规则 / 参与关系 / 计量台账 / 结算单
-- =====================================================================
-- 目标库：`go_video_creator_revenue`（库名取自 services/creator-revenue/etc/creatorrevenue.v1.yaml
--   的 DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
--
-- 数据所有者：creator-revenue 服务（AGENTS.md §5「创作者分成计量与结算台账」）。
-- 其他服务禁止直接读写本目录的表；spm / coin / membership 只持有各自的原始事实，
-- 本服务只保存**按规则折算后的应计台账**，不复制内容主资料，也不反向修改它们的计数。
-- 特别禁止：spm 直接产出收益金额，或任何服务绕过本服务直写 cr_metric。
--
-- 金额语义（务必读三遍）：
--   amount_minor / capped_amount_minor / cr_settlement.amount_minor 全是
--   **应计金额**（分），不是已支付金额。出金（提现、打款、银行卡、发票、税务、
--   对账文件）不在本期范围内：cr_settlement.payout_state 写入即 1（NOT_PAYABLE），
--   且不会有任何代码路径把它改成别的值。任何调用方都不能从本目录的表里
--   得出「钱已出账」的结论。
--
-- 折算与取整口径（与 model.ComputeAmountMinor 逐字一致）：
--   amount = quantity * unit_price_per_1000_minor / 1000，整数除法向下取整，
--   余数（不足 1 分）丢弃且不跨周期追溯补偿；误差方向恒为「少算」，利于平台。
--   quantity < min_quantity 时 capped_amount_minor = 0（防刷门槛）。
--   月度封顶按 (period, mid, source_type) 组内以 aid、metric_id 升序确定性分配，
--   被扣掉的额度记在 cr_settlement.cap_applied_minor，不让运营反推。
--
-- 「同一周期同一作者至多一张在效结算单」的实现方式（重要，偏离朴素 UNIQUE(period,mid)）：
--   cr_settlement 用 UNIQUE KEY (period, mid, void_seq) 达成该约束：
--   在效行 void_seq 恒为 0，所以 (period, mid) 在效行只可能有一张；
--   作废时把该行的 void_seq 改写为自己的 settlement_id（天然唯一），
--   于是 VOIDED 历史行能与新单共存于同一 (period, mid) 下 —— 既保住
--   「(period, mid) 唯一」的幂等语义，又满足「旧单置 VOIDED + 新单另起单号」
--   的审计要求（朴素 UNIQUE(period,mid) 会让强制重算直接撞键）。
--
-- 索引要点：
--   1. cr_metric 的 uniq_metric_key(period, mid, aid, source_type) 既是「重复上报
--      视为更正」的判定依据，也以 (period, mid) 前缀服务出单聚合扫描；
--      另有 idx_period_mid_source(period, mid, source_type) 直接服务封顶重算与分项聚合。
--   2. cr_settlement 的 uniq_active_period_mid(period, mid, void_seq) 与
--      idx_period_state(period, state) 覆盖「按周期批量出单/按周期查台账」，
--      idx_mid_period(mid, period) 覆盖创作者端概览与「最近出单周期」。
--   3. 参与名单按 (state, mid) 索引分页，出单前的 JOIN cr_enrollment 走 uniq_mid。
--
-- 回滚（本文件只建表，回滚即按依赖倒序 DROP，不丢其他服务数据）：
--   DROP TABLE IF EXISTS `cr_settlement_item`;
--   DROP TABLE IF EXISTS `cr_settlement`;
--   DROP TABLE IF EXISTS `cr_metric_change_log`;
--   DROP TABLE IF EXISTS `cr_metric`;
--   DROP TABLE IF EXISTS `cr_enrollment`;
--   DROP TABLE IF EXISTS `cr_rule_change_log`;
--   DROP TABLE IF EXISTS `cr_revenue_rule`;
--
-- 锁风险：全部为 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰已有表，无 ALGORITHM=COPY
--   风险；首次建表只持有 MDL 独占创建锁，毫秒级。后续任何变更必须新增 0000NN_*.sql，
--   禁止修改本文件；给 cr_metric / cr_settlement 这类大表加索引须用
--   ALGORITHM=INPLACE, LOCK=NONE 并避开出单批次时段（GenerateSettlement 会在事务里
--   持行锁，与在线 DDL 抢 MDL）。
--
-- 字符序：表级 utf8mb4_unicode_ci（仓库多数派）；参与唯一性判定的列
--   （rule_code、period、settlement_no、request_id 以及由它们组成的唯一键）
--   一律列级 utf8mb4_bin —— _ci 会折叠大小写，让只差大小写的 rule_code/request_id
--   撞上同一唯一键，表现为「规则串档」「幂等键互相吞」且静默不报错。
-- =====================================================================

-- 分成规则：一类收益怎么折算成金额。rule_code 唯一，一行代表一条规则，
-- 就地更新并以 version 递增；单价/状态的历史值见 cr_rule_change_log。
CREATE TABLE IF NOT EXISTS `cr_revenue_rule` (
  `rule_id`                   BIGINT       NOT NULL AUTO_INCREMENT COMMENT '规则 ID（主键）',
  `rule_code`                 VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '稳定编码，计量台账按它定位规则（唯一，故列级 bin）',
  `source_type`               TINYINT      NOT NULL COMMENT '收益来源：1 会员有效观看、2 投币、3 有效互动、4 运营活动',
  `name`                      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '规则名称（运营可读）',
  `description`               VARCHAR(512) NOT NULL DEFAULT '' COMMENT '规则说明：折算口径、有效量定义',
  `unit_price_per_1000_minor` BIGINT       NOT NULL DEFAULT 0 COMMENT '单价，按每 1000 计量单位计（分）；负数由服务端拒绝',
  `currency`                  VARCHAR(8)   NOT NULL DEFAULT 'CNY' COMMENT '记账币种；台账禁止跨币种混算',
  `unit`                      VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '计量单位：minute / coin / interaction',
  `min_quantity`              BIGINT       NOT NULL DEFAULT 0 COMMENT '防刷门槛：单条 quantity 低于此值不结算',
  `monthly_cap_minor`         BIGINT       NOT NULL DEFAULT 0 COMMENT '(period,mid,source_type) 月度封顶（分），0 表示不限',
  `state`                     TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 DRAFT、2 ACTIVE、3 ARCHIVED（终态）',
  `effective_from`            BIGINT       NOT NULL DEFAULT 0 COMMENT '生效起点（Unix 秒）：周期起点不早于它才可用本规则',
  `version`                   BIGINT       NOT NULL DEFAULT 1 COMMENT '版本号，每次编辑/状态切换递增（台账锁定口径）',
  `created_by`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '创建人（网关渲染的会话身份）',
  `updated_by`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后修改人',
  `ctime`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`rule_id`),
  UNIQUE KEY `uniq_rule_code` (`rule_code`),
  KEY `idx_source_state` (`source_type`, `state`),
  KEY `idx_state` (`state`, `rule_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分成规则表（rule_code 唯一，就地更新 + version 递增）';

-- 规则变更台账：单价与状态的每次变化都留 from/to 对，只追加不修改。
-- 与 cr_revenue_rule 同事务写入；出现结算争议时这里是唯一可复核证据（AGENTS.md §8）。
-- request_id 唯一 = 运营重放同一请求不会二次生效。
CREATE TABLE IF NOT EXISTS `cr_rule_change_log` (
  `log_id`                         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `rule_code`                      VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '被变更的规则编码（参与唯一性判定链，列级 bin）',
  `source_type`                    TINYINT      NOT NULL DEFAULT 0 COMMENT '变更时的收益来源',
  `action`                         VARCHAR(16)  NOT NULL DEFAULT '' COMMENT 'CREATE / UPDATE / ACTIVATE / ARCHIVE / AUTO_ARCHIVE',
  `from_state`                     TINYINT      NOT NULL DEFAULT 0 COMMENT '变更前状态',
  `to_state`                       TINYINT      NOT NULL DEFAULT 0 COMMENT '变更后状态',
  `from_unit_price_per_1000_minor` BIGINT       NOT NULL DEFAULT 0 COMMENT '变更前单价（每 1000 单位，分）',
  `to_unit_price_per_1000_minor`   BIGINT       NOT NULL DEFAULT 0 COMMENT '变更后单价（每 1000 单位，分）',
  `from_version`                   BIGINT       NOT NULL DEFAULT 0 COMMENT '变更前版本号',
  `to_version`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '变更后版本号',
  `operator`                       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '操作人',
  `reason`                         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '变更原因（必填）',
  `request_id`                     VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '幂等键（唯一，列级 bin）',
  `ctime`                          BIGINT       NOT NULL DEFAULT 0 COMMENT '写入时间（Unix 秒）',
  PRIMARY KEY (`log_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_rule_from_version` (`rule_code`, `from_version`),
  KEY `idx_rule_ctime` (`rule_code`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分成规则变更台账（只追加，历史版本唯一复核来源）';

-- 参与关系：一人一行，mid 唯一。SUSPENDED 表示违规暂停，出单判定必须排除。
CREATE TABLE IF NOT EXISTS `cr_enrollment` (
  `enrollment_id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`                 BIGINT       NOT NULL COMMENT '作者用户 ID（唯一；资料主数据在 user-profile/creator，本表只存主键）',
  `state`               TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 ENROLLED、2 LEFT、3 SUSPENDED',
  `agreed_rule_version` BIGINT       NOT NULL DEFAULT 0 COMMENT '参加时确认的规则版本快照，必须可回溯',
  `enrolled_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT '本次参加时间（Unix 秒）',
  `left_at`             BIGINT       NOT NULL DEFAULT 0 COMMENT '退出时间（Unix 秒），0 表示未退出',
  `operator`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后操作人：自助为 user，运营为工号，系统为 cron',
  `remark`              VARCHAR(512) NOT NULL DEFAULT '' COMMENT '备注/原因摘要（退出与暂停必填原因）',
  `ctime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`enrollment_id`),
  UNIQUE KEY `uniq_mid` (`mid`),
  KEY `idx_state_mid` (`state`, `mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='创作者分成参与关系（一人一行，mid 唯一）';

-- 计量台账：一条 = 某周期、某内容、某来源的折算结果。
-- 唯一键 (period, mid, aid, source_type) 让「重复上报」等价于「以本次为准的更正」，
-- 更正前的旧值必须先写 cr_metric_change_log；该周期结算单已 CONFIRMED 时禁止更正。
-- corrected 标出这行被更正过（rpc.RevenueMetricInfo 未暴露该位，属契约缺口，见服务 README）。
CREATE TABLE IF NOT EXISTS `cr_metric` (
  `metric_id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `period`               VARCHAR(6)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '结算周期 YYYYMM（参与唯一键，列级 bin）',
  `mid`                  BIGINT       NOT NULL COMMENT '收益归属作者',
  `aid`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '稿件/内容 ID；0 表示不挂具体内容（活动激励）',
  `source_type`          TINYINT      NOT NULL COMMENT '收益来源，与规则一致',
  `rule_code`            VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '折算所用规则编码（参与唯一性判定链，列级 bin）',
  `rule_version`         BIGINT       NOT NULL DEFAULT 0 COMMENT '计算时锁定的规则版本，重算口径可追溯',
  `quantity`             BIGINT       NOT NULL DEFAULT 0 COMMENT '计量数量（负数由服务端拒绝）',
  `unit`                 VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '计量单位快照（规则改单位不影响历史行）',
  `amount_minor`         BIGINT       NOT NULL DEFAULT 0 COMMENT '按公式算出的应计金额（分），未做门槛/封顶前',
  `capped_amount_minor`  BIGINT       NOT NULL DEFAULT 0 COMMENT '门槛/封顶后的实际应计（分）；被门槛拦掉时为 0',
  `threshold_blocked`    TINYINT      NOT NULL DEFAULT 0 COMMENT '是否被 min_quantity 门槛拦掉：1 表示 capped 恒 0 且不占用月度封顶额度',
  `source_detail`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '计算依据摘要，不含 PII；门槛拦掉时写明「被门槛拦掉」与公式值',
  `corrected`            TINYINT      NOT NULL DEFAULT 0 COMMENT '是否被更正过：0 首次写入、1 已发生更正',
  `ctime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`metric_id`),
  UNIQUE KEY `uniq_metric_key` (`period`, `mid`, `aid`, `source_type`),
  KEY `idx_period_mid_source` (`period`, `mid`, `source_type`),
  KEY `idx_mid_period` (`mid`, `period`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分成计量台账（应计金额，非已支付；唯一键支持更正语义）';

-- 计量更正台账：旧值不静默覆盖，谁改的、依据什么改的都在这里。
-- 与 cr_metric 的更正在同一事务写入；本表只追加。
CREATE TABLE IF NOT EXISTS `cr_metric_change_log` (
  `log_id`                    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `period`                    VARCHAR(6)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '被更正台账的周期 YYYYMM',
  `mid`                       BIGINT       NOT NULL COMMENT '被更正台账的作者',
  `aid`                       BIGINT       NOT NULL DEFAULT 0 COMMENT '被更正台账的内容',
  `source_type`               TINYINT      NOT NULL COMMENT '被更正台账的来源',
  `rule_code`                 VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '本次更正所用规则编码',
  `old_rule_version`          BIGINT       NOT NULL DEFAULT 0 COMMENT '更正前锁定的规则版本',
  `new_rule_version`          BIGINT       NOT NULL DEFAULT 0 COMMENT '更正后锁定的规则版本',
  `old_quantity`              BIGINT       NOT NULL DEFAULT 0 COMMENT '更正前数量',
  `new_quantity`              BIGINT       NOT NULL DEFAULT 0 COMMENT '更正后数量',
  `old_amount_minor`          BIGINT       NOT NULL DEFAULT 0 COMMENT '更正前封顶前应计（分）',
  `new_amount_minor`          BIGINT       NOT NULL DEFAULT 0 COMMENT '更正后封顶前应计（分）',
  `old_capped_amount_minor`   BIGINT       NOT NULL DEFAULT 0 COMMENT '更正前封顶后应计（分）',
  `new_capped_amount_minor`   BIGINT       NOT NULL DEFAULT 0 COMMENT '更正后封顶后应计（分）',
  `operator`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '更正发起方：cron / spm / 运营工号',
  `reason`                    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '更正原因（手工更正必填）',
  `request_id`                VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '本次更正的调用幂等键（可空，仅索引）',
  `ctime`                     BIGINT       NOT NULL DEFAULT 0 COMMENT '写入时间（Unix 秒）',
  PRIMARY KEY (`log_id`),
  KEY `idx_metric_key` (`period`, `mid`, `aid`, `source_type`),
  KEY `idx_request_id` (`request_id`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='计量更正台账（旧值留痕，只追加不修改）';

-- 结算单：一周期一人一单。amount_minor 是应计合计，cap_applied_minor 显式记录
-- 被月度封顶扣掉的额度；payout_state 写入即 1（NOT_PAYABLE）且永不变更。
CREATE TABLE IF NOT EXISTS `cr_settlement` (
  `settlement_id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（也用作 VOIDED 行的 void_seq 槽位值）',
  `settlement_no`     VARCHAR(48)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '结算单号 CRS<period>-<mid>-<rev>（唯一，列级 bin）',
  `period`            VARCHAR(6)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '结算周期 YYYYMM（参与唯一键，列级 bin）',
  `mid`               BIGINT       NOT NULL COMMENT '收益归属作者',
  `amount_minor`      BIGINT       NOT NULL DEFAULT 0 COMMENT '应计合计（分）——不是已支付金额',
  `cap_applied_minor` BIGINT       NOT NULL DEFAULT 0 COMMENT '因月度封顶从应计合计中扣减的额度（分），透明化不留猜',
  `currency`          VARCHAR(8)   NOT NULL DEFAULT 'CNY' COMMENT '记账币种',
  `metric_count`      BIGINT       NOT NULL DEFAULT 0 COMMENT '本单聚合的计量台账行数',
  `state`             TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 DRAFT、2 CONFIRMED（金额冻结）、3 VOIDED',
  `payout_state`      TINYINT      NOT NULL DEFAULT 1 COMMENT '出金状态：恒为 1 NOT_PAYABLE（本项目无出金通道，rpc GetRevenueSummary.payout_available 恒 false；任何取值都不代表钱已出账）',
  `confirmed_by`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '确认人（网关渲染的会话身份）',
  `confirmed_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '确认时间（Unix 秒），0 表示未确认',
  `void_reason`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '作废原因（强制作废已确认单必填）',
  `request_id`        VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '行级幂等键 <request_id>#<period>#<mid>（唯一，列级 bin）',
  `void_seq`          BIGINT       NOT NULL DEFAULT 0 COMMENT '在效行恒 0；作废时改写为 settlement_id，用于 (period,mid) 在效唯一 + 保留作废历史',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`settlement_id`),
  UNIQUE KEY `uniq_settlement_no` (`settlement_no`),
  UNIQUE KEY `uniq_active_period_mid` (`period`, `mid`, `void_seq`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_period_state` (`period`, `state`),
  KEY `idx_mid_period` (`mid`, `period`),
  KEY `idx_mid_state` (`mid`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分成结算单（应计金额，payout_state 恒 NOT_PAYABLE）';

-- 结算分项：同一单内一个来源一行，给运营一眼看懂钱从哪来；明细仍在 cr_metric。
CREATE TABLE IF NOT EXISTS `cr_settlement_item` (
  `item_id`       BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `settlement_no` VARCHAR(48) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '所属结算单号（列级 bin）',
  `source_type`   TINYINT     NOT NULL COMMENT '收益来源',
  `rule_code`     VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '该来源下应计最高的主规则编码（同来源多规则时取主规则）',
  `quantity`      BIGINT      NOT NULL DEFAULT 0 COMMENT '该来源聚合数量',
  `amount_minor`  BIGINT      NOT NULL DEFAULT 0 COMMENT '该来源封顶后应计合计（分）',
  `ctime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`item_id`),
  UNIQUE KEY `uniq_no_source` (`settlement_no`, `source_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='结算单分项（按来源聚合，与结算单同事务写入）';
