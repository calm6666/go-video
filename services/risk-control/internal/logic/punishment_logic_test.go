package logic

// punishment_logic_test.go 覆盖处罚域三个入口：ApplyPunishment / LiftPunishment / ListPunishments。
//
// 被测路径同样是**真实 Repository + 真实 model 替身**（见 fakes_test.go），所以这里断言的是
// 处罚状态机与幂等语义本身，而不是「logic 有没有转调」：
//  1. 幂等两层：idempotency_key 命中即在 FindByKey 处短路（不得再走 ExpireStale/Insert），
//     且回包是**库存行的原值**（不得被本次请求的 decision 悄悄改写）；
//  2. 同一 (mid, scope) 重叠处罚必须被拒，而「全域处罚」与「动作处罚」按 MatchesScope
//     精确区分——解除某个动作的处罚绝不能顺带解除全域封禁；
//  3. 到期推进（ExpireStale）发生在重叠检查**之前**，所以到期处罚不挡新处罚；
//  4. 依赖故障一律原样上抛（不伪造成功），且要说清「哪一步已经落了库」——
//     Insert 之后失败与 FindByKey 之后失败的运营处置方式完全不同；
//  5. 列表接口的过滤/分页参数必须逐字进 SQL 口径（mid/scope/state/onlyActive/offset/limit），
//     total=0 时不得发第二条查询。
//
// 时间戳与自增主键不可注入，因此序列里的 FindOne:<id> 一律用**读回**的 ID 拼接，
// 时间断言用 wantRangeInt64；缺口 #13/#15/#16 以「行为哨兵」形式钉住现状（见对应用例注释）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

const (
	testOperator = int64(9001)
	testPunKey   = "op-pun-key-1"
)

// applyReq 是一条合法的下发请求（mid=42、scope=评论、BLOCK 600 秒）。
func applyReq(key string) *rpc.ApplyPunishmentReq {
	return &rpc.ApplyPunishmentReq{
		Mid:             testMid,
		Scope:           rpc.GuardedAction_ACTION_COMMENT,
		Decision:        rpc.Decision_DECISION_BLOCK,
		Reason:          "批量灌水，人工确认",
		ReasonCode:      "risk.punish.spam",
		Operator:        testOperator,
		DurationSeconds: 600,
		IdempotencyKey:  key,
	}
}

func applyPunishment(t *testing.T, st *store, in *rpc.ApplyPunishmentReq) (*rpc.ApplyPunishmentReply, error) {
	t.Helper()
	return NewApplyPunishmentLogic(context.Background(), st.svcCtx).ApplyPunishment(in)
}

func liftPunishment(t *testing.T, st *store, in *rpc.LiftPunishmentReq) (*rpc.LiftPunishmentReply, error) {
	t.Helper()
	return NewLiftPunishmentLogic(context.Background(), st.svcCtx).LiftPunishment(in)
}

func listPunishments(t *testing.T, st *store, in *rpc.ListPunishmentsReq) (*rpc.ListPunishmentsReply, error) {
	t.Helper()
	return NewListPunishmentsLogic(context.Background(), st.svcCtx).ListPunishments(in)
}

// findPunishOp 下发/解除读写的五步序列（幂等短路时是其前缀）。
func findPunishOp(key string) string { return "punish.FindByKey:" + key }

func insertPunishOp(key string) string { return "punish.Insert:" + key }

func findPunishOneOp(id int64) string { return "punish.FindOne:" + strconv.FormatInt(id, 10) }

func liftOp(id int64) string { return "punish.Lift:" + strconv.FormatInt(id, 10) }

// --- ApplyPunishment ---

func TestApplyPunishmentPersistsExactlyWhatSqlWrites(t *testing.T) {
	st := newStore(t)
	before := time.Now().Unix()

	reply, err := applyPunishment(t, st, applyReq(testPunKey))
	wantNoErr(t, "ApplyPunishment", err)
	id := reply.Punishment.GetPunishmentId()

	wantOps(t, "下发序列", st.log.all(), []string{
		findPunishOp(testPunKey),
		"punish.ExpireStale:42",
		"punish.ListActive:42",
		insertPunishOp(testPunKey),
		findPunishOneOp(id),
	})
	// 处罚不走缓存：任何 Redis 动作都是多余的（CheckAction 每次回源读处罚）。
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Fatalf("下发触碰 Redis %d 次（应为 0）：%v", n, st.log.all())
	}

	wantEQ(t, "下发", "created", reply.Created, true)
	wantEQ(t, "下发", "mid", reply.Punishment.GetMid(), testMid)
	wantEQ(t, "下发", "scope", int32(reply.Punishment.GetScope()), int32(model.ActionComment))
	wantEQ(t, "下发", "decision", int32(reply.Punishment.GetDecision()), int32(model.DecisionBlock))
	wantEQ(t, "下发", "operator", reply.Punishment.GetOperator(), testOperator)
	wantEQ(t, "下发", "reason", reply.Punishment.GetReason(), "批量灌水，人工确认")
	wantEQ(t, "下发", "reason_code", reply.Punishment.GetReasonCode(), "risk.punish.spam")
	wantEQ(t, "下发", "idempotency_key", reply.Punishment.GetIdempotencyKey(), testPunKey)

	after := time.Now().Unix()
	// start_at 由服务端时钟决定（logic 不接受调用方传时间），end_at = start_at + duration。
	wantRangeInt64(t, "下发", "start_at", reply.Punishment.GetStartAt(), before, after)
	wantEQ(t, "下发", "end_at-start_at", reply.Punishment.GetEndAt()-reply.Punishment.GetStartAt(), int64(600))
	wantRangeInt64(t, "下发", "ctime", reply.Punishment.GetCtime(), before, after)

	row := st.punishmentRow(id)
	wantEQ(t, "库存行", "rows 总数", len(st.punish.rows), 1)
	wantEQ(t, "库存行", "mid", row.Mid, testMid)
	wantEQ(t, "库存行", "end_at", row.EndAt, reply.Punishment.GetEndAt())
	assertNoRawPII(t, "处罚行", fmt.Sprintf("%+v", row))
}

func TestApplyPunishmentPermanentAndTimeBoundaries(t *testing.T) {
	st := newStore(t)

	// duration=0 → 永久：end_at 必须落 0，不能被算成「当前秒」。
	reply, err := applyPunishment(t, st, &rpc.ApplyPunishmentReq{
		Mid: testMid, Scope: rpc.GuardedAction_ACTION_LOGIN, Decision: rpc.Decision_DECISION_CHALLENGE,
		Operator: testOperator, DurationSeconds: 0, IdempotencyKey: "key-perm",
	})
	wantNoErr(t, "永久处罚", err)
	wantEQ(t, "永久处罚", "end_at", reply.Punishment.GetEndAt(), int64(0))
	wantEQ(t, "永久处罚", "decision", int32(reply.Punishment.GetDecision()), int32(model.DecisionChallenge))

	// duration<0 直接拒绝（0 已有含义：永久），且不产生任何依赖调用。
	before := st.log.snapshot()
	_, err = applyPunishment(t, st, &rpc.ApplyPunishmentReq{
		Mid: testMid, Scope: rpc.GuardedAction_ACTION_LOGIN, Decision: rpc.Decision_DECISION_REVIEW,
		Operator: testOperator, DurationSeconds: -1, IdempotencyKey: "key-neg",
	})
	wantErrIs(t, "负 duration", err, model.ErrInvalidTarget)
	wantNoCall(t, "负 duration", st, before)
	wantEQ(t, "负 duration", "rows 总数", len(st.punish.rows), 1)
}

func TestApplyPunishmentReplayShortCircuitsAtIdempotencyKey(t *testing.T) {
	st := newStore(t)
	// 库里已有一条同幂等键的处罚（裁决是 CHALLENGE）。
	seeded := st.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionChallenge,
		Reason: "首次下发", Operator: testOperator, StartAt: time.Now().Unix() - 30,
		EndAt: 0, State: model.PunishmentStateActive, IdempotencyKey: testPunKey,
	})

	// 请求体故意带不同的 decision/reason/duration：幂等命中必须回库存原值。
	in := applyReq(testPunKey)
	in.Decision = rpc.Decision_DECISION_BLOCK
	in.Reason = "重试时改的说明"
	in.DurationSeconds = 999

	reply, err := applyPunishment(t, st, in)
	wantNoErr(t, "重试下发", err)
	wantOps(t, "重试序列", st.log.all(), []string{findPunishOp(testPunKey)})
	wantEQ(t, "重试", "created", reply.Created, false)
	wantEQ(t, "重试", "punishment_id", reply.Punishment.GetPunishmentId(), seeded)
	wantEQ(t, "重试", "decision（库存原值）", int32(reply.Punishment.GetDecision()), int32(model.DecisionChallenge))
	wantEQ(t, "重试", "reason（库存原值）", reply.Punishment.GetReason(), "首次下发")
	wantEQ(t, "重试", "end_at（不被本次 duration 改写）", reply.Punishment.GetEndAt(), int64(0))
	wantEQ(t, "重试", "rows 总数（未新增第二条）", len(st.punish.rows), 1)

	// 幂等短路发生在 ExpireStale 之前：重试既不加锁也不推进任何状态。
	if row := st.punishmentRow(seeded); row.State != model.PunishmentStateActive {
		t.Fatalf("重试把库存行状态改成了 %d，应为原样 ACTIVE", row.State)
	}
}

func TestApplyPunishmentRejectsOverlappingPunishmentButNotOtherScope(t *testing.T) {
	// 同一 (mid, scope) 已有生效处罚 → ErrPunishmentAlreadyActive，且不得走到 Insert。
	st := newStore(t)
	active := st.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 5, EndAt: 0,
		State: model.PunishmentStateActive, IdempotencyKey: "old-key",
	})

	_, err := applyPunishment(t, st, applyReq("new-key"))
	wantErrIs(t, "重叠处罚", err, model.ErrPunishmentAlreadyActive)
	wantOps(t, "重叠拒绝序列", st.log.all(), []string{
		findPunishOp("new-key"), "punish.ExpireStale:42", "punish.ListActive:42",
	})
	// 错误里必须带冲突处罚 ID，否则运营无法定位该解除哪一条。
	if !strings.Contains(err.Error(), "punishment_id="+strconv.FormatInt(active, 10)) {
		t.Fatalf("错误未回传冲突处罚 ID：%v", err)
	}
	wantEQ(t, "重叠拒绝", "rows 总数", len(st.punish.rows), 1)

	// 不同 scope（全域 vs 具体动作）按 MatchesScope 精确判定：不视为重叠。
	// 这是「先全域封禁、再对评论单独加严」的正常运营形态。
	other := newStore(t)
	other.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionAll, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 5, EndAt: 0,
		State: model.PunishmentStateActive, IdempotencyKey: "global-key",
	})
	reply, err := applyPunishment(t, other, applyReq("comment-key"))
	wantNoErr(t, "全域处罚不挡动作处罚", err)
	wantEQ(t, "全域不重叠", "created", reply.Created, true)
	wantOps(t, "全域不重叠序列", other.log.opsFrom(0), []string{
		findPunishOp("comment-key"), "punish.ExpireStale:42", "punish.ListActive:42",
		insertPunishOp("comment-key"), findPunishOneOp(reply.Punishment.GetPunishmentId()),
	})
}

func TestApplyPunishmentAdvancesExpiredBeforeOverlapCheck(t *testing.T) {
	st := newStore(t)
	stale := st.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 100, EndAt: time.Now().Unix() - 10,
		State: model.PunishmentStateActive, IdempotencyKey: "stale-key",
	})

	reply, err := applyPunishment(t, st, applyReq("after-stale"))
	wantNoErr(t, "到期后重新下发", err)
	wantEQ(t, "到期后重新下发", "created", reply.Created, true)
	// 推进状态在重叠检查之前：已到期的处罚不再构成「已存在生效处罚」。
	wantOps(t, "到期推进序列", st.log.all(), []string{
		findPunishOp("after-stale"), "punish.ExpireStale:42", "punish.ListActive:42",
		insertPunishOp("after-stale"), findPunishOneOp(reply.Punishment.GetPunishmentId()),
	})
	wantEQ(t, "到期推进", "旧行 state", st.punishmentRow(stale).State, model.PunishmentStateExpired)
	wantEQ(t, "到期推进", "rows 总数", len(st.punish.rows), 2)
}

func TestApplyPunishmentGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.ApplyPunishmentReq)
		want   error
		wantIn string // 错误文本里应出现的关键字（说明是哪条守卫挡下的）
	}{
		{"动作越界", func(in *rpc.ApplyPunishmentReq) { in.Scope = rpc.GuardedAction(8) }, model.ErrInvalidTarget, "scope=8"},
		{"裁决为 ALLOW", func(in *rpc.ApplyPunishmentReq) { in.Decision = rpc.Decision_DECISION_ALLOW }, model.ErrInvalidTarget, "CHALLENGE/BLOCK/REVIEW"},
		{"裁决 UNSPECIFIED", func(in *rpc.ApplyPunishmentReq) { in.Decision = rpc.Decision_DECISION_UNSPECIFIED }, model.ErrInvalidTarget, "CHALLENGE/BLOCK/REVIEW"},
		{"裁决越界", func(in *rpc.ApplyPunishmentReq) { in.Decision = rpc.Decision(9) }, model.ErrInvalidTarget, "CHALLENGE/BLOCK/REVIEW"},
		{"说明超长", func(in *rpc.ApplyPunishmentReq) { in.Reason = strings.Repeat("r", maxReasonLen+1) }, model.ErrInvalidTarget, "reason too long"},
		{"无操作人", func(in *rpc.ApplyPunishmentReq) { in.Operator = 0 }, model.ErrOperatorRequired, "operator"},
		{"操作人为负", func(in *rpc.ApplyPunishmentReq) { in.Operator = -1 }, model.ErrOperatorRequired, "operator"},
		{"无幂等键", func(in *rpc.ApplyPunishmentReq) { in.IdempotencyKey = "   " }, model.ErrIdempotencyKeyRequired, "idempotency"},
		{"无账号", func(in *rpc.ApplyPunishmentReq) { in.Mid = 0 }, model.ErrInvalidTarget, "invalid action or target"},
		{"账号为负", func(in *rpc.ApplyPunishmentReq) { in.Mid = -3 }, model.ErrInvalidTarget, "invalid action or target"},
	}
	for _, tc := range cases {
		st := newStore(t)
		in := applyReq("guard-key")
		tc.mutate(in)
		_, err := applyPunishment(t, st, in)
		wantErrIs(t, tc.name, err, tc.want)
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s：错误文本 %q 未包含 %q", tc.name, err.Error(), tc.wantIn)
		}
		// 守卫必须先于任何库/缓存调用：拒绝不得留下读放大或半成品写入。
		wantNoCall(t, tc.name, st, 0)
		wantEQ(t, tc.name, "rows 总数", len(st.punish.rows), 0)
	}
}

func TestApplyPunishmentShortensExternalStringsBeforeLookup(t *testing.T) {
	st := newStore(t)
	// 幂等键带首尾空白与控制符：规范化必须发生在 FindByKey **之前**，否则重试打不中同一行。
	dirty := "  key-with-\t\nctrl  "
	reply, err := applyPunishment(t, st, applyReq(dirty))
	wantNoErr(t, "脏幂等键", err)
	wantOps(t, "脏幂等键序列", st.log.all(), []string{
		findPunishOp("key-with-ctrl"), "punish.ExpireStale:42", "punish.ListActive:42",
		insertPunishOp("key-with-ctrl"), findPunishOneOp(reply.Punishment.GetPunishmentId()),
	})
	wantEQ(t, "脏幂等键", "落库 key", st.punishmentRow(reply.Punishment.GetPunishmentId()).IdempotencyKey, "key-with-ctrl")

	// 超长 reason_code 裁到 64（与列宽一致）。
	long := newStore(t)
	in := applyReq("reason-code-len")
	in.ReasonCode = strings.Repeat("c", 300)
	reply, err = applyPunishment(t, long, in)
	wantNoErr(t, "超长 reason_code", err)
	row := long.punishmentRow(reply.Punishment.GetPunishmentId())
	wantEQ(t, "超长 reason_code", "reason_code 长度", len(row.ReasonCode), 64)
	wantEQ(t, "超长 reason_code", "幂等键原样入库", row.IdempotencyKey, "reason-code-len")

	// 幂等键按 risk_punishment.idempotency_key 的列宽（VARCHAR(64)）逐字节把关：
	// 唯一键上静默裁断会让两次不同处罚共用同一键，后一次被误判成重试而丢弃，所以只能拒绝。
	accepted := newStore(t)
	in = applyReq(strings.Repeat("k", maxIdempotencyKeyLen))
	_, err = applyPunishment(t, accepted, in)
	wantNoErr(t, "恰好列宽的幂等键", err)
	wantEQ(t, "恰好列宽的幂等键", "rows 总数", len(accepted.punish.rows), 1)

	rejected := newStore(t)
	in = applyReq(strings.Repeat("k", maxIdempotencyKeyLen+1))
	_, err = applyPunishment(t, rejected, in)
	wantErrIs(t, "超出列宽 1 字节的幂等键", err, model.ErrInvalidTarget)
	wantNoCall(t, "超出列宽 1 字节的幂等键", rejected, 0)
}

func TestApplyPunishmentDependencyFailuresPropagateWithoutFakeSuccess(t *testing.T) {
	type step struct {
		method   string
		wantOps  []string
		rowsLeft int // 失败时已落库的行数（爆炸半径）
	}
	cases := []step{
		{"FindByIDempotencyKey", []string{"punish.FindByKey:k"}, 0},
		{"ExpireStale", []string{"punish.FindByKey:k", "punish.ExpireStale:42"}, 0},
		{"ListActiveByMid", []string{"punish.FindByKey:k", "punish.ExpireStale:42", "punish.ListActive:42"}, 0},
		{"Insert", []string{"punish.FindByKey:k", "punish.ExpireStale:42", "punish.ListActive:42", "punish.Insert:k"}, 0},
		// Insert 成功但读回失败：行已经在库里，运营重试同 key 会命中幂等分支。
		{"FindOne", []string{"punish.FindByKey:k", "punish.ExpireStale:42", "punish.ListActive:42", "punish.Insert:k", "punish.FindOne:1"}, 1},
	}
	for _, tc := range cases {
		st := newStore(t)
		boom := errors.New("boom-" + tc.method)
		st.punish.failWith(tc.method, boom)

		reply, err := applyPunishment(t, st, applyReq("k"))
		wantErrIs(t, "下发故障 "+tc.method, err, boom)
		if reply != nil {
			t.Errorf("下发故障 %s：仍返回了响应体 %+v", tc.method, reply)
		}
		wantOps(t, "下发故障序列 "+tc.method, st.log.all(), tc.wantOps)
		wantEQ(t, "下发故障爆炸半径 "+tc.method, "rows 总数", len(st.punish.rows), tc.rowsLeft)
	}

	// 读回故障恢复后，同一幂等键必须走通并返回既有行（不产生第二条处罚）。
	st := newStore(t)
	boomFindOne := errors.New("boom-findone")
	st.punish.failWith("FindOne", boomFindOne)
	_, err := applyPunishment(t, st, applyReq("k"))
	wantErrIs(t, "读回故障", err, boomFindOne)
	st.punish.clear("FindOne")
	before := st.log.snapshot()
	reply, err := applyPunishment(t, st, applyReq("k"))
	wantNoErr(t, "故障恢复后重试", err)
	wantEQ(t, "故障恢复后重试", "created", reply.Created, false)
	wantEQ(t, "故障恢复后重试", "rows 总数", len(st.punish.rows), 1)
	// 恢复后的重试只走幂等短路：不再 Insert，也不再读回。
	wantOps(t, "故障恢复后重试序列", st.log.opsFrom(before), []string{"punish.FindByKey:k"})
	wantEQ(t, "故障恢复后重试", "punishment_id", reply.Punishment.GetPunishmentId(), int64(1))
}

// TestApplyPunishmentLandsActiveAndTakesEffectImmediately 钉住处罚的「下发即生效」链路。
//
// 这里是回归高危点：model/punishment.go 的 INSERT 显式绑定 state 列，DDL 的
// `state TINYINT NOT NULL DEFAULT 1` 因此不会生效——一旦构造行时漏设 State，
// 处罚就落成 state=0，而 ListActiveByMid / ExpireStale / Effective 都按 state=1 判定，
// 结果是「运营看到下发成功、端上完全不生效」，且按 ID 解除只拿到 changed=false 的空操作。
// repository.ApplyPunishment 显式归一 State，本用例保证这条归一不会被再次删掉。
func TestApplyPunishmentLandsActiveAndTakesEffectImmediately(t *testing.T) {
	st := newStore(t)
	reply, err := applyPunishment(t, st, applyReq("state-active"))
	wantNoErr(t, "下发", err)
	id := reply.Punishment.GetPunishmentId()

	row := st.punishmentRow(id)
	wantEQ(t, "落库", "state", row.State, model.PunishmentStateActive)
	wantEQ(t, "回包", "state", int32(reply.Punishment.GetState()), int32(model.PunishmentStateActive))
	wantEQ(t, "落库", "Effective", row.Effective(time.Now().Unix()), true)

	// 端上视角：刚下发的 BLOCK 处罚立即改变裁决，且读处罚列表确实发生了一次。
	before := st.log.snapshot()
	dec, err := checkAction(t, st, commentReq("req-after-apply"))
	wantNoErr(t, "下发后裁决", err)
	wantEQ(t, "下发后裁决", "decision", int32(dec.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "下发后裁决", "basis", dec.Basis, policy.BasisPunishment)
	if dec.Punishment == nil {
		t.Errorf("下发后裁决：处罚摘要缺失，客户端无法渲染原因码与到期时间")
	} else {
		wantEQ(t, "下发后裁决", "摘要 punishment_id", dec.Punishment.GetPunishmentId(), id)
	}
	if n := st.log.countPrefixFrom(before, "punish.ListActive"); n != 1 {
		t.Fatalf("CheckAction 读取处罚列表 %d 次（应为 1）：%v", n, st.log.opsFrom(before))
	}

	// 运营报表视角：ACTIVE 口径与不过滤口径必须给出同一个答案。
	active, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Mid: testMid, State: rpc.PunishmentState_PUNISHMENT_STATE_ACTIVE})
	wantNoErr(t, "按 ACTIVE 过滤", err)
	wantEQ(t, "按 ACTIVE 过滤", "命中数", active.Total, int32(1))
	all, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Mid: testMid})
	wantNoErr(t, "不过滤", err)
	wantEQ(t, "不过滤", "命中数", all.Total, int32(1))

	// 生效中的处罚挡住同 (mid, scope) 的第二条：这条短路同样依赖 state=1。
	_, err = applyPunishment(t, st, applyReq("state-active-2"))
	wantErrIs(t, "重叠处罚", err, model.ErrPunishmentAlreadyActive)
}

// --- LiftPunishment ---

func TestLiftPunishmentByIDAdvancesToTerminalStateOnce(t *testing.T) {
	st := newStore(t)
	id := st.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 10, EndAt: 0,
		State: model.PunishmentStateActive, IdempotencyKey: "lift-me",
	})

	reply, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{
		PunishmentId: id, Operator: 777, Reason: "申诉通过", IdempotencyKey: "lift-req-key",
	})
	wantNoErr(t, "按 ID 解除", err)
	wantOps(t, "按 ID 解除序列", st.log.all(), []string{
		findPunishOneOp(id), liftOp(id), findPunishOneOp(id),
	})
	wantEQ(t, "按 ID 解除", "changed", reply.Changed, true)
	wantEQ(t, "按 ID 解除", "state", int32(reply.Punishment.GetState()), int32(model.PunishmentStateLifted))
	wantEQ(t, "按 ID 解除", "lift_operator", reply.Punishment.GetLiftOperator(), int64(777))

	row := st.punishmentRow(id)
	wantEQ(t, "解除落库", "state", row.State, model.PunishmentStateLifted)
	wantEQ(t, "解除落库", "lift_operator", row.LiftOperator, int64(777))
	wantEQ(t, "解除落库", "lift_reason", row.LiftReason, "申诉通过")
	// 解除请求的幂等键只做日志关联（rpc 契约里没有落库通道），不得污染任何调用轨迹。
	for _, o := range st.log.all() {
		if strings.Contains(o, "lift-req-key") {
			t.Fatalf("解除幂等键进入了依赖调用：%s", o)
		}
	}

	// 重复解除：终态幂等返回，不得再发 UPDATE。
	before := st.log.snapshot()
	again, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: id, Operator: 778, Reason: "再解一次"})
	wantNoErr(t, "重复解除", err)
	wantOps(t, "重复解除序列", st.log.opsFrom(before), []string{findPunishOneOp(id)})
	wantEQ(t, "重复解除", "changed", again.Changed, false)
	wantEQ(t, "重复解除", "state", int32(again.Punishment.GetState()), int32(model.PunishmentStateLifted))
	wantEQ(t, "重复解除", "lift_operator 未被覆盖", st.punishmentRow(id).LiftOperator, int64(777))
}

func TestLiftPunishmentByMidScopeResolvesTargetExplicitly(t *testing.T) {
	st := newStore(t)
	id := st.seedPunishment(model.RiskPunishment{
		Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionReview,
		Operator: testOperator, StartAt: time.Now().Unix() - 10, EndAt: 0,
		State: model.PunishmentStateActive, IdempotencyKey: "by-scope",
	})

	reply, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{
		Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779, Reason: "误罚",
	})
	wantNoErr(t, "按 (mid,scope) 解除", err)
	wantOps(t, "按 (mid,scope) 解除序列", st.log.all(), []string{
		"punish.ExpireStale:42", "punish.ListActive:42",
		findPunishOneOp(id), liftOp(id), findPunishOneOp(id),
	})
	wantEQ(t, "按 (mid,scope) 解除", "changed", reply.Changed, true)
	wantEQ(t, "按 (mid,scope) 解除", "punishment_id", reply.Punishment.GetPunishmentId(), id)

	// 无生效处罚：只发两条读，不得盲发 UPDATE。
	before := st.log.snapshot()
	_, err = liftPunishment(t, st, &rpc.LiftPunishmentReq{Mid: 99, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779})
	wantErrIs(t, "无生效处罚", err, model.ErrPunishmentNotFound)
	wantOps(t, "未命中序列", st.log.opsFrom(before), []string{"punish.ExpireStale:99", "punish.ListActive:99"})
}

func TestLiftPunishmentNeverGuessesAmongAmbiguousRows(t *testing.T) {
	st := newStore(t)
	// 两条同 (mid, scope) 的生效处罚：生产上由并发下发绕过「先查后写」产生
	// （risk_punishment 只有 uniq_idempotency_key，没有 (mid,scope) 唯一索引 ⇒ 缺口 #15）。
	a := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 30, State: model.PunishmentStateActive, IdempotencyKey: "dup-1"})
	b := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionReview,
		Operator: testOperator, StartAt: time.Now().Unix() - 20, State: model.PunishmentStateActive, IdempotencyKey: "dup-2"})

	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779})
	wantErrIs(t, "歧义处罚", err, model.ErrAmbiguousPunishment)
	wantOps(t, "歧义序列", st.log.all(), []string{"punish.ExpireStale:42", "punish.ListActive:42"})
	for _, id := range []int64{a, b} {
		if st.punishmentRow(id).State != model.PunishmentStateActive {
			t.Fatalf("歧义时误改了 punishment_id=%d 的状态", id)
		}
	}

	// 显式给 ID 仍可精确解除（歧义只封锁「猜一条」这条路径）。
	before := st.log.snapshot()
	reply, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: b, Operator: 779})
	wantNoErr(t, "歧义后按 ID 解除", err)
	wantOps(t, "歧义后按 ID 解除序列", st.log.opsFrom(before), []string{findPunishOneOp(b), liftOp(b), findPunishOneOp(b)})
	wantEQ(t, "歧义后按 ID 解除", "changed", reply.Changed, true)
	wantEQ(t, "歧义后按 ID 解除", "另一条不受影响", st.punishmentRow(a).State, model.PunishmentStateActive)
}

func TestLiftPunishmentScopeIsExactNotCovering(t *testing.T) {
	// 全域处罚存在时，按 (mid, 具体动作) 解除必须未命中：
	// 解除动作级请求顺手解掉全域封禁是灾难性后果（见 liftpunishmentlogic.go:27-31 注释）。
	st := newStore(t)
	global := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionAll, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 10, State: model.PunishmentStateActive, IdempotencyKey: "g"})
	st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionLogin, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 10, State: model.PunishmentStateActive, IdempotencyKey: "l"})

	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779})
	wantErrIs(t, "scope 精确匹配", err, model.ErrPunishmentNotFound)
	wantEQ(t, "scope 精确匹配", "全域处罚仍在生效", st.punishmentRow(global).State, model.PunishmentStateActive)

	// 显式按全域处罚的 ID 解除是允许的（运营确实想解全域）。
	reply, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: global, Operator: 779})
	wantNoErr(t, "按 ID 解全域", err)
	wantEQ(t, "按 ID 解全域", "changed", reply.Changed, true)
	wantEQ(t, "按 ID 解全域", "scope", int32(reply.Punishment.GetScope()), int32(model.ActionAll))
}

// TestLiftPunishmentExpiredRowDisagreesBetweenPaths 是「已知缺口 #13」的行为哨兵：
// 同一行「已到期的 ACTIVE 处罚」，两条解除路径给出**不同终态**——
// 按 punishment_id 走 repository.go:321 的判断（state 仍是 ACTIVE ⇒ 不算终态）直接 UPDATE 成 LIFTED，
// 按 (mid, scope) 则先 ExpireStale 推进为 EXPIRED，随后 ListActiveByMid 读不到 ⇒ ErrPunishmentNotFound。
// 结果：终态与 lift_operator 取决于运营从哪个入口点进来，审计口径不可复现。
func TestLiftPunishmentExpiredRowDisagreesBetweenPaths(t *testing.T) {
	expiredAt := time.Now().Unix() - 5

	byID := newStore(t)
	id := byID.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 60, EndAt: expiredAt,
		State: model.PunishmentStateActive, IdempotencyKey: "exp-1"})
	reply, err := liftPunishment(t, byID, &rpc.LiftPunishmentReq{PunishmentId: id, Operator: 780, Reason: "补手续"})
	wantNoErr(t, "按 ID 解除到期处罚", err)
	wantOps(t, "按 ID 解除到期序列", byID.log.all(), []string{findPunishOneOp(id), liftOp(id), findPunishOneOp(id)})
	wantEQ(t, "缺口 #13 现状（按 ID）", "changed", reply.Changed, true)
	row := byID.punishmentRow(id)
	wantEQ(t, "缺口 #13 现状（按 ID）", "state", row.State, model.PunishmentStateLifted)
	wantEQ(t, "缺口 #13 现状（按 ID）", "lift_operator", row.LiftOperator, int64(780))

	byScope := newStore(t)
	id2 := byScope.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 60, EndAt: expiredAt,
		State: model.PunishmentStateActive, IdempotencyKey: "exp-2"})
	_, err = liftPunishment(t, byScope, &rpc.LiftPunishmentReq{Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 780, Reason: "补手续"})
	wantErrIs(t, "按 (mid,scope) 解除到期处罚", err, model.ErrPunishmentNotFound)
	wantOps(t, "按 (mid,scope) 解除到期序列", byScope.log.all(), []string{"punish.ExpireStale:42", "punish.ListActive:42"})
	wantEQ(t, "缺口 #13 现状（按 scope）", "state", byScope.punishmentRow(id2).State, model.PunishmentStateExpired)
}

func TestLiftPunishmentDependencyFailuresPropagateWithoutFakeSuccess(t *testing.T) {
	boom := errors.New("boom-lift")

	// 首读失败：一条 UPDATE 都不发。
	st := newStore(t)
	id := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "f1"})
	st.punish.failWith("FindOne", boom)
	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: id, Operator: 779})
	wantErrIs(t, "首读失败", err, boom)
	wantOps(t, "首读失败序列", st.log.all(), []string{findPunishOneOp(id)})
	wantEQ(t, "首读失败", "state 未变", st.punishmentRow(id).State, model.PunishmentStateActive)

	// 解除语句本身失败（非终态哨兵）：原样上抛。
	st2 := newStore(t)
	id2 := st2.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "f2"})
	st2.punish.failWith("Lift", boom)
	_, err = liftPunishment(t, st2, &rpc.LiftPunishmentReq{PunishmentId: id2, Operator: 779})
	wantErrIs(t, "UPDATE 失败", err, boom)
	wantOps(t, "UPDATE 失败序列", st2.log.all(), []string{findPunishOneOp(id2), liftOp(id2)})

	// UPDATE 成功但读回失败：状态已推进（爆炸半径），重试必须幂等而不是二次解除。
	// 故障只注入到第 2 次 FindOne（读回），否则会连前置定位那次读一起失败，测不到本段。
	st3 := newStore(t)
	id3 := st3.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "f3"})
	st3.punish.failFrom("FindOne", 2, boom)
	_, err = liftPunishment(t, st3, &rpc.LiftPunishmentReq{PunishmentId: id3, Operator: 779})
	wantErrIs(t, "读回失败", err, boom)
	wantOps(t, "读回失败序列", st3.log.all(), []string{findPunishOneOp(id3), liftOp(id3), findPunishOneOp(id3)})
	wantEQ(t, "读回失败爆炸半径", "state 已推进", st3.punishmentRow(id3).State, model.PunishmentStateLifted)
	st3.punish.clear("FindOne")
	retry, err := liftPunishment(t, st3, &rpc.LiftPunishmentReq{PunishmentId: id3, Operator: 781})
	wantNoErr(t, "读回失败后重试", err)
	wantEQ(t, "读回失败后重试", "changed", retry.Changed, false)
	wantEQ(t, "读回失败后重试", "lift_operator 保持首次值", st3.punishmentRow(id3).LiftOperator, int64(779))

	// 并发解除：UPDATE 影响 0 行（真实 model 返回未包装哨兵）→ 幂等返回终态，不算失败。
	st4 := newStore(t)
	id4 := st4.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "f4"})
	st4.punish.failWith("Lift", model.ErrPunishmentAlreadyFinished)
	got, err := liftPunishment(t, st4, &rpc.LiftPunishmentReq{PunishmentId: id4, Operator: 779})
	wantNoErr(t, "并发解除 0 行", err)
	wantOps(t, "并发解除序列", st4.log.all(), []string{findPunishOneOp(id4), liftOp(id4), findPunishOneOp(id4)})
	wantEQ(t, "并发解除", "changed", got.Changed, false)

	// (mid, scope) 路径的两次读各自失败：都在第一条 UPDATE 之前停下。
	for _, m := range []struct {
		method  string
		wantOps []string
	}{{"ExpireStale", []string{"punish.ExpireStale:42"}}, {"ListActiveByMid", []string{"punish.ExpireStale:42", "punish.ListActive:42"}}} {
		st5 := newStore(t)
		st5.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
			Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "f5"})
		st5.punish.failWith(m.method, boom)
		_, err = liftPunishment(t, st5, &rpc.LiftPunishmentReq{Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779})
		wantErrIs(t, "定位阶段失败 "+m.method, err, boom)
		wantOps(t, "定位阶段序列 "+m.method, st5.log.all(), m.wantOps)
		wantEQ(t, "定位阶段 state 未变 "+m.method, "state", st5.punishmentRow(1).State, model.PunishmentStateActive)
	}
}

func TestLiftPunishmentGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.LiftPunishmentReq
		want   error
		remark string
	}{
		{"动作越界", &rpc.LiftPunishmentReq{PunishmentId: 1, Operator: testOperator, Scope: rpc.GuardedAction(9)}, model.ErrInvalidTarget, "logic 守卫"},
		{"解除说明超长", &rpc.LiftPunishmentReq{PunishmentId: 1, Operator: testOperator, Reason: strings.Repeat("x", maxReasonLen+1)}, model.ErrInvalidTarget, "logic 守卫"},
		// 操作人必填落在 repository：但它先于任何读，所以同样不得触库。
		{"无操作人按 ID", &rpc.LiftPunishmentReq{PunishmentId: 1, Operator: 0}, model.ErrOperatorRequired, "repository 守卫"},
		{"无操作人按 scope", &rpc.LiftPunishmentReq{Mid: testMid, Scope: rpc.GuardedAction_ACTION_COMMENT}, model.ErrOperatorRequired, "repository 守卫"},
		{"按 scope 但无账号", &rpc.LiftPunishmentReq{Mid: 0, Scope: rpc.GuardedAction_ACTION_COMMENT, Operator: 779}, model.ErrInvalidTarget, "repository 守卫"},
	}
	for _, tc := range cases {
		st := newStore(t)
		st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
			Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "guard"})
		_, err := liftPunishment(t, st, tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		// 守卫必须先于任何库调用（尤其：无操作人时不得先读一遍再拒）。
		wantNoCall(t, tc.name, st, 0)
	}
	// 处罚 ID 不存在：只发一条读。
	st := newStore(t)
	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: 555, Operator: 779})
	wantErrIs(t, "处罚不存在", err, model.ErrPunishmentNotFound)
	wantOps(t, "不存在序列", st.log.all(), []string{"punish.FindOne:555"})
}

func TestLiftPunishmentNeverTouchesRedis(t *testing.T) {
	st := newStore(t)
	id := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "no-cache"})
	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: id, Operator: 779})
	wantNoErr(t, "解除", err)
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Fatalf("解除触碰 Redis %d 次：%v", n, st.log.all())
	}
}

// --- ListPunishments ---

func TestListPunishmentsFiltersPagesAndReadsBackRows(t *testing.T) {
	st := newStore(t)
	now := time.Now().Unix()
	// 种子顺序即自增 ID 顺序：1..5。
	st.seedPunishment(model.RiskPunishment{Mid: 11, Scope: model.ActionComment, Decision: model.DecisionBlock, Operator: testOperator,
		StartAt: now - 100, State: model.PunishmentStateActive, IdempotencyKey: "p1", Reason: "r1"})
	st.seedPunishment(model.RiskPunishment{Mid: 11, Scope: model.ActionLogin, Decision: model.DecisionReview, Operator: testOperator,
		StartAt: now - 50, State: model.PunishmentStateLifted, IdempotencyKey: "p2", Reason: "r2"})
	st.seedPunishment(model.RiskPunishment{Mid: 22, Scope: model.ActionAll, Decision: model.DecisionBlock, Operator: testOperator,
		StartAt: now - 10, State: model.PunishmentStateExpired, IdempotencyKey: "p3", Reason: "r3"})
	st.seedPunishment(model.RiskPunishment{Mid: 22, Scope: model.ActionAll, Decision: model.DecisionBlock, Operator: testOperator,
		StartAt: now + 3600, State: model.PunishmentStateActive, IdempotencyKey: "p4", Reason: "未来生效"})
	st.seedPunishment(model.RiskPunishment{Mid: 33, Scope: model.ActionLiveStart, Decision: model.DecisionChallenge, Operator: testOperator,
		StartAt: now - 10, EndAt: now - 1, State: model.PunishmentStateActive, IdempotencyKey: "p5", Reason: "到期未推进"})

	all, err := listPunishments(t, st, &rpc.ListPunishmentsReq{})
	wantNoErr(t, "无过滤列表", err)
	wantOps(t, "无过滤序列", st.log.all(), []string{"punish.List:0/0/0/false/0/20"})
	wantEQ(t, "无过滤", "total", all.Total, int32(5))
	wantEQ(t, "无过滤", "pn", all.Pn, int32(1))
	wantEQ(t, "无过滤", "ps", all.Ps, int32(20))
	// ORDER BY punishment_id DESC。
	for i, want := range []int64{5, 4, 3, 2, 1} {
		wantEQ(t, "无过滤", fmt.Sprintf("ids[%d]", i), all.Punishments[i].GetPunishmentId(), want)
	}
	// 运营视角接口必须回 reason（端上摘要接口故意不回，二者故意分开）。
	wantEQ(t, "无过滤", "reason 下发", all.Punishments[4].GetReason(), "r1")
	wantEQ(t, "无过滤", "operator 下发", all.Punishments[4].GetOperator(), testOperator)

	// mid + state 组合过滤（22 的 EXPIRED 一条）。
	filtered, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Mid: 22, State: rpc.PunishmentState_PUNISHMENT_STATE_EXPIRED})
	wantNoErr(t, "组合过滤", err)
	wantOps(t, "组合过滤序列", st.log.opsFrom(1), []string{"punish.List:22/0/3/false/0/20"})
	wantEQ(t, "组合过滤", "total", filtered.Total, int32(1))
	wantEQ(t, "组合过滤", "id", filtered.Punishments[0].GetPunishmentId(), int64(3))

	// scope 过滤（0 表示不过滤，与 repository 的 `scope > ActionAll` 同口径）。
	// 期望串里的 5 是 ACTION_LOGIN 的编号（proto 与 model 常量同号），逐字进 SQL 参数。
	scope, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Scope: rpc.GuardedAction_ACTION_LOGIN})
	wantNoErr(t, "scope 过滤", err)
	wantOps(t, "scope 过滤序列", st.log.opsFrom(2), []string{"punish.List:0/5/0/false/0/20"})
	wantEQ(t, "scope 过滤", "total", scope.Total, int32(1))

	// onlyActive：排除未来生效、到期未推进、终态三种行。
	active, err := listPunishments(t, st, &rpc.ListPunishmentsReq{OnlyActive: true})
	wantNoErr(t, "onlyActive", err)
	wantOps(t, "onlyActive 序列", st.log.opsFrom(3), []string{"punish.List:0/0/0/true/0/20"})
	wantEQ(t, "onlyActive", "total", active.Total, int32(1))
	wantEQ(t, "onlyActive", "id", active.Punishments[0].GetPunishmentId(), int64(1))

	// 分页：pn=2/ps=2 → offset=2；DESC 序取到 id=3。
	page2, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Pn: 2, Ps: 2})
	wantNoErr(t, "第二页", err)
	wantOps(t, "第二页序列", st.log.opsFrom(4), []string{"punish.List:0/0/0/false/2/2"})
	wantInt64s(t, "第二页", "ids", idsOfPunishments(page2), []int64{3, 2})
	wantEQ(t, "第二页", "total", page2.Total, int32(5))

	// 越界页：total 仍回传，列表为空。
	empty, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "total", empty.Total, int32(5))
	wantEQ(t, "越界页", "len", len(empty.Punishments), 0)

	// 无命中：total=0 时不得发第二条查询（替身与真实 SQL 同口径）。
	before := st.log.snapshot()
	none, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Mid: 999})
	wantNoErr(t, "无命中", err)
	wantOps(t, "无命中序列", st.log.opsFrom(before), []string{"punish.List:999/0/0/false/0/20"})
	wantEQ(t, "无命中", "total", none.Total, int32(0))
	wantEQ(t, "无命中", "len", len(none.Punishments), 0)
}

func idsOfPunishments(reply *rpc.ListPunishmentsReply) []int64 {
	out := make([]int64, 0, len(reply.Punishments))
	for _, p := range reply.Punishments {
		out = append(out, p.GetPunishmentId())
	}
	return out
}

func TestListPunishmentsPageSizeClampedToContractCap(t *testing.T) {
	cases := []struct {
		name    string
		pn, ps  int32
		wantOps string
		wantPn  int32
		wantPs  int32
	}{
		{"零值走默认", 0, 0, "punish.List:0/0/0/false/0/20", 1, 20},
		{"负数走默认", -5, -1, "punish.List:0/0/0/false/0/20", 1, 20},
		{"上限 50", 1, 999, "punish.List:0/0/0/false/0/50", 1, 50},
		{"恰为上限", 1, int32(maxPageSize), "punish.List:0/0/0/false/0/50", 1, int32(maxPageSize)},
		{"第三页", 3, 10, "punish.List:0/0/0/false/20/10", 3, 10},
	}
	for _, tc := range cases {
		st := newStore(t)
		st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
			Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "cap"})
		reply, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Pn: tc.pn, Ps: tc.ps})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, st.log.all(), []string{tc.wantOps})
		wantEQ(t, tc.name, "pn", reply.Pn, tc.wantPn)
		wantEQ(t, tc.name, "ps", reply.Ps, tc.wantPs)
	}
}

func TestListPunishmentsGuardsAndFailureSurface(t *testing.T) {
	// 动作/state 越界必须先拒绝，不发任何查询。
	for _, tc := range []struct {
		name string
		in   *rpc.ListPunishmentsReq
	}{
		{"动作越界", &rpc.ListPunishmentsReq{Scope: rpc.GuardedAction(9)}},
		{"state 越界", &rpc.ListPunishmentsReq{State: rpc.PunishmentState(4)}},
		{"state 负值", &rpc.ListPunishmentsReq{State: rpc.PunishmentState(-1)}},
	} {
		st := newStore(t)
		_, err := listPunishments(t, st, tc.in)
		wantErrIs(t, tc.name, err, model.ErrInvalidTarget)
		wantNoCall(t, tc.name, st, 0)
	}

	// 查询失败原样上抛（列表接口没有降级空间：静默返回空表会让运营以为「没有处罚」）。
	st := newStore(t)
	st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, State: model.PunishmentStateActive, IdempotencyKey: "boom"})
	boom := errors.New("boom-list")
	st.punish.failWith("List", boom)
	reply, err := listPunishments(t, st, &rpc.ListPunishmentsReq{})
	wantErrIs(t, "列表故障", err, boom)
	if reply != nil {
		t.Errorf("列表故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "列表故障序列", st.log.all(), []string{"punish.List:0/0/0/false/0/20"})

	st.punish.clear("List")
	ok, err := listPunishments(t, st, &rpc.ListPunishmentsReq{})
	wantNoErr(t, "列表故障恢复", err)
	wantEQ(t, "列表故障恢复", "total", ok.Total, int32(1))
}

// TestListPunishmentsLiftAuditHasNoContractExit 钉住「已知缺口 #16」：
// lift_reason 会落库（model/punishment.go:163-178 的 UPDATE 写了该列），
// 但 rpc.Punishment 契约里没有对应字段，convert.go:84-104 也无从映射 ⇒
// 运营后台看得到「谁解除的」，看不到「以什么理由解除」，而后者正是申诉复核最需要的一行。
func TestListPunishmentsLiftAuditHasNoContractExit(t *testing.T) {
	st := newStore(t)
	id := st.seedPunishment(model.RiskPunishment{Mid: testMid, Scope: model.ActionComment, Decision: model.DecisionBlock,
		Operator: testOperator, StartAt: time.Now().Unix() - 10, State: model.PunishmentStateActive, IdempotencyKey: "lift-audit"})
	_, err := liftPunishment(t, st, &rpc.LiftPunishmentReq{PunishmentId: id, Operator: 780, Reason: "申诉材料齐全"})
	wantNoErr(t, "解除", err)

	// 落库侧完整：解除人 + 解除理由都在行里。
	row := st.punishmentRow(id)
	wantEQ(t, "落库审计", "lift_operator", row.LiftOperator, int64(780))
	wantEQ(t, "落库审计", "lift_reason", row.LiftReason, "申诉材料齐全")

	// 契约侧缺失：列表接口把整条记录序列化后仍找不到解除理由。
	reply, err := listPunishments(t, st, &rpc.ListPunishmentsReq{Mid: testMid})
	wantNoErr(t, "解除后列表", err)
	wantEQ(t, "解除后列表", "total", reply.Total, int32(1))
	wantEQ(t, "契约出口", "lift_operator 可见", reply.Punishments[0].GetLiftOperator(), int64(780))
	if strings.Contains(reply.Punishments[0].String(), "申诉材料齐全") {
		t.Fatal("缺口 #16 已修复：rpc.Punishment 已有 lift_reason 出口，请同步 README 与 convert.go 的映射")
	}
}
