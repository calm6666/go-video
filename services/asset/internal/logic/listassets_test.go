package logic

// listassets_test.go 覆盖读侧分页查询 ListAssets：pn/ps 守卫的先后与边界、
// 参数透传、过滤条件口径、排序/分页以 model 的 ORDER BY 为唯一事实源、
// total==0 时省掉第二条 SELECT、列表不碰缓存、两类下游故障。

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// seedAssetsWithIDs 按给定 ID 顺序布 rows 条媒资（同 mid、同 state，只让 asset_id 有差异）。
func seedAssetsWithIDs(t *testing.T, st *store, ids []int64, mid int64, state int32) {
	t.Helper()
	for _, id := range ids {
		m := fullAsset(id)
		m.Mid = mid
		m.State = state
		seedAsset(t, st, m)
	}
}

func TestListAssetsRejectsPageSizeBoundaries(t *testing.T) {
	cases := []struct {
		ps    int32
		want  error // nil 表示放行
		label string
	}{
		{ps: 1, label: "下边界放行"},
		{ps: 50, label: "上边界放行"},
		{ps: 51, want: model.ErrPsTooLarge, label: "越上边界"},
		{ps: 100, want: model.ErrPsTooLarge, label: "远超上边界"},
		{ps: 0, want: model.ErrPsTooLarge, label: "0 不补默认值"},
		{ps: -1, want: model.ErrPsTooLarge, label: "负数"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("ps=%d/%s", c.ps, c.label), func(t *testing.T) {
			st := newStore()
			seedAssetsWithIDs(t, st, []int64{123}, fakeMid, model.StateUploaded)
			l := NewListAssetsLogic(context.Background(), newTestSvc(st))

			got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Ps: c.ps})

			if c.want != nil {
				wantErrIdentity(t, "ListAssets", err, c.want)
				if got != nil {
					t.Errorf("应答 = %v, want nil", got)
				}
				wantNoCall(t, "ps 守卫", st, 0)
				return
			}
			wantNoErr(t, "ListAssets", err)
			// 放行时必须真的打到 COUNT：否则「放行」这一半没有判别力。
			wantCount(t, "ps 放行后查库", st.log, "meta.Count", 1)
		})
	}
}

// TestListAssetsNormalizesPnBeforeCheckingPs 钉守卫先后：pn 归一发生在 ps 判定之前，
// 而且归一是**就地改写调用方传进来的 message**——即便这次请求最终被拒。
func TestListAssetsNormalizesPnBeforeCheckingPs(t *testing.T) {
	st := newStore()
	seedAssetsWithIDs(t, st, []int64{123}, fakeMid, model.StateUploaded)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	in := &rpc.ListReq{Mid: fakeMid, Pn: 0, Ps: 51}
	got, err := l.ListAssets(in)
	wantErrIdentity(t, "ListAssets", err, model.ErrPsTooLarge)
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantEQ(t, "被拒请求仍被改写", "pn", in.GetPn(), int32(1))
	wantNoCall(t, "ps 守卫", st, 0)

	// 对照：pn=-7 同样归一到第 1 页，且请求合法时能读到数据。
	in2 := &rpc.ListReq{Mid: fakeMid, Pn: -7, Ps: 20}
	got2, err := l.ListAssets(in2)
	wantNoErr(t, "ListAssets pn=-7", err)
	wantEQ(t, "pn 归一", "pn", in2.GetPn(), int32(1))
	wantEQ(t, "pn=-7 等价第 1 页", "条数", len(got2.GetItems()), 1)
}

// TestListAssetsPassesArgsThroughWithoutClamping 钉 logic→model 的参数透传：
// ps 落在 [1,50] 时原样下传（model 里那条 ps→20 的钳制分支在 RPC 路径上不可达），
// pn 归一后的值下传，state 按 int32 转换下传。
func TestListAssetsPassesArgsThroughWithoutClamping(t *testing.T) {
	st := newStore()
	seedAssetsWithIDs(t, st, []int64{5001}, fakeMid, model.StateScanned)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, State: rpc.AssetState_STATE_SCANNED, Pn: 3, Ps: 7})
	wantNoErr(t, "ListAssets", err)
	wantOps(t, "参数透传", st.log.opsFrom(0), []string{
		"meta.Count:mid=" + itoa(fakeMid) + "/state=2",
		"meta.Select:mid=" + itoa(fakeMid) + "/state=2/pn=3/ps=7",
	})
	// 第 3 页（offset 14）越过唯一的命中行：列表空，但 total 是真命中数。
	wantEQ(t, "越界页", "total", got.GetTotal(), int32(1))
	wantEQ(t, "越界页", "条数", len(got.GetItems()), 0)
}

// TestListAssetsStateFilterOnlyAppliesWhenPositive 钉过滤口径：只有 state<=0 才不进 WHERE；
// 任何正值（含 proto 未定义的脏枚举）都是普通等值条件，等价「过滤后无匹配」。
func TestListAssetsStateFilterOnlyAppliesWhenPositive(t *testing.T) {
	cases := []struct {
		name      string
		state     rpc.AssetState
		wantCount int
	}{
		{"STATE_UNSPECIFIED 不过滤", rpc.AssetState_STATE_UNSPECIFIED, 5},
		{"按 SCANNED 过滤", rpc.AssetState_STATE_SCANNED, 1},
		{"按 UPLOADED 过滤", rpc.AssetState_STATE_UPLOADED, 4},
		{"未知值 7 是等值过滤（库里无 7）", rpc.AssetState(7), 0},
		{"负值 -1 不过滤", rpc.AssetState(-1), 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			// 4 条 UPLOADED + 1 条 SCANNED：不过滤时必须恰好是 5 条。
			seedAssetsWithIDs(t, st, []int64{1, 2, 3, 4}, fakeMid, model.StateUploaded)
			scanned := fullAsset(5)
			scanned.State = model.StateScanned
			seedAsset(t, st, scanned)

			l := NewListAssetsLogic(context.Background(), newTestSvc(st))
			got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, State: c.state, Pn: 1, Ps: 50})
			wantNoErr(t, "ListAssets", err)
			wantEQ(t, c.name, "total", got.GetTotal(), int32(c.wantCount))
			wantEQ(t, c.name, "条数", len(got.GetItems()), c.wantCount)
			// state 原样进查询参数（logic 不做归一/清洗），否则上面的计数说明不了什么。
			ops := st.log.opsFrom(0)
			if len(ops) == 0 {
				t.Fatalf("%s：一次依赖都没碰", c.name)
			}
			wantEQ(t, c.name, "COUNT 参数", ops[0],
				"meta.Count:mid="+itoa(fakeMid)+"/state="+itoa(int64(c.state)))
		})
	}
}

// TestListAssetsUnknownStateIsJustAnEqualityCondition 补一刀：state 传入 proto 未定义的正值
// 时不是「特殊值」，库里真躺着 7 就能被原样查出来（也说明本服务不清洗脏数据）。
func TestListAssetsUnknownStateIsJustAnEqualityCondition(t *testing.T) {
	st := newStore()
	seedAssetsWithIDs(t, st, []int64{6001}, fakeMid, model.StateUploaded)
	seedAssetsWithIDs(t, st, []int64{6002}, fakeMid, 7)
	seedAssetsWithIDs(t, st, []int64{6003}, fakeMid, model.StateScanned)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, State: rpc.AssetState(7), Pn: 1, Ps: 50})
	wantNoErr(t, "ListAssets state=7", err)
	wantSeq(t, "脏状态也能等值命中", lines(got.GetItems(), replyAssetLineForIDs), []string{"asset=6002"})
	wantEQ(t, "脏状态", "total", got.GetTotal(), int32(1))
}

// TestListAssetsMidIsOptionalFilterNotOwnershipCheck 钉现状：mid=0 时返回所有用户的媒资，
// 服务侧没有归属约束，租户隔离只能由调用方（网关）保证。
func TestListAssetsMidIsOptionalFilterNotOwnershipCheck(t *testing.T) {
	st := newStore()
	seedAssetsWithIDs(t, st, []int64{11}, fakeMid, model.StateUploaded)
	seedAssetsWithIDs(t, st, []int64{22}, fakeMidOther, model.StateUploaded)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	all, err := l.ListAssets(&rpc.ListReq{Pn: 1, Ps: 50})
	wantNoErr(t, "ListAssets 不带 mid", err)
	wantSeq(t, "mid=0 跨用户返回", lines(all.GetItems(), replyAssetLineForIDs),
		orderLines(t, orderByAssetsList, []int64{11, 22}))

	onlyMine, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: 1, Ps: 50})
	wantNoErr(t, "ListAssets 带 mid", err)
	wantSeq(t, "mid 过滤", lines(onlyMine.GetItems(), replyAssetLineForIDs),
		orderLines(t, orderByAssetsList, []int64{11}))
}

// replyAssetLineForIDs 只取 asset_id 序列：分页/顺序用例关心的是「哪些行、什么顺序」。
func replyAssetLineForIDs(r *rpc.AssetReply) string { return "asset=" + itoa(r.GetAssetId()) }

func orderLines(t *testing.T, clause string, ids []int64) []string {
	t.Helper()
	return lines(orderIDs(t, clause, ids), func(id int64) string { return "asset=" + itoa(id) })
}

// TestListAssetsOrdersByAssetIDDescPerModelSQL 钉排序：顺序完全来自 model 的
// ORDER BY asset_id DESC（logic 不重排、替身按登记表排），期望值由 orderIDs 现算。
func TestListAssetsOrdersByAssetIDDescPerModelSQL(t *testing.T) {
	st := newStore()
	seedIDs := []int64{1071, 1043, 1099, 1007, 1055} // 刻意乱序布景
	seedAssetsWithIDs(t, st, seedIDs, fakeMid, model.StateUploaded)

	wantIDs := orderIDs(t, orderByAssetsList, seedIDs)
	if slices.Equal(wantIDs, seedIDs) {
		t.Fatalf("布景顺序与期望顺序相同（%v）：本用例失去判别力", wantIDs)
	}

	l := NewListAssetsLogic(context.Background(), newTestSvc(st))
	got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: 1, Ps: 50})
	wantNoErr(t, "ListAssets", err)
	wantSeq(t, "ORDER BY asset_id DESC", lines(got.GetItems(), replyAssetLineForIDs),
		orderLines(t, orderByAssetsList, seedIDs))
	wantCount(t, "列表不碰缓存", st.log, "cache.", 0)
	// 整行投影逐项比对：顺序对了还得保证每行内容对得上自己的主键。
	wantSeq(t, "整行投影", lines(got.GetItems(), replyAssetLine),
		lines(wantIDs, func(id int64) string { return assetLine(st.meta.row(id)) }))
}

// TestListAssetsPaginatesByOffsetAndKeepsTotal 钉分页语义：offset=(pn-1)*ps、
// total 是全量过滤计数（不是本页条数）、翻到底之后是空页而不是回绕。
func TestListAssetsPaginatesByOffsetAndKeepsTotal(t *testing.T) {
	st := newStore()
	seedIDs := []int64{3001, 3002, 3003, 3004, 3005}
	seedAssetsWithIDs(t, st, seedIDs, fakeMid, model.StateUploaded)
	all := orderIDs(t, orderByAssetsList, seedIDs)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	for pn, wantPage := range [][]int64{all[0:2], all[2:4], all[4:5]} {
		got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: int32(pn + 1), Ps: 2})
		wantNoErr(t, "ListAssets", err)
		wantSeq(t, fmt.Sprintf("第 %d 页", pn+1), lines(got.GetItems(), replyAssetLineForIDs),
			orderLines(t, orderByAssetsList, wantPage))
		wantEQ(t, fmt.Sprintf("第 %d 页", pn+1), "total", got.GetTotal(), int32(len(all)))
	}

	// 第 4 页越界：空列表 + 真实 total，不回绕到第 1 页。
	got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: 4, Ps: 2})
	wantNoErr(t, "ListAssets 越界页", err)
	wantEQ(t, "越界页", "条数", len(got.GetItems()), 0)
	wantEQ(t, "越界页", "total", got.GetTotal(), int32(len(all)))
}

// TestListAssetsEmptyResultSkipsSecondQuery 钉 model 的两段查询语义：
// COUNT 为 0 时不再发第二条 SELECT，应答是空列表 + total=0，且不当成错误。
func TestListAssetsEmptyResultSkipsSecondQuery(t *testing.T) {
	st := newStore()
	seedAssetsWithIDs(t, st, []int64{11}, fakeMid, model.StateUploaded)
	l := NewListAssetsLogic(context.Background(), newTestSvc(st))

	got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, State: rpc.AssetState_STATE_TRANSCODED, Pn: 1, Ps: 20})
	wantNoErr(t, "ListAssets 空结果", err)
	wantEQ(t, "空结果", "total", got.GetTotal(), int32(0))
	wantEQ(t, "空结果", "条数", len(got.GetItems()), 0)
	wantOps(t, "COUNT=0 时不发第二条 SELECT", st.log.opsFrom(0),
		[]string{"meta.Count:mid=" + itoa(fakeMid) + "/state=3"})
}

func TestListAssetsDownstreamFailuresPropagateRaw(t *testing.T) {
	t.Run("COUNT 失败", func(t *testing.T) {
		st := newStore()
		seedAssetsWithIDs(t, st, []int64{11}, fakeMid, model.StateUploaded)
		st.meta.failWith("ListCount", errBoom)
		l := NewListAssetsLogic(context.Background(), newTestSvc(st))

		got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: 1, Ps: 20})
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantErrIs(t, "COUNT 失败上抛", err, errBoom)
		wantOps(t, "COUNT 失败顺序", st.log.opsFrom(0), []string{"meta.Count:mid=" + itoa(fakeMid) + "/state=0"})
	})

	t.Run("取行失败", func(t *testing.T) {
		st := newStore()
		seedAssetsWithIDs(t, st, []int64{11}, fakeMid, model.StateUploaded)
		st.meta.failWith("ListRows", errBoom)
		l := NewListAssetsLogic(context.Background(), newTestSvc(st))

		got, err := l.ListAssets(&rpc.ListReq{Mid: fakeMid, Pn: 1, Ps: 20})
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantErrIs(t, "取行失败上抛", err, errBoom)
		wantOps(t, "取行失败顺序", st.log.opsFrom(0), []string{
			"meta.Count:mid=" + itoa(fakeMid) + "/state=0",
			"meta.Select:mid=" + itoa(fakeMid) + "/state=0/pn=1/ps=20",
		})
	})
}
