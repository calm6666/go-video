package logic

import (
	"context"
	"fmt"
	"testing"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 ListBlockWords（运营侧分页查询屏蔽词）。
//
// 这是读接口，要钉的三件事：
//  1. 匿名读必须被拒（operator_mid > 0），且拒绝发生在查库之前；
//  2. 作用域过滤参数的映射（UNSPECIFIED 不过滤 / GLOBAL / OID）与分页参数**原样下传**，
//     由 model 负责夹紧——逻辑层不许自创默认值，否则运营看到的页码与服务端口径不一致；
//  3. model 行到 rpc 的投影逐字段对齐，不许漏列、不许串列。
//
// 另外钉住一条边界：本方法只读 danmaku_blockword，**不许碰发送侧词库缓存**
// （dm:bw:* 是发送链路的视图，运营列表翻页不该把它清掉）。

const bwListOid = int64(3003)

func callListBlockWords(t *testing.T, e *env, in *rpc.ListBlockWordsReq) (*rpc.ListBlockWordsReply, error) {
	t.Helper()
	return NewListBlockWordsLogic(context.Background(), e.svcCtx).ListBlockWords(in)
}

func blListOp(scope int32, oid int64, onlyEnabled bool, pn, ps int32) string {
	return fmt.Sprintf("blockword.List:%d/%d/%t/%d/%d", scope, oid, onlyEnabled, pn, ps)
}

// seedThreeWords 布三条可区分的词条：1 全局生效、2 分区生效、3 分区停用。
// 自增主键按写入顺序为 501/502/503，List 按 word_id DESC 返回 ⇒ 顺序确定。
func seedThreeWords(t *testing.T, e *env) []*model.BlockWord {
	t.Helper()
	return []*model.BlockWord{
		seedBlockWord(t, e.st, &model.BlockWord{
			Word: "全局辱骂词", Scope: model.ScopeGlobal, State: model.BlockWordEnabled, Operator: 7101,
		}),
		seedBlockWord(t, e.st, &model.BlockWord{
			Word: "分区黑话", Scope: model.ScopeOid, Oid: bwListOid, State: model.BlockWordEnabled, Operator: 7102,
		}),
		seedBlockWord(t, e.st, &model.BlockWord{
			Word: "已停用词", Scope: model.ScopeOid, Oid: bwListOid, State: model.BlockWordDisabled, Operator: 7103,
		}),
	}
}

// --- 守卫 ---

// TestListBlockWordsRejectsInvalidRequests 守卫表：非法入参不许产生任何依赖调用。
func TestListBlockWordsRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListBlockWordsReq
		want error
	}{
		{"operator 缺失", &rpc.ListBlockWordsReq{Scope: rpc.BlockWordScope_SCOPE_GLOBAL}, model.ErrOperatorRequired},
		{"operator 为负", &rpc.ListBlockWordsReq{OperatorMid: -1}, model.ErrOperatorRequired},
		{"operator 非法优先于作用域非法", &rpc.ListBlockWordsReq{Scope: rpc.BlockWordScope(88)}, model.ErrOperatorRequired},
		{"未知作用域取值", &rpc.ListBlockWordsReq{OperatorMid: bwOperator, Scope: rpc.BlockWordScope(88)}, model.ErrInvalidBlockWord},
		{"未知作用域取值（负数）", &rpc.ListBlockWordsReq{OperatorMid: bwOperator, Scope: rpc.BlockWordScope(-1)}, model.ErrInvalidBlockWord},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedThreeWords(t, e)
			e.st.cache.warmBlockWords(0, "发送侧词库")
			before := e.st.log.snapshot()

			reply, err := callListBlockWords(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			// 守卫阶段不许把发送侧词库缓存清掉
			_, ok := e.st.cache.blockWordSet(0)
			wantEQ(t, tc.name, "dm:bw:0 未被清", ok, true)
		})
	}
}

// --- 正常路径 ---

// TestListBlockWordsProjectsEveryField 逐字段钉死 model→rpc 投影：
// 每条返回项的 8 个字段都要对上库里那一行，且顺序按 word_id DESC。
func TestListBlockWordsProjectsEveryField(t *testing.T) {
	e := newEnv(t)
	seeded := seedThreeWords(t, e)

	reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{
		Scope: rpc.BlockWordScope_SCOPE_UNSPECIFIED, OperatorMid: bwOperator, Pn: 1, Ps: 10,
	})
	wantNoErr(t, "运营列表首页", err)
	if reply == nil {
		t.Fatal("运营列表首页：回复 = nil, want 非空")
	}

	wantEQ(t, "运营列表首页", "条数", len(reply.Words), 3)
	wantEQ(t, "运营列表首页", "total（不过滤状态时含停用项）", reply.Total, int32(3))
	// DESC 顺序：503 已停用词 → 502 分区黑话 → 501 全局辱骂词
	wantInt64sEQ(t, "运营列表首页", []int64{reply.Words[0].WordId, reply.Words[1].WordId, reply.Words[2].WordId},
		[]int64{seeded[2].WordID, seeded[1].WordID, seeded[0].WordID})

	// 逐字段核对第一条（停用项）与第二条（分区项）
	check := func(label string, got *rpc.BlockWordInfo, want *model.BlockWord) {
		wantEQ(t, label, "word_id", got.WordId, want.WordID)
		wantEQ(t, label, "word", got.Word, want.Word)
		wantEQ(t, label, "scope", got.Scope, want.Scope)
		wantEQ(t, label, "oid", got.Oid, want.Oid)
		wantEQ(t, label, "state", got.State, want.State)
		wantEQ(t, label, "operator", got.Operator, want.Operator)
		wantEQ(t, label, "ctime", got.Ctime, want.Ctime)
		wantEQ(t, label, "mtime", got.Mtime, want.Mtime)
	}
	// seeded[i] 是 seedBlockWord 返回的**当时**的副本，ctime/mtime 由 Upsert 写入，
	// 这里回读库里的行作为权威期望值，避免拿旧副本比。
	rows := []*model.BlockWord{bwRow(t, e, "已停用词"), bwRow(t, e, "分区黑话"), bwRow(t, e, "全局辱骂词")}
	check("运营列表首行", reply.Words[0], rows[0])
	check("运营列表第二行", reply.Words[1], rows[1])
	check("运营列表第三行", reply.Words[2], rows[2])

	// 作用域不过滤 ⇒ 下传 scope=0；分页参数原样下传
	wantOps(t, "运营列表首页", e.ops(), []string{blListOp(0, 0, false, 1, 10)})
	// 运营列表是旁路读，不许动发送侧词库缓存
	wantCount(t, "运营列表首页", e.st.log, "cache.", 0)
	wantCount(t, "运营列表首页", e.st.log, "blockword.Upsert", 0)
}

// TestListBlockWordsScopeFilterMapping 作用域映射表：三种合法取值下传给 model 的过滤条件。
func TestListBlockWordsScopeFilterMapping(t *testing.T) {
	cases := []struct {
		name      string
		scope     rpc.BlockWordScope
		oid       int64
		wantScope int32
		wantWords []string
	}{
		{"不过滤", rpc.BlockWordScope_SCOPE_UNSPECIFIED, 0, 0, []string{"已停用词", "分区黑话", "全局辱骂词"}},
		{"只看全局", rpc.BlockWordScope_SCOPE_GLOBAL, 0, model.ScopeGlobal, []string{"全局辱骂词"}},
		{"只看某分区", rpc.BlockWordScope_SCOPE_OID, bwListOid, model.ScopeOid, []string{"已停用词", "分区黑话"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedThreeWords(t, e)

			reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{
				Scope: tc.scope, Oid: tc.oid, OperatorMid: bwOperator, Pn: 1, Ps: 10,
			})
			wantNoErr(t, tc.name, err)

			got := make([]string, 0, len(reply.Words))
			for _, w := range reply.Words {
				got = append(got, w.Word)
			}
			wantStringsEQ(t, tc.name, "返回词条", got, tc.wantWords)
			wantEQ(t, tc.name, "total 与列表同口径", reply.Total, int32(len(tc.wantWords)))
			wantOps(t, tc.name, e.ops(), []string{blListOp(tc.wantScope, tc.oid, false, 1, 10)})
		})
	}
}

// TestListBlockWordsOnlyEnabledFilter only_enabled 只影响过滤，不影响 total 与列表的口径一致性。
func TestListBlockWordsOnlyEnabledFilter(t *testing.T) {
	cases := []struct {
		name        string
		onlyEnabled bool
		wantWords   []string
		wantTotal   int32
	}{
		{"含停用项", false, []string{"已停用词", "分区黑话", "全局辱骂词"}, 3},
		{"仅生效项", true, []string{"分区黑话", "全局辱骂词"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedThreeWords(t, e)

			reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{
				OnlyEnabled: tc.onlyEnabled, OperatorMid: bwOperator, Pn: 1, Ps: 10,
			})
			wantNoErr(t, tc.name, err)
			got := make([]string, 0, len(reply.Words))
			for _, w := range reply.Words {
				got = append(got, w.Word)
			}
			wantStringsEQ(t, tc.name, "返回词条", got, tc.wantWords)
			wantEQ(t, tc.name, "total", reply.Total, tc.wantTotal)
			wantOps(t, tc.name, e.ops(), []string{blListOp(0, 0, tc.onlyEnabled, 1, 10)})
		})
	}
}

// TestListBlockWordsPaginationIsPassedThroughUnchanged 分页参数必须原样下传（由 model 夹紧）：
// pn=0/ps=0/ps=500 在 logic 里都不许被改写成别的默认值，否则页码语义会在两层之间漂。
func TestListBlockWordsPaginationIsPassedThroughUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		pn, ps      int32
		wantPage1   bool // model 夹紧后实际返回第 1 页
		wantAllRows int
	}{
		{"正常第 1 页 2 条", 1, 2, true, 2},
		{"页码 0（model 夹成 1）", 0, 2, true, 2},
		{"负页码（model 夹成 1）", -5, 2, true, 2},
		{"每页 0（model 夹成 20）", 1, 0, true, 3},
		{"每页 500（超上限，model 夹成 20）", 1, 500, true, 3},
		{"每页负数（model 夹成 20）", 1, -3, true, 3},
		{"越界页码返回空页但 total 不变", 9, 2, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedThreeWords(t, e)

			reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{OperatorMid: bwOperator, Pn: tc.pn, Ps: tc.ps})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "条数", len(reply.Words), tc.wantAllRows)
			// total 恒为过滤后的全量，不受分页影响
			wantEQ(t, tc.name, "total", reply.Total, int32(3))
			// 下传的仍是原始 pn/ps：夹紧发生在 model 里
			wantOps(t, tc.name, e.ops(), []string{blListOp(0, 0, false, tc.pn, tc.ps)})
			if !tc.wantPage1 && len(reply.Words) == 0 {
				// 空页必须是 nil 切片经 make 之后的空集合，而不是 nil（proto 侧要能安全遍历）
				if reply.Words == nil {
					t.Errorf("%s：空页 Words = nil, want 非 nil 空切片", tc.name)
				}
			}
		})
	}
}

// TestListBlockWordsEmptyResultIsNonNilSlice 词表为空时返回非 nil 空切片 + total 0，
// 并且不许因为「查无结果」报错。
func TestListBlockWordsEmptyResultIsNonNilSlice(t *testing.T) {
	e := newEnv(t)

	reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{OperatorMid: bwOperator, Pn: 1, Ps: 20})
	wantNoErr(t, "空词表", err)
	if reply == nil {
		t.Fatal("空词表：回复 = nil, want 非空")
	}
	if reply.Words == nil {
		t.Error("空词表：Words = nil, want 非 nil 空切片")
	}
	wantEQ(t, "空词表", "条数", len(reply.Words), 0)
	wantEQ(t, "空词表", "total", reply.Total, int32(0))
}

// TestListBlockWordsScopeOidWithoutOidListsAllPartitions 钉住现状缺陷：
// SCOPE_OID 但没有 oid 时，logic 不校验（对比 BlockWord 的 ADD 分支会拒 ErrInvalidOid），
// model 的 `oid > 0` 条件不成立 ⇒ 过滤条件退化成「只按 scope=2」，
// 一次翻页就把**所有分区**的词条全列出来了。
// 缺陷登记进 README 已知缺口；修法落地时本用例应改为断言 ErrInvalidOid。
func TestListBlockWordsScopeOidWithoutOidListsAllPartitions(t *testing.T) {
	e := newEnv(t)
	seedBlockWord(t, e.st, &model.BlockWord{Word: "本区词", Scope: model.ScopeOid, Oid: bwListOid, State: model.BlockWordEnabled, Operator: 7101})
	seedBlockWord(t, e.st, &model.BlockWord{Word: "别区词", Scope: model.ScopeOid, Oid: 4242, State: model.BlockWordEnabled, Operator: 7102})

	reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{
		Scope: rpc.BlockWordScope_SCOPE_OID, Oid: 0, OperatorMid: bwOperator, Pn: 1, Ps: 10,
	})
	wantNoErr(t, "SCOPE_OID 不带 oid（现状）", err)
	got := make([]string, 0, len(reply.Words))
	otherPartition := 0
	for _, w := range reply.Words {
		got = append(got, w.Word)
		if w.Oid != bwListOid {
			otherPartition++
		}
	}
	wantEQ(t, "SCOPE_OID 不带 oid（现状）", "越界读到的分区条目数", otherPartition, 1)
	wantStringsEQ(t, "SCOPE_OID 不带 oid（现状）", "返回词条", got, []string{"别区词", "本区词"})
	wantOps(t, "SCOPE_OID 不带 oid（现状）", e.ops(), []string{blListOp(model.ScopeOid, 0, false, 1, 10)})
}

// --- 下游故障 ---

// TestListBlockWordsPropagatesListFailure model 报错必须原样上抛：
// 运营列表不能把「查库失败」渲染成「一条词都没有」。
func TestListBlockWordsPropagatesListFailure(t *testing.T) {
	e := newEnv(t)
	seedThreeWords(t, e)
	e.st.blockWord.failWith("List", errDB)

	reply, err := callListBlockWords(t, e, &rpc.ListBlockWordsReq{OperatorMid: bwOperator, Pn: 1, Ps: 10})

	wantErrIs(t, "列表查询失败", err, errDB)
	if reply != nil {
		t.Errorf("列表查询失败仍返回 %+v", reply)
	}
	wantOps(t, "列表查询失败", e.ops(), []string{blListOp(0, 0, false, 1, 10)})
	// 失败路径同样不许碰发送侧缓存
	wantCount(t, "列表查询失败", e.st.log, "cache.", 0)
}
