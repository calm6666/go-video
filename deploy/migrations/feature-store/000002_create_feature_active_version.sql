-- =====================================================================
-- feature-store 服务 - 对外生效版本指针表（控制位，唯一生效保证）
-- =====================================================================
-- 用途：建立 feature_active_version，记录每个 feature_key「现在对外可读的是哪个版本」。
--   GetFeature / BatchGetFeatures 里 version=0（"当前 ACTIVE 版本"）的解析、
--   SwitchFeatureVersion 的乐观切换、UpdateFeatureState 的下线，全部只认这一行。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--
-- 数据库：go_video_feature_store。
--
-- 「唯一生效」怎么保证（契约要求同一 key 不得有两个 ACTIVE 指针，否则读语义二义）：
--   本表是单行指针表：每个 feature_key 一行，UNIQUE KEY uniq_feature_key (feature_key)
--   就是这条不变量的数据库层保证。不是靠 feature_definition.state=ACTIVE 计数：
--   那需要「先查再改」，两个并发切换能各自看到 0 个 ACTIVE 再各自写一份，
--   不变量只在事务里成立一次；唯一键 + 条件更新才是可依赖的。
--   model.ActiveVersionModel 的三个写方法都要求传事务 session（session=nil 直接
--   ErrTransactionRequired），因为指针变更必须与 feature_version_switch 审计行同生共死。
--
-- 幂等与并发：
--   · Ensure：INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime（显式 no-op）。
--     主键从写入清单里摘掉（不给 AUTO_INCREMENT 列传 0），换个 sql_mode 也不会插入 0 主键；
--     对已存在行 RowsAffected=0，因此并发注册不会把现值冲掉；
--   · Switch：UPDATE ... SET active_version = to,
--       previous_version = IF(expectFrom = 0, previous_version, expectFrom),
--       last_switch_id = ? WHERE feature_key = ? AND active_version = expectFrom
--     —— CAS 而不是「先读再无条件写」。model 已先拒掉 expectFrom == toVersion，
--     因此 active_version 必然改变，RowsAffected 只剩「指针被并发切走」一个含义
--     （与全仓 DSN 不开 clientFoundRows 的约定一致），调用方转 ErrVersionConflict；
--   · LockForUpdate：事务内 SELECT ... FOR UPDATE，把「校验目标版本口径 + 切指针 + 写审计」
--     串成一条临界区。脱离事务的 FOR UPDATE 会立刻放锁，那是假的串行化保证。
--
-- 哨兵值约定：active_version / previous_version 用 0 表示「没有」而不是 NULL。
--   NULL 参与比较永远是 unknown，0 能被 WHERE 直接筛出、也能被 CAS 条件安全比较
--   （model.NoActiveVersion）。previous_version 在首次上线时保持 0，
--   否则 PREVIOUS_VERSION 降级会指向一个从来没生效过的版本。
--
-- 事实定位：控制位，不是业务事实，也不是特征值。
--   可由 feature_version_switch 按 feature_key 倒序重放最后一条版本类审计重建
--   （activate / version_switch / rollback / backfill_auto_switch 都带 to_version），
--   因此本表丢失不等于数据丢失；但重建完成前在线读会全量降级为 DEFAULT_VALUE。
--
-- 索引：
--   · PRIMARY KEY (pointer_id) 只是 InnoDB 的聚簇锚点，所有读写都走 uniq_feature_key；
--   · 不为 active_version 建索引：唯一生效版本数不是本表的问题——按 key 点查才是，
--     「哪些 key 还没有生效版本」是低频运维查询，扫描本表（行数 = 特征 key 数，
--     量级 10^2~10^3）成本可忽略，为它维护一棵树会让每次切换多一跳写放大。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_active_version`;
--   重建：按 feature_key 取 feature_version_switch 中 switch_type IN
--   ('activate','version_switch','rollback','backfill_auto_switch') 的最大 switch_id 行，
--   把 to_version 写回 active_version、该行的 from_version 写回 previous_version、
--   switch_id 写回 last_switch_id（重建脚本属逻辑轮运维工具，本轮不提供）。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据；
--   切换是单行等值更新（uniq_feature_key 定位 + 条件更新），只锁一行，
--   不存在表级锁窗口；FOR UPDATE 的持锁时长受事务内其余语句影响，
--   因此事务里禁止再做批量值扫描（见 README「版本切换」的事务边界约束）。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_active_version` (
  `pointer_id`       BIGINT      NOT NULL AUTO_INCREMENT COMMENT '聚簇主键锚点；业务定位一律走 uniq_feature_key，本列不参与任何谓词',
  `feature_key`      VARCHAR(64) NOT NULL COMMENT '特征键（每 key 一行，唯一键即「同一时刻只有一个生效版本」这条不变量的保证）',
  `active_version`   INT         NOT NULL DEFAULT 0 COMMENT '当前对外生效的版本号；0 = 该 key 还没有生效版本（读侧按 DEFAULT_VALUE 降级，不是返回空值）。刻意不用自增语义，才能被 CAS 条件安全比较',
  `previous_version` INT         NOT NULL DEFAULT 0 COMMENT '上一个生效过的版本号；0 = 从未切换过（首次上线不覆盖本列）。PREVIOUS_VERSION 降级的取值来源',
  `last_switch_id`   BIGINT      NOT NULL DEFAULT 0 COMMENT '最近一次生效切换的 feature_version_switch.switch_id（0 = 尚无切换）。排障：当前版本是怎么来的，一跳就到审计行',
  `ctime`            BIGINT      NOT NULL DEFAULT 0 COMMENT '指针行创建时间（Unix 秒），由 Ensure 幂等建立',
  `mtime`            BIGINT      NOT NULL DEFAULT 0 COMMENT '指针最后变更时间（Unix 秒）；Ensure 命中已有行时自等，故它也是「真的切过」的证据',
  PRIMARY KEY (`pointer_id`),
  UNIQUE KEY `uniq_feature_key` (`feature_key`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='特征对外生效版本指针表（每 key 一行；读路径版本解析与切换/回滚的唯一支点，可由切换审计重建）';
