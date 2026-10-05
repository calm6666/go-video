package logic

// preparelive_logic_test.go 覆盖 PrepareLive（preparelivelogic.go）。
//
// 锁的结论：
//  1. 入参守卫顺序 room_id → mid → request_id → platform → device_hash → ip_hash，
//     六条守卫全部在**触库之前**，键不烧、下游不碰；platform 只接受契约里定义的 1..4
//     与 UNSPECIFIED（归一为 android），未定义取值一律拒（不做「未知即放行」）。
//  2. 业务前置（房间存在 / 非终态 / 主播生效绑定）在抢幂等键**之前**；
//     抢键之后的失败才会留下「已消费但无结果」的键。
//  3. 五项检查恒定按 anchor_qualification / risk_control / room_verified / not_banned /
//     setting_ok 的顺序全量产出，**任何一项失败都不短路**——客户端渲染的是清单而不是
//     第一个错误；因此「清单长度恒为 5」「清单里第一项失败时后面四项仍然评估」都被钉住。
//  4. 下游未接线 = 该项 degraded=true 且按未通过处理（静默放行等于把风控当可选件）。
//     CHALLENGE / BLOCK / UNSPECIFIED 都不放行；degraded 即使 decision=ALLOW 也算未通过。
//  5. 只有「全通过且房间在 PENDING」才 PENDING→READY，且迁移与审计不在同一事务里
//     （见 README 已知缺口：Transition 与 state_log.Insert 是两条独立 SQL）；
//     READY 复检保持 READY（幂等、零写入）；LIVING 走不到 already_living 分支（死代码，
//     由 TestPrepareLiveLivingRoomReportsRoomNotVerifiedNotAlreadyLiving 钉住）。
//  6. 重放路径在「抢键之前的前置读」之外零依赖：只 Claim + Find 结果快照，
//     不打 creator / risk、不再改状态。
//
// 本轮发现并 pin 的生产现状（未改代码）：
//   - denyRiskChallenge（conv.go:212）在整个包里没有任何引用：CHALLENGE 的 deny_code
//     与 BLOCK 同为 risk_denied，客户端只能靠 retry_after_seconds 区分（缺口 A）。
//   - retry_after_seconds 在「首个失败项不是风控」时依然按风控的 TTL 回填（归因错位，缺口 B）。
//   - PENDING→READY 的 Transition 与审计 Insert 非同一事务、且审计失败只记日志（缺口 C）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
	riskrpc "go-video/services/risk-control/rpc"
)

const (
	prepNow   int64 = 1_700_000_000
	prepRoom  int64 = 4101
	prepMid   int64 = 101
	prepArea  int64 = 7001
	prepDevH        = "dev-hash-1"
	prepIPH         = "ip-hash-1"
	prepReqID       = "req-prepare-1"
)

// prepCheckCodes 是契约承诺的稳定顺序（liveroom.proto:205 的注释口径）。
var prepCheckCodes = []string{
	checkAnchorQualification, checkRiskControl, checkRoomVerified, checkNotBanned, checkSettingOk,
}

func prepReq(reqID string) *rpc.PrepareLiveReq {
	return &rpc.PrepareLiveReq{
		RoomId: prepRoom, Mid: prepMid, Platform: rpc.Platform_PLATFORM_ANDROID,
		DeviceHash: prepDevH, IpHash: prepIPH, RequestId: reqID, TraceId: "trace-prep",
	}
}

func newPrepareLiveLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *PrepareLiveLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewPrepareLiveLogic(context.Background(), st.svcCtxWith(c))
}

// seedPrepScene 布一个「PENDING + 资料已过审 + 房主生效绑定 + 有配置行」的可开播形态。
// 下游 creator/risk 默认**不接线**，用例按需 st.wireCreator()/wireRisk()。
func seedPrepScene(t *testing.T, st *store, state int32) *model.LiveRoom {
	t.Helper()
	r := baseRoom(prepRoom, prepMid, state)
	r.AreaID = prepArea
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(811, prepRoom, prepMid, model.AnchorRoleOwner))
	st.seedSetting(baseSetting(prepRoom, prepNow))
	return r
}

// prepHappySeq 是「全通过并把 PENDING 抬到 READY」的完整依赖序列。
// 注意两处实现事实：creator 检查在 Claim 之后（所以降级也会烧键），
// 而房间状态迁移与审计是**两条独立 SQL**（无 TransactCtx）。
func prepHappySeq(now int64) []string {
	return []string{
		fmt.Sprintf("live_room.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
		fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		fmt.Sprintf("creator.UpAttr:%d/f%d", prepMid, creatorFromLiveUP),
		fmt.Sprintf("risk.CheckAction:%d/a%d", prepMid, riskrpc.GuardedAction_ACTION_LIVE_START),
		fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, now),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, now),
		fmt.Sprintf("live_room_setting.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room.Transition:%d:%d->%d/v1", prepRoom, model.RoomStatePending, model.RoomStateReady),
		fmt.Sprintf("live_room_state_log.Insert:t%d:%d->%d:prepare_passed",
			model.LogTypeRoomState, model.RoomStatePending, model.RoomStateReady),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", prepReqID),
	}
}

func TestPrepareLiveGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.PrepareLiveReq)
		want error
	}{
		{"room_id=0", func(r *rpc.PrepareLiveReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"room_id<0", func(r *rpc.PrepareLiveReq) { r.RoomId = -1 }, model.ErrInvalidRoomID},
		{"mid=0", func(r *rpc.PrepareLiveReq) { r.Mid = 0 }, model.ErrInvalidMid},
		{"request_id 空", func(r *rpc.PrepareLiveReq) { r.RequestId = "" }, model.ErrRequestIDRequired},
		{"request_id 只有空格", func(r *rpc.PrepareLiveReq) { r.RequestId = "   " }, model.ErrRequestIDRequired},
		{"request_id 超列宽", func(r *rpc.PrepareLiveReq) { r.RequestId = string(make([]byte, 65)) }, model.ErrDedupIDTooLong},
		{"platform 未定义值", func(r *rpc.PrepareLiveReq) { r.Platform = rpc.Platform(99) }, model.ErrPlatformInvalid},
		{"device_hash 含空格", func(r *rpc.PrepareLiveReq) { r.DeviceHash = "has space" }, model.ErrStreamRefMismatch},
		{"ip_hash 是 URL（明文泄漏）", func(r *rpc.PrepareLiveReq) { r.IpHash = "https://x/y" }, model.ErrStreamRefMismatch},
		{"device_hash 超长", func(r *rpc.PrepareLiveReq) { r.DeviceHash = string(make([]byte, 65)) }, model.ErrStreamRefMismatch},
		// 守卫顺序：room_id 先于 mid，mid 先于 request_id，request_id 先于 platform。
		{"room_id 与 mid 同时非法（先报 room_id）", func(r *rpc.PrepareLiveReq) {
			r.RoomId = 0
			r.Mid = 0
		}, model.ErrInvalidRoomID},
		{"mid 与 request_id 同时非法（先报 mid）", func(r *rpc.PrepareLiveReq) {
			r.Mid = 0
			r.RequestId = ""
		}, model.ErrInvalidMid},
		{"request_id 与 platform 同时非法（先报 request_id）", func(r *rpc.PrepareLiveReq) {
			r.RequestId = ""
			r.Platform = rpc.Platform(99)
		}, model.ErrRequestIDRequired},
		{"device_hash 先于 ip_hash", func(r *rpc.PrepareLiveReq) {
			r.DeviceHash = "bad hash"
			r.IpHash = "bad hash"
		}, model.ErrStreamRefMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedPrepScene(t, st, model.RoomStatePending)
			st.wireCreator().wireRisk()
			req := prepReq(prepReq2())
			tc.mut(req)

			got, err := newPrepareLiveLogic(t, st).PrepareLive(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			// 一条依赖都不许碰：库、去重键、两个下游全是零调用。
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantKeyUnburned(t, tc.name, prepReq2(), st)
			if st.creator.upCount() != 0 || st.risk.reqCount() != 0 {
				t.Errorf("%s：守卫拒绝却打了下游 creator=%d risk=%d",
					tc.name, st.creator.upCount(), st.risk.reqCount())
			}
		})
	}

	t.Run("in 为 nil", func(t *testing.T) {
		st := newStore()
		_, err := newPrepareLiveLogic(t, st).PrepareLive(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, 0)
	})
}

// prepReq2 是给守卫用例用的稳定 request_id（与 happy path 常量分开，避免键冲突误判）。
func prepReq2() string { return "req-guard" }

// TestPrepareLivePreconditionsRejectBeforeClaim 锁「业务前置在抢键之前」：
// 房间不存在 / 已关闭 / 主播未生效绑定三种拒绝都不得消费 request_id，
// 否则调用方一改参数就失去重试资格。
func TestPrepareLivePreconditionsRejectBeforeClaim(t *testing.T) {
	t.Run("房间不存在", func(t *testing.T) {
		st := newStore()
		st.wireCreator().wireRisk()
		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReq2()))
		wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
		if got != nil {
			t.Errorf("房间不存在仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", prepRoom))
		wantKeyUnburned(t, "房间不存在", prepReq2(), st)
	})

	t.Run("房间已关闭是硬错误不是检查项", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStateFinished)
		st.wireCreator().wireRisk()

		_, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReq2()))
		wantErrIs(t, "终态房间", err, model.ErrRoomFinished)
		wantSeq(t, "轨迹", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", prepRoom))
		wantKeyUnburned(t, "终态房间", prepReq2(), st)
	})

	t.Run("未绑定主播", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		req := prepReq(prepReq2())
		req.Mid = 999

		_, err := newPrepareLiveLogic(t, st).PrepareLive(req)
		wantErrIs(t, "未绑定", err, model.ErrAnchorForbidden)
		wantSeq(t, "轨迹", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", prepRoom),
			fmt.Sprintf("live_room_anchor.IsEnabled:%d/999", prepRoom))
		wantKeyUnburned(t, "未绑定", prepReq2(), st)
	})

	t.Run("绑定已解绑（state=0 不算生效）", func(t *testing.T) {
		st := newStore()
		st.seedRoom(baseRoom(prepRoom, prepMid, model.RoomStatePending))
		a := baseAnchor(811, prepRoom, prepMid, model.AnchorRoleOwner)
		a.State = model.BindStateDisabled
		a.OwnerRoomID.Valid = false // 解绑行 owner_room_id 必须 NULL（uniq_active_owner）
		st.seedAnchor(a)
		st.seedSetting(baseSetting(prepRoom, prepNow))

		_, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReq2()))
		wantErrIs(t, "解绑后不可开播", err, model.ErrAnchorForbidden)
		wantKeyUnburned(t, "解绑后不可开播", prepReq2(), st)
	})

	// 判别性对照：同一房间、同一 request_id，换成生效绑定的主播就受理成功。
	t.Run("对照：生效房主可受理", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReq2()))
		wantNoErr(t, "生效绑定", err)
		wantEQ(t, "生效绑定", "replayed", got.GetReplayed(), false)
	})
}

// TestPrepareLiveHappyPathPromotesPendingToReady 是全绿的基准形态：
// creator 有资格 + risk ALLOW + 资料过审 + 无禁播 + 有配置行 → PENDING→READY、ready=true、
// 五项全 passed、deny_code 空、审计一条 t1:1->2。
func TestPrepareLiveHappyPathPromotesPendingToReady(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "PrepareLive 全通过", err)
	defer st.checkRaces(t)

	wantSeq(t, "PrepareLive 首次全通过", st.log, 0, prepHappySeq(prepNow)...)
	// 状态迁移不是事务：整条链路一次 TransactCtx 都没有（README 缺口 C）。
	wantTxCount(t, "PrepareLive 无事务", st.conn, 0)

	wantEQ(t, "应答", "room_id", got.GetRoomId(), prepRoom)
	wantEQ(t, "应答", "state", got.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "应答", "ready", got.GetReady(), true)
	wantEQ(t, "应答", "deny_code", got.GetDenyCode(), "")
	wantEQ(t, "应答", "retry_after_seconds", got.GetRetryAfterSeconds(), int64(0))
	wantEQ(t, "应答", "replayed", got.GetReplayed(), false)
	wantStringsEQ(t, "应答", "checks 顺序", checkCodes(got.GetChecks()), prepCheckCodes)
	for i, it := range got.GetChecks() {
		wantEQ(t, fmt.Sprintf("检查项 %d", i+1), "passed", it.GetPassed(), true)
		wantEQ(t, fmt.Sprintf("检查项 %d", i+1), "degraded", it.GetDegraded(), false)
		wantEQ(t, fmt.Sprintf("检查项 %d", i+1), "detail 为空", it.GetDetail(), "")
	}

	row := st.roomAt(t, prepRoom)
	wantEQ(t, "库存", "state", row.State, model.RoomStateReady)
	wantEQ(t, "库存", "state_version", row.StateVersion, int32(2))
	wantEQ(t, "库存", "mtime", row.Mtime, prepNow)
	// PrepareLive 只推业务状态，绝不顺手改资料审核结论。
	wantEQ(t, "库存", "verify_state 不变", row.VerifyState, model.VerifyStatePassed)

	logs := st.logsOf(prepRoom)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "审计", "state_type", logs[0].StateType, model.LogTypeRoomState)
	wantEQ(t, "审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "1->2")
	wantEQ(t, "审计", "source", logs[0].Source, model.SourceRPCClient)
	wantEQ(t, "审计", "operator_mid 取自请求", logs[0].OperatorMid, prepMid)
	wantEQ(t, "审计", "request_id", logs[0].RequestID, prepReqID)
	wantEQ(t, "审计", "trace_id", logs[0].TraceID, "trace-prep")
	wantEQ(t, "审计", "reason", logs[0].Reason, "prepare_passed")

	// 幂等键已登记并回填结果。
	rec := st.idemAt(prepReqID)
	if rec == nil {
		t.Fatalf("幂等键未登记")
	}
	wantEQ(t, "幂等键", "rpc", rec.Rpc, rpcPrepareLive)
	wantEQ(t, "幂等键", "kind", rec.Kind, model.IdempotencyKindRequest)
	wantEQ(t, "幂等键", "room_id", rec.RoomID, prepRoom)
	if rec.ResultJSON == "" {
		t.Errorf("幂等键未回填结果，重放将拿不到应答")
	}

	// 发给风控的请求本身就是「开播」这一动作，且只带摘要不带明文。
	if n := st.risk.reqCount(); n == 1 {
		rq := st.risk.reqs[0]
		wantEQ(t, "风控请求", "action", rq.GetAction(), riskrpc.GuardedAction_ACTION_LIVE_START)
		wantEQ(t, "风控请求", "platform 字符串口径", rq.GetPlatform(), "android")
		wantEQ(t, "风控请求", "device_id 是摘要", rq.GetDeviceId(), prepDevH)
		wantEQ(t, "风控请求", "request_id", rq.GetRequestId(), prepReqID)
	}
}

func checkCodes(items []*rpc.PrepareCheckItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetCode())
	}
	return out
}

func findCheck(items []*rpc.PrepareCheckItem, code string) *rpc.PrepareCheckItem {
	for _, it := range items {
		if it.GetCode() == code {
			return it
		}
	}
	return nil
}

// TestPrepareLiveEmitsAllFiveChecksEvenWhenDownstreamsAreMissing 钉「不可评估 ≠ 放行」：
// 两个下游都没接线时，前两项都是 degraded=true 的失败项，清单仍然是 5 项，
// 房间一格都不动（Transition 零调用），deny_code 走 degraded 分支。
func TestPrepareLiveEmitsAllFiveChecksEvenWhenDownstreamsAreMissing(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	// 故意不 wireCreator / wireRisk。

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "下游未接线", err)

	wantSeq(t, "轨迹", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
		fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow),
		fmt.Sprintf("live_room_setting.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", prepReqID),
	)
	wantCount(t, "不碰 creator/risk", st.log, "creator.", 0)
	wantCount(t, "不碰 creator/risk", st.log, "risk.", 0)
	wantMethodCount(t, "不迁移状态", st.log, "live_room.Transition", 0)

	wantEQ(t, "降级", "ready", got.GetReady(), false)
	wantEQ(t, "降级", "state 仍是 PENDING", got.GetState(), rpc.RoomState_ROOM_STATE_PENDING)
	wantEQ(t, "降级", "deny_code", got.GetDenyCode(), denyDownstreamDegraded)
	wantStringsEQ(t, "降级", "checks 顺序", checkCodes(got.GetChecks()), prepCheckCodes)
	anchor := findCheck(got.GetChecks(), checkAnchorQualification)
	wantEQ(t, "降级", "anchor passed", anchor.GetPassed(), false)
	wantEQ(t, "降级", "anchor degraded", anchor.GetDegraded(), true)
	wantEQ(t, "降级", "anchor detail", anchor.GetDetail(), model.ErrCreatorNotConfigured.Error())
	risk := findCheck(got.GetChecks(), checkRiskControl)
	wantEQ(t, "降级", "risk detail", risk.GetDetail(), model.ErrRiskControlNotConfigured.Error())
	// 后三项与下游无关，仍然被完整评估为通过——证明「不短路」。
	for _, code := range []string{checkRoomVerified, checkNotBanned, checkSettingOk} {
		it := findCheck(got.GetChecks(), code)
		if it == nil {
			t.Fatalf("检查项 %s 缺失", code)
		}
		wantEQ(t, code, "passed", it.GetPassed(), true)
	}
	wantEQ(t, "降级", "库里 state 不变", st.roomAt(t, prepRoom).State, model.RoomStatePending)
	wantEQ(t, "降级", "无审计", len(st.logsOf(prepRoom)), 0)
	// 降级同样烧键并回填结果：重放会拿到这份「未通过」的应答而不是重新评估。
	if rec := st.idemAt(prepReqID); rec == nil || rec.ResultJSON == "" {
		t.Errorf("降级路径也应登记并回填 request_id %q", prepReqID)
	}
}

// TestPrepareLiveRiskDecisionMatrix 逐裁决钉 riskCheckItem 的落点。
// 判别性在两侧：ALLOW/REVIEW 放行，CHALLENGE/BLOCK/UNSPECIFIED 不放行；
// degraded=true 时**即使 decision=ALLOW 也算未通过**（降级下的 ALLOW 不代表评估过）。
func TestPrepareLiveRiskDecisionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		reply      *riskrpc.CheckActionReply
		wantPassed bool
		wantDegr   bool
		wantRetry  int64
		wantDeny   string
	}{
		{"ALLOW", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_ALLOW}, true, false, 0, ""},
		{"REVIEW 放行不阻断", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_REVIEW}, true, false, 0, ""},
		{"CHALLENGE 带 TTL", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE,
			ChallengeTtlSeconds: 60, ActionCode: "captcha"}, false, false, 60, denyRiskControl},
		{"CHALLENGE 无 TTL 走兜底 30s", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE},
			false, false, defaultChallengeRetrySeconds, denyRiskControl},
		// 缺口 A：denyRiskChallenge 全仓无引用，CHALLENGE 与 BLOCK 的 deny_code 相同。
		{"BLOCK 用 action_code 归因", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_BLOCK,
			ActionCode: "abc"}, false, false, 0, denyRiskControl},
		{"BLOCK 优先用处罚原因码", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_BLOCK,
			ActionCode: "abc", Punishment: &riskrpc.PunishmentSnapshot{ReasonCode: "ban_live"}}, false, false, 0, denyRiskControl},
		{"UNSPECIFIED 裁决不放行", &riskrpc.CheckActionReply{}, false, false, 0, denyRiskControl},
		{"degraded + ALLOW 仍算未通过", &riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_ALLOW,
			Degraded: true, Basis: "rule-store-down"}, false, true, 0, denyDownstreamDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, prepNow)
			st := newStore()
			seedPrepScene(t, st, model.RoomStatePending)
			st.wireCreator().wireRisk()
			st.risk.returns(tc.reply)

			got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
			wantNoErr(t, tc.name, err)

			it := findCheck(got.GetChecks(), checkRiskControl)
			wantEQ(t, tc.name, "passed", it.GetPassed(), tc.wantPassed)
			wantEQ(t, tc.name, "degraded", it.GetDegraded(), tc.wantDegr)
			wantEQ(t, tc.name, "retry_after_seconds", got.GetRetryAfterSeconds(), tc.wantRetry)
			wantEQ(t, tc.name, "deny_code", got.GetDenyCode(), tc.wantDeny)
			if tc.wantPassed {
				// 判别面的一侧：风控放行时这一项不再阻断，整条链路真的把房间抬到 READY。
				wantEQ(t, tc.name, "ready", got.GetReady(), true)
				wantEQ(t, tc.name, "房间抬到 READY", st.roomAt(t, prepRoom).State, model.RoomStateReady)
				wantMethodCount(t, tc.name, st.log, "live_room.Transition", 1)
				wantEQ(t, tc.name, "审计行数", len(st.logsOf(prepRoom)), 1)
			} else {
				wantEQ(t, tc.name, "ready", got.GetReady(), false)
				wantEQ(t, tc.name, "房间不动", st.roomAt(t, prepRoom).State, model.RoomStatePending)
				wantMethodCount(t, tc.name, st.log, "live_room.Transition", 0)
				wantEQ(t, tc.name, "无审计", len(st.logsOf(prepRoom)), 0)
			}
		})
	}

	// 风控 RPC 报错也是 degraded（不是「查不到就放行」）。
	t.Run("CheckAction 传输失败", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.risk.failWith(errPrepDown)

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, "风控传输失败", err)
		it := findCheck(got.GetChecks(), checkRiskControl)
		wantEQ(t, "风控传输失败", "degraded", it.GetDegraded(), true)
		wantEQ(t, "风控传输失败", "detail", it.GetDetail(), model.ErrDownstreamUnavailable.Error())
		wantEQ(t, "风控传输失败", "deny_code", got.GetDenyCode(), denyDownstreamDegraded)
	})

	// 判别性对照：creator 侧报错同样是 degraded，且 detail 不含下游原文。
	t.Run("creator 传输失败", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.creator.failWith(errPrepDown)

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, "creator 传输失败", err)
		it := findCheck(got.GetChecks(), checkAnchorQualification)
		wantEQ(t, "creator 传输失败", "degraded", it.GetDegraded(), true)
		wantEQ(t, "creator 传输失败", "detail 不含下游原文", it.GetDetail(), model.ErrDownstreamUnavailable.Error())
		wantEQ(t, "creator 传输失败", "deny_code", got.GetDenyCode(), denyDownstreamDegraded)
	})
}

var errPrepDown = errors.New("dial tcp: connection refused")

// TestPrepareLiveQualificationIsByIsAuthorNotByMid 钉资格判定的取值口径：
// 只有 is_author==1 才算有资格；creator 回 0 或任意其他值都算没有，且**不是降级**。
func TestPrepareLiveQualificationIsByIsAuthorNotByMid(t *testing.T) {
	for _, isAuthor := range []int32{0, 2, -1} {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.creator.authorReturns(isAuthor)

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, fmt.Sprintf("is_author=%d", isAuthor), err)
		it := findCheck(got.GetChecks(), checkAnchorQualification)
		wantEQ(t, fmt.Sprintf("is_author=%d", isAuthor), "passed", it.GetPassed(), false)
		wantEQ(t, fmt.Sprintf("is_author=%d", isAuthor), "degraded=false（是结论不是降级）",
			it.GetDegraded(), false)
		wantEQ(t, fmt.Sprintf("is_author=%d", isAuthor), "deny_code", got.GetDenyCode(), denyAnchorQualification)
		wantEQ(t, fmt.Sprintf("is_author=%d", isAuthor), "房间不动", st.roomAt(t, prepRoom).State, model.RoomStatePending)
	}

	// 对照：is_author=1 时同一形态放行（happy path 已证，这里锁同一条请求的两侧）。
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()
	st.creator.authorReturns(model.BoolToInt32(true))
	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "is_author=1", err)
	wantEQ(t, "is_author=1", "ready", got.GetReady(), true)
}

// TestPrepareLiveBanChecks 钉 not_banned 的三条拒绝路径与一条放行路径：
// 房间维度 BANNED 态、主播维度生效禁播、房间维度生效禁播记录，都只记检查项不报错；
// 已到期的临时禁播**不算生效记录**（放行）。两次禁播查询共用同一个 now。
func TestPrepareLiveBanChecks(t *testing.T) {
	t.Run("房间处于 BANNED 态", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStateBanned)
		st.wireCreator().wireRisk()

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, "BANNED 房间", err)
		it := findCheck(got.GetChecks(), checkNotBanned)
		wantEQ(t, "BANNED", "passed", it.GetPassed(), false)
		wantEQ(t, "BANNED", "detail", it.GetDetail(), "room is banned")
		wantEQ(t, "BANNED", "deny_code", got.GetDenyCode(), denyBanned)
		// BANNED 命中即返回，两张禁播表都不查（早于查库的短路，与「清单不短路」不同层）。
		wantMethodCount(t, "BANNED 不查禁播表", st.log, "live_room_ban.HasActiveByMid", 0)
		wantMethodCount(t, "BANNED 不查禁播表", st.log, "live_room_ban.FindActiveByRoom", 0)
		// room_verified 对 BANNED 是放行的（可开播态族含 BANNED），拒绝只来自 not_banned 一项。
		wantEQ(t, "BANNED", "room_verified 通过",
			findCheck(got.GetChecks(), checkRoomVerified).GetPassed(), true)
	})

	t.Run("主播维度生效禁播", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		b := baseBan(6001, prepRoom, model.BanStateActive)
		b.Mid = prepMid
		st.seedBan(b)
		st.wireCreator().wireRisk()

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, "主播被禁", err)
		wantEQ(t, "主播被禁", "detail", findCheck(got.GetChecks(), checkNotBanned).GetDetail(),
			"anchor has an active ban")
		wantEQ(t, "主播被禁", "deny_code", got.GetDenyCode(), denyBanned)
		// HasActiveByMid 命中后不再查房间维度那条（省一次 SQL），setting 仍要查。
		wantMethodCount(t, "主播被禁", st.log, "live_room_ban.FindActiveByRoom", 0)
		wantMethodCount(t, "主播被禁", st.log, "live_room_setting.FindOne", 1)
	})

	t.Run("房间维度生效禁播", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		b := baseBan(6002, prepRoom, model.BanStateActive)
		b.Mid = 999999 // 与请求 mid 无关，只按房间命中
		st.seedBan(b)
		st.wireCreator().wireRisk()

		got, _ := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantEQ(t, "房间被禁", "detail", findCheck(got.GetChecks(), checkNotBanned).GetDetail(),
			"room has an active ban")
		wantMethodCount(t, "房间被禁", st.log, "live_room_ban.FindActiveByRoom", 1)
	})

	t.Run("已到期临时禁播放行", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		b := baseBan(6003, prepRoom, model.BanStateActive)
		b.BanType = model.BanTypeTemporary
		b.EndAt = prepNow - 1 // end_at<=now 不算生效
		st.seedBan(b)
		st.wireCreator().wireRisk()

		got, _ := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantEQ(t, "到期禁播", "not_banned 通过", findCheck(got.GetChecks(), checkNotBanned).GetPassed(), true)
		wantEQ(t, "到期禁播", "ready", got.GetReady(), true)
		// 两条禁播查询都用被钉住的 now，不会因为两次取时间而「一边过期一边生效」。
		wantOpSeen(t, "到期禁播", st.log,
			fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow))
		wantOpSeen(t, "到期禁播", st.log,
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow))
	})

	// 对照：永久禁播（end_at=0）永远算生效。
	t.Run("永久禁播拒绝", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		b := baseBan(6004, prepRoom, model.BanStateActive)
		b.BanType = model.BanTypePermanent
		b.EndAt = 0
		st.seedBan(b)
		st.wireCreator().wireRisk()

		got, _ := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantEQ(t, "永久禁播", "not_banned 未通过", findCheck(got.GetChecks(), checkNotBanned).GetPassed(), false)
	})
}

// TestPrepareLiveRoomVerifiedAndSetting 钉最后两项检查的判定面：
//   - room_verified 只认 verify_state==PASSED **且**房间在 Pending/Ready/Banned 态族；
//   - setting_ok 缺行是**结论**（不是默认值兜底），错误原文按 ErrNoSettingRow 口径下发。
func TestPrepareLiveRoomVerifiedAndSetting(t *testing.T) {
	t.Run("资料未过审", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		r := baseRoom(prepRoom, prepMid, model.RoomStatePending)
		r.VerifyState = model.VerifyStateReviewing
		st.seedRoom(r)
		st.seedAnchor(baseAnchor(811, prepRoom, prepMid, model.AnchorRoleOwner))
		st.seedSetting(baseSetting(prepRoom, prepNow))
		st.wireCreator().wireRisk()

		got, _ := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		it := findCheck(got.GetChecks(), checkRoomVerified)
		wantEQ(t, "未过审", "passed", it.GetPassed(), false)
		wantEQ(t, "未过审", "detail", it.GetDetail(), fmt.Sprintf("verify_state=%d", model.VerifyStateReviewing))
		wantEQ(t, "未过审", "deny_code", got.GetDenyCode(), denyRoomNotVerified)
	})

	t.Run("DISABLED 房间：资料过审也不算可开播态族", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStateDisabled)
		st.wireCreator().wireRisk()

		got, _ := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		it := findCheck(got.GetChecks(), checkRoomVerified)
		wantEQ(t, "DISABLED", "passed", it.GetPassed(), false)
		wantEQ(t, "DISABLED", "detail", it.GetDetail(),
			fmt.Sprintf("room state=%d 不在可开播态族", model.RoomStateDisabled))
	})

	t.Run("缺配置行", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		st.seedRoom(baseRoom(prepRoom, prepMid, model.RoomStatePending))
		st.seedAnchor(baseAnchor(811, prepRoom, prepMid, model.AnchorRoleOwner))
		// 故意不 seedSetting
		st.wireCreator().wireRisk()

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantNoErr(t, "缺配置行是结论不是错误", err)
		it := findCheck(got.GetChecks(), checkSettingOk)
		wantEQ(t, "缺配置行", "passed", it.GetPassed(), false)
		wantEQ(t, "缺配置行", "detail", it.GetDetail(), model.ErrNoSettingRow.Error())
		wantEQ(t, "缺配置行", "deny_code", got.GetDenyCode(), denySettingMissing)
		wantEQ(t, "缺配置行", "ready", got.GetReady(), false)
		wantMethodCount(t, "缺配置行", st.log, "live_room.Transition", 0)
	})

	t.Run("配置读库失败原样上抛", func(t *testing.T) {
		fixClock(t, prepNow)
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.settings.failWith("FindOne", errPrepDown)

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantErrIs(t, "配置读库失败", err, errPrepDown)
		if got != nil {
			t.Errorf("读库失败仍返回 reply %+v", got)
		}
		// 失败发生在最后一项检查：前四项的 SQL 都已发生，键已烧但结果没回填。
		wantSeq(t, "轨迹", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", prepRoom),
			fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
			fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
			fmt.Sprintf("creator.UpAttr:%d/f%d", prepMid, creatorFromLiveUP),
			fmt.Sprintf("risk.CheckAction:%d/a%d", prepMid, riskrpc.GuardedAction_ACTION_LIVE_START),
			fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow),
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow),
			fmt.Sprintf("live_room_setting.FindOne:%d", prepRoom),
		)
		wantKeyBurnedNoResult(t, "配置读库失败", prepReqID, st)
		wantEQ(t, "配置读库失败", "房间不动", st.roomAt(t, prepRoom).State, model.RoomStatePending)
	})
}

// TestPrepareLiveLivingRoomReportsRoomNotVerifiedNotAlreadyLiving 钉住一段死代码：
// preparelivelogic.go:128-131 的「已在播 → deny_code=already_living」分支永远走不到，
// 因为 verifyStateCheck（conv.go:242-248 的可开播态族只含 Pending/Ready/Banned）
// 对 LIVING 房间必定判否，于是 !allChecksPassed 分支先命中。
// 这条用例同时是缺口 D 的证据：客户端拿到的是 room_not_verified，
// 而真实原因「已在播」被归因成了资料问题。
func TestPrepareLiveLivingRoomReportsRoomNotVerifiedNotAlreadyLiving(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStateLiving)
	st.wireCreator().wireRisk()

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "LIVING 复检", err)
	wantEQ(t, "LIVING 复检", "deny_code", got.GetDenyCode(), denyRoomNotVerified)
	if got.GetDenyCode() == denyRoomLiving {
		t.Errorf("already_living 分支竟然可达，与实现不符（room_verified 必定先判否）")
	}
	wantEQ(t, "LIVING 复检", "room_verified 未通过",
		findCheck(got.GetChecks(), checkRoomVerified).GetPassed(), false)
	wantEQ(t, "LIVING 复检", "ready", got.GetReady(), false)
	wantMethodCount(t, "LIVING 复检", st.log, "live_room.Transition", 0)
	// 整包没有任何地方引用 denyRiskChallenge / 这里的 already_living 也从未下发。
	wantEQ(t, "LIVING 复检", "房间仍是 LIVING", st.roomAt(t, prepRoom).State, model.RoomStateLiving)
}

// TestPrepareLiveRetryAfterIsAttributedToRiskEvenWhenAnotherCheckFails 钉缺口 B：
// retry_after_seconds 只在「有检查项未通过」时回填，但取的是**风控的 TTL**，
// 与首个失败项是谁无关。于是「主播无资格 + 风控 CHALLENGE(60s)」会告诉客户端 60 秒后再试，
// 而这次失败和风控毫无关系。判别性：把裁决换成 ALLOW 后同一场景 TTL 必须是 0。
func TestPrepareLiveRetryAfterIsAttributedToRiskEvenWhenAnotherCheckFails(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()
	st.creator.authorReturns(0)
	st.risk.returns(&riskrpc.CheckActionReply{Decision: riskrpc.Decision_DECISION_CHALLENGE,
		ChallengeTtlSeconds: 60})

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "归因错位", err)
	wantEQ(t, "归因错位", "首个失败项是主播资格",
		firstFailedCheck(got.GetChecks()).GetCode(), checkAnchorQualification)
	wantEQ(t, "归因错位", "deny_code 归到主播", got.GetDenyCode(), denyAnchorQualification)
	wantEQ(t, "归因错位", "retry_after 却来自风控", got.GetRetryAfterSeconds(), int64(60))

	// 对照：风控放行时同样的主播无资格场景 TTL 为 0。
	st2 := newStore()
	seedPrepScene(t, st2, model.RoomStatePending)
	st2.wireCreator().wireRisk()
	st2.creator.authorReturns(0)
	got2, err := newPrepareLiveLogic(t, st2).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "对照", err)
	wantEQ(t, "对照", "retry_after", got2.GetRetryAfterSeconds(), int64(0))
	wantEQ(t, "对照", "deny_code", got2.GetDenyCode(), denyAnchorQualification)
}

// TestPrepareLiveReadyRoomRecheckIsIdempotentAndWritesNothing 钉 default 分支：
// READY 房间全通过时回 ready=true、state=READY，且**一次写都没有**（不 Transition、不写审计）。
// 这是与 happy path（PENDING 会写两行）成对的一侧。
func TestPrepareLiveReadyRoomRecheckIsIdempotentAndWritesNothing(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStateReady)
	st.wireCreator().wireRisk()

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "READY 复检", err)
	wantSeq(t, "READY 复检轨迹", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
		fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		fmt.Sprintf("creator.UpAttr:%d/f%d", prepMid, creatorFromLiveUP),
		fmt.Sprintf("risk.CheckAction:%d/a%d", prepMid, riskrpc.GuardedAction_ACTION_LIVE_START),
		fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow),
		fmt.Sprintf("live_room_setting.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_idempotency.SaveResult:%s", prepReqID),
	)
	wantEQ(t, "READY 复检", "ready", got.GetReady(), true)
	wantEQ(t, "READY 复检", "state", got.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "READY 复检", "deny_code", got.GetDenyCode(), "")
	row := st.roomAt(t, prepRoom)
	wantEQ(t, "READY 复检", "state 不变", row.State, model.RoomStateReady)
	wantEQ(t, "READY 复检", "state_version 不变", row.StateVersion, int32(1))
	wantEQ(t, "READY 复检", "mtime 不变", row.Mtime, 1000+prepRoom)
	wantEQ(t, "READY 复检", "无审计", len(st.logsOf(prepRoom)), 0)
	wantTxCount(t, "READY 复检", st.conn, 0)
}

// TestPrepareLiveReplayTouchesNothingButDedup 钉重放路径：同一 request_id 第二次调用
// 只 Claim + Find，回存的应答逐字段一致并带上 replayed=true。
func TestPrepareLiveReplayTouchesNothingButDedup(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()
	lg := newPrepareLiveLogic(t, st)

	first, err := lg.PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "首次", err)
	before := st.log.snapshot()
	counts := st.counts()
	creatorN, riskN := st.creator.upCount(), st.risk.reqCount()

	second, err := lg.PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "重放", err)
	// 重放仍然要过抢键之前的三条前置读（房间 / 绑定 / Claim），之后只回读结果快照：
	// 不再打 creator / risk、不再改状态。
	wantSeq(t, "重放轨迹", st.log, before,
		fmt.Sprintf("live_room.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
		fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		fmt.Sprintf("live_room_idempotency.Find:%s", prepReqID),
	)
	wantEQ(t, "重放", "replayed", second.GetReplayed(), true)
	wantEQ(t, "重放", "ready", second.GetReady(), first.GetReady())
	wantEQ(t, "重放", "state", second.GetState(), first.GetState())
	wantEQ(t, "重放", "room_id", second.GetRoomId(), first.GetRoomId())
	wantDeepEQ(t, "重放", "checks 与首次逐字段一致", second.GetChecks(), first.GetChecks())

	wantEQ(t, "重放", "库存形态不变", st.counts(), counts)
	wantEQ(t, "重放", "creator 不再被调用", st.creator.upCount(), creatorN)
	wantEQ(t, "重放", "risk 不再被调用", st.risk.reqCount(), riskN)
	// 两次调用合计：房间读两次（首次 + 重放的前置守卫），状态迁移只发生一次。
	wantMethodCount(t, "重放", st.log, "live_room.FindOne", 2)
	wantMethodCount(t, "重放", st.log, "live_room.Transition", 1)
}

// TestPrepareLiveDedupFailureModes 钉重放侧的三种坏形态都**不会**被降级成成功：
//   - 键被别的 RPC 用过 → ErrRequestIDReused；
//   - 键存在但结果没回填（首次执行中途失败）→ ErrIdempotencyResultMissing；
//   - 键存在、结果不是本方法的应答形状 → 反序列化错误而不是「空应答」。
func TestPrepareLiveDedupFailureModes(t *testing.T) {
	t.Run("键被别的 RPC 用过", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.seedIdem(prepReqID, rpcStartLive, `{"room_id":4101}`)

		got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantErrIs(t, "键复用", err, model.ErrRequestIDReused)
		if got != nil {
			t.Errorf("键复用仍返回 reply %+v", got)
		}
		wantSeq(t, "轨迹", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", prepRoom),
			fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
			fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
			fmt.Sprintf("live_room_idempotency.Find:%s", prepReqID),
		)
		wantCount(t, "键复用", st.log, "creator.", 0)
		wantEQ(t, "键复用", "房间不动", st.roomAt(t, prepRoom).State, model.RoomStatePending)
	})

	t.Run("键存在但结果未回填", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.seedIdem(prepReqID, rpcPrepareLive, "")

		_, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantErrIs(t, "结果缺失", err, model.ErrIdempotencyResultMissing)
		wantKeyBurnedNoResult(t, "结果缺失", prepReqID, st)
		wantCount(t, "结果缺失", st.log, "creator.", 0)
	})

	t.Run("Claim 自身失败", func(t *testing.T) {
		st := newStore()
		seedPrepScene(t, st, model.RoomStatePending)
		st.wireCreator().wireRisk()
		st.idem.failWith("Claim", errPrepDown)

		_, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
		wantErrIs(t, "Claim 失败", err, errPrepDown)
		wantSeq(t, "轨迹", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", prepRoom),
			fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
			fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		)
		wantCount(t, "Claim 失败", st.log, "creator.", 0)
	})
}

// TestPrepareLiveCasLossOnPendingToReady 钉并发窗口：
// 房间版本在 Claim 之后、Transition 之前被别的入口推走 → CAS 未命中 →
// ErrConcurrentUpdate（不伪造成功），键已烧且无结果。
func TestPrepareLiveCasLossOnPendingToReady(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()
	st.raceBefore("live_room.Transition:4101:1->2", func() {
		st.rooms.rows[0].StateVersion = 42
	})

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantErrIs(t, "并发丢失", err, model.ErrConcurrentUpdate)
	if got != nil {
		t.Errorf("并发失败仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
		fmt.Sprintf("live_room_idempotency.Claim:%s", prepReqID),
		fmt.Sprintf("creator.UpAttr:%d/f%d", prepMid, creatorFromLiveUP),
		fmt.Sprintf("risk.CheckAction:%d/a%d", prepMid, riskrpc.GuardedAction_ACTION_LIVE_START),
		fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow),
		fmt.Sprintf("live_room_setting.FindOne:%d", prepRoom),
		fmt.Sprintf("live_room.Transition:%d:%d->%d/v1", prepRoom, model.RoomStatePending, model.RoomStateReady),
	)
	wantKeyBurnedNoResult(t, "并发丢失", prepReqID, st)
	row := st.roomAt(t, prepRoom)
	wantEQ(t, "并发丢失", "state 未变", row.State, model.RoomStatePending)
	wantEQ(t, "并发丢失", "state_version 是插队者抬到 42（本调用一条 UPDATE 都没改成）", row.StateVersion, int32(42))
	wantEQ(t, "并发丢失", "无审计", len(st.logsOf(prepRoom)), 0)
	st.checkRaces(t)
}

// TestPrepareLiveAuditInsertFailureDoesNotRollBackTransition 钉缺口 C：
// 状态 UPDATE 与审计 INSERT 是两条独立 SQL（无 TransactCtx），
// 且 logic 对 Insert 的错误**只记日志**（preparelivelogic.go:142-149）——
// 于是会出现「房间已 READY 但 live_room_state_log 一行都没有」的无审计形态，
// 而 AGENTS.md §8 要求状态推进留证据。
func TestPrepareLiveAuditInsertFailureDoesNotRollBackTransition(t *testing.T) {
	fixClock(t, prepNow)
	st := newStore()
	seedPrepScene(t, st, model.RoomStatePending)
	st.wireCreator().wireRisk()
	st.stateLogs.failWith("Insert", errPrepDown)

	got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
	wantNoErr(t, "审计写失败被吞掉", err)
	wantEQ(t, "审计写失败", "ready", got.GetReady(), true)
	wantEQ(t, "审计写失败", "应答 state", got.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "审计写失败", "库里已经 READY", st.roomAt(t, prepRoom).State, model.RoomStateReady)
	wantEQ(t, "审计写失败", "审计行数为 0（无证据链）", len(st.logsOf(prepRoom)), 0)
	wantTxCount(t, "整条链路无事务", st.conn, 0)
	// 吞掉之后仍然回填结果：重放会拿到这份「成功但没有审计」的应答。
	if rec := st.idemAt(prepReqID); rec == nil || rec.ResultJSON == "" {
		t.Errorf("审计失败后 request_id 应仍被回填，实得 %+v", rec)
	}
}

// TestPrepareLiveReadFailuresPropagate 钉三个读依赖失败都原样上抛、不降级成
// 「检查项未通过」也不降级成 not found：失败就是失败，客户端可重试。
func TestPrepareLiveReadFailuresPropagate(t *testing.T) {
	type arm func(st *store)
	cases := []struct {
		name string
		at   string // 断言轨迹截到这一条
		arm  arm
	}{
		{"房间读失败", fmt.Sprintf("live_room.FindOne:%d", prepRoom),
			func(st *store) { st.rooms.failWith("FindOne", errPrepDown) }},
		{"绑定读失败", fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", prepRoom, prepMid),
			func(st *store) { st.anchors.failWith("IsEnabled", errPrepDown) }},
		{"主播禁播读失败", fmt.Sprintf("live_room_ban.HasActiveByMid:%d@%d", prepMid, prepNow),
			func(st *store) { st.bans.failWith("HasActiveByMid", errPrepDown) }},
		{"房间禁播读失败", fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", prepRoom, prepNow),
			func(st *store) { st.bans.failWith("FindActiveByRoom", errPrepDown) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, prepNow)
			st := newStore()
			seedPrepScene(t, st, model.RoomStatePending)
			st.wireCreator().wireRisk()
			tc.arm(st)

			got, err := newPrepareLiveLogic(t, st).PrepareLive(prepReq(prepReqID))
			wantErrIs(t, tc.name, err, errPrepDown)
			if got != nil {
				t.Errorf("%s 仍返回 reply %+v", tc.name, got)
			}
			if errors.Is(err, model.ErrRoomNotFound) {
				t.Errorf("%s：DB 故障被降级成「房间不存在」：%v", tc.name, err)
			}
			// 失败点之后一次依赖都不许再碰。
			wantLastOp(t, tc.name, st.log, tc.at)
			wantEQ(t, tc.name, "房间不动", st.roomAt(t, prepRoom).State, model.RoomStatePending)
		})
	}
}
