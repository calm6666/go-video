package logic

// transitionstate_test.go 覆盖写侧状态机 TransitionState：两条入参守卫的先后、
// 合法边的完整依赖序列（读穿→写→失效→回读）、非法边「一字节都不写库」的判定，
// 以及三条现状风险：from 取自可能陈旧的缓存（可把已转码的行写回退）、
// 缓存里有幽灵行时写失败但不失效、失效失败被就地吞掉后状态推进对读侧不可见。
//
// 状态边界的唯一事实源是 model/errors.go 的 validTransitions；
// 用例按它列全 5 条合法边与 14 条非法边，而不是凭记忆挑几条。

import (
	"context"
	"strings"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// seedAssetInState 布一行处于指定状态的媒资（其余字段用 fullAsset 的全非零值）。
// 刻意让「探测字段已经有值」，这样状态推进有没有顺手改元数据就能被看出来。
func seedAssetInState(t *testing.T, st *store, assetID int64, state int32) *model.AssetMeta {
	t.Helper()
	m := fullAsset(assetID)
	m.State = state
	return seedAsset(t, st, m)
}

// coldReadOps 是缓存冷时 Repository.GetAsset 的读穿三步。
func coldReadOps(assetID int64) []string {
	return []string{opCacheGet(assetID), opMetaFindOne(assetID), opCacheSet(assetID)}
}

func TestTransitionStateRejectsBadArgsBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name    string
		req     *rpc.TransitionReq
		wantErr error
	}{
		{"asset_id=0 且 to=UNSPECIFIED：先判主键", &rpc.TransitionReq{AssetId: 0, ToState: rpc.AssetState_STATE_UNSPECIFIED}, model.ErrInvalidAssetID},
		{"asset_id=0", &rpc.TransitionReq{AssetId: 0, ToState: rpc.AssetState_STATE_SCANNED}, model.ErrInvalidAssetID},
		{"asset_id 负数", &rpc.TransitionReq{AssetId: -910001, ToState: rpc.AssetState_STATE_SCANNED}, model.ErrInvalidAssetID},
		{"to_state=UNSPECIFIED", &rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_UNSPECIFIED}, model.ErrInvalidState},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			seedAssetInState(t, st, fakeAssetID, model.StateUploaded) // 有可读行：失败只能来自守卫
			l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

			got, err := l.TransitionState(c.req)

			wantErrIdentity(t, "TransitionState", err, c.wantErr)
			if got != nil {
				t.Errorf("TransitionState(%s) 应答 = %v, want nil", c.name, got)
			}
			wantNoCall(t, "TransitionState 守卫", st, 0)
			wantEQ(t, "守卫拒绝", "库存 state", st.meta.row(fakeAssetID).State, int32(model.StateUploaded))
		})
	}

	// 正向对照。
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	l := NewTransitionStateLogic(context.Background(), newTestSvc(st))
	if _, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_SCANNED}); err != nil {
		t.Fatalf("合规请求被拒：%v", err)
	}
	wantCount(t, "合规请求真的推进了", st.log, "meta.UpdateState", 1)
}

// TestTransitionStateAllowsEveryLegalEdge 按 model.validTransitions 列全 5 条合法边，
// 并钉冷缓存下一次推进**恰好**打这 6 条依赖、按这个顺序：
// 读缓存→查库→回填（GetAsset 读穿）→ UPDATE state → DEL 缓存 → 再查库取新行做应答。
// 顺带钉「推进只改 state 与 mtime」：其余列一律保持布景值。
func TestTransitionStateAllowsEveryLegalEdge(t *testing.T) {
	edges := []struct {
		name string
		from int32
		to   rpc.AssetState
	}{
		{"UPLOADED→SCANNED", model.StateUploaded, rpc.AssetState_STATE_SCANNED},
		{"UPLOADED→FAILED", model.StateUploaded, rpc.AssetState_STATE_FAILED},
		{"SCANNED→TRANSCODED", model.StateScanned, rpc.AssetState_STATE_TRANSCODED},
		{"SCANNED→FAILED", model.StateScanned, rpc.AssetState_STATE_FAILED},
		{"TRANSCODED→FAILED", model.StateTranscoded, rpc.AssetState_STATE_FAILED},
	}
	for _, e := range edges {
		t.Run(e.name, func(t *testing.T) {
			st := newStore()
			before := seedAssetInState(t, st, fakeAssetID, e.from)
			l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

			writeAt := time.Now().Unix()
			got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: e.to})
			after := time.Now().Unix()

			wantNoErr(t, "TransitionState", err)
			wantOps(t, "一次推进的完整依赖序列", st.log.opsFrom(0), append(append(coldReadOps(fakeAssetID),
				opMetaUpdateState(fakeAssetID, int32(e.to)), opCacheDel(fakeAssetID)), opMetaFindOne(fakeAssetID)))

			row := st.meta.row(fakeAssetID)
			wantEQ(t, "库存新状态", "state", row.State, int32(e.to))
			wantEQ(t, "应答状态", "state", got.GetState(), e.to)
			wantEQ(t, "应答就是回读的那一行", "整行", replyAssetLine(got), assetLine(row))
			// 只动 state：其余列必须还是布景值（含同样是「写」的 ctime）。
			wantEQ(t, "不动元数据", "整行除状态外", strings.TrimSuffix(assetLine(row), "/state="+itoa(int64(row.State))),
				strings.TrimSuffix(assetLine(before), "/state="+itoa(int64(before.State))))
			wantEQ(t, "不动时间戳", "ctime", row.Ctime, before.Ctime)
			wantUnixWindow(t, "mtime 由 UPDATE 现取", "mtime", row.Mtime, writeAt, after)
			// 推进完必须留空缓存：下一次读才拿得到新状态。
			if _, ok := st.cache.stored(fakeAssetID); ok {
				t.Errorf("推进后详情缓存没被失效：%v", st.cache.storedAsset(fakeAssetID))
			}
		})
	}
}

// TestTransitionStateRejectsEveryIllegalEdgeWithoutWriting 按同一张迁移表取补集，
// 列全 14 条非法边（跳级、回退、自环、终态出边、脏当前状态、proto 未定义目标）。
// 每条都钉三件事：返回**未包装**的 ErrInvalidTransition（logic 用 == 短路，不写日志）、
// UPDATE 一次都没发生、库里那一行原封不动。
// 注意后两条：to=7 / to=-1 在守卫处**不被**当成非法状态（只判 ==UNSPECIFIED），
// 它们是靠状态机被拒的，所以报 ErrInvalidTransition 而不是 ErrInvalidState。
func TestTransitionStateRejectsEveryIllegalEdgeWithoutWriting(t *testing.T) {
	const dirty = int32(7) // TINYINT 没有 CHECK 约束，库里可能出现枚举外的值
	cases := []struct {
		name string
		from int32
		to   rpc.AssetState
	}{
		{"UPLOADED→TRANSCODED 跳级", model.StateUploaded, rpc.AssetState_STATE_TRANSCODED},
		{"UPLOADED→UPLOADED 自环", model.StateUploaded, rpc.AssetState_STATE_UPLOADED},
		{"SCANNED→UPLOADED 回退", model.StateScanned, rpc.AssetState_STATE_UPLOADED},
		{"SCANNED→SCANNED 自环", model.StateScanned, rpc.AssetState_STATE_SCANNED},
		{"TRANSCODED→UPLOADED 回退", model.StateTranscoded, rpc.AssetState_STATE_UPLOADED},
		{"TRANSCODED→SCANNED 回退", model.StateTranscoded, rpc.AssetState_STATE_SCANNED},
		{"TRANSCODED→TRANSCODED 自环", model.StateTranscoded, rpc.AssetState_STATE_TRANSCODED},
		{"FAILED→UPLOADED 终态出边", model.StateFailed, rpc.AssetState_STATE_UPLOADED},
		{"FAILED→SCANNED 终态出边", model.StateFailed, rpc.AssetState_STATE_SCANNED},
		{"FAILED→TRANSCODED 终态出边", model.StateFailed, rpc.AssetState_STATE_TRANSCODED},
		{"FAILED→FAILED 终态自环", model.StateFailed, rpc.AssetState_STATE_FAILED},
		{"库里 0（UNSPECIFIED）→SCANNED", model.StateUnspecified, rpc.AssetState_STATE_SCANNED},
		{"库里脏值 7→FAILED", dirty, rpc.AssetState_STATE_FAILED},
		{"库里脏值 7→脏值 8", dirty, rpc.AssetState(8)},
		{"UPLOADED→未定义目标 7", model.StateUploaded, rpc.AssetState(7)},
		{"UPLOADED→负目标 -1", model.StateUploaded, rpc.AssetState(-1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			before := seedAssetInState(t, st, fakeAssetID, c.from)
			l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

			got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: c.to})

			wantErrIdentity(t, "非法推进", err, model.ErrInvalidTransition)
			if got != nil {
				t.Errorf("应答 = %v, want nil", got)
			}
			// 判定发生在 UPDATE 之前：整次调用只有读穿三步。
			wantOps(t, "非法边不写库", st.log.opsFrom(0), coldReadOps(fakeAssetID))
			wantCount(t, "非法边不写库", st.log, "meta.UpdateState", 0)
			wantEQ(t, "非法边", "库存 state", st.meta.row(fakeAssetID).State, c.from)
			wantEQ(t, "非法边", "库存 mtime", st.meta.row(fakeAssetID).Mtime, before.Mtime)
			// 残留形态：读穿那一步已经把旧状态回填进缓存，所以脏值 7 这类
			// 「永远推不动」的行会一直从缓存里被读出来（读侧不做值域校验）。
			cached := st.cache.storedAsset(fakeAssetID)
			if cached == nil {
				t.Fatalf("读穿回填没发生，非法边用例的前提不成立")
			}
			wantEQ(t, "残留缓存仍是旧状态", "state", cached.State, c.from)
		})
	}
}

// TestTransitionStateTakesFromFromCacheAndCanRewindTheRow 钉 README 缺口 3 的现状：
// from 来自 GetAsset（可能是最长 60 秒前的缓存），UPDATE 里又没有任何
// `AND state = ?` 条件，于是「库里已 TRANSCODED、缓存里还写 UPLOADED」时，
// 对库里并不合法的 TRANSCODED→SCANNED 会被当成合法的 UPLOADED→SCANNED **写成功**，
// 状态被回退一格，而应答读的是回写后的新行，看起来完全自洽——并发丢写无法被发现。
func TestTransitionStateTakesFromFromCacheAndCanRewindTheRow(t *testing.T) {
	st := newStore()
	fresh := seedAssetInState(t, st, fakeAssetID, model.StateTranscoded)
	stale := *fresh
	stale.State = model.StateUploaded // 另一个 worker 60 秒前读到的样子
	st.cache.warmAsset(&stale)

	l := NewTransitionStateLogic(context.Background(), newTestSvc(st))
	got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_SCANNED})
	wantNoErr(t, "陈旧 from 下的推进（现状：被接受）", err)

	// 判定用的 from 从未回库：整次调用只有一次 FindOne，而且是写之后的回读。
	wantOps(t, "from 取自缓存", st.log.opsFrom(0), []string{
		opCacheGet(fakeAssetID),
		opMetaUpdateState(fakeAssetID, model.StateScanned),
		opCacheDel(fakeAssetID),
		opMetaFindOne(fakeAssetID),
	})
	wantCount(t, "写前不查库", st.log, "meta.FindOne", 1)
	wantEQ(t, "库里被回退", "state", st.meta.row(fakeAssetID).State, int32(model.StateScanned))
	wantEQ(t, "应答", "state", got.GetState(), rpc.AssetState_STATE_SCANNED)
	// 应答与库存一致 ⇒ 调用方拿不到任何「你覆盖别人了」的信号。
	wantEQ(t, "应答自洽（丢写不可见）", "整行", replyAssetLine(got), assetLine(st.meta.row(fakeAssetID)))
}

// TestTransitionStateMissingRowReturnsSentinelAndKeepsPhantomCache 钉两条「不存在」路径：
//  1. 冷缓存：读穿查不到 ⇒ 裸 model.ErrAssetNotFound，UPDATE 与失效都不发生。
//  2. 幽灵缓存（key 在、库里没这行）：判定照样通过并尝试 UPDATE，UPDATE 返回
//     ErrAssetNotFound 时 Repository **已经过了 DelAsset 之前的 return**（repository.go:191-194），
//     于是幽灵 key 留着：读侧继续「查得到」这台不存在的媒资，推进也继续失败。
func TestTransitionStateMissingRowReturnsSentinelAndKeepsPhantomCache(t *testing.T) {
	t.Run("冷缓存：读穿即失败", func(t *testing.T) {
		st := newStore()
		seedAssetInState(t, st, fakeAssetID+1, model.StateUploaded) // 只有邻居
		l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

		got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_SCANNED})
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantErrIdentity(t, "媒资不存在", err, model.ErrAssetNotFound)
		wantOps(t, "失败即止", st.log.opsFrom(0), []string{opCacheGet(fakeAssetID), opMetaFindOne(fakeAssetID)})
		wantCount(t, "不存在不写库", st.log, "meta.UpdateState", 0)
		wantCount(t, "不存在不失效", st.log, "cache.Del", 0)
	})

	t.Run("幽灵缓存：写失败但缓存不清", func(t *testing.T) {
		st := newStore()
		st.cache.warmAsset(&model.AssetMeta{
			AssetID: fakeAssetID, UploadID: fakeUploadID, Mid: fakeMid,
			Bucket: fakeBucket, ObjectKey: fakeOriginKey, State: model.StateUploaded,
			Ctime: fakeCtime, Mtime: fakeCtime,
		})
		l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

		got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_SCANNED})
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		// 是 ErrAssetNotFound 而不是 ErrInvalidTransition：边合法、行不存在。
		wantErrIdentity(t, "幽灵行推进", err, model.ErrAssetNotFound)
		wantOps(t, "UPDATE 失败即止", st.log.opsFrom(0), []string{
			opCacheGet(fakeAssetID), opMetaUpdateState(fakeAssetID, model.StateScanned),
		})
		wantCount(t, "失败不失效缓存", st.log, "cache.Del", 0)
		if st.cache.storedAsset(fakeAssetID) == nil {
			t.Fatalf("幽灵缓存被清掉了：说明写侧开始失效缓存，本用例与 README 缺口都要改")
		}

		// 后果：同一 asset_id 的详情读仍然成功，且永远推不动。
		detail, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: fakeAssetID})
		wantNoErr(t, "幽灵缓存仍被读到", err)
		wantEQ(t, "幽灵缓存", "state", detail.GetState(), rpc.AssetState_STATE_UPLOADED)
		wantCount(t, "幽灵缓存不查库", st.log, "meta.FindOne", 0)
	})
}

// TestTransitionStateDelFailureIsSwallowedAndStaleStateKeepsServing 钉写侧失效的静默失败：
// repository.go:194 是 `_ = r.cache.DelAsset(...)`，没有日志也没有重试。
// 于是「库里已推进、缓存里还是旧状态」可以持续到 TTL 结束，而这次调用返回成功。
func TestTransitionStateDelFailureIsSwallowedAndStaleStateKeepsServing(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	st.cache.failWith("DelAsset", errBoom)
	l := NewTransitionStateLogic(context.Background(), newTestSvc(st))

	got, err := l.TransitionState(&rpc.TransitionReq{AssetId: fakeAssetID, ToState: rpc.AssetState_STATE_SCANNED})
	wantNoErr(t, "失效失败不该让推进失败", err)
	wantEQ(t, "推进结果", "库存 state", st.meta.row(fakeAssetID).State, int32(model.StateScanned))
	wantEQ(t, "推进结果", "应答 state", got.GetState(), rpc.AssetState_STATE_SCANNED)
	wantOps(t, "失效确实尝试过", st.log.opsFrom(0), append(append(coldReadOps(fakeAssetID),
		opMetaUpdateState(fakeAssetID, model.StateScanned), opCacheDel(fakeAssetID)), opMetaFindOne(fakeAssetID)))

	// 残留形态：缓存里是**推进前**读穿回填的那一行。
	cached := st.cache.storedAsset(fakeAssetID)
	if cached == nil {
		t.Fatalf("缓存里没有回填行：DelAsset 注入失败却删掉了，替身漏实现")
	}
	wantEQ(t, "陈旧缓存", "state", cached.State, int32(model.StateUploaded))

	after, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "失效失败后读详情", err)
	wantEQ(t, "失效失败对读侧的后果", "state", after.GetState(), rpc.AssetState_STATE_UPLOADED)
	wantCount(t, "读侧仍不回源", st.log, "meta.FindOne", 2) // 推进前的那次读穿 + 写后回读，没有第三次
}

// TestTransitionStateHasNoOwnershipInput 钉契约层面的事实：TransitionReq 只带
// asset_id/to_state/ip（rpc/asset.proto:83-87），没有任何调用方身份或归属参数，
// logic 也不做归属判定，所以任意两个用户的媒资都能被同一次调用推进。
// 授权只能在网关或调用方，asset 侧没有第二道锁。
func TestTransitionStateHasNoOwnershipInput(t *testing.T) {
	st := newStore()
	mine := fullAsset(fakeAssetID)
	mine.State, mine.Mid = model.StateUploaded, fakeMid
	seedAsset(t, st, mine)
	theirs := fullAsset(fakeAssetID + 1)
	theirs.State, theirs.Mid = model.StateUploaded, fakeMidOther
	seedAsset(t, st, theirs)

	l := NewTransitionStateLogic(context.Background(), newTestSvc(st))
	wantMids := map[int64]int64{fakeAssetID: fakeMid, fakeAssetID + 1: fakeMidOther}
	for id, wantMid := range wantMids {
		got, err := l.TransitionState(&rpc.TransitionReq{AssetId: id, ToState: rpc.AssetState_STATE_SCANNED})
		wantNoErr(t, "推进他人媒资（现状）", err)
		wantEQ(t, "两个 mid 都照样推进", "库存 state", st.meta.row(id).State, int32(model.StateScanned))
		// 归属字段既没被读来判定，也没被改写。
		wantEQ(t, "应答 mid 就是库存归属", "应答 mid", got.GetMid(), wantMid)
		wantEQ(t, "库存 mid 未被改写", "库存 mid", st.meta.row(id).Mid, wantMid)
	}
	// 两次调用各自「读穿 1 次 + 写后回读 1 次」，没有第三次读、也没有任何按 mid 的查询。
	wantCount(t, "归属检查不发生任何额外读", st.log, "meta.FindOne", 4)
	wantCount(t, "不按 mid 过滤", st.log, "meta.Count", 0)
	wantCount(t, "不按 mid 过滤", st.log, "meta.Select", 0)
}

// TestTransitionStateUpdateSQLHasNoStateGuard 是源码对账，不是行为验证：
// 「并发推进不丢」这件事内存替身证明不了（替身按 asset_id 唯一命中），
// 所以这里锁住生产 SQL 的 WHERE 只有主键、没有 CAS 条件——
// 上面那条 TRANSCODED→SCANNED 回退用例的根因就是它。
// 哪天 UPDATE 加上 `AND state = ?` 并按 RowsAffected 判冲突，本用例先红，
// 提醒同时删除/改写陈旧 from 那组用例与 README 缺口 3。
func TestTransitionStateUpdateSQLHasNoStateGuard(t *testing.T) {
	src := readSource(t, modelSourcePath)

	const wantSQL = "UPDATE asset_meta SET state = ?, mtime = ? WHERE asset_id = ?"
	if !strings.Contains(src, wantSQL) {
		t.Errorf("model/assetmodel.go 里找不到 UpdateState 的 `%s`：推进 SQL 变了，先对齐缺口 3 与陈旧 from 用例", wantSQL)
	}
	if n := strings.Count(src, "WHERE asset_id = ? AND"); n != 0 {
		t.Errorf("model 里出现了 %d 条带附加条件的 `WHERE asset_id = ? AND ...`：状态机已加 CAS，本用例与缺口 3 要一起收口", n)
	}
	// 合法性判定只能在 repository 层（SQL 里没有 state 的等值条件可读）。
	if strings.Contains(src, "AND state = ? ") {
		t.Errorf("UpdateState/UpdateMeta 的 SQL 里出现了 state 条件：判定位置变了")
	}
}
