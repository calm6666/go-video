-- =====================================================================
-- coin 服务 - 硬币账户 + 日额度 + 投币记录 + 硬币流水台账（4 张 cn_* 表）
-- =====================================================================
-- 用途：落地投币域的全部事实。硬币是**社区虚拟币，不是钱**：
--       获得只有两条路（运营发放 GrantCoin、买硬币包由 trade-order 履约调 GrantCoin），
--       消耗只有投币；与 payment 服务的现金余额是两套账，不互换、不共表、不换算，
--       全链路不涉及真实资金（AGENTS.md §1 2026-09-22 修订记录）。
--
-- 数据所有者：coin 服务（AGENTS.md §5「硬币余额与投币记录 → coin」）。
--       其他服务禁止直接读写本目录的表；video/engagement 侧的 coin_count 只是投影，
--       只能由本服务的读接口（GetTargetSummary/BatchGetTargetSummary）供数，
--       别处不得自行扣币或改计数。
--
-- 与 model 的对应：每张表的列顺序与 services/coin/model/cn_*.go 里的
--       accountColumns / dailyTossColumns / tossColumns / flowColumns 逐列一致，
--       改列必须同时改两边（列清单是查询语句的一部分，漏改就是 Unknown column）。
--
-- 记账不变式（对账与排障的唯一依据，建表就必须支撑住）：
--       cn_account.balance == SUM(cn_flow.delta) WHERE mid = 同一 mid
--       因此「新建账户发初始币」也必须落一条 flow_type=4(ADMIN_GRANT)、
--       biz_no='INITIAL_BALANCE'、request_id='coin:init:<mid>' 的流水（model 侧写入），
--       否则余额比流水多出的部分无法解释。
--       cn_account.total_tossed 是历史口径：**取消投币不回退它**（proto 已锁定），
--       回退的只有 balance 与当日额度。
--
-- 幂等依赖的唯一键（本服务不重复扣币的唯一保证）：
--       cn_flow.uniq_request_id (request_id)
--           投币/取消/发放三类写接口全部以「本次是否已留下这条 request_id 的流水」
--           判定首次受理还是重放：重放原样返回首次结论（duplicated=true），
--           参数不同则报冲突，绝不任选一份结论。
--           同 request_id 必然同 mid，而同一 mid 的写又先被 cn_account 行锁串行化，
--           所以「查键 → 扣币 → 写键」这条序列不会有两个事务同时通过。
--       cn_account.PRIMARY (mid)
--           既是账户唯一性，也是 model 的 EnsureTx 抢键点（ON DUPLICATE KEY UPDATE mid=mid
--           返回 affected=0 表示已存在）；投币事务的第一步就是锁这一行。
--       cn_daily_toss.uniq_mid_date (mid, date)
--           一行一天 ⇒ 跨日是天然重置，不存在「自己判断是否跨日再清零」的竞态代码。
--       cn_toss.uniq_mid_target (mid, target_aid)
--           「一人对一内容一条投币聚合」的事实约束，也是 coin_count 聚合的分母依据。
--
-- 排序规则（collation）——本文件最容易埋雷的地方，逐列说明：
--       表级 utf8mb4_unicode_ci：面向人阅读/比对的文本列（operator、remark、trace_id）
--       需要与全仓其它库一致的大小写与重音折叠语义。
--       但**参与唯一性判定或精确匹配鉴别的列一律列级 COLLATE utf8mb4_bin**：
--         - cn_flow.request_id          唯一键。utf8mb4_unicode_ci 会把 'ABC' 与 'abc'
--                                       视为同一个键，重放判定就会张冠李戴：
--                                       两个仅大小写不同的请求被误判为重复，
--                                       其中一次真实扣币被静默丢弃（丢单）。
--         - cn_toss.last_request_id     与 cn_flow.request_id 同义同源，必须同一套比较语义。
--         - cn_flow.biz_no              订单号/工单号，对账按等值精确匹配；
--                                       折叠会让两笔订单落到同一条查询结果里。
--       整数列（mid、target_aid、date）不参与 collation，唯一性由值本身决定。
--
-- 索引依据（只建 model 实际走到的访问路径）：
--       cn_account          PRIMARY(mid)                       所有写路径的行锁入口
--                           （无二级索引：本表不做任何按余额/时间的检索，
--                             运营侧的批量核对走 cn_flow）
--       cn_daily_toss       uniq_mid_date(mid, date)           AccumulateTx/RollbackTx/FindOne
--                           idx_date(date)                     按日聚合运营报表与冷数据清理
--       cn_toss             uniq_mid_target(mid, target_aid)   FindOne/LockByTargetTx/我的记录定位
--                           idx_mid_state_last(mid, state, last_tossed_at)
--                                                              ListMyTosses：WHERE mid (+state)
--                                                              ORDER BY last_tossed_at DESC, id DESC
--                           idx_target_state(target_aid, state) GetTargetSummary/Batch 的
--                                                              GROUP BY 聚合与 ListActiveByTarget
--       cn_flow             PRIMARY(id)                        LastInsertId
--                           uniq_request_id(request_id)        幂等锚点
--                           idx_mid_ctime_id(mid, ctime, id)   ListCoinFlows：WHERE mid
--                                                              ORDER BY ctime DESC, id DESC
--                           idx_ctime(ctime)                   跨用户（mid=0）时间窗台账
--                           idx_biz_no(biz_no)                 按订单号对账
--                           idx_target_type(mid, target_aid, flow_type)
--                                                              FindLatestByTarget（取消重放回捞 flow_id）
--       刻意不给 cn_toss 加 (target_aid, last_tossed_at) 之类的组合排序索引：
--       投币人列表按人数规模（每片最多 PerTargetLimit 枚 × 投币人数）本就有限，
--       idx_target_state 前缀已够用，多加索引只会拖慢每一次投币写入。
--
-- 容量与保留：cn_flow 是 append-only 台账，随投币量线性增长，且**不允许物理删除**
--       （AGENTS.md §8 要求删除/下架保留审计证据）。冷热分层由 services/cron 规划：
--       超保留窗口的行迁移到归档库，本表只提供 INSERT/SELECT，不提供 DELETE 接口。
--       cn_daily_toss 每天每活跃用户一行，可按 date 归档（额度回看窗口远小于台账）。
--
-- 锁风险：
--       1. 本文件只有 CREATE TABLE IF NOT EXISTS，可在线重复执行，无 ALTER、无回填。
--       2. 运行期锁行为：投币事务按 cn_account(行锁) → cn_toss(行锁) →
--          cn_daily_toss(条件更新) → cn_flow(插入) 的**固定顺序**取锁，
--          顺序写反（例如先锁 cn_toss 再锁 cn_account）会在并发投币下死锁。
--          同一用户的并发投币被账户行锁串行化，持锁时间 = 单个事务长度（毫秒级，
--          4 条语句，事务内禁止任何外部 RPC / MQ 调用）。
--          不同用户之间互不阻塞（PK 不同行），热点只可能出现在超高并发同一用户，
--          那是客户端重试风暴的信号，不是正常业务形态。
--       3. cn_flow.uniq_request_id 的插入会在唯一索引上加插入意向锁：
--          同一 request_id 的并发重试互相阻塞到对方提交，这是**正确行为**
--          （正是它挡住重复扣币），但因此必须把流水插入放在事务最后一步。
--       4. 条件更新依赖驱动默认 affected=changed rows 语义
--          （DSN 不带 clientFoundRows=true，见 services/coin/etc/coin.v1.yaml）。
--          若在 DSN 上加了 clientFoundRows=true，RowsAffected 会返回「匹配行数」，
--          幂等与余额不足的判定会全部失真 —— 变更该 DSN 参数属于本服务的破坏性变更。
--
-- 回滚：
--   DROP TABLE IF EXISTS `cn_flow`;
--   DROP TABLE IF EXISTS `cn_toss`;
--   DROP TABLE IF EXISTS `cn_daily_toss`;
--   DROP TABLE IF EXISTS `cn_account`;
--   注意：这是**有损回滚**（余额、投币记录、台账全部消失，且没有任何地方能重建它们 ——
--   硬币不是钱，没有支付渠道对账单可以回补）。回滚前必须先下线本服务的写接口，
--   并把 video/engagement 侧已投影的 coin_count 一并清零或冻结，
--   否则会留下一批「永远对不上账」的展示计数。
-- =====================================================================

-- 硬币账户：余额的唯一真值。mid 直接沿用 account 域用户主键，不建外键跨库。
CREATE TABLE IF NOT EXISTS `cn_account` (
  `mid`          BIGINT   NOT NULL COMMENT '用户 ID（主键，不自增：由 account 域签发，本服务只引用不自造）',
  `balance`      BIGINT   NOT NULL DEFAULT 0 COMMENT '当前可用硬币，恒 >= 0；扣减一律走条件 UPDATE ... WHERE balance >= ?',
  `total_tossed` BIGINT   NOT NULL DEFAULT 0 COMMENT '历史累计投出枚数（历史口径：取消投币**不回退**本列，只回退 balance）',
  `version`      BIGINT   NOT NULL DEFAULT 0 COMMENT '余额变更次数（每次 balance 变化 +1）；读侧与排障用，写门禁由 WHERE 条件承担',
  `ctime`        BIGINT   NOT NULL DEFAULT 0 COMMENT '建仓时间（Unix 秒）',
  `mtime`        BIGINT   NOT NULL DEFAULT 0 COMMENT '最后一次余额变更时间（Unix 秒）',
  PRIMARY KEY (`mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='硬币账户（余额唯一真值；与 payment 现金余额分账，不可互换）';

-- 日额度：一行一天，(mid, date) 唯一。date 用 YYYYMMDD 整数，跨日天然重置。
CREATE TABLE IF NOT EXISTS `cn_daily_toss` (
  `id`     BIGINT  NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`    BIGINT  NOT NULL COMMENT '用户 ID',
  `date`   INT     NOT NULL COMMENT '日桶编号 YYYYMMDD（服务本地时区自然日，与 DSN loc=Local 同源）；非保留字，SQL 里仍统一加反引号',
  `tossed` INT     NOT NULL DEFAULT 0 COMMENT '当日已投枚数；累加走 UPDATE ... WHERE tossed + ? <= DailyLimit，回退用 GREATEST(tossed - ?, 0) 夹底',
  `ctime`  BIGINT  NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`  BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_date` (`mid`, `date`),
  KEY `idx_date` (`date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='单用户单日投币额度（一天一行，跨日天然重置，不做手工清零）';

-- 投币记录：一人对一内容一条聚合，是 coin_count 的事实来源（不另建汇总表）。
CREATE TABLE IF NOT EXISTS `cn_toss` (
  `id`               BIGINT      NOT NULL AUTO_INCREMENT COMMENT '投币记录 ID（rpc.TossInfo.toss_id）',
  `mid`              BIGINT      NOT NULL COMMENT '投币人用户 ID',
  `target_aid`       BIGINT      NOT NULL COMMENT '被投稿件 aid：只存引用，不建外键、不校验存在性（跨服务越权，见 services/coin/README 已知缺口）',
  `count`            INT         NOT NULL DEFAULT 0 COMMENT '当前生效投币枚数（state=2 时保留历史值作为退款依据）。count 是 MySQL 函数名，SQL 中必须写反引号',
  `state`            TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 ACTIVE、2 CANCELLED（取消为软状态，保留行作审计证据，不做物理删除）',
  `first_tossed_at`  BIGINT      NOT NULL DEFAULT 0 COMMENT '该行首次投币时间（Unix 秒，取消后重投不回拨）',
  `last_tossed_at`   BIGINT      NOT NULL DEFAULT 0 COMMENT '最后一次投币时间（Unix 秒）：取消窗口的起算点',
  `cancelled_at`     BIGINT      NOT NULL DEFAULT 0 COMMENT '取消时间（Unix 秒），0 表示未取消',
  `last_request_id`  VARCHAR(64) COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '最后一次「投币」请求的幂等键；取消不改本列（取消的幂等锚点在 cn_flow.request_id）。列级 utf8mb4_bin：与 cn_flow.request_id 保持同一套逐字节比较语义',
  `platform`         TINYINT     NOT NULL DEFAULT 0 COMMENT '客户端平台，取值同 rpc.Platform：0 未指明、1 Android、2 iOS、3 HarmonyOS、4 桌面、5 Web',
  `trace_id`         VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID（不含明文 IP/设备号）',
  `last_toss_date`   INT         NOT NULL DEFAULT 0 COMMENT '最后一次投币落的日桶编号 YYYYMMDD；取消投币按它回退 cn_daily_toss.tossed',
  `ctime`            BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`            BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_target` (`mid`, `target_aid`),
  KEY `idx_mid_state_last` (`mid`, `state`, `last_tossed_at`),
  KEY `idx_target_state` (`target_aid`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='投币记录（一人对一内容一行；coin_count = SUM(`count`) WHERE state=1，不建投影表）';

-- 硬币流水台账：append-only，只插入不更新不删除，兼作全部写接口的幂等锚点。
CREATE TABLE IF NOT EXISTS `cn_flow` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '流水 ID（rpc.CoinFlowInfo.flow_id）',
  `mid`            BIGINT       NOT NULL COMMENT '归属用户 ID',
  `flow_type`      TINYINT      NOT NULL COMMENT '流水类型，取值同 rpc.CoinFlowType：1 投币(负)、2 取消退回(正)、3 硬币包履约(正)、4 运营发放/扣回(可正负)、5 过期(未开启，恒不写入)',
  `delta`          BIGINT       NOT NULL COMMENT '变动枚数，正入负出；不允许为 0（0 额流水无记账意义且会骗过对账）',
  `balance_after`  BIGINT       NOT NULL COMMENT '本笔落库后的余额快照：重放时据此回显，不必再从别处猜',
  `target_aid`     BIGINT       NOT NULL DEFAULT 0 COMMENT '投币/取消类流水的目标稿件 aid，发放类为 0',
  `biz_no`         VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL DEFAULT '' COMMENT '订单号（硬币包履约）/运营工单号/INITIAL_BALANCE。列级 utf8mb4_bin：对账按等值精确匹配，折叠大小写会把两笔订单混成一条',
  `operator`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '操作者：user（自助）/trade-order（履约）/运营工号/system（建仓初始币）/cron',
  `request_id`     VARCHAR(64)  COLLATE utf8mb4_bin NOT NULL COMMENT '幂等键（唯一索引）。列级 utf8mb4_bin 是硬要求：unicode_ci 会把仅大小写不同的两个键判为同一个，导致一次真实扣币被当成重放静默丢弃',
  `remark`         VARCHAR(255) NOT NULL DEFAULT '' COMMENT '摘要（例如运营填写的原因文本），不得含 PII 与凭据',
  `trace_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '落库时间（Unix 秒）；台账分页与时间窗查询的唯一排序列',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime_id` (`mid`, `ctime`, `id`),
  KEY `idx_ctime` (`ctime`),
  KEY `idx_biz_no` (`biz_no`),
  KEY `idx_target_type` (`mid`, `target_aid`, `flow_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='硬币流水台账（append-only；balance 恒等于本表 delta 之和，不允许 DELETE）';
