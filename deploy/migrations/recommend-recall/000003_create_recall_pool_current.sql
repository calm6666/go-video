-- =====================================================================
-- recommend-recall 服务 - 池当前生效版本指针表（控制位，CAS 支点）
-- =====================================================================
-- 用途：建立 recall_pool_current，记录每个池「现在在线生效的是哪个版本」。
--   在线召回（RecallCandidates）只认这张表指向的版本，因此它是发布/回滚的唯一支点。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。
--
-- 数据库：go_video_recommend_recall。
--
-- 事实定位：控制位，不是业务事实。
--   它与 recall_pool_version.state 在同一事务内一起写，二者互为镜像：
--   读路径只认本表的 version，state=CURRENT 那一行是给人排查的冗余视图；
--   任意一边丢失都可由另一边重建（本表按 version>0 与 state 列核对即可），
--   因此不需要事务外备份，但切换必须走 CAS（见下）。
--
-- 版本切换/回滚的并发正确性（model/poolcurrent.go Switch）：
--   1. EnsureRow：INSERT ... ON DUPLICATE KEY UPDATE pool_key = pool_key（显式 no-op）
--      建出 version=0 的指针行，让首次上线也有一条可 CAS 的行；
--      对已存在的行 MySQL 返回 RowsAffected=0，不覆盖现值，因此并发 Ensure 安全。
--   2. Switch：UPDATE ... SET previous_version = version, version = ?, switch_count = switch_count + 1
--      WHERE source = ? AND pool_key = ? AND version = ?(expectVersion)
--      —— 条件更新 + RowsAffected 判定，而不是"先读再无条件写"：
--      · RowsAffected=1：本次真的切换了（switched=true）；
--      · RowsAffected=0 且重读发现指针已等于目标版本：并发对手切到了同一目标，
--        属幂等重放，返回 switched=false 且不重复计数；
--      · RowsAffected=0 且指针指向别的版本：返回 model.ErrSwitchConflict，
--        调用方必须重读后重试，绝不允许"冲突就无条件覆盖"（那会静默抹掉并发的另一次上线）。
--      Switch 要求 session != nil：指针、版本状态、Outbox 事件、幂等标记必须同事务提交。
--
-- 主键设计：PRIMARY KEY (source, pool_key) 而非自增 id。
--   本表每池一行，全部读写（FindOne/EnsureRow/Switch/ListBySources）都按 (source, pool_key) 等值定位，
--   聚簇主键就是唯一访问路径；再加自增 id 只会让每次定位多一跳二级索引回表，
--   并把 CAS 的行锁放到二级索引记录上（更容易与并发切换形成间隙锁死锁）。
--   InnoDB 下不含自增列的主键完全合法，这里也是刻意的。
--
-- 索引：不建任何二级索引，理由写在查询路径上——
--   · FindOne / Switch / EnsureRow：WHERE source = ? AND pool_key = ? -> 主键；
--   · ListBySources：WHERE (source, pool_key) IN (...) LIMIT ? -> 主键（池数上限 MaxPoolRefsPerQuery=200）；
--   · ListCurrent：WHERE version > 0 ORDER BY published_at DESC, source ASC LIMIT ?
--     本表行数等于池数（量级 10^2~10^3，上限 MaxReadyPools=50 下发），
--     全表过滤 + 排序的成本可忽略，为它建 (published_at) 索引反而会为每次切换多维护一棵树；
--     因此刻意不加——若未来池数增长到需要索引，必须同时把 ORDER BY 的第二键方向改成与索引一致。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_pool_current`;
--   本表丢失后可由 recall_pool_version 中 state=CURRENT 的行重建（重建脚本属第二轮运维工具），
--   但在线召回在重建完成前会对所有池报 pool_not_ready，属全量降级，须显式确认。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行；
--   单次切换只锁一行（主键等值 + 条件更新），不存在表级锁窗口。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_pool_current` (
  `source`           SMALLINT     NOT NULL DEFAULT 0 COMMENT '召回路（联合主键之一），取值同 rpc.Source',
  `pool_key`         VARCHAR(128) NOT NULL DEFAULT '' COMMENT '池键（联合主键之一），语法受 model.ValidatePoolKey 约束',
  `version`          BIGINT       NOT NULL DEFAULT 0 COMMENT '当前生效版本；0 表示"建了行但从未上线"，调用方按 pool_not_ready 降级，不得当成空池',
  `batch_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '生效版本的生成批次（与 recall_pool_version.batch_id 一致）',
  `previous_version` BIGINT       NOT NULL DEFAULT 0 COMMENT '上一次生效的版本号（回滚排障：一眼看出"从哪个版本切过来的"）',
  `switch_count`     BIGINT       NOT NULL DEFAULT 0 COMMENT '累计切换次数（只在 CAS 命中时 +1，是发布频率与抖动观测口径）',
  `operator`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次切换的操作者（PublishPoolVersion/RollbackPoolVersion 必填）',
  `note`             VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最近一次切换的原因（审计要求，不含用户敏感信息）',
  `published_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次切换生效时间（Unix 秒）；ListCurrent 排序列、stale 判定基准',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`source`, `pool_key`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='召回池当前生效版本指针表（每池一行；发布/回滚的 CAS 支点，在线读的唯一权威版本来源）';
