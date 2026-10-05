package logic

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/video/model"
	"go-video/services/video/rpc"
)

// pairUpdateSuccessSeq 是 UpdateSubmission 成功的完整依赖轨迹：
// 读（判属主与状态）→ 写元信息 → 失效详情缓存 → 复读投影。
// 顺序本身就是结论：失效必须发生在落库**之后**、复读之前，否则复读会把旧值再缓存一遍。
// 本文件是 Update/Delete 配对用例文件，与分方法文件的重叠判定保留不删；
// 因此这里的包级标识符一律带 pair 前缀，避免与 update_submission_logic_test.go 的同名声明冲突。
var pairUpdateSuccessSeq = []string{
	"video_submission.FindOne:101",
	"video_submission.UpdateFields:101",
	"cache.DelSubmission:101",
	"video_submission.FindOne:101",
}

// TestPairUpdateSubmissionGuardOrderKeepsRowUntouched 锁 aid → mid 的守卫顺序与「零依赖调用」。
// 与 update_submission_logic_test.go 的同名判定重叠，保留为配对文件的独立证据；
// 包内必须换名，否则同包重复声明会让整个 logic 测试包编译失败。
func TestPairUpdateSubmissionGuardOrderKeepsRowUntouched(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UpdateSubmissionReq
		want error
	}{
		{"aid=0", &rpc.UpdateSubmissionReq{Aid: 0, Mid: 7, Title: "x"}, model.ErrInvalidAid},
		{"aid<0", &rpc.UpdateSubmissionReq{Aid: -5, Mid: 7, Title: "x"}, model.ErrInvalidAid},
		{"mid=0", &rpc.UpdateSubmissionReq{Aid: 101, Mid: 0, Title: "x"}, model.ErrInvalidMid},
		{"两者都非法先报 aid", &rpc.UpdateSubmissionReq{Aid: 0, Mid: 0}, model.ErrInvalidAid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)
			from := st.log().snapshot()
			got, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).UpdateSubmission(tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
			wantEq(t, tc.name, "mtime 未被改写", st.sub(101).Mtime, staleTime)
		})
	}
}

// TestUpdateSubmissionRejectionOrder 锁业务拒绝顺序：
// 不存在 → 非属主 → 非草稿。特别是「非属主」必须先于「非草稿」报出，
// 否则别人可以通过错误码探测某只稿件处于什么状态。
func TestUpdateSubmissionRejectionOrder(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		mid   int64
		want  error
	}{
		{"稿件不存在", model.StateDraft, 7, model.ErrSubmissionNotFound}, // 不布数据
		{"非属主（稿件已发布）", model.StatePublished, 8, model.ErrNotOwner},
		{"非属主（稿件是草稿）", model.StateDraft, 8, model.ErrNotOwner},
		{"属主但非草稿", model.StateUploading, 7, model.ErrSubmissionNotDraft},
		{"属主但已删除", model.StateDeleted, 7, model.ErrSubmissionNotDraft},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			if tc.name != "稿件不存在" {
				st.seedDraft(101, 7, tc.state)
			}
			from := st.log().snapshot()
			_, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).
				UpdateSubmission(&rpc.UpdateSubmissionReq{Aid: 101, Mid: tc.mid, Title: "改一下"})
			wantErrIs(t, tc.name, err, tc.want)
			// 三种拒绝都只许读一次，之后零写、零失效。
			wantSeq(t, tc.name, st.log(), from, "video_submission.FindOne:101")
			wantEq(t, tc.name, "审计行数", st.auditCount(), 0)
			wantEq(t, tc.name, "事务数", st.txCount(), 0)
		})
	}
}

// TestUpdateSubmissionMergesEmptyFields 锁「空串/0 = 不改」的部分更新语义，
// 同时把它的后果 pin 住：**没有任何入参能把 desc/cover/tag 清空、也没有能把 typeid 归 0**。
// 这是 proto3 标量字段的固有歧义（要区分「未设置」与「置空」需要 optional/FieldMask），
// 本轮不擅改契约，只锁现状。
func TestUpdateSubmissionMergesEmptyFields(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	got, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).
		UpdateSubmission(&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "新标题"})
	wantNoErr(t, "只改标题", err)
	wantSeq(t, "只改标题轨迹", st.log(), 0, pairUpdateSuccessSeq...)

	s := st.sub(101)
	wantEq(t, "只改标题", "title", s.Title, "新标题")
	wantEq(t, "只改标题（空串=保持原值）", "desc", s.Desc, "d-101")
	wantEq(t, "只改标题（空串=保持原值）", "cover", s.Cover, "c-101")
	wantEq(t, "只改标题（0=保持原值）", "typeid", s.Typeid, int32(11))
	wantEq(t, "只改标题（空串=保持原值）", "tag", s.Tag, "g-101")
	wantEq(t, "ctime 不许被改", "ctime", s.Ctime, staleTime)
	if s.Mtime <= staleTime {
		t.Errorf("mtime = %d，应当被 UPDATE 刷成当前时间（> %d）", s.Mtime, staleTime)
	}
	if s.Mtime < time.Now().Unix()-2 {
		t.Errorf("mtime = %d 太旧，不像本次写入", s.Mtime)
	}
	wantEq(t, "应答用复读值", "title", got.GetSubmission().GetTitle(), "新标题")
	wantEq(t, "应答用复读值", "state", got.GetSubmission().GetState(), rpc.SubmissionState_STATE_DRAFT)
}

// TestUpdateSubmissionReplacesAllFields 锁五个元信息字段同时给值时全部覆盖，
// 且 UpdateFields 的入参顺序（title, desc, cover, typeid, tag）没有串列——
// 串列在这条用例下会立刻体现为「cover 里装着 tag」。
func TestUpdateSubmissionReplacesAllFields(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)

	_, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).UpdateSubmission(
		&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "T", Desc: "D", Cover: "C", Typeid: 22, Tag: "G"})
	wantNoErr(t, "全量更新", err)

	s := st.sub(101)
	wantEq(t, "全量更新", "title", s.Title, "T")
	wantEq(t, "全量更新", "desc", s.Desc, "D")
	wantEq(t, "全量更新", "cover", s.Cover, "C")
	wantEq(t, "全量更新", "typeid", s.Typeid, int32(22))
	wantEq(t, "全量更新", "tag", s.Tag, "G")
	wantEq(t, "state 不属于元信息", "state", s.State, model.StateDraft)
	wantEq(t, "mid 不可改", "mid", s.Mid, int64(7))
}

// TestUpdateSubmissionPropagatesWriteError 锁 UPDATE 失败原样透传，
// 并且**不失效缓存**（没改成功就不该让别人的缓存白掉一次）、不写审计。
func TestUpdateSubmissionPropagatesWriteError(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.fail("submission.UpdateFields")

	got, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).
		UpdateSubmission(&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "新标题"})
	wantErrIs(t, "UPDATE 故障", err, errBoom)
	if got != nil {
		t.Errorf("UPDATE 失败仍返回 reply %+v", got)
	}
	wantSeq(t, "UPDATE 故障轨迹", st.log(), 0,
		"video_submission.FindOne:101", "video_submission.UpdateFields:101")
	wantEq(t, "UPDATE 失败后", "title 未变", st.sub(101).Title, "t-101")
	wantEq(t, "UPDATE 失败后", "mtime 未变", st.sub(101).Mtime, staleTime)
	wantCount(t, "UPDATE 失败不得失效缓存", st.log(), "cache.DelSubmission", 0)
}

// TestUpdateSubmissionCacheInvalidationErrorIsSwallowed 把缺陷 pin 成哨兵：
// repository.UpdateSubmissionFields 用 `_ = r.cache.DelSubmission(...)` 吞掉 Redis 故障，
// 于是接口返回成功、DB 已改、缓存里仍是旧详情。当前 GetSubmission 不读缓存所以看不出差异，
// 一旦详情缓存接线，这里就是 60s 脏读；修法是让失效失败可见（告警或改异步补偿）。
func TestUpdateSubmissionCacheInvalidationErrorIsSwallowed(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.s.fail("cache.DelSubmission", errBoom)

	got, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).
		UpdateSubmission(&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "新标题"})
	wantNoErr(t, "缓存故障被吞后写请求仍成功", err)
	wantEq(t, "缓存故障被吞后写请求仍成功", "title", got.GetSubmission().GetTitle(), "新标题")
	wantSeq(t, "失效尝试仍发生在落库后", st.log(), 0, pairUpdateSuccessSeq...)
}

// TestUpdateSubmissionRereadErrorStillReportsFailure 锁「更新成功但复读失败」的形态：
// 整个调用报错（不返回半成品应答），但**库里已经改掉了**——这是本方法非幂等可观察的地方：
// 客户端按错误重试时，第二次更新是「值全同」的空更新，见下一条用例。
func TestUpdateSubmissionRereadErrorStillReportsFailure(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	// 第 2 次 FindOne 就是落库后的复读。
	st.failNth("submission.FindOne", 2)

	got, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).
		UpdateSubmission(&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "新标题"})
	wantErrIs(t, "复读故障", err, errBoom)
	if got != nil {
		t.Errorf("复读失败仍返回 reply %+v", got)
	}
	wantEq(t, "复读失败但更新已落库", "title", st.sub(101).Title, "新标题")
	wantSeq(t, "复读失败轨迹", st.log(), 0, pairUpdateSuccessSeq...)
	wantCount(t, "落库后确实尝试过失效缓存", st.log(), "cache.DelSubmission", 1)
}

// TestPairUpdateSubmissionSameValuesPatchIsNotFound 锁真实 MySQL 的行为差异（本服务 DSN 未开
// clientFoundRows，RowsAffected 是 changed rows）：把字段更新成它已有的值 → 0 行 →
// model 判 ErrSubmissionNotFound，logic 原样抛出。后果是上一条用例里的「重试」不但不能
// 收敛，还会把已成功的更新报成「稿件不存在」。这里打开替身的 changed-rows 模式复现它，
// 默认模式（命中行数）下的幂等形态由 TestUpdateSubmissionMergesEmptyFields 覆盖。
// 分方法文件里的 `TestUpdateSubmissionNoOpUpdateDivergence` 打的是「空补丁」，本用例打的是
// 「五个字段全给回原值」——两种入参在真库上都是 0 changed rows，故两侧判定都保留。
func TestPairUpdateSubmissionSameValuesPatchIsNotFound(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.markChangedRows()

	_, err := NewUpdateSubmissionLogic(context.Background(), st.svcCtx).UpdateSubmission(
		&rpc.UpdateSubmissionReq{Aid: 101, Mid: 7, Title: "t-101", Desc: "d-101", Cover: "c-101", Typeid: 11, Tag: "g-101"})
	wantErrIs(t, "空更新在真实 MySQL 上", err, model.ErrSubmissionNotFound)
	if errors.Is(err, errBoom) {
		t.Fatal("不应把 changed-rows 混成注入故障")
	}
	wantSeq(t, "空更新轨迹", st.log(), 0,
		"video_submission.FindOne:101", "video_submission.UpdateFields:101")
	wantCount(t, "空更新不失效缓存", st.log(), "cache.DelSubmission", 0)
}

// --- DeleteSubmission ---

// deleteSuccessSeq 是删除（状态流转到 DELETED）的依赖轨迹。
// 注意最后**没有** cache.DelSubmission：DeleteSubmission 不失效详情缓存（缺陷，见 README）。
// 尾部的 video_outbox.Insert 只在稿件删除前公开过（PUBLISHED/OFFLINE/EXPIRED）时才出现，
// 判定来自 content_event_logic_test.go 的期望表而不是 repository 的实现。
func deleteSeq(aid, from int64) []string {
	ops := []string{
		"video_submission.FindOne:" + itoa(aid),
		"db.TransactCtx",
		"video_submission.FindOne:" + itoa(aid),
		"video_submission.UpdateState:" + itoa(aid) + "/14",
		"video_audit_log.Insert:" + itoa(aid) + ":" + itoa(from) + "->14",
	}
	if wantEventAction(int32(from), model.StateDeleted) != "" {
		ops = append(ops, "video_outbox.Insert:"+itoa(aid))
	}
	return ops
}

// TestPairDeleteSubmissionGuardOrderKeepsRowUntouched 锁 aid → mid 守卫，拒绝后零依赖调用。
// 与 delete_submission_logic_test.go 的同名判定重叠，保留为配对文件的独立证据，故包内换名。
func TestPairDeleteSubmissionGuardOrderKeepsRowUntouched(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.SubmissionReq
		want error
	}{
		{"aid=0", &rpc.SubmissionReq{Aid: 0, Mid: 7}, model.ErrInvalidAid},
		{"aid<0 且 mid<0 先报 aid", &rpc.SubmissionReq{Aid: -1, Mid: -1}, model.ErrInvalidAid},
		{"mid=0", &rpc.SubmissionReq{Aid: 101, Mid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, model.StateDraft)
			from := st.log().snapshot()
			got, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).DeleteSubmission(tc.in)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantNoCallAfter(t, tc.name, st.log(), from)
			wantEq(t, tc.name, "state 未变", st.sub(101).State, model.StateDraft)
			wantEq(t, tc.name, "审计行数", st.auditCount(), 0)
		})
	}
}

// TestDeleteSubmissionRejectsNonOwnerAndMissing 锁属主校验与不存在的区分，两者都只读一次。
func TestDeleteSubmissionRejectsNonOwnerAndMissing(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StatePublished)

	_, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
		DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 8})
	wantErrIs(t, "非属主删除", err, model.ErrNotOwner)

	_, err = NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
		DeleteSubmission(&rpc.SubmissionReq{Aid: 999, Mid: 7})
	wantErrIs(t, "删除不存在的稿件", err, model.ErrSubmissionNotFound)

	_, err = NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
		DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "属主删除已发布稿件", err)
}

// TestDeleteSubmissionIllegalFromStates 锁「删除也是状态流转」：只有状态机允许
// →DELETED 的稿件才删得掉。这里同时把两条缺陷 pin 成哨兵：
//   - SCANNING/TRANSCODING/READY_FOR_REVIEW/APPROVED/SCHEDULED/APPEAL 的稿件用户永远删不掉；
//   - EXPIRED 是**死状态**（legalTransitions 里连出边都没有），下架/过期后无法再删除或恢复。
func TestDeleteSubmissionIllegalFromStates(t *testing.T) {
	blocked := []struct {
		name  string
		state int32
	}{
		{"SCANNING", model.StateScanning},
		{"TRANSCODING", model.StateTranscoding},
		{"READY_FOR_REVIEW", model.StateReadyForReview},
		{"APPROVED", model.StateApproved},
		{"SCHEDULED", model.StateScheduled},
		{"APPEAL", model.StateAppeal},
		{"EXPIRED", model.StateExpired},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, tc.state)
			from := st.log().snapshot()

			got, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
				DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
			wantErrIs(t, tc.name+" 删不掉", err, model.ErrInvalidStateTransition)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回 reply %+v", tc.name, got)
			}
			wantSeq(t, tc.name, st.log(), from, "video_submission.FindOne:101")
			wantEq(t, tc.name, "state 未变", st.sub(101).State, tc.state)
			wantEq(t, tc.name, "审计行数", st.auditCount(), 0)
			wantEq(t, tc.name, "事务数", st.txCount(), 0)
		})
	}
}

// TestDeleteSubmissionWritesStateAndAuditInOneTx 锁删除的落库形态：
// 一个事务里先改 state 再写审计，审计的 from/to、operator、reason 都符合
// 「owner:<mid> + user delete submission」的口径（AGENTS.md §8 要求保留审计证据）。
func TestDeleteSubmissionWritesStateAndAuditInOneTx(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"DRAFT→DELETED", model.StateDraft},
		{"PUBLISHED→DELETED", model.StatePublished},
		{"OFFLINE→DELETED", model.StateOffline},
		{"REJECTED→DELETED", model.StateRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedDraft(101, 7, tc.state)

			got, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
				DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
			wantNoErr(t, tc.name, err)
			if got == nil {
				t.Fatalf("%s：删除成功却返回 nil reply", tc.name)
			}
			wantSeq(t, tc.name, st.log(), 0, deleteSeq(101, int64(tc.state))...)
			wantEq(t, tc.name, "事务数", st.txCount(), 1)

			s := st.sub(101)
			wantEq(t, tc.name, "state", s.State, model.StateDeleted)
			wantEq(t, tc.name, "ctime 不动", s.Ctime, staleTime)

			rows := st.auditRows(101)
			wantEq(t, tc.name, "审计行数", len(rows), 1)
			wantEq(t, tc.name, "审计 from", rows[0].FromState, tc.state)
			wantEq(t, tc.name, "审计 to", rows[0].ToState, model.StateDeleted)
			wantEq(t, tc.name, "审计 operator", rows[0].Operator, "owner:7")
			wantEq(t, tc.name, "审计 reason", rows[0].Reason, "user delete submission")
			wantEq(t, tc.name, "审计 aid", rows[0].Aid, int64(101))
			if rows[0].Ctime <= staleTime {
				t.Errorf("审计 ctime = %d，应当是本次写入时间", rows[0].Ctime)
			}
			wantCount(t, tc.name+"：删除不失效详情缓存（缺陷）", st.log(), "cache.DelSubmission", 0)
		})
	}
}

// TestDeleteSubmissionAlreadyDeletedIsIdempotent 锁重复删除返回成功且零写：
// 但注意这条支路**既不写审计也不失效缓存**，所以「谁在什么时候又点了一次删除」无痕迹，
// 且缓存里若仍有旧详情不会被清掉。当前实现如此，本用例锁住现状。
func TestDeleteSubmissionAlreadyDeletedIsIdempotent(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDeleted)
	from := st.log().snapshot()

	got, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
		DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantNoErr(t, "重复删除", err)
	if got == nil {
		t.Fatal("重复删除返回 nil reply，want 空应答（幂等成功）")
	}
	wantSeq(t, "重复删除只读一次", st.log(), from, "video_submission.FindOne:101")
	wantEq(t, "重复删除", "事务数", st.txCount(), 0)
	wantEq(t, "重复删除", "审计行数", st.auditCount(), 0)
}

// TestDeleteSubmissionPropagatesTransitionError 锁「删除的 UPDATE 失败原样透传」：
// 既不吞错也不留半截审计。替身事务不回滚，所以断言的是「库里到底还剩什么」。
func TestDeleteSubmissionPropagatesTransitionError(t *testing.T) {
	st := newStore()
	st.seedDraft(101, 7, model.StateDraft)
	st.fail("submission.UpdateState")

	_, err := NewDeleteSubmissionLogic(context.Background(), st.svcCtx).
		DeleteSubmission(&rpc.SubmissionReq{Aid: 101, Mid: 7})
	wantErrIs(t, "删除时 UPDATE 故障", err, errBoom)
	wantSeq(t, "删除故障轨迹", st.log(), 0,
		"video_submission.FindOne:101", "db.TransactCtx",
		"video_submission.FindOne:101", "video_submission.UpdateState:101/14")
	wantEq(t, "删除失败后", "state 未变", st.sub(101).State, model.StateDraft)
	wantEq(t, "删除失败后", "审计行数", st.auditCount(), 0)
}
