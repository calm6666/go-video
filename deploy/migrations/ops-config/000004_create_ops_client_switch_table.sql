-- =====================================================================
-- ops-config 服务 - 客户端开关表
-- =====================================================================
-- 数据库：go_video_ops_config。数据所有者：ops-config 服务。
--
-- 表与代码一一对应（列名严格取自 model/ops_client_switch.go 的 db tag 与 SQL 字符串，
-- 与 ops_client_switch.go 顶部声明的本文件名保持一致）。
--
-- 这张表回答的是一个**布尔问题**：「这一端、这个版本 range 内，这个能力开不开」。
-- 它与 ops_config_item 的分工必须清楚，否则同一件事会有两个地方可以改：
--   * 开关 = 按端 + 版本区间的能力可用性，值只有开/关，不携带任何内容。
--   * 配置项 = 带类型的值（string/int/bool/json），有版本历史、能灰度放量、能回滚。
--   * 因此 platform **必填**（ErrPlatformRequired）：允许「不限端」的开关行，就会出现
--     同一个 key 既有按端行又有全局行，读取时无从判定优先级 —— 想不限端请走 cfg_key。
--   * config_id 是可选关联（0 表示无关联）：把「端有能力」与「配置已放量」串起来，
--     但仍然一行一事实 —— 开关不复制配置值，值只在 ops_config_version。
--
-- 与其他域的边界（AGENTS.md §1、§5、§6、§7）：
--   * 终端只有 Android / iOS / HarmonyOS / 桌面端四端，**不含小程序**。
--   * 本表不存在广告、投放、出价、排期购买、计费、分成、会员相关列。
--     「版本区间」只表达客户端能力，不表达运营排期。
--   * operator_id 是 operation 侧管理员主键的引用，不复制姓名与权限（鉴权不在本服务做）。
--
-- 唯一键与幂等：
--   1. uniq_key_platform(switch_key, platform)：一个开关在一个端上只能有一行。
--      这就是 SaveClientSwitch 的 upsert 句柄，也是 model 侧 FindByKeyPlatform 的查询路径
--      （switch_id=0 时先按 (switch_key, platform) 定位，避免后台点两次生成两条逻辑重复行）。
--   2. version 列是乐观锁：SaveClientSwitchReq.expect_version 比的就是它。
--      更新走条件 UPDATE（`WHERE switch_id = ? AND version = ?`）+ RowsAffected，
--      改 key/platform（即换幂等句柄）前 model 会先探一次
--      `switch_key = ? AND platform = ? AND switch_id <> ?` 把冲突变成 ErrSwitchConflict。
--
-- 索引取自真实查询路径：
--   * idx_platform_key(platform, switch_key)：ListForPlatform 的
--     `WHERE platform = ? ORDER BY switch_key ASC LIMIT ?` —— 端上启动时一次拉全本端开关，
--     索引列序与 WHERE+ORDER BY 一致，不产生 filesort。
--     注意它**不过滤 enabled**：停用行也要能取到（回答「为什么这个开关没生效」），
--     开/关与版本区间由 model.ClientSwitch.Available 在 Go 侧判定，两个实现在同一文件里。
--   * idx_config(config_id)：ListClientSwitches 的 `AND config_id = ?` 过滤，
--     以及配置项下线时反查「还有哪些端挂着它」。
--   * switch_key 的模糊过滤不建索引：model.List 用的是 `%kw%` 双端通配（escapeLike 后前后各加 %），
--     任何 B+Tree 索引都服务不了它，这是刻意的取舍 —— 后台按关键字找开关是低频动作、
--     本表行数是百级，而写路径与运行时读路径已被 uniq_key_platform / idx_platform_key 覆盖。
--     将来若开关上千，改法是把过滤改成前缀匹配（`kw%`）以复用 uniq_key_platform 最左前缀，
--     而不是给 switch_key 建 FULLTEXT。
--
-- owner：平台治理（ops-config 服务）；影响范围：新增 1 张表，不改任何既有表。
-- 回滚：
--   DROP TABLE IF EXISTS `ops_client_switch`;
--   开关行是运营手工配置，无自动重建来源，回滚前整表备份。
-- 锁风险：
--   * 新建空表，不锁既有表，不加外键。
--   * 写路径是单表条件 UPDATE / 单行 INSERT，无跨表事务；批量读走索引前缀。
--   * 没有宽更新：RefreshCache(target=all) 只 bump ops_config_item.epoch，不动本表。
-- =====================================================================

-- 客户端能力开关：按 (switch_key, platform) 唯一定位一个端的可用性。
CREATE TABLE IF NOT EXISTS `ops_client_switch` (
  `switch_id`     BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `switch_key`    VARCHAR(64) NOT NULL COMMENT '开关键，格式 ^[a-z][a-z0-9_.]{1,63}$（model.switchKeyRe），如 vertical_feed。必填 → ErrSwitchKeyRequired',
  `platform`      TINYINT     NOT NULL COMMENT '生效端，值域来自 proto ClientPlatform：1 android、2 ios、3 harmony、4 desktop（Go 侧 int32，转换点集中在 model/platform.go）。**0 禁止入库**：不限端属于配置项 cfg_key，不属于开关表',
  `min_version`   VARCHAR(32) NOT NULL DEFAULT '' COMMENT '具备能力的最小版本（闭区间），空表示不限。按点号逐段比较（model.AppVersionInRange / CompareAppVersion），**不做字符串序**',
  `max_version`   VARCHAR(32) NOT NULL DEFAULT '' COMMENT '具备能力的最大版本（闭区间），空表示不限；与下界倒挂在 normalize 阶段即拒',
  `enabled`       TINYINT     NOT NULL DEFAULT 2 COMMENT '1 开、2 关。默认关：默认开等于给一个还没验证过的端上了新能力',
  `config_id`     BIGINT      NOT NULL DEFAULT 0 COMMENT '可选关联的配置项（ops_config_item.config_id），0 表示无关联。写期由 logic 校验存在性（ErrConfigNotFound）：指向不存在配置的开关，端上会拿到一个永远解析不出的引用',
  `operator_id`   BIGINT      NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id（引用 operation，不复制资料）',
  `remark`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注：为什么这一端要单独开关（半年后唯一还能读懂的东西）',
  `version`       BIGINT      NOT NULL DEFAULT 1 COMMENT '乐观锁版本（expect_version 比它），每次成功更新 +1',
  `ctime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`switch_id`),
  UNIQUE KEY `uniq_key_platform` (`switch_key`, `platform`),
  KEY `idx_platform_key` (`platform`, `switch_key`),
  KEY `idx_config` (`config_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='客户端能力开关表（按端 + 版本区间回答「这个能力开不开」；值语义在 ops_config_item/version）';
