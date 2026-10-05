-- =====================================================================
-- payment 服务 - 资金台账五表（余额 / 充值单 / 支付单 / 退款单 / 资金流水）
-- =====================================================================
-- 用途：落地 services/payment/rpc/payment.proto 的数据模型，是「钱」的唯一写入库。
--
-- 目标库：`go_video_payment`（库名取自 services/payment/etc/payment.v1.yaml 的 DataSource；
--   建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
--
-- 数据所有者：payment 服务（AGENTS.md §5「资金台账（余额/充值/支付/退款流水）→ payment」）。
--   trade-order / membership / creator-revenue 等业务服务只保存本库返回的单据号
--   （payment_no / recharge_no / refund_no），禁止直连本目录任何表、禁止自记余额；
--   硬币余额在 coin 服务，与这里的现金余额是两套账，不互换、不对账折算。
--
-- 沙箱语义（决定本库为什么可以只有这五张表）：
--   1. 唯一渠道是 SANDBOX：OpenRecharge / SettleSandboxRecharge / CreatePayment 只在
--      本库上改数，不请求第三方支付、不产生真实资金移动；
--   2. 「充值到账」「支付成功」是本库的真实状态推进，不是伪造成功；
--   3. 因此本库没有渠道单号、回调报文、对账差异、提现单等表——那些能力需要真实渠道
--      才成立，本项目不开接口（被请求时返回 FailedPrecondition not-configured）。
--
-- 设计口径：
--   1. 金额一律 BIGINT 最小货币单位（人民币＝分），禁止浮点列；币种随行显式记录。
--   2. 单据号与幂等键参与唯一性判定 → 列级 utf8mb4_bin（二进制比较，大小写敏感），
--      避免 *_ci 排序规则把两个不同大小写的 request_id 判成同一个键而误吞重放。
--   3. 余额扣减的正确性由 SQL 守卫保证：
--      UPDATE pm_wallet SET balance_minor = balance_minor - ? WHERE mid = ? AND balance_minor >= ?
--      影响行数 0 即余额不足；「先查后改」在并发下会超扣，服务侧禁止。
--      CHECK 约束是兜底防线：万一有旁路写入把余额写成负数，MySQL 8.0.16+ 直接拒绝。
--   4. pm_flow 是 append-only 台账：只插不改不删，需要纠正时再记一条反向流水
--      （biz_type=4 运营调整），禁止改写历史行；因此本表没有 mtime 列。
--   5. 索引按读侧口径建：(mid, ctime) 供「我的台账」倒序翻页，(state, ctime) 供
--      运营按状态巡检，biz_no / payment_no / biz_order_no 供跨服务按单号回查。
--      跨用户（mid=0）列表在服务侧强制时间窗与分页上限（Payment.MaxListWindowSeconds、
--      Payment.MaxPageSize），不允许无界扫本库。
--
-- 幂等：uniq_request_id 分布在 pm_recharge / pm_payment / pm_refund / pm_flow 上，
--   pm_payment 另有 uniq_biz_order_no（一单一支付）。写入冲突由服务侧回查后按重放返回，
--   不把唯一键冲突当失败上报（见 services/payment/internal/logic 各方法）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `pm_flow`;
--   DROP TABLE IF EXISTS `pm_refund`;
--   DROP TABLE IF EXISTS `pm_payment`;
--   DROP TABLE IF EXISTS `pm_recharge`;
--   DROP TABLE IF EXISTS `pm_wallet`;
--   （资金表不提供反向数据迁移：一旦有台账数据，回滚只能整库回退或前滚修复。）
--
-- 锁风险：全部为 CREATE TABLE IF NOT EXISTS，可重复执行、不改动既有表，无锁风险。
--   后续任何列/索引变更必须新增 0000NN_*.sql，禁止改本文件；
--   ALTER TABLE pm_wallet / pm_payment（大表）需 ALGORITHM=INPLACE, LOCK=NONE 并单独评审。
-- =====================================================================

-- 余额账户：一个用户一行（uniq_mid），现金余额的唯一真值。
CREATE TABLE IF NOT EXISTS `pm_wallet` (
  `id`            BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`           BIGINT      NOT NULL COMMENT '用户 ID（账户唯一键）',
  `balance_minor` BIGINT      NOT NULL DEFAULT 0 COMMENT '可用余额（分），禁止为负',
  `frozen_minor`  BIGINT      NOT NULL DEFAULT 0 COMMENT '冻结余额（分）；本项目无预授权流程，恒为 0',
  `currency`      CHAR(3)     CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种 ISO 4217',
  `version`       BIGINT      NOT NULL DEFAULT 0 COMMENT '余额变更次数，每次条件更新自增',
  `ctime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid` (`mid`),
  CONSTRAINT `chk_pm_wallet_balance_non_negative` CHECK (`balance_minor` >= 0),
  CONSTRAINT `chk_pm_wallet_frozen_non_negative` CHECK (`frozen_minor` >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='余额账户（现金台账唯一写入口）';

-- 充值单：开单只落 PENDING，入账只由 SettleSandboxRecharge 推进（无渠道回调）。
CREATE TABLE IF NOT EXISTS `pm_recharge` (
  `id`              BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `recharge_no`     VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '充值单号（对外唯一标识）',
  `request_id`      VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '开单幂等键（客户端重试必须复用）',
  `mid`             BIGINT      NOT NULL COMMENT '入账用户',
  `amount_minor`    BIGINT      NOT NULL COMMENT '充值金额（分）',
  `currency`        CHAR(3)     CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种 ISO 4217',
  `channel`         TINYINT     NOT NULL DEFAULT 1 COMMENT '渠道：1 SANDBOX（唯一取值）',
  `state`           TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 PENDING、2 SUCCESS、3 CANCELLED、4 FAILED',
  `operator`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '推进到终态的主体：user / 运营工号 / cron',
  `reason`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '取消或人工结算理由（无 PII、无凭据）',
  `settled_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '入账时间（Unix 秒），0 表示未入账',
  `client_trace_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '调用方链路 ID（排障串联用，非判定依据）',
  `ctime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_recharge_no` (`recharge_no`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  CONSTRAINT `chk_pm_recharge_amount_positive` CHECK (`amount_minor` > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='充值单（沙箱台账，无渠道回调）';

-- 支付单：供 trade-order 使用。biz_order_no 唯一 = 一单一支付；
-- 同单号不同金额/币种在服务侧被判成冲突错误，绝不在库里静默改价。
CREATE TABLE IF NOT EXISTS `pm_payment` (
  `id`              BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `payment_no`      VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '支付单号（对外唯一标识）',
  `biz_order_no`    VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '业务订单号（trade-order 主键引用，唯一）',
  `request_id`      VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '受理幂等键',
  `mid`             BIGINT      NOT NULL COMMENT '付款用户',
  `amount_minor`    BIGINT      NOT NULL COMMENT '支付金额（分）',
  `refunded_minor`  BIGINT      NOT NULL DEFAULT 0 COMMENT '累计已退金额（分），恒 <= amount_minor',
  `currency`        CHAR(3)     CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种 ISO 4217',
  `method`          TINYINT     NOT NULL COMMENT '支付方式：1 BALANCE、2 SANDBOX_CHANNEL',
  `state`           TINYINT     NOT NULL COMMENT '状态：1 PENDING、2 PAID、3 FAILED、4 CLOSED、5 REFUNDED、6 PARTIALLY_REFUNDED',
  `subject`         VARCHAR(255) NOT NULL DEFAULT '' COMMENT '摘要（禁止写 PII 与凭据）',
  `operator`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '受理/推进主体：user / 运营工号 / cron',
  `paid_at`         BIGINT      NOT NULL DEFAULT 0 COMMENT '支付成功时间（Unix 秒），0 表示未成功',
  `expire_at`       BIGINT      NOT NULL DEFAULT 0 COMMENT '受理超时（Unix 秒），0 表示不过期',
  `last_request_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '最近一次状态推进请求号（内部审计列，契约不回显，只用于幂等重放判定）',
  `remark`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最近一次状态推进理由（经 PaymentInfo.remark 回显给运营面；禁止 PII 与凭据）',
  `ctime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_payment_no` (`payment_no`),
  UNIQUE KEY `uniq_biz_order_no` (`biz_order_no`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  CONSTRAINT `chk_pm_payment_amount_positive` CHECK (`amount_minor` > 0),
  CONSTRAINT `chk_pm_payment_refunded_range` CHECK (`refunded_minor` >= 0 AND `refunded_minor` <= `amount_minor`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='支付单（一单一支付，余额或沙箱渠道受理）';

-- 退款单：只支持退余额（destination=BALANCE），写入即终态。
CREATE TABLE IF NOT EXISTS `pm_refund` (
  `id`           BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `refund_no`    VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '退款单号（对外唯一标识）',
  `request_id`   VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '退款幂等键',
  `payment_no`   VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '原支付单号（跨表回查用）',
  `biz_order_no` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '原业务订单号（业务服务只持有订单号时也能查）',
  `mid`          BIGINT      NOT NULL COMMENT '退款入账用户',
  `amount_minor` BIGINT      NOT NULL COMMENT '退款金额（分）',
  `currency`     CHAR(3)     CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种 ISO 4217',
  `state`        TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 SUCCEEDED、2 FAILED（沙箱退余额同步落账）',
  `destination`  VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'BALANCE' COMMENT '退款去向：BALANCE；原路退回渠道未配置',
  `operator`     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '发起主体（运营工号 / user / cron）',
  `reason`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '退款理由（必填，禁止 PII 与凭据）',
  `ctime`        BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_refund_no` (`refund_no`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_payment_no` (`payment_no`),
  KEY `idx_biz_order_no` (`biz_order_no`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  CONSTRAINT `chk_pm_refund_amount_positive` CHECK (`amount_minor` > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='退款单（只退余额，写入即终态）';

-- 资金流水：append-only 台账，钱变动的唯一解释链。
-- 无 state 列，故不给 (state, ctime) 索引，改用等价的 (biz_type, ctime) 供运营按类型巡检。
CREATE TABLE IF NOT EXISTS `pm_flow` (
  `flow_id`             BIGINT      NOT NULL AUTO_INCREMENT COMMENT '流水 ID（主键）',
  `mid`                 BIGINT      NOT NULL COMMENT '账户',
  `biz_type`            TINYINT     NOT NULL COMMENT '类型：1 RECHARGE、2 PAYMENT、3 REFUND、4 ADMIN_ADJUST',
  `biz_no`              VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '关联单号：recharge_no / payment_no / refund_no / 调整单号',
  `delta_minor`         BIGINT      NOT NULL COMMENT '变动（分），正入负出，禁止 0',
  `balance_after_minor` BIGINT      NOT NULL DEFAULT 0 COMMENT '本次变动后的余额快照（分），审计与对账用',
  `currency`            CHAR(3)     CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种 ISO 4217',
  `remark`              VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注（禁止 PII 与凭据）',
  `operator`            VARCHAR(64) NOT NULL DEFAULT '' COMMENT '发起主体：user / 运营工号 / cron',
  `request_id`          VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '入账幂等键：同一请求号只能落一条流水',
  `ctime`               BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`flow_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  UNIQUE KEY `uniq_biz_type_biz_no` (`biz_type`, `biz_no`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_biz_type_ctime` (`biz_type`, `ctime`),
  KEY `idx_biz_no` (`biz_no`),
  CONSTRAINT `chk_pm_flow_delta_nonzero` CHECK (`delta_minor` <> 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='资金流水（append-only，只插不改不删）';
