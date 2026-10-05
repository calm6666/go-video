// 测试域 7：主体自助导出（ListEntityFeatures）。
//
// 这是「主体看到自己被存了什么」的合规读路径，危险只有一种：**静默少给**。
// 少一行、少一列、把读失败当成「这个主体没有特征」，调用方都无法与「确实没存」区分，
// 所以本域钉的是：
//  1. 入参非法必须在碰到任何存储之前显式报错（契约写明了 ps 上限，越界只能拒绝不能夹取）；
//  2. 单条读失败（定义缺失、值列解码不出来、批量取定义本身失败）必须是错误，
//     绝不退化成一条空 entries 的「干净」应答；
//  3. 新鲜度与可见性按实现如实表达：只导出 ACTIVE 指针指向的那一版、过期行带 EXPIRED
//     标记而不是被悄悄丢掉；
//  4. 隐私下限（调用方兴趣）与导出封顶（服务端配置）是两条彼此独立的谓词；
//  5. 返回的条目与被导出的那一行一一对应：键、版本、值、TTL、产出/到期时刻、口径键、
//     主体逐项可核对，键集合或顺序一旦错就必然留下可见差异。
//
// 顺序类期望的事实源是 model 的 SQL：
//   - model/featurevalue.go:880 `ORDER BY v.feature_key ASC, v.version ASC LIMIT ? OFFSET ?`
//   - model/featurevalue.go:809-812 的三表 JOIN（隐私级别取自定义列、生效指针、state=ACTIVE）
//   - model/featurevalue.go:818-840 privacyClause / privacyMaxClause（两条独立列条件）
//
// 实现与注释不一致的地方（坏配置反而放宽可见范围）在下面的用例里钉成「观察到的行为」。
package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"google.golang.org/protobuf/proto"
)

// --- 本域共用的调用与断言小工具 ---

func exportReq(entity *rpc.EntityRef, minPrivacy, pn, ps int32) *rpc.ListEntityFeaturesReq {
	return &rpc.ListEntityFeaturesReq{
		Entity:          entity,
		MinPrivacyLevel: rpc.PrivacyLevel(minPrivacy),
		Pn:              pn,
		Ps:              ps,
	}
}

func callExport(f *fixture, in *rpc.ListEntityFeaturesReq) (*rpc.ListEntityFeaturesReply, error) {
	return NewListEntityFeaturesLogic(f.ctx, f.ServiceContext).ListEntityFeatures(in)
}

// payloadText 把中立值压成一行文本。
// 值必须整列参与比对，而不是只比 int64：本域的种子既有标量也有向量列，
// 只比标量会让「列表行被换成空列表 / 串成别人的列表」这类错位悄悄通过。
func payloadText(p model.ValuePayload) string {
	switch p.ValueType {
	case model.ValueTypeInt64:
		return fmt.Sprintf("i64=%d", p.Int64Value)
	case model.ValueTypeDouble:
		return fmt.Sprintf("f64=%g", p.DoubleValue)
	case model.ValueTypeBool:
		return fmt.Sprintf("bool=%v", p.BoolValue)
	case model.ValueTypeString:
		return fmt.Sprintf("str=%q", p.StringValue)
	case model.ValueTypeInt64List:
		return fmt.Sprintf("i64list=%v", p.Int64List)
	case model.ValueTypeDoubleList:
		return fmt.Sprintf("f64list=%v", p.DoubleList)
	default:
		return fmt.Sprintf("type=%d", p.ValueType)
	}
}

// entrySignature 把一个条目压成一行文本：值来自哪一行必须由这一行自证。
// 逐字段比对而不是只比键名——键集合错、顺序错、把历史版本的时刻/口径带错、
// 用请求维度代替行维度，都会在这一行上留下可见差异。
// 值走生产的 payloadFromProto：条目里的值列一旦编不出来，用例直接 Fatal，
// 而不是让比对退化。
func entrySignature(t *testing.T, e *rpc.FeatureEntry) string {
	t.Helper()
	p, err := payloadFromProto(e.GetValue())
	if err != nil {
		t.Fatalf("%s@v%d: entry value does not decode: %v",
			e.GetFeature().GetFeatureKey(), e.GetResolvedVersion(), err)
	}
	return fmt.Sprintf("%s@v%d|%s|ttl=%d|et=%d|exp=%d|deg=%d|src=%s|ent=%d/%s",
		e.GetFeature().GetFeatureKey(), e.GetResolvedVersion(), payloadText(p),
		e.GetTtlSeconds(), e.GetEventTime(), e.GetExpireAt(), int32(e.GetDegradation()),
		e.GetSourceMetricKey(), int32(e.GetEntity().GetEntityScope()), e.GetEntity().GetEntityId())
}

// wantSignature 从「库里真实存在的行 + 该行对应的定义」组出期望文本。
// 期望值来自种子事实而不是手抄常量：种子写错了会立刻报出来，不会让断言空转。
// ttl 取定义列（新鲜度口径的定义方），其余取行列（值与时刻的事实方），
// 两者各有归属：把 ttl 写成行上的常量就测不出「口径来自定义」。
func wantSignature(f *fixture, row *model.FeatureValue, deg rpc.FeatureDegradation) string {
	f.t.Helper()
	p, err := row.Payload()
	if err != nil {
		f.t.Fatalf("test premise broken: seeded row %s@v%d does not decode: %v",
			row.FeatureKey, row.Version, err)
	}
	def, ok := f.defs.rows[model.DefinitionKey{FeatureKey: row.FeatureKey, Version: row.Version}]
	if !ok {
		f.t.Fatalf("test premise broken: no definition stored for %s@v%d",
			row.FeatureKey, row.Version)
	}
	return fmt.Sprintf("%s@v%d|%s|ttl=%d|et=%d|exp=%d|deg=%d|src=%s|ent=%d/%s",
		row.FeatureKey, row.Version, payloadText(p), def.TTLSeconds, row.EventTime, row.ExpireAt,
		int32(deg), row.SourceMetricKey, row.EntityScope, row.EntityID)
}

func exportSignatureOf(f *fixture, rows []*model.FeatureValue, deg rpc.FeatureDegradation) string {
	f.t.Helper()
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, wantSignature(f, r, deg))
	}
	return strings.Join(parts, " || ")
}

func replySignature(t *testing.T, reply *rpc.ListEntityFeaturesReply) string {
	t.Helper()
	parts := make([]string, 0, len(reply.GetEntries()))
	for _, e := range reply.GetEntries() {
		parts = append(parts, entrySignature(t, e))
	}
	return strings.Join(parts, " || ")
}

// assertReadModelShape 钉住「导出只走那一条 JOIN 读，不点查、不回退到在线读路径」。
// 逐条 FindOne / 重解 ACTIVE 指针都会把一致性视图拆散（隐私级别与指针必须与值同快照）。
func assertReadModelShape(t *testing.T, f *fixture) {
	t.Helper()
	if n := countCalled(f.values.calls, "values.FindOne"); n != 0 {
		t.Errorf("values.FindOne calls=%d, want 0: 导出是一次 JOIN 读，不是逐行点查", n)
	}
	if n := countCalled(f.values.calls, "values.FindEntries"); n != 0 {
		t.Errorf("values.FindEntries calls=%d, want 0: 在线读的回源键集合不适用于主体视图", n)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.ListByKeys"); n != 0 {
		t.Errorf("pointer re-resolution calls=%d, want 0: 生效版本必须与值同快照", n)
	}
	if n := countCalled(f.defs.calls, "definitions.FindOne"); n != 0 {
		t.Errorf("definitions.FindOne calls=%d, want 0: 定义只能批量取，否则 N+1", n)
	}
}

// int64ListSeed 造一对「定义默认值文本 / 载荷切片」，长度即维度。
// 默认值文本与维度必须自洽，否则 newDef 的契约校验会 panic（那是种子的错，不是被测代码的错）。
func int64ListSeed(v int64, dimension int) ([]int64, string) {
	vals := make([]int64, dimension)
	parts := make([]string, dimension)
	for i := range vals {
		vals[i] = v
		parts[i] = fmt.Sprintf("%d", v)
	}
	return vals, strings.Join(parts, ",")
}

// --- 守卫与守卫顺序：被拒的请求一次存储都不许碰 ---

func TestListEntityFeaturesGuardOrderTouchesNothing(t *testing.T) {
	ok := func() *rpc.ListEntityFeaturesReq {
		return exportReq(midRef(testMid), 0, 1, 10)
	}
	patched := func(mut func(*rpc.ListEntityFeaturesReq)) *rpc.ListEntityFeaturesReq {
		r := ok()
		mut(r)
		return r
	}
	cases := []struct {
		name string
		in   *rpc.ListEntityFeaturesReq
		want error
	}{
		{"没有主体", exportReq(nil, 0, 1, 10), model.ErrEntityScopeRequired},
		{"主体维度未声明", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity = &rpc.EntityRef{EntityId: testMid}
		}), model.ErrEntityScopeRequired},
		{"主体维度越界", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity = &rpc.EntityRef{EntityScope: rpc.EntityScope(model.EntityScopeIPHash + 1),
				EntityId: testMid}
		}), model.ErrEntityScopeRequired},
		// 隐私闸门在 SQL 之前：明文设备号与原始 IP 连查询条件都不该变成。
		{"明文设备号", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity = &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE,
				EntityId: plaintextDeviceSerial}
		}), model.ErrEntityIDInvalid},
		{"原始 IP", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity = &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_IP_HASH,
				EntityId: rawIPv4}
		}), model.ErrEntityIDInvalid},
		{"mid 非数字", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity = midRef("mid-10086")
		}), model.ErrEntityIDInvalid},
		{"pn 从 0 起", patched(func(r *rpc.ListEntityFeaturesReq) { r.Pn = 0 }),
			model.ErrLimitTooLarge},
		{"pn 为负", patched(func(r *rpc.ListEntityFeaturesReq) { r.Pn = -1 }),
			model.ErrLimitTooLarge},
		{"ps 为 0", patched(func(r *rpc.ListEntityFeaturesReq) { r.Ps = 0 }),
			model.ErrLimitTooLarge},
		// 契约写的是「ps 上限 100」（rpc/featurestore.proto 的 ps 注释），越界只能报错：
		// 夹取等于调用方以为拿到了一整页，实际少给。
		{"ps 越界则拒绝而非夹取", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Ps = model.MaxListPageSize + 1
		}), model.ErrLimitTooLarge},
		{"隐私下限未声明", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.MinPrivacyLevel = rpc.PrivacyLevel(model.PrivacyUserProfile + 1)
		}), model.ErrPrivacyUnsetNotAllowed},
		{"隐私下限为负", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.MinPrivacyLevel = rpc.PrivacyLevel(-3)
		}), model.ErrPrivacyUnsetNotAllowed},
		// 顺序判别①：主体闸门先于分页（两者都坏时报主体错）。
		// 若把 ValidatePageSize 提到最前，这条期望 ErrEntityScopeRequired 的用例会变红。
		{"主体先于分页", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Entity, r.Ps = nil, 0
		}), model.ErrEntityScopeRequired},
		// 顺序判别②：分页先于隐私下限（两者都坏时报分页错）。
		{"分页先于隐私下限", patched(func(r *rpc.ListEntityFeaturesReq) {
			r.Ps, r.MinPrivacyLevel = 0, rpc.PrivacyLevel(model.PrivacyUserProfile+1)
		}), model.ErrLimitTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			// 库里确有数据：只有这样「零 ops」才等于「被守卫挡住」，而不是本来就查不到。
			d := f.registerActive(newDef("u_play_finish_7d", 1))
			f.putInt64(d, 1, testMid, 7, f.nowUnix()-60)

			reply, err := callExport(f, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			if reply != nil {
				t.Errorf("reply=%+v, want nil alongside the rejection", reply)
			}
			assertTouchedNothing(t, f)
		})
	}
}

// --- 可见性：只导出当前对外生效的那一版 ---

func TestListEntityFeaturesExportsOnlyTheActivelyServedVersion(t *testing.T) {
	f := newFixture(t)
	// 同一个 key 的两版都有值，指针指向 v2：v1 的残值不能出现在「当前生效」视图里。
	v1 := f.putDef(newDef("u_play_finish_7d", 1))
	v2 := f.putDef(newDef("u_play_finish_7d", 2))
	f.putPointer("u_play_finish_7d", 2, 1)
	staleRow := f.putInt64(v1, 1, testMid, 11, f.nowUnix()-60)
	servingRow := f.putInt64(v2, 2, testMid, 99, f.nowUnix()-60)

	// DRAFT 版本有值有指针：state 条件（d.state=ACTIVE）把它挡在外面。
	draft := f.putDef(newDef("u_draft_feature_7d", 1, withState(model.FeatureStateDraft)))
	f.putPointer(draft.FeatureKey, 1, 0)
	f.putInt64(draft, 1, testMid, 21, f.nowUnix()-60)

	// RETIRED 版本：契约禁止向它写入，残值只能绕过构造函数塞进来（与真库的历史残值同形）。
	retired := f.putDef(newDef("u_retired_feature_7d", 1, withState(model.FeatureStateRetired)))
	f.putPointer(retired.FeatureKey, 1, 0)
	f.putRawValue(&model.FeatureValue{
		FeatureKey: retired.FeatureKey, Version: 1, EntityScope: model.EntityScopeMid,
		EntityID: testMid, ValueType: model.ValueTypeInt64, Int64Value: 31,
		EventTime: f.nowUnix() - 60, ExpireAt: f.nowUnix() + 3540,
		SourceMetricKey: testSourceMetricKey,
	})

	// 定义 ACTIVE 但没有指针行：注册未走完，JOIN 不到生效版本，不能导出。
	orphans := f.putDef(newDef("u_no_pointer_7d", 1))
	f.putInt64(orphans, 1, testMid, 41, f.nowUnix()-60)

	// 同一特征、别人的主体 / 别的主体维度：导出范围以 (entity_scope, entity_id) 为界。
	f.putInt64(v2, 2, "10087", 51, f.nowUnix()-60)
	aidDef := f.registerActive(newDef("a_item_heat_7d", 1, withScope(model.EntityScopeAid),
		withPrivacy(model.PrivacyContentAttribute)))
	f.putInt64(aidDef, 1, "555", 61, f.nowUnix()-60)

	// 前提核对：上面几类「不该出现」的行确实都躺在库里，
	// 否则「只出一条」可能只是因为没播种。
	if len(f.values.rows) != 7 {
		t.Fatalf("seeded rows=%d, want 7 (本用例的全部差异都来自可见性判定而不是数据缺失)",
			len(f.values.rows))
	}
	if _, ok := f.valueRow(staleRow.Key()); !ok {
		t.Fatal("test premise broken: the superseded v1 row must exist")
	}

	reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 100))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := exportSignatureOf(f, []*model.FeatureValue{servingRow},
		rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
	if got := replySignature(t, reply); got != want {
		t.Errorf("exported entries=\n%s\nwant only the actively served row:\n%s", got, want)
	}
	if reply.GetTotal() != 1 {
		t.Errorf("total=%d, want 1: 被可见性条件滤掉的行不能计入总数", reply.GetTotal())
	}
	seen := f.values.listByEntitySeen
	if len(seen) != 1 || seen[0].entityScope != model.EntityScopeMid || seen[0].entityID != testMid {
		t.Fatalf("ListByEntity predicate calls=%+v, want exactly one MID/%s read", seen, testMid)
	}
	assertReadModelShape(t, f)
}

// --- 隐私：下限是调用方兴趣，封顶来自服务端配置，两条谓词互不覆盖 ---

func TestListEntityFeaturesPrivacyFloorAndCeilingAreSeparatePredicates(t *testing.T) {
	const pseudoKey, profileKey = "u_pseudo_7d", "u_profile_7d"
	setup := func(t *testing.T) (*fixture, *model.FeatureValue, *model.FeatureValue) {
		f := newFixture(t)
		pseudo := f.registerActive(newDef(pseudoKey, 1, withPrivacy(model.PrivacyPseudonymous)))
		profile := f.registerActive(newDef(profileKey, 1, withPrivacy(model.PrivacyUserProfile)))
		low := f.putInt64(pseudo, 1, testMid, 11, f.nowUnix()-60)
		high := f.putInt64(profile, 1, testMid, 22, f.nowUnix()-60)
		return f, low, high
	}
	// 播种顺序是「先 PSEUDONYMOUS 再 USER_PROFILE」，而 SQL 的 ORDER BY feature_key ASC
	// 把 u_profile_7d 排在 u_pseudo_7d 之前（第 4 个字符 'r'(0x72) < 's'(0x73)）。
	// 期望串因此按 SQL 事实源写死而不是照抄播种顺序：任何「按插入顺序返回」的实现
	// 都会在这里留下可见差异。
	both := func(t *testing.T, f *fixture, low, high *model.FeatureValue) string {
		t.Helper()
		return exportSignatureOf(f, []*model.FeatureValue{high, low},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
	}

	t.Run("不填下限即只受封顶约束", func(t *testing.T) {
		f, low, high := setup(t)
		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if got, want := replySignature(t, reply), both(t, f, low, high); got != want {
			t.Errorf("entries=\n%s\nwant\n%s", got, want)
		}
		// 0 是「不加下限条件」而不是「只出级别 0 的行」：条件里那一列整个省掉。
		if c := f.values.listByEntitySeen[0]; c.minPrivacy != 0 ||
			c.maxPrivacy != model.PrivacyUserProfile {
			t.Errorf("privacy predicate = %+v, want min 0 (不加条件) + 服务端封顶 %d",
				c, model.PrivacyUserProfile)
		}
	})

	t.Run("收紧封顶只影响可见行而不改动下限", func(t *testing.T) {
		f, low, high := setup(t)
		f.withExportMaxPrivacy(model.PrivacyPseudonymous)
		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export with tightened ceiling: %v", err)
		}
		want := exportSignatureOf(f, []*model.FeatureValue{low},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
		if got := replySignature(t, reply); got != want {
			t.Errorf("entries=\n%s, want only the PSEUDONYMOUS row (封顶以上不外导):\n%s", got, want)
		}
		if high.FeatureKey != profileKey {
			t.Fatalf("premise broken: 高隐私行没入库，got %q want %q", high.FeatureKey, profileKey)
		}
		if c := f.values.listByEntitySeen[0]; c.minPrivacy != 0 ||
			c.maxPrivacy != model.PrivacyPseudonymous {
			t.Errorf("privacy predicate = %+v, want min 0 + max %d (封顶来自配置)",
				c, model.PrivacyPseudonymous)
		}
	})

	// 互换判别：若实现把两列合并成一列（用封顶覆盖下限或反之），
	// 期望的「两行都有」会立刻掉一行。
	t.Run("下限与封顶同向时两行都出", func(t *testing.T) {
		f, low, high := setup(t)
		reply, err := callExport(f, exportReq(midRef(testMid), model.PrivacyPseudonymous, 1, 10))
		if err != nil {
			t.Fatalf("export with floor 3 and ceiling 4: %v", err)
		}
		if got, want := replySignature(t, reply), both(t, f, low, high); got != want {
			t.Errorf("entries=\n%s\nwant\n%s", got, want)
		}
		if c := f.values.listByEntitySeen[0]; c.minPrivacy != model.PrivacyPseudonymous ||
			c.maxPrivacy != model.PrivacyUserProfile {
			t.Errorf("privacy predicate = %+v, want min 3 / max 4 两列各自下传", c)
		}
	})

	t.Run("下限落在单档时只出该档", func(t *testing.T) {
		f, _, high := setup(t)
		reply, err := callExport(f, exportReq(midRef(testMid), model.PrivacyUserProfile, 1, 10))
		if err != nil {
			t.Fatalf("export with floor 4: %v", err)
		}
		want := exportSignatureOf(f, []*model.FeatureValue{high},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
		if got := replySignature(t, reply); got != want {
			t.Errorf("entries=\n%s, want only the USER_PROFILE row:\n%s", got, want)
		}
	})
}

// TestListEntityFeaturesFloorAboveCeilingIsAnEmptyReplyWithNoRead 钉住实现的选择：
// 请求下限高于本环境封顶时，返回**空应答**（不是放宽封顶、也不是伪装成不存在），
// 并且一次存储都不碰。
//
// 现状缺口（见交付说明）：应答里没有任何字段能区分「这一档在本环境不对外导出」与
// 「这个主体确实没被存过」。合规核对方拿到的就是 total=0 的空页，
// 而库里明明有主体的 USER_PROFILE 行。若日后补上显式错误或表达字段，
// 下面 entries/total 两条断言必须一起改。
func TestListEntityFeaturesFloorAboveCeilingIsAnEmptyReplyWithNoRead(t *testing.T) {
	f := newFixture(t).withExportMaxPrivacy(model.PrivacyPseudonymous)
	profile := f.registerActive(newDef("u_profile_7d", 1, withPrivacy(model.PrivacyUserProfile)))
	blocked := f.putInt64(profile, 1, testMid, 22, f.nowUnix()-60)
	if _, ok := f.valueRow(blocked.Key()); !ok {
		t.Fatal("test premise broken: the subject does have a USER_PROFILE row stored")
	}

	reply, err := callExport(f, exportReq(midRef(testMid), model.PrivacyUserProfile, 1, 10))
	if err != nil {
		t.Fatalf("floor above ceiling must not be an error in the current implementation: %v", err)
	}
	if len(reply.GetEntries()) != 0 || reply.GetTotal() != 0 {
		t.Errorf("reply entries=%d total=%d, want the observed empty answer (0/0)",
			len(reply.GetEntries()), reply.GetTotal())
	}
	// 「没有存储调用」是本用例的核心：这一档根本不进 SQL。
	assertTouchedNothing(t, f)

	// 判别对照：同一份配置、下限落在封顶之内时，行是读得到的
	// （空应答确实来自这道闸门，而不是谓词本身查不到）。
	f2 := newFixture(t).withExportMaxPrivacy(model.PrivacyPseudonymous)
	pseudo := f2.registerActive(newDef("u_pseudo_7d", 1, withPrivacy(model.PrivacyPseudonymous)))
	row := f2.putInt64(pseudo, 1, testMid, 11, f.nowUnix()-60)
	reply2, err := callExport(f2, exportReq(midRef(testMid), model.PrivacyPseudonymous, 1, 10))
	if err != nil {
		t.Fatalf("floor inside the ceiling: %v", err)
	}
	want := exportSignatureOf(f2, []*model.FeatureValue{row},
		rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
	if got := replySignature(t, reply2); got != want {
		t.Errorf("entries=\n%s\nwant\n%s", got, want)
	}
}

// TestListEntityFeaturesBadExportCeilingFallsOpenToTheMostSensitiveLevel 钉住坏配置的处置：
// listentityfeatureslogic.go:56 的注释写的是「导出可见范围不能因为一个坏配置变成全部可见」，
// 实现收敛到的却是 PrivacyUserProfile(4)——四档里的最高敏感级别，也就是**全部可见**。
// 这里按观察到的行为断言（0 与 9 两种非法值都放宽到 4），不是断言期望值。
// 对照：一个「合法但更严」的级别（1）会如实收紧，所以放宽只发生在越界这一档。
//
// 顺带一条启动期事实：internal/config/config.go:186 只在
// 「值非 0 且不合法」时报错，所以 0（未声明）能通过启动校验、到这里才被放宽。
func TestListEntityFeaturesBadExportCeilingFallsOpenToTheMostSensitiveLevel(t *testing.T) {
	for _, bad := range []int32{model.PrivacyUnspecified, model.PrivacyUserProfile + 5} {
		t.Run(fmt.Sprintf("越界封顶%d", bad), func(t *testing.T) {
			f := newFixture(t).withExportMaxPrivacy(bad)
			pseudo := f.registerActive(newDef("u_pseudo_7d", 1,
				withPrivacy(model.PrivacyPseudonymous)))
			profile := f.registerActive(newDef("u_profile_7d", 1,
				withPrivacy(model.PrivacyUserProfile)))
			low := f.putInt64(pseudo, 1, testMid, 11, f.nowUnix()-60)
			high := f.putInt64(profile, 1, testMid, 22, f.nowUnix()-60)

			reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
			if err != nil {
				t.Fatalf("export with ceiling %d: %v", bad, err)
			}
			// 顺序同上一条用例的事实源：u_profile_7d 在 u_pseudo_7d 之前。
			want := exportSignatureOf(f, []*model.FeatureValue{high, low},
				rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
			if got := replySignature(t, reply); got != want {
				t.Errorf("entries=\n%s, want BOTH rows: 观察到的行为是越界封顶收敛到 4（全部可见）\nwant\n%s",
					got, want)
			}
			if c := f.values.listByEntitySeen[0]; c.maxPrivacy != model.PrivacyUserProfile {
				t.Errorf("ceiling passed down=%d, want the observed convergence to %d",
					c.maxPrivacy, model.PrivacyUserProfile)
			}
		})
	}

	t.Run("对照：合法的低封顶如实收紧", func(t *testing.T) {
		f := newFixture(t).withExportMaxPrivacy(model.PrivacyPublicAggregate)
		profile := f.registerActive(newDef("u_profile_7d", 1, withPrivacy(model.PrivacyUserProfile)))
		f.putInt64(profile, 1, testMid, 22, f.nowUnix()-60)

		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export with ceiling 1: %v", err)
		}
		if len(reply.GetEntries()) != 0 {
			t.Fatalf("entries=%d, want 0 with a declared ceiling of 1: %s",
				len(reply.GetEntries()), replySignature(t, reply))
		}
		// 与越界档的差别必须看得见：合法低封顶真的下传了 1，并且查过库。
		if c := f.values.listByEntitySeen[0]; c.maxPrivacy != model.PrivacyPublicAggregate {
			t.Errorf("ceiling passed down=%d, want the configured 1", c.maxPrivacy)
		}
	})
}

// --- 条目与行一一对应：顺序、字段、分页、批量取定义的形状 ---

func TestListEntityFeaturesRowsMapOneToOneToTheExportedDefinitions(t *testing.T) {
	f := newFixture(t)
	// 播种顺序刻意与 SQL 的 ORDER BY（feature_key ASC, version ASC）不同，
	// 且字节序里 '_'(0x5F) < 'a'(0x61)，所以 u_a_first 排在 u_aa_second 之前：
	// 任何「按插入顺序返回」或「键集合错位」都在这条期望串上留下可见差异。
	wantKeys := []string{"u_a_first|1", "u_aa_second|1", "u_b_third|1", "u_c_fourth|2"}
	defs := map[string]*model.FeatureDefinition{
		"u_a_first":   f.registerActive(newDef("u_a_first", 1, withTTL(600), withDefault("1"))),
		"u_aa_second": f.registerActive(newDef("u_aa_second", 1, withTTL(1200), withDefault("2"))),
		"u_b_third":   f.registerActive(newDef("u_b_third", 1, withTTL(1800), withDefault("3"))),
		"u_c_fourth":  f.registerActive(newDef("u_c_fourth", 2, withTTL(2400), withDefault("4"))),
		// 同主体维度、另一个特征、别人的主体：必须不在本页，也不能被顺手取定义。
		"u_z_other": f.registerActive(newDef("u_z_other", 1, withTTL(900))),
	}
	rows := map[string]*model.FeatureValue{
		"u_a_first":   f.putInt64(defs["u_a_first"], 1, testMid, 10, f.nowUnix()-120),
		"u_b_third":   f.putInt64(defs["u_b_third"], 1, testMid, 30, f.nowUnix()-240),
		"u_c_fourth":  f.putInt64(defs["u_c_fourth"], 2, testMid, 40, f.nowUnix()-360),
		"u_aa_second": f.putInt64(defs["u_aa_second"], 1, testMid, 20, f.nowUnix()-480),
		"u_z_other":   f.putInt64(defs["u_z_other"], 1, "10087", 90, f.nowUnix()-60),
	}
	ordered := []*model.FeatureValue{rows["u_a_first"], rows["u_aa_second"],
		rows["u_b_third"], rows["u_c_fourth"]}
	wantAll := exportSignatureOf(f, ordered, rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)

	// 整页一次拿全：条目顺序与期望串逐项相等。
	full, err := callExport(f, exportReq(midRef(testMid), 0, 1, 100))
	if err != nil {
		t.Fatalf("export full page: %v", err)
	}
	if got := replySignature(t, full); got != wantAll {
		t.Errorf("full page entries=\n%s\nwant (ORDER BY v.feature_key ASC, v.version ASC):\n%s",
			got, wantAll)
	}
	if full.GetTotal() != 4 {
		t.Errorf("total=%d, want the filtered 4 rather than the 5 stored rows", full.GetTotal())
	}
	for _, e := range full.GetEntries() {
		assertEntryStatesItsVersion(t, e)
		if int32(e.GetEntity().GetEntityScope()) != model.EntityScopeMid ||
			e.GetEntity().GetEntityId() != testMid {
			t.Errorf("%s: entry echoes entity %+v, want the requested MID/%s",
				e.GetFeature().GetFeatureKey(), e.GetEntity(), testMid)
		}
	}

	// 定义批量读的键集合 = 本页行的键集合，一个不多一个不少。
	if n := countCalled(f.defs.calls, "definitions.ListByKeys"); n != 1 {
		t.Fatalf("definitions.ListByKeys calls=%d, want 1 (本页一次批量取)", n)
	}
	var gotKeys []string
	for _, chunk := range f.defs.listByKeysSeen {
		for _, k := range chunk {
			gotKeys = append(gotKeys, k.FeatureKey+"|"+int32Text(k.Version))
		}
	}
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("definition keys=%v, want the page's own %v (别人的特征不能顺手多取)",
			gotKeys, wantKeys)
	}
	if n := countCalled(f.values.calls, "values.ListByEntity"); n != 1 {
		t.Errorf("values.ListByEntity calls=%d, want 1", n)
	}
	assertReadModelShape(t, f)

	// 分页拼接：OFFSET = (pn-1)*ps 的语义由「两页拼起来等于整页、且不重不漏」判别。
	// 谓词比对从 base 起算：上面整页那一次读也在同一张记录表里（第 0 条是 ps=100 的那次），
	// 直接按 pn-1 取会读到别人的调用，那条断言就会空转。
	base := len(f.values.listByEntitySeen)
	if base != 1 {
		t.Fatalf("recorded ListByEntity calls=%d before paging, want 1 (整页那一次)", base)
	}
	var paged []string
	for pn := int32(1); pn <= 2; pn++ {
		reply, err := callExport(f, exportReq(midRef(testMid), 0, pn, 2))
		if err != nil {
			t.Fatalf("page %d: %v", pn, err)
		}
		if reply.GetTotal() != 4 {
			t.Errorf("page %d total=%d, want 4: total 是过滤后的总数，与本页条数无关", pn,
				reply.GetTotal())
		}
		if len(reply.GetEntries()) != 2 {
			t.Errorf("page %d entries=%d, want the requested ps=2", pn, len(reply.GetEntries()))
		}
		for _, e := range reply.GetEntries() {
			paged = append(paged, entrySignature(t, e))
		}
		if c := f.values.listByEntitySeen[base+int(pn)-1]; c.pn != pn || c.ps != 2 {
			t.Errorf("page %d predicate = %+v, want pn/ps 原样下传", pn, c)
		}
	}
	if strings.Join(paged, " || ") != wantAll {
		t.Errorf("pages concatenated to\n%s\nwant\n%s", strings.Join(paged, " || "), wantAll)
	}

	// 页码越界：空页 + 真实 total，而不是回绕到第一页。
	past, err := callExport(f, exportReq(midRef(testMid), 0, 3, 2))
	if err != nil {
		t.Fatalf("page past the end: %v", err)
	}
	if len(past.GetEntries()) != 0 {
		t.Errorf("page past the end returned %d entries, want 0 (不回绕)", len(past.GetEntries()))
	}
	if past.GetTotal() != 4 {
		t.Errorf("page past the end total=%d, want 4 so the caller can see it is not the end of data",
			past.GetTotal())
	}

	// 主体标识两侧空白被归一化后才是查询条件：否则「同一个主体」会因为条件不同而查不到。
	padBase := len(f.values.listByEntitySeen)
	padded, err := callExport(f, exportReq(midRef("  "+testMid+"  "), 0, 1, 100))
	if err != nil {
		t.Fatalf("export with a padded entity id: %v", err)
	}
	if got := replySignature(t, padded); got != wantAll {
		t.Errorf("padded-entity entries=\n%s\nwant\n%s", got, wantAll)
	}
	if c := f.values.listByEntitySeen[padBase]; c.entityID != testMid {
		t.Errorf("entity_id passed down=%q, want the normalized %q", c.entityID, testMid)
	}
}

// TestListEntityFeaturesChunksDefinitionReadBeyondTheBatchLimit 钉住两个上限的分歧：
// 页大小上限 100（model.MaxListPageSize）大于定义批量上限 50（model.MaxBatchFeatures），
// 所以整页必须切块批量取；「顺手把页夹到 50」是静默截断，会在这里变红。
func TestListEntityFeaturesChunksDefinitionReadBeyondTheBatchLimit(t *testing.T) {
	f := newFixture(t)
	const n = model.MaxListPageSize // 一整页 100 行 > 50，必须切成两块
	seeded := make([]*model.FeatureValue, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("u_chunk_probe_%03d", i)
		def := f.registerActive(newDef(key, 1, withTTL(600)))
		seeded = append(seeded, f.putInt64(def, 1, testMid, int64(i), f.nowUnix()-60))
	}

	reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, model.MaxListPageSize))
	if err != nil {
		t.Fatalf("export of a full page: %v", err)
	}
	if len(reply.GetEntries()) != n || reply.GetTotal() != int64(n) {
		t.Fatalf("entries=%d total=%d, want the whole %d-row page (夹取或截断都会在这里变红)",
			len(reply.GetEntries()), reply.GetTotal(), n)
	}
	want := exportSignatureOf(f, seeded, rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
	if got := replySignature(t, reply); got != want {
		t.Errorf("page signature mismatch:\ngot  %.300s\nwant %.300s", got, want)
	}
	if c := f.values.listByEntitySeen[0]; c.ps != model.MaxListPageSize {
		t.Errorf("ps passed down=%d, want the requested %d verbatim", c.ps, model.MaxListPageSize)
	}

	// 块大小：<= MaxBatchFeatures 一块，替身的 ListByKeys 在超限时回 ErrTooManyEntries，
	// 所以「不切块」的实现会直接把整页变成错误而不是静默少给。
	chunks := f.defs.listByKeysSeen
	if len(chunks) != 2 {
		t.Fatalf("definition batch reads=%d, want 2 (100 键 / 50 一块)", len(chunks))
	}
	if len(chunks[0]) != model.MaxBatchFeatures || len(chunks[1]) != n-model.MaxBatchFeatures {
		t.Errorf("chunk sizes=%d/%d, want %d/%d", len(chunks[0]), len(chunks[1]),
			model.MaxBatchFeatures, n-model.MaxBatchFeatures)
	}
	total := 0
	for i, c := range chunks {
		if len(c) > model.MaxBatchFeatures {
			t.Errorf("chunk %d has %d keys > %d", i, len(c), model.MaxBatchFeatures)
		}
		total += len(c)
	}
	if total != n {
		t.Errorf("definition keys fetched=%d, want one per exported row (%d)", total, n)
	}
	if n := countCalled(f.values.calls, "values.ListByEntity"); n != 1 {
		t.Errorf("values.ListByEntity calls=%d, want 1: 切块只作用于定义批量读", n)
	}
	assertReadModelShape(t, f)
}

// --- 单条读失败必须是错误，不能是「干净」的空应答 ---

func TestListEntityFeaturesReadFailuresAreErrorsNotAnEmptyPage(t *testing.T) {
	boom := errors.New("dial tcp 127.0.0.1:3306: connectex: no connection")

	t.Run("主存读失败原样上抛", func(t *testing.T) {
		f := newFixture(t)
		def := f.registerActive(newDef("u_play_finish_7d", 1))
		f.putInt64(def, 1, testMid, 7, f.nowUnix()-60)
		f.values.errListByEntity = boom

		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		// 「查不到」与「查失败」必须能区分：这里拿到的是同一个原始 error，不是空页。
		if err != boom {
			t.Fatalf("err=%v, want the identical raw storage error", err)
		}
		if reply != nil {
			t.Errorf("reply=%+v, want nil alongside the error", reply)
		}
		if n := countCalled(f.defs.calls, "definitions.ListByKeys"); n != 0 {
			t.Errorf("definitions.ListByKeys calls=%d, want 0: 行都没拿到就不该再取定义", n)
		}
	})

	t.Run("定义批量读失败带上下文上抛", func(t *testing.T) {
		f := newFixture(t)
		def := f.registerActive(newDef("u_play_finish_7d", 1))
		f.putInt64(def, 1, testMid, 7, f.nowUnix()-60)
		f.defs.errListByKeys = boom

		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		if !errors.Is(err, boom) {
			t.Fatalf("err=%v, want it to wrap the raw storage error", err)
		}
		if err == boom {
			t.Errorf("err must carry which step failed, got the bare error %v", err)
		}
		if !strings.Contains(err.Error(), "load definitions for export") {
			t.Errorf("err=%v, want the failing step named so日志能定位是哪一次读", err)
		}
		if reply != nil {
			t.Errorf("reply=%+v, want nil", reply)
		}
	})

	t.Run("有值却无定义是数据损坏而不是空集", func(t *testing.T) {
		f := newFixture(t)
		def := f.registerActive(newDef("u_play_finish_7d", 1))
		f.putInt64(def, 1, testMid, 7, f.nowUnix()-60)
		f.putInt64(def, 1, "10087", 8, f.nowUnix()-60)
		// 注入缝（替身已有）：库里这一行存在，但批量取定义时读不到 ——
		// 真库对应两次读之间的并发删除或索引不一致，其余语义下这条分支不可达
		// （替身的 entityRows 与 ListByKeys 读的是同一张 map）。
		lookupKey := model.DefinitionKey{FeatureKey: def.FeatureKey, Version: 1}
		f.defs.omitFromListByKeys = map[model.DefinitionKey]bool{lookupKey: true}
		if _, ok := f.defs.rows[lookupKey]; !ok {
			t.Fatal("test premise broken: the definition row must exist for the injection to mean anything")
		}

		reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
		// 没有定义就没有口径，返回裸值等于让人把含义不明的数当特征用。
		if !errors.Is(err, model.ErrFeatureNotFound) {
			t.Fatalf("err=%v, want %v", err, model.ErrFeatureNotFound)
		}
		if !strings.Contains(err.Error(), "u_play_finish_7d@v1") {
			t.Errorf("err=%v, want the offending feature@version named", err)
		}
		if reply != nil {
			t.Errorf("reply=%+v, want nil: 一条行读不到定义时不能带着其余行当成功", reply)
		}
	})

	t.Run("值列解码失败不伪装成 0 值", func(t *testing.T) {
		cases := []struct {
			name string
			want error
		}{
			{"未声明的 value_type", model.ErrMalformedValue},
			{"向量列内容坏掉", model.ErrMalformedListValue},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				f := newFixture(t)
				switch c.name {
				case "未声明的 value_type":
					def := f.registerActive(newDef("u_dirty_scalar_7d", 1, withTTL(3600)))
					f.putRawValue(&model.FeatureValue{
						FeatureKey: def.FeatureKey, Version: 1, EntityScope: model.EntityScopeMid,
						EntityID: testMid, ValueType: model.ValueTypeUnspecified, Int64Value: 55,
						EventTime: f.nowUnix() - 60, ExpireAt: f.nowUnix() + 3540,
						SourceMetricKey: testSourceMetricKey,
					})
				default:
					seed, text := int64ListSeed(2, 3)
					def := f.registerActive(newDef("u_dirty_vector_7d", 1, withTTL(3600),
						withInt64List(int32(len(seed)), text)))
					f.putRawValue(&model.FeatureValue{
						FeatureKey: def.FeatureKey, Version: 1, EntityScope: model.EntityScopeMid,
						EntityID: testMid, ValueType: model.ValueTypeInt64List,
						ListValues:      "1,not-a-number",
						EventTime:       f.nowUnix() - 60,
						ExpireAt:        f.nowUnix() + 3540,
						SourceMetricKey: testSourceMetricKey,
					})
				}
				// 同主体另有一行干净数据：脏行不能把整页变成「空」，也不能被跳过。
				clean := f.registerActive(newDef("a_clean_7d", 1))
				f.putInt64(clean, 1, testMid, 5, f.nowUnix()-60)

				reply, err := callExport(f, exportReq(midRef(testMid), 0, 1, 10))
				if !errors.Is(err, c.want) {
					t.Fatalf("err=%v, want %v", err, c.want)
				}
				if !strings.Contains(err.Error(), "@v1") {
					t.Errorf("err=%v, want the failing row identified by feature@version", err)
				}
				if reply != nil {
					t.Errorf("reply=%+v, want nil: 解码失败的一行被丢下、其余行照发等于把脏数据问题藏起来", reply)
				}
			})
		}
	})
}

// --- 新鲜度：过期行照样导出，但必须带着 EXPIRED ---

// TestListEntityFeaturesExpiredRowsAreExportedWithTheExpiredMarker 钉住
// listentityfeatureslogic.go:93-95 的那条注释：过期不是「隐藏这一条」的理由。
//
// 顺带记一条不可达分支（不改代码、不放宽断言）：logic 在 ClassifyDegradation 返回
// error 时报错（:99-101），但导出路径把 DefinitionFound/ValueFound/AllowStale/
// SourceAvailable 全写死为 true，而 model/types.go:321-339 里这几项齐备时
// 唯一能返回错误的条件是 DefinitionFound=false —— 所以这一档恒不进错误分支，
// 状态只可能是 NONE 或 EXPIRED。下面两条子用例因此按「观察到的两态」断言。
func TestListEntityFeaturesExpiredRowsAreExportedWithTheExpiredMarker(t *testing.T) {
	const ttl = int64(3600)
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1, withTTL(ttl)))
	// 边界对：expire_at == now 判过期，expire_at == now+1 仍是新值
	// （model/featurevalue.go:349-353 `ExpireAt <= now`）。
	expired := f.putInt64(d, 1, testMid, 55, f.nowUnix()-ttl)
	fresh := f.putInt64(d, 1, "10087", 66, f.nowUnix()-ttl+1)
	if !expired.IsExpired(f.nowUnix()) {
		t.Fatal("test premise broken: the row meant to be expired is not expired at the pinned clock")
	}
	if fresh.IsExpired(f.nowUnix()) {
		t.Fatal("test premise broken: the row meant to be fresh is expired at the pinned clock")
	}

	// 单行场景：整页只有这一条过期行，去掉「EXPIRED」标记与保留它这两种实现的区别
	// 必须在这一组断言上分离（值/时刻照旧，只有标记不同）。
	single := newFixture(t)
	sd := single.registerActive(newDef("u_play_finish_7d", 1, withTTL(ttl)))
	sdRow := single.putInt64(sd, 1, testMid, 55, f.nowUnix()-ttl)

	t.Run("过期行不被丢弃而是带 EXPIRED 与自己的到期时刻", func(t *testing.T) {
		reply, err := callExport(single, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export of the expired row: %v", err)
		}
		if len(reply.GetEntries()) != 1 || reply.GetTotal() != 1 {
			t.Fatalf("entries=%d total=%d, want the expired row still exported",
				len(reply.GetEntries()), reply.GetTotal())
		}
		e := reply.GetEntries()[0]
		if got := e.GetDegradation(); got != rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED {
			t.Errorf("degradation=%v, want EXPIRED: 过期只能显式表达，不能靠丢弃", got)
		}
		want := exportSignatureOf(single, []*model.FeatureValue{sdRow},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED)
		if got := replySignature(t, reply); got != want {
			t.Errorf("entries=\n%s\nwant\n%s", got, want)
		}
		if e.GetExpireAt() != f.nowUnix() || e.GetEventTime() != f.nowUnix()-ttl {
			t.Errorf("expire_at=%d event_time=%d, want the row's own %d / %d so 调用方能看出旧到哪一步",
				e.GetExpireAt(), e.GetEventTime(), f.nowUnix(), f.nowUnix()-ttl)
		}
		if e.GetTtlSeconds() != ttl {
			t.Errorf("ttl_seconds=%d, want the definition's %d: 新鲜度口径必须跟着条目一起出",
				e.GetTtlSeconds(), ttl)
		}
		if got := e.GetValue().GetInt64Value(); got != 55 {
			t.Errorf("value=%d, want the stored 55: EXPIRED 表达的是「这一条旧」而不是换一个值", got)
		}
	})

	t.Run("同一主体跨过到期边界后同一条从 NONE 变 EXPIRED", func(t *testing.T) {
		// 边界对的第二半：时钟没跨过 expire_at 时是 NONE，跨过之后必须是 EXPIRED，
		// 且行还是同一行（值/版本/时刻都不动）。
		cross := newFixture(t)
		cd := cross.registerActive(newDef("u_play_finish_7d", 1, withTTL(ttl)))
		row := cross.putInt64(cd, 1, testMid, 55, cross.nowUnix()-ttl+1) // expire_at = now+1
		before, err := callExport(cross, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export just before expiry: %v", err)
		}
		if got, want := replySignature(t, before), exportSignatureOf(cross,
			[]*model.FeatureValue{row}, rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE); got != want {
			t.Errorf("entries before the boundary=\n%s\nwant\n%s", got, want)
		}
		cross.advance(1) // 现在 expire_at == now，IsExpired 取真
		after, err := callExport(cross, exportReq(midRef(testMid), 0, 1, 10))
		if err != nil {
			t.Fatalf("export at the expiry second: %v", err)
		}
		if got, want := replySignature(t, after), exportSignatureOf(cross,
			[]*model.FeatureValue{row}, rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED); got != want {
			t.Errorf("entries at the boundary=\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("过期与否按行自己判定，别的主体的行不进本次导出", func(t *testing.T) {
		// 两个主体各带一行（一条过期、一条新鲜），逐主体分页对照：
		// 若实现「本页有过期行就把整页标记成过期」或「按值而不是按 expire_at 判定」，
		// 这两条期望串会立刻错位。
		var got []string
		for pn := int32(1); pn <= 2; pn++ {
			reply, err := callExport(f, exportReq(midRef(testMid), 0, pn, 1))
			if err != nil {
				t.Fatalf("page %d: %v", pn, err)
			}
			if reply.GetTotal() != 1 {
				t.Fatalf("page %d total=%d, want 1: 别的主体的行不进本次导出", pn, reply.GetTotal())
			}
			for _, e := range reply.GetEntries() {
				got = append(got, entrySignature(t, e))
			}
		}
		want := exportSignatureOf(f, []*model.FeatureValue{expired},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED)
		if strings.Join(got, " || ") != want {
			t.Errorf("exported=%v, want only this subject's row:\n%s", got, want)
		}

		freshReply, err := callExport(f, exportReq(midRef("10087"), 0, 1, 10))
		if err != nil {
			t.Fatalf("export for the fresh subject: %v", err)
		}
		wantFresh := exportSignatureOf(f, []*model.FeatureValue{fresh},
			rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
		if got := replySignature(t, freshReply); got != wantFresh {
			t.Errorf("fresh subject entries=\n%s\nwant\n%s", got, wantFresh)
		}
	})
}

// --- 响应体上限：整页拒绝，而不是把超出的部分悄悄丢掉 ---

func TestListEntityFeaturesResponseByteCapRejectsTheWholePage(t *testing.T) {
	f := newFixture(t)
	// 向量列 × 若干行：条数合法（ps=100 之内）但体积可以远超上限，
	// 所以条数闸门之外还必须有一道体积闸门（listentityfeatureslogic.go:109）。
	const dimension = 512
	keys := []string{"u_vector_a_7d", "u_vector_b_7d", "u_vector_c_7d"}
	rows := make([]*model.FeatureValue, 0, len(keys))
	for i, key := range keys {
		seed, text := int64ListSeed(int64(i+1), dimension)
		def := f.registerActive(newDef(key, 1, withTTL(3600),
			withInt64List(int32(len(seed)), text)))
		rows = append(rows, f.putValue(def, 1, testMid, model.ValuePayload{
			ValueType: model.ValueTypeInt64List, Int64List: seed,
		}, f.nowUnix()-60))
	}

	ok, err := callExport(f, exportReq(midRef(testMid), 0, 1, 100))
	if err != nil {
		t.Fatalf("export under the default cap: %v", err)
	}
	if len(ok.GetEntries()) != len(rows) {
		t.Fatalf("entries=%d, want the whole %d-row page", len(ok.GetEntries()), len(rows))
	}
	size := proto.Size(ok)
	if size <= 1 {
		t.Fatalf("reply size=%d, the test needs a non-trivial payload", size)
	}

	// 边界对：上限 = 实际字节数时放行，少 1 字节时整页报错。
	// 「夹到上限再返回」的实现在这里会给出 2 条条目 + nil error，立刻变红。
	f.withMaxResponseBytes(size)
	allowed, err := callExport(f, exportReq(midRef(testMid), 0, 1, 100))
	if err != nil {
		t.Fatalf("cap exactly at %d bytes: %v", size, err)
	}
	want := exportSignatureOf(f, rows, rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE)
	if got := replySignature(t, allowed); got != want {
		t.Errorf("entries under the exact cap=\n%.300s\nwant\n%.300s", got, want)
	}

	f.withMaxResponseBytes(size - 1)
	truncated, err := callExport(f, exportReq(midRef(testMid), 0, 1, 100))
	if !errors.Is(err, model.ErrTooManyEntries) {
		t.Fatalf("err=%v, want %v when the page does not fit", err, model.ErrTooManyEntries)
	}
	if truncated != nil {
		t.Errorf("reply=%+v, want nil: 超限时不能带着半个页面报成功", truncated)
	}
	if !strings.Contains(err.Error(), "reduce page size") {
		t.Errorf("err=%v, want the message to tell the caller what to change", err)
	}
	// 闸门在读之后、应答之前：三次尝试各读一次，读本身没有被改写，
	// 所以「体积不足」不能伪装成谓词少查。
	if n := countCalled(f.values.calls, "values.ListByEntity"); n != 3 {
		t.Errorf("values.ListByEntity calls=%d, want 3 (three attempts, one read each)", n)
	}
}
