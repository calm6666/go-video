package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"
)

// 采样与 schema 兼容判定的纯函数（ec_dispatch_policy 语义）。
//
// 顺序硬约束（proto 注释、AGENTS.md §7）：校验 → 脱敏 → 采样 → 落库/投递。
// 采样必须在脱敏之后：否则被丢弃的事件仍可能把明文 IP/设备号留在中间日志里。

// SampleBase 采样基点：10000 = 全量。
const SampleBase = 10000

// SampleWildcard 兜底规则的 event_type。
const SampleWildcard = "*"

// SampleHit 按 event_id 做确定性采样：同一事件在任何实例上结论一致。
//
// 用 event_id 而不是随机数，是为了让「重放同一批次」得到同样的采样结论，
// 否则幂等回放会出现第一次 ACCEPTED、第二次 SAMPLED_OUT 的自相矛盾台账。
// bps<=0 全丢，bps>=10000 全留。
func SampleHit(eventID string, bps int32) bool {
	if bps >= SampleBase {
		return true
	}
	if bps <= 0 || eventID == "" {
		return false
	}
	sum := sha256.Sum256([]byte(eventID))
	bucket := binary.BigEndian.Uint32(sum[:4])
	return int32(bucket%uint32(SampleBase)) < bps
}

// EffectiveSampleBps 选出作用于该事件的采样比例：
//  1. 先找 event_type 精确匹配、且 quality 标记匹配的规则；
//  2. 再找 "*" 兜底规则（其 quality_events 为 true 时只作用于质量事件）；
//  3. 找不到返回 -1，调用方必须按「全量」保守处理并报警，不得当成 0 丢事件。
func EffectiveSampleBps(rules []SampleRule, eventType string, isQuality bool) int32 {
	var fallback int32 = -1
	for _, r := range rules {
		if r.QualityEvents != isQuality {
			continue
		}
		if r.EventType == eventType && eventType != "" {
			return clampBps(r.SampleBps)
		}
		if r.EventType == SampleWildcard && fallback < 0 {
			fallback = clampBps(r.SampleBps)
		}
	}
	return fallback
}

func clampBps(bps int32) int32 {
	if bps < 0 {
		return 0
	}
	if bps > SampleBase {
		return SampleBase
	}
	return bps
}

// MinSupportedSchemaVersion 本服务接受的最小事件结构版本。
const MinSupportedSchemaVersion = 1

// SchemaSupported 判定客户端声明的 schema_version 是否可用：
// 只接受 1..serverMax（高于服务端支持版本一律 REJECT_UNSUPPORTED_SCHEMA_VERSION，
// 不做「猜测性兼容」，否则新字段会被静默丢掉、下游特征算错却无人察觉）。
func SchemaSupported(requested, serverMax int32) bool {
	return requested >= MinSupportedSchemaVersion && requested <= serverMax
}

// NormalizeEventType 归一化 event_type：为空时由 category 推导 behavior.<suffix>。
// 返回 ok=false 表示既没给 event_type 也没有合法 category（REJECT_MISSING_EVENT_TYPE）。
func NormalizeEventType(eventType string, categorySuffix string) (string, bool) {
	if s := strings.TrimSpace(eventType); s != "" {
		return s, true
	}
	if categorySuffix == "" {
		return "", false
	}
	return "behavior." + categorySuffix, true
}

// SampleRule 单类事件的采样规则（JSON 存进 ec_dispatch_policy.sample_rules）。
type SampleRule struct {
	EventType     string `json:"event_type"`
	SampleBps     int32  `json:"sample_bps"`
	QualityEvents bool   `json:"quality_events"`
}

// EncodeStringList / DecodeStringList：白名单与禁止字段以 JSON 数组存进 TEXT 列，
// 空切片统一编码成 "[]"，避免 NULL 与空串两套语义。
func EncodeStringList(list []string) (string, error) {
	if len(list) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(list)
	return string(b), err
}

func DecodeStringList(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// EncodeSampleRules / DecodeSampleRules 同上，针对采样规则。
func EncodeSampleRules(rules []SampleRule) (string, error) {
	if len(rules) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(rules)
	return string(b), err
}

func DecodeSampleRules(s string) ([]SampleRule, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []SampleRule
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// ValidateSampleRules 策略入库前的自检：event_type 非空、bps 在 0..10000、通配符最多一条。
func ValidateSampleRules(rules []SampleRule) bool {
	wildcards := 0
	for _, r := range rules {
		if strings.TrimSpace(r.EventType) == "" {
			return false
		}
		if r.SampleBps < 0 || r.SampleBps > SampleBase {
			return false
		}
		if r.EventType == SampleWildcard {
			wildcards++
			if r.QualityEvents {
				// 通配 + 只作用质量事件 = 一半事件没有归属规则，属于配错。
				return false
			}
		}
	}
	return wildcards <= 1
}
