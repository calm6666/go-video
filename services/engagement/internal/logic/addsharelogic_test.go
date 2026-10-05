package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestAddShareRejectsGuardsBeforeTouchingDeps oid 先于 mid（与 logic 的判定顺序一致）。
func TestAddShareRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.AddShareReq
		want error
	}{
		{"oid 为 0", &rpc.AddShareReq{Oid: 0, Mid: 7, Type: 2}, model.ErrInvalidOid},
		{"oid 为负", &rpc.AddShareReq{Oid: -1, Mid: 7, Type: 2}, model.ErrInvalidOid},
		{"mid 为 0（oid 合法）", &rpc.AddShareReq{Oid: 101, Mid: 0, Type: 2}, model.ErrInvalidMid},
		{"mid 为负", &rpc.AddShareReq{Oid: 101, Mid: -7, Type: 2}, model.ErrInvalidMid},
		{"两者都非法时 oid 优先", &rpc.AddShareReq{Oid: 0, Mid: 0}, model.ErrInvalidOid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()
			got, err := NewAddShareLogic(context.Background(), newTestSvc(st)).AddShare(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestAddShareRecordsOnceAndReturnsCount 正常路径逐字段：返回的 shares 必须是
// COUNT(*) 的真实结果，而不是「本次是否新增」这类布尔伪装。
func TestAddShareRecordsOnceAndReturnsCount(t *testing.T) {
	st := newStore()
	// 已有两个别人分享过同一对象：本次应当叠加成 3。
	seedShare(st, 10001, 88, 2, 20260901)
	seedShare(st, 10001, 99, 2, 20260901)
	// 同 oid 不同 tp 不算同一对象（CountByOid 的 WHERE 带 type）。
	seedShare(st, 10001, 77, 3, 20260901)

	got, err := NewAddShareLogic(context.Background(), newTestSvc(st)).
		AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantNoErr(t, "AddShare", err)
	if got == nil {
		t.Fatalf("AddShare() 响应 = nil")
	}
	wantOps(t, "AddShare", st.log.opsFrom(0), []string{
		"share.AddIfNotExists:10001:7:2",
		"share.CountByOid:10001:2",
	})
	wantEQ(t, "AddShare", "Shares（只数同 oid+tp）", got.Shares, int64(3))
	wantEQ(t, "AddShare", "share_log 行数", st.share.rowCount(), 4)
	if !st.share.existsToday(10001, 7, 2) {
		t.Errorf("AddShare：本次分享未落库（day 应为今天）")
	}
}

// TestAddShareIsIdempotentWithinDay 本域不变量：share_log 的唯一键是
// (oid, mid, tp, day)（对着 000003_create_share.sql 的 uniq_oid_mid_tp_day），
// 所以同一用户同一对象**当天**重复上报只计一次，count 不涨。
func TestAddShareIsIdempotentWithinDay(t *testing.T) {
	st := newStore()
	st.share.dayLock = 20260922 // 钉住「今天」，避免用例跨午夜抖动
	l := NewAddShareLogic(context.Background(), newTestSvc(st))
	in := &rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2}

	first, err := l.AddShare(in)
	wantNoErr(t, "首次分享", err)
	second, err := l.AddShare(in)
	wantNoErr(t, "重复分享", err)
	third, err := l.AddShare(in)
	wantNoErr(t, "第三次分享", err)

	wantEQ(t, "重复上报", "首次 shares", first.Shares, int64(1))
	wantEQ(t, "重复上报", "第二次 shares 不叠加", second.Shares, int64(1))
	wantEQ(t, "重复上报", "第三次 shares 不叠加", third.Shares, int64(1))
	wantEQ(t, "重复上报", "share_log 只有一行", st.share.rowCount(), 1)
	wantCount(t, "重复上报", st.log, "share.AddIfNotExists:", 3)
	wantOps(t, "重复上报", st.log.opsFrom(0), []string{
		"share.AddIfNotExists:10001:7:2", "share.CountByOid:10001:2",
		"share.AddIfNotExists:10001:7:2", "share.CountByOid:10001:2",
		"share.AddIfNotExists:10001:7:2", "share.CountByOid:10001:2",
	})
}

// TestAddShareCountsAgainNextDay 锁定幂等窗口的**边界**：幂等是按天，不是永久。
// 跨天后同一个人再分享会新增一行、count 变 2。这是设计口径（day 在唯一键里），
// 用例把「别把它改成永久去重」这件事钉住。
func TestAddShareCountsAgainNextDay(t *testing.T) {
	st := newStore()
	l := NewAddShareLogic(context.Background(), newTestSvc(st))
	in := &rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2}

	st.share.dayLock = 20260922
	first, err := l.AddShare(in)
	wantNoErr(t, "第一天", err)
	st.share.dayLock = 20260923
	second, err := l.AddShare(in)
	wantNoErr(t, "第二天", err)

	wantEQ(t, "跨天", "首次 shares", first.Shares, int64(1))
	wantEQ(t, "跨天", "次日 shares", second.Shares, int64(2))
	wantEQ(t, "跨天", "行数", st.share.rowCount(), 2)
}

// TestAddShareDifferentUsersBothCount 不同用户对同一对象各计一次（分享数是「人次」）。
func TestAddShareDifferentUsersBothCount(t *testing.T) {
	st := newStore()
	st.share.dayLock = 20260922
	l := NewAddShareLogic(context.Background(), newTestSvc(st))

	a, err := l.AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantNoErr(t, "用户 A", err)
	b, err := l.AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 8, Type: 2})
	wantNoErr(t, "用户 B", err)
	wantEQ(t, "分享人次", "A 之后", a.Shares, int64(1))
	wantEQ(t, "分享人次", "B 之后", b.Shares, int64(2))
}

// TestAddSharePropagatesInsertFailure 插入失败时不得再去查数、也不得返回 0 当成功。
func TestAddSharePropagatesInsertFailure(t *testing.T) {
	st := newStore()
	st.share.failWith("AddIfNotExists", errBoom)

	got, err := NewAddShareLogic(context.Background(), newTestSvc(st)).
		AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantFail(t, "插入失败", got, err, errBoom)
	wantOps(t, "插入失败后的调用", st.log.opsFrom(0), []string{"share.AddIfNotExists:10001:7:2"})
	wantEQ(t, "插入失败", "未落库", st.share.rowCount(), 0)
}

// TestAddSharePropagatesCountFailure 计数失败时**分享本身已经记上**：
// 因为 INSERT IGNORE 按天幂等，客户端重试不会双计，所以这个半截状态是可恢复的。
// 用例锁的就是「恢复路径成立」这件事。
func TestAddSharePropagatesCountFailure(t *testing.T) {
	st := newStore()
	st.share.dayLock = 20260922
	st.share.failWith("CountByOid", errBoom)

	got, err := NewAddShareLogic(context.Background(), newTestSvc(st)).
		AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantFail(t, "计数失败", got, err, errBoom)
	wantOps(t, "计数失败后的调用", st.log.opsFrom(0), []string{
		"share.AddIfNotExists:10001:7:2", "share.CountByOid:10001:2",
	})
	wantEQ(t, "计数失败", "分享已落库", st.share.rowCount(), 1)

	// 重试：INSERT IGNORE 不新增行，shares 回到真值 1（不是 2）。
	st2 := newStore()
	st2.share.dayLock = 20260922
	l := NewAddShareLogic(context.Background(), newTestSvc(st2))
	_, err = l.AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantNoErr(t, "首次", err)
	retry, err := l.AddShare(&rpc.AddShareReq{Oid: 10001, Mid: 7, Type: 2})
	wantNoErr(t, "重试", err)
	wantEQ(t, "重试", "shares 不双计", retry.Shares, int64(1))
}
