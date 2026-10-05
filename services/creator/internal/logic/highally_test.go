package logic

// highally_test.go 覆盖 GetHighAllyUps（高能联盟签约批量查询，直打 DB、不走缓存）。
//
// 钉住的结论：
//   - 空 mids 在 logic 就短路（零依赖调用），应答是空 map 而不是 nil；
//   - 批量不缓存：两次相同请求各发一次 SQL，路径上没有任何缓存调用——
//     所以这里既不会读到旧快照，也不会把结果预热给别的域（cache.go:23 的 up:ha:%d 至今无人使用）；
//   - 应答 map 的键集合＝「被查且库里真有行」的 mid 集合：缺失的 mid **缺席**，
//     绝不出现 state=0 的占位值（否则调用方会把「查无此人」读成「已终止的签约」）；
//   - 本方法**不按 state 或时间窗过滤**：到期/终止的签约照原样返回，判定归调用方；
//   - 错误原样透传且**不带归属前缀**（repository.go:252 是裸 return，与 UpSpecial/UpAttr 不同），
//     用例把这一点也钉住，避免有人改成 `%w` 包装后调用方的错误分类悄悄变化；
//   - 缺陷 D4：mids 长度无上限（对照 UpsSpecial 的 100），一次请求能把任意长 IN 列表打到 MySQL。
//
// 缺陷登记见 fakes_test.go 文件头。

import (
	"context"
	"strings"
	"testing"

	"go-video/services/creator/rpc"
)

const (
	allyMid  = int64(31)
	allyMid2 = int64(32)
)

func TestGetHighAllyUpsEmptyBatchShortCircuits(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)
	before := st.log.snapshot()

	for name, in := range map[string]*rpc.HighAllyUpsReq{
		"nil mids": {},
		"空切片":      {Mids: []int64{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).GetHighAllyUps(in)
			wantNoErr(t, name, err)
			lists := got.GetLists()
			if lists == nil {
				t.Fatalf("应答 map = nil, want 空 map（调用方 range 前不该判 nil）")
			}
			wantEQ(t, "短路应答不含任何行", "map 大小", len(lists), 0)
		})
	}
	// 短路必须发生在触库之前：一条 SQL 都不该发出去。
	wantNoCallAfter(t, "空批量", st.log, before)
	wantCacheUntouched(t, "空批量", st.log)
}

func TestGetHighAllyUpsProjectsAllFourFields(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 1_700_000_000, 1_800_000_000)
	st.seedSignUp(allyMid2, 2, 1_600_000_111, 1_699_999_888) // 另一份不同窗口的签约

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid, allyMid2}})
	wantNoErr(t, "批量", err)
	lists := got.GetLists()
	wantEQ(t, "两行都在", "map 大小", len(lists), 2)

	for mid, want := range map[int64]*rpc.SignUp{
		allyMid:  {Mid: allyMid, State: 1, BeginDate: 1_700_000_000, EndDate: 1_800_000_000},
		allyMid2: {Mid: allyMid2, State: 2, BeginDate: 1_600_000_111, EndDate: 1_699_999_888},
	} {
		row := lists[mid]
		if row == nil {
			t.Fatalf("mid=%d 的值是 nil（或缺席）", mid)
		}
		wantEQ(t, "mid", "Mid", row.GetMid(), want.Mid)         // 键与值里的 mid 必须一致
		wantEQ(t, "state", "State", row.GetState(), want.State) // 签约状态原样透出，不做映射
		wantEQ(t, "begin_date", "BeginDate", row.GetBeginDate(), want.BeginDate)
		wantEQ(t, "end_date", "EndDate", row.GetEndDate(), want.EndDate)
	}
	wantSeq(t, "一次 IN 查询搞定两行", st.log, 0, "sign_up.FindMany:n=2")
	wantInt64sEQ(t, "IN 列表原样透传", "mids", st.sign.findManyCalls[0], []int64{allyMid, allyMid2})
}

// TestGetHighAllyUpsMissingMidIsAbsentNotZeroFilled 是本文件最要紧的分辨性用例：
// 混合「有签约 / 没签约 / 非法 mid」的请求走的是逐条部分成功，
// 缺失的 mid 必须从 map 里缺席——出现 state=0 的占位值会被读成「已终止的签约」。
func TestGetHighAllyUpsMissingMidIsAbsentNotZeroFilled(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid, 999, 0, -5}})
	wantNoErr(t, "含非法/缺失 mid 的批量", err)
	lists := got.GetLists()
	wantEQ(t, "只有真存在的 mid 进 map", "map 大小", len(lists), 1)
	if _, ok := lists[allyMid]; !ok {
		t.Errorf("有签约的 mid=%d 反而缺席：%v", allyMid, lists)
	}
	for _, absent := range []int64{999, 0, -5} {
		row, ok := lists[absent]
		if ok {
			t.Errorf("mid=%d 不该出现在应答里，却有值 %#v", absent, row)
		}
	}
	// 非法 mid 不报错也不被过滤：原样进 IN 列表（logic 无守卫，见缺陷 D3/D4 同族）。
	wantInt64sEQ(t, "IN 列表含非法 mid", "mids", st.sign.findManyCalls[0], []int64{allyMid, 999, 0, -5})
}

// TestGetHighAllyUpsDoesNotFilterByStateOrWindow 钉「判定归调用方」：
// 到期（state=2 且 end_date 早已过去）与终止（state=3）的行照原样返回，
// logic 里既没有 now 参与，也没有按 state 丢弃行。
func TestGetHighAllyUpsDoesNotFilterByStateOrWindow(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200) // 生效中（窗口是历史值，本方法不看时间）
	st.seedSignUp(allyMid2, 2, 1, 2)    // 已到期
	other := int64(33)
	st.seedSignUp(other, 3, 0, 0) // 已终止，且窗口全 0

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid, allyMid2, other}})
	wantNoErr(t, "混合状态", err)
	lists := got.GetLists()
	wantEQ(t, "三种状态都返回", "map 大小", len(lists), 3)
	wantEQ(t, "生效中", "state", lists[allyMid].GetState(), int32(1))
	wantEQ(t, "已到期（未被丢掉）", "state", lists[allyMid2].GetState(), int32(2))
	wantEQ(t, "已终止（未被丢掉）", "state", lists[other].GetState(), int32(3))
	wantEQ(t, "窗口零值原样透出", "end_date", lists[other].GetEndDate(), int64(0))
}

// TestGetHighAllyUpsNeverTouchesCacheAndIsNotIdempotentlyCached 钉批量策略：
// 每次请求都实打实发一条 SQL，一次缓存都不碰（重复提交的**结果**一致，但**轨迹**也一致）。
func TestGetHighAllyUpsNeverTouchesCacheAndIsNotIdempotentlyCached(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)
	in := &rpc.HighAllyUpsReq{Mids: []int64{allyMid}}
	l := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx())

	first, err := l.GetHighAllyUps(in)
	wantNoErr(t, "首次", err)
	before := st.log.snapshot()
	second, err := l.GetHighAllyUps(in)
	wantNoErr(t, "重复提交", err)

	wantSeq(t, "重复提交的轨迹与首次一致（没有「第二次改走缓存」的分支）", st.log, before,
		"sign_up.FindMany:n=1")
	if a, b := first.String(), second.String(); a != b {
		t.Errorf("重复提交应答不一致：首次 %s, 重复 %s", a, b)
	}
	wantMethodCount(t, "两次两条 SQL", st.log, "sign_up.FindMany", 2)
	wantCacheUntouched(t, "批量路径", st.log)
	if keys := st.cache.keysSorted(); len(keys) != 0 {
		t.Errorf("缓存里出现了键 %v，与「批量不缓存」的策略冲突", keys)
	}
}

// TestGetHighAllyUpsAcceptsUnboundedMids pin 缺陷 D4：
// GetHighAllyUps 对 mids 长度**没有上限**，1000 个 mid 也照样展开成一条 IN 查询打到 MySQL；
// 同包的 UpsSpecial 在 101 个就整批拒绝（errTooManyMids），两者口径不一致。
// 未修原因：上限取多少属容量决策，需与调用方一起定（见 fakes_test.go D4）。
func TestGetHighAllyUpsAcceptsUnboundedMids(t *testing.T) {
	const n = 1000
	st := newStore()
	mids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		mids = append(mids, int64(i+1))
	}

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: mids})
	wantNoErr(t, "pin D4：1000 个 mid 不报错", err)
	wantEQ(t, "pin D4：整批照打 DB", "map 大小", len(got.GetLists()), 0)
	wantSeq(t, "pin D4：一条超长 IN 查询", st.log, 0, "sign_up.FindMany:n=1000")
	wantEQ(t, "IN 列表长度未被裁剪", "mids", len(st.sign.findManyCalls[0]), n)

	// 对照面：同样的长度在 UpsSpecial 里是整批拒绝，且拒绝前零依赖调用。
	before := st.log.snapshot()
	_, err = NewUpsSpecialLogic(context.Background(), st.svcCtx()).UpsSpecial(&rpc.UpsSpecialReq{Mids: mids})
	wantErrIs(t, "对照 UpsSpecial 的上限", err, errTooManyMids)
	wantNoCallAfter(t, "对照 UpsSpecial 的上限", st.log, before)
}

// TestGetHighAllyUpsDuplicateMidsCollapse 锁 IN 的集合语义：
// 重复 mid 既不翻倍结果也不报错，应答 map 只有一个键。
func TestGetHighAllyUpsDuplicateMidsCollapse(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid, allyMid, allyMid}})
	wantNoErr(t, "重复 mid", err)
	wantEQ(t, "应答键数", "map 大小", len(got.GetLists()), 1)
	wantEQ(t, "只有一个 mid 值", "mid", got.GetLists()[allyMid].GetMid(), allyMid)
	wantInt64sEQ(t, "入参未被去重（去重是 SQL 集合语义做的）", "mids",
		st.sign.findManyCalls[0], []int64{allyMid, allyMid, allyMid})
}

func TestGetHighAllyUpsDBFailurePropagatesVerbatim(t *testing.T) {
	st := newStore()
	st.sign.failWith("FindMany", errFakeDB)

	got, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid}})
	wantErrIs(t, "DB 失败原样透传", err, errFakeDB)
	// 本方法**不加归属前缀**（repository.go:252 裸 return，与 UpSpecial/UpAttr 的 `fmt.Errorf` 不同）。
	// 这条断言故意写成字面相等：谁将来加了包装，这里会红，逼着调用方的错误分类一起复核。
	if err != nil && err.Error() != "test: mysql unavailable" {
		t.Errorf("错误信息被改写了：%q, want 原样 %q", err.Error(), "test: mysql unavailable")
	}
	if got != nil {
		t.Errorf("应答 = %#v, want nil（绝不能降级成空 map）", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "sign_up.FindMany:n=1")
	wantCacheUntouched(t, "失败路径", st.log)
}

// TestGetHighAllyUpsSecondPageFailureOnly 用「第 2 次调用才炸」证明两条请求彼此独立：
// 第一次的成功不会让第二次跳过 SQL，失败也不会污染第一次的应答。
func TestGetHighAllyUpsSecondCallFailure(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)
	st.sign.failOn("FindMany", 2, errFakeDB)
	l := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx())
	in := &rpc.HighAllyUpsReq{Mids: []int64{allyMid}}

	first, err := l.GetHighAllyUps(in)
	wantNoErr(t, "首次", err)
	wantEQ(t, "首次拿到行", "map 大小", len(first.GetLists()), 1)

	got, err := l.GetHighAllyUps(in)
	wantErrIs(t, "第二次才发作的 DB 故障", err, errFakeDB)
	if got != nil {
		t.Errorf("第二次应答 = %#v, want nil", got)
	}
	wantSeq(t, "两次都发了 SQL", st.log, 0, "sign_up.FindMany:n=1", "sign_up.FindMany:n=1")
	wantEQ(t, "首次应答未被第二次改写", "map 大小", len(first.GetLists()), 1)
}

// TestGetHighAllyUpsOnlyTouchesSignUp 确认数据域边界（AGENTS.md §5）：
// 签约查询只读 sign_up，不越界读别的域，也不写任何缓存。
func TestGetHighAllyUpsOnlyTouchesSignUp(t *testing.T) {
	st := newStore()
	st.seedSignUp(allyMid, 1, 100, 200)
	st.seedSpecial(allyMid, 3)
	st.seedAttr(allyMid, 0, 1)
	st.seedSwitch(allyMid, 0, 1)

	if _, err := NewGetHighAllyUpsLogic(context.Background(), st.svcCtx()).
		GetHighAllyUps(&rpc.HighAllyUpsReq{Mids: []int64{allyMid}}); err != nil {
		t.Fatalf("GetHighAllyUps：%v", err)
	}
	for _, o := range st.log.ops {
		if !strings.HasPrefix(o, "sign_up.") {
			t.Errorf("签约路径越界触达：%s", o)
		}
	}
	wantEQ(t, "行数未变（读方法不得写库）", "库存", st.counts().sign, 1)
	wantCacheUntouched(t, "签约路径", st.log)
}
