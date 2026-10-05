package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// 本文件固化 audit 的完整性自证算法。**任何改动都会让已入库条目的 entry_hash
// 无法复算**，因此只允许在 schema_version 递增并新开链时修改，禁止就地调整。
// 算法说明同步写在 services/audit/rpc/audit.proto 文件头与 services/audit/README.md。

// fieldSep 是哈希序列化用的字段分隔符：0x1F（单元分隔符）。
// 选它是因为业务字段是脱敏后的可见字符，正常文本里不会出现裸控制字符，
// 因此不需要再做转义就能保证「字段拼接唯一 → 序列化唯一」。
const fieldSep = "\x1f"

// genesisSeed 是链头 prev_hash 的种子前缀，实际值再混入 chain_key，
// 使不同链的创世摘要不同，避免把 A 链条目整段搬到 B 链还能自洽。
const genesisSeed = "go-video/audit/v1/genesis/"

// GenesisHash 返回指定链的链头 prev_hash：sha256hex(genesisSeed + chainKey)。
func GenesisHash(chainKey string) string {
	return sha256Hex([]byte(genesisSeed + chainKey))
}

// ChainKey 由动作域与业务发生时间推出所属哈希链："<action_domain>/<UTC 自然日>"。
// 按「域 + 日」分链的理由：
//   - 单条全局链会把所有写入串行化在同一行的 SELECT ... FOR UPDATE 上，吞吐被一行锁限死；
//   - 按天分段后，校验与归档都能按链并行，且历史链一旦封口就再也不变化；
//   - 域隔离让「运营配置」的存证不被审核洪峰淹没，某域异常不影响其它域继续写入。
func ChainKey(actionDomain string, occurredAt int64) string {
	return actionDomain + "/" + DateKey(occurredAt)
}

// serializeV1 按 rpc/audit.proto 声明的固定顺序拼接参与哈希的字段，
// 以 prev_hash 结尾。整数用十进制（无符号、无前导零），字符串原样（已脱敏）。
//
// 参与哈希的 19 个字段：schema_version, chain_key, seq, event_id, actor_type,
// actor_id, action, target_type, target_id, result, before_digest, after_digest,
// reason, occurred_at, trace_id, source_app, ip_hash, device_hash, prev_hash。
// 不参与：entry_id（由 chain_key + seq 决定）、ctime（入库时间）、archived_at
// （归档时改写，其证据由 audit_archive_batch.manifest_hash 覆盖）。
func serializeV1(e *AuditEntry) string {
	fields := []string{
		strconv.FormatInt(int64(e.SchemaVersion), 10),
		e.ChainKey,
		strconv.FormatInt(e.Seq, 10),
		e.EventID,
		strconv.FormatInt(int64(e.ActorType), 10),
		strconv.FormatInt(e.ActorID, 10),
		e.Action,
		e.TargetType,
		e.TargetID,
		strconv.FormatInt(int64(e.Result), 10),
		e.BeforeDigest,
		e.AfterDigest,
		e.Reason,
		strconv.FormatInt(e.OccurredAt, 10),
		e.TraceID,
		strconv.FormatInt(int64(e.SourceApp), 10),
		e.IPHash,
		e.DeviceHash,
		e.PrevHash,
	}
	return strings.Join(fields, fieldSep)
}

// ComputeEntryHash 计算条目的 entry_hash（sha256hex，64 位小写十六进制）。
// 调用前必须已填好 ChainKey/Seq/PrevHash 三个链字段。
func ComputeEntryHash(e *AuditEntry) string {
	return sha256Hex([]byte(serializeV1(e)))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ShortHashLen 是来源 IP / 设备标识落库长度：sha256hex 前 32 个字符。
// 截断到 32 位是为了让索引更小而碰撞概率对审计维度仍可忽略
// （同一盐值空间下 2^16 量级来源才需要担心生日碰撞）。
const ShortHashLen = 32

// ShortHash 计算来源标识的不可逆短哈希：sha256hex(salt + value) 的前 32 位。
// salt 必填：裸 SHA-256(IP) 可被枚举反查（IPv4 只有 2^32 种），加盐后才谈得上不可逆。
// value 为空时返回空串，让「未知来源」与「已知来源」在库里可区分，
// 不要用一个假哈希去占位。
func ShortHash(salt, value string) string {
	if value == "" {
		return ""
	}
	full := sha256Hex([]byte(salt + value))
	return full[:ShortHashLen]
}

// 摘要白名单：只允许下面三种形态，其余一律拒绝入库。
//
// 采用「白名单格式」而不是「黑名单关键词」，是因为黑名单永远列不全，
// 而审计摘要的语义本来就是「变更点不可逆摘要」，不需要自由文本：
//  1. 空串：无变更或无对应侧（新增只有 after，删除只有 before）；
//  2. 64 位十六进制：整对象摘要；
//  3. 分号分隔的 `字段名=16 位十六进制` 列表：逐字段摘要，如
//     state=a1b2c3d4e5f60718;reason=9f8e7d6c5b4a3210。
var (
	digestFullHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestFieldRe   = regexp.MustCompile(`^[A-Za-z0-9_.]{1,64}=[0-9a-f]{16}$`)
)

// ValidDigest 判定前后摘要是否落在允许的格式内。
func ValidDigest(s string) bool {
	if s == "" {
		return true
	}
	if digestFullHexRe.MatchString(s) {
		return true
	}
	for _, pair := range strings.Split(s, ";") {
		if !digestFieldRe.MatchString(pair) {
			return false
		}
	}
	return true
}

// 明文敏感信息兜底检测（AGENTS.md §7）。摘要侧已经走白名单，
// reason / actor_name 仍是自由文本，写入前统一扫一遍，
// 命中即拒绝整条，让调用方改传摘要而不是悄悄入库。
var (
	piiMobileRe = regexp.MustCompile(`\b1[3-9][0-9]{9}\b`)
	piiEmailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	piiIDCardRe = regexp.MustCompile(`\b[0-9]{17}[0-9Xx]\b`)
	piiCredRe   = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|authorization|cookie)\b\s*[:=]`)
)

// LooksLikePII 判定自由文本是否疑似含明文敏感信息。
// 判定刻意保守：宁可拒写也不落明文。调用方若确实需要记录敏感字段的变更，
// 必须改传 ValidDigest 允许的不可逆摘要形式。
// 注意：target_id / actor_id 这类必然为纯数字的引用字段不走该判定
// （它们是本服务只读引用的业务主键，不是隐私标识）。
func LooksLikePII(s string) bool {
	if s == "" {
		return false
	}
	return piiMobileRe.MatchString(s) ||
		piiEmailRe.MatchString(s) ||
		piiIDCardRe.MatchString(s) ||
		piiCredRe.MatchString(s)
}
