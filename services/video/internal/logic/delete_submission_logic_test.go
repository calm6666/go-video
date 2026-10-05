package logic

// delete_submission_logic_test.go 钉 DeleteSubmission 的真实口径：
//   - 删除是**状态流转到 DELETED**（deletesubmissionlogic.go:53），不是物理删除：
//     行还在、meta 列原封不动，只有 state 与 mtime 变；
//   - 与 TransitionState 复用同一条仓储路径 repository.TransitionState，
//     「状态 + 审计行」在同一个 TransactCtx 里（repository.go:134-163）；
//   - 重复删除走 sub.State==StateDeleted 短路，幂等且不再写第二条审计；
//   - operator 由 logic 拼成 "owner:"+mid、reason 固定串，调用方无法伪造；
//   - 与 TransitionState 的**唯一**行为差异：删除不失效稿件详情缓存（见 README 已知缺口 2）。
//
// 「校验后被插队」的丢失更新由 transition_state_logic_test.go 的
// TestTransitionStateLostUpdateIsNotDetected 统一钉住（两条路径共用同一个仓储事务）。

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

func deleteCall(st *store, in *rpc.SubmissionReq) (*rpc.EmptyReply, error) {
	return NewDeleteSubmissionLogic(context.Background(), st.svcCtx).DeleteSubmission(in)
}

// deleteTxSeq 返回「删除成功」的完整依赖序列（由实现反推：
// logic 首读 → repository.TransitionState 开事务 → 事务内二次校验读 →
// UpdateState(session) → audit Insert(session) →
// 若稿件删除前公开过，再写一行 video_outbox.Insert(session)；
// 草稿/上传中/被驳回的稿件从未进过索引，删除不产事件（判定表见 content_event_logic_test.go）。
func deleteTxSeq(fromState int32) []string {
	ops := []string{
		"video_submission.FindOne:101",
		"db.TransactCtx",
		"video_submission.FindOne:101",
		"video_submission.UpdateState:101/14",
		fmt.Sprintf("video_audit_log.Insert:101:%d->14", fromState),
	}
	if wantEventAction(fromState, model.StateDeleted) != "" {
		ops = append(ops, "video_outbox.Insert:101")
	}
	return ops
}

// TestDeleteSubmissionGuardsRejectBeforeAnyDependency 锁 aid → mid 的守卫顺序，
// 并锁拒绝时不触库、不动行。
func TestDeleteSubmissionGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.SubmissionReq
		want error
	}{
		{"aid=0", &rpc.SubmissionReq{Aid: 0, Mid: 7}, model.ErrInvalidAid},
		{"aid<0", &rpc.SubmissionReq{Aid: -5, Mid: 7}, model.ErrInvalidAid},
		{"mid=0", &rpc.SubmissionReq{Aid: 101, Mid: 0}, model.ErrInvalidMid},
		{"mid<0", &rpc.SubmissionReq{Aid: 101, Mid: -1}, model.ErrInvalidMid},
		{"两个都非法时先报 aid（守卫顺序）", &rpc.SubmissionReq{Aid: 0, Mid: 0}, model.ErrInvalidAid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)
			from := st.log().snapshot()

			got, err := deleteCall(st, tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
			wantEq(t, tc.name, "state 未变", st.sub(101).State, model.StateDraft)
			wantEq(t, tc.name, "事务次数", st.txCount(), 0)
			wantEq(t, tc.name, "审计行数", st.auditCount(), 0)
		})
	}
}

// TestDeleteSubmissionIsStateTransitionNotPhysicalDelete 钉「删除＝状态流转」：
// 行必须还在（物理删除这里会红），标题/简介/封面/分区/标签/mid/ctime 全部保持原值，
// 只有 state=DELETED、mtime 被 SQL 刷新；并且只发一个事务、只碰 video_submission。
func TestDeleteSubmissionIsStateTransitionNotPhysicalDelete(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StatePublished)

	got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7, Ip: "10.0.0.7"})
	wantNoErr(t, "删除稿件", err)
	if got == nil {
		t.Fatal("删除成功应返回空应答而不是 nil")
	}

	stored := st.sub(101)
	if stored == nil {
		t.Fatal("稿件行不见了：删除变成了物理删除，违反 AGENTS.md §8「保留审计证据」的前提")
	}
	wantEq(t, "软删", "state", stored.State, model.StateDeleted)
	wantEq(t, "软删", "aid", stored.Aid, int64(101))
	wantEq(t, "软删", "mid", stored.Mid, int64(7))
	wantEq(t, "软删", "title", stored.Title, "t-101")
	wantEq(t, "软删", "desc", stored.Desc, "d-101")
	wantEq(t, "软删", "cover", stored.Cover, "c-101")
	wantEq(t, "软删", "typeid", stored.Typeid, int32(11))
	wantEq(t, "软删", "tag", stored.Tag, "g-101")
	wantEq(t, "软删", "ctime", stored.Ctime, staleTime)
	if stored.Mtime <= staleTime {
		t.Errorf("mtime = %d，状态更新的 SET mtime = nowUnix() 没生效", stored.Mtime)
	}

	wantSeq(t, "删除轨迹", st.log(), 0, deleteTxSeq(model.StatePublished)...)
	wantEq(t, "删除", "事务次数", st.txCount(), 1)
	wantCount(t, "删除不该写版次表", st.log(), "video_version.", 0)
	wantCount(t, "删除不该有第二次状态 UPDATE", st.log(), "video_submission.UpdateState", 1)
}

// TestDeleteSubmissionWritesOneAuditRow 钉审计行的每一列：
// from 是删除前的真实状态、to 恒为 DELETED、operator 是 logic 自己拼的 "owner:"+mid
// （调用方无法伪造）、reason 固定、ctime 取事务内当前时间、按 id 升序只有一条。
func TestDeleteSubmissionWritesOneAuditRow(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 42, model.StateRejected)
	before := time.Now().Unix()

	_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 42})
	wantNoErr(t, "删除被驳回的稿件", err)
	after := time.Now().Unix()

	rows := st.auditRows(101)
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1（%+v）", len(rows), rows)
	}
	row := rows[0]
	wantEq(t, "审计", "aid", row.Aid, int64(101))
	wantEq(t, "审计", "from_state", row.FromState, model.StateRejected)
	wantEq(t, "审计", "to_state", row.ToState, model.StateDeleted)
	wantEq(t, "审计", "operator", row.Operator, "owner:42")
	wantEq(t, "审计", "reason", row.Reason, "user delete submission")
	if row.Ctime < before || row.Ctime > after {
		t.Errorf("审计 ctime = %d, want 落在 [%d, %d]", row.Ctime, before, after)
	}
	wantEq(t, "审计", "id 由自增分配", row.ID, int64(1))

	// 审计写入必须带事务会话：fake 只在 session 非 nil 时接受，
	// 这里再确认轨迹里 UpdateState 与 audit Insert 都排在 db.TransactCtx 之后。
	ops := st.log().opsFrom(0)
	txAt := slices.Index(ops, "db.TransactCtx")
	auditAt := slices.IndexFunc(ops, func(o string) bool { return o == "video_audit_log.Insert:101:7->14" })
	if txAt < 0 || auditAt < 0 || auditAt < txAt {
		t.Errorf("审计行不在事务之后写入：[%v]", ops)
	}
}

// TestDeleteSubmissionTwiceIsIdempotentWithoutSecondAudit 钉重复删除的口径：
// 第二次同样返回成功（客户端重试不该看到 404），但**只发一条 SELECT**——
// 不起事务、不写第二条审计、不再改 mtime。
func TestDeleteSubmissionTwiceIsIdempotentWithoutSecondAudit(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	first, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "第一次删除", err)
	if first == nil {
		t.Fatal("第一次删除没有返回应答")
	}
	mtimeAfterFirst := st.sub(101).Mtime

	from := st.log().snapshot()
	second, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "第二次删除", err)
	if second == nil {
		t.Fatal("重复删除应返回幂等成功应答")
	}
	wantSeq(t, "第二次删除轨迹", st.log(), from, "video_submission.FindOne:101")
	wantEq(t, "重复删除", "事务总次数仍是 1", st.txCount(), 1)
	wantEq(t, "重复删除", "审计总行数仍是 1", st.auditCount(), 1)
	wantEq(t, "重复删除", "state", st.sub(101).State, model.StateDeleted)
	wantEq(t, "重复删除", "mtime 没被第二次刷新", st.sub(101).Mtime, mtimeAfterFirst)
}

// TestDeleteSubmissionRejectsStrangerBeforeDeletedShortCircuit 钉守卫顺序：
// 属主判定在「已删除短路」之前——陌生 mid 删别人的已删除稿件得到 ErrNotOwner，
// 而不是白送的幂等成功（否则调用方可以拿 404/200 的差异枚举出「谁的稿件存在」）。
func TestDeleteSubmissionRejectsStrangerBeforeDeletedShortCircuit(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDeleted)

	got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 8})
	wantErrIs(t, "陌生 mid 删别人已删除稿件", err, model.ErrNotOwner)
	if got != nil {
		t.Errorf("拒绝时仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:101")
	wantEq(t, "陌生 mid", "审计行数", st.auditCount(), 0)
}

// TestDeleteSubmissionMissingRowIsNotFound 钉「行不存在」与「事务内行消失」两种
// not found：前者由 logic 首读判定，后者由 repository 事务内二次校验判定
// （repository.go:141-143），两者都不许留下 UPDATE 或审计行。
func TestDeleteSubmissionMissingRowIsNotFound(t *testing.T) {
	t.Run("首读就没有", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)

		_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 999, Mid: 7})
		wantErrIs(t, "稿件不存在", err, model.ErrSubmissionNotFound)
		wantSeq(t, "轨迹", st.log(), 0, "video_submission.FindOne:999")
		wantEq(t, "稿件不存在", "事务次数", st.txCount(), 0)
	})

	t.Run("开事务前被人删掉", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.before("db.TransactCtx", func() { st.dropSub(101) })

		_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantErrIs(t, "事务内发现行没了", err, model.ErrSubmissionNotFound)
		wantSeq(t, "轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101")
		wantCount(t, "行没了不该还去 UPDATE", st.log(), "video_submission.UpdateState", 0)
		wantEq(t, "行没了", "审计行数", st.auditCount(), 0)
		st.s.checkHooks(t)
	})
}

// TestDeleteSubmissionAllowedAndRejectedStatesFollowMachineMatrix 不手抄枚举：
// 期望值直接由 statemachine.go 的 legalTransitions 里「谁能到 DELETED」推出来，
// 于是矩阵加/删一条出边时这条用例会跟着红，逼着改状态机的人同时确认删除口径。
// 被拒的一律「只读不写」：不起事务、不写审计、state 与 mtime 都不动。
func TestDeleteSubmissionAllowedAndRejectedStatesFollowMachineMatrix(t *testing.T) {
	allStates := []int32{
		model.StateDraft, model.StateUploading, model.StateUploaded, model.StateScanning,
		model.StateTranscoding, model.StateReadyForReview, model.StateRejected, model.StateAppeal,
		model.StateApproved, model.StateScheduled, model.StatePublished, model.StateOffline,
		model.StateExpired, model.StateDeleted,
	}
	for _, from := range allStates {
		if from == model.StateDeleted {
			continue // 已删除走幂等短路，见 TestDeleteSubmissionTwiceIsIdempotentWithoutSecondAudit
		}
		wantOK := slices.Contains(legalTransitions[from], model.StateDeleted)
		st := newStore()
		st.seedDraft(101, 7, from)

		_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		if wantOK {
			wantNoErr(t, "矩阵允许的删除", err)
			wantEq(t, "矩阵允许的删除", "state", st.sub(101).State, model.StateDeleted)
			wantEq(t, "矩阵允许的删除", "事务次数", st.txCount(), 1)
			continue
		}
		wantErrIs(t, "矩阵拒绝的删除", err, model.ErrInvalidStateTransition)
		wantSeq(t, "矩阵拒绝的删除轨迹", st.log(), 0, "video_submission.FindOne:101")
		wantEq(t, "矩阵拒绝的删除", "state 不变", st.sub(101).State, from)
		wantEq(t, "矩阵拒绝的删除", "mtime 不变", st.sub(101).Mtime, staleTime)
		wantEq(t, "矩阵拒绝的删除", "事务次数", st.txCount(), 0)
		wantEq(t, "矩阵拒绝的删除", "审计行数", st.auditCount(), 0)
	}
}

// TestDeleteSubmissionDoesNotInvalidateSubmissionCache 把缺陷钉成哨兵：
// 同样是一次状态流转，TransitionState 会调 InvalidateSubmissionCache
// （transitionstatelogic.go:57），DeleteSubmission 却一条缓存调用都不发
// （deletesubmissionlogic.go:53-56 只走 repository.TransitionState）。
// 现状无害（全仓无人写 video:sub:*，缓存恒空），一旦接入 cache-aside，
// 「稿件已被删除但详情还能命中 60 秒」就是这条差异的直接后果。
// 修法若定了（在 logic 补失效，或把失效下沉到 repository.TransitionState），本用例会红。
func TestDeleteSubmissionDoesNotInvalidateSubmissionCache(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StatePublished)

	_, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "删除", err)
	wantCount(t, "删除后的缓存失效", st.log(), "cache.", 0)
	if len(st.cache.dels) != 0 {
		t.Errorf("删除竟然失效了 %v，与当前实现不符，请同步 README 已知缺口", st.cache.dels)
	}
	// 对照组：同一条仓储路径的 TransitionState 确实会失效 aid=101。
	st2 := newStore()
	st2.seedDraft(101, 7, model.StateApproved)
	_, err = NewTransitionStateLogic(context.Background(), st2.svcCtx).
		TransitionState(&rpc.TransitionReq{Aid: 101, Target: rpc.SubmissionState_STATE_SCHEDULED, Operator: "op"})
	wantNoErr(t, "对照组：状态推进", err)
	if len(st2.cache.dels) != 1 || st2.cache.dels[0] != 101 {
		t.Errorf("对照组：TransitionState 应失效 aid=101，实得 %v", st2.cache.dels)
	}
}

// TestDeleteSubmissionTxStageFailuresLeaveKnownResidue 钉事务三段失败的「库里还剩什么」。
// 注意：本仓 fake **不回滚**（fakes_test.go 纪律 5），所以「state 已改成 DELETED 但审计为 0」
// 是替身语义、不是生产结论（生产由同事务回滚）；这里真正锁的是可跨实现复用的三件事：
//  1. 语句顺序——UPDATE 在审计 INSERT 之前（否则 audit 失败时 state 不会变）；
//  2. 任一段失败整调用必须原样报错，不许返回幂等成功；
//  3. 失败后 logic 不再往下走（既不失效缓存，也不复读）。
func TestDeleteSubmissionTxStageFailuresLeaveKnownResidue(t *testing.T) {
	t.Run("开启事务就失败", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("db.TransactCtx")

		got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantErrIs(t, "事务故障", err, errBoom)
		if got != nil {
			t.Errorf("事务失败仍返回 reply %+v", got)
		}
		wantSeq(t, "事务故障轨迹", st.log(), 0, "video_submission.FindOne:101", "db.TransactCtx")
		wantEq(t, "事务故障", "state", st.sub(101).State, model.StateDraft)
		wantEq(t, "事务故障", "审计行数", st.auditCount(), 0)
	})

	t.Run("状态 UPDATE 失败", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("submission.UpdateState")

		got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantErrIs(t, "UPDATE 故障", err, errBoom)
		if got != nil {
			t.Errorf("UPDATE 失败仍返回 reply %+v", got)
		}
		wantSeq(t, "UPDATE 故障轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx",
			"video_submission.FindOne:101", "video_submission.UpdateState:101/14")
		wantEq(t, "UPDATE 故障", "state", st.sub(101).State, model.StateDraft)
		wantEq(t, "UPDATE 故障", "mtime", st.sub(101).Mtime, staleTime)
		wantEq(t, "UPDATE 故障", "审计行数", st.auditCount(), 0)
		wantCount(t, "UPDATE 故障", st.log(), "cache.", 0)
	})

	t.Run("审计 INSERT 失败时 UPDATE 已经跑过", func(t *testing.T) {
		st := newStore()
		st.seedDraft(101, 7, model.StateDraft)
		st.fail("audit.Insert")

		got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
		wantErrIs(t, "审计故障", err, errBoom)
		if got != nil {
			t.Errorf("审计失败仍返回 reply %+v（不能把丢审计当成功）", got)
		}
		wantSeq(t, "审计故障轨迹", st.log(), 0,
			"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101",
			"video_submission.UpdateState:101/14", "video_audit_log.Insert:101:1->14")
		// 替身不回滚：state 已被改成 DELETED——这条只用来证明 UPDATE 先于审计 INSERT。
		wantEq(t, "审计故障（替身语义，生产由事务回滚）", "state", st.sub(101).State, model.StateDeleted)
		wantEq(t, "审计故障", "审计行数", st.auditCount(), 0)
		wantCount(t, "审计故障后不该再动缓存", st.log(), "cache.", 0)
	})
}

// TestDeleteSubmissionDetectsConcurrentStateChangeBeforeTxCheck 是「插队窗口」的一侧：
// 别的入口在 logic 首读之后、事务内二次校验之前把稿件推走，
// repository 用 fromState 比对比对得到 → ErrInvalidStateTransition，调用方必须重试。
// 另一侧（二次校验之后插队＝检测不到）见 TestTransitionStateLostUpdateIsNotDetected。
func TestDeleteSubmissionDetectsConcurrentStateChangeBeforeTxCheck(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.before("db.TransactCtx", func() { st.setState(101, model.StateScanning) })

	got, err := deleteCall(st, &rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantErrIs(t, "插队后状态已变", err, model.ErrInvalidStateTransition)
	if got != nil {
		t.Errorf("拒绝时仍返回 reply %+v", got)
	}
	wantSeq(t, "轨迹", st.log(), 0,
		"video_submission.FindOne:101", "db.TransactCtx", "video_submission.FindOne:101")
	wantEq(t, "插队", "state 保持插队后的值", st.sub(101).State, model.StateScanning)
	wantEq(t, "插队", "审计行数", st.auditCount(), 0)
	st.s.checkHooks(t)
}
