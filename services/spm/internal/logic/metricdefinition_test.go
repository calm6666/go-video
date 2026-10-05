package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/spm/model"
	"go-video/services/spm/rpc"
)

// 本文件钉住 README「口径版本策略」与「其它实现约定」对注册表的全部承诺：
// 已登记版本不可变（只能新增版本）、状态机 CAS、ACTIVE 指针单写者、
// 以及「读接口绝不伪造口径」的 fail-closed 线。

func regReq(metricKey string, version int32) *rpc.UpsertMetricDefinitionReq {
	return &rpc.UpsertMetricDefinitionReq{
		Definition: &rpc.MetricDefinition{
			MetricKey: metricKey, MetricVersion: version, Name: metricKey + " 展示名",
			Formula: "count(distinct event_id)", Unit: "count",
			SupportedWindows: []rpc.WindowType{rpc.WindowType_WINDOW_TYPE_DAY,
				rpc.WindowType_WINDOW_TYPE_5_MIN, rpc.WindowType_WINDOW_TYPE_HOUR,
				rpc.WindowType_WINDOW_TYPE_5_MIN},
			SourceEventTypes: "behavior.play,behavior.exposure",
			State:            rpc.DefinitionState_DEFINITION_STATE_UNSPECIFIED,
			Description:      "首版说明", CreatedBy: "author",
		},
		Operator: "reviewer", RequestId: "req-reg-" + metricKey,
	}
}

func callUpsert(t *testing.T, d *deps,
	in *rpc.UpsertMetricDefinitionReq) (*rpc.UpsertMetricDefinitionReply, error) {
	t.Helper()
	return NewUpsertMetricDefinitionLogic(context.Background(), d.ctx).UpsertMetricDefinition(in)
}

func callStateUpdate(t *testing.T, d *deps,
	in *rpc.UpdateMetricDefinitionStateReq) (*rpc.UpdateMetricDefinitionStateReply, error) {
	t.Helper()
	return NewUpdateMetricDefinitionStateLogic(context.Background(), d.ctx).
		UpdateMetricDefinitionState(in)
}

func callGetDef(t *testing.T, d *deps, in *rpc.GetMetricDefinitionReq) (*rpc.GetMetricDefinitionReply, error) {
	t.Helper()
	return NewGetMetricDefinitionLogic(context.Background(), d.ctx).GetMetricDefinition(in)
}

func callListDef(t *testing.T, d *deps,
	in *rpc.ListMetricDefinitionsReq) (*rpc.ListMetricDefinitionsReply, error) {
	t.Helper()
	return NewListMetricDefinitionsLogic(context.Background(), d.ctx).ListMetricDefinitions(in)
}

func stateReq(metricKey string, version, toState int32) *rpc.UpdateMetricDefinitionStateReq {
	return &rpc.UpdateMetricDefinitionStateReq{
		MetricKey: metricKey, MetricVersion: version, State: rpc.DefinitionState(toState),
		Operator: "reviewer", Reason: "评审通过上线", RequestId: "req-st-" + metricKey,
	}
}

func storedDef(t *testing.T, d *deps, metricKey string, version int32) *model.MetricDefinition {
	t.Helper()
	row, ok := d.db.defs[defKey(metricKey, version)]
	if !ok {
		t.Fatalf("口径 %s@v%d 未登记", metricKey, version)
	}
	return row
}

// TestUpsertMetricDefinitionForcesDraftAndNormalizesSpec 登记只能产出 DRAFT，
// 且 CSV 列按语义归一（去重、排序、裁剪空白）后才落库：否则同一份口径会因为
// 书写顺序被登记成两个版本。
func TestUpsertMetricDefinitionForcesDraftAndNormalizesSpec(t *testing.T) {
	d := newDeps(t)
	in := regReq(" play_finish_rate ", 1)
	in.Definition.Name = "  完播率  "
	in.Definition.Formula = "  count(distinct event_id)  "

	reply, err := callUpsert(t, d, in)
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if !reply.Created || reply.Reused {
		t.Fatalf("created/reused=%v/%v，期望 true/false", reply.Created, reply.Reused)
	}
	if reply.Definition == nil {
		t.Fatal("响应未带回登记后的口径")
	}
	if got := reply.Definition.State; got != rpc.DefinitionState_DEFINITION_STATE_DRAFT {
		t.Fatalf("登记后 state=%v，新登记一律 DRAFT（ACTIVE 只能走状态迁移）", got)
	}
	row := storedDef(t, d, "play_finish_rate", 1)
	if row.SupportedWindows != "1,2,3" {
		t.Fatalf("supported_windows=%q，期望归一为 \"1,2,3\"（去重 + 升序）", row.SupportedWindows)
	}
	if row.SourceEventTypes != "behavior.exposure,behavior.play" {
		t.Fatalf("source_event_types=%q，期望按字典序归一", row.SourceEventTypes)
	}
	if row.Name != "完播率" || row.Formula != "count(distinct event_id)" {
		t.Fatalf("空白未裁剪: name=%q formula=%q", row.Name, row.Formula)
	}
	if row.MetricKey != "play_finish_rate" {
		t.Fatalf("metric_key 未裁剪: %q", row.MetricKey)
	}
	if row.RequestID != in.RequestId {
		t.Fatalf("request_id 未落行: %q", row.RequestID)
	}
	// created_by 是登记人字段，缺省才回落到 operator：两条审计线索不能互相顶替。
	if row.CreatedBy != "author" {
		t.Fatalf("created_by=%q，期望取 definition.created_by", row.CreatedBy)
	}
	// 已知缺口 D-3：首次创建的响应直接回带入参归一行，ctime/mtime 由库赋值而 model 不回填，
	// 所以这里恒为 0（管理后台按 ctime 排序展示时会看到 1970）。reused 路径回的是库里的行，
	// 时间戳齐全。本断言钉住现状，修 D-3（创建后回读一次）时应改为「非 0」。
	if reply.Definition.Ctime != 0 || reply.Definition.Mtime != 0 {
		t.Fatalf("新建口径的响应时间戳被回带了（D-3 已修？请同步本断言）: %+v", reply.Definition)
	}
	if got := storedDef(t, d, "play_finish_rate", 1); got.Ctime == 0 || got.Mtime == 0 {
		t.Fatalf("库里的登记行没有时间戳: %+v", got)
	}

	// created_by 缺省回落 operator。
	fallback := regReq("play_cnt", 1)
	fallback.Definition.CreatedBy = "   "
	if _, err := callUpsert(t, d, fallback); err != nil {
		t.Fatal(err)
	}
	if got := storedDef(t, d, "play_cnt", 1).CreatedBy; got != "reviewer" {
		t.Fatalf("created_by=%q，期望回落 operator", got)
	}
}

// TestUpsertMetricDefinitionRepeatIsReusedNotRewritten 同一份登记的重复投递回 reused，
// 且不得改写已有行（说明文本也不能覆盖：评审时看到的必须是登记当时那一份）。
func TestUpsertMetricDefinitionRepeatIsReusedNotRewritten(t *testing.T) {
	d := newDeps(t)
	first := regReq("hot_score", 4)
	first.RequestId = "req-a"
	if _, err := callUpsert(t, d, first); err != nil {
		t.Fatal(err)
	}
	before := *storedDef(t, d, "hot_score", 4)

	again := regReq("hot_score", 4)
	again.RequestId = "req-b"
	again.Definition.Name = "改了展示名"
	again.Definition.Description = "改了说明"
	// 粒度顺序换一下、重复值去掉、空白加上：语义与首次登记完全一致。
	again.Definition.SupportedWindows = []rpc.WindowType{
		rpc.WindowType_WINDOW_TYPE_HOUR, rpc.WindowType_WINDOW_TYPE_DAY,
		rpc.WindowType(999), // 非法值：先于归一被拒绝，见下面的单独用例
	}
	if _, err := callUpsert(t, d, again); err == nil {
		t.Fatal("未登记粒度 999 竟然通过了校验")
	} else {
		again.Definition.SupportedWindows = []rpc.WindowType{
			rpc.WindowType_WINDOW_TYPE_HOUR, rpc.WindowType_WINDOW_TYPE_DAY,
			rpc.WindowType_WINDOW_TYPE_5_MIN}
	}

	reply, err := callUpsert(t, d, again)
	if err != nil {
		t.Fatalf("重复登记失败: %v", err)
	}
	if reply.Created || !reply.Reused {
		t.Fatalf("created/reused=%v/%v，期望 false/true", reply.Created, reply.Reused)
	}
	if d.defs.insertCalls != 1 {
		t.Fatalf("重复登记又发起了一次插入（%d 次）", d.defs.insertCalls)
	}
	after := *storedDef(t, d, "hot_score", 4)
	if after.Name != before.Name || after.Description != before.Description ||
		after.RequestID != before.RequestID {
		t.Fatalf("重复登记改写了已有行: %+v vs %+v", after, before)
	}
	if reply.Definition.MetricVersion != 4 || reply.Definition.State !=
		rpc.DefinitionState_DEFINITION_STATE_DRAFT {
		t.Fatalf("reused 响应内容不对: %+v", reply.Definition)
	}
	// 新增版本才是合法演进。
	next := regReq("hot_score", 5)
	next.RequestId = "req-c"
	next.Definition.Formula = "count(distinct event_id)/count(distinct mid)"
	next.Definition.Unit = "ratio"
	if _, err := callUpsert(t, d, next); err != nil {
		t.Fatalf("新增版本被拒: %v", err)
	}
	if v4 := storedDef(t, d, "hot_score", 4); v4.Formula != before.Formula {
		t.Fatalf("登记 v5 改动了 v4: %q", v4.Formula)
	}
}

// TestUpsertMetricDefinitionImmutableColumns 口径本体四列任一不同都必须拒绝，
// 并且错误要点名是哪一列不同（只回「不可变」的话调用方只能反复重投）。
func TestUpsertMetricDefinitionImmutableColumns(t *testing.T) {
	cases := []struct {
		why string
		col string
		mut func(*rpc.UpsertMetricDefinitionReq)
	}{
		{"公式变了", "formula", func(in *rpc.UpsertMetricDefinitionReq) {
			in.Definition.Formula = "sum(play_seconds)"
		}},
		{"单位变了", "unit", func(in *rpc.UpsertMetricDefinitionReq) {
			in.Definition.Unit = "seconds"
		}},
		{"粒度集合变了", "supported_windows", func(in *rpc.UpsertMetricDefinitionReq) {
			in.Definition.SupportedWindows = []rpc.WindowType{
				rpc.WindowType_WINDOW_TYPE_5_MIN}
		}},
		{"订阅事件变了", "source_event_types", func(in *rpc.UpsertMetricDefinitionReq) {
			in.Definition.SourceEventTypes = "behavior.play"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			d := newDeps(t)
			if _, err := callUpsert(t, d, regReq("play_cnt", 2)); err != nil {
				t.Fatal(err)
			}
			before := *storedDef(t, d, "play_cnt", 2)

			in := regReq("play_cnt", 2)
			in.RequestId = "req-other"
			tc.mut(in)
			_, err := callUpsert(t, d, in)
			requireErrIs(t, err, model.ErrMetricVersionImmutable, tc.why)
			if !strings.Contains(err.Error(), tc.col) {
				t.Fatalf("错误未点名列 %q: %v", tc.col, err)
			}
			after := *storedDef(t, d, "play_cnt", 2)
			if after.Formula != before.Formula || after.Unit != before.Unit ||
				after.SupportedWindows != before.SupportedWindows ||
				after.SourceEventTypes != before.SourceEventTypes ||
				after.State != before.State {
				t.Fatalf("已登记版本被原地改写: %+v vs %+v", after, before)
			}
			if d.defs.insertCalls != 1 {
				t.Fatalf("不可变冲突仍插入了新行（%d 次）", d.defs.insertCalls)
			}
		})
	}
}

// TestUpsertMetricDefinitionRejectsUnsafeRegistrations 整单级拒绝：任何一项不合规
// 都不落库（注册表是所有人读口径的入口，脏登记比没登记更难查）。
func TestUpsertMetricDefinitionRejectsUnsafeRegistrations(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	cases := []struct {
		why   string
		want  error
		build func() *rpc.UpsertMetricDefinitionReq
	}{
		{"缺幂等键", model.ErrRequestIdRequired, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.RequestId = "  "
			return in
		}},
		{"幂等键超长", model.ErrRequestIdRequired, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.RequestId = long(129)
			return in
		}},
		{"缺操作人", model.ErrOperatorRequired, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Operator = ""
			return in
		}},
		{"操作人超长", model.ErrOperatorRequired, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Operator = long(65)
			return in
		}},
		{"登记人为空且无操作人", model.ErrOperatorRequired, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Operator, in.Definition.CreatedBy = long(70), long(70)
			return in
		}},
		{"definition 缺失", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			return &rpc.UpsertMetricDefinitionReq{Operator: "o", RequestId: "r"}
		}},
		{"metric_key 为空", model.ErrMetricKeyEmpty, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("   ", 1)
			return in
		}},
		{"metric_key 超长", model.ErrMetricKeyEmpty, func() *rpc.UpsertMetricDefinitionReq {
			return regReq(long(101), 1)
		}},
		{"版本为 0", model.ErrMetricVersionRequired, func() *rpc.UpsertMetricDefinitionReq {
			return regReq("k", 0)
		}},
		{"版本为负", model.ErrMetricVersionRequired, func() *rpc.UpsertMetricDefinitionReq {
			return regReq("k", -2)
		}},
		{"登记即上线", model.ErrInvalidDefinitionState, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.State = rpc.DefinitionState_DEFINITION_STATE_ACTIVE
			return in
		}},
		{"登记成 RETIRED", model.ErrInvalidDefinitionState, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.State = rpc.DefinitionState_DEFINITION_STATE_RETIRED
			return in
		}},
		{"缺展示名", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Name = " "
			return in
		}},
		{"展示名超长", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Name = long(101)
			return in
		}},
		{"缺公式", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Formula = "  "
			return in
		}},
		{"公式超长", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Formula = long(501)
			return in
		}},
		{"单位不在白名单", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Unit = "yuan"
			return in
		}},
		{"说明超长", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.Description = long(501)
			return in
		}},
		{"无合法粒度", model.ErrInvalidWindow, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.SupportedWindows = nil
			return in
		}},
		{"粒度含 UNSPECIFIED", model.ErrInvalidWindow, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.SupportedWindows = []rpc.WindowType{
				rpc.WindowType_WINDOW_TYPE_UNSPECIFIED, rpc.WindowType_WINDOW_TYPE_DAY}
			return in
		}},
		{"粒度越界", model.ErrInvalidWindow, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.SupportedWindows = []rpc.WindowType{rpc.WindowType(7)}
			return in
		}},
		{"订阅事件为空", model.ErrInvalidDefinitionSpec, func() *rpc.UpsertMetricDefinitionReq {
			in := regReq("k", 1)
			in.Definition.SourceEventTypes = " , "
			return in
		}},
		{"订阅事件越界（含广告域自造事件）", model.ErrInvalidDefinitionSpec,
			func() *rpc.UpsertMetricDefinitionReq {
				in := regReq("k", 1)
				in.Definition.SourceEventTypes = "behavior.play,ad.imp"
				return in
			}},
	}
	d := newDeps(t)
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			_, err := callUpsert(t, d, tc.build())
			requireErrIs(t, err, tc.want, tc.why)
		})
	}
	if d.defs.insertCalls != 0 {
		t.Fatalf("以上拒绝路径尝试插入 %d 次", d.defs.insertCalls)
	}
	if len(d.db.defs) != 0 {
		t.Fatalf("以上拒绝路径留下了 %d 行登记", len(d.db.defs))
	}
	if d.read.calls != 0 {
		t.Fatalf("写入口不该消耗读侧令牌（%d 次）", d.read.calls)
	}
}

// TestUpsertMetricDefinitionConcurrentInsertKeepsWinner 并发下另一个实例先落同一版本：
// 插入返回未生效必须回读并按同一规则判定，绝不覆盖对方那行；回读为空则显式报错。
func TestUpsertMetricDefinitionConcurrentInsertKeepsWinner(t *testing.T) {
	t.Run("同规格并发按 reused 回带赢家行", func(t *testing.T) {
		d := newDeps(t)
		winner := seedDef(t, d, "race_key", 1, model.DefinitionStateDraft)
		d.defs.insertCalls = 0 // 只统计本次调用的插入
		d.defs.findBlind = 1   // 迁移前那一眼读不到（并发窗口）

		in := regReq("race_key", 1)
		in.RequestId = "req-loser"
		in.Definition.Formula = winner.Formula
		in.Definition.Unit = winner.Unit
		in.Definition.SupportedWindows = []rpc.WindowType{rpc.WindowType_WINDOW_TYPE_5_MIN,
			rpc.WindowType_WINDOW_TYPE_HOUR, rpc.WindowType_WINDOW_TYPE_DAY}
		in.Definition.SourceEventTypes = winner.SourceEventTypes
		reply, err := callUpsert(t, d, in)
		if err != nil {
			t.Fatalf("并发同规格登记应回 reused: %v", err)
		}
		if reply.Created || !reply.Reused {
			t.Fatalf("created/reused=%v/%v", reply.Created, reply.Reused)
		}
		if got := storedDef(t, d, "race_key", 1).RequestID; got != winner.RequestID {
			t.Fatalf("输家改写了赢家的登记幂等键: %q", got)
		}
		if d.defs.insertCalls != 1 {
			t.Fatalf("插入次数=%d", d.defs.insertCalls)
		}
	})

	t.Run("异规格并发仍按不可变拒绝", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "race_key", 1, model.DefinitionStateDraft)
		d.defs.findBlind = 1
		in := regReq("race_key", 1)
		in.RequestId = "req-loser"
		_, err := callUpsert(t, d, in)
		requireErrIs(t, err, model.ErrMetricVersionImmutable, "并发不同口径")
		if got := storedDef(t, d, "race_key", 1).Formula; got != "count(distinct event_id)" {
			t.Fatalf("并发路径改写了已有公式: %q", got)
		}
	})

	t.Run("插成功却读不到必须显式报错", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "race_key", 1, model.DefinitionStateDraft)
		d.defs.findBlind = 99 // 两次点查都空
		_, err := callUpsert(t, d, regReq("race_key", 1))
		requireErrIs(t, err, model.ErrMetricDefinitionNotFound, "回读为空")
		if !strings.Contains(err.Error(), "回读为空") {
			t.Fatalf("错误应说明「插入未生效且回读为空」: %v", err)
		}
	})

	t.Run("点查库故障原样上抛", func(t *testing.T) {
		d := newDeps(t)
		d.defs.errOn = "find"
		_, err := callUpsert(t, d, regReq("k", 1))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		if d.defs.insertCalls != 0 {
			t.Fatal("读库失败时不该继续插入（fail closed）")
		}
	})
}

// TestUpdateMetricDefinitionStateMachine 状态机：DRAFT->ACTIVE、DRAFT/ACTIVE->RETIRED 三条边，
// RETIRED 不出（历史窗口不能换解释）、ACTIVE->DRAFT 不出（发布过的口径不能假装没发布）。
func TestUpdateMetricDefinitionStateMachine(t *testing.T) {
	cases := []struct {
		why       string
		from      int32
		to        int32
		wantErr   error
		wantState int32
		wantCas   string
		wantSame  bool
	}{
		{"DRAFT->ACTIVE 合法", model.DefinitionStateDraft, model.DefinitionStateActive,
			nil, model.DefinitionStateActive, "sm_key@v1:1->2", false},
		{"DRAFT->RETIRED 合法（评审不过直接废）", model.DefinitionStateDraft,
			model.DefinitionStateRetired, nil, model.DefinitionStateRetired,
			"sm_key@v1:1->3", false},
		{"ACTIVE->RETIRED 合法", model.DefinitionStateActive, model.DefinitionStateRetired,
			nil, model.DefinitionStateRetired, "sm_key@v1:2->3", false},
		{"RETIRED->ACTIVE 拒绝（不复活）", model.DefinitionStateRetired,
			model.DefinitionStateActive, model.ErrInvalidDefinitionState,
			model.DefinitionStateRetired, "", false},
		{"RETIRED->DRAFT 拒绝", model.DefinitionStateRetired, model.DefinitionStateDraft,
			model.ErrInvalidDefinitionState, model.DefinitionStateRetired, "", false},
		{"ACTIVE->DRAFT 拒绝", model.DefinitionStateActive, model.DefinitionStateDraft,
			model.ErrInvalidDefinitionState, model.DefinitionStateActive, "", false},
		{"同状态按重放处理", model.DefinitionStateDraft, model.DefinitionStateDraft,
			nil, model.DefinitionStateDraft, "", true},
		{"目标 UNSPECIFIED 拒绝", model.DefinitionStateDraft,
			model.DefinitionStateUnspecified, model.ErrInvalidDefinitionState,
			model.DefinitionStateDraft, "", false},
		{"目标越界拒绝", model.DefinitionStateDraft, 9, model.ErrInvalidDefinitionState,
			model.DefinitionStateDraft, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			d := newDeps(t)
			seedDef(t, d, "sm_key", 1, tc.from)
			in := stateReq("sm_key", 1, tc.to)
			reply, err := callStateUpdate(t, d, in)
			if tc.wantErr != nil {
				requireErrIs(t, err, tc.wantErr, tc.why)
				if reply != nil {
					t.Fatalf("拒绝路径还回了响应: %+v", reply)
				}
				// 非法迁移绝不能碰 CAS，更不能改状态。
				if len(d.defs.updateCalls) != 0 {
					t.Fatalf("非法迁移仍发起了 CAS: %v", d.defs.updateCalls)
				}
			} else {
				if err != nil {
					t.Fatalf("%s: %v", tc.why, err)
				}
				if reply.Reused != tc.wantSame {
					t.Fatalf("reused=%v，期望 %v", reply.Reused, tc.wantSame)
				}
				if reply.Definition == nil {
					t.Fatal("响应未回带当前口径")
				}
				cas := ""
				if len(d.defs.updateCalls) == 1 {
					cas = d.defs.updateCalls[0]
				}
				if len(d.defs.updateCalls) > 1 {
					t.Fatalf("CAS 发起了 %d 次: %v", len(d.defs.updateCalls), d.defs.updateCalls)
				}
				if cas != tc.wantCas {
					t.Fatalf("CAS 轨迹=%q，期望 %q", cas, tc.wantCas)
				}
			}
			row := storedDef(t, d, "sm_key", 1)
			if row.State != tc.wantState {
				t.Fatalf("state=%d，期望 %d", row.State, tc.wantState)
			}
			if tc.wantErr == nil && !tc.wantSame && tc.to != model.DefinitionStateUnspecified {
				if row.Description != in.Reason {
					t.Fatalf("迁移理由未落库: %q", row.Description)
				}
			}
		})
	}
}

// TestUpdateMetricDefinitionStateRequiresEverything 审计三要素与定位参数缺一不受理。
func TestUpdateMetricDefinitionStateRequiresEverything(t *testing.T) {
	d := newDeps(t)
	seedDef(t, d, "audit_key", 1, model.DefinitionStateDraft)
	cases := []struct {
		why  string
		want error
		mut  func(*rpc.UpdateMetricDefinitionStateReq)
	}{
		{"缺操作人", model.ErrOperatorRequired, func(in *rpc.UpdateMetricDefinitionStateReq) {
			in.Operator = " "
		}},
		{"缺理由", model.ErrReasonRequired, func(in *rpc.UpdateMetricDefinitionStateReq) {
			in.Reason = ""
		}},
		{"理由超长", model.ErrReasonRequired, func(in *rpc.UpdateMetricDefinitionStateReq) {
			in.Reason = strings.Repeat("理", 501)
		}},
		{"缺幂等键", model.ErrRequestIdRequired, func(in *rpc.UpdateMetricDefinitionStateReq) {
			in.RequestId = ""
		}},
		{"version=0 无起点可迁", model.ErrMetricVersionRequired,
			func(in *rpc.UpdateMetricDefinitionStateReq) { in.MetricVersion = 0 }},
		{"metric_key 为空", model.ErrMetricKeyEmpty,
			func(in *rpc.UpdateMetricDefinitionStateReq) { in.MetricKey = "  " }},
	}
	for _, tc := range cases {
		in := stateReq("audit_key", 1, model.DefinitionStateActive)
		tc.mut(in)
		_, err := callStateUpdate(t, d, in)
		requireErrIs(t, err, tc.want, tc.why)
	}
	t.Run("未登记的版本", func(t *testing.T) {
		_, err := callStateUpdate(t, d, stateReq("ghost_key", 7, model.DefinitionStateActive))
		requireErrIs(t, err, model.ErrMetricDefinitionNotFound, "未登记")
	})
	t.Run("点查库故障不降级", func(t *testing.T) {
		d.defs.errOn = "find"
		_, err := callStateUpdate(t, d, stateReq("audit_key", 1, model.DefinitionStateActive))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d.defs.errOn = ""
	})
	t.Run("CAS 库故障原样上抛", func(t *testing.T) {
		d.defs.errOn = "update"
		_, err := callStateUpdate(t, d, stateReq("audit_key", 1, model.DefinitionStateActive))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d.defs.errOn = ""
	})
	if got := storedDef(t, d, "audit_key", 1).State; got != model.DefinitionStateDraft {
		t.Fatalf("以上失败路径改动了 state=%d", got)
	}
}

// TestUpdateMetricDefinitionStateCasMissKeepsWinner 读后写之间被人改过：
// CAS 未命中必须显式报出当前真值，且绝不改写对方已经落定的状态。
func TestUpdateMetricDefinitionStateCasMissKeepsWinner(t *testing.T) {
	d := newDeps(t)
	seedDef(t, d, "cas_key", 9, model.DefinitionStateDraft)
	// 并发赢家在 CAS 条件生效之前把这一行退役了。
	d.defs.onUpdateState = func() {
		d.db.defs[defKey("cas_key", 9)].State = model.DefinitionStateRetired
	}
	_, err := callStateUpdate(t, d, stateReq("cas_key", 9, model.DefinitionStateActive))
	requireErrIs(t, err, model.ErrInvalidDefinitionState, "CAS 未命中")
	if !strings.Contains(err.Error(), "当前 state=3") || !strings.Contains(err.Error(), "期望起点 1") {
		t.Fatalf("错误未给出现状与起点，调用方无法判断该重试还是接受: %v", err)
	}
	if len(d.defs.updateCalls) != 1 {
		t.Fatalf("CAS 只应尝试一次: %v", d.defs.updateCalls)
	}
	if got := storedDef(t, d, "cas_key", 9).State; got != model.DefinitionStateRetired {
		t.Fatalf("CAS 未命中却改动了赢家的状态: %d", got)
	}
}

// TestUpdateMetricDefinitionStateActiveMustBeSingle ACTIVE 指针是 version=0 读请求的唯一
// 解释来源，schema 里没有约束保证唯一（README「契约缺口」），所以迁移前必须数一遍并拒绝。
func TestUpdateMetricDefinitionStateActiveMustBeSingle(t *testing.T) {
	t.Run("已有 ACTIVE 时拒绝再激活", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "ptr_key", 1, model.DefinitionStateActive)
		seedDef(t, d, "ptr_key", 2, model.DefinitionStateDraft)

		_, err := callStateUpdate(t, d, stateReq("ptr_key", 2, model.DefinitionStateActive))
		requireErrIs(t, err, model.ErrMultipleActiveDefinition, "双 ACTIVE")
		if len(d.defs.updateCalls) != 0 {
			t.Fatalf("冲突未挡住 CAS: %v", d.defs.updateCalls)
		}
		if got := storedDef(t, d, "ptr_key", 2).State; got != model.DefinitionStateDraft {
			t.Fatalf("被拒的激活仍改写了 v2: %d", got)
		}
		if got := storedDef(t, d, "ptr_key", 1).State; got != model.DefinitionStateActive {
			t.Fatalf("当前 ACTIVE 指针被动了: %d", got)
		}
	})

	t.Run("RETIRED 版本不占用 ACTIVE 名额", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "ptr_key", 1, model.DefinitionStateRetired)
		seedDef(t, d, "ptr_key", 2, model.DefinitionStateDraft)
		if _, err := callStateUpdate(t, d, stateReq("ptr_key", 2, model.DefinitionStateActive)); err != nil {
			t.Fatalf("历史 RETIRED 版本不该挡住新激活: %v", err)
		}
		if got := storedDef(t, d, "ptr_key", 2).State; got != model.DefinitionStateActive {
			t.Fatalf("state=%d", got)
		}
	})

	t.Run("退役当前版本后另一版本可激活", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "ptr_key", 1, model.DefinitionStateActive)
		seedDef(t, d, "ptr_key", 2, model.DefinitionStateDraft)
		if _, err := callStateUpdate(t, d, stateReq("ptr_key", 1, model.DefinitionStateRetired)); err != nil {
			t.Fatal(err)
		}
		if _, err := callStateUpdate(t, d, stateReq("ptr_key", 2, model.DefinitionStateActive)); err != nil {
			t.Fatalf("退役后应可激活后继版本: %v", err)
		}
		if n := activeCount(d, "ptr_key"); n != 1 {
			t.Fatalf("ACTIVE 数量=%d，期望 1", n)
		}
	})

	t.Run("CountActive 失败时不得放行激活", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "ptr_key", 2, model.DefinitionStateDraft)
		d.defs.errOn = "count"
		_, err := callStateUpdate(t, d, stateReq("ptr_key", 2, model.DefinitionStateActive))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		if len(d.defs.updateCalls) != 0 {
			t.Fatalf("数不清 ACTIVE 仍能改状态: %v", d.defs.updateCalls)
		}
	})
}

func activeCount(d *deps, metricKey string) int {
	var n int
	for _, row := range d.db.defs {
		if row.MetricKey == metricKey && row.State == model.DefinitionStateActive {
			n++
		}
	}
	return n
}

// TestUpdateMetricDefinitionStateConcurrentActiveRollback 迁移后复查发现并发赢家：
// 必须报错让调用方知道这次激活没成（已知缺陷 D-1：回滚写成了 ACTIVE->ACTIVE 空操作）。
//
// 修复 guardSingleActive 之后，本文件的「库里仍有 2 个 ACTIVE」这条断言应改成 1；
// 现在它钉住的是可观测现状，不是理想行为。
func TestUpdateMetricDefinitionStateConcurrentActiveRollback(t *testing.T) {
	d := newDeps(t)
	seedDef(t, d, "race_act", 1, model.DefinitionStateDraft)
	seedDef(t, d, "race_act", 2, model.DefinitionStateActive)
	// 第一眼看 0（并发窗口内 v2 尚未激活），第二眼（迁移后复查）看到 2。
	d.defs.countSeq = []countStep{{n: 0}, {n: 2}}

	_, err := callStateUpdate(t, d, stateReq("race_act", 1, model.DefinitionStateActive))
	requireErrIs(t, err, model.ErrMultipleActiveDefinition, "并发双 ACTIVE")

	want := []string{"race_act@v1:1->2", "race_act@v1:2->2"}
	if strings.Join(d.defs.updateCalls, "|") != strings.Join(want, "|") {
		t.Fatalf("CAS 轨迹=%v，期望 %v（第二条即「回滚」）", d.defs.updateCalls, want)
	}
	// D-1：所谓「回滚」是 ACTIVE -> ACTIVE，条件恒成立，等于什么都没退。
	if n := activeCount(d, "race_act"); n != 2 {
		t.Fatalf("库里 ACTIVE=%d，与当前实现不符（若已修复请同步本断言）", n)
	}
	if !strings.Contains(err.Error(), "本次激活已回滚") {
		t.Fatalf("错误文本声称已回滚，实际没有: %v", err)
	}

	t.Run("复查计数失败时不回滚也不失败", func(t *testing.T) {
		d2 := newDeps(t)
		seedDef(t, d2, "ok_key", 1, model.DefinitionStateDraft)
		d2.defs.countSeq = []countStep{{n: 0}, {err: errStub}}
		reply, err := callStateUpdate(t, d2, stateReq("ok_key", 1, model.DefinitionStateActive))
		if err != nil {
			t.Fatalf("已经激活成功的一眼没看成，不该回滚也不该失败: %v", err)
		}
		if reply.Definition.State != rpc.DefinitionState_DEFINITION_STATE_ACTIVE {
			t.Fatalf("响应 state=%v", reply.Definition.State)
		}
		if len(d2.defs.updateCalls) != 1 {
			t.Fatalf("不该有第二次写入: %v", d2.defs.updateCalls)
		}
	})
}

// TestGetMetricDefinitionReadsEveryRegisteredVersion 单点读：显式版本连 DRAFT/RETIRED 都要
// 能读到（评审与历史窗口解释的入口），未登记给 found=false 而不是伪造口径。
func TestGetMetricDefinitionReadsEveryRegisteredVersion(t *testing.T) {
	d := newDeps(t)
	seedDef(t, d, "draft_key", 1, model.DefinitionStateDraft)
	seedDef(t, d, "retired_key", 2, model.DefinitionStateRetired)
	seedDef(t, d, "active_key", 3, model.DefinitionStateActive)

	cases := []struct {
		why       string
		in        *rpc.GetMetricDefinitionReq
		wantFound bool
		wantVer   int32
	}{
		{"DRAFT 可读（评审入口）", &rpc.GetMetricDefinitionReq{MetricKey: "draft_key",
			MetricVersion: 1}, true, 1},
		{"RETIRED 可读（历史窗口解释）", &rpc.GetMetricDefinitionReq{MetricKey: "retired_key",
			MetricVersion: 2}, true, 2},
		{"version=0 取 ACTIVE", &rpc.GetMetricDefinitionReq{MetricKey: "active_key"}, true, 3},
		{"未登记版本", &rpc.GetMetricDefinitionReq{MetricKey: "draft_key", MetricVersion: 9},
			false, 0},
		{"整键不存在", &rpc.GetMetricDefinitionReq{MetricKey: "ghost_key"}, false, 0},
		{"只有 DRAFT 时 version=0 视为没有 ACTIVE",
			&rpc.GetMetricDefinitionReq{MetricKey: "draft_key"}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			reply, err := callGetDef(t, d, tc.in)
			if err != nil {
				t.Fatalf("%s: %v", tc.why, err)
			}
			if reply.Found != tc.wantFound {
				t.Fatalf("found=%v，期望 %v", reply.Found, tc.wantFound)
			}
			if !tc.wantFound {
				if reply.Definition != nil {
					t.Fatalf("未命中却回了口径: %+v", reply.Definition)
				}
				return
			}
			if reply.Definition.MetricVersion != tc.wantVer {
				t.Fatalf("version=%d，期望 %d", reply.Definition.MetricVersion, tc.wantVer)
			}
		})
	}

	t.Run("多 ACTIVE 不得降级成查不到", func(t *testing.T) {
		d2 := newDeps(t)
		seedDef(t, d2, "amb_key", 1, model.DefinitionStateActive)
		seedDef(t, d2, "amb_key", 2, model.DefinitionStateActive)
		_, err := callGetDef(t, d2, &rpc.GetMetricDefinitionReq{MetricKey: "amb_key"})
		requireErrIs(t, err, model.ErrMultipleActiveDefinition, "ACTIVE 指针二义")
	})
	t.Run("负版本拒绝", func(t *testing.T) {
		_, err := callGetDef(t, d, &rpc.GetMetricDefinitionReq{MetricKey: "active_key",
			MetricVersion: -1})
		requireErrIs(t, err, model.ErrMetricVersionRequired, "负版本")
	})
	t.Run("空键拒绝", func(t *testing.T) {
		_, err := callGetDef(t, d, &rpc.GetMetricDefinitionReq{MetricKey: " "})
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "空键")
	})
	t.Run("库故障原样上抛", func(t *testing.T) {
		d2 := newDeps(t)
		d2.defs.errOn = "find"
		_, err := callGetDef(t, d2, &rpc.GetMetricDefinitionReq{MetricKey: "k", MetricVersion: 1})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
	})
	t.Run("读侧限流显式失败", func(t *testing.T) {
		d2 := newDeps(t)
		d2.read.denies = 1
		_, err := callGetDef(t, d2, &rpc.GetMetricDefinitionReq{MetricKey: "k", MetricVersion: 1})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌")
	})
}

// TestListMetricDefinitionsFiltersAndBounds 列表：过滤条件精确匹配 + ps 越界拒绝 +
// 越界页只回 total 不再发查询。
func TestListMetricDefinitionsFiltersAndBounds(t *testing.T) {
	d := newDeps(t)
	seedDef(t, d, "play_cnt", 1, model.DefinitionStateActive)
	// 让 Count 报出足够大的 total，越界页那两条用例才有意义。
	bigTotal := int64(25)

	t.Run("过滤条件逐列下传", func(t *testing.T) {
		d.defs.listTotal = &bigTotal
		d.defs.seenFts = nil
		if _, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{MetricKey: " play_cnt ",
			State: rpc.DefinitionState_DEFINITION_STATE_ACTIVE, Pn: 3, Ps: 10}); err != nil {
			t.Fatal(err)
		}
		want := model.MetricDefinitionFilter{MetricKey: "play_cnt",
			State: model.DefinitionStateActive, Offset: 20, Limit: 10}
		if len(d.defs.seenFts) != 2 || d.defs.seenFts[0] != want || d.defs.seenFts[1] != want {
			t.Fatalf("下传的过滤条件=%v，期望 COUNT 与 LIST 都是 %+v", d.defs.seenFts, want)
		}
	})
	t.Run("不限过滤", func(t *testing.T) {
		d.defs.seenFts = nil
		if _, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{}); err != nil {
			t.Fatal(err)
		}
		want := model.MetricDefinitionFilter{Limit: d.ctx.Config.Spm.PageSize}
		if d.defs.seenFts[0] != want {
			t.Fatalf("ps=0 应取默认页大小: %+v", d.defs.seenFts[0])
		}
	})
	t.Run("非法状态过滤拒绝而不是当不限", func(t *testing.T) {
		_, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{
			State: rpc.DefinitionState(7)})
		requireErrIs(t, err, model.ErrInvalidDefinitionState, "越界 state")
	})
	t.Run("超长过滤串按不限处理", func(t *testing.T) {
		d.defs.seenFts = nil
		if _, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{
			MetricKey: strings.Repeat("k", 101)}); err != nil {
			t.Fatal(err)
		}
		if d.defs.seenFts[0].MetricKey != "" {
			t.Fatalf("超长键应视为不限: %q", d.defs.seenFts[0].MetricKey)
		}
	})

	t.Run("越界页只回 total 不发查询", func(t *testing.T) {
		total := int64(3)
		d.defs.listTotal = &total
		d.defs.seenFts = nil
		d.defs.listCalls = 0
		reply, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{Pn: 2, Ps: 10})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != 3 {
			t.Fatalf("total=%d", reply.Total)
		}
		if len(reply.Definitions) != 0 {
			t.Fatalf("越界页仍回了 %d 行", len(reply.Definitions))
		}
		if d.defs.listCalls != 1 {
			t.Fatalf("越界页发了 %d 次查询（应只有 COUNT）", d.defs.listCalls)
		}
	})
	t.Run("深翻页保护与 total 无关", func(t *testing.T) {
		total := int64(5_000_000) // 远大于 offset：只有 1e6 守卫能挡住
		d.defs.listTotal = &total
		d.defs.listCalls = 0
		d.defs.seenFts = nil
		reply, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{Pn: 10_001, Ps: 100})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != total {
			t.Fatalf("total=%d，期望 %d", reply.Total, total)
		}
		if d.defs.listCalls != 1 {
			t.Fatalf("OFFSET=%d 量级仍去查列表（%d 次查询）",
				d.defs.seenFts[0].Offset, d.defs.listCalls)
		}
		if d.defs.seenFts[0].Offset != 1_000_000 {
			t.Fatalf("offset=%d，期望 10000 页起算 %d", d.defs.seenFts[0].Offset, 1_000_000)
		}
	})
	t.Run("COUNT 与 LIST 的库故障都上抛", func(t *testing.T) {
		d.defs.listTotal = &bigTotal
		d.defs.errOn = "countlist"
		_, err := callListDef(t, d, &rpc.ListMetricDefinitionsReq{})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d.defs.errOn = "list"
		_, err = callListDef(t, d, &rpc.ListMetricDefinitionsReq{})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
		d.defs.errOn = ""
	})
}
