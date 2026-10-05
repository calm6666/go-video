package logic

// nickupdatedlogic_test.go 覆盖 NickUpdated 与 IsInMonitor 两个「布尔结论」接口。
// 它们最贵的失效模式是**把「查不到」说成「是」**：
//   - NickUpdated 误报 true → 用户被判定为「已改过昵称」，改名入口被锁死；
//   - IsInMonitor 误报 true → 普通用户被当成监控对象，下游风控会凭空收紧策略。
//
// 因此两侧都按「位/软删除」判定：无行、位未置位、已软删除三种情况一律 false 且**不是错误**；
// 只有下游真故障才允许 err != nil，且此时 reply 必须为 nil（logic 不得把 false 当成功返回）。
// 另外钉住这两个接口都**没有缓存层**（纯 DB 读），加了缓存就是口径变更，必须改这里。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestNickUpdatedReadsExactlyOneFlagBit(t *testing.T) {
	e := newEnv(t)
	e.st.flag.put(90201, uint(model.NickUpdated))

	l := NewNickUpdatedLogic(context.Background(), e.svcCtx)
	reply, err := l.NickUpdated(&rpc.MemberMidReq{Mid: 90201})
	wantNoErr(t, "已改过昵称", err)
	wantEQ(t, "已改过昵称", "nick_updated", reply.GetNickUpdated(), true)
	wantOps(t, "已改过昵称", e.ops(0), []string{"flag.HasAttr:90201/1"})
	// 一次读、零缓存：加了缓存层就是口径变更。
	if n := e.st.log.countPrefix("cache."); n != 0 {
		t.Errorf("已改过昵称：触碰了缓存 %d 次，want 0", n)
	}
}

func TestNickUpdatedOtherBitsAndMissingRowAreFalseNotError(t *testing.T) {
	cases := []struct {
		label string
		flag  uint
		set   bool
	}{
		{"无 flag 行（新用户）", 0, false},
		{"只置了别的位（bit2）", uint(1 << 1), false},
		{"别的位 + 目标位", uint(1<<1 | 1<<3 | model.NickUpdated), true},
		{"目标位重复置位仍是 true", uint(model.NickUpdated | model.NickUpdated), true},
	}
	for _, tc := range cases {
		e := newEnv(t)
		if tc.set || tc.flag != 0 {
			e.st.flag.put(90202, tc.flag)
		}
		l := NewNickUpdatedLogic(context.Background(), e.svcCtx)
		reply, err := l.NickUpdated(&rpc.MemberMidReq{Mid: 90202})
		wantNoErr(t, tc.label, err)
		wantEQ(t, tc.label, "nick_updated", reply.GetNickUpdated(), tc.set)
		wantOps(t, tc.label, e.ops(0), []string{"flag.HasAttr:90202/1"})
		wantEQ(t, tc.label, "没有写库", e.st.log.countPrefix("flag.SetAttr"), 0)
	}
}

func TestNickUpdatedFailureDoesNotMasqueradeAsFalse(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select from user_flag: down")
	e.st.flag.failWith("HasAttr", boom)

	l := NewNickUpdatedLogic(context.Background(), e.svcCtx)
	reply, err := l.NickUpdated(&rpc.MemberMidReq{Mid: 90203})
	wantErrIs(t, "标志位库故障", err, boom)
	if reply != nil {
		t.Errorf("标志位库故障：reply = %+v, want nil（故障时返回 false 会被上游当成结论）", reply)
	}
	wantOps(t, "标志位库故障", e.ops(0), []string{"flag.HasAttr:90203/1"})
}

func TestNickUpdatedDoesNotGuardNonPositiveMid(t *testing.T) {
	for _, mid := range []int64{0, -3} {
		e := newEnv(t)
		l := NewNickUpdatedLogic(context.Background(), e.svcCtx)
		reply, err := l.NickUpdated(&rpc.MemberMidReq{Mid: mid})
		wantNoErr(t, "mid="+strconv.FormatInt(mid, 10), err)
		wantEQ(t, "mid="+strconv.FormatInt(mid, 10), "nick_updated", reply.GetNickUpdated(), false)
		wantOps(t, "mid 无守卫", e.ops(0), []string{"flag.HasAttr:" + strconv.FormatInt(mid, 10) + "/1"})
	}
}

// === IsInMonitor ===

func TestIsInMonitorActiveRowIsTrue(t *testing.T) {
	e := newEnv(t)
	e.st.monitor.put(&model.UserMonitor{Mid: 90301, Operator: "风控甲", Remark: "疑似刷币", IsDeleted: 0})

	l := NewIsInMonitorLogic(context.Background(), e.svcCtx)
	reply, err := l.IsInMonitor(&rpc.MidReq{Mid: 90301})
	wantNoErr(t, "在监控中", err)
	wantEQ(t, "在监控中", "is_in_monitor", reply.GetIsInMonitor(), true)
	wantOps(t, "在监控中", e.ops(0), []string{"monitor.InMonitor:90301"})
	if n := e.st.log.countPrefix("cache."); n != 0 {
		t.Errorf("在监控中：触碰了缓存 %d 次，want 0", n)
	}
}

func TestIsInMonitorMissingAndSoftDeletedAreFalseNotError(t *testing.T) {
	t.Run("名单里没有这个人", func(t *testing.T) {
		e := newEnv(t)
		e.st.monitor.put(&model.UserMonitor{Mid: 90302, Operator: "风控乙", IsDeleted: 0}) // 别人的行，不构成命中
		l := NewIsInMonitorLogic(context.Background(), e.svcCtx)
		reply, err := l.IsInMonitor(&rpc.MidReq{Mid: 90303})
		wantNoErr(t, "未监控", err)
		wantEQ(t, "未监控", "is_in_monitor", reply.GetIsInMonitor(), false)
		wantOps(t, "未监控", e.ops(0), []string{"monitor.InMonitor:90303"})
	})

	t.Run("已移出名单（软删除）", func(t *testing.T) {
		e := newEnv(t)
		e.st.monitor.put(&model.UserMonitor{Mid: 90304, Operator: "风控丙", Remark: "已复核放行", IsDeleted: 1})
		l := NewIsInMonitorLogic(context.Background(), e.svcCtx)
		reply, err := l.IsInMonitor(&rpc.MidReq{Mid: 90304})
		wantNoErr(t, "软删除", err)
		wantEQ(t, "软删除", "is_in_monitor 必须是 false", reply.GetIsInMonitor(), false)
		wantOps(t, "软删除", e.ops(0), []string{"monitor.InMonitor:90304"})
	})
}

func TestIsInMonitorFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select count from user_monitor: boom")
	e.st.monitor.failWith("InMonitor", boom)

	l := NewIsInMonitorLogic(context.Background(), e.svcCtx)
	reply, err := l.IsInMonitor(&rpc.MidReq{Mid: 90305})
	wantErrIs(t, "监控表故障", err, boom)
	if reply != nil {
		t.Errorf("监控表故障：reply = %+v, want nil（故障时返回 false 等于放行监控对象）", reply)
	}
	wantOps(t, "监控表故障", e.ops(0), []string{"monitor.InMonitor:90305"})
}

func TestIsInMonitorDoesNotGuardNonPositiveMid(t *testing.T) {
	e := newEnv(t)
	l := NewIsInMonitorLogic(context.Background(), e.svcCtx)
	reply, err := l.IsInMonitor(&rpc.MidReq{Mid: 0})
	wantNoErr(t, "mid=0", err)
	wantEQ(t, "mid=0", "is_in_monitor", reply.GetIsInMonitor(), false)
	wantOps(t, "mid=0", e.ops(0), []string{"monitor.InMonitor:0"})
}
