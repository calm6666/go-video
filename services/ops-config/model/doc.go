// Package model 是 ops-config 服务的数据访问层：一个表一个文件，
// 只依赖 sqlx 与本包内的哨兵错误，不 import 任何其它服务的内部包。
//
// 本服务拥有的 8 张表（库名 go_video_ops_config，迁移见 deploy/migrations/ops-config）：
//
//	ops_config_item         配置项（键 + 生效范围 + 当前正式版本指针 + 缓存代次）
//	ops_config_version      不可变版本快照（发布/回滚历史，含 audit 条目引用与 request_id 幂等键）
//	ops_rollout_rule        灰度规则（作用于 config_id + version，收口靠 state 不靠删除）
//	ops_topic               专题/合集（只引用 catalog 的分区与标签 ID）
//	ops_topic_item          专题条目（只引用内容主键）
//	ops_recommend_slot      推荐位定义（坑位编码、容量、生效端）
//	ops_recommend_slot_item 坑位条目与排期
//	ops_client_switch       客户端能力开关（某端从哪个版本起具备某能力）
//
// 派生数据与「可重算的只读投影」（deploy/migrations/README.md 要求逐服务写明）：
//   - ops_config_item.latest_version 与 epoch 是**指针列**，不是独立事实源：
//     事实源是 ops_config_version（不可变快照）。任一时刻
//     latest_version == MAX(version)（不含只走灰度的版本），
//     因此这两列可由本表重算，重算脚本不需要外部输入。
//   - 专题、坑位、开关三类**运行时读缓存全部落在 Redis（CacheRedis），本包不建缓存表**：
//     它们是纯投影，可由 ops_topic/ops_topic_item/ops_recommend_slot/
//     ops_recommend_slot_item/ops_client_switch 整域重建，
//     RefreshCache 就是「丢掉投影」的接口，最坏情况是回源打一次库。
//   - 除上述两处外，本包没有计数表：配置服务的数据量是千级，
//     任何「预计算结果表」都会引入第二个事实源，本期不建。
//
// 边界（AGENTS.md §5，完整结论见 services/ops-config/README.md）：
//   - 分区与标签的事实源是 catalog（catalog_zone / catalog_tag）。本包只在
//     ops_topic.zone_ids / tag_ids 里保存 ID 引用，绝不复制分区名、标签名等可变主资料。
//   - 稿件/作品/季/集只以 item_type + item_id 引用，存在性与上下架状态由
//     video / catalog / rights 判定，本包不建副本。
//   - 管理员与 RBAC 归 operation，本包只保存 operator_id。
//   - 广告投放、出价、排期购买、计费、分成等字段一律不存在（AGENTS.md §1、§7）。
package model
