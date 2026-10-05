-- =====================================================================
-- trade-order 服务 - 订单主表 to_order + 状态流转台账 to_order_event
-- =====================================================================
-- 用途：商业订单（会员单 / 硬币包单）的事实表与状态机台账。
--   订单状态机、金额快照、履约指令与履约结果只在这里落地；
--   资金台账在 payment（go_video_payment）、会员身份与授予台账在 membership
--   （go_video_membership）、硬币余额与流水在 coin（go_video_coin）。
--
-- 目标库：`go_video_trade_order`（库名取自 services/trade-order/etc/tradeorder.v1.yaml 的 DataSource；
--   建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
--
-- 数据所有者：trade-order 服务（AGENTS.md §5）。
--   payment / membership / coin 一律不得回写本目录任何表：它们只在被本服务同步 RPC
--   调用时写自己的库，本服务是订单状态与履约指令的唯一写入口。
--   其他服务读订单结论只能走 tradeorder.v1.rpc，禁止直连本库。
--
-- 金额与幂等口径：
--   1. amount_minor / unit_price_minor 恒为服务端向 membership.GetPlan 重算的快照，
--      客户端上报金额只做一致性校验，不作为扣款依据（防前端改价）；
--   2. uniq_request_id(请求体里的建单幂等键) 是「一次建单最多一张订单」的最终防线；
--      order_no 由 common/idgen（ULID）+ 可读前缀 `to_` 生成，
--      uniq_order_no 兜住跨实例碰撞；
--   3. payment_no 上有普通索引而不是唯一索引：未支付订单该列为 ''（DEFAULT ''），
--      一行订单对应一笔支付单的事实由「payment.biz_order_no 唯一 + 本服务只在 CAS 成功时回填」保证，
--      把 '' 纳入唯一索引会让所有未支付订单互相冲突。
--   4. 履约与退款的下游幂等键由 order_no 派生（`grant_<order_no>` 等），
--      因此本表不需要额外的重试表。
--
-- 索引设计：
--   idx_mid_state_created  (mid, state, created_at)  我的订单分页（终端主读路径）；
--   idx_state_updated      (state, updated_at)       cron 卡单扫描（ListStuckOrders 按 updated_at 超时）；
--   idx_payment_no         (payment_no)              按支付单号反查订单（运营排障 / 沙箱台账核对）；
--   idx_state_created      (state, created_at)       运营面跨用户窗口扫描（无界时间窗在 logic 层就被拒绝）；
--   uniq_order_no 上已是主读路径（GetOrder），不再另建 order_no 普通索引。
--
-- 锁风险：
--   仅 CREATE TABLE IF NOT EXISTS，首次执行建空表、重复执行为 no-op，不锁已有业务表；
--   无 ALTER/DROP，不触碰其他服务 schema；后续变更必须新增 0000NN_*.sql，禁止修改本文件。
--   注意：本表是资金相关热点表，任何后续 ALTER 都应走 pt-online-schema-change/gh-ost 之类
--   的在线改表流程，避免长事务在订单更新高峰期持锁。
--
-- 回滚（本服务无历史数据依赖，先子表后主表）：
--   DROP TABLE IF EXISTS `to_order_event`;
--   DROP TABLE IF EXISTS `to_order`;
-- =====================================================================

CREATE TABLE IF NOT EXISTS `to_order` (
  `id`                BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键（对外标识用 order_no）',
  `order_no`          VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '订单号：to_ + ULID，全局唯一，参与唯一性判定故列级 bin',
  `request_id`        VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '建单幂等键，参与唯一性判定故列级 bin',
  `mid`               BIGINT        NOT NULL COMMENT '下单用户（游客不可下单，恒为正）',
  `biz_type`          TINYINT       NOT NULL COMMENT '业务类型：1 会员单、2 硬币包',
  `plan_id`           BIGINT        NOT NULL DEFAULT 0 COMMENT '套餐/SKU 主键（membership 持有，本表只存引用，不建外键）',
  `plan_code`         VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '套餐稳定编码快照（精确匹配，不做大小写折叠）',
  `title`             VARCHAR(200)  NOT NULL DEFAULT '' COMMENT '下单时商品名快照（改价改名不影响历史单）',
  `quantity`          INT           NOT NULL DEFAULT 1 COMMENT '份数',
  `duration_days`     INT           NOT NULL DEFAULT 0 COMMENT '会员单本次总时长 = 套餐 duration_days × unit_count × 份数',
  `coin_amount`       INT           NOT NULL DEFAULT 0 COMMENT '硬币包本次发放枚数 = 套餐约定枚数 × 份数',
  `unit_price_minor`  BIGINT        NOT NULL DEFAULT 0 COMMENT '服务端重算的单价快照（分）',
  `amount_minor`      BIGINT        NOT NULL DEFAULT 0 COMMENT '应付=实付总额（分），服务端重算值',
  `refunded_minor`    BIGINT        NOT NULL DEFAULT 0 COMMENT '已退金额（分），退款成功时累加；退款只回沙箱余额、不到银行卡（AGENTS.md §1 范围外）',
  `currency`          VARCHAR(8)    CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'CNY' COMMENT '币种（ISO 4217，金额不用浮点）',
  `pay_method`        TINYINT       NOT NULL COMMENT '支付方式：1 余额、2 沙箱渠道；两者都不产生真实资金移动',
  `state`             TINYINT       NOT NULL COMMENT '订单状态：1 CREATED、2 PAYING、3 PAID、4 FULFILLING、5 FULFILLED、6 CANCELLED、7 FAILED、8 REFUND_REQUESTED、9 REFUND_APPROVED、10 REFUNDED、11 REFUND_REJECTED',
  `fulfill_state`     TINYINT       NOT NULL DEFAULT 1 COMMENT '履约结果：1 PENDING、2 SUCCEEDED、3 FAILED（与 state 分开：一个是走到哪步，一个是该给的给到没有）',
  `fulfill_attempts`  INT           NOT NULL DEFAULT 0 COMMENT '履约尝试次数，达 FulfillMaxAttempts 后拒绝再试',
  `fulfill_detail`    VARCHAR(500)  NOT NULL DEFAULT '' COMMENT '最近一次失败摘要（不写堆栈、不写 PII、不写凭据；成功路径清空）',
  `payment_no`        VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT 'payment 支付单号引用（只存引用、不建外键）；列级 bin 是因为精确匹配查单，索引只有普通 KEY：未支付单恒为空串，进唯一索引会互相冲突（见头部口径 3）',
  `grant_ref`         VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '履约产物引用：会员 grant_id 或硬币 flow_id',
  `expire_at`         BIGINT        NOT NULL DEFAULT 0 COMMENT '未支付关单时间（Unix 秒），同时透传给 payment',
  `platform`          TINYINT       NOT NULL DEFAULT 0 COMMENT '下单端：1 Android、2 iOS、3 HarmonyOS、4 桌面、5 Web',
  `client_trace_id`   VARCHAR(64)   CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '客户端链路 ID（排障用，不作为幂等依据）',
  `version`           BIGINT        NOT NULL DEFAULT 1 COMMENT '乐观锁位点：状态推进 WHERE state=? AND version=? 的 CAS 条件',
  `created_at`        BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `updated_at`        BIGINT        NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒），卡单扫描依据',
  `paid_at`           BIGINT        NOT NULL DEFAULT 0 COMMENT '支付结论绑定时间（0 表示未支付）',
  `fulfilled_at`      BIGINT        NOT NULL DEFAULT 0 COMMENT '履约完成时间（0 表示未履约）',
  `closed_at`         BIGINT        NOT NULL DEFAULT 0 COMMENT '关单时间（取消/退款终态）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_order_no` (`order_no`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_state_created` (`mid`, `state`, `created_at`),
  KEY `idx_state_updated` (`state`, `updated_at`),
  KEY `idx_state_created` (`state`, `created_at`),
  KEY `idx_payment_no` (`payment_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='商业订单主表（订单事实与履约指令的唯一所有者；payment/membership 不得回写）';

-- 状态流转台账：每次状态迁移一行，与 to_order 的主表更新同事务提交。
-- 这张表是回答「这单为什么变成 PAID、谁推的、理由是什么」的唯一出处，
-- 也是 RejectRefund 反查「申请前原状态」的依据，因此禁止 UPDATE/DELETE。
-- 回滚：DROP TABLE IF EXISTS `to_order_event`;
CREATE TABLE IF NOT EXISTS `to_order_event` (
  `event_id`   BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `order_no`   VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT '订单号引用（跨服务/跨表只存主键，不建外键）',
  `from_state` TINYINT      NOT NULL DEFAULT 0 COMMENT '迁移前状态；0 表示建单行（此前不存在状态）',
  `to_state`   TINYINT      NOT NULL COMMENT '迁移后状态，取值同 to_order.state',
  `operator`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '操作者："user" / 运营工号 / "cron" / "system" / "trade-order"',
  `reason`     VARCHAR(500) NOT NULL DEFAULT '' COMMENT '迁移理由或结论摘要（不含 PII 与凭据）',
  `request_id` VARCHAR(64)  CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '触发本次迁移的幂等键（同一次请求可产生多行，故不建唯一索引）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '记录时间（Unix 秒）',
  PRIMARY KEY (`event_id`),
  KEY `idx_order_ctime` (`order_no`, `ctime`),
  KEY `idx_order_request` (`order_no`, `request_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='订单状态流转台账（与主表更新同事务；只追加，不更新不删除）';
