package logic

import (
	"errors"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// TestRoomTransitionMatrix 把 logic 依赖的迁移矩阵逐状态钉住：
// 任何一条边的增删都必须同时改这里与 README，杜绝「代码悄悄放宽」。
func TestRoomTransitionMatrix(t *testing.T) {
	want := map[int32][]int32{
		model.RoomStatePending:  {model.RoomStateReady, model.RoomStateFinished, model.RoomStateBanned, model.RoomStateDisabled},
		model.RoomStateReady:    {model.RoomStatePending, model.RoomStateLiving, model.RoomStateFinished, model.RoomStateBanned, model.RoomStateDisabled},
		model.RoomStateLiving:   {model.RoomStateReady, model.RoomStateFinished, model.RoomStateBanned},
		model.RoomStateFinished: {},
		model.RoomStateBanned:   {model.RoomStatePending, model.RoomStateReady, model.RoomStateFinished},
		model.RoomStateDisabled: {model.RoomStatePending, model.RoomStateReady, model.RoomStateFinished, model.RoomStateBanned},
	}
	for from, targets := range want {
		got := model.RoomTransitionTargets(from)
		if len(got) != len(targets) {
			t.Fatalf("state %d targets = %v, want %v", from, got, targets)
		}
		for i := range got {
			if got[i] != targets[i] {
				t.Fatalf("state %d targets = %v, want %v（升序且逐项一致）", from, got, targets)
			}
		}
	}
	// 未指定与未知取值不得有任何出边（禁止「未知即放行」）。
	for _, from := range []int32{model.RoomStateUnspecified, 7, 99, -1} {
		if len(model.RoomTransitionTargets(from)) != 0 {
			t.Fatalf("state %d 不该有合法目标", from)
		}
		if model.ValidRoomState(from) && from != model.RoomStateUnspecified {
			t.Fatalf("ValidRoomState(%d) = true，未定义取值必须拒绝", from)
		}
	}
	// FINISHED 是终态：任何目标都不合法。
	for to := model.RoomStatePending; to <= model.RoomStateDisabled; to++ {
		if model.CanRoomTransition(model.RoomStateFinished, to) {
			t.Fatalf("FINISHED -> %d 必须非法", to)
		}
	}
	if !model.RoomStateIsTerminal(model.RoomStateFinished) {
		t.Fatal("FINISHED 必须是终态")
	}
	if model.RoomStateIsTerminal(model.RoomStateBanned) {
		t.Fatal("BANNED 不是终态：LiftBan 要能离开")
	}
}

func TestVerifyTargetForVerdict(t *testing.T) {
	cases := []struct {
		name      string
		cur       int32
		verdict   rpc.ModerationVerdict
		want      int32
		changed   bool
		wantError bool
	}{
		{"reviewing 通过", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_PASS, model.VerifyStatePassed, true, false},
		{"reviewing 驳回", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_REJECT, model.VerifyStateRejected, true, false},
		{"reviewing 转人审", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_REVIEW, model.VerifyStateReviewing, false, false},
		{"重复通过", model.VerifyStatePassed, rpc.ModerationVerdict_VERDICT_PASS, model.VerifyStatePassed, false, false},
		{"未送审直接给结论", model.VerifyStateNone, rpc.ModerationVerdict_VERDICT_PASS, 0, false, true},
		{"已通过后补驳回", model.VerifyStatePassed, rpc.ModerationVerdict_VERDICT_REJECT, 0, false, true},
		{"驳回后未重审就通过", model.VerifyStateRejected, rpc.ModerationVerdict_VERDICT_PASS, 0, false, true},
		{"驳回后转人审", model.VerifyStateRejected, rpc.ModerationVerdict_VERDICT_REVIEW, model.VerifyStateReviewing, true, false},
		{"结论未指定", model.VerifyStateReviewing, rpc.ModerationVerdict_VERDICT_UNSPECIFIED, 0, false, true},
		{"当前态未指定", model.VerifyStateUnspecified, rpc.ModerationVerdict_VERDICT_PASS, 0, false, true},
		{"结论未知取值", model.VerifyStateReviewing, rpc.ModerationVerdict(9), 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed, err := verifyTargetForVerdict(c.cur, c.verdict)
			if c.wantError {
				if err == nil {
					t.Fatalf("期望错误，实得 target=%d changed=%t", got, changed)
				}
				if !errors.Is(err, model.ErrInvalidVerifyTransition) && !errors.Is(err, model.ErrVerdictInvalid) {
					t.Fatalf("错误类型不对：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want || changed != c.changed {
				t.Fatalf("target=%d changed=%t, want %d/%t", got, changed, c.want, c.changed)
			}
		})
	}
}

// TestRoomStateForVerifyResult 钉住「资料结论落点」：只有两条合法边，
// 正在直播的房间绝不因结论回写而降级。
func TestRoomStateForVerifyResult(t *testing.T) {
	cases := []struct {
		verify int32
		room   int32
		to     int32
		ok     bool
	}{
		{model.VerifyStatePassed, model.RoomStatePending, model.RoomStateReady, true},
		{model.VerifyStateRejected, model.RoomStateReady, model.RoomStatePending, true},
		{model.VerifyStatePassed, model.RoomStateLiving, 0, false},
		{model.VerifyStateRejected, model.RoomStateLiving, 0, false},
		{model.VerifyStatePassed, model.RoomStateBanned, 0, false},
		{model.VerifyStateRejected, model.RoomStatePending, 0, false},
		{model.VerifyStateReviewing, model.RoomStateReady, 0, false},
		{model.VerifyStatePassed, model.RoomStateFinished, 0, false},
	}
	for _, c := range cases {
		to, ok := roomStateForVerifyResult(c.verify, c.room)
		if ok != c.ok || to != c.to {
			t.Fatalf("verify=%d room=%d -> (%d,%t), want (%d,%t)", c.verify, c.room, to, ok, c.to, c.ok)
		}
		if ok && !model.CanRoomTransition(c.room, to) {
			t.Fatalf("给出了一条矩阵外的边 %d->%d", c.room, to)
		}
	}
}

func TestRoomStateAfterBanLift(t *testing.T) {
	if got := roomStateAfterBanLift(model.VerifyStatePassed); got != model.RoomStateReady {
		t.Fatalf("审核通过应回 READY，实得 %d", got)
	}
	for _, vs := range []int32{model.VerifyStateNone, model.VerifyStateReviewing, model.VerifyStateRejected, model.VerifyStateUnspecified} {
		got := roomStateAfterBanLift(vs)
		if got != model.RoomStatePending {
			t.Fatalf("verify=%d 应回 PENDING，实得 %d", vs, got)
		}
		if !model.CanRoomTransition(model.RoomStateBanned, got) {
			t.Fatalf("BANNED -> %d 不在矩阵内", got)
		}
	}
}

func TestVerifyTargetAfterResubmit(t *testing.T) {
	cases := map[int32]int32{
		model.VerifyStateNone:        model.VerifyStateReviewing,
		model.VerifyStatePassed:      model.VerifyStateReviewing,
		model.VerifyStateRejected:    model.VerifyStateReviewing,
		model.VerifyStateReviewing:   model.VerifyStateReviewing, // 已在审：保持原值即正确
		model.VerifyStateUnspecified: model.VerifyStateUnspecified,
	}
	for cur, want := range cases {
		if got := verifyTargetAfterResubmit(cur); got != want {
			t.Fatalf("verifyTargetAfterResubmit(%d) = %d, want %d", cur, got, want)
		}
	}
}

// TestEndReasonAndReplayAllowlist 锁住「客户端可写的原因」与「可给的回放目标」。
func TestEndReasonAndReplayAllowlist(t *testing.T) {
	allow := []int32{model.EndReasonAnchorStop, model.EndReasonStreamReplay}
	deny := []int32{model.EndReasonUnspecified, model.EndReasonBanned, model.EndReasonRoomClosed,
		model.EndReasonStreamTimeout, 9}
	for _, r := range allow {
		if !allowEndReasonForEndLive(r) {
			t.Fatalf("end_reason %d 应由 EndLive 受理", r)
		}
	}
	for _, r := range deny {
		if allowEndReasonForEndLive(r) {
			t.Fatalf("end_reason %d 只能由内部入口写，客户端不得自选", r)
		}
	}
	for _, s := range []int32{model.ReplayStateProcessing, model.ReplayStateAvailable, model.ReplayStateRemoved} {
		if !allowReplayTarget(s) {
			t.Fatalf("replay target %d 应允许", s)
		}
	}
	for _, s := range []int32{model.ReplayStateUnspecified, model.ReplayStateNone, 9} {
		if allowReplayTarget(s) {
			t.Fatalf("replay target %d 必须拒绝（NONE 只由建档产生）", s)
		}
	}
}

func TestRoomInfoEditableStatesAndRoles(t *testing.T) {
	areaStates := roomInfoEditableStates(true)
	allStates := roomInfoEditableStates(false)
	if !stateIn(areaStates, model.RoomStatePending) || !stateIn(areaStates, model.RoomStateReady) {
		t.Fatal("改分区至少允许 PENDING/READY")
	}
	if stateIn(areaStates, model.RoomStateLiving) {
		t.Fatal("在播时不得改分区（场次快照与发现归属会分叉）")
	}
	if !stateIn(allStates, model.RoomStateLiving) {
		t.Fatal("在播时允许改标题（场次已落快照）")
	}
	for _, s := range []int32{model.RoomStateBanned, model.RoomStateDisabled, model.RoomStateFinished} {
		if stateIn(allStates, s) || stateIn(areaStates, s) {
			t.Fatalf("状态 %d 不可改资料", s)
		}
	}
	if !canEditRoomInfo(model.AnchorRoleOwner) || !canEditRoomInfo(model.AnchorRoleCohost) {
		t.Fatal("房主与联合主播应可改资料")
	}
	if canEditRoomInfo(model.AnchorRoleManager) || canEditRoomInfo(model.AnchorRoleUnspecified) {
		t.Fatal("房管与未定角色不可改资料")
	}
}

func TestBanEndAt(t *testing.T) {
	if got, err := banEndAt(model.BanTypePermanent, 0, 1000); err != nil || got != 0 {
		t.Fatalf("永久禁播 end_at 必须为 0，实得 %d/%v", got, err)
	}
	if got, err := banEndAt(model.BanTypePermanent, 3600, 1000); err != nil || got != 0 {
		t.Fatalf("永久禁播忽略时长，实得 %d/%v", got, err)
	}
	if got, err := banEndAt(model.BanTypeTemporary, 60, 1000); err != nil || got != 1060 {
		t.Fatalf("临时禁播 end_at = start+duration，实得 %d/%v", got, err)
	}
	if _, err := banEndAt(model.BanTypeTemporary, 0, 1000); !errors.Is(err, model.ErrBanDurationRequired) {
		t.Fatalf("临时禁播缺时长应拒绝：%v", err)
	}
	if _, err := banEndAt(model.BanTypeTemporary, -5, 1000); !errors.Is(err, model.ErrBanDurationRequired) {
		t.Fatalf("负时长应拒绝：%v", err)
	}
	if _, err := banEndAt(model.BanTypeTemporary, 1<<62, 1<<62); !errors.Is(err, model.ErrBanDurationRequired) {
		t.Fatalf("溢出应被拦住：%v", err)
	}
	if _, err := banEndAt(model.BanTypeUnspecified, 60, 1000); !errors.Is(err, model.ErrBanTypeInvalid) {
		t.Fatalf("未指定禁播类型应拒绝：%v", err)
	}
	if _, err := banEndAt(9, 60, 1000); !errors.Is(err, model.ErrBanTypeInvalid) {
		t.Fatalf("未知禁播类型应拒绝：%v", err)
	}
}

func TestOwnerAndAreaLimitStates(t *testing.T) {
	if !stateIn(ownerLimitStates(), model.RoomStateFinished) {
		t.Fatal("关闭房间不返还建房额度，FINISHED 必须计入")
	}
	if stateIn(areaOccupancyStates(), model.RoomStateFinished) {
		t.Fatal("已关闭房间不占用分区")
	}
	for _, s := range areaOccupancyStates() {
		if !model.ValidRoomState(s) {
			t.Fatalf("占用集合含未定义状态 %d", s)
		}
	}
}

// TestPrepareCheckAggregation 检查项聚合口径：空清单不算通过，deny_code 稳定。
func TestPrepareCheckAggregation(t *testing.T) {
	if allChecksPassed(nil) {
		t.Fatal("空检查清单不得判为通过")
	}
	if allChecksPassed([]*rpc.PrepareCheckItem{newCheckItem(checkSettingOk, true, "", false), nil}) {
		t.Fatal("含 nil 项不得判为通过")
	}
	items := []*rpc.PrepareCheckItem{
		newCheckItem(checkAnchorQualification, true, "", false),
		newCheckItem(checkRiskControl, false, "denied", false),
	}
	if allChecksPassed(items) {
		t.Fatal("有未通过项时不得判为通过")
	}
	if got := firstFailedCheck(items); got == nil || got.GetCode() != checkRiskControl {
		t.Fatalf("firstFailedCheck = %v", got)
	}
	if got := denyCodeForCheck(checkRiskControl, false); got != denyRiskControl {
		t.Fatalf("risk 拒绝的 deny_code = %s", got)
	}
	if got := denyCodeForCheck(checkRiskControl, true); got != denyDownstreamDegraded {
		t.Fatalf("降级必须回 %s，实得 %s", denyDownstreamDegraded, got)
	}
	if got := denyCodeForCheck("未知项", false); got != denyRoomNotReady {
		t.Fatalf("未知 code 不得回落成通过，实得 %s", got)
	}
}

// TestVerifyStateCheck 资料检查项：只有 PASSED 且处于可开播态族才算通过。
func TestVerifyStateCheck(t *testing.T) {
	passCases := []int32{model.RoomStatePending, model.RoomStateReady, model.RoomStateBanned}
	for _, st := range passCases {
		item := verifyStateCheck(&model.LiveRoom{VerifyState: model.VerifyStatePassed, State: st})
		if !item.GetPassed() {
			t.Fatalf("state=%d verify=PASSED 应通过", st)
		}
	}
	for _, st := range []int32{model.RoomStateLiving, model.RoomStateFinished, model.RoomStateDisabled} {
		item := verifyStateCheck(&model.LiveRoom{VerifyState: model.VerifyStatePassed, State: st})
		if item.GetPassed() {
			t.Fatalf("state=%d 不在可开播态族，不应通过", st)
		}
	}
	item := verifyStateCheck(&model.LiveRoom{VerifyState: model.VerifyStateReviewing, State: model.RoomStateReady})
	if item.GetPassed() || item.GetDegraded() {
		t.Fatalf("审核中不算通过也不算降级：%v", item)
	}
}
