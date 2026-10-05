package model

// 写入状态闸门的归属测试。
//
// 「哪个状态允许出现新的特征值」这条规则曾同时在 model 的行构造入口和 logic 的写入入口
// 各写一遍，而 model 那一遍写错了方向：它复用 ErrFeatureNotActive 挡写入，而这个哨兵的
// 声明语义是「特征处于 DRAFT/RETIRED，不参与对外读」（model/errors.go:50-51），
// 契约里唯一写明不接受写入的状态是 RETIRED（rpc/featurestore.proto:93，哨兵 ErrFeatureRetired）。
// 挡错的直接后果是把契约要求可写的 DRAFT 一起挡死 —— 回填目标版本恒为 DRAFT
// （rpc/featurestore.proto:345），而「升 ACTIVE 前该版本已有值」是 UpdateFeatureState 的
// 上线前置（README §5 表 UpdateFeatureState 行），NewFeatureValue 又是「构造待写行的唯一入口」
// （model/featurevalue.go:197），三者叠加后一个 feature_key 的**首个**版本（没有上一版本可降级）
// 在任何路径上都拿不到值。
//
// 现在的分工（本文件与 internal/logic/idempotent_writes_test.go 各钉一侧）：
//   - NewFeatureValue 只挡契约明确禁止写入的 RETIRED（rpc/featurestore.proto:93），
//     以及状态不在已声明枚举内的脏定义行；
//   - 「外部写入只接受 ACTIVE」留在 internal/logic/writefeatureslogic.go 的 checkRow。
// 因此这里若被改宽一分，logic 侧那条入口断言必须变红；反之亦然。

import (
	"errors"
	"testing"
)

// gateDef 造一条「除 state 以外全部合法」的定义：状态是唯一变量，
// 失败只能归因于状态闸门，而不是别的校验顺带挡了一下。
func gateDef(state int32) *FeatureDefinition {
	return &FeatureDefinition{
		FeatureKey:    "user_play_finish_7d",
		Version:       1,
		Name:          "user_play_finish_7d",
		ValueType:     ValueTypeInt64,
		EntityScope:   EntityScopeMid,
		Source:        SourceSpmMetric,
		PrivacyLevel:  PrivacyPseudonymous,
		WindowSeconds: 7 * 24 * 3600,
		TTLSeconds:    3600,
		DefaultValue:  "7",
		State:         state,
		Description:   "近 7 日完播率分子",
		ChangeNote:    "首版",
		CreatedBy:     "admin:tester",
	}
}

func gateKey() ValueKey {
	return ValueKey{FeatureKey: "user_play_finish_7d", Version: 1,
		EntityScope: EntityScopeMid, EntityID: "10086"}
}

func gatePayload() ValuePayload {
	return ValuePayload{ValueType: ValueTypeInt64, Int64Value: 5}
}

// TestDraftDefinitionPassesItsOwnContract 是上面那两个测试的前提检查：
// 如果 DRAFT 定义本身就不过注册校验，那「DRAFT 可写」这条断言什么也没测到。
func TestDraftDefinitionPassesItsOwnContract(t *testing.T) {
	for _, state := range []int32{FeatureStateDraft, FeatureStateActive, FeatureStateRetired} {
		def := gateDef(state)
		if err := ValidateFeatureDefinition(def); err != nil {
			t.Fatalf("gate definition (state %d) violates its own contract: %v", state, err)
		}
	}
}

// TestNewFeatureValueAcceptsTheDraftBackfillTarget 钉住契约要求的那一侧：
// 回填与上线前铺值都发生在 DRAFT 上，行构造入口必须放行，且放出来的行是完整的。
func TestNewFeatureValueAcceptsTheDraftBackfillTarget(t *testing.T) {
	def := gateDef(FeatureStateDraft)
	const eventTime = int64(1_700_000_000)

	row, err := NewFeatureValue(def, gateKey(), gatePayload(), eventTime,
		"play_finish_rate@v1", "req-draft-write", "offline-job:9", 9)
	if err != nil {
		t.Fatalf("NewFeatureValue on a DRAFT target: %v", err)
	}
	if row.Key() != gateKey() {
		t.Errorf("row key = %+v, want %+v", row.Key(), gateKey())
	}
	if row.Int64Value != 5 || row.ValueType != ValueTypeInt64 {
		t.Errorf("row value = %d/%d, want the encoded int64 5", row.Int64Value, row.ValueType)
	}
	if row.ExpireAt != eventTime+def.TTLSeconds {
		t.Errorf("expire_at = %d, want event_time + ttl = %d", row.ExpireAt, eventTime+def.TTLSeconds)
	}
	if row.BackfillJobID != 9 || row.SourceMetricKey != "play_finish_rate@v1" {
		t.Errorf("row traceability lost: job=%d metric=%q, want 9/play_finish_rate@v1",
			row.BackfillJobID, row.SourceMetricKey)
	}
	// 放行不等于放宽：同一道闸里的 TTL 与主体形态约束对 DRAFT 一样生效。
	if _, err := NewFeatureValue(def, gateKey(), gatePayload(), eventTime,
		"play_finish_rate@v1", "req", "system:x", 0); err != nil {
		t.Errorf("the non-backfill DRAFT write failed: %v", err)
	}
	// 设备摘要塞进 MID 维度：MID 只收十进制主键串，形态闸门必须挡住。
	// （ValidEntityID 对数字维度只校验「十进制正整数」这一形态，所以它拦不住
	//  恰好是数字形态的明文标识 —— 明文设备号/原始 IP 靠 DEVICE、IP_HASH
	//  两档强制十六进制摘要来挡，见 logic 侧的 ENTITY_ID_INVALID 用例。）
	mismatched := gateKey()
	mismatched.EntityID = "0123456789abcdef0123456789abcdef01234567"
	if _, err := NewFeatureValue(def, mismatched, gatePayload(), eventTime,
		"play_finish_rate@v1", "req", "system:x", 0); !errors.Is(err, ErrEntityIDInvalid) {
		t.Errorf("hex digest under MID scope err = %v, want %v", err, ErrEntityIDInvalid)
	}
	// 没有到期时间的行一律不放行：DRAFT 也不是「永久保留个体数据」的借口。
	noTTL := gateDef(FeatureStateDraft)
	noTTL.TTLSeconds = 0
	if _, err := NewFeatureValue(noTTL, gateKey(), gatePayload(), eventTime,
		"play_finish_rate@v1", "req", "system:x", 0); !errors.Is(err, ErrTTLRequired) {
		t.Errorf("zero-TTL DRAFT row err = %v, want %v", err, ErrTTLRequired)
	}
}

// TestNewFeatureValueStateGateRefusesOnlyTheContractProhibitedStates 钉住另一侧：
// RETIRED 必须继续被拒（契约里唯一写明「不再接受写入」的状态），
// 未声明状态（脏定义行）也不许写。
func TestNewFeatureValueStateGateRefusesOnlyTheContractProhibitedStates(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  error
	}{
		{"retired", FeatureStateRetired, ErrFeatureRetired},
		{"unspecified", FeatureStateUnspecified, ErrFeatureStateTransition},
		{"out of range", FeatureStateRetired + 7, ErrFeatureStateTransition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewFeatureValue(gateDef(c.state), gateKey(), gatePayload(),
				1_700_000_000, "play_finish_rate@v1", "req", "offline-job:9", 9)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestIsWritableStillGatesExternalWritesToActiveVersions 是被改窄的那道闸的反向守卫：
// IsWritable 表达的是「WriteFeatures 这个对外入口接不接受这一行」，
// 它的口径必须停留在「只有 ACTIVE」，否则上面那条 DRAFT 放行就变成给外部开洞。
func TestIsWritableStillGatesExternalWritesToActiveVersions(t *testing.T) {
	for _, c := range []struct {
		name  string
		state int32
		want  bool
	}{
		{"draft", FeatureStateDraft, false},
		{"active", FeatureStateActive, true},
		{"retired", FeatureStateRetired, false},
		{"unspecified", FeatureStateUnspecified, false},
	} {
		if got := gateDef(c.state).IsWritable(); got != c.want {
			t.Errorf("IsWritable(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	var nilDef *FeatureDefinition
	if nilDef.IsWritable() {
		t.Error("IsWritable on a nil definition = true: an unknown version can never be written")
	}
}
