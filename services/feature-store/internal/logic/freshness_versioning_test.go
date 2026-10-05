// 测试域 1：新鲜度与版本表达。
//
// README「降级矩阵」与 model.ClassifyDegradation 的契约是：
//  1. 每一次读都必须自己说明「服务的是哪一版、是不是降级」，调用方不需要靠约定猜；
//  2. 快照缺失/过期/上游不可用只能以显式降级码出现，绝不把 0 值当成真实特征；
//  3. 定义或 ACTIVE 指针本身读不到是**错误**，不是降级（没有定义就没有可信默认值）。
package logic

import (
	"errors"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 本域共用的调用与断言小工具（只是将重复代码收拢，不做任何放宽）---

func midEntity() *rpc.EntityRef {
	return &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID, EntityId: testMid}
}

func callGetFeature(f *fixture, key string, version int32, allowStale bool) (*rpc.GetFeatureReply, error) {
	return NewGetFeatureLogic(f.ctx, f.ServiceContext).GetFeature(&rpc.GetFeatureReq{
		Feature:    &rpc.FeatureRef{FeatureKey: key, Version: version},
		Entity:     midEntity(),
		AllowStale: allowStale,
	})
}

// assertEntryStatesItsVersion 是「每一次读都自证版本」这条不变量。
// 值来自哪一版、请求解析到哪一版，必须是同一个可核对的事实。
func assertEntryStatesItsVersion(t *testing.T, e *rpc.FeatureEntry) {
	t.Helper()
	if e == nil {
		t.Fatal("entry is nil")
	}
	if e.GetFeature() == nil || e.GetFeature().GetFeatureKey() == "" {
		t.Fatalf("entry does not state which feature it serves: %+v", e)
	}
	if e.GetResolvedVersion() < 1 {
		t.Errorf("%s: resolved_version=%d, every read must name a concrete version",
			e.GetFeature().GetFeatureKey(), e.GetResolvedVersion())
	}
	if e.GetFeature().GetVersion() != e.GetResolvedVersion() {
		t.Errorf("%s: feature.version=%d disagrees with resolved_version=%d",
			e.GetFeature().GetFeatureKey(), e.GetFeature().GetVersion(), e.GetResolvedVersion())
	}
	if !model.ValidDegradation(int32(e.GetDegradation())) {
		t.Errorf("%s: degradation %d is not a declared enum value",
			e.GetFeature().GetFeatureKey(), e.GetDegradation())
	}
}

// assertDegradedNeverLooksLikeRealValue 是「不把 0 值冒充真实值」的可检查形式：
// 降级条目要么带 found=false，要么显式标 EXPIRED/PREVIOUS_VERSION，
// 并且绝不带着一个像真快照的时间戳。
func assertDegradedNeverLooksLikeRealValue(t *testing.T, e *rpc.FeatureEntry, found bool) {
	t.Helper()
	assertEntryStatesItsVersion(t, e)
	if !found && isDegraded(e) {
		if e.GetEventTime() != 0 || e.GetExpireAt() != 0 {
			t.Errorf("un-found entry carries event_time=%d expire_at=%d: a degraded entry must not look like a real snapshot",
				e.GetEventTime(), e.GetExpireAt())
		}
	}
	if found && !isDegraded(e) && (e.GetEventTime() == 0 || e.GetExpireAt() == 0) {
		t.Errorf("fresh entry must carry its own event_time/expire_at so the caller can re-check freshness")
	}
	if got, want := found, foundByDegradation(e.GetDegradation()); got != want {
		t.Errorf("reply.found=%v contradicts entry.degradation=%v", got, want)
	}
}

// --- ACTIVE 指针与显式版本 ---

func TestGetFeatureVersionZeroServesActivePointerVersion(t *testing.T) {
	f := newFixture(t)
	v1 := f.putDef(newDef("user_play_finish_7d", 1))
	v2 := f.putDef(newDef("user_play_finish_7d", 2))
	f.putPointer("user_play_finish_7d", 2, 1)
	f.putInt64(v2, 2, testMid, 99, testNow-60)
	f.putInt64(v1, 1, testMid, 11, testNow-60)

	reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
	if got := reply.GetEntry().GetResolvedVersion(); got != 2 {
		t.Errorf("resolved_version=%d, want the ACTIVE pointer's 2", got)
	}
	if got := reply.GetEntry().GetValue().GetInt64Value(); got != 99 {
		t.Errorf("value=%d, want v2's 99: the pointer, not the storage order, picks the snapshot", got)
	}
	if reply.GetEntry().GetDegradation() != rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE {
		t.Errorf("degradation=%v for a fresh value, want NONE", reply.GetEntry().GetDegradation())
	}
	if !reply.GetFound() {
		t.Error("found=false for a fresh value")
	}
}

func TestGetFeatureExplicitVersionIgnoresActivePointer(t *testing.T) {
	f := newFixture(t)
	v1 := f.putDef(newDef("user_play_finish_7d", 1))
	v2 := f.registerActive(newDef("user_play_finish_7d", 2))
	f.putInt64(v1, 1, testMid, 11, testNow-60)
	f.putInt64(v2, 2, testMid, 99, testNow-60)

	reply, err := callGetFeature(f, "user_play_finish_7d", 1, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	assertEntryStatesItsVersion(t, reply.GetEntry())
	if got := reply.GetEntry().GetResolvedVersion(); got != 1 {
		t.Errorf("resolved_version=%d, an explicit version must not be silently re-pointed at the ACTIVE v2", got)
	}
	if got := reply.GetEntry().GetValue().GetInt64Value(); got != 11 {
		t.Errorf("value=%d, want v1's 11", got)
	}
}

func TestGetFeatureMissingDefinitionIsErrorNotDegradedValue(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 2))

	_, err := callGetFeature(f, "user_play_finish_7d", 9, false)
	if !errors.Is(err, model.ErrFeatureNotFound) {
		t.Fatalf("err=%v, want the explicit %v", err, model.ErrFeatureNotFound)
	}
}

func TestGetFeatureWithoutActiveVersionIsError(t *testing.T) {
	t.Run("pointer row missing", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 1))
		// 没有指针行：注册未走完，读路径不能猜一个版本出来。

		_, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if !errors.Is(err, model.ErrNoActiveVersion) {
			t.Fatalf("err=%v, want %v", err, model.ErrNoActiveVersion)
		}
	})
	t.Run("pointer exists but active=0", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 1))
		f.putPointer("user_play_finish_7d", model.NoActiveVersion, model.NoActiveVersion)

		_, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if !errors.Is(err, model.ErrNoActiveVersion) {
			t.Fatalf("err=%v, want %v", err, model.ErrNoActiveVersion)
		}
	})
}

// --- 缺失 / 过期 / 上一版本 ---

func TestGetFeatureWithoutValueUsesDeclaredDefaultNotZero(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withDefault("7")))

	reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
	if reply.GetFound() {
		t.Error("found=true for a cold-start entity: the caller would feed a default into the model as if it were observed")
	}
	if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE {
		t.Errorf("degradation=%v, want DEFAULT_VALUE", got)
	}
	if got := reply.GetEntry().GetValue().GetInt64Value(); got != 7 {
		t.Errorf("value=%d, want the definition's declared default 7 and never an implicit 0", got)
	}
}

func TestGetFeatureWithUnparseableStoredDefaultFails(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("user_play_finish_7d", 1, withDefault("7")))
	// 造脏数据：库里那一行的 default_value 在写入规则收紧之前就已经不可解析。
	// 这种行只能报错，不能给一个 0 让调用方当特征用。
	k := model.DefinitionKey{FeatureKey: d.FeatureKey, Version: 1}
	row := f.defs.rows[k]
	row.DefaultValue = "not-a-number"
	f.defs.rows[k] = row

	_, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if !errors.Is(err, model.ErrDefaultValueInvalid) {
		t.Fatalf("err=%v, want %v", err, model.ErrDefaultValueInvalid)
	}
}

func TestGetFeatureExpiredValueSelections(t *testing.T) {
	const ttl = int64(3600)
	t.Run("allow_stale returns the real old value flagged EXPIRED", func(t *testing.T) {
		f := newFixture(t)
		d := f.registerActive(newDef("user_play_finish_7d", 1, withTTL(ttl)))
		f.putInt64(d, 1, testMid, 55, testNow-2*ttl) // expire_at = now-3600

		reply, err := callGetFeature(f, "user_play_finish_7d", 0, true)
		if err != nil {
			t.Fatalf("GetFeature: %v", err)
		}
		assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
		if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED {
			t.Errorf("degradation=%v, want EXPIRED", got)
		}
		if !reply.GetFound() {
			t.Error("found=false while returning a real stored value")
		}
		if got := reply.GetEntry().GetValue().GetInt64Value(); got != 55 {
			t.Errorf("value=%d, want the stale but real 55", got)
		}
		if got := reply.GetEntry().GetExpireAt(); got != testNow-ttl {
			t.Errorf("expire_at=%d, want the row's own %d so the caller sees how stale it is", got, testNow-ttl)
		}
	})

	t.Run("stale not accepted falls back to the previous version", func(t *testing.T) {
		f := newFixture(t)
		v1 := f.putDef(newDef("user_play_finish_7d", 1, withTTL(ttl)))
		v2 := f.putDef(newDef("user_play_finish_7d", 2, withTTL(ttl)))
		f.putPointer("user_play_finish_7d", 2, 1)
		f.putInt64(v2, 2, testMid, 55, testNow-2*ttl) // 过期
		f.putInt64(v1, 1, testMid, 33, testNow-10)    // 上一版本仍新鲜

		reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if err != nil {
			t.Fatalf("GetFeature: %v", err)
		}
		assertEntryStatesItsVersion(t, reply.GetEntry())
		if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_PREVIOUS_VERSION {
			t.Errorf("degradation=%v, want PREVIOUS_VERSION", got)
		}
		if got := reply.GetEntry().GetResolvedVersion(); got != 1 {
			t.Errorf("resolved_version=%d: the entry must name v1, the version it actually served", got)
		}
		if got := reply.GetEntry().GetValue().GetInt64Value(); got != 33 {
			t.Errorf("value=%d, want v1's 33", got)
		}
	})

	t.Run("expired and no previous version falls to DEFAULT_VALUE", func(t *testing.T) {
		f := newFixture(t)
		d := f.registerActive(newDef("user_play_finish_7d", 1, withTTL(ttl), withDefault("7")))
		f.putInt64(d, 1, testMid, 55, testNow-2*ttl)

		reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if err != nil {
			t.Fatalf("GetFeature: %v", err)
		}
		assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
		if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE {
			t.Errorf("degradation=%v, want DEFAULT_VALUE", got)
		}
		if reply.GetEntry().GetValue().GetInt64Value() != 7 {
			t.Error("a DEFAULT_VALUE entry must carry the declared default")
		}
	})
}

func TestGetFeaturePreviousVersionRowAlsoExpiredIsNotUsed(t *testing.T) {
	f := newFixture(t)
	const ttl = int64(3600)
	// 当前版本压根没有值，上一版本只有一行早已过 TTL 的残值：
	// PREVIOUS_VERSION 这一档要求「上一版仍可用」，过期残值不满足，只能落到默认值。
	v1 := f.putDef(newDef("user_play_finish_7d", 1, withTTL(ttl)))
	f.putDef(newDef("user_play_finish_7d", 2, withTTL(ttl)))
	f.putPointer("user_play_finish_7d", 2, 1)
	f.putInt64(v1, 1, testMid, 33, testNow-2*ttl)

	reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
	if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE {
		t.Errorf("degradation=%v, want DEFAULT_VALUE: an expired previous-version row is not a usable fallback", got)
	}
	if reply.GetEntry().GetValue().GetInt64Value() != 7 {
		t.Errorf("value=%d, want the declared default 7 rather than the 33-hour-old 33",
			reply.GetEntry().GetValue().GetInt64Value())
	}
}

// --- RETIRED / SOURCE_UNAVAILABLE / 越权维度 ---

func TestGetFeatureRetiredStatesRetiredAndReadsNoValue(t *testing.T) {
	f := newFixture(t)
	d := f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateRetired), withDefault("7")))
	f.putPointer("user_play_finish_7d", 1, 0)
	// 下线版本上的历史残值只能绕过 NewFeatureValue 塞进去（契约规定 RETIRED 不接受写入），
	// 正是这一行用来验证「RETIRED 的值不可能再被读出去」。
	f.putRawValue(&model.FeatureValue{
		FeatureKey: d.FeatureKey, Version: 1, EntityScope: model.EntityScopeMid,
		EntityID: testMid, ValueType: model.ValueTypeInt64, Int64Value: 55,
		EventTime: testNow - 60, ExpireAt: testNow + 3600,
	})

	reply, err := callGetFeature(f, "user_play_finish_7d", 1, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
	if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_FEATURE_RETIRED {
		t.Errorf("degradation=%v, want FEATURE_RETIRED", got)
	}
	if reply.GetFound() {
		t.Error("found=true for a retired feature")
	}
	if called(f.values.calls, "values.FindEntries") {
		t.Error("a retired feature still read feature_value: the retired value must not reach a caller")
	}
}

func TestGetFeatureSourceUnavailableNeverLooksLikeColdStart(t *testing.T) {
	t.Run("fallback disabled", func(t *testing.T) {
		f := newFixture(t).withDBFallback(false)
		f.registerActive(newDef("user_play_finish_7d", 1, withDefault("7")))

		reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if err != nil {
			t.Fatalf("GetFeature: %v", err)
		}
		assertDegradedNeverLooksLikeRealValue(t, reply.GetEntry(), reply.GetFound())
		if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_SOURCE_UNAVAILABLE {
			t.Errorf("degradation=%v, want SOURCE_UNAVAILABLE: 「读不到」与「没有值」在调用方是两种处置", got)
		}
		if reply.GetFound() {
			t.Error("found=true while the store is unavailable")
		}
	})
	t.Run("db fallback fails", func(t *testing.T) {
		f := newFixture(t)
		d := f.registerActive(newDef("user_play_finish_7d", 1, withDefault("7")))
		f.putInt64(d, 1, testMid, 55, testNow-60)
		f.values.errFindEntries = errors.New("feature_value: connection refused")

		reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
		if err != nil {
			t.Fatalf("GetFeature: %v", err)
		}
		if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_SOURCE_UNAVAILABLE {
			t.Errorf("degradation=%v, want SOURCE_UNAVAILABLE", got)
		}
		if reply.GetFound() {
			t.Error("found=true for a value that was never read")
		}
		if reply.GetEntry().GetValue().GetInt64Value() != 7 {
			t.Error("SOURCE_UNAVAILABLE must still carry the declared default, never a 0")
		}
	})
}

func TestGetFeatureWrongEntityScopeReadsNothing(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("user_play_finish_7d", 1,
		withScope(model.EntityScopeMid), withDefault("7")))
	// 同一 key 在库里确实有一行 DEVICE 维度的值（历史脏数据）：
	// 定义说的是 MID 维度，读请求带 DEVICE 主体时一行都不许被取走。
	f.putRawValue(&model.FeatureValue{
		FeatureKey: d.FeatureKey, Version: 1, EntityScope: model.EntityScopeDevice,
		EntityID: testDeviceID, ValueType: model.ValueTypeInt64, Int64Value: 55,
		EventTime: testNow - 60, ExpireAt: testNow + 3600,
	})

	reply, err := NewGetFeatureLogic(f.ctx, f.ServiceContext).GetFeature(&rpc.GetFeatureReq{
		Feature: &rpc.FeatureRef{FeatureKey: "user_play_finish_7d", Version: 0},
		Entity: &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE,
			EntityId: testDeviceID},
	})
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if called(f.values.calls, "values.FindEntries") {
		t.Errorf("a scope-mismatched read still hit feature_value: %+v", f.values.entries)
	}
	// 越权维度只能落在「定义自带的默认值 + 显式降级」上。
	if got := reply.GetEntry().GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE {
		t.Errorf("degradation=%v, want DEFAULT_VALUE", got)
	}
	if reply.GetEntry().GetValue().GetInt64Value() != 7 {
		t.Errorf("value=%d, want the declared default and never the other scope's 55",
			reply.GetEntry().GetValue().GetInt64Value())
	}
}

// --- 批量读：逐条自证版本 + 降级计数 ---

func TestBatchGetStatesVersionForEveryEntry(t *testing.T) {
	f := newFixture(t)
	a := f.registerActive(newDef("user_play_finish_7d", 2))
	f.putDef(newDef("user_play_finish_7d", 1))
	b := f.registerActive(newDef("user_up_count_7d", 1))
	c := f.putDef(newDef("aid_hot_score_7d", 3, withScope(model.EntityScopeAid)))
	f.putPointer("aid_hot_score_7d", 3, 0)
	f.putInt64(a, 2, testMid, 200, testNow-60)
	f.putInt64(b, 1, testMid, 300, testNow-60)
	f.putInt64(c, 3, testMid, 400, testNow-60) // 故意写在错的维度上，永远读不到

	reply, err := NewBatchGetFeaturesLogic(f.ctx, f.ServiceContext).BatchGetFeatures(
		&rpc.BatchGetFeaturesReq{
			Features: []*rpc.FeatureRef{
				{FeatureKey: "user_play_finish_7d"},          // 走指针
				{FeatureKey: "user_up_count_7d"},             // 走指针
				{FeatureKey: "aid_hot_score_7d", Version: 3}, // 显式版本、维度不符
			},
			Entities: []*rpc.EntityRef{
				midEntity(),
				{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID, EntityId: "10087"}, // 冷启动
			},
		})
	if err != nil {
		t.Fatalf("BatchGetFeatures: %v", err)
	}
	if len(reply.GetEntries()) != 6 {
		t.Fatalf("entries=%d, want 3 features x 2 entities = 6", len(reply.GetEntries()))
	}
	if reply.GetRequested() != 6 {
		t.Errorf("requested=%d, want 6", reply.GetRequested())
	}
	var degraded int32
	for _, e := range reply.GetEntries() {
		assertEntryStatesItsVersion(t, e)
		if isDegraded(e) {
			degraded++
			if e.GetValue() == nil {
				t.Errorf("%s: a degraded entry must still carry the declared default value", e.GetFeature().GetFeatureKey())
			}
		}
	}
	if reply.GetDegraded() != degraded {
		t.Errorf("degraded=%d but %d entries carry a non-NONE code", reply.GetDegraded(), degraded)
	}
	if degraded < 4 {
		t.Errorf("degraded=%d, want at least the cold-start pair, the retired-scope pair and the 404-free mismatches: %+v",
			degraded, reply.GetEntries())
	}
	// 冷启动那一条必须走默认值而不是被当成真值。
	cold := reply.GetEntries()[1]
	if cold.GetEntity().GetEntityId() != "10087" {
		t.Fatalf("entry ordering broke: %+v", cold)
	}
	if cold.GetDegradation() != rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE ||
		cold.GetValue().GetInt64Value() != 7 {
		t.Errorf("cold-start entry = degradation %v value %d, want DEFAULT_VALUE and the declared default 7",
			cold.GetDegradation(), cold.GetValue().GetInt64Value())
	}
}

func TestBatchReadFallsBackWithVersionedKeysForBothSnapshots(t *testing.T) {
	f := newFixture(t)
	v1 := f.putDef(newDef("user_play_finish_7d", 1))
	v2 := f.putDef(newDef("user_play_finish_7d", 2))
	f.putPointer("user_play_finish_7d", 2, 1)
	f.putInt64(v1, 1, testMid, 11, testNow-60)
	f.putInt64(v2, 2, testMid, 99, testNow-60)

	_, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	got := fallbackCacheKeys(f.values.entries)
	// 主读键按版本分列，所以一次读必须同时取「当前版」与「上一版」两套快照：
	// 只取当前版意味着 PREVIOUS_VERSION 这一档降级在回源路径上根本不可达。
	want := []string{
		model.ValueCacheKey("user_play_finish_7d", 1, model.EntityScopeMid, testMid),
		model.ValueCacheKey("user_play_finish_7d", 2, model.EntityScopeMid, testMid),
	}
	if len(got) != len(want) {
		t.Fatalf("fallback key set = %v, want %v", sortedKeys(got), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("fallback did not read %v (read set %v)", k, sortedKeys(got))
		}
	}
}

func TestActivePointerReadOncePerBatch(t *testing.T) {
	f := newFixture(t)
	for _, k := range []string{"user_play_finish_7d", "user_up_count_7d", "user_danmaku_7d"} {
		f.registerActive(newDef(k, 1))
	}
	_, err := NewBatchGetFeaturesLogic(f.ctx, f.ServiceContext).BatchGetFeatures(
		&rpc.BatchGetFeaturesReq{
			Features: []*rpc.FeatureRef{
				{FeatureKey: "user_play_finish_7d"}, {FeatureKey: "user_up_count_7d"},
				{FeatureKey: "user_danmaku_7d"},
			},
			Entities: []*rpc.EntityRef{midEntity()},
		})
	if err != nil {
		t.Fatalf("BatchGetFeatures: %v", err)
	}
	// 指针一次批量取回：逐 key 点查在 50 x 20 的读请求里会放大成几十次往返。
	if n := countCalled(f.pointers.calls, "activeVersions.FindOne"); n != 0 {
		t.Errorf("active pointer point-queries=%d, batch reads must use ListByKeys", n)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.ListByKeys"); n < 1 {
		t.Errorf("ListByKeys calls=%d, want the pointer table read in one batch", n)
	}
}

func TestFoundFlagAndDegradationCodesAgree(t *testing.T) {
	// foundByDegradation 是 GetFeatureReply.found 的唯一来源，逐码穷尽。
	cases := []struct {
		deg  rpc.FeatureDegradation
		want bool
	}{
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE, true},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_DEFAULT_VALUE, false},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_PREVIOUS_VERSION, true},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED, true},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_SOURCE_UNAVAILABLE, false},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_FEATURE_RETIRED, false},
		{rpc.FeatureDegradation_FEATURE_DEGRADATION_UNSPECIFIED, false},
	}
	for _, c := range cases {
		if got := foundByDegradation(c.deg); got != c.want {
			t.Errorf("foundByDegradation(%v)=%v, want %v", c.deg, got, c.want)
		}
	}
}

func TestExpiredBoundaryUsesServerClock(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("user_play_finish_7d", 1, withTTL(3600), withDefault("7")))
	f.putInt64(d, 1, testMid, 55, testNow) // expire_at = now + 3600

	reply, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if reply.GetEntry().GetDegradation() != rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE {
		t.Fatalf("a row expiring in one hour must be fresh, got %v", reply.GetEntry().GetDegradation())
	}
	// 同一行跨过 expire_at 之后必须降级 —— 而且不能仍被标成 NONE。
	f.advance(3601)
	after, err := callGetFeature(f, "user_play_finish_7d", 0, false)
	if err != nil {
		t.Fatalf("GetFeature after TTL: %v", err)
	}
	if after.GetEntry().GetDegradation() == rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE {
		t.Fatal("a row past expire_at was served as a fresh value")
	}
	if after.GetFound() {
		t.Error("found=true for an expired row with no previous version")
	}
}
