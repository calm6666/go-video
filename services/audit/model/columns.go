package model

import (
	"regexp"
	"strings"
)

// 本文件固化 audit 五张表的**列宽事实**与**列级格式规则**（AGENTS.md §4/§5）。
//
// 为什么常量要待在 model 而不是散在 logic 里：
//   - 校验上限必须与 DDL 列宽对齐。校验放过 512 字节而列只有 VARCHAR(128) 时，
//     表现不是「报个难懂的错」而是写入期 1406 Data too long，
//     而审计写入失败等于证据缺失；
//   - migration_parity_test.go 直接拿这里的常量与 deploy/migrations/audit/*.sql 的
//     列宽逐列比对，改列宽不改常量（或反之）会立刻测失败；
//   - logic 与 repository 共用同一份上限，避免两边各写一套兜底值互相漂移。
//
// 命名约定：Max<列名>Bytes = 该列允许写入的最大字节数，且必须 <= SQL 列宽。

// audit_entry（000001_create_audit_entry_and_chain_tables.sql）
const (
	MaxEventIDBytes      = 128 // VARCHAR(128)
	MaxChainKeyBytes     = 64  // VARCHAR(64)
	MaxActorNameBytes    = 128 // VARCHAR(128)，不参与哈希，允许截断
	MaxActionBytes       = 64  // VARCHAR(64)
	MaxActionDomainBytes = 32  // VARCHAR(32)
	MaxTargetTypeBytes   = 64  // VARCHAR(64)
	MaxTargetIDBytes     = 64  // VARCHAR(64)
	MaxDigestBytes       = 255 // VARCHAR(255)（白名单格式下最长 64 或 16 的倍数 + 分隔符）
	MaxReasonBytes       = 512 // VARCHAR(512)
	MaxTraceIDBytes      = 64  // VARCHAR(64)
	MaxRequestIDBytes    = 64  // VARCHAR(64)
	MaxCallerNameBytes   = 32  // VARCHAR(32)
	// ShortHashBytes 是 ip_hash / device_hash 的 CHAR(32) 宽度（= ShortHashLen）。
	ShortHashBytes = ShortHashLen
	// FullHashBytes 是 prev_hash / entry_hash / manifest_hash / last_entry_hash 的 CHAR(64)。
	FullHashBytes = 64
)

// audit_export_task（000002_create_audit_export_task_table.sql）
const (
	MaxFilterJSONBytes = 2048 // VARCHAR(2048)
	MaxFormatBytes     = 16   // VARCHAR(16)
	MaxBucketBytes     = 64   // bucket VARCHAR(64)
	MaxObjectKeyBytes  = 255  // object_key VARCHAR(255)
	MaxErrMsgBytes     = 512  // VARCHAR(512)
)

// audit_retention_policy / audit_archive_batch（000003）
const (
	MaxRemarkBytes    = 255 // audit_retention_policy.remark VARCHAR(255)
	MaxStateNameBytes = 16  // state VARCHAR(16)
)

// HashHexLen 是 sha256hex 的字符数（摘要类列宽的唯一来源）。
const HashHexLen = 64

// MaxScanBatchRows 是导出游标扫描的单批硬上限。
// 配置 Export.BatchRows 配得再大也会被夹到这里：一批全进内存，
// 上限即导出进程的 RSS 上限，不能靠调用方参数决定。
const MaxScanBatchRows = 50000

// MaxFutureOccurredSkewSeconds 是 occurred_at 允许超前服务端时钟的容忍秒数。
// 超过即拒绝写入：未来时间戳会把条目推进一条「今天」的归档与校验作业永远扫不到的链，
// 相当于把证据放进没人看守的抽屉（AGENTS.md §9 宁可拒写也不留隐患）。
const MaxFutureOccurredSkewSeconds = 300

var (
	// 动作域：小写字母/数字/下划线，<= 32。它直接拼进 chain_key 与保留策略唯一键，
	// 所以字符集必须比 action 更严（不允许点号，避免 chain_key 出现歧义分隔）。
	actionDomainRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	// 动作标识：<对象>.<动作>，允许小写字母/数字/下划线/点/中划线/冒号。
	actionRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.:-]{0,63}$`)
	// 链标识：<action_domain>/<yyyy-MM-dd>。日期段固定 10 字符，与 model.DateKey 一致。
	chainKeyRe = regexp.MustCompile(`^[a-z0-9_]{1,32}/[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	// 摘要里的字段名（ValidDigest 的分号列表左侧），与 hashchain.go 的白名单同源。
	digestFieldNameRe = regexp.MustCompile(`^[A-Za-z0-9_.]{1,64}$`)
)

// ValidActionDomain 判定动作域是否可作为链名与策略键。
func ValidActionDomain(s string) bool { return actionDomainRe.MatchString(s) }

// ValidAction 判定动作标识是否形如 <对象>.<动作>。
// 强制至少一个点号：审计检索与保留策略都按「对象前缀」聚合，
// 没有点号的 action 会让「谁改了什么」无法按对象归并。
func ValidAction(s string) bool {
	return actionRe.MatchString(s) && strings.Contains(s, ".")
}

// ValidChainKey 判定链标识是否由 ChainKey 生成（防止校验/归档接口拼出错位链名）。
func ValidChainKey(s string) bool { return chainKeyRe.MatchString(s) }

// DomainOfChainKey 取链标识里的动作域（归档时据此查保留策略）。
// 非法链名返回空串，由调用方判错，不做「猜一个域」的降级。
func DomainOfChainKey(chainKey string) string {
	if !ValidChainKey(chainKey) {
		return ""
	}
	i := strings.LastIndex(chainKey, "/")
	return chainKey[:i]
}

// ValidDigestFieldName 判定自审计摘要的字段名是否可写入 digest 列。
func ValidDigestFieldName(s string) bool { return digestFieldNameRe.MatchString(s) }

// TruncateRunes 按 rune 截断，绝不切出半个多字节字符：
// 中文昵称/UA 被按字节截断会产生非法 UTF-8，日志与导出文件都会坏掉。
// 只允许用于**不参与哈希**的展示列（actor_name / user_agent），其余字段超长一律拒写。
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}
