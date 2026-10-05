-- =====================================================================
-- ops-config 服务 - 推荐位两张表（坑位定义 / 坑位条目与排期）
-- =====================================================================
-- 数据库：go_video_ops_config。数据所有者：ops-config 服务。
--
-- 表与代码一一对应（列名严格取自 model/*.go 的 db tag 与 SQL 字符串）：
--   * ops_recommend_slot       model/ops_recommend_slot.go       RecommendSlot —— 坑位定义（位置本身）。
--   * ops_recommend_slot_item  model/ops_recommend_slot_item.go  SlotItem      —— 坑位里排的内容与其生效时段。
--
-- 为什么拆两张：坑位是「这块位置存在、给哪些端看、能放几个」，条目是「此刻这里放什么」。
-- 前者的变更频率是季度级、后者是每天级；合成一张表会让「改一个文案」去重写整块位置，
-- 也让容量校验（position <= capacity）没有落点。
--
-- 边界（AGENTS.md §5，与本服务 README 同结论）：
--   1. 坑位是**运营配置**，不是推荐算法。ResolveSlot 只做「过滤 + 截断 + 稳定次序」，
--      不做个性化；weight 只是运营手工次序。真正的召回/排序归 recommend-recall / recommend-rank。
--   2. item_id 只存内容主键引用，不复制标题/封面/播放量；内容是否已下架由**读侧**在取回后
--      向内容服务批量核对（逻辑轮），不在写入时校验 —— 本服务连不上别人的库，
--      写期校验会造出一个假的同步点。
--   3. position 的语义是「运营指定第几格」，不是算法排名。
--
-- 唯一键与幂等：
--   1. uniq_code(code)：code（如 home.banner）是端上的寻址标识，必须唯一；
--      它同时是 SaveSlot 新建的冲突仲裁点（ErrSlotCodeConflict）与
--      ListSlots `ORDER BY code ASC`、ListEnabled `WHERE state = ? ORDER BY code ASC` 的有序来源。
--      下发后改名的后果与专题改 slug 一样严重，因此 model 侧格式固化（slotCodeRe）且带点号白名单。
--   2. uniq_slot_position(slot_id, position)：SaveSlotItems 全量覆盖
--      （事务内 DELETE by slot_id + 多值 INSERT）下，位置重复即撞键。
--      这一键必须存在：同一格两条内容时，端上渲染哪条取决于数据库返回顺序 —— 不可复现的 bug。
--      注意它**不**保证 position<=capacity（那是代码校验，见 model.ReplaceAll），
--      容量是可变的业务约束，不该用 CHECK 或触发器钉死在建表里。
--
-- 索引取自真实查询路径：
--   * idx_state_code(state, code)：ListEnabled 的 `WHERE state = ? ORDER BY code ASC LIMIT ?`
--     —— 缓存重建与后台下拉都走它，必须带 LIMIT（没有上限的「取全部启用坑位」是一个隐蔽的无界读）。
--   * idx_page(page, state, code)：ListSlots 按页面归组巡检（`WHERE page = ? AND state = ?`）。
--     platforms 的 `(platforms = '' OR platforms LIKE ',p,')` 不建索引：
--     四端最多 15 种组合，且坑位总量是几十到几百行，走 LIKE 的成本低于维护一条低选择性索引。
--   * ops_recommend_slot_item 的 ListEffective（`WHERE slot_id = ? AND state = ? AND 窗口 ORDER BY
--     position ASC, weight DESC, id ASC LIMIT ?`）走 uniq_slot_position 的 slot_id 前缀，
--     不另建 (slot_id, state) 索引：单坑位条目数被 capacity 与 MaxSlotItems 双重夹住（百级），
--     回表后 filesort 的成本可忽略；排序里带 id 是为了让 position/weight 全等时结果仍确定。
--   * ops_recommend_slot_item.idx_ref(item_type, item_id, state)：内容下架时反查受影响坑位。
--     专题与坑位都提供这个反查，是因为「内容方要能问谁在引用我」是**唯一的**跨域查询需求，
--     它不需要 JOIN，因此不需要为此建关系表。
--
-- 排期字段说明：start_at/end_at 只描述「这条内容在不在这一格里」，
-- 不含投放时段、频次上限、曝光目标等任何投放语义（AGENTS.md §1、§7 禁区）。
--
-- owner：平台治理（ops-config 服务）；影响范围：新增 2 张表，不改任何既有表。
-- 回滚：
--   DROP TABLE IF EXISTS `ops_recommend_slot_item`;
--   DROP TABLE IF EXISTS `ops_recommend_slot`;
--   与专题同理：坑位与排期没有自动重建来源，回滚前必须整表备份。
-- 锁风险：
--   * 新建空表，不锁既有表。
--   * SaveSlotItems 事务 = 「DELETE 该坑位全部条目 + 一次多值 INSERT（<= 200 行）」，
--     行锁限定在单个 slot_id；不同坑位互不阻塞。**事务内不得做外部 RPC**。
--   * ResolveSlot 是纯读路径，不持锁；它读的是 uniq_slot_position 前缀上的小结果集。
--   * SaveSlot 更新用条件 UPDATE（`WHERE slot_id = ? AND version = ?`）+ RowsAffected，
--     不用 FOR UPDATE：把容量改小的动作与别人正在排内容的动作相撞时，必须有一方明确失败。
-- =====================================================================

-- 坑位定义表：位置本身（哪些端可见、能放几个）。
CREATE TABLE IF NOT EXISTS `ops_recommend_slot` (
  `slot_id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键；条目录挂在它下面',
  `code`        VARCHAR(64)  NOT NULL COMMENT '坑位编码，格式 ^[a-z0-9][a-z0-9_.]{1,63}$（model.slotCodeRe），如 home.banner、detail.below_player。端上按此寻址，故唯一且不可随意改名',
  `page`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '归属页面标识，仅用于后台归组与筛选；服务端不解释页面布局（布局是端与网关的事）',
  `title`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '坑位名，后台展示用',
  `platforms`   VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '生效端列表，存储形态 ",1,2,"（值域来自 proto ClientPlatform 1..4，见 model/platform.go）；**空串表示不限端**。按端筛选时 SQL 必须同时命中「不限端」与「含该端」两支，否则后台会漏掉大部分坑位',
  `capacity`    INT          NOT NULL DEFAULT 20 COMMENT '坑位数量上限：ResolveSlot 未显式给 limit 时按它截断，SaveSlotItems 按它校验 position。没有上限就等于给网关一个无界结果集（默认 model.DefaultSlotCapacity=20，硬上限 model.MaxSlotCapacityHard=200）',
  `state`       TINYINT      NOT NULL DEFAULT 2 COMMENT '1 启用、2 停用。默认停用：新坑位必须先定义再排内容，不能让端上拿到一个空位置。停用坑位在 ResolveSlot 里按「未命中」处理而非报错',
  `version`     BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本（SaveSlotReq.expect_version 比它），每次成功更新 +1',
  `operator_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id（引用 operation，不复制资料）',
  `remark`      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注：这个位置给谁用、什么时候可以被下线',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`slot_id`),
  UNIQUE KEY `uniq_code` (`code`),
  KEY `idx_state_code` (`state`, `code`),
  KEY `idx_page` (`page`, `state`, `code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='推荐位（坑位）定义表：位置、可见端与容量。运营配置，不含推荐算法语义';

-- 坑位条目与排期表：全量覆盖语义，位置唯一且不越界（越界由代码校验）。
CREATE TABLE IF NOT EXISTS `ops_recommend_slot_item` (
  `id`          BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键；也是 position/weight 全等时的最终 tiebreaker',
  `slot_id`     BIGINT      NOT NULL COMMENT '所属坑位（ops_recommend_slot.slot_id）；不建外键，同库同服务写入',
  `position`    INT         NOT NULL COMMENT '第几格，1..capacity。越界由 model.ReplaceAll 拒（ErrSlotPositionOutOfRange）：capacity 是可变的业务约束，不适合钉死在建表里',
  `item_type`   VARCHAR(16) NOT NULL COMMENT '内容类型：ugc_video / pgc_season / pgc_episode / topic。与专题条目表的差别**只有**允许 topic（坑位挂一个专题是正常编排；专题内再套专题不允许）',
  `item_id`     VARCHAR(32) NOT NULL COMMENT '引用内容主键（字符串）。只存引用：不复制标题、不在写期校验存在性（AGENTS.md §5）',
  `weight`      INT         NOT NULL DEFAULT 0 COMMENT '同 position 并列时的次序（大者先）。**不是算法分数**：本服务不做推荐排序，真正的个性化归 recommend-*，见 README 缺口',
  `start_at`    BIGINT      NOT NULL DEFAULT 0 COMMENT '排期生效起（Unix 秒），0 表示不限',
  `end_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '排期生效止（Unix 秒），0 表示不限；非 0 时 end_at<=start_at 即拒 —— 一条永远不生效的排期是排障黑洞。此处仅「展示时段」，不含投放/频次语义（AGENTS.md §7）',
  `state`       TINYINT     NOT NULL DEFAULT 1 COMMENT '1 生效、2 停用。ResolveSlot 只取 1；后台排障走 ListAll 才能看到被过滤的行（回答「为什么没出」）',
  `operator_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id',
  `ctime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）；整批共用同一 ts，排期是成组生效的东西',
  `mtime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_slot_position` (`slot_id`, `position`),
  KEY `idx_ref` (`item_type`, `item_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='坑位条目与排期表（引用内容主键 + 格子位置 + 生效时段；由 SaveSlotItems 在一个事务内全量覆盖）';
