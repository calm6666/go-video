package logic

// applymoderationresult_test.go 覆盖 ApplyModerationResult：moderation.result.v1 的消费者入口
// （事件消费者落在 internal/consumer，接入后调用这里）——
// 参数守卫 → 结论映射 → 弹幕存在性 → 事件去重快查 → 状态机门禁 →
// 主表 CAS + op_log 同事务 → 提交后才调整段计数与段列表缓存。
//
// 四条被测不变量（AGENTS.md §5「审核结论只能由本方法推进」、§8「回调只能推进合法状态」）：
//  1. 只有 VERDICT_PASS 能把弹幕放进普通池对所有人可见；非法/越界结论在碰任何依赖之前被拒；
//  2. 带 event_id 的重复投递必须被挡下，且不产生第二次状态写入、第二次留痕、第二次段计数；
//  3. 非法迁移（已删除、枚举外脏状态）返回错误，绝不「顺手洗成可见」；
//  4. 事务内任何一步失败都整体回滚；缓存/派生侧失败只降级，但失效调用必须仍然发生。
//
// 本轮另外钉住四条现状（见 README「已知缺口」4~7）：成功迁移的应答 applied 恒 false、
// 幂等判定只看 state 不看 pool、段缓存/段计数写失败被就地吞掉、
// CAS 未命中后重读失败时下游原始错误顶替了可重试哨兵。
// 相应注释写在对应用例上，收严后那些断言必须变红。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

const (
	modOid    = int64(1001)
	modAid    = int64(500)
	modAuthor = int64(7001) // 弹幕作者：审核回写与归属无关，只按 dmid 定位
	modSeg    = int32(4)    // progress_ms=24_500 在 6 秒分段下的分段号
	modTask   = int64(9001) // 回写携带的机审任务 ID

	modMachineOp = int64(0)    // 机审/事件消费：operator 留空
	modHumanOp   = int64(9002) // 人审处理人
	modReason    = "命中辱骂词模型 0.92"
)

// --- 期望序列片段（格式与替身记轨迹的口径逐字一致） ---

func existsEventOp(eventID string) string { return "oplog.ExistsEvent:" + eventID }

// modApplyOps 拼「读弹幕 →（事件去重快查）→ 事务（留痕/CAS/task 回填）→ 提交 → 派生与失效」的
// 完整期望序列。eventID 非空 ⇒ 留痕排在 CAS 之前（uniq_event 是并发重复消费的最终防线，
// 见 internal/repository/repository.go:286-293）；为空 ⇒ 留痕排在 CAS 之后（repository.go:305-309）。
func modApplyOps(dmid int64, from, to, toPool int32, eventID string, taskID int64, tail []string) []string {
	ops := []string{findOneOp(dmid)}
	if eventID != "" {
		ops = append(ops, existsEventOp(eventID))
	}
	ops = append(ops, opTxBegin)
	cas := transitionTxOp(dmid, from, to, toPool)
	if eventID != "" {
		ops = append(ops, oplogTxOp(dmid, model.ActionModeration, eventID), cas)
	} else {
		ops = append(ops, cas)
	}
	if taskID > 0 {
		ops = append(ops, setTaskTxOp(dmid, taskID))
	}
	if eventID == "" {
		ops = append(ops, oplogTxOp(dmid, model.ActionModeration, ""))
	}
	ops = append(ops, opTxCommit)
	return append(ops, tail...)
}

// modGainOps / modLossOps / modDelOnlyOps 复刻 repository.ApplyStateTransition:323-332 的三分支：
// 不可见→可见段计数 +1、可见→不可见段计数 -1、两侧都不可见时只失效段列表缓存。
func modGainOps() []string {
	return []string{segIncrOp(modOid, modSeg, 1), cntIncrOp(modOid, modSeg, 1), segDelOp(modOid, modSeg)}
}

func modLossOps() []string {
	return []string{segIncrOp(modOid, modSeg, -1), cntIncrOp(modOid, modSeg, -1), segDelOp(modOid, modSeg)}
}

func modDelOnlyOps() []string { return []string{segDelOp(modOid, modSeg)} }

// modCasOp 是 CAS 那一步的期望值（非法/短路场景里它根本不该出现，出现在错误断言里）。
func modCasOp(dmid int64, from, to, toPool int32) string {
	return transitionTxOp(dmid, from, to, toPool)
}

// --- 布景与调用 ---

func applyModeration(t *testing.T, e *env, in *rpc.ApplyModerationResultReq) (*rpc.ApplyModerationResultReply, error) {
	t.Helper()
	return NewApplyModerationResultLogic(context.Background(), e.svcCtx).ApplyModerationResult(in)
}

// modSeed 布一行指定状态/池的弹幕（首行 dmid 恒为 101），moderation_task_id 从 0 起。
func modSeed(t *testing.T, e *env, state, pool int32) *model.Danmaku {
	t.Helper()
	return modSeedTask(t, e, state, pool, 0)
}

// modSeedTask 同上，但把机审任务号预置成非 0，用于钉「只填首个任务号」。
func modSeedTask(t *testing.T, e *env, state, pool int32, taskID int64) *model.Danmaku {
	t.Helper()
	return seedDanmaku(t, e.st, &model.Danmaku{
		Oid: modOid, Aid: modAid, Mid: modAuthor, ProgressMs: 24_500,
		Mode: int32(rpc.DanmakuMode_MODE_SCROLL), Fontsize: 25, Color: 0x112233,
		Content: "等待审核结论回写的弹幕", State: state, Pool: pool, SegNo: modSeg,
		IdempotencyKey: "idem-mod-1", ModerationTaskId: taskID,
		TraceId: "trace-seed", Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})
}

// modReq 组一条「事件驱动 + 机审无人」的结论回写请求（moderation.result.v1 消费者的形态）。
func modReq(dmid int64, verdict rpc.ModerationVerdict, eventID string) *rpc.ApplyModerationResultReq {
	return &rpc.ApplyModerationResultReq{
		Dmid: dmid, TaskId: modTask, Verdict: verdict, Reason: modReason,
		Operator: modMachineOp, EventId: eventID, TraceId: "trace-mod",
	}
}

// modPlainReq 组一条不带 event_id、不带 task_id 的裸回写（运营后台手工改判的形态）。
func modPlainReq(dmid int64, verdict rpc.ModerationVerdict, operator int64) *rpc.ApplyModerationResultReq {
	return &rpc.ApplyModerationResultReq{
		Dmid: dmid, Verdict: verdict, Reason: modReason, Operator: operator, TraceId: "trace-mod",
	}
}

// modRow 读主表那一行（副本，断言用）。
func modRow(t *testing.T, e *env, dmid int64) *model.Danmaku {
	t.Helper()
	row := e.st.danmaku.get(dmid)
	if row == nil {
		t.Fatalf("前置条件不成立：弹幕 %d 不在库里", dmid)
	}
	return row
}

// cacheCount 读缓存段计数（键不存在返回 -1，与段表 countAt 同口径）。
func cacheCount(e *env, oid int64, seg int32) int32 {
	v, ok := e.st.cache.count(oid, seg)
	if !ok {
		return -1
	}
	return v
}

// --- 守卫 ---

// TestApplyModerationResultRejectsInvalidRequests 守卫表：dmid 与结论两个守卫都必须发生在
// 读主表之前——一条畸形结论不许产生「先查一遍弹幕再决定认不认」的读放大。
// dmid 非法优先于结论非法（applymoderationresultlogic.go:38 在 :41 之前）。
func TestApplyModerationResultRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ApplyModerationResultReq
		want error
	}{
		{"dmid 为 0", &rpc.ApplyModerationResultReq{Verdict: rpc.ModerationVerdict_VERDICT_PASS}, model.ErrInvalidDmid},
		{"dmid 为负", &rpc.ApplyModerationResultReq{Dmid: -7, Verdict: rpc.ModerationVerdict_VERDICT_PASS}, model.ErrInvalidDmid},
		{"dmid 非法优先于结论非法", &rpc.ApplyModerationResultReq{Dmid: 0}, model.ErrInvalidDmid},
		{"结论未指定（VERDICT_UNSPECIFIED）", &rpc.ApplyModerationResultReq{Dmid: 101}, model.ErrInvalidVerdict},
		{"结论是枚举外的 7", &rpc.ApplyModerationResultReq{Dmid: 101, Verdict: rpc.ModerationVerdict(7)}, model.ErrInvalidVerdict},
		{"结论是负值", &rpc.ApplyModerationResultReq{Dmid: 101, Verdict: rpc.ModerationVerdict(-1)}, model.ErrInvalidVerdict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := applyModeration(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍被受理 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			wantCount(t, tc.name, e.st.log, "tx.", 0)
			wantCount(t, tc.name, e.st.log, "cache.", 0)
		})
	}
}

// --- 结论映射与正常路径 ---

// TestApplyModerationResultProjectsVerdictToStatePoolAndAudit 三种结论各跑一次正常路径：
// 逐字段钉死「结论 → 目标状态/池」的映射、「主表被改成的每一列」和「同事务那条 op_log」。
// 段计数的增减口径同样在这里钉：只有可见性真的翻转才动计数（repository.go:323-332）。
func TestApplyModerationResultProjectsVerdictToStatePoolAndAudit(t *testing.T) {
	cases := []struct {
		name                string
		from, fromPool      int32
		verdict             rpc.ModerationVerdict
		wantState, wantPool int32
		// delta：+1 不可见→可见、-1 可见→不可见、0 两侧都不可见
		delta int32
	}{
		{
			"通过：待审 → 普通池，这是唯一让弹幕对所有人可见的路径",
			model.StatePending, model.PoolReview,
			rpc.ModerationVerdict_VERDICT_PASS, model.StateNormal, model.PoolNormal, 1,
		},
		{
			"转人审：已下发的普通池收回审核池",
			model.StateNormal, model.PoolNormal,
			rpc.ModerationVerdict_VERDICT_REVIEW, model.StatePending, model.PoolReview, -1,
		},
		{
			"驳回：折叠 → 屏蔽池，两侧本来就不可见，只失效段列表",
			model.StateFolded, model.PoolBlock,
			rpc.ModerationVerdict_VERDICT_REJECT, model.StateRejected, model.PoolBlock, 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := modSeed(t, e, tc.from, tc.fromPool)
			seedSegment(t, e.st, modOid, modSeg, 7)
			e.st.cache.warmSegmentCount(modOid, modSeg, 7)
			e.st.cache.warmSegment(modOid, modSeg, seeded) // 段列表缓存里有一份下发包
			calledAt := time.Now().Unix()

			reply, err := applyModeration(t, e, modReq(seeded.Dmid, tc.verdict, "evt-project"))
			wantNoErr(t, tc.name, err)
			if reply == nil {
				t.Fatalf("%s：回复 = nil, want 非空", tc.name)
			}

			// 应答投影：状态/池必须是结论映射出来的目标值
			wantEQ(t, tc.name, "dmid", reply.GetDmid(), seeded.Dmid)
			wantEQ(t, tc.name, "state", reply.GetState(), tc.wantState)
			wantEQ(t, tc.name, "pool", reply.GetPool(), tc.wantPool)

			// 主表：状态与池被推进，其余列一个都不许丢，task_id 被首次回填
			row := modRow(t, e, seeded.Dmid)
			wantEQ(t, tc.name, "state", row.State, tc.wantState)
			wantEQ(t, tc.name, "pool", row.Pool, tc.wantPool)
			wantEQ(t, tc.name, "moderation_task_id", row.ModerationTaskId, modTask)
			wantEQ(t, tc.name, "oid", row.Oid, modOid)
			wantEQ(t, tc.name, "aid", row.Aid, modAid)
			wantEQ(t, tc.name, "mid（不改成处理人）", row.Mid, modAuthor)
			wantEQ(t, tc.name, "seg_no", row.SegNo, modSeg)
			wantEQ(t, tc.name, "progress_ms", row.ProgressMs, int64(24_500))
			wantEQ(t, tc.name, "content", row.Content, "等待审核结论回写的弹幕")
			wantEQ(t, tc.name, "idempotency_key", row.IdempotencyKey, "idem-mod-1")
			wantEQ(t, tc.name, "trace_id（不被应答 trace 覆盖）", row.TraceId, "trace-seed")
			wantEQ(t, tc.name, "ctime", row.Ctime, int64(1_700_000_000))
			if row.Mtime <= 1_700_000_000 || row.Mtime < calledAt {
				t.Errorf("%s：mtime = %d, want 被刷成当前时间（原值 1700000000）", tc.name, row.Mtime)
			}

			// 审计留痕：同事务、同 dmid、from→to 可追，机审无人时角色是系统
			if len(e.st.opLog.rows) != 1 {
				t.Fatalf("%s：op_log 行数 = %d, want 1", tc.name, len(e.st.opLog.rows))
			}
			lg := *e.st.opLog.rows[0]
			wantEQ(t, tc.name, "log_id", lg.LogID, int64(301))
			wantEQ(t, tc.name, "dmid", lg.Dmid, seeded.Dmid)
			wantEQ(t, tc.name, "action", lg.Action, model.ActionModeration)
			wantEQ(t, tc.name, "from_state", lg.FromState, tc.from)
			wantEQ(t, tc.name, "to_state", lg.ToState, tc.wantState)
			wantEQ(t, tc.name, "operator_mid", lg.OperatorMid, int64(0))
			wantEQ(t, tc.name, "operator_role", lg.OperatorRole, model.RoleSystem)
			wantEQ(t, tc.name, "reason", lg.Reason, modReason)
			wantEQ(t, tc.name, "event_id", lg.EventID, "evt-project")
			wantEQ(t, tc.name, "trace_id", lg.TraceID, "trace-mod")
			if lg.Ctime < calledAt || lg.Ctime > calledAt+2 {
				t.Errorf("%s：op_log ctime = %d, want ≈ %d", tc.name, lg.Ctime, calledAt)
			}
			wantEQ(t, tc.name, "留痕在事务内", e.st.opLog.txSawSession, true)
			wantEQ(t, tc.name, "留痕绕过事务", e.st.opLog.txNilSession, false)
			wantEQ(t, tc.name, "CAS 在事务内", e.st.danmaku.txSawSession, true)
			wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, 1)
			wantEQ(t, tc.name, "提交次数", e.st.conn.committed, 1)

			// 完整有序序列：读 → 去重快查 → 事务（留痕先于 CAS）→ 提交 → 派生与失效
			tail := modDelOnlyOps()
			switch {
			case tc.delta > 0:
				tail = modGainOps()
			case tc.delta < 0:
				tail = modLossOps()
			}
			wantOps(t, tc.name, e.ops(), modApplyOps(seeded.Dmid, tc.from, tc.wantState, tc.wantPool, "evt-project", modTask, tail))

			// 派生投影只在可见性真的翻转时移动，且 DB 与缓存同步
			wantEQ(t, tc.name, "段表计数", e.st.segment.countAt(modOid, modSeg), 7+tc.delta)
			wantEQ(t, tc.name, "缓存段计数", cacheCount(e, modOid, modSeg), 7+tc.delta)
			wantEQ(t, tc.name, "段表 Incr 次数", countIn(e.ops(), "segment.Incr"), boolToInt(tc.delta != 0))
			_, segCached := e.st.cache.segmentRows(modOid, modSeg)
			wantEQ(t, tc.name, "段列表缓存已失效", segCached, false)
			// 回写不许动词库/屏蔽视图缓存：它们与「结论生效」无关
			wantCount(t, tc.name, e.st.log, "cache.DelBW", 0)
			wantCount(t, tc.name, e.st.log, "cache.DelUB", 0)
			wantCount(t, tc.name, e.st.log, "cache.GetSeg", 0)
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestApplyModerationResultStateMatrixPinsReachableTransitions 逐「在库状态 × 结论」跑一遍门禁：
// 哪个组合真被推进、哪个按幂等短路、哪个被判非法，全部钉成一张表。
// 已删除态和枚举外脏值必须报错且零副作用——绝不能把已删内容或脏状态洗成可见。
func TestApplyModerationResultStateMatrixPinsReachableTransitions(t *testing.T) {
	const (
		pass   = rpc.ModerationVerdict_VERDICT_PASS
		review = rpc.ModerationVerdict_VERDICT_REVIEW
		reject = rpc.ModerationVerdict_VERDICT_REJECT
	)
	cases := []struct {
		name      string
		from      int32
		fromPool  int32
		verdict   rpc.ModerationVerdict
		wantErr   error // 非 nil ⇒ 状态机拒绝
		shortCkt  bool  // true ⇒ 「状态已达成」幂等短路
		wantState int32
		wantPool  int32
		delta     int32
	}{
		{"待审 + 通过 → 普通池", model.StatePending, model.PoolReview, pass, nil, false, model.StateNormal, model.PoolNormal, 1},
		{"待审 + 转人审 → 短路", model.StatePending, model.PoolReview, review, nil, true, model.StatePending, model.PoolReview, 0},
		{"待审 + 驳回 → 屏蔽池", model.StatePending, model.PoolReview, reject, nil, false, model.StateRejected, model.PoolBlock, 0},

		{"普通池 + 通过 → 短路", model.StateNormal, model.PoolNormal, pass, nil, true, model.StateNormal, model.PoolNormal, 0},
		{"普通池 + 转人审 → 收回审核池", model.StateNormal, model.PoolNormal, review, nil, false, model.StatePending, model.PoolReview, -1},
		{"普通池 + 驳回 → 收回屏蔽池", model.StateNormal, model.PoolNormal, reject, nil, false, model.StateRejected, model.PoolBlock, -1},

		{"折叠 + 通过 → 普通池", model.StateFolded, model.PoolBlock, pass, nil, false, model.StateNormal, model.PoolNormal, 1},
		{"折叠 + 转人审 → 审核池", model.StateFolded, model.PoolBlock, review, nil, false, model.StatePending, model.PoolReview, 0},
		{"折叠 + 驳回 → 屏蔽池", model.StateFolded, model.PoolBlock, reject, nil, false, model.StateRejected, model.PoolBlock, 0},

		{"驳回 + 通过（复核放行）→ 普通池", model.StateRejected, model.PoolBlock, pass, nil, false, model.StateNormal, model.PoolNormal, 1},
		{"驳回 + 转人审 → 审核池", model.StateRejected, model.PoolBlock, review, nil, false, model.StatePending, model.PoolReview, 0},
		{"驳回 + 驳回 → 短路", model.StateRejected, model.PoolBlock, reject, nil, true, model.StateRejected, model.PoolBlock, 0},

		{"已删除 + 通过 → 非法（不得复活被删内容）", model.StateDeleted, model.PoolBlock, pass, model.ErrInvalidStateTransition, false, model.StateDeleted, model.PoolBlock, 0},
		{"已删除 + 转人审 → 非法", model.StateDeleted, model.PoolBlock, review, model.ErrInvalidStateTransition, false, model.StateDeleted, model.PoolBlock, 0},
		{"已删除 + 驳回 → 非法（终态）", model.StateDeleted, model.PoolBlock, reject, model.ErrInvalidStateTransition, false, model.StateDeleted, model.PoolBlock, 0},

		{"枚举外脏状态 9 + 通过 → 非法", 9, model.PoolNormal, pass, model.ErrInvalidStateTransition, false, 9, model.PoolNormal, 0},
		{"枚举外脏状态 -1 + 驳回 → 非法（不顺手洗数据）", -1, model.PoolBlock, reject, model.ErrInvalidStateTransition, false, -1, model.PoolBlock, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := modSeed(t, e, tc.from, tc.fromPool)
			seedSegment(t, e.st, modOid, modSeg, 7)
			e.st.cache.warmSegmentCount(modOid, modSeg, 7)

			reply, err := applyModeration(t, e, modPlainReq(seeded.Dmid, tc.verdict, modMachineOp))

			switch {
			case tc.wantErr != nil:
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：非法迁移仍返回 %+v", tc.name, reply)
				}
			case tc.shortCkt:
				wantNoErr(t, tc.name, err)
				wantEQ(t, tc.name, "applied", reply.GetApplied(), false)
				wantEQ(t, tc.name, "message", reply.GetMessage(), "state already applied")
				wantEQ(t, tc.name, "state 回显在库值", reply.GetState(), tc.from)
				wantEQ(t, tc.name, "pool 回显在库值", reply.GetPool(), tc.fromPool)
			default:
				wantNoErr(t, tc.name, err)
				wantEQ(t, tc.name, "state", reply.GetState(), tc.wantState)
				wantEQ(t, tc.name, "pool", reply.GetPool(), tc.wantPool)
			}

			row := modRow(t, e, seeded.Dmid)
			wantEQ(t, tc.name, "主表 state", row.State, tc.wantState)
			wantEQ(t, tc.name, "主表 pool", row.Pool, tc.wantPool)
			wantEQ(t, tc.name, "段表计数", e.st.segment.countAt(modOid, modSeg), 7+tc.delta)

			// 副作用是否发生与结论是否被接受严格一致
			wantEQ(t, tc.name, "留痕行数", len(e.st.opLog.rows), boolToInt(err == nil && !tc.shortCkt))
			wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, boolToInt(err == nil && !tc.shortCkt))
			if tc.shortCkt || tc.wantErr != nil {
				// 短路/非法：只读了一次主表，没进事务，也没动任何缓存
				wantOps(t, tc.name, e.ops(), []string{findOneOp(seeded.Dmid)})
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				wantCount(t, tc.name, e.st.log, "segment.", 0)
				return
			}
			tail := modDelOnlyOps()
			switch {
			case tc.delta > 0:
				tail = modGainOps()
			case tc.delta < 0:
				tail = modLossOps()
			}
			wantOps(t, tc.name, e.ops(), modApplyOps(seeded.Dmid, tc.from, tc.wantState, tc.wantPool, "", 0, tail))
		})
	}
}

// TestApplyModerationResultAuditTrailOrderDependsOnEventID 留痕与 CAS 的先后由 event_id 决定，
// 而这个先后是有后果的：带事件的投递把 uniq_event 占位写在状态推进之前，
// 重复投递连主表都不用碰就被唯一索引挡下；不带事件时 CAS 先跑，留痕失败才回滚状态。
// 两种形态都在「留痕写失败」下对照：期望序列里 CAS 一步的有无就是判别证据。
func TestApplyModerationResultAuditTrailOrderDependsOnEventID(t *testing.T) {
	t.Run("带 event_id：留痕先于 CAS", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		e.st.opLog.failWith("InsertTx", errDB)

		reply, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-order"))
		wantErrIs(t, "带事件留痕失败", err, errDB)
		if reply != nil {
			t.Errorf("带事件留痕失败仍返回 %+v", reply)
		}
		// CAS 根本没被执行——这是「占位在先」的直接证据
		wantOps(t, "带事件留痕失败", e.ops(), []string{
			findOneOp(seeded.Dmid), existsEventOp("evt-order"), opTxBegin,
			oplogTxOp(seeded.Dmid, model.ActionModeration, "evt-order"), opTxRollback,
		})
		wantEQ(t, "带事件留痕失败", "状态未推进", modRow(t, e, seeded.Dmid).State, model.StatePending)
	})

	t.Run("不带 event_id：CAS 先于留痕，失败一起回滚", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		e.st.opLog.failWith("InsertTx", errDB)

		_, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, modMachineOp))
		wantErrIs(t, "无事件留痕失败", err, errDB)
		wantOps(t, "无事件留痕失败", e.ops(), []string{
			findOneOp(seeded.Dmid), opTxBegin,
			modCasOp(seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal),
			oplogTxOp(seeded.Dmid, model.ActionModeration, ""), opTxRollback,
		})
		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "无事件留痕失败", "状态被回滚", row.State, model.StatePending)
		wantEQ(t, "无事件留痕失败", "池被回滚", row.Pool, model.PoolReview)
	})

	t.Run("两种形态的成功序列只差留痕位置与去重快查", func(t *testing.T) {
		withEvent := newEnv(t)
		a := modSeed(t, withEvent, model.StatePending, model.PoolReview)
		_, err := applyModeration(t, withEvent, modReq(a.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-x"))
		wantNoErr(t, "带事件成功", err)

		noEvent := newEnv(t)
		b := modSeed(t, noEvent, model.StatePending, model.PoolReview)
		_, err = applyModeration(t, noEvent, modPlainReq(b.Dmid, rpc.ModerationVerdict_VERDICT_PASS, modMachineOp))
		wantNoErr(t, "无事件成功", err)

		wantOps(t, "带事件成功", withEvent.ops(),
			modApplyOps(a.Dmid, model.StatePending, model.StateNormal, model.PoolNormal, "evt-x", modTask, modGainOps()))
		// modPlainReq 不带 task_id：期望序列里连那条 moderation_task_id 的 UPDATE 都不该有
		wantOps(t, "无事件成功", noEvent.ops(),
			modApplyOps(b.Dmid, model.StatePending, model.StateNormal, model.PoolNormal, "", 0, modGainOps()))
		wantCount(t, "无事件成功", noEvent.st.log, "danmaku.SetTask", 0)
		// 无事件形态连去重快查都不发：不存在「同一结论重复下发」的第二次防护，只靠状态短路
		wantCount(t, "无事件成功", noEvent.st.log, "oplog.ExistsEvent", 0)
	})
}

// --- 幂等 ---

// TestApplyModerationResultDuplicateEventDeliveryIsNoOp 事件重放的四种判定：
// 去重快查优先于状态短路、也优先于非法迁移判定（否则重放会永久失败），但排在「弹幕存在性」之后。
// 重放必须零写入：不推状态、不留痕、不动段计数、不失效缓存。
func TestApplyModerationResultDuplicateEventDeliveryIsNoOp(t *testing.T) {
	t.Run("重复投递优先于合法迁移", func(t *testing.T) {
		e := newEnv(t)
		// 在库状态是被另一次结论驳回（4/4），而重放的这条事件是「通过」——
		// 若按结论投影就会把它放回普通池，去重必须把它挡下。
		seeded := modSeed(t, e, model.StateRejected, model.PoolBlock)
		seedOpLog(t, e.st, &model.OpLog{
			Dmid: seeded.Dmid, Action: model.ActionModeration, FromState: model.StatePending,
			ToState: model.StateRejected, OperatorRole: model.RoleSystem,
			EventID: "evt-replay", TraceID: "trace-old", Ctime: 1_700_000_000,
		})
		seedSegment(t, e.st, modOid, modSeg, 7)
		e.st.cache.warmSegmentCount(modOid, modSeg, 7)

		reply, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-replay"))
		wantNoErr(t, "重复投递", err)
		if reply == nil {
			t.Fatal("重复投递：回复 = nil, want 幂等成功")
		}
		// 应答回显的是库里的真实状态，而不是本次结论的目标值
		wantEQ(t, "重复投递", "dmid", reply.GetDmid(), seeded.Dmid)
		wantEQ(t, "重复投递", "state", reply.GetState(), model.StateRejected)
		wantEQ(t, "重复投递", "pool", reply.GetPool(), model.PoolBlock)
		wantEQ(t, "重复投递", "applied", reply.GetApplied(), false)
		wantEQ(t, "重复投递", "message", reply.GetMessage(), "duplicate event")

		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "重复投递", "状态未被重放改回可见", row.State, model.StateRejected)
		wantEQ(t, "重复投递", "池未被重放改回普通池", row.Pool, model.PoolBlock)
		wantEQ(t, "重复投递", "留痕没有第二条", len(e.st.opLog.rows), 1)
		wantOps(t, "重复投递", e.ops(), []string{findOneOp(seeded.Dmid), existsEventOp("evt-replay")})
		wantEQ(t, "重复投递", "事务次数", e.st.conn.transactions, 0)
		wantCount(t, "重复投递", e.st.log, "cache.", 0)
		wantCount(t, "重复投递", e.st.log, "segment.", 0)
		wantEQ(t, "重复投递", "段表计数未二次累加", e.st.segment.countAt(modOid, modSeg), int32(7))
	})

	t.Run("重复投递优先于非法迁移（重放不会永久失败）", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StateDeleted, model.PoolBlock)
		seedOpLog(t, e.st, &model.OpLog{
			Dmid: seeded.Dmid, Action: model.ActionModeration, EventID: "evt-deleted",
			OperatorRole: model.RoleSystem, Ctime: 1_700_000_000,
		})

		reply, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-deleted"))
		wantNoErr(t, "已删除 + 重复投递", err)
		wantEQ(t, "已删除 + 重复投递", "message", reply.GetMessage(), "duplicate event")
		wantEQ(t, "已删除 + 重复投递", "state", reply.GetState(), model.StateDeleted)
		wantOps(t, "已删除 + 重复投递", e.ops(), []string{findOneOp(seeded.Dmid), existsEventOp("evt-deleted")})
	})

	t.Run("弹幕不存在优先于去重", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		seedOpLog(t, e.st, &model.OpLog{
			Dmid: seeded.Dmid, Action: model.ActionModeration, EventID: "evt-gone",
			OperatorRole: model.RoleSystem, Ctime: 1_700_000_000,
		})

		reply, err := applyModeration(t, e, modReq(seeded.Dmid+50, rpc.ModerationVerdict_VERDICT_PASS, "evt-gone"))
		wantErrIs(t, "不存在优先于去重", err, model.ErrDanmakuNotFound)
		if reply != nil {
			t.Errorf("不存在优先于去重仍返回 %+v", reply)
		}
		// 快查排在读主表之后：行都定位不到就不必再问事件
		wantOps(t, "不存在优先于去重", e.ops(), []string{findOneOp(seeded.Dmid + 50)})
	})

	t.Run("同一结论不带 event_id 重复下发只靠状态短路", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		first := modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "")
		first.TaskId = 0

		_, err := applyModeration(t, e, first)
		wantNoErr(t, "首次无事件回写", err)
		second := modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-new")
		second.TaskId = 0

		before := e.st.log.snapshot()
		reply, err := applyModeration(t, e, second)
		wantNoErr(t, "二次无事件回写", err)
		wantEQ(t, "二次无事件回写", "message", reply.GetMessage(), "state already applied")
		wantEQ(t, "二次无事件回写", "applied", reply.GetApplied(), false)
		// 第二次带了一个全新 event_id：去重快查必然 miss，挡下二次写入的只剩状态短路
		wantOps(t, "二次无事件回写", e.st.log.opsFrom(before),
			[]string{findOneOp(seeded.Dmid), existsEventOp("evt-new")})
		wantEQ(t, "二次无事件回写", "留痕总行数", len(e.st.opLog.rows), 1)
		wantEQ(t, "二次无事件回写", "段表计数只加过一次", e.st.segment.countAt(modOid, modSeg), int32(1))
	})
}

// TestApplyModerationResultUniqueIndexFallbackIsUnreachableForEventConflict 钉住现状缺陷
// （README 缺口 6）：repository 的注释写着「uniq_event 唯一索引兜底并发重复投递，
// 重复投递会按已处理返回」（repository.go:287、316-319），但翻译条件里的 updatedByEvent
// 是在 InsertTx **成功之后**才置真的（repository.go:288-291），而冲突恰恰发生在 InsertTx 本身，
// 于是 :317 那条分支永远走不到：MySQL 1062 原始报文从 applymoderationresultlogic.go:95 一路
// 上抛给事件消费者，连「目标状态是否已被对方达成」的那次重读都不做（调用序列里没有第二个
// danmaku.FindOne 就是证据）。即便对方已把状态推到本次目标、我方事务回滚后库里其实已经达成了，
// 消费者拿到的仍是错误 ⇒ 只能无限重试同一条结论。
// 判别对照：同样的竞态窗口换成「对方用另一个 event_id 抢先推进」⇒ CAS 未命中 ⇒
// 走到 :97-114 的重读并友好返回。把 updatedByEvent 改成「本事务尝试过事件占位」后，
// 第一条子用例的 wantErrIs(1062) 必须变红。
func TestApplyModerationResultUniqueIndexFallbackIsUnreachableForEventConflict(t *testing.T) {
	t.Run("去重快查打盲 + 撞 uniq_event ⇒ 原始 1062 上抛，幂等判定没机会跑", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		// 对方那次消费者已经把同一条 event_id 的留痕提交了
		seedOpLog(t, e.st, &model.OpLog{
			Dmid: seeded.Dmid, Action: model.ActionModeration, FromState: model.StatePending,
			ToState: model.StateNormal, OperatorRole: model.RoleSystem,
			EventID: "evt-race", TraceID: "trace-other", Ctime: 1_700_000_000,
		})
		seedSegment(t, e.st, modOid, modSeg, 7)
		e.st.cache.warmSegmentCount(modOid, modSeg, 7)
		// 快查读到旧副本（恒 miss），真实写入时才撞唯一索引
		e.st.opLog.dedupeBlind = true
		// 我们的事务失败的同一瞬间，对方的状态写入提交了
		e.st.conn.onRollback = func() {
			e.st.danmaku.advance(seeded.Dmid, model.StateNormal, model.PoolNormal)
		}

		reply, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-race"))
		if err == nil {
			t.Fatalf("撞唯一索引却被当成幂等成功返回了 %+v：现状应上抛原始 1062", reply)
		}
		if errors.Is(err, model.ErrConcurrentUpdate) {
			t.Errorf("错误被翻译成了 %v，与现状（原始报文上抛）不符", err)
		}
		wantErrContains(t, "撞唯一索引（现状：原始报文上抛）", err, "Error 1062")
		wantErrContains(t, "撞唯一索引（现状：原始报文上抛）", err, "Duplicate entry 'evt-race'")
		if reply != nil {
			t.Errorf("撞唯一索引仍返回 %+v", reply)
		}
		// 尾部没有第二次 danmaku.FindOne：幂等判定连读都没读
		wantOps(t, "撞唯一索引", e.ops(), []string{
			findOneOp(seeded.Dmid), existsEventOp("evt-race"), opTxBegin,
			oplogTxOp(seeded.Dmid, model.ActionModeration, "evt-race"), opTxRollback,
		})
		wantEQ(t, "撞唯一索引", "我方没有提交", e.st.conn.committed, 0)
		wantEQ(t, "撞唯一索引", "我方事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "撞唯一索引", "留痕只有对方那一条", len(e.st.opLog.rows), 1)
		wantCount(t, "撞唯一索引", e.st.log, "cache.", 0)
		wantCount(t, "撞唯一索引", e.st.log, "segment.", 0)
		// 最刺眼的一条：库里其实已经达成本次目标，应答却是错误
		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "撞唯一索引", "状态已被对方推到本次目标", row.State, model.StateNormal)
		wantEQ(t, "撞唯一索引", "池已被对方推到普通池", row.Pool, model.PoolNormal)
	})

	t.Run("对照：对方用另一个 event_id 抢先推进 ⇒ CAS 未命中走友好幂等路径", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		seedOpLog(t, e.st, &model.OpLog{
			Dmid: seeded.Dmid, Action: model.ActionModeration, FromState: model.StatePending,
			ToState: model.StateNormal, OperatorRole: model.RoleSystem,
			EventID: "evt-other", TraceID: "trace-other", Ctime: 1_700_000_000,
		})
		// 对方的 UPDATE 落在我方 CAS 之前：事件占位不冲突，冲突的是状态本身
		e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateNormal)

		reply, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-mine"))
		wantNoErr(t, "另一个事件的并发抢先", err)
		wantEQ(t, "另一个事件的并发抢先", "message", reply.GetMessage(), "applied by concurrent consumer")
		wantOps(t, "另一个事件的并发抢先", e.ops(), []string{
			findOneOp(seeded.Dmid), existsEventOp("evt-mine"), opTxBegin,
			oplogTxOp(seeded.Dmid, model.ActionModeration, "evt-mine"),
			modCasOp(seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal),
			opTxRollback, findOneOp(seeded.Dmid),
		})
	})
}

// TestApplyModerationResultIdempotenceIgnoresPoolMismatch 钉住现状缺陷（README 缺口 5）：
// 幂等短路只比 state、不比 pool（applymoderationresultlogic.go:69），
// 所以「状态正常但留在审核池」这类 state/pool 错配行永远无法靠回写结论修复，
// 应答还会把错配的 pool 原样回显，让调用方以为「已达成」。
// 对照行是同 state 的其它池值：收严（短路需 state 与 pool 同时匹配）后本用例必须变红。
func TestApplyModerationResultIdempotenceIgnoresPoolMismatch(t *testing.T) {
	cases := []struct {
		name          string
		from          int32
		fromPool      int32
		verdict       rpc.ModerationVerdict
		wantShortCkt  bool
		wantRowState  int32
		wantRowPool   int32
		wantReplyPool int32
	}{
		{
			"状态正常却留在审核池 + 通过 ⇒ 短路，池不修复（弹幕永远不可见）",
			model.StateNormal, model.PoolReview, rpc.ModerationVerdict_VERDICT_PASS, true,
			model.StateNormal, model.PoolReview, model.PoolReview,
		},
		{
			"状态正常且已在普通池 + 通过 ⇒ 短路（正确）",
			model.StateNormal, model.PoolNormal, rpc.ModerationVerdict_VERDICT_PASS, true,
			model.StateNormal, model.PoolNormal, model.PoolNormal,
		},
		{
			"待审态却错进普通池 + 转人审 ⇒ 短路，池不修复",
			model.StatePending, model.PoolNormal, rpc.ModerationVerdict_VERDICT_REVIEW, true,
			model.StatePending, model.PoolNormal, model.PoolNormal,
		},
		{
			"对照：驳回态 + 通过 ⇒ 状态与池一起被修正",
			model.StateRejected, model.PoolBlock, rpc.ModerationVerdict_VERDICT_PASS, false,
			model.StateNormal, model.PoolNormal, model.PoolNormal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := modSeed(t, e, tc.from, tc.fromPool)
			e.st.cache.warmSegment(modOid, modSeg, seeded)

			reply, err := applyModeration(t, e, modPlainReq(seeded.Dmid, tc.verdict, modMachineOp))
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "应答 pool", reply.GetPool(), tc.wantReplyPool)

			row := modRow(t, e, seeded.Dmid)
			wantEQ(t, tc.name, "主表 pool", row.Pool, tc.wantRowPool)
			wantEQ(t, tc.name, "主表 state", row.State, tc.wantRowState)

			if tc.wantShortCkt {
				wantEQ(t, tc.name, "message", reply.GetMessage(), "state already applied")
				// 短路时连段列表缓存都不失效——旧下发包可以继续被读走
				wantOps(t, tc.name, e.ops(), []string{findOneOp(seeded.Dmid)})
				_, cached := e.st.cache.segmentRows(modOid, modSeg)
				wantEQ(t, tc.name, "段列表缓存原样保留", cached, true)
				wantEQ(t, tc.name, "留痕行数", len(e.st.opLog.rows), 0)
				return
			}
			wantEQ(t, tc.name, "message", reply.GetMessage(), "")
			_, cached := e.st.cache.segmentRows(modOid, modSeg)
			wantEQ(t, tc.name, "真迁移会失效段列表缓存", cached, false)
		})
	}
}

// TestApplyModerationResultSuccessReplyDoesNotSetApplied 钉住现状缺陷（README 缺口 4）：
// 真正生效的迁移返回 applied=false、message=""，与三条幂等分支唯一的区别只剩 message 文案，
// 而 danmaku.proto:263 写着「applied=false 表示重复投递或无需迁移」。
// 判别对照：同一个 env 里先做一次真生效回写、再做一次重复投递，
// 两条应答的 applied 完全相同（false），但库里明显只有第一次真的动了状态与段计数。
// 修法落地（成功迁移应回 applied=true）后本用例的第一条断言必须变红。
func TestApplyModerationResultSuccessReplyDoesNotSetApplied(t *testing.T) {
	e := newEnv(t)
	seeded := modSeed(t, e, model.StatePending, model.PoolReview)
	seedSegment(t, e.st, modOid, modSeg, 7)
	e.st.cache.warmSegmentCount(modOid, modSeg, 7)

	first, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-first"))
	wantNoErr(t, "真生效的回写", err)

	// 第一次确实生效了：状态被推进、留痕落了、段计数 +1
	row := modRow(t, e, seeded.Dmid)
	wantEQ(t, "真生效的回写", "主表 state", row.State, model.StateNormal)
	wantEQ(t, "真生效的回写", "主表 pool", row.Pool, model.PoolNormal)
	wantEQ(t, "真生效的回写", "留痕行数", len(e.st.opLog.rows), 1)
	wantEQ(t, "真生效的回写", "段表计数", e.st.segment.countAt(modOid, modSeg), int32(8))
	// 现状：应答却说「没有应用」
	wantEQ(t, "真生效的回写（现状哨兵）", "applied", first.GetApplied(), false)
	wantEQ(t, "真生效的回写（现状哨兵）", "message", first.GetMessage(), "")

	// 对照：同一条弹幕换个 event_id 再来一次，这次是幂等短路
	second, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-second"))
	wantNoErr(t, "重复结论的短路", err)
	wantEQ(t, "重复结论的短路", "applied", second.GetApplied(), false)
	wantEQ(t, "重复结论的短路", "message", second.GetMessage(), "state already applied")
	wantEQ(t, "重复结论的短路", "段表计数没被二次推进", e.st.segment.countAt(modOid, modSeg), int32(8))

	if first.GetApplied() == second.GetApplied() && first.GetMessage() == second.GetMessage() {
		t.Errorf("应答字段完全相同的两条回写让用例失去判别性：真生效与幂等短路无法区分")
	}
}

// --- 审计主体与身份边界 ---

// TestApplyModerationResultRecordsOperatorRoleFromRequestFieldOnly 钉住现状（README 缺口 8）：
// 操作者身份完全来自请求体——operator=0 记系统、operator>0 一律记 RoleAdmin、
// 负数照样原样进 operator_mid；本方法不核对调用方是谁，也不看 ctx 里的登录信息。
// 因此「谁能让待审弹幕变可见」在服务侧没有任何约束。
func TestApplyModerationResultRecordsOperatorRoleFromRequestFieldOnly(t *testing.T) {
	cases := []struct {
		name      string
		operator  int64
		wantRole  int32
		wantOpMid int64
	}{
		{"机审/事件消费（operator 留空）", modMachineOp, model.RoleSystem, int64(0)},
		{"人审处理人 ⇒ 一律记成运营", modHumanOp, model.RoleAdmin, modHumanOp},
		{"operator 为负 ⇒ 仍记系统且负值原样入审计列", -3, model.RoleSystem, int64(-3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := modSeed(t, e, model.StatePending, model.PoolReview)

			_, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, tc.operator))
			wantNoErr(t, tc.name, err)

			if len(e.st.opLog.rows) != 1 {
				t.Fatalf("%s：op_log 行数 = %d, want 1", tc.name, len(e.st.opLog.rows))
			}
			lg := *e.st.opLog.rows[0]
			wantEQ(t, tc.name, "operator_role", lg.OperatorRole, tc.wantRole)
			wantEQ(t, tc.name, "operator_mid", lg.OperatorMid, tc.wantOpMid)
			wantEQ(t, tc.name, "action", lg.Action, model.ActionModeration)
			// 弹幕作者列不因操作者而改变
			row := modRow(t, e, seeded.Dmid)
			wantEQ(t, tc.name, "作者 mid", row.Mid, modAuthor)
			// 没有任何身份门禁的可见后果：匿名（operator=0）一条 PASS 就把待审弹幕放进普通池
			wantEQ(t, tc.name, "状态已成可见", row.State, model.StateNormal)
			wantEQ(t, tc.name, "池已成普通池", row.Pool, model.PoolNormal)
			wantEQ(t, tc.name, "段计数已 +1", e.st.segment.countAt(modOid, modSeg), int32(1))
			// 全程只有数据依赖，没有鉴权/风控/事件下游调用
			wantCount(t, tc.name, e.st.log, "moderation.", 0)
			wantCount(t, tc.name, e.st.log, "limiter.", 0)
		})
	}
}

// --- 机审任务号回填 ---

// TestApplyModerationResultBackfillsModerationTaskIDOnlyOnce task_id 的回填沿用
// 「只填首个任务号」的 SQL 语义（model/danmaku.go 的 WHERE moderation_task_id=0）：
// 第二次结论仍会执行那条 UPDATE（序列里看得到），但库里的值不换。
// 边界对照：请求不带 task_id 时那条 UPDATE 一次都不该发。
func TestApplyModerationResultBackfillsModerationTaskIDOnlyOnce(t *testing.T) {
	t.Run("首次回填 + 后续结论不改写", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)

		_, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-t1"))
		wantNoErr(t, "首次回写", err)
		wantEQ(t, "首次回写", "task_id 被填上", modRow(t, e, seeded.Dmid).ModerationTaskId, modTask)
		wantCount(t, "首次回写", e.st.log, "danmaku.SetTaskTx", 1)
		wantCount(t, "首次回写", e.st.log, "danmaku.SetTask:", 0) // 只走事务内的版本

		before := e.st.log.snapshot()
		second := modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_REJECT, "evt-t2")
		second.TaskId = 8888
		_, err = applyModeration(t, e, second) // 普通池 → 驳回，合法迁移
		wantNoErr(t, "第二次结论", err)
		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "第二次结论", "state 被推进", row.State, model.StateRejected)
		wantEQ(t, "第二次结论", "task_id 保持首个", row.ModerationTaskId, modTask)
		wantOps(t, "第二次结论", e.st.log.opsFrom(before), []string{
			findOneOp(seeded.Dmid), existsEventOp("evt-t2"), opTxBegin,
			oplogTxOp(seeded.Dmid, model.ActionModeration, "evt-t2"),
			modCasOp(seeded.Dmid, model.StateNormal, model.StateRejected, model.PoolBlock),
			setTaskTxOp(seeded.Dmid, 8888), opTxCommit,
			segIncrOp(modOid, modSeg, -1), cntIncrOp(modOid, modSeg, -1), segDelOp(modOid, modSeg),
		})
	})

	t.Run("请求不带 task_id ⇒ 不发那条 UPDATE", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StateFolded, model.PoolBlock)
		in := modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_REJECT, "evt-t3")
		in.TaskId = 0

		_, err := applyModeration(t, e, in)
		wantNoErr(t, "不带 task_id", err)
		wantCount(t, "不带 task_id", e.st.log, "danmaku.SetTask", 0)
		wantEQ(t, "不带 task_id", "task_id 仍为 0", modRow(t, e, seeded.Dmid).ModerationTaskId, int64(0))
	})

	t.Run("已有 task_id 且请求带新值 ⇒ 值不换", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeedTask(t, e, model.StateRejected, model.PoolBlock, 777)
		in := modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-t4")

		_, err := applyModeration(t, e, in)
		wantNoErr(t, "复核放行", err)
		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "复核放行", "task_id 仍是旧值", row.ModerationTaskId, int64(777))
		wantEQ(t, "复核放行", "state 已推进", row.State, model.StateNormal)
	})
}

// --- 并发覆盖 ---

// TestApplyModerationResultCasMissReReadDecidesOutcome CAS 影响 0 行之后必须重读定论
// （applymoderationresultlogic.go:97-114）：目标达成 ⇒ 幂等成功；未达成 ⇒ 可重试的并发冲突；
// 重读自己报错 ⇒ 上抛原始错误；重读已无行 ⇒ 404。
// 后两种是现状哨兵（README 缺口 6、7）：重读失败时下游原始错误顶掉了 ErrConcurrentUpdate，
// 调用方没法用哨兵判定「这条结论还没落地、必须重试」。
func TestApplyModerationResultCasMissReReadDecidesOutcome(t *testing.T) {
	t.Run("对方推到本次目标 ⇒ 幂等成功且保留对方的值", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StateFolded, model.PoolBlock)
		e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateRejected)

		reply, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_REJECT, modHumanOp))
		wantNoErr(t, "CAS 未命中且目标已达成", err)
		wantEQ(t, "CAS 未命中且目标已达成", "applied", reply.GetApplied(), false)
		wantEQ(t, "CAS 未命中且目标已达成", "message", reply.GetMessage(), "applied by concurrent consumer")
		// 应答回显重读到的库值，而不是本次请求映射出来的目标值
		wantEQ(t, "CAS 未命中且目标已达成", "state", reply.GetState(), model.StateRejected)
		wantEQ(t, "CAS 未命中且目标已达成", "pool", reply.GetPool(), model.PoolBlock)
		wantOps(t, "CAS 未命中且目标已达成", e.ops(), []string{
			findOneOp(seeded.Dmid), opTxBegin,
			modCasOp(seeded.Dmid, model.StateFolded, model.StateRejected, model.PoolBlock),
			opTxRollback, findOneOp(seeded.Dmid),
		})
		wantEQ(t, "CAS 未命中且目标已达成", "留痕没被写进去", len(e.st.opLog.rows), 0)
		wantEQ(t, "CAS 未命中且目标已达成", "事务回滚", e.st.conn.rolledBack, 1)
		wantCount(t, "CAS 未命中且目标已达成", e.st.log, "cache.", 0)
	})

	t.Run("对方推到了别的状态 ⇒ ErrConcurrentUpdate 且不覆盖对方", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateFolded)

		reply, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, modMachineOp))
		wantErrIs(t, "CAS 未命中且目标未达成", err, model.ErrConcurrentUpdate)
		if reply != nil {
			t.Errorf("CAS 未命中且目标未达成仍返回 %+v", reply)
		}
		row := modRow(t, e, seeded.Dmid)
		wantEQ(t, "CAS 未命中且目标未达成", "对方结论保留", row.State, model.StateFolded)
		wantEQ(t, "CAS 未命中且目标未达成", "池没被我们覆盖", row.Pool, model.PoolReview)
		wantOps(t, "CAS 未命中且目标未达成", e.ops(), []string{
			findOneOp(seeded.Dmid), opTxBegin,
			modCasOp(seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal),
			opTxRollback, findOneOp(seeded.Dmid),
		})
		wantEQ(t, "CAS 未命中且目标未达成", "留痕行数", len(e.st.opLog.rows), 0)
		wantCount(t, "CAS 未命中且目标未达成", e.st.log, "segment.", 0)
	})

	t.Run("重读自己报错 ⇒ 原始错误顶掉可重试哨兵（现状）", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateFolded)
		// 第一次 FindOne 已经发生，所以在回滚时才武装故障，让重读那一次失败
		e.st.conn.onRollback = func() { e.st.danmaku.failWith("FindOne", errDB) }

		_, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, modMachineOp))
		wantErrIs(t, "重读失败（现状）", err, errDB)
		if errors.Is(err, model.ErrConcurrentUpdate) {
			t.Errorf("重读失败（现状）：错误里竟还带着 ErrConcurrentUpdate：%v", err)
		}
		wantOps(t, "重读失败（现状）", e.ops(), []string{
			findOneOp(seeded.Dmid), opTxBegin,
			modCasOp(seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal),
			opTxRollback, findOneOp(seeded.Dmid),
		})
	})

	t.Run("重读时无行 ⇒ 404（现状：与「弹幕本来就不存在」同一个错误）", func(t *testing.T) {
		e := newEnv(t)
		seeded := modSeed(t, e, model.StatePending, model.PoolReview)
		e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateFolded)
		e.st.conn.onRollback = func() {
			// 模拟合规清理任务在两个事务之间把那行物理删掉
			delete(e.st.danmaku.rows, seeded.Dmid)
		}

		_, err := applyModeration(t, e, modPlainReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, modMachineOp))
		wantErrIs(t, "重读无行", err, model.ErrDanmakuNotFound)
		wantOps(t, "重读无行", e.ops(), []string{
			findOneOp(seeded.Dmid), opTxBegin,
			modCasOp(seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal),
			opTxRollback, findOneOp(seeded.Dmid),
		})
		wantEQ(t, "重读无行", "留痕行数", len(e.st.opLog.rows), 0)
	})
}

// --- 下游故障 ---

// TestApplyModerationResultPropagatesDownstreamFailures 三类故障口径分得很清：
//   - 事务/DB 侧：原始错误上抛 + 整体回滚 + 缓存与派生一步都不做；
//   - 去重快查失败：上抛（宁可让 MQ 重试，也不冒重复消费的风险）；
//   - 段表/段计数/段列表失效：就地吞掉只写日志，回写本身照常成功——
//     代价是 DB 与 Redis 计数漂移、被驳回的弹幕在段缓存 TTL（缺省 15s）内继续被下发。
func TestApplyModerationResultPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name      string
		arm       func(e *env, dmid int64)
		verdict   rpc.ModerationVerdict
		from      int32
		fromPool  int32
		wantErr   error // nil ⇒ 可降级，回写照常成功
		wantState int32
		wantPool  int32
		// wantIncrs 是段派生（段表 + 缓存计数）被调用的次数，用于钉「失败也被调用过」
		wantIncrs int
		// audit 是期望的留痕行数
		audit int
		// 吞错的真实代价逐列钉死：段表计数、缓存计数（-1 = 键不存在）、段列表缓存是否还留着脏行。
		// 种子值两边都是 7，所以任何一侧没跟上都会立刻显形。
		wantSegCount   int32
		wantCacheCnt   int32
		wantSegListHit bool
	}{
		{
			name:    "主表读失败",
			arm:     func(e *env, _ int64) { e.st.danmaku.failWith("FindOne", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: errDB, wantState: model.StatePending, wantPool: model.PoolReview, wantIncrs: 0, audit: 0,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
		{
			name:    "去重快查失败",
			arm:     func(e *env, _ int64) { e.st.opLog.failWith("ExistsEvent", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: errDB, wantState: model.StatePending, wantPool: model.PoolReview, wantIncrs: 0, audit: 0,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
		{
			name:    "留痕写失败",
			arm:     func(e *env, _ int64) { e.st.opLog.failWith("InsertTx", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: errDB, wantState: model.StatePending, wantPool: model.PoolReview, wantIncrs: 0, audit: 0,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
		{
			name:    "状态 CAS 失败",
			arm:     func(e *env, _ int64) { e.st.danmaku.failWith("TransitionStateTx", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: errDB, wantState: model.StatePending, wantPool: model.PoolReview, wantIncrs: 0, audit: 0,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
		{
			name:    "task_id 回填失败 ⇒ 状态与留痕一起回滚",
			arm:     func(e *env, _ int64) { e.st.danmaku.failWith("SetModerationTaskIDTx", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: errDB, wantState: model.StatePending, wantPool: model.PoolReview, wantIncrs: 0, audit: 0,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
		{
			// 段表这一侧没跟上：主表已可见、缓存已到 8 ⇒ 两边从此差 1，靠 cron 对账收敛
			name:    "段表派生失败只记日志",
			arm:     func(e *env, _ int64) { e.st.segment.failWith("Incr", errDB) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: nil, wantState: model.StateNormal, wantPool: model.PoolNormal, wantIncrs: 2, audit: 1,
			wantSegCount: 7, wantCacheCnt: 8, wantSegListHit: false,
		},
		{
			// 缓存这一侧没跟上：段表已到 8、缓存停在 7 ⇒ 读侧走缓存时少报一条
			name:    "段计数缓存失败只记日志",
			arm:     func(e *env, _ int64) { e.st.cache.failWith("IncrSegmentCount", errCache) },
			verdict: rpc.ModerationVerdict_VERDICT_PASS,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: nil, wantState: model.StateNormal, wantPool: model.PoolNormal, wantIncrs: 2, audit: 1,
			wantSegCount: 8, wantCacheCnt: 7, wantSegListHit: false,
		},
		{
			// 计数两边都到 6，但失效失败 ⇒ 已驳回的弹幕在段缓存里继续被下发
			name:    "可见→不可见的段列表失效失败被吞",
			arm:     func(e *env, _ int64) { e.st.cache.failWith("DelSegment", errCache) },
			verdict: rpc.ModerationVerdict_VERDICT_REJECT,
			from:    model.StateNormal, fromPool: model.PoolNormal,
			wantErr: nil, wantState: model.StateRejected, wantPool: model.PoolBlock, wantIncrs: 2, audit: 1,
			wantSegCount: 6, wantCacheCnt: 6, wantSegListHit: true,
		},
		{
			// 两侧都不可见时只该做失效；失效失败被吞 ⇒ 脏下发包留在缓存里，计数一动不动
			name:    "两侧都不可见时 default 分支的失效失败被吞",
			arm:     func(e *env, _ int64) { e.st.cache.failWith("DelSegment", errCache) },
			verdict: rpc.ModerationVerdict_VERDICT_REJECT,
			from:    model.StatePending, fromPool: model.PoolReview,
			wantErr: nil, wantState: model.StateRejected, wantPool: model.PoolBlock, wantIncrs: 0, audit: 1,
			wantSegCount: 7, wantCacheCnt: 7, wantSegListHit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := modSeed(t, e, tc.from, tc.fromPool)
			seedSegment(t, e.st, modOid, modSeg, 7)
			e.st.cache.warmSegmentCount(modOid, modSeg, 7)
			e.st.cache.warmSegment(modOid, modSeg, seeded) // 缓存里存着这一份下发包
			tc.arm(e, seeded.Dmid)

			reply, err := applyModeration(t, e, modReq(seeded.Dmid, tc.verdict, "evt-fail"))

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
				}
				// 事务侧失败：状态原样、留痕回滚、派生与缓存一步都不做
				wantEQ(t, tc.name, "主表 state", modRow(t, e, seeded.Dmid).State, tc.wantState)
				wantEQ(t, tc.name, "主表 pool", modRow(t, e, seeded.Dmid).Pool, tc.wantPool)
				wantEQ(t, tc.name, "留痕行数", len(e.st.opLog.rows), tc.audit)
				wantEQ(t, tc.name, "提交次数", e.st.conn.committed, 0)
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				wantCount(t, tc.name, e.st.log, "segment.", 0)
				wantEQ(t, tc.name, "段表计数未被改动", e.st.segment.countAt(modOid, modSeg), int32(7))
				wantEQ(t, tc.name, "段列表缓存未被失效", func() bool { _, ok := e.st.cache.segmentRows(modOid, modSeg); return ok }(), true)
				return
			}

			wantNoErr(t, tc.name, err)
			if reply == nil {
				t.Fatalf("%s：可降级故障把回写打成了失败", tc.name)
			}
			wantEQ(t, tc.name, "应答 state", reply.GetState(), tc.wantState)
			wantEQ(t, tc.name, "应答 pool", reply.GetPool(), tc.wantPool)
			row := modRow(t, e, seeded.Dmid)
			wantEQ(t, tc.name, "主表 state", row.State, tc.wantState)
			wantEQ(t, tc.name, "主表 pool", row.Pool, tc.wantPool)
			wantEQ(t, tc.name, "留痕行数", len(e.st.opLog.rows), tc.audit)
			wantEQ(t, tc.name, "提交次数", e.st.conn.committed, 1)
			// 派生与失效确实被调用过（只是失败后被放弃）
			wantEQ(t, tc.name, "段派生调用次数", countIn(e.ops(), "segment.Incr")+countIn(e.ops(), "cache.IncrCnt"), tc.wantIncrs)
			wantEQ(t, tc.name, "段列表失效被调用", countIn(e.ops(), "cache.DelSeg"), 1)
			wantEQ(t, tc.name, "段表计数", e.st.segment.countAt(modOid, modSeg), tc.wantSegCount)
			wantEQ(t, tc.name, "缓存段计数", cacheCount(e, modOid, modSeg), tc.wantCacheCnt)
			_, segCached := e.st.cache.segmentRows(modOid, modSeg)
			wantEQ(t, tc.name, "段列表缓存是否还留着行", segCached, tc.wantSegListHit)
		})
	}
}

// TestApplyModerationResultCacheCountDriftsWhenSegmentTableFails 把「段表派生失败」这一条单独拆开钉：
// 主表已经可见、缓存计数已经 +1，但 MySQL 派生表停在旧值 ⇒ 两者从此不一致，
// 只能靠 services/cron 对账修复（repository.go:514-522 的注释就是这么写的）。
func TestApplyModerationResultCacheCountDriftsWhenSegmentTableFails(t *testing.T) {
	e := newEnv(t)
	seeded := modSeed(t, e, model.StatePending, model.PoolReview)
	seedSegment(t, e.st, modOid, modSeg, 7)
	e.st.cache.warmSegmentCount(modOid, modSeg, 7)
	e.st.segment.failWith("Incr", errDB)

	_, err := applyModeration(t, e, modReq(seeded.Dmid, rpc.ModerationVerdict_VERDICT_PASS, "evt-drift"))
	wantNoErr(t, "段表派生失败", err)

	wantEQ(t, "段表派生失败", "段表计数停在 7", e.st.segment.countAt(modOid, modSeg), int32(7))
	wantEQ(t, "段表派生失败", "缓存计数已到 8", cacheCount(e, modOid, modSeg), int32(8))
	wantOps(t, "段表派生失败", e.ops(), modApplyOps(
		seeded.Dmid, model.StatePending, model.StateNormal, model.PoolNormal, "evt-drift", modTask, modGainOps()))
}
