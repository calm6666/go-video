package logic

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 DeleteDanmaku：归属校验 → 幂等判定 → 状态机合法性 → 主表 CAS + op_log 同事务
// → 提交后才调整段计数与段缓存。
//
// 三条被测不变量（AGENTS.md §5「弹幕 owner」、§8「删除保留审计证据」）：
//  1. 只能删自己的弹幕，管理员分支靠 in.Admin 显式打开，且留痕角色记成 RoleAdmin；
//  2. 删除是软删：行必须留在库里，content/oid/mid/ctime/幂等键一个都不许丢；
//  3. 状态推进与审计留痕在同一事务，事务失败时两边一起消失，缓存失效排在提交之后。

const (
	delOid     = int64(1001)
	delMid     = int64(7001) // 弹幕作者
	delAdmin   = int64(9002) // 管理员操作者 mid
	delSegNo   = int32(3)
	delAid     = int64(500)
	delIdemKey = "idem-del-1"
)

// --- 写侧公共期望序列（applymoderationresult_test.go 复用本块） ---

const (
	opTxBegin    = "tx.Begin"
	opTxCommit   = "tx.Commit"
	opTxRollback = "tx.Rollback"
)

func findOneOp(dmid int64) string { return fmt.Sprintf("danmaku.FindOne:%d", dmid) }

func transitionTxOp(dmid int64, from, to, toPool int32) string {
	return fmt.Sprintf("danmaku.TransitionTx:%d/%d->%d/%d", dmid, from, to, toPool)
}

func oplogTxOp(dmid int64, action, eventID string) string {
	return fmt.Sprintf("oplog.InsertTx:%d/%s/%s", dmid, action, eventLabel(eventID))
}

func setTaskTxOp(dmid, taskID int64) string {
	return fmt.Sprintf("danmaku.SetTaskTx:%d/%d", dmid, taskID)
}

// segIncrOp 是 MySQL 派生表的段计数增量（与 cntIncrOp 的缓存增量成对出现）。
func segIncrOp(oid int64, segNo, delta int32) string {
	return fmt.Sprintf("segment.Incr:%d/%d/%+d", oid, segNo, delta)
}

// --- 布景与调用 ---

func deleteDanmaku(t *testing.T, e *env, in *rpc.DeleteDanmakuReq) (*rpc.EmptyReply, error) {
	t.Helper()
	return NewDeleteDanmakuLogic(context.Background(), e.svcCtx).DeleteDanmaku(in)
}

// seedVisible 布一条指定状态/池的弹幕并返回落库行（dmid 由自增分配，首行恒为 101）。
func seedVisible(t *testing.T, e *env, state, pool int32) *model.Danmaku {
	t.Helper()
	return seedDanmaku(t, e.st, &model.Danmaku{
		Oid: delOid, Aid: delAid, Mid: delMid, ProgressMs: 21_000,
		Mode: int32(rpc.DanmakuMode_MODE_SCROLL), Fontsize: 25, Color: 0xFFFFFF,
		Content: "要被删除的那条弹幕", State: state, Pool: pool, SegNo: delSegNo,
		IdempotencyKey: delIdemKey, TraceId: "trace-seed",
		Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})
}

func delReq(dmid, mid int64, admin bool) *rpc.DeleteDanmakuReq {
	return &rpc.DeleteDanmakuReq{Dmid: dmid, Mid: mid, Admin: admin, Reason: "用户自助删除", TraceId: "trace-del"}
}

// assertRowUntouched 断言某一行的可见性列与审计列都没被动过（守卫/越权拒绝路径用）。
func assertRowUntouched(t *testing.T, label string, e *env, dmid int64, state, pool int32) {
	t.Helper()
	row := e.st.danmaku.get(dmid)
	wantEQ(t, label, "state", row.State, state)
	wantEQ(t, label, "pool", row.Pool, pool)
}

// --- 守卫 ---

// TestDeleteDanmakuRejectsInvalidRequests dmid/mid 守卫必须发生在读库之前：
// 一条畸形删除请求不许产生任何依赖调用（尤其不许先查一遍主表再决定认不认）。
func TestDeleteDanmakuRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.DeleteDanmakuReq
		want error
	}{
		{"dmid 为 0", &rpc.DeleteDanmakuReq{Mid: delMid}, model.ErrInvalidDmid},
		{"dmid 为负", &rpc.DeleteDanmakuReq{Dmid: -7, Mid: delMid}, model.ErrInvalidDmid},
		{"dmid 非法优先于 mid 非法", &rpc.DeleteDanmakuReq{Dmid: 0, Mid: 0}, model.ErrInvalidDmid},
		{"mid 为 0（管理员也必须带操作者身份）", &rpc.DeleteDanmakuReq{Dmid: 101, Admin: true}, model.ErrInvalidMid},
		{"mid 为负", &rpc.DeleteDanmakuReq{Dmid: 101, Mid: -1}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := deleteDanmaku(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法请求仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			wantCount(t, tc.name, e.st.log, "tx.", 0)
		})
	}
}

// --- 归属与权限 ---

// TestDeleteDanmakuNonOwnerIsRejectedWithoutSideEffects 归属校验：
// 别人（哪怕登录了、哪怕知道 dmid）不能删我的弹幕；管理员分支必须显式打开。
// 拒绝必须发生在状态机与事务之前——一行都不许改、一条留痕都不许写、缓存一次都不许失效。
func TestDeleteDanmakuNonOwnerIsRejectedWithoutSideEffects(t *testing.T) {
	cases := []struct {
		name  string
		mid   int64
		admin bool
		want  error
	}{
		{"他人普通用户删除", 2002, false, model.ErrForbidden},
		{"他人带原因码删除", 2003, false, model.ErrForbidden},
		{"本人删除（放行）", delMid, false, nil},
		{"管理员删他人（放行，走 admin 分支）", delAdmin, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
			before := e.st.log.snapshot()

			_, err := deleteDanmaku(t, e, delReq(seeded.Dmid, tc.mid, tc.admin))

			if tc.want != nil {
				wantErrIs(t, tc.name, err, tc.want)
				// 只读了一次主表就拿定权限，没有二次回源、没有碰缓存
				wantOps(t, tc.name, e.st.log.opsFrom(before), []string{findOneOp(seeded.Dmid)})
				assertRowUntouched(t, tc.name, e, seeded.Dmid, model.StateNormal, model.PoolNormal)
				wantEQ(t, tc.name, "op_log 行数", len(e.st.opLog.rows), 0)
				wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, 0)
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				wantCount(t, tc.name, e.st.log, "segment.", 0)
				return
			}
			wantNoErr(t, tc.name, err)
		})
	}
}

// TestDeleteDanmakuByOwnerPersistsStateAndAudit 本人删除的正常路径：
// 逐字段钉死「主表软删后的每一列」与「同事务那条 op_log」。
func TestDeleteDanmakuByOwnerPersistsStateAndAudit(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
	seedSegment(t, e.st, delOid, delSegNo, 5) // 段表里原有 5 条可见
	e.st.cache.warmSegmentCount(delOid, delSegNo, 5)
	e.st.cache.warmSegment(delOid, delSegNo, seeded) // 段缓存里有这份下发包
	calledAt := time.Now().Unix()

	reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))
	wantNoErr(t, "本人删除", err)
	if reply == nil {
		t.Fatal("本人删除：回复 = nil, want 非空的 EmptyReply")
	}

	// 主表：软删 + 屏蔽池，其余列原样保留（AGENTS.md §8 审计证据）
	row := e.st.danmaku.get(seeded.Dmid)
	wantEQ(t, "本人删除", "dmid", row.Dmid, seeded.Dmid)
	wantEQ(t, "本人删除", "state", row.State, model.StateDeleted)
	wantEQ(t, "本人删除", "pool", row.Pool, model.PoolBlock)
	wantEQ(t, "本人删除", "oid", row.Oid, delOid)
	wantEQ(t, "本人删除", "aid", row.Aid, delAid)
	wantEQ(t, "本人删除", "mid（不改成操作者）", row.Mid, delMid)
	wantEQ(t, "本人删除", "progress_ms", row.ProgressMs, int64(21_000))
	wantEQ(t, "本人删除", "mode", row.Mode, int32(rpc.DanmakuMode_MODE_SCROLL))
	wantEQ(t, "本人删除", "fontsize", row.Fontsize, int32(25))
	wantEQ(t, "本人删除", "color", row.Color, int32(0xFFFFFF))
	wantEQ(t, "本人删除", "content（软删不清正文）", row.Content, "要被删除的那条弹幕")
	wantEQ(t, "本人删除", "seg_no", row.SegNo, delSegNo)
	wantEQ(t, "本人删除", "idempotency_key", row.IdempotencyKey, delIdemKey)
	wantEQ(t, "本人删除", "trace_id", row.TraceId, "trace-seed")
	wantEQ(t, "本人删除", "ctime", row.Ctime, int64(1_700_000_000))
	if row.Mtime <= 1_700_000_000 || row.Mtime < calledAt {
		t.Errorf("本人删除：mtime = %d, want 被刷成当前时间（原值 1700000000）", row.Mtime)
	}
	wantEQ(t, "本人删除", "主表仍是 1 行（不物理删）", int64(e.st.danmaku.countRows()), 1)

	// 审计留痕：同事务、同 dmid、from→to 可追
	if len(e.st.opLog.rows) != 1 {
		t.Fatalf("本人删除：op_log 行数 = %d, want 1", len(e.st.opLog.rows))
	}
	lg := *e.st.opLog.rows[0]
	wantEQ(t, "删除留痕", "log_id", lg.LogID, int64(301))
	wantEQ(t, "删除留痕", "dmid", lg.Dmid, seeded.Dmid)
	wantEQ(t, "删除留痕", "action", lg.Action, model.ActionDelete)
	wantEQ(t, "删除留痕", "from_state", lg.FromState, model.StateNormal)
	wantEQ(t, "删除留痕", "to_state", lg.ToState, model.StateDeleted)
	wantEQ(t, "删除留痕", "operator_mid", lg.OperatorMid, delMid)
	wantEQ(t, "删除留痕", "operator_role", lg.OperatorRole, model.RoleSelf)
	wantEQ(t, "删除留痕", "reason", lg.Reason, "用户自助删除")
	wantEQ(t, "删除留痕", "event_id（非事件驱动）", lg.EventID, "")
	wantEQ(t, "删除留痕", "trace_id", lg.TraceID, "trace-del")
	if lg.Ctime < calledAt || lg.Ctime > calledAt+2 {
		t.Errorf("删除留痕：ctime = %d, want ≈ %d", lg.Ctime, calledAt)
	}
	// 留痕与主表 CAS 必须拿到同一个事务会话，且全程只开一个事务
	wantEQ(t, "删除留痕", "op_log 在事务内", e.st.opLog.txSawSession, true)
	wantEQ(t, "删除留痕", "op_log 绕过事务", e.st.opLog.txNilSession, false)
	wantEQ(t, "删除留痕", "主表 CAS 在事务内", e.st.danmaku.txSawSession, true)
	wantEQ(t, "删除留痕", "事务次数", e.st.conn.transactions, 1)
	wantEQ(t, "删除留痕", "提交次数", e.st.conn.committed, 1)

	// 派生与失效：原本可见 ⇒ 段计数 -1（先 DB 再缓存），最后失效段列表
	wantOps(t, "本人删除", e.ops(), []string{
		findOneOp(seeded.Dmid),
		opTxBegin,
		transitionTxOp(seeded.Dmid, model.StateNormal, model.StateDeleted, model.PoolBlock),
		oplogTxOp(seeded.Dmid, model.ActionDelete, ""),
		opTxCommit,
		segIncrOp(delOid, delSegNo, -1),
		cntIncrOp(delOid, delSegNo, -1),
		segDelOp(delOid, delSegNo),
	})
	wantEQ(t, "本人删除", "段表计数回退", e.st.segment.countAt(delOid, delSegNo), int32(4))
	wantEQ(t, "本人删除", "缓存段计数回退", func() int32 { v, _ := e.st.cache.count(delOid, delSegNo); return v }(), int32(4))
	_, segCached := e.st.cache.segmentRows(delOid, delSegNo)
	wantEQ(t, "本人删除", "段列表缓存已失效", segCached, false)
	// 删除不许回填 task_id（DeleteDanmaku 传 taskID=0）
	wantCount(t, "本人删除", e.st.log, "danmaku.SetTask", 0)
}

// TestDeleteDanmakuByAdminRecordsAdminRole 管理员分支单独钉：
// 留痕角色是 RoleAdmin、operator_mid 是管理员自己，弹幕作者列不被改写。
func TestDeleteDanmakuByAdminRecordsAdminRole(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StatePending, model.PoolReview)

	_, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delAdmin, true))
	wantNoErr(t, "管理员删他人", err)

	row := e.st.danmaku.get(seeded.Dmid)
	wantEQ(t, "管理员删他人", "state", row.State, model.StateDeleted)
	wantEQ(t, "管理员删他人", "mid 仍是作者", row.Mid, delMid)
	lg := *e.st.opLog.rows[0]
	wantEQ(t, "管理员删他人", "operator_role", lg.OperatorRole, model.RoleAdmin)
	wantEQ(t, "管理员删他人", "operator_mid", lg.OperatorMid, delAdmin)
	wantEQ(t, "管理员删他人", "from_state", lg.FromState, model.StatePending)

	// 待审池本来就不在下发包里：只失效段列表，不动段计数（否则会把别人的计数扣脏）
	wantOps(t, "管理员删他人", e.ops(), []string{
		findOneOp(seeded.Dmid), opTxBegin,
		transitionTxOp(seeded.Dmid, model.StatePending, model.StateDeleted, model.PoolBlock),
		oplogTxOp(seeded.Dmid, model.ActionDelete, ""), opTxCommit,
		segDelOp(delOid, delSegNo),
	})
	wantCount(t, "管理员删他人", e.st.log, "segment.Incr", 0)
	wantCount(t, "管理员删他人", e.st.log, "cache.IncrCnt", 0)
	wantEQ(t, "管理员删他人", "段表无行", e.st.segment.countAt(delOid, delSegNo), int32(-1))
}

// TestDeleteDanmakuStateMatrixPinsWhichStatesMayBeDeleted 逐状态跑一遍删除：
// 四种非删除态都能删（可见态额外回退段计数），已删除态按幂等成功返回且零副作用。
func TestDeleteDanmakuStateMatrixPinsWhichStatesMayBeDeleted(t *testing.T) {
	cases := []struct {
		name          string
		state, pool   int32
		wantIncr      bool
		wantFromState int32
	}{
		{"正常态", model.StateNormal, model.PoolNormal, true, model.StateNormal},
		{"正常态但待审池", model.StateNormal, model.PoolReview, false, model.StateNormal},
		{"待审态", model.StatePending, model.PoolReview, false, model.StatePending},
		{"折叠态", model.StateFolded, model.PoolBlock, false, model.StateFolded},
		{"驳回态", model.StateRejected, model.PoolBlock, false, model.StateRejected},
		{"已删除态（幂等）", model.StateDeleted, model.PoolBlock, false, model.StateDeleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := seedVisible(t, e, tc.state, tc.pool)

			reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))

			wantNoErr(t, tc.name, err)
			if reply == nil {
				t.Fatalf("%s：回复 = nil, want 非空", tc.name)
			}
			row := e.st.danmaku.get(seeded.Dmid)
			wantEQ(t, tc.name, "state", row.State, model.StateDeleted)
			wantEQ(t, tc.name, "pool（统一进屏蔽池）", row.Pool, model.PoolBlock)

			if tc.state == model.StateDeleted {
				// 已是删除态：幂等成功，不写留痕、不开事务、不二次扣计数（否则计数会被删成负数）
				wantOps(t, tc.name, e.ops(), []string{findOneOp(seeded.Dmid)})
				wantEQ(t, tc.name, "op_log 行数", len(e.st.opLog.rows), 0)
				wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, 0)
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				return
			}
			lg := *e.st.opLog.rows[0]
			wantEQ(t, tc.name, "from_state", lg.FromState, tc.wantFromState)
			wantEQ(t, tc.name, "to_state", lg.ToState, model.StateDeleted)
			wantEQ(t, tc.name, "本次只写一条留痕", len(e.st.opLog.rows), 1)
			wantEQ(t, tc.name, "段计数是否回退", countIn(e.ops(), "segment.Incr") == 1, tc.wantIncr)
		})
	}
}

// TestDeleteDanmakuIllegalTransitionOnlyForDirtyState 状态机门禁的可达性如实钉住：
// 四个合法非删除态到 DELETED 都在 legalTransitions 里，所以 ErrInvalidStateTransition
// 只有「库里出现枚举外的脏状态」时才会命中（danmaku.state 是裸 TINYINT，无 CHECK 约束）。
// 脏值必须被拒绝且零副作用，绝不能被顺手洗成删除态或可见态。
func TestDeleteDanmakuIllegalTransitionOnlyForDirtyState(t *testing.T) {
	cases := []struct {
		name  string
		state int32
	}{
		{"枚举外的脏状态 9", 9},
		{"枚举外的脏状态 -1", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := seedVisible(t, e, tc.state, model.PoolNormal)

			reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))

			wantErrIs(t, tc.name, err, model.ErrInvalidStateTransition)
			if reply != nil {
				t.Errorf("%s：非法迁移仍返回 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), []string{findOneOp(seeded.Dmid)})
			assertRowUntouched(t, tc.name, e, seeded.Dmid, tc.state, model.PoolNormal)
			wantEQ(t, tc.name, "op_log 行数", len(e.st.opLog.rows), 0)
			wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, 0)
		})
	}
}

// TestDeleteDanmakuNotFound 目标不存在时是 404 语义而不是「静默成功」，
// 并且不允许靠插入一行来补齐（幂等只针对已删除态，不针对不存在的 dmid）。
func TestDeleteDanmakuNotFound(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)

	reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid+50, delMid, false))

	wantErrIs(t, "弹幕不存在", err, model.ErrDanmakuNotFound)
	if reply != nil {
		t.Errorf("弹幕不存在仍返回 %+v", reply)
	}
	wantOps(t, "弹幕不存在", e.ops(), []string{findOneOp(seeded.Dmid + 50)})
	wantEQ(t, "弹幕不存在", "行数没有被补写", int64(e.st.danmaku.countRows()), 1)
}

// TestDeleteDanmakuCasMissReportsConcurrentUpdate 并发消费者在「读到行」与「UPDATE」之间
// 改了状态 ⇒ CAS 影响 0 行：必须报可重试的 ErrConcurrentUpdate，
// 不得覆盖对方的结论、不得留下半截审计、不得动缓存。
func TestDeleteDanmakuCasMissReportsConcurrentUpdate(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
	// 并发机审先把它判成驳回
	e.st.danmaku.flipBeforeCas(seeded.Dmid, model.StateRejected)

	reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))

	wantErrIs(t, "CAS 未命中", err, model.ErrConcurrentUpdate)
	if reply != nil {
		t.Errorf("CAS 未命中仍返回 %+v", reply)
	}
	wantOps(t, "CAS 未命中", e.ops(), []string{
		findOneOp(seeded.Dmid), opTxBegin,
		transitionTxOp(seeded.Dmid, model.StateNormal, model.StateDeleted, model.PoolBlock),
		opTxRollback,
	})
	row := e.st.danmaku.get(seeded.Dmid)
	wantEQ(t, "CAS 未命中", "state 保留对方结论", row.State, model.StateRejected)
	wantEQ(t, "CAS 未命中", "pool 保留对方结论", row.Pool, model.PoolNormal)
	wantEQ(t, "CAS 未命中", "留痕没被写进去", len(e.st.opLog.rows), 0)
	wantCount(t, "CAS 未命中", e.st.log, "oplog.InsertTx", 0)
	wantEQ(t, "CAS 未命中", "事务回滚", e.st.conn.rolledBack, 1)
	wantEQ(t, "CAS 未命中", "事务提交", e.st.conn.committed, 0)
	wantCount(t, "CAS 未命中", e.st.log, "cache.", 0)
	wantCount(t, "CAS 未命中", e.st.log, "segment.Incr", 0)
}

// TestDeleteDanmakuPropagatesDownstreamFailures 每个依赖各注入一次故障。
// 分界线很清楚：**DB/事务侧失败必须上抛且整体回滚，缓存与派生计数侧失败只能降级**。
func TestDeleteDanmakuPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		arm     func(e *env, dmid int64)
		wantErr error // nil 表示该故障被按口径吞掉，删除本身照常成功
		// wantOps 是从入口到故障点的完整有序序列
		wantOps func(dmid int64) []string
		// wantState 是故障后的主表状态
		wantState int32
		wantAudit int // op_log 行数
		// wantTx 是故障时已经开启的事务数（在读主表就失败的守卫，事务数应为 0）
		wantTx int
	}{
		{
			name:      "主表读失败",
			arm:       func(e *env, _ int64) { e.st.danmaku.failWith("FindOne", errDB) },
			wantErr:   errDB,
			wantOps:   func(dmid int64) []string { return []string{findOneOp(dmid)} },
			wantState: model.StateNormal,
			wantAudit: 0,
			wantTx:    0,
		},
		{
			name:      "状态 CAS 失败",
			arm:       func(e *env, _ int64) { e.st.danmaku.failWith("TransitionStateTx", errDB) },
			wantErr:   errDB,
			wantOps:   delFailOps,
			wantState: model.StateNormal,
			wantAudit: 0,
			wantTx:    1,
		},
		{
			name:      "审计留痕写失败 ⇒ 主表必须一起回滚",
			arm:       func(e *env, _ int64) { e.st.opLog.failWith("InsertTx", errDB) },
			wantErr:   errDB,
			wantOps:   func(dmid int64) []string { return append(delCASOps(dmid), opTxRollback) },
			wantState: model.StateNormal,
			wantAudit: 0,
			wantTx:    1,
		},
		{
			name:      "段表派生失败只记日志",
			arm:       func(e *env, _ int64) { e.st.segment.failWith("Incr", errDB) },
			wantErr:   nil,
			wantOps:   delTailOps,
			wantState: model.StateDeleted,
			wantAudit: 1,
		},
		{
			name:      "段计数缓存失败只记日志",
			arm:       func(e *env, _ int64) { e.st.cache.failWith("IncrSegmentCount", errCache) },
			wantErr:   nil,
			wantOps:   delTailOps,
			wantState: model.StateDeleted,
			wantAudit: 1,
		},
		{
			name:      "段缓存失效失败只记日志",
			arm:       func(e *env, _ int64) { e.st.cache.failWith("DelSegment", errCache) },
			wantErr:   nil,
			wantOps:   delTailOps,
			wantState: model.StateDeleted,
			wantAudit: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
			seedSegment(t, e.st, delOid, delSegNo, 5)
			e.st.cache.warmSegmentCount(delOid, delSegNo, 5)
			e.st.cache.warmSegment(delOid, delSegNo, seeded)
			tc.arm(e, seeded.Dmid)

			reply, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))

			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				if reply != nil {
					t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
				}
			} else {
				wantNoErr(t, tc.name, err)
				if reply == nil {
					t.Fatalf("%s：可降级故障把删除打成了失败", tc.name)
				}
			}
			wantOps(t, tc.name, e.ops(), tc.wantOps(seeded.Dmid))
			wantEQ(t, tc.name, "主表 state", e.st.danmaku.get(seeded.Dmid).State, tc.wantState)
			wantEQ(t, tc.name, "op_log 行数", len(e.st.opLog.rows), tc.wantAudit)
			// 事务失败时既不能提交也不能顺手把缓存清了（否则缓存与 DB 谁都不认识谁）
			if tc.wantErr != nil {
				wantCount(t, tc.name, e.st.log, "cache.", 0)
				wantCount(t, tc.name, e.st.log, "segment.", 0)
				wantEQ(t, tc.name, "段表计数未被改动", e.st.segment.countAt(delOid, delSegNo), int32(5))
				wantEQ(t, tc.name, "事务次数", e.st.conn.transactions, tc.wantTx)
				wantEQ(t, tc.name, "提交次数", e.st.conn.committed, 0)
				wantEQ(t, tc.name, "屏蔽池没被半写", e.st.danmaku.get(seeded.Dmid).Pool, model.PoolNormal)
			}
		})
	}
}

// delCASPrefix 是「读主表 → 进事务 → 主表 CAS」的三段，事务结尾（提交或回滚）由调用方补。
// 每个期望序列都从这里现建现用，避免多条用例共享同一底层数组被 append 改写。
func delCASPrefix(dmid int64) []string {
	return []string{
		findOneOp(dmid), opTxBegin,
		transitionTxOp(dmid, model.StateNormal, model.StateDeleted, model.PoolBlock),
	}
}

// delCASOps 是「进事务 → CAS → 写留痕」的四段（不含结尾的提交或回滚）。
func delCASOps(dmid int64) []string {
	return append(delCASPrefix(dmid), oplogTxOp(dmid, model.ActionDelete, ""))
}

// delFailOps 是 CAS 本身报错的序列：事务只到 CAS 一步就回滚，留痕根本没被调用。
func delFailOps(dmid int64) []string {
	return append(delCASPrefix(dmid), opTxRollback)
}

// delTailOps 是可降级故障下的完整序列：事务照常提交，派生与失效一步不少。
func delTailOps(dmid int64) []string {
	return append(delCASOps(dmid),
		opTxCommit, segIncrOp(delOid, delSegNo, -1), cntIncrOp(delOid, delSegNo, -1), segDelOp(delOid, delSegNo))
}

// TestDeleteDanmakuSegmentCountNeverGoesNegative 段计数是派生投影：
// 段表没行（cron 还没算/已被归档）时删除可见弹幕，GREATEST 口径必须停在 0 而不是负数。
func TestDeleteDanmakuSegmentCountNeverGoesNegative(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
	seedSegment(t, e.st, delOid, delSegNo, 0)

	_, err := deleteDanmaku(t, e, delReq(seeded.Dmid, delMid, false))

	wantNoErr(t, "段计数下溢", err)
	wantOps(t, "段计数下溢", e.ops(), []string{
		findOneOp(seeded.Dmid), opTxBegin,
		transitionTxOp(seeded.Dmid, model.StateNormal, model.StateDeleted, model.PoolBlock),
		oplogTxOp(seeded.Dmid, model.ActionDelete, ""), opTxCommit,
		segIncrOp(delOid, delSegNo, -1), cntIncrOp(delOid, delSegNo, -1), segDelOp(delOid, delSegNo),
	})
	wantEQ(t, "段计数下溢", "段表计数停在 0", e.st.segment.countAt(delOid, delSegNo), int32(0))
	// 缓存计数回退到 0 以下时按真实 Cache 口径回删键，让下一次读回源修正
	_, cntHit := e.st.cache.count(delOid, delSegNo)
	wantEQ(t, "段计数下溢", "缓存计数键已清", cntHit, false)
}

// TestDeleteDanmakuReasonTooLongIsNotGuarded 钉住现状：op_log.reason 是 VARCHAR(255)，
// 但 logic 不校验 in.Reason 长度，超长原因会由 MySQL 抛 1406 ⇒ gRPC 报错，
// 而不是干净的 4xx（缺陷清单第 ⑤ 条）。修法落地时本用例应改成断言守卫错误。
func TestDeleteDanmakuReasonTooLongIsNotGuarded(t *testing.T) {
	e := newEnv(t)
	seeded := seedVisible(t, e, model.StateNormal, model.PoolNormal)
	in := delReq(seeded.Dmid, delMid, false)
	in.Reason = strings.Repeat("长", 300)

	_, err := deleteDanmaku(t, e, in)

	wantNoErr(t, "超长删除原因（内存替身不校验列宽）", err)
	wantEQ(t, "超长删除原因（现状）", "op_log.reason 字节数", len(e.st.opLog.rows[0].Reason), 900)
}
