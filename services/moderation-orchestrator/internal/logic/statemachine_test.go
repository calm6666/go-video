package logic

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// statemachine_test.go 锁的是 AGENTS.md §8 说的「全仓最要紧的状态机」。
//
// 本服务没有显式的迁移表：状态机是**散落在 repository 三处 UpdateState 调用的 fromStates 变参里**
// （repository.go:290 worker → DONE ← {PENDING,PROCESSING}；:313 申诉 → APPEALED ← {DONE}；
// :354 结案 → APPEAL_DONE ← {APPEALED}），加上 model 的 CAS（RowsAffected=0 ⇒ ErrInvalidStateTransition）。
// 所以下面这张矩阵是**手工抄写**的期望表：行 = 当前态（proto 全部 7 个枚举 + 1 个枚举外脏值 + 1 个「任务行不存在」），
// 列 = 三条会推进状态的动作；每格手写「动作后任务的 state」+「CAS 有没有真的推进一行」。
// 期望值来自阅读 SQL 与 §8 的合法边，**不是**跑一遍现网再抄回来的：
// 现网与手写期望不一致的格子，都在注释里标了 TODO(缺陷) 并登记进 README。
//
// 另有两条对账用例（TestModerationStateMachineMatrixCoversEveryProtoState /
// TestModerationStateMachineOnlyHasTheThreeDeclaredEdges）专门核对：
// 矩阵行没漏枚举、排序方向与期望表一致、并且**没有第四条边**（漏边/多边都会红）。

// stateCell 是矩阵的一行。三个 *State 字段是「该动作执行后 moderation_task.state 的值」，
// -1 表示任务行不存在（fake 的行缺失口径，与 model 的 (nil,nil) 一致）。
type stateCell struct {
	name   string
	from   int32
	seeded bool // false ⇒ 不布任务行（被删/从没建过）

	// 列 1：worker 回写结论（目标 DONE，门槛 PENDING/PROCESSING）
	workerState int32
	workerMoved bool
	// 列 2：用户申诉（目标 APPEALED，门槛 DONE）
	appealState int32
	appealMoved bool
	// 列 3：运营结案（目标 APPEAL_DONE，门槛 APPEALED）
	closeState int32
	closeMoved bool
}

// stateMatrix 是手工抄写的期望矩阵。
//
// 合法边只有三条：1→3、2→3（worker 回写）、3→4（申诉）、4→5（结案）。
// 4→3（worker 在申诉中改写结论）、5→3、9→3、3→5（跳过申诉直接结案）都必须是「不推进」。
var stateMatrix = []stateCell{
	// 当前态 = UNSPECIFIED（0，枚举里是「未指定」，DB 默认值 0 能被写出来：
	// SubmitForReview 显式写 PENDING，但列没有 CHECK 约束，脏值照样能进来）。
	{name: "UNSPECIFIED(0)", from: stUnspecified, seeded: true,
		workerState: stUnspecified, appealState: stUnspecified, closeState: stUnspecified},
	{name: "PENDING(1)", from: stPending, seeded: true,
		workerState: stDone, workerMoved: true, // 唯一合法入口
		appealState: stPending, closeState: stPending},
	{name: "PROCESSING(2)", from: stProcessing, seeded: true,
		workerState: stDone, workerMoved: true,
		appealState: stProcessing, closeState: stProcessing},
	{name: "DONE(3)", from: stDone, seeded: true,
		// 重复回写：状态不动（幂等），但结论行会被 uniq_task 整行覆盖 ⇒ TODO(缺陷)：README 缺口 #4。
		workerState: stDone,
		appealState: stAppealed, appealMoved: true, // 唯一合法的申诉边
		closeState: stDone},
	{name: "APPEALED(4)", from: stAppealed, seeded: true,
		// worker 在申诉期间回写被咽成成功 ⇒ 申诉人看到的还是旧结论。TODO(缺陷)：README 缺口 #10。
		workerState: stAppealed,
		appealState: stAppealed, // 重复申诉不推进（这里靠 CAS 挡住，是巧合的正确）
		closeState:  stAppealDone, closeMoved: true},
	{name: "APPEAL_DONE(5)", from: stAppealDone, seeded: true,
		// 已终态：三条动作都不该再推进任务，但结论/申诉行照写。TODO(缺陷)：README 缺口 #4、#10。
		workerState: stAppealDone, appealState: stAppealDone, closeState: stAppealDone},
	{name: "CANCELED(9)", from: stCanceled, seeded: true,
		workerState: stCanceled, appealState: stCanceled, closeState: stCanceled},
	{name: "枚举外脏值(7)", from: stGhost, seeded: true,
		workerState: stGhost, appealState: stGhost, closeState: stGhost},
	{name: "任务行不存在", from: -1, seeded: false,
		// 结论/申诉孤儿行；UpdateState 影响 0 行也被咽成成功。TODO(缺陷)：README 缺口 #6。
		workerState: -1, appealState: -1, closeState: -1},
}

// advanceEdge 把「是否推进」翻译成期望的迁移流水（fake 记的是 "<id>:<from>-><to>"）。
// 三个 target 与 repository 三处调用手抄对齐，写错格子即红。
func advanceEdge(moved bool, from, to int32) []string {
	if !moved {
		return nil
	}
	return []string{fmt.Sprintf("501:%d->%d", from, to)}
}

// TestModerationStateMachineMatrix 用矩阵逐格执行三条动作。
func TestModerationStateMachineMatrix(t *testing.T) {
	for _, tc := range stateMatrix {
		// --- 列 1：worker 回写结论，目标 DONE ---
		t.Run("worker回写/"+tc.name, func(t *testing.T) {
			st := newStore()
			if tc.seeded {
				seedTask(st, 501, tc.from, nil)
			}
			_, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
				SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "机审命中搬运指纹"))
			// 三条动作在**所有**格子上都返回成功：非法迁移被 repository 咽掉了。
			// 这是矩阵里最要紧的一条事实：调用方（worker）拿不到「你的回写不合法」。
			wantNoErr(t, "状态机矩阵 worker回写 / "+tc.name, err)
			wantEQ(t, "状态机矩阵 worker回写 / "+tc.name, "任务状态", st.task.state(501), tc.workerState)
			wantStringsEQ(t, "状态机矩阵 worker回写 / "+tc.name, "迁移流水", st.task.advances,
				advanceEdge(tc.workerMoved, tc.from, stDone))
			// 结论行永远先落库，与状态机门槛无关（这正是「半截数据」的来源）。
			wantEQ(t, "状态机矩阵 worker回写 / "+tc.name, "结论行数", st.result.count(501), 1)
		})

		// --- 列 2：用户申诉，目标 APPEALED ---
		t.Run("申诉/"+tc.name, func(t *testing.T) {
			st := newStore()
			if tc.seeded {
				seedTask(st, 501, tc.from, nil)
			}
			_, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
				SubmitAppeal(appealReq(501, midAlice, "被误判为搬运，原创证明见附件"))
			wantNoErr(t, "状态机矩阵 申诉 / "+tc.name, err)
			wantEQ(t, "状态机矩阵 申诉 / "+tc.name, "任务状态", st.task.state(501), tc.appealState)
			wantStringsEQ(t, "状态机矩阵 申诉 / "+tc.name, "迁移流水", st.task.advances,
				advanceEdge(tc.appealMoved, tc.from, stAppealed))
			// 非 DONE 任务（含不存在/已结案/已撤销）也能建出申诉行 ⇒ 审核台会收到永远结不了案的工单。
			// TODO(缺陷)：README 缺口 #10。
			wantEQ(t, "状态机矩阵 申诉 / "+tc.name, "申诉行数", st.appeal.countForTask(501), 1)
		})

		// --- 列 3：运营结案，目标 APPEAL_DONE ---
		t.Run("结案/"+tc.name, func(t *testing.T) {
			st := newStore()
			if tc.seeded {
				seedTask(st, 501, tc.from, nil)
			}
			seedAppeal(st, 601, 501, nil)
			_, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
				ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
			wantNoErr(t, "状态机矩阵 结案 / "+tc.name, err)
			wantEQ(t, "状态机矩阵 结案 / "+tc.name, "任务状态", st.task.state(501), tc.closeState)
			wantStringsEQ(t, "状态机矩阵 结案 / "+tc.name, "迁移流水", st.task.advances,
				advanceEdge(tc.closeMoved, tc.from, stAppealDone))
			// 申诉侧无条件结案（UPDATE ... WHERE id=? AND state=0，与任务状态无关）。
			wantEQ(t, "状态机矩阵 结案 / "+tc.name, "申诉状态", st.appeal.rows[601].State, appealStateHandled)
		})
	}
}

// wantStatesEQ 比较 int32 状态序列（wantIntsEQ 是 []int64 的，任务 ID 专用）。
func wantStatesEQ(t *testing.T, label, field string, got, want []int32) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s：%s = %v, want %v", label, field, got, want)
	}
}

// TestModerationStateMachineMatrixCoversEveryProtoState 核对期望表本身没漏边、没写错方向：
// 矩阵里 seeded 的行必须恰好等于 proto 枚举里**按数值升序**的全部状态。
// 若 proto 新增一个 TaskState（例如加 PUBLISHED），这条即红，逼着维护者补格子而不是默默漏掉。
func TestModerationStateMachineMatrixCoversEveryProtoState(t *testing.T) {
	var enumStates []int32
	for code := range rpc.TaskState_name {
		enumStates = append(enumStates, code)
	}
	sort.Slice(enumStates, func(i, j int) bool { return enumStates[i] < enumStates[j] }) // 升序，与下面 seeded 行的口径一致

	var matrixStates []int32
	ghostRows := 0
	for _, tc := range stateMatrix {
		if !tc.seeded {
			continue
		}
		if _, ok := rpc.TaskState_name[tc.from]; !ok {
			ghostRows++
			continue
		}
		matrixStates = append(matrixStates, tc.from)
	}
	// 收集顺序 = 矩阵书写顺序，比对的是升序枚举 ⇒ 矩阵行序打乱即红（排序方向对账）。
	wantStatesEQ(t, "状态机矩阵自检", "行 = proto 枚举（升序）", matrixStates, enumStates)
	wantEQ(t, "状态机矩阵自检", "枚举外脏值行数", ghostRows, 1)

	// 三个目标态必须都是 proto 里存在的态；写了不存在的目标态说明我抄错了 repository 的调用。
	for _, to := range []int32{stDone, stAppealed, stAppealDone} {
		if _, ok := rpc.TaskState_name[to]; !ok {
			t.Errorf("状态机矩阵自检：目标态 %d 不在 proto 枚举里", to)
		}
	}
	// 脏值行不参与上面的对账，单独确认它真的是「枚举外」的值。
	if _, ok := rpc.TaskState_name[stGhost]; ok {
		t.Errorf("状态机矩阵自检：stGhost=%d 现在是合法枚举了，矩阵的脏值行需要换一个值", stGhost)
	}
}

// TestModerationStateMachineOnlyHasTheThreeDeclaredEdges 断言整个状态机**只有**三条边：
// 把矩阵摊平成 (from, action, to) 全集，出现的迁移必须是
// {1→3, 2→3, 3→4, 4→5}，且没有任何动作能把任务写成 6/7/8（发布/撤销/通过一类越权态）。
func TestModerationStateMachineOnlyHasTheThreeDeclaredEdges(t *testing.T) {
	// 手工抄写的合法边集合（升序，便于和 wantStringsEQ 的口径对齐）。
	wantEdges := []string{"1->3", "2->3", "3->4", "4->5"}

	var gotEdges []string
	for _, tc := range stateMatrix {
		if !tc.seeded {
			continue
		}
		for _, e := range []struct {
			moved bool
			to    int32
		}{
			{tc.workerMoved, stDone},
			{tc.appealMoved, stAppealed},
			{tc.closeMoved, stAppealDone},
		} {
			if e.moved {
				gotEdges = append(gotEdges, fmt.Sprintf("%d->%d", tc.from, e.to))
			}
		}
	}
	sort.Strings(gotEdges)
	wantStringsEQ(t, "状态机边集", "全部合法边", gotEdges, wantEdges)

	// 「没有第四条边」用轨迹说话：三条动作实际发起过的目标态里，
	// 不允许出现 §8 明令禁止的发布/审批类状态码（6/7/8），也不允许出现 PENDING/PROCESSING/CANCELED 回退。
	for _, to := range attemptedTargets(t) {
		if to == 6 || to == 7 || to == 8 {
			t.Errorf("状态机边集：动作把任务写成了越权态 %d", to)
		}
		if to == stPending || to == stProcessing || to == stCanceled {
			t.Errorf("状态机边集：动作把任务写回了 %d，等于凭空多出一条回退边", to)
		}
	}
}

// TestModerationStateMachineHasNoWriterForProcessing 查「漏边」：
// PROCESSING(2) 与 CANCELED(9) 是 proto 里定义、但**没有任何逻辑会把任务写成**的态。
// 也就是说 worker 拉了任务不会置 PROCESSING（AGENTS.md §8 要求的中间态没人写），
// 撤销审核也没有入口（用户删稿、运营撤单都无处安放）。TODO(缺陷)：README 缺口 #2、#11。
func TestModerationStateMachineHasNoWriterForProcessing(t *testing.T) {
	// 「谁把任务写成 PROCESSING / CANCELED」不能靠读代码断言，所以这里**从调用轨迹里把三条
	// 动作实际发起过的目标态抠出来**：把矩阵每一行的三个动作各跑一遍，收集
	// `task.UpdateState:501->N` 里的 N。这样只要 repository 里多出一条写 PROCESSING/CANCELED/发布态
	// 的边，或者有人给 UpdateTaskState 找到调用方，这条即红。
	targets := attemptedTargets(t)
	wantStatesEQ(t, "状态机可达集", "三条动作实际发起过的目标态", targets, []int32{stDone, stAppealed, stAppealDone})

	// 于是 PROCESSING / CANCELED 只能由「布景」出现，任何动作都造不出它们：
	// 逐格核对矩阵里所有动作后状态，凡出现 2 或 9 的格子，其当前态必须本来就是 2 或 9。
	for _, tc := range stateMatrix {
		for _, cell := range []struct {
			label string
			after int32
		}{
			{"worker回写", tc.workerState}, {"申诉", tc.appealState}, {"结案", tc.closeState},
		} {
			if cell.after != stProcessing && cell.after != stCanceled {
				continue
			}
			if tc.from != cell.after {
				t.Errorf("状态机可达集 %s/%s：动作凭空造出了 %d", tc.name, cell.label, cell.after)
			}
		}
	}

	// PENDING 也只能由建单的 INSERT 写出来，改单路径写不出来。
	st := newStore()
	if _, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil)); err != nil {
		t.Fatalf("SubmitForReview：%v", err)
	}
	wantEQ(t, "状态机可达集", "建单后的状态", st.task.state(501), stPending)
	wantCount(t, "状态机可达集", st.log, "task.UpdateState", 0)
}

// TestModerationHappyPathLifecycleAdvancesOncePerEdge 走一遍 §8 的完整生命周期，
// 断言三条 CAS 边按序各推进一次（建单的 PENDING 由 INSERT 写），且**终态之后再打任何写入口都不产生新的迁移**。
func TestModerationHappyPathLifecycleAdvancesOncePerEdge(t *testing.T) {
	st := newStore()
	lc := newTestSvc(st)

	// 1) 建单：PENDING（0 → 1，由 Insert 写，不是 UpdateState）
	sub, err := NewSubmitForReviewLogic(testCtx(), lc).SubmitForReview(submitReq(nil))
	wantNoErr(t, "生命周期 建单", err)
	taskID := sub.GetTask().GetTaskId()
	wantEQ(t, "生命周期 建单", "task_id", taskID, int64(501))
	wantEQ(t, "生命周期 建单", "state", st.task.state(taskID), stPending)
	wantStringsEQ(t, "生命周期 建单", "迁移流水", st.task.advances, nil)

	// 2) worker 回写：PENDING → DONE
	if _, err := NewSubmitWorkerResultLogic(testCtx(), lc).
		SubmitWorkerResult(workerReq(taskID, workerOne, rpc.Verdict_VERDICT_REJECT, "机审命中搬运指纹")); err != nil {
		t.Fatalf("生命周期 worker 回写：%v", err)
	}
	wantEQ(t, "生命周期 worker 回写", "state", st.task.state(taskID), stDone)

	// 3) 用户申诉：DONE → APPEALED
	ap, err := NewSubmitAppealLogic(testCtx(), lc).SubmitAppeal(appealReq(taskID, midAlice, "原创证明见附件"))
	wantNoErr(t, "生命周期 申诉", err)
	appealID := ap.GetAppeal().GetAppealId()
	wantEQ(t, "生命周期 申诉", "appeal_id", appealID, int64(701))
	wantEQ(t, "生命周期 申诉", "state", st.task.state(taskID), stAppealed)

	// 4) 运营结案：APPEALED → APPEAL_DONE
	if _, err := NewProcessAppealLogic(testCtx(), lc).
		ProcessAppeal(processReq(appealID, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立")); err != nil {
		t.Fatalf("生命周期 结案：%v", err)
	}
	wantEQ(t, "生命周期 结案", "state", st.task.state(taskID), stAppealDone)

	wantStringsEQ(t, "生命周期", "三条 CAS 边各推进一次", st.task.advances, []string{
		"501:1->3", "501:3->4", "501:4->5",
	})

	// 5) 终态后再打三个写入口：一条新迁移都不许产生。
	before := append([]string(nil), st.task.advances...)
	if _, err := NewSubmitWorkerResultLogic(testCtx(), lc).
		SubmitWorkerResult(workerReq(taskID, workerTwo, rpc.Verdict_VERDICT_PASS, "另一个 worker 想翻案")); err != nil {
		t.Fatalf("生命周期 终态后 worker 回写：%v", err)
	}
	if _, err := NewSubmitAppealLogic(testCtx(), lc).
		SubmitAppeal(appealReq(taskID, midAlice, "再申诉一次")); err != nil {
		t.Fatalf("生命周期 终态后申诉：%v", err)
	}
	_, err = NewProcessAppealLogic(testCtx(), lc).
		ProcessAppeal(processReq(appealID, opAdminEve, rpc.Verdict_VERDICT_REJECT, "换个运营再判"))
	wantErrIs(t, "生命周期 终态后结案", err, model.ErrAppealAlreadyHandled) // 这一条门槛是对的（申诉侧 state=0）
	wantStringsEQ(t, "生命周期 终态后", "迁移流水不再增长", st.task.advances, before)
	wantEQ(t, "生命周期 终态后", "state 仍是 APPEAL_DONE", st.task.state(taskID), stAppealDone)

	// 但「状态机没动」不等于「数据没动」：终态后 worker 回写仍然覆盖了已审计的结论行，
	// 申诉也仍然插出新行，第一个运营的结案记录也留在第一行申诉上。
	// 结论翻案由 GetResult 断言（README 缺口 #4）；这里断言的是行数的变化口径。
	wantEQ(t, "生命周期 终态后", "结论仍是一行（uniq_task 覆盖）", st.result.count(taskID), 1)
	wantEQ(t, "生命周期 终态后", "申诉变成两行（无唯一键）", st.appeal.countForTask(taskID), 2)
	wantEQ(t, "生命周期 终态后", "第二个 worker 的结论覆盖第一个",
		st.result.rows[taskID].Verdict, vPass)
	wantEQ(t, "生命周期 终态后", "worker_id 一并被覆盖", st.result.rows[taskID].WorkerID, workerTwo)
	// TODO(缺陷)：README 缺口 #4 —— 已终态、且已进入申诉流程的结论被静默改写，审计链断裂。
}

// TestNoActionCanProducePassWithoutWorkerCallback 查「跳过 worker 直接结论」：
// 除了 worker 回调，没有任何入口能写下审核结论；建单/申诉/结案都不能伪造「审过了」。
func TestNoActionCanProducePassWithoutWorkerCallback(t *testing.T) {
	st := newStore()
	lc := newTestSvc(st)

	// 建单后立刻查结论：必须是「没有结论」，而不是默认的 PASS。
	sub, err := NewSubmitForReviewLogic(testCtx(), lc).SubmitForReview(submitReq(nil))
	wantNoErr(t, "无 worker 直判 建单", err)
	taskID := sub.GetTask().GetTaskId()
	wantEQ(t, "无 worker 直判 建单", "结论行数", st.result.count(taskID), 0)
	_, err = NewGetResultLogic(testCtx(), lc).GetResult(&rpc.ResultReq{TaskId: taskID})
	wantErrIs(t, "无 worker 直判 建单", err, model.ErrResultNotFound)

	// 申诉 + 结案把 final_verdict 写成 PASS，但 moderation_result 一个字都不许凭空出现。
	ap, err := NewSubmitAppealLogic(testCtx(), lc).SubmitAppeal(appealReq(taskID, midAlice, "被误判为搬运"))
	wantNoErr(t, "无 worker 直判 申诉", err)
	appealID := ap.GetAppeal().GetAppealId()
	if _, err := NewProcessAppealLogic(testCtx(), lc).
		ProcessAppeal(processReq(appealID, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立")); err != nil {
		t.Fatalf("无 worker 直判 结案：%v", err)
	}
	wantEQ(t, "无 worker 直判 结案", "结论行数仍为 0", st.result.count(taskID), 0)
	wantCount(t, "无 worker 直判 结案", st.log, "result.Upsert", 0)
	// 翻案的 PASS 只活在申诉行里 ⇒ 内容所有者拿到的 GetResult 依然是 ErrResultNotFound，
	// 而 GetTask 已经显示 APPEAL_DONE：状态说「审完了」，结论表却说「没审过」。
	// TODO(缺陷)：README 缺口 #1、#13。
	_, err = NewGetResultLogic(testCtx(), lc).GetResult(&rpc.ResultReq{TaskId: taskID})
	wantErrIs(t, "无 worker 直判 结案", err, model.ErrResultNotFound)
	wantEQ(t, "无 worker 直判 结案", "PASS 只落在申诉行", st.appeal.rows[appealID].FinalVerdict, vPass)
	got, err := NewGetTaskLogic(testCtx(), lc).GetTask(&rpc.TaskReq{TaskId: taskID})
	wantNoErr(t, "无 worker 直判 结案", err)
	// 状态机在这里是对的：PENDING 不是 DONE，所以申诉没能把任务推进；
	// 但申诉行本身已经被运营「结案 + 判 PASS」，审核台于是出现一条「已处理完毕、底下却没有任何审核结论」的工单。
	// TODO(缺陷)：README 缺口 #10（结案不校验任务是否真在 APPEALED）。
	wantEQ(t, "无 worker 直判 结案", "任务态（仍在 PENDING）", got.GetTask().GetState(), rpc.TaskState_TASK_STATE_PENDING)
	wantEQ(t, "无 worker 直判 结案", "申诉却已处理", st.appeal.rows[appealID].State, appealStateHandled)
}

// TestTerminalStatesRejectEveryReopenAttemptButOnlyInState 是「已终态任务再回写」的正面对账：
// 手工列出终态集合，逐格确认状态机不接受任何 reopened 目标；
// 同时把「状态没动但业务行被写脏」的差值钉死，将来修事务边界时这条会红。
func TestTerminalStatesRejectEveryReopenAttemptButOnlyInState(t *testing.T) {
	// 手工抄写的终态集合：结案与撤销之后任务不可再迁移（AGENTS.md §8：终态不可回退）。
	terminals := []struct {
		name  string
		state int32
	}{
		{"APPEAL_DONE(5)", stAppealDone},
		{"CANCELED(9)", stCanceled},
	}
	// 每个终态下允许出现的迁移：0 条。
	for _, term := range terminals {
		t.Run(term.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, term.state, nil)
			seedResult(st, 501, vReject, nil)
			seedAppeal(st, 601, 501, func(a *model.ModerationAppeal) {
				a.State = appealStateHandled
				a.Handler = opAdminDan
				a.FinalVerdict = vReject
				a.FinalReason = "第一次：维持驳回"
			})
			before := st.log.snapshot()

			// 三个写入口全打一遍。
			_, e1 := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
				SubmitWorkerResult(workerReq(501, workerTwo, rpc.Verdict_VERDICT_PASS, "worker 想把已结案任务改成通过"))
			_, e2 := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
				SubmitAppeal(appealReq(501, midAlice, "再申诉一次"))
			_, e3 := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
				ProcessAppeal(processReq(601, opAdminEve, rpc.Verdict_VERDICT_PASS, "第二个运营想改判"))
			wantNoErr(t, "终态回写 worker", e1)
			wantNoErr(t, "终态回写 申诉", e2)
			wantErrIs(t, "终态回写 结案（申诉已处理）", e3, model.ErrAppealAlreadyHandled)

			wantEQ(t, "终态回写", "任务状态不动", st.task.state(501), term.state)
			wantStringsEQ(t, "终态回写", "迁移流水", st.task.advances, nil)
			ops := st.log.opsFrom(before)
			// 三个入口合计 7 次依赖调用，逐字钉死顺序：任何一次「多写一条业务行 / 少一步失效」都会红。
			wantOps(t, "终态回写", ops, []string{
				"result.Upsert:501:1", // PASS 先落库
				"task.UpdateState:501->3",
				"appeal.Insert:501", // 新申诉行
				"task.UpdateState:501->4",
				"cache.DelTask:mod:task:501",
				"cache.SetAppeal:mod:appeal:701",
				"appeal.Update:601", // 已处理 ⇒ 直接失败
			})
			wantEQ(t, "终态回写", "结论被覆盖（缺陷）", st.result.rows[501].Verdict, vPass)
			wantEQ(t, "终态回写", "多出第二行申诉（缺陷）", st.appeal.countForTask(501), 2)
			wantEQ(t, "终态回写", "第一次结案的证据仍在", st.appeal.rows[601].Handler, opAdminDan)
		})
	}
}

// attemptedTargets 把矩阵每一行的三个动作各跑一遍，从调用轨迹里抠出
// `task.UpdateState:<id>-><N>` 的 N，去重后按升序返回——即「生产代码实际尝试写入的状态集合」。
func attemptedTargets(t *testing.T) []int32 {
	t.Helper()
	var seen []int32
	collect := func(st *store) {
		for _, op := range st.log.ops {
			const marker = "task.UpdateState:501->"
			i := strings.Index(op, marker)
			if i < 0 {
				continue
			}
			n, err := strconv.ParseInt(op[i+len(marker):], 10, 32)
			if err != nil {
				t.Fatalf("轨迹里的目标态解析不出来：%q", op)
			}
			to := int32(n)
			if !slices.Contains(seen, to) {
				seen = append(seen, to)
			}
		}
	}
	for _, tc := range stateMatrix {
		w := newStore()
		a := newStore()
		c := newStore()
		for _, st := range []*store{w, a, c} {
			if tc.seeded {
				seedTask(st, 501, tc.from, nil)
			}
		}
		seedAppeal(c, 601, 501, nil)
		if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(w)).
			SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "矩阵自检回写")); err != nil {
			t.Fatalf("矩阵自检 worker：%v", err)
		}
		if _, err := NewSubmitAppealLogic(testCtx(), newTestSvc(a)).
			SubmitAppeal(appealReq(501, midAlice, "矩阵自检申诉")); err != nil {
			t.Fatalf("矩阵自检 申诉：%v", err)
		}
		if _, err := NewProcessAppealLogic(testCtx(), newTestSvc(c)).
			ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "矩阵自检结案")); err != nil {
			t.Fatalf("矩阵自检 结案：%v", err)
		}
		for _, st := range []*store{w, a, c} {
			collect(st)
		}
	}
	slices.Sort(seen)
	return seen
}
