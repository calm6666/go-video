-- 评论主表
-- 对应 obc reply 模块的 reply_subject + reply 的简化合并。
-- rpid 为全局雪花 ID（由 model.Insert 调用方传入或由 comment 序列表生成）。
CREATE TABLE IF NOT EXISTS `comment` (
  `rpid`        BIGINT       NOT NULL COMMENT '评论 ID',
  `oid`         BIGINT       NOT NULL COMMENT '目标 ID（视频 aid 等）',
  `tp`          TINYINT      NOT NULL DEFAULT 1 COMMENT '目标类型：1 视频、4 动态、11 番剧等',
  `root`        BIGINT       NOT NULL DEFAULT 0 COMMENT '根评论 ID（根评论自身为 0）',
  `parent`      BIGINT       NOT NULL DEFAULT 0 COMMENT '父评论 ID',
  `mid`         BIGINT       NOT NULL COMMENT '评论者用户 ID',
  `content`     VARBINARY(9000) NOT NULL COMMENT '评论内容（保留原始字节，审核系统会做过滤）',
  `state`       TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 审核中、2 删除、3 待审、4 屏蔽',
  `floor`       INT          NOT NULL DEFAULT 0 COMMENT '楼层号',
  `like_count`  INT          NOT NULL DEFAULT 0 COMMENT '点赞数快照',
  `reply_count` INT          NOT NULL DEFAULT 0 COMMENT '回复数快照',
  `pinned`      TINYINT      NOT NULL DEFAULT 0 COMMENT '是否被置顶：0 否、1 是',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`rpid`),
  KEY `idx_oid_tp_state` (`oid`, `tp`, `state`),
  KEY `idx_root` (`root`),
  KEY `idx_mid` (`mid`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='评论主表';

-- 评论举报记录
-- 不直接修改评论状态；moderation-orchestrator 通过 trace_id 关联后回调推进状态。
CREATE TABLE IF NOT EXISTS `comment_report` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '举报记录 ID',
  `rpid`          BIGINT       NOT NULL COMMENT '被举报评论 ID',
  `reporter_mid`  BIGINT       NOT NULL COMMENT '举报者用户 ID',
  `reason`        TINYINT      NOT NULL DEFAULT 0 COMMENT '举报理由类型',
  `content`       VARCHAR(500) NOT NULL DEFAULT '' COMMENT '举报附加说明',
  `trace_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `state`         TINYINT      NOT NULL DEFAULT 0 COMMENT '0 待处理、1 已处理',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_rpid` (`rpid`),
  KEY `idx_reporter_mid` (`reporter_mid`),
  KEY `idx_trace_id` (`trace_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='评论举报记录表';
