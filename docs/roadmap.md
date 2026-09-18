# 开发路线图

## 阶段 0：骨架与契约

交付：go-zero 服务模板、网关健康检查、配置样例、错误码、trace、事件 envelope、数据库迁移规范、Compose 基础依赖、CI 基础检查。

验收：所有占位服务有职责 README；`go test ./...`、格式化和文档检查可在空实现仓库执行；没有商业化或小程序接口。

## 阶段 1：账号、UGC 投稿与播放闭环

交付：identity、content、media、playback、moderation、operation 的合并部署单元；账号登录、创作者资料、分片上传、转码、审核、稿件发布、播放签名和基础点赞/收藏。

验收：稿件状态机完整；失败可重试；播放不会暴露长期对象存储地址；所有发布事件可追踪。

## 阶段 2：社区和搜索

交付：comment、danmaku、social-graph、feed、notification、search-query；关注、动态、评论、弹幕、通知、搜索和历史记录。

验收：高频写入幂等；弹幕与评论隔离；黑名单可见性正确；搜索最终一致且可重建索引。

## 阶段 3：直播和版权目录

交付：live-room、live-ingest、live-media、live-gateway；catalog、rights；直播开关播、录制回放、作品/季/集、版权排期和自动下架。

验收：断流可恢复；回放重新经过审核；权利到期不会继续返回播放地址。

## 阶段 4：推荐和 SPM

交付：event-collector、spm、recommend-recall、recommend-rank、feature-store；行为采集、推荐特征、召回、排序、冷启动和降级。

验收：SPM 事件脱敏、可重放；推荐不可用时回退热门/关注流；不包含广告分析和商业化字段。

## 阶段 5：开放平台和规模化

交付：open-platform、服务独立扩缩容、索引/数据归档、跨可用区恢复、容量压测和客户端版本治理。

验收：OAuth 应用可撤销和限额；关键服务有 SLO、恢复目标和压测报告。

阶段之间不以“目录创建完成”为完成标准，而以 API、事件、权限、数据迁移、测试和运行指标全部具备为准。
