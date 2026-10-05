package model

// 本文件收录「代码侧硬上限」，它们与建表列宽、契约注释一一对应。
//
// 为什么上限要写在代码里而不是只写在配置里：
//   - 列宽（VARCHAR(n) / TEXT）是**建表时**就定死的，运行期改配置改不了它。
//     如果只有配置有上限，就会出现「配置说能存 5000 字符、列只装得下 512」，
//     最后报的是 MySQL 错误，运营看到的是「保存失败」。
//   - 因此关系固定为：**硬上限在代码，配置只能在它内部收紧**
//     （internal/config/config_load_test.go 有守护用例）。
//   - 改这里的数字必须同时改 deploy/migrations/ops-config 对应文件，二者不可只改一处。

const (
	// MaxCfgValueBytes 是 ops_config_version.cfg_value 的物理上限：列类型为 TEXT，
	// 即 65535 字节。配置 OpsValue.MaxBytes（默认 8192）是落在其中的运营上限——
	// 按**字节**计，因为 TEXT 的限额就是字节，一条中文 JSON 的字符数与字节数不等。
	MaxCfgValueBytes = 65535

	// MaxReasonChars 是 ops_config_version.reason 的列宽上限（VARCHAR(512)，按字符计）。
	// 配置 OpsValue.MaxReasonLen 默认 500，留 12 个字符余量给调用方拼接前后缀；
	// 超长是**拒绝**而不是截断（理由参与审计摘要，截断会让摘要与库值不一致）。
	MaxReasonChars = 512

	// MaxResolveKeys 是 BatchResolveConfig 的单次键数上限（契约注释写的就是 50）。
	// 没有它，一次首页聚合请求就能扇出成一次无界 IN 查询。
	MaxResolveKeys = 50

	// MaxPageSizeHard 是所有列表接口 ps 的契约上限（proto 里 ps 上限 100）。
	// 配置 Query.MaxPageSize 不得超过它，否则「后台一页取出全表」会成为默认行为。
	MaxPageSizeHard = 100

	// MaxTopicKeywordChars / MaxSlotCodeChars 是模糊关键词与编码的入参长度上限：
	// 关键词只用于 LIKE，超过这个长度多半是调用方把整段文本塞进了搜索框。
	MaxTopicKeywordChars = 64
	MaxSlotCodeChars     = 64

	// MaxOperatorNameChars 是 ops_config_version.operator_name 的列宽（VARCHAR(64)）。
	// 它是**发布时刻的展示名快照**，改名不追改，因此宁可拒绝超长输入也不截断：
	// 截断后的名字与真实账号对不上，事后无法归因。
	MaxOperatorNameChars = 64

	// MaxTTLSecondsHard 是任何「建议缓存秒数」的服务端天花板。
	// 与配置 Cache.MaxTTLSeconds 的关系同其它限额：配置只能收紧。
	// 24 小时是刻意的：配置类数据的正确性靠发布/刷新推进，
	// 让一个建议 TTL 超过一天，等于让一次漏删的缓存活过整个运维班次。
	MaxTTLSecondsHard = 86400

	// 下面四项都是「有唯一键/幂等语义的标识列」，列宽即 VARCHAR(32) 或 VARCHAR(64)。
	// 它们必须在上限处拒绝而不是交给 MySQL：这些字段参与唯一键与批量写入，
	// 一条超长行会让整批 ReplaceAll 以 1406 失败，运营看到的是「保存失败」，
	// 排查方向还会被带到「是不是我网络有问题」。
	// 取值与 deploy/migrations/ops-config 的列宽严格相等，由 migration_parity_test 锁住。

	// MaxItemIDChars 引用列 item_id 的字符上限（ops_topic_item / ops_recommend_slot_item）。
	MaxItemIDChars = 32
	// MaxAppVersionChars 版本边界字符串的字符上限（ops_rollout_rule / ops_client_switch）。
	// 32 已足够表达「4 段 × 每段若干位」，超长的版本号不是版本号而是误传。
	MaxAppVersionChars = 32
	// MaxRuleNameChars 灰度规则名的字符上限（uniq_config_version_name 的一列）。
	MaxRuleNameChars = 64
	// MaxRequestIDChars 幂等键 request_id 的字符上限（uniq_request_id）。
	MaxRequestIDChars = 64
)

// exceedsColumn 判定字符串是否超过列宽（VARCHAR(n) 按**字符**计，不是字节）。
// 与 len() 的区别在中文：一个汉字 VARCHAR(32) 装得下 32 个，len() 会算 96 字节。
// 因此按字符的上限一律走这里，按字节的（cfg_value TEXT）才用 len()。
func exceedsColumn(s string, maxChars int) bool {
	n := 0
	for range s {
		n++
		if n > maxChars {
			return true
		}
	}
	return false
}
